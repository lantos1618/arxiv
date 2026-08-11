package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	arxiv "github.com/lantos1618/arxiv.gg"
)

const (
	mcpProtocolVersion = "2025-06-18"
	mcpMaxBodyBytes    = 256 << 10
	mcpMaxBatchSize    = 20
	mcpMaxBatchCost    = 24
)

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	hasID   bool
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

func (req *mcpRequest) UnmarshalJSON(data []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope == nil {
		return fmt.Errorf("request must be an object")
	}
	if raw, ok := envelope["jsonrpc"]; ok {
		if err := json.Unmarshal(raw, &req.JSONRPC); err != nil {
			return fmt.Errorf("jsonrpc must be a string")
		}
	}
	if raw, ok := envelope["method"]; ok {
		if err := json.Unmarshal(raw, &req.Method); err != nil {
			return fmt.Errorf("method must be a string")
		}
	}
	if raw, ok := envelope["params"]; ok {
		req.Params = append(req.Params[:0], raw...)
	}
	if raw, ok := envelope["id"]; ok {
		req.hasID = true
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		if err := decoder.Decode(&req.ID); err != nil {
			return fmt.Errorf("invalid id")
		}
	}
	return nil
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		var account map[string]any
		if user, ok := s.currentUser(r); ok {
			account = mcpAccountSummary(user)
		}
		if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "SSE transport is unavailable for this request", http.StatusMethodNotAllowed)
			return
		}
		respondJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data: map[string]any{
				"name":        "arxiv_gg",
				"transport":   "http-post-jsonrpc",
				"streaming":   false,
				"methods":     []string{http.MethodPost},
				"url":         canonicalURL("/mcp"),
				"api":         canonicalURL("/api/v1/"),
				"account":     account,
				"description": "MCP JSON-RPC tools over HTTP POST for cached paper search, metadata, citations, and Qwen related-work maps. SSE sessions are not supported.",
			},
		})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := mcpUserFromRequest(s, r)

	body, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxBodyBytes+1))
	if err != nil {
		writeMCPError(w, nil, -32700, "read request failed")
		return
	}
	if len(body) > mcpMaxBodyBytes {
		writeMCPError(w, nil, -32600, "request body too large")
		return
	}
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		writeMCPError(w, nil, -32600, "empty request")
		return
	}
	if !json.Valid(body) {
		writeMCPError(w, nil, -32700, "invalid JSON")
		return
	}

	if body[0] == '[' {
		var rawRequests []json.RawMessage
		if err := json.Unmarshal(body, &rawRequests); err != nil {
			writeMCPError(w, nil, -32600, "invalid request")
			return
		}
		if len(rawRequests) == 0 {
			writeMCPError(w, nil, -32600, "batch must not be empty")
			return
		}
		if len(rawRequests) > mcpMaxBatchSize {
			writeMCPError(w, nil, -32600, "batch contains too many requests")
			return
		}
		requests := make([]mcpRequest, len(rawRequests))
		validationErrors := make([]*mcpError, len(rawRequests))
		totalCost := 0
		for index, raw := range rawRequests {
			if err := json.Unmarshal(raw, &requests[index]); err != nil {
				validationErrors[index] = &mcpError{Code: -32600, Message: "invalid request"}
				continue
			}
			if validationErr := validateMCPRequest(requests[index]); validationErr != nil {
				validationErrors[index] = validationErr
				continue
			}
			totalCost += mcpRequestCost(requests[index])
		}
		if totalCost > mcpMaxBatchCost {
			writeMCPError(w, nil, -32600, "batch cost limit exceeded")
			return
		}
		responses := make([]mcpResponse, 0, len(requests))
		for index, req := range requests {
			if validationErr := validationErrors[index]; validationErr != nil {
				responses = append(responses, mcpErrorResponse(req.ID, validationErr.Code, validationErr.Message))
				continue
			}
			resp := s.mcpResponseForRequest(r.Context(), req, user)
			if req.hasID {
				responses = append(responses, resp)
			}
		}
		if len(responses) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(responses)
		return
	}

	var req mcpRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeMCPError(w, nil, -32600, "invalid request")
		return
	}
	if validationErr := validateMCPRequest(req); validationErr != nil {
		writeMCPError(w, req.ID, validationErr.Code, validationErr.Message)
		return
	}
	resp := s.mcpResponseForRequest(r.Context(), req, user)
	if !req.hasID {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

func mcpUserFromRequest(s *server, r *http.Request) *arxiv.User {
	if user, ok := s.currentUser(r); ok {
		return user
	}
	return nil
}

func mcpAccountSummary(user *arxiv.User) map[string]any {
	if user == nil {
		return map[string]any{"authenticated": false}
	}
	return map[string]any{
		"authenticated": true,
		"id":            user.ID,
		"email":         user.Email,
		"name":          user.Name,
		"plan":          user.Plan,
		"signIn":        user.AuthProvider,
	}
}

func (s *server) mcpResponseForRequest(ctx context.Context, req mcpRequest, user *arxiv.User) mcpResponse {
	switch req.Method {
	case "initialize":
		return mcpResultResponse(req.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "arxiv_gg",
				"version": buildCommitLabel(),
			},
			"instructions": "Use arxiv_get_paper for exact IDs, arxiv_search for discovery, arxiv_citations/arxiv_cited_by for cached citation edges, and arxiv_related_papers for Qwen abstract maps. Deep search and account details require authentication. This endpoint uses JSON-RPC over HTTP POST and does not provide SSE sessions.",
		})
	case "ping":
		return mcpResultResponse(req.ID, map[string]any{})
	case "tools/list":
		return mcpResultResponse(req.ID, map[string]any{"tools": mcpTools()})
	case "tools/call":
		result, err := s.callMCPTool(ctx, req.Params, user)
		if err != nil {
			return mcpResultResponse(req.ID, mcpToolTextResult(err.Error(), true))
		}
		return mcpResultResponse(req.ID, result)
	default:
		return mcpErrorResponse(req.ID, -32601, "method not found")
	}
}

