package arxiv

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestCache(t *testing.T) *Cache {
	t.Helper()
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cache.Close() })
	return cache
}

func TestUpdateCitationsReplacesExistingEdgesWithEmptySet(t *testing.T) {
	cache := openTestCache(t)
	if err := cache.db.Create(&Citation{FromID: "paper-a", ToID: "paper-b"}).Error; err != nil {
		t.Fatalf("seed citation: %v", err)
	}
	if err := cache.UpdateCitations(context.Background(), "paper-a", t.TempDir()); err != nil {
		t.Fatalf("UpdateCitations: %v", err)
	}
	var count int64
	if err := cache.db.Model(&Citation{}).Where("from_id = ?", "paper-a").Count(&count).Error; err != nil {
		t.Fatalf("count citations: %v", err)
	}
	if count != 0 {
		t.Fatalf("citation count = %d, want 0", count)
	}
}

func TestExtractReferencesHandlesLongLines(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("x", 128*1024) + " arXiv:2601.12345"
	if err := os.WriteFile(filepath.Join(dir, "refs.bib"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := extractReferences(dir)
	if err != nil {
		t.Fatalf("extractReferences: %v", err)
	}
	if !reflect.DeepEqual(refs, []string{"2601.12345"}) {
		t.Fatalf("refs = %#v", refs)
	}
}

func TestParseAndExportCommaFormattedAuthors(t *testing.T) {
	authors := "Doe, Jane and Roe, John"
	if got := ParseAuthors(authors); !reflect.DeepEqual(got, []string{"Doe, Jane", "Roe, John"}) {
		t.Fatalf("ParseAuthors = %#v", got)
	}
	paper := Paper{ID: "2601.00001", Authors: authors}
	ris := paper.ToRIS()
	if !strings.Contains(ris, "AU  - Doe, Jane\n") || !strings.Contains(ris, "AU  - Roe, John\n") {
		t.Fatalf("RIS authors incorrectly formatted:\n%s", ris)
	}
}

func TestParseAuthorsPairsCommaFormattedNames(t *testing.T) {
	got := ParseAuthors("Doe, Jane, Roe, John")
	want := []string{"Doe, Jane", "Roe, John"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseAuthors = %#v, want %#v", got, want)
	}
}

func TestEscapeBibTeXDoesNotReescapeInsertedBackslashes(t *testing.T) {
	if got, want := escapeBibTeX(`a_b\c`), `a\_b\textbackslash{}c`; got != want {
		t.Fatalf("escapeBibTeX = %q, want %q", got, want)
	}
}

func TestSiteBaseURLTrimsTrailingSlashes(t *testing.T) {
	t.Setenv("SITE_URL", "https://example.test///")
	if got := SiteBaseURL(); got != "https://example.test" {
		t.Fatalf("SiteBaseURL = %q", got)
	}
}

func TestValidArxivIDRejectsUpstreamQueryInjection(t *testing.T) {
	for _, id := range []string{"2601.12345", "hep-th/9901001", "2601.12345v2"} {
		if !ValidArxivID(id) {
			t.Fatalf("ValidArxivID rejected %q", id)
		}
	}
	for _, id := range []string{"2601.12345&max_results=1000", "evil/1234567", "../hep-th/9901001", "2601.12345?x=1"} {
		if ValidArxivID(id) {
			t.Fatalf("ValidArxivID accepted %q", id)
		}
	}
}

func TestExtractSourceRejectsTraversal(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "source.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, contents := range map[string]string{"../escape.tex": "bad", "safe/main.tex": "good"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(contents)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "extract")
	if err := extractSource(archivePath, dst); err != nil {
		t.Fatalf("extractSource: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dst), "escape.tex")); !os.IsNotExist(err) {
		t.Fatalf("traversal file was created: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dst, "safe", "main.tex")); err != nil || string(data) != "good" {
		t.Fatalf("safe file = %q, %v", data, err)
	}
}

func TestPersistSyncPageUpdatesTokenAndInvalidatesPaperCache(t *testing.T) {
	cache := openTestCache(t)
	original := Paper{ID: "2601.00001", Title: "old", Abstract: "abstract"}
	if err := cache.db.Create(&original).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := cache.GetPaper(context.Background(), original.ID); err != nil {
		t.Fatal(err)
	}
	incoming := []Paper{{ID: original.ID, Title: "new", Abstract: "abstract"}}
	if err := cache.persistSyncPage(context.Background(), incoming, "next-token"); err != nil {
		t.Fatalf("persistSyncPage: %v", err)
	}
	var state SyncState
	if err := cache.db.Where("key = ?", "resumption_token").First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Value != "next-token" {
		t.Fatalf("token = %q", state.Value)
	}
	paper, err := cache.GetPaper(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paper.Title != "new" {
		t.Fatalf("cached title = %q", paper.Title)
	}
}

func TestOAIListRecordsSkipsDeletedRecords(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<OAI-PMH><ListRecords>
  <record><header status="deleted"><identifier>oai:arXiv.org:2601.00001</identifier></header></record>
  <record><header><identifier>oai:arXiv.org:2601.00002</identifier></header><metadata><arXiv>
    <id>2601.00002</id><created>2026-01-01</created><title>Kept</title><abstract>Text</abstract>
  </arXiv></metadata></record>
</ListRecords></OAI-PMH>`))
	}))
	defer server.Close()
	client := NewOAIClient()
	client.baseURL = server.URL
	response, err := client.ListRecords(context.Background(), "", time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if len(response.Papers) != 1 || response.Papers[0].ID != "2601.00002" {
		t.Fatalf("papers = %#v", response.Papers)
	}
}

func TestEmbeddingWorkerClaimsQueueByPriority(t *testing.T) {
	cache := openTestCache(t)
	papers := []Paper{
		{ID: "low", Title: "Low", Abstract: "Text"},
		{ID: "high", Title: "High", Abstract: "Text"},
	}
	if err := cache.db.Create(&papers).Error; err != nil {
		t.Fatal(err)
	}
	if err := cache.QueueEmbedding(context.Background(), "low", 1); err != nil {
		t.Fatal(err)
	}
	if err := cache.QueueEmbedding(context.Background(), "high", 10); err != nil {
		t.Fatal(err)
	}
	worker := NewEmbeddingWorker(cache, DefaultEmbeddingWorkerConfig())
	claimed, err := worker.getPendingPapers(context.Background(), 1)
	if err != nil {
		t.Fatalf("getPendingPapers: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != "high" {
		t.Fatalf("claimed = %#v", claimed)
	}
	var job EmbeddingJob
	if err := cache.db.Where("paper_id = ?", "high").First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != EmbeddingJobProcessing || job.Attempts != 1 {
		t.Fatalf("job status=%q attempts=%d", job.Status, job.Attempts)
	}
}

func TestConcurrentAPIKeyRotationLeavesOneActiveKey(t *testing.T) {
	cache := openTestCache(t)
	const rotations = 12
	var wg sync.WaitGroup
	errs := make(chan error, rotations)
	for i := 0; i < rotations; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := cache.RegenerateUserAPIKey(context.Background(), "user-1", "Agent access")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("RegenerateUserAPIKey: %v", err)
		}
	}
	var active int64
	if err := cache.db.Model(&UserAPIKey{}).Where("user_id = ? AND name = ? AND revoked_at IS NULL", "user-1", "Agent access").Count(&active).Error; err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active keys = %d, want 1", active)
	}
}

func TestFuzzyMatchHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fuzzyMatchContext(ctx, strings.Repeat("a", 10000), "bbbb")
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestValidPDFFileRejectsPartialFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paper.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.7\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !validPDFFile(path) {
		t.Fatal("valid PDF was rejected")
	}
	if err := os.WriteFile(path, []byte("%PD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if validPDFFile(path) {
		t.Fatal("partial PDF was accepted")
	}
}

func TestPaperPublicJSONAndMetadataCacheExcludeInternalFullText(t *testing.T) {
	cache := openTestCache(t)
	paper := Paper{
		ID:               "2601.00001",
		Title:            "Safe metadata",
		Abstract:         "Public abstract",
		PDFPath:          "/private/cache/paper.pdf",
		SourcePath:       "/private/cache/source",
		PDFText:          "private full paper text",
		PDFDownloaded:    true,
		SourceDownloaded: true,
	}
	if err := cache.db.Create(&paper).Error; err != nil {
		t.Fatal(err)
	}

	loaded, err := cache.GetPaper(context.Background(), paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.PDFText != "" {
		t.Fatal("GetPaper loaded full text into the metadata cache")
	}
	encoded, err := json.Marshal(&paper)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{paper.PDFPath, paper.SourcePath, paper.PDFText} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("public paper JSON exposed internal value %q: %s", secret, encoded)
		}
	}
	text, err := cache.EnsurePDFText(context.Background(), paper.ID)
	if err != nil {
		t.Fatal(err)
	}
	if text != paper.PDFText {
		t.Fatalf("EnsurePDFText = %q, want stored text", text)
	}
}

func TestEmbeddingWorkerSendsMutationToken(t *testing.T) {
	const token = "test-mutation-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"processed":1,"skipped":0}`))
	}))
	defer server.Close()

	config := DefaultEmbeddingWorkerConfig()
	config.ServiceURL = server.URL
	config.MutationToken = token
	worker := NewEmbeddingWorker(nil, config)
	processed, failed := worker.embedPapers(context.Background(), []Paper{{ID: "2601.00001", Title: "Test"}})
	if processed != 1 || failed != 0 {
		t.Fatalf("processed=%d failed=%d", processed, failed)
	}
}
