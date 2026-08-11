package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	arxiv "github.com/lantos1618/arxiv.gg"
)

func TestAPIContractValidatesModesAndCategories(t *testing.T) {
	for raw, want := range map[string]searchMode{
		"quick": searchModeQuick, "keyword": searchModeKeyword, "semantic": searchModeSemantic,
		"search": searchModeSemantic, "deep": searchModeDeep,
	} {
		got, err := parseSearchMode(raw, searchModeQuick)
		if err != nil || got != want {
			t.Fatalf("parseSearchMode(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := parseSearchMode("full-paper", searchModeQuick); err == nil {
		t.Fatal("legacy full-paper alias should not be accepted by API/MCP contracts")
	}
	for _, category := range []string{"cs.AI", "cond-mat.mtrl-sci", "astro-ph"} {
		if got, err := validateSearchCategory(category); err != nil || got != category {
			t.Fatalf("validateSearchCategory(%q) = %q, %v", category, got, err)
		}
	}
	if _, err := validateSearchCategory("cs.AI math.AG"); err == nil {
		t.Fatal("multiple categories should be rejected")
	}
}

func TestSemanticFallbackUsesPartialStatusAndStructuredRetry(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/semantic?q=attention&category=cs.AI", nil)
	rec := httptest.NewRecorder()
	(&server{cache: cache}).handleAPISearchSemantic(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status=%d, want %d; body=%s", rec.Code, http.StatusPartialContent, rec.Body.String())
	}
	var response struct {
		Data struct {
			Mode          searchMode `json:"mode"`
			RequestedMode searchMode `json:"requestedMode"`
			Fallback      struct {
				Used       bool   `json:"used"`
				ReasonCode string `json:"reasonCode"`
			} `json:"fallback"`
			Retry struct {
				Recommended bool `json:"recommended"`
				After       int  `json:"afterSeconds"`
			} `json:"retry"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Data.Mode != searchModeQuick || response.Data.RequestedMode != searchModeSemantic || !response.Data.Fallback.Used || response.Data.Fallback.ReasonCode == "" || response.Data.Retry.Recommended || response.Data.Retry.After != 0 {
		t.Fatalf("unexpected fallback contract: %#v", response.Data)
	}
}

func TestSemanticExecutionIsExplicitAndDoesNotCreateOrphanJobs(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	server := &server{cache: cache}
	if server.qwenExecutionConfigured() {
		t.Fatal("Qwen execution must default to unavailable")
	}
	if _, err := server.generateQwenQueryEmbedding(t.Context(), "attention mechanisms"); !errors.Is(err, errQwenQueryEmbeddingUnavailable) {
		t.Fatalf("generateQwenQueryEmbedding error=%v, want unavailable", err)
	}
	status, err := cache.QwenQueryEmbeddingStatus(t.Context(), "attention mechanisms")
	if err != nil {
		t.Fatalf("load query status: %v", err)
	}
	if status.QueuedJobs != 0 || len(status.Jobs) != 0 {
		t.Fatalf("unavailable execution created jobs: %#v", status)
	}

	server.qwenAsyncWorkerEnabled = true
	if !server.qwenExecutionConfigured() {
		t.Fatal("enabled async worker must configure Qwen execution")
	}
}

func TestSemanticStreamDefaultsToQuickAndUsesTruthfulFallback(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	recorder := httptest.NewRecorder()
	(&server{cache: cache}).handleAPISearchStream(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/search/stream?q=attention&mode=semantic", nil))
	body := recorder.Body.String()
	for _, expected := range []string{`"type":"fallback"`, `"effectiveMode":"quick"`, `"recommended":false`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("stream response missing %s: %s", expected, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "gpu") {
		t.Fatalf("stream must not claim a GPU is starting: %s", body)
	}
}

func TestLegacyBulkEmbeddingEndpointIsGone(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/generate", nil)
	rec := httptest.NewRecorder()
	(&server{localMode: true}).handleAPIGenerateEmbeddings(rec, req)
	if rec.Code != http.StatusGone {
		t.Fatalf("status=%d, want %d; body=%s", rec.Code, http.StatusGone, rec.Body.String())
	}
}

func TestQwenPipelineStatusHandlesMissingCache(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/status", nil)
	rec := httptest.NewRecorder()
	(&server{}).handleAPIEmbeddingWorkerStatus(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d; body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

func TestQwenPipelineStatusRequiresCompatibleReadyService(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	for name, health := range map[string]string{
		"wrong model": `{"ready":true,"model":"all-MiniLM-L6-v2","dimension":384}`,
		"loading":     `{"ready":false,"model":"Qwen/Qwen3-Embedding-8B","dimension":1024}`,
	} {
		t.Run(name, func(t *testing.T) {
			service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(health))
			}))
			defer service.Close()

			recorder := httptest.NewRecorder()
			(&server{cache: cache, qwenEmbeddingServiceURL: service.URL}).handleAPIEmbeddingWorkerStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/embeddings/status", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response struct {
				Data struct {
					Available bool   `json:"available"`
					State     string `json:"state"`
					Execution struct {
						SynchronousUp bool `json:"synchronousUp"`
					} `json:"execution"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Data.Available || response.Data.Execution.SynchronousUp || response.Data.State != "synchronous-service-down" {
				t.Fatalf("incompatible service reported ready: %#v", response.Data)
			}
		})
	}
}