func validateMCPRequest(req mcpRequest) *mcpError {
	if req.JSONRPC != "2.0" {
		return &mcpError{Code: -32600, Message: "jsonrpc must be 2.0"}
	}
	if strings.TrimSpace(req.Method) == "" {
		return &mcpError{Code: -32600, Message: "method is required"}
	}
	if req.hasID {
		switch req.ID.(type) {
		case nil, string, json.Number:
		default:
			return &mcpError{Code: -32600, Message: "id must be a string, number, or null"}
		}
	}
	if len(req.Params) > 0 {
		params := bytesTrimSpace(req.Params)
		if len(params) == 0 || (params[0] != '{' && params[0] != '[') {
			return &mcpError{Code: -32602, Message: "params must be an object or array"}
		}
	}
	return nil
}

func mcpRequestCost(req mcpRequest) int {
	if req.Method != "tools/call" {
		return 1
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(req.Params, &call) != nil {
		return 1
	}
	var args struct {
		Mode  string `json:"mode"`
		Limit int    `json:"limit"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	units := clampInt(args.Limit, 10, 1, 80)
	units = (units + 9) / 10
	switch call.Name {
	case "arxiv_search":
		mode, _ := parseSearchMode(args.Mode, searchModeQuick)
		if mode == searchModeSemantic || mode == searchModeDeep {
			return 2 + 3*units
		}
		return 1 + units
	case "arxiv_related_papers":
		return 1 + units
	default:
		return 1
	}
}

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}

func mcpTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "arxiv_api_overview",
			"description": "Return exact REST/MCP transport, authentication, search-mode, fallback, queue, and endpoint contracts.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "arxiv_account",
			"description": "Return the authenticated arXiv.gg account attached to this session or API key.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "arxiv_search",
			"description": "Search arXiv.gg papers by Quick lookup, keyword/full-text, semantic abstract search, or authenticated full-paper search when Qwen vectors are available.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"query"},
				"properties": map[string]any{
					"query":    map[string]any{"type": "string", "description": "Search query, arXiv ID, author, keyword, or research idea."},
					"mode":     map[string]any{"type": "string", "enum": []string{"quick", "keyword", "semantic", "deep"}, "description": "Search mode. Defaults to quick. Deep requires account auth."},
					"category": map[string]any{"type": "string", "description": "Optional arXiv category filter, such as cs.AI."},
					"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum results. Defaults to 10."},
				},
			},
		},
		{
			"name":        "arxiv_get_paper",
			"description": "Get cached paper metadata by arXiv ID.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "arXiv ID or arXiv URL."},
				},
			},
		},
		{
			"name":        "arxiv_related_papers",
			"description": "Get semantically related papers and map data for a paper with a Qwen abstract profile.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id":    map[string]any{"type": "string", "description": "arXiv ID or arXiv URL."},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 80, "description": "Maximum related papers. Defaults to 20."},
				},
			},
		},
		{
			"name":        "arxiv_citations",
			"description": "List cached arXiv references cited by a paper. Results only include citation edges extracted into arXiv.gg.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{"type": "string", "description": "arXiv ID or arXiv URL."},
				},
			},
		},
		{
			"name":        "arxiv_cited_by",
			"description": "List cached papers that cite an arXiv paper. This is cached-corpus coverage, not a global citation index.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id":    map[string]any{"type": "string", "description": "arXiv ID or arXiv URL."},
					"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Maximum citing papers. Defaults to 50."},
				},
			},
		},
	}
}

func (s *server) callMCPTool(ctx context.Context, params json.RawMessage, user *arxiv.User) (map[string]any, error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, fmt.Errorf("invalid tools/call params")
	}

	switch call.Name {
	case "arxiv_api_overview":
		data := map[string]any{
			"baseUrl": canonicalURL("/"),
			"apiUrl":  canonicalURL("/api/v1/"),
			"mcp": map[string]any{
				"url": canonicalURL("/mcp"), "transport": "HTTP POST with JSON-RPC 2.0", "sseSessions": false,
			},
			"login": canonicalURL("/login"),
			"account": map[string]any{
				"authenticated": user != nil,
			},
			"tools": []string{"arxiv_api_overview", "arxiv_account", "arxiv_search", "arxiv_get_paper", "arxiv_citations", "arxiv_cited_by", "arxiv_related_papers"},
			"search": map[string]any{
				"modes":            []string{"quick", "keyword", "semantic", "deep"},
				"category":         "Optional single arXiv category; applied in every mode.",
				"semanticModel":    arxiv.QwenEmbeddingModel,
				"deepAuth":         true,
				"fallbackContract": "When semantic search is unavailable, results identify requestedMode/effectiveMode plus structured fallback and retry metadata.",
			},
			"restEndpoints": []map[string]any{
				{"method": "GET", "path": "/api/v1/papers/{id}", "auth": "public", "description": "Cached metadata plus cached references and cited-by count."},
				{"method": "GET", "path": "/api/v1/papers/{id}/citations", "auth": "public", "description": "Cached extracted arXiv references."},
				{"method": "GET", "path": "/api/v1/papers/{id}/cited-by", "auth": "public", "description": "Cached citing papers; limit 1..200."},
				{"method": "GET", "path": "/api/v1/search", "auth": "public", "description": "Keyword search; q, category, limit."},
				{"method": "GET", "path": "/api/v1/search/semantic", "auth": "public", "description": "Qwen abstract semantic search; may return HTTP 206 with explicit fallback metadata."},
				{"method": "GET", "path": "/api/v1/search/stream", "auth": "deep mode requires account", "description": "SSE result events for quick, keyword, semantic, or deep mode."},
				{"method": "POST", "path": "/api/v1/papers/{id}/embeddings", "auth": "rate limited", "description": "Generate or queue the Qwen paper profile; HTTP 202 includes statusUrl when queued."},
			},
			"notes": "MCP discovery/search/citation tools are public. Deep search and account details require a session cookie or bearer API key. Worker and administrative REST routes have separate authorization.",
		}
		if user != nil {
			data["account"] = mcpAccountSummary(user)
		}
		return mcpToolJSONResult(data)
	case "arxiv_account":
		if user == nil {
			return nil, fmt.Errorf("account auth required; pass Authorization: Bearer <api key> or sign in in the browser")
		}
		return mcpToolJSONResult(mcpAccountSummary(user))
	case "arxiv_search":
		return s.callMCPSearch(ctx, call.Arguments, user)
	case "arxiv_get_paper":
		return s.callMCPGetPaper(ctx, call.Arguments)
	case "arxiv_related_papers":
		return s.callMCPRelatedPapers(ctx, call.Arguments)
	case "arxiv_citations":
		return s.callMCPCitations(ctx, call.Arguments)
	case "arxiv_cited_by":
		return s.callMCPCitedBy(ctx, call.Arguments)
	default:
		return nil, fmt.Errorf("unknown tool %q", call.Name)
	}
}

func (s *server) callMCPSearch(ctx context.Context, raw json.RawMessage, user *arxiv.User) (map[string]any, error) {
	var args struct {
		Query    string `json:"query"`
		Mode     string `json:"mode"`
		Category string `json:"category"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arxiv_search arguments")
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return nil, fmt.Errorf("query is required")
	}
	limit := clampInt(args.Limit, 10, 1, 50)

	mode, err := parseSearchMode(args.Mode, searchModeQuick)
	if err != nil {
		return nil, err
	}
	category, err := validateSearchCategory(args.Category)
	if err != nil {
		return nil, err
	}

	var data any
	switch mode {
	case searchModeSemantic:
		data, err = s.semanticSearchForMCP(ctx, args.Query, category, limit)
		if err != nil {
			papers, total, searchErr := quickSearchPapers(ctx, s.cache, args.Query, category, limit)
			if searchErr != nil {
				return nil, searchErr
			}
			reasonCode := "semantic_unavailable"
			retryAfter := 30
			if errors.Is(err, errQwenQueryEmbeddingQueued) {
				reasonCode = "query_embedding_queued"
				retryAfter = qwenQueryRetryAfterSeconds
			}
			data = map[string]any{
				"mode": searchModeQuick, "requestedMode": searchModeSemantic, "category": category,
				"fallback": map[string]any{"used": true, "reasonCode": reasonCode, "notice": err.Error()},
				"retry":    map[string]any{"recommended": true, "afterSeconds": retryAfter},
				"count":    len(papers), "total": total, "papers": papers,
			}
		}
	case searchModeKeyword:
		papers, searchErr := s.cache.Search(ctx, args.Query, category, limit)
		if searchErr != nil {
			return nil, searchErr
		}
		data = map[string]any{"mode": searchModeKeyword, "category": category, "count": len(papers), "papers": papers}
	case searchModeDeep:
		if user == nil {
			return nil, fmt.Errorf("deep search requires account auth; pass Authorization: Bearer <api key> or sign in in the browser")
		}
		data, err = s.deepSearchForMCP(ctx, args.Query, category, limit)
		if err != nil {
			return nil, err
		}
	case searchModeQuick:
		papers, total, searchErr := quickSearchPapers(ctx, s.cache, args.Query, category, limit)
		if searchErr != nil {
			return nil, searchErr
		}
		data = map[string]any{"mode": searchModeQuick, "category": category, "count": len(papers), "total": total, "papers": papers}
	}
	return mcpToolJSONResult(data)
}

func (s *server) deepSearchForMCP(ctx context.Context, query, category string, limit int) (map[string]any, error) {
	ready, err := s.searchModeReady(ctx, searchModeDeep)
	if err != nil {
		return nil, err
	}
	if !ready {
		return nil, fmt.Errorf("deep search is warming up; try quick mode")
	}
	embedding, err := s.generateQwenQueryEmbedding(ctx, query)
	if err != nil {
		return nil, err
	}
	candidateLimit := limit
	if category != "" {
		candidateLimit = min(limit*4, 200)
	}
	results, err := s.cache.SearchDeepQwen(ctx, embedding, candidateLimit)
	if err != nil {
		return nil, err
	}
	results = filterDeepResultsByCategory(results, category, limit)
	return map[string]any{"mode": searchModeDeep, "category": category, "model": arxiv.QwenEmbeddingModel, "count": len(results), "results": results, "metadata": deepMetadataCoverage(results)}, nil
}

func (s *server) semanticSearchForMCP(ctx context.Context, query, category string, limit int) (map[string]any, error) {
	stats, err := s.cache.Stats(ctx)
	if err != nil {
		return nil, err
	}
	if stats.QwenEmbeddingsCount == 0 {
		return nil, fmt.Errorf("semantic search is warming up; using Quick fallback")
	}
	embedding, err := s.generateQwenQueryEmbedding(ctx, query)
	if err != nil {
		return nil, err
	}
	candidateLimit := limit
	if category != "" {
		candidateLimit = min(limit*4, 200)
	}
	results, err := s.cache.SearchSemanticQwen(ctx, embedding, candidateLimit)
	if err != nil {
		return nil, err
	}
	results = filterSemanticResultsByCategory(results, category, limit)
	return map[string]any{"mode": searchModeSemantic, "category": category, "model": arxiv.QwenEmbeddingModel, "count": len(results), "results": results, "metadata": semanticMetadataCoverage(results)}, nil
}

func (s *server) callMCPGetPaper(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arxiv_get_paper arguments")
	}
	id := extractArxivID(args.ID)
	if id == "" {
		id = strings.TrimSpace(args.ID)
	}
	id = stripArxivVersion(id)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}

	paper, err := s.cache.GetPaper(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("paper %s not found", id)
	}
	return mcpToolJSONResult(map[string]any{
		"paper":        paper,
		"url":          canonicalURL(paperPath(paper.ID)),
		"hasEmbedding": s.cache.HasQwenEmbedding(ctx, paper.ID),
	})
}

