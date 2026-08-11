package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRespondAPIInternalErrorDoesNotExposeDetail(t *testing.T) {
	rec := httptest.NewRecorder()
	detail := "dial postgres host=db.internal password=super-secret: connection refused"
	respondAPIInternalError(rec, http.StatusServiceUnavailable, "test database operation", "service temporarily unavailable", errors.New(detail))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if strings.Contains(rec.Body.String(), detail) || strings.Contains(rec.Body.String(), "super-secret") || strings.Contains(rec.Body.String(), "db.internal") {
		t.Fatalf("response exposed internal detail: %s", rec.Body.String())
	}

	var response APIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Success || response.Error != "service temporarily unavailable" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestParseAuthorParameterRejectsOversizedCacheKeys(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/authors/stats?author="+strings.Repeat("x", 257), nil)
	rec := httptest.NewRecorder()
	if _, ok := parseAuthorParameter(rec, req); ok {
		t.Fatal("oversized author parameter was accepted")
	}
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "256 bytes") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthorGraphBuildRejectsConcurrentRun(t *testing.T) {
	server := &server{localMode: true}
	server.authorGraphBuildMu.Lock()
	defer server.authorGraphBuildMu.Unlock()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authors/graph/build", nil)
	rec := httptest.NewRecorder()
	server.handleAPIBuildAuthorGraph(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already running") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPDFSearchRequiresAuthentication(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/pdf?q=secret", nil)
	rec := httptest.NewRecorder()
	(&server{}).handleAPISearchPDF(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "sign in") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
