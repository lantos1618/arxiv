package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arxiv "github.com/lantos1618/arxiv.gg"
)

func TestMCPMetadataEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()

	(&server{}).handleMCP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	for _, want := range []string{`"name":"arxiv_gg"`, `/mcp`, `/api/v1/`} {
		if !strings.Contains(body, want) {
			t.Fatalf("MCP metadata missing %q in %s", want, body)
		}
	}
	if !strings.Contains(body, `"transport":"http-post-jsonrpc"`) || !strings.Contains(body, `"streaming":false`) {
		t.Fatalf("MCP metadata does not describe the actual transport: %s", body)
	}
	if strings.Contains(body, `"transport":"streamable-http"`) {
		t.Fatalf("MCP metadata overclaims streamable HTTP: %s", body)
	}
}

func TestMCPMetadataEndpointRejectsSSEStream(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	(&server{}).handleMCP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestMCPInitializeAndToolsList(t *testing.T) {
	srv := &server{}

	initBody := postMCP(t, srv, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	})
	for _, want := range []string{`"protocolVersion"`, `"name":"arxiv_gg"`, `"tools"`} {
		if !strings.Contains(initBody, want) {
			t.Fatalf("initialize response missing %q in %s", want, initBody)
		}
	}

	toolsBody := postMCP(t, srv, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	for _, want := range []string{"arxiv_api_overview", "arxiv_account", "arxiv_search", "arxiv_get_paper", "arxiv_related_papers", "arxiv_citations", "arxiv_cited_by"} {
		if !strings.Contains(toolsBody, want) {
			t.Fatalf("tools/list missing %q in %s", want, toolsBody)
		}
	}
}

func TestMCPNotificationAccepted(t *testing.T) {
	srv := &server{}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	srv.handleMCP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("notification response body = %q, want empty", rec.Body.String())
	}
}

func TestMCPRejectsOversizedBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat(" ", mcpMaxBodyBytes+1)))
	rec := httptest.NewRecorder()

	(&server{}).handleMCP(rec, req)

	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "request body too large") {
		t.Fatalf("status/body = %d %q", rec.Code, rec.Body.String())
	}
}

func TestMCPRejectsInvalidJSONRPCEnvelopes(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "missing version", payload: `{"id":1,"method":"ping"}`, want: "jsonrpc must be 2.0"},
		{name: "wrong version", payload: `{"jsonrpc":"1.0","id":1,"method":"ping"}`, want: "jsonrpc must be 2.0"},
		{name: "missing method", payload: `{"jsonrpc":"2.0","id":1}`, want: "method is required"},
		{name: "boolean id", payload: `{"jsonrpc":"2.0","id":true,"method":"ping"}`, want: "id must be a string, number, or null"},
		{name: "scalar params", payload: `{"jsonrpc":"2.0","id":1,"method":"ping","params":1}`, want: "params must be an object or array"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(test.payload))
			rec := httptest.NewRecorder()
			(&server{}).handleMCP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.want) {
				t.Fatalf("status/body = %d %q, want %q", rec.Code, rec.Body.String(), test.want)
			}
		})
	}
}

func TestMCPRejectsEmptyAndOversizedBatches(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload any
		want    string
	}{
		{name: "empty", payload: []any{}, want: "batch must not be empty"},
		{name: "too many", payload: makeMCPPings(mcpMaxBatchSize + 1), want: "batch contains too many requests"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(test.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			(&server{}).handleMCP(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), test.want) {
				t.Fatalf("status/body = %d %q, want %q", rec.Code, rec.Body.String(), test.want)
			}
		})
	}
}

func TestMCPRejectsBatchCostBeforeExecution(t *testing.T) {
	requests := make([]map[string]any, 4)
	for index := range requests {
		requests[index] = map[string]any{
			"jsonrpc": "2.0",
			"id":      index + 1,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "arxiv_search",
				"arguments": map[string]any{
					"query": "attention",
					"mode":  "semantic",
					"limit": 50,
				},
			},
		}
	}
	body, _ := json.Marshal(requests)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	(&server{}).handleMCP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "batch cost limit exceeded") {
		t.Fatalf("status/body = %d %q", rec.Code, rec.Body.String())
	}
}

func makeMCPPings(count int) []map[string]any {
	requests := make([]map[string]any, count)
	for index := range requests {
		requests[index] = map[string]any{"jsonrpc": "2.0", "id": index + 1, "method": "ping"}
	}
	return requests
}

