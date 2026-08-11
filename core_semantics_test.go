package arxiv

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSQLitePostgresCapabilityErrorIsTyped(t *testing.T) {
	cache := openTestCache(t)
	_, err := cache.SearchSemantic(context.Background(), []float32{1}, 1)
	if !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("SearchSemantic error = %v, want capability error", err)
	}
	var capabilityErr *CapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Backend != DBTypeSQLite {
		t.Fatalf("SearchSemantic error = %#v", err)
	}
}

func TestAdminStatsExposeMetricAvailabilityWithoutPaidLifecycle(t *testing.T) {
	cache := openTestCache(t)
	if err := cache.db.Create(&User{ID: "manual-label", Email: "label@example.test", Plan: "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := cache.AdminStats(context.Background())
	if err != nil {
		t.Fatalf("AdminStats: %v", err)
	}
	availability := stats.Availability["qwen_abstract_embeddings"]
	if availability.Available || availability.Reason == "" {
		t.Fatalf("qwen availability = %#v", availability)
	}
	paidAvailability := stats.Availability["paid_user_lifecycle"]
	if paidAvailability.Available || paidAvailability.Reason == "" || stats.Users.PaidUsers != 1 {
		t.Fatalf("paid lifecycle availability=%#v count=%d", paidAvailability, stats.Users.PaidUsers)
	}
}

func TestOmittedMetadataIDsAreSurfaced(t *testing.T) {
	requested := []string{"one", "two", "three"}
	returned := map[string]bool{"one": true, "three": true}
	if got := omittedMetadataIDs(requested, returned); !reflect.DeepEqual(got, []string{"two"}) {
		t.Fatalf("omittedMetadataIDs = %#v", got)
	}
	err := &PartialMetadataError{MissingIDs: omittedMetadataIDs(requested, returned)}
	if !strings.Contains(err.Error(), "two") {
		t.Fatalf("PartialMetadataError = %q", err)
	}
}

func TestDownloadPapersSerializesProgressAndQueuesEmbedding(t *testing.T) {
	cache := openTestCache(t)
	dir := t.TempDir()
	const count = 8
	papers := make([]Paper, 0, count)
	for i := 0; i < count; i++ {
		id := "paper-" + string(rune('a'+i))
		pdfPath := filepath.Join(dir, id+".pdf")
		if err := os.WriteFile(pdfPath, []byte("%PDF-1.7\nbody"), 0o644); err != nil {
			t.Fatal(err)
		}
		sourcePath := filepath.Join(dir, id)
		if err := os.Mkdir(sourcePath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sourcePath, "main.tex"), []byte("text"), 0o644); err != nil {
			t.Fatal(err)
		}
		papers = append(papers, Paper{ID: id, Title: id, Abstract: "text", PDFPath: pdfPath, SourcePath: sourcePath, PDFDownloaded: true, SourceDownloaded: true})
	}
	if err := cache.db.Create(&papers).Error; err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(papers))
	for i := range papers {
		ids[i] = papers[i].ID
	}

	var callbacks atomic.Int64
	var inCallback atomic.Int64
	var concurrent atomic.Bool
	err := cache.downloadPapers(context.Background(), ids, DownloadOptions{
		Concurrency:       4,
		RateLimit:         -1,
		DownloadPDF:       true,
		DownloadSource:    true,
		GenerateEmbedding: true,
		Progress: func(_ string, _, _ int) {
			if inCallback.Add(1) != 1 {
				concurrent.Store(true)
			}
			time.Sleep(time.Millisecond)
			callbacks.Add(1)
			inCallback.Add(-1)
		},
	})
	if err != nil {
		t.Fatalf("downloadPapers: %v", err)
	}
	if callbacks.Load() != count || concurrent.Load() {
		t.Fatalf("callbacks=%d concurrent=%v", callbacks.Load(), concurrent.Load())
	}
	var queued int64
	if err := cache.db.Model(&EmbeddingJob{}).Where("status = ?", EmbeddingJobPending).Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued != count {
		t.Fatalf("queued embeddings = %d, want %d", queued, count)
	}
}

func TestOAIListRecordsRetriesAndReportsDeletion(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`<OAI-PMH><ListRecords>
<record><header status="deleted"><identifier>oai:arXiv.org:2601.00001</identifier></header></record>
</ListRecords></OAI-PMH>`))
	}))
	defer server.Close()
	client := NewOAIClient()
	client.baseURL = server.URL
	response, err := client.ListRecords(context.Background(), "", time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if requests.Load() != 2 || response.RecordCount != 1 || !reflect.DeepEqual(response.DeletedPaperIDs, []string{"2601.00001"}) {
		t.Fatalf("requests=%d response=%#v", requests.Load(), response)
	}
}