func (s *server) callMCPRelatedPapers(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var args struct {
		ID    string `json:"id"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arxiv_related_papers arguments")
	}
	id := extractArxivID(args.ID)
	if id == "" {
		id = strings.TrimSpace(args.ID)
	}
	id = stripArxivVersion(id)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	if !s.cache.HasQwenEmbedding(ctx, id) {
		return nil, fmt.Errorf("paper %s is not ready for related-work maps", id)
	}

	limit := clampInt(args.Limit, 20, 1, 80)
	semanticMap, results, err := s.cache.SimilarPaperMapQwen(ctx, id, limit)
	if err != nil {
		return nil, err
	}
	return mcpToolJSONResult(map[string]any{
		"paperId":  id,
		"model":    arxiv.QwenEmbeddingModel,
		"count":    len(results),
		"results":  results,
		"map":      semanticMap,
		"metadata": semanticMetadataCoverage(results),
	})
}

func (s *server) callMCPCitations(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	id, err := mcpPaperIDArgument(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid arxiv_citations arguments: %w", err)
	}
	references, err := s.cache.References(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("load cached references for %s: %w", id, err)
	}
	return mcpToolJSONResult(map[string]any{
		"paperId": id,
		"coverage": map[string]any{
			"scope":               "cached extracted arXiv-ID references",
			"globalCitationIndex": false,
		},
		"count":      len(references),
		"references": references,
	})
}

func (s *server) callMCPCitedBy(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var args struct {
		ID    string `json:"id"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, fmt.Errorf("invalid arxiv_cited_by arguments")
	}
	id := normalizeMCPPaperID(args.ID)
	if id == "" {
		return nil, fmt.Errorf("id is required")
	}
	limit := clampInt(args.Limit, 50, 1, 200)
	papers, err := s.cache.CitedBy(ctx, id, limit)
	if err != nil {
		return nil, fmt.Errorf("load cached citing papers for %s: %w", id, err)
	}
	return mcpToolJSONResult(map[string]any{
		"paperId": id,
		"coverage": map[string]any{
			"scope":               "papers and citation edges cached by arXiv.gg",
			"globalCitationIndex": false,
		},
		"count":  len(papers),
		"limit":  limit,
		"papers": papers,
	})
}

func mcpPaperIDArgument(raw json.RawMessage) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	id := normalizeMCPPaperID(args.ID)
	if id == "" {
		return "", fmt.Errorf("id is required")
	}
	return id, nil
}

func normalizeMCPPaperID(value string) string {
	id := extractArxivID(value)
	if id == "" {
		id = strings.TrimSpace(value)
	}
	return stripArxivVersion(id)
}

func mcpToolJSONResult(value any) (map[string]any, error) {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	result := mcpToolTextResult(string(body), false)
	result["structuredContent"] = value
	return result, nil
}

func mcpToolTextResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []mcpToolContent{{Type: "text", Text: text}},
		"isError": isError,
	}
}

func mcpResultResponse(id any, result any) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func mcpErrorResponse(id any, code int, message string) mcpResponse {
	return mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message}}
}

func writeMCPError(w http.ResponseWriter, id any, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(mcpErrorResponse(id, code, message))
}

func clampInt(value, fallback, minValue, maxValue int) int {
	if value == 0 {
		value = fallback
	}
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