func TestMCPAccountToolWithAPIKey(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	user, err := cache.FindOrCreateUser(ctx, "agent@example.edu", "Agent Reader", "", true, "google", time.Now().UTC())
	if err != nil {
		t.Fatalf("FindOrCreateUser: %v", err)
	}
	_, rawKey, _, err := cache.EnsureUserAPIKey(ctx, user.ID, "Agent access")
	if err != nil {
		t.Fatalf("EnsureUserAPIKey: %v", err)
	}

	body := postMCPWithHeaders(t, &server{cache: cache}, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "arxiv_account",
			"arguments": map[string]any{},
		},
	}, map[string]string{"Authorization": "Bearer " + rawKey})
	for _, want := range []string{`"isError":false`, `\"authenticated\": true`, `agent@example.edu`} {
		if !strings.Contains(body, want) {
			t.Fatalf("account tool response missing %q in %s", want, body)
		}
	}
}

func TestAPIRootDoesNotCreateAPIKeyForSignedInUser(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	user, err := cache.FindOrCreateUser(ctx, "agent@example.edu", "Agent Reader", "", true, "google", time.Now().UTC())
	if err != nil {
		t.Fatalf("FindOrCreateUser: %v", err)
	}
	sessionToken, err := cache.CreateUserSession(ctx, user.ID, "127.0.0.1", "test-agent", time.Minute)
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionToken})
	rec := httptest.NewRecorder()

	(&server{cache: cache}).handleAPIRoot(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`data-api-key=""`, `Open`, `arxiv_account`} {
		if !strings.Contains(body, want) {
			t.Fatalf("API root missing %q in body", want)
		}
	}
	key, err := cache.ActiveUserAPIKey(ctx, user.ID, "Agent access")
	if err != nil {
		t.Fatalf("ActiveUserAPIKey: %v", err)
	}
	if key != nil {
		t.Fatalf("GET /api/v1/ silently created key %q", key.ID)
	}
}

func TestMCPSearchToolOnEmptyCache(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	body := postMCP(t, &server{cache: cache}, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "arxiv_search",
			"arguments": map[string]any{
				"query": "attention",
				"mode":  "quick",
				"limit": 5,
			},
		},
	})
	for _, want := range []string{`"isError":false`, `\"mode\": \"quick\"`, `\"count\": 0`} {
		if !strings.Contains(body, want) {
			t.Fatalf("search tool response missing %q in %s", want, body)
		}
	}
}

func TestMCPSearchRejectsUnknownModeAndInvalidCategory(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	for _, arguments := range []map[string]any{
		{"query": "attention", "mode": "surprise"},
		{"query": "attention", "mode": "quick", "category": "cs.AI OR 1=1"},
	} {
		body := postMCP(t, &server{cache: cache}, map[string]any{
			"jsonrpc": "2.0", "id": 9, "method": "tools/call",
			"params": map[string]any{"name": "arxiv_search", "arguments": arguments},
		})
		if !strings.Contains(body, `"isError":true`) {
			t.Fatalf("invalid search arguments were accepted: %s", body)
		}
	}
}

func TestMCPSemanticFallbackHasStructuredMetadata(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	body := postMCP(t, &server{cache: cache}, map[string]any{
		"jsonrpc": "2.0", "id": 10, "method": "tools/call",
		"params": map[string]any{
			"name":      "arxiv_search",
			"arguments": map[string]any{"query": "attention", "mode": "semantic", "category": "cs.AI"},
		},
	})
	for _, want := range []string{`"structuredContent"`, `"requestedMode":"semantic"`, `"reasonCode":"semantic_unavailable"`, `"afterSeconds":30`} {
		if !strings.Contains(body, want) {
			t.Fatalf("semantic fallback missing %q in %s", want, body)
		}
	}
}

func TestMCPCitationToolsReturnExplicitCachedCoverage(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	for _, tool := range []string{"arxiv_citations", "arxiv_cited_by"} {
		body := postMCP(t, &server{cache: cache}, map[string]any{
			"jsonrpc": "2.0", "id": 11, "method": "tools/call",
			"params": map[string]any{"name": tool, "arguments": map[string]any{"id": "2501.00001"}},
		})
		for _, want := range []string{`"isError":false`, `"globalCitationIndex":false`, `"paperId":"2501.00001"`} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s result missing %q in %s", tool, want, body)
			}
		}
	}
}

func postMCP(t *testing.T, srv *server, payload map[string]any) string {
	t.Helper()
	return postMCPWithHeaders(t, srv, payload, nil)
}

func postMCPWithHeaders(t *testing.T, srv *server, payload map[string]any, headers map[string]string) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()

	srv.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	return rec.Body.String()
}