func TestPersistSyncRecordsAppliesDeletionAndScopesState(t *testing.T) {
	cache := openTestCache(t)
	if err := cache.db.Create(&Paper{ID: "deleted", Title: "old"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := cache.db.Create(&CategoryStat{Name: "cs.AI", Count: 1}).Error; err != nil {
		t.Fatal(err)
	}
	key := syncResumptionKey(SyncOptions{Set: "cs", From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err := cache.persistSyncRecords(context.Background(), []Paper{{ID: "kept", Title: "new"}}, []string{"deleted"}, key, "next", 1); err != nil {
		t.Fatal(err)
	}
	if cache.PaperExists(context.Background(), "deleted") {
		t.Fatal("deleted OAI record remains in papers")
	}
	var categoryCount int64
	if err := cache.db.Model(&CategoryStat{}).Count(&categoryCount).Error; err != nil || categoryCount != 0 {
		t.Fatalf("stale category rows=%d err=%v", categoryCount, err)
	}
	var state SyncState
	if err := cache.db.Where("key = ?", key).First(&state).Error; err != nil || state.Value != "next" {
		t.Fatalf("state=%#v err=%v", state, err)
	}
	other := syncResumptionKey(SyncOptions{Set: "math", From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if key == other {
		t.Fatal("different OAI sets share a checkpoint key")
	}
}

func TestFuzzyPDFMatchReturnsActualTypoPosition(t *testing.T) {
	text := "introduction then quantom mechanics appears here"
	score, position, err := bestBoundedMatch(context.Background(), text, "quantum mechanics")
	if err != nil {
		t.Fatal(err)
	}
	if score < 0.8 || position != strings.Index(text, "quantom") {
		t.Fatalf("score=%f position=%d", score, position)
	}
	snippet := extractContextAt(text, position, "quantum mechanics", 30)
	if !strings.Contains(snippet, "quantom mechanics") {
		t.Fatalf("snippet = %q", snippet)
	}
}

func TestSemanticLinksAreAcyclicAndBounded(t *testing.T) {
	rows := []semanticVectorRow{
		{PaperID: "a", Vector: []float64{1, 0}, Anchor: true},
		{PaperID: "b", Vector: []float64{0.9, 0.1}},
		{PaperID: "c", Vector: []float64{0.8, 0.2}},
		{PaperID: "d", Vector: []float64{0, 1}},
	}
	links := buildSemanticLinks(rows)
	if len(links) != len(rows)-1 {
		t.Fatalf("links=%d want=%d", len(links), len(rows)-1)
	}
	parents := map[string]string{}
	for _, link := range links {
		parents[link.Target] = link.Source
	}
	for node := range parents {
		seen := map[string]bool{}
		for current := node; current != ""; current = parents[current] {
			if seen[current] {
				t.Fatalf("cycle detected from %s: %#v", node, links)
			}
			seen[current] = true
		}
	}
}

func TestAuthorStatsUseLiveCollaboratorsNotMaterializedGraph(t *testing.T) {
	cache := openTestCache(t)
	papers := []Paper{
		{ID: "one", Authors: "Ada Lovelace, Grace Hopper", Created: time.Now()},
		{ID: "two", Authors: "Ada Lovelace, Alan Turing", Created: time.Now()},
	}
	if err := cache.db.Create(&papers).Error; err != nil {
		t.Fatal(err)
	}
	stats, err := cache.GetAuthorStats(context.Background(), "Ada Lovelace")
	if err != nil {
		t.Fatal(err)
	}
	if stats.PaperCount != 2 || stats.CollaboratorCount != 2 {
		t.Fatalf("stats=%#v", stats)
	}
}

func TestConcurrentDownloadOptionNormalizationIsIndependent(t *testing.T) {
	input := &DownloadOptions{Concurrency: 99, RateLimit: -1}
	var wait sync.WaitGroup
	for i := 0; i < 20; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			normalized := normalizedDownloadOptions(input)
			if normalized.Concurrency != 16 || normalized.RateLimit != 0 {
				t.Errorf("normalized=%#v", normalized)
			}
		}()
	}
	wait.Wait()
	if input.Concurrency != 99 || input.RateLimit != -1 {
		t.Fatalf("input mutated: %#v", input)
	}
}
