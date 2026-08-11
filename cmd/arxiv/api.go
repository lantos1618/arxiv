package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lantos1618/arxiv.gg"
	"gorm.io/gorm"
)

// APIResponse is a standard API response wrapper
type APIResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
}

type paperSummary struct {
	ID         string    `json:"id"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
	Title      string    `json:"title"`
	Authors    string    `json:"authors"`
	Categories string    `json:"categories"`
	DOI        string    `json:"doi,omitempty"`
}

type similarPaperResult struct {
	PaperID    string        `json:"paperId"`
	Similarity float64       `json:"similarity"`
	Paper      *paperSummary `json:"paper,omitempty"`
}

type searchMode string

const (
	searchModeQuick    searchMode = "quick"
	searchModeKeyword  searchMode = "keyword"
	searchModeSemantic searchMode = "semantic"
	searchModeDeep     searchMode = "deep"
)

var arxivCategoryPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(?:\.[A-Za-z0-9-]+)?$`)

func parseSearchMode(raw string, fallback searchMode) (searchMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return fallback, nil
	case "quick":
		return searchModeQuick, nil
	case "keyword":
		return searchModeKeyword, nil
	case "semantic", "search":
		return searchModeSemantic, nil
	case "deep":
		return searchModeDeep, nil
	default:
		return "", fmt.Errorf("invalid search mode; use quick, keyword, semantic, or deep")
	}
}

func validateSearchCategory(category string) (string, error) {
	category = strings.TrimSpace(category)
	if category == "" {
		return "", nil
	}
	if len(category) > 64 || !arxivCategoryPattern.MatchString(category) {
		return "", fmt.Errorf("invalid category; use one arXiv category such as cs.AI")
	}
	return category, nil
}

func paperInCategory(categories, category string) bool {
	if category == "" {
		return true
	}
	for _, candidate := range strings.Fields(categories) {
		if candidate == category {
			return true
		}
	}
	return false
}

type qwenWorkerClaimRequest struct {
	Kinds        []string `json:"kinds"`
	Limit        int      `json:"limit"`
	LeaseOwner   string   `json:"leaseOwner"`
	LeaseSeconds int      `json:"leaseSeconds"`
}

type qwenWorkerClaimedJob struct {
	ID              string `json:"id"`
	PaperID         string `json:"paperId"`
	QueryHash       string `json:"queryHash,omitempty"`
	Kind            string `json:"kind"`
	Scope           string `json:"scope"`
	Model           string `json:"model"`
	Dim             int    `json:"dim"`
	Text            string `json:"text"`
	TextChars       int    `json:"textChars"`
	TokenEstimate   int    `json:"tokenEstimate"`
	Attempts        int    `json:"attempts"`
	LeaseOwner      string `json:"leaseOwner"`
	LeaseGeneration int    `json:"leaseGeneration"`
	SourceHash      string `json:"sourceHash"`
}

type qwenWorkerCompleteRequest struct {
	Embedding       []float32 `json:"embedding"`
	LeaseOwner      string    `json:"leaseOwner,omitempty"`
	LeaseGeneration int       `json:"leaseGeneration,omitempty"`
	SourceHash      string    `json:"sourceHash,omitempty"`
}

type qwenWorkerFailRequest struct {
	Error           string `json:"error"`
	LeaseOwner      string `json:"leaseOwner,omitempty"`
	LeaseGeneration int    `json:"leaseGeneration,omitempty"`
}

type qwenWorkerHeartbeatRequest struct {
	LeaseOwner      string `json:"leaseOwner"`
	LeaseGeneration int    `json:"leaseGeneration"`
	LeaseSeconds    int    `json:"leaseSeconds"`
}

const (
	defaultQwenWorkerLeaseSeconds = 900
	maxQwenWorkerLeaseSeconds     = 3600
)

func normalizeQwenWorkerLeaseSeconds(seconds int) int {
	if seconds <= 0 {
		return defaultQwenWorkerLeaseSeconds
	}
	if seconds > maxQwenWorkerLeaseSeconds {
		return maxQwenWorkerLeaseSeconds
	}
	return seconds
}

func qwenWorkerLeasedJobID(jobID string, generation int, sourceHash string) string {
	leasedID := jobID + "~" + strconv.Itoa(generation)
	if sourceHash != "" {
		leasedID += "~" + sourceHash
	}
	return leasedID
}

func parseQwenWorkerLeasedJobID(value string) (string, int, string) {
	value = strings.TrimSpace(value)
	parts := strings.Split(value, "~")
	if len(parts) < 2 {
		return value, 0, ""
	}
	generation, err := strconv.Atoi(parts[1])
	if err != nil || generation <= 0 {
		return value, 0, ""
	}
	sourceHash := ""
	if len(parts) == 3 {
		sourceHash = parts[2]
	}
	return parts[0], generation, sourceHash
}

func summarizePaper(p *arxiv.Paper) *paperSummary {
	if p == nil {
		return nil
	}
	return &paperSummary{
		ID:         p.ID,
		Created:    p.Created,
		Updated:    p.Updated,
		Title:      p.Title,
		Authors:    compactAuthors(p.Authors),
		Categories: p.Categories,
		DOI:        p.DOI,
	}
}

func compactAuthors(authors string) string {
	authors = strings.Join(strings.Fields(authors), " ")
	if authors == "" {
		return ""
	}
	const maxRunes = 260
	runes := []rune(authors)
	if len(runes) <= maxRunes {
		return authors
	}

	parts := strings.Split(authors, ",")
	if len(parts) > 1 {
		visible := parts
		if len(visible) > 8 {
			visible = visible[:8]
		}
		summary := strings.TrimSpace(strings.Join(visible, ",")) + " +" + strconv.Itoa(len(parts)-len(visible)) + " more"
		if len([]rune(summary)) <= maxRunes {
			return summary
		}
	}

	return strings.TrimSpace(string(runes[:maxRunes])) + "..."
}

func summarizeSimilarResults(results []arxiv.SemanticResult) []similarPaperResult {
	summaries := make([]similarPaperResult, len(results))
	for i, result := range results {
		summaries[i] = similarPaperResult{
			PaperID:    result.PaperID,
			Similarity: result.Similarity,
			Paper:      summarizePaper(result.Paper),
		}
	}
	return summaries
}

func semanticMetadataCoverage(results []arxiv.SemanticResult) map[string]interface{} {
	missing := make([]string, 0)
	for _, result := range results {
		if result.Paper == nil {
			missing = append(missing, result.PaperID)
		}
	}
	return map[string]interface{}{"complete": len(missing) == 0, "missingPaperIds": missing}
}

func deepMetadataCoverage(results []arxiv.DeepSearchResult) map[string]interface{} {
	missing := make([]string, 0)
	for _, result := range results {
		if result.Paper == nil {
			missing = append(missing, result.PaperID)
		}
	}
	return map[string]interface{}{"complete": len(missing) == 0, "missingPaperIds": missing}
}

// setSSEHeaders sets headers for Server-Sent Events, including buffering controls
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Accel-Buffering", "no") // Disable nginx buffering
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

// handleAPIPaper returns paper metadata as JSON
// Handles: /api/v1/papers/{id}, /api/v1/papers/{id}/citations, /api/v1/papers/{id}/cited-by, /api/v1/papers/{id}/graph, /api/v1/papers/{id}/fetch, /api/v1/papers/{id}/export/{format}
func (s *server) handleAPIPaper(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	if path == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}

	ctx := r.Context()

	// Handle sub-routes
	if strings.HasSuffix(path, "/citations") {
		s.handleAPICitations(w, r)
		return
	}
	if strings.HasSuffix(path, "/cited-by") {
		s.handleAPICitedBy(w, r)
		return
	}
	if strings.HasSuffix(path, "/graph") {
		s.handleAPICitationGraph(w, r)
		return
	}
	if strings.HasSuffix(path, "/similar") {
		s.handleAPISimilarPapers(w, r)
		return
	}
	if strings.HasSuffix(path, "/embedding-status") {
		s.handleAPIEmbeddingStatus(w, r)
		return
	}
	if strings.HasSuffix(path, "/fetch") {
		s.handleAPIFetch(w, r)
		return
	}
	if strings.HasSuffix(path, "/embeddings") {
		s.handleAPIEmbeddings(w, r)
		return
	}
	if strings.Contains(path, "/export/") {
		s.handleAPIExport(w, r)
		return
	}

	// Default: return paper metadata
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	paper, err := s.cache.GetPaper(ctx, path)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "paper not found",
		})
		return
	}

	citedByCount, _ := s.cache.CitedByCount(ctx, path)
	refs, _ := s.cache.References(ctx, path)

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paper":        paper,
			"citedByCount": citedByCount,
			"references":   refs,
		},
	})
}

// handleAPISearchSemantic handles semantic search API requests.
func (s *server) handleAPISearchSemantic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "query parameter 'q' required",
		})
		return
	}

	limit, err := parseLimit(r, 20, 100)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}
	category, err := validateSearchCategory(r.URL.Query().Get("category"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: err.Error()})
		return
	}

	ctx := r.Context()

	stats, err := s.cache.Stats(ctx)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load semantic search statistics", "search is temporarily unavailable", err)
		return
	}

	if stats.QwenEmbeddingsCount == 0 {
		s.respondSemanticSearchFallback(w, r, query, category, limit, "semantic_not_ready", "Semantic search is not ready; showing Quick results.", 0)
		return
	}

	queryEmbedding, err := s.generateQwenQueryEmbedding(ctx, query)
	if err != nil {
		logAPIInternalError("generate semantic search query embedding", err)
		reasonCode := "semantic_unavailable"
		notice := "Semantic search is temporarily unavailable; showing Quick results."
		retryAfter := 0
		if errors.Is(err, errQwenQueryEmbeddingQueued) {
			reasonCode = "query_embedding_queued"
			notice = "Semantic results are being prepared; showing Quick results now."
			retryAfter = qwenQueryRetryAfterSeconds
		}
		s.respondSemanticSearchFallback(w, r, query, category, limit, reasonCode, notice, retryAfter)
		return
	}

	candidateLimit := limit
	if category != "" {
		candidateLimit = min(limit*4, 400)
	}
	results, err := s.cache.SearchSemanticQwen(ctx, queryEmbedding, candidateLimit)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "semantic search", "semantic search failed", err)
		return
	}
	results = filterSemanticResultsByCategory(results, category, limit)

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"results":  results,
			"count":    len(results),
			"query":    query,
			"mode":     searchModeSemantic,
			"category": category,
			"model":    arxiv.QwenEmbeddingModel,
			"metadata": semanticMetadataCoverage(results),
		},
	})
}

func (s *server) respondSemanticSearchFallback(w http.ResponseWriter, r *http.Request, query, category string, limit int, reasonCode, notice string, retryAfterSeconds int) {
	papers, _, err := quickSearchPapers(r.Context(), s.cache, query, category, limit)
	if err != nil {
		respondAPIInternalError(w, http.StatusServiceUnavailable, "fallback search", "search is temporarily unavailable", err)
		return
	}

	results := make([]map[string]interface{}, len(papers))
	for i := range papers {
		results[i] = map[string]interface{}{
			"paperId":    papers[i].ID,
			"paper":      papers[i],
			"similarity": nil,
			"fallback":   true,
		}
	}

	respondJSON(w, http.StatusPartialContent, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"results":       results,
			"papers":        papers,
			"count":         len(papers),
			"query":         query,
			"category":      category,
			"mode":          searchModeQuick,
			"requestedMode": searchModeSemantic,
			"model":         nil,
			"fallback": map[string]interface{}{
				"used":       true,
				"reasonCode": reasonCode,
				"notice":     notice,
			},
			"retry": map[string]interface{}{
				"recommended":  retryAfterSeconds > 0,
				"afterSeconds": retryAfterSeconds,
			},
		},
	})
}

func filterSemanticResultsByCategory(results []arxiv.SemanticResult, category string, limit int) []arxiv.SemanticResult {
	if category == "" && len(results) <= limit {
		return results
	}
	filtered := make([]arxiv.SemanticResult, 0, min(len(results), limit))
	for _, result := range results {
		if result.Paper != nil && paperInCategory(result.Paper.Categories, category) {
			filtered = append(filtered, result)
			if len(filtered) == limit {
				break
			}
		}
	}
	return filtered
}

func filterDeepResultsByCategory(results []arxiv.DeepSearchResult, category string, limit int) []arxiv.DeepSearchResult {
	if category == "" && len(results) <= limit {
		return results
	}
	filtered := make([]arxiv.DeepSearchResult, 0, min(len(results), limit))
	for _, result := range results {
		if result.Paper != nil && paperInCategory(result.Paper.Categories, category) {
			filtered = append(filtered, result)
			if len(filtered) == limit {
				break
			}
		}
	}
	return filtered
}

// handleAPISimilarPapers returns nearest neighbors for a paper embedding.
func (s *server) handleAPISimilarPapers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	paperID := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	paperID = strings.TrimSuffix(paperID, "/similar")
	if paperID == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}

	limit, err := parseLimit(r, 80, 160)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	ctx := r.Context()
	paper, err := s.cache.GetPaper(ctx, paperID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "paper not found",
		})
		return
	}

	if !s.cache.HasQwenEmbedding(ctx, paperID) {
		respondJSON(w, http.StatusConflict, APIResponse{
			Success: false,
			Error:   "paper qwen embedding not found",
		})
		return
	}

	semanticMap, results, err := s.cache.SimilarPaperMapQwen(ctx, paperID, limit)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load similar paper map", "similar papers unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paper":    summarizePaper(paper),
			"results":  summarizeSimilarResults(results),
			"map":      semanticMap,
			"count":    len(results),
			"model":    "Qwen3-Embedding-8B",
			"metadata": semanticMetadataCoverage(results),
		},
	})
}

// handleAPISearch handles search API requests
func (s *server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "query parameter 'q' required",
		})
		return
	}

	category, err := validateSearchCategory(r.URL.Query().Get("category"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: err.Error()})
		return
	}
	limit, err := parseLimit(r, 20, 100)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	ctx := r.Context()
	papers, err := s.cache.Search(ctx, query, category, limit)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "paper search", "search is temporarily unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"papers": papers,
			"count":  len(papers),
		},
	})
}

// handleAPISearchQuick handles quick multi-field search for dropdown/autocomplete
func (s *server) handleAPISearchQuick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "query parameter 'q' required",
		})
		return
	}

	limit, err := parseLimit(r, 10, 50)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	ctx := r.Context()
	type authorSuggestion struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		PaperCount int64  `json:"paperCount"`
	}
	authors := []authorSuggestion{}
	if author, ok := authorQueryCandidate(query); ok {
		if paperCount := s.cache.CountPapersByAuthor(ctx, author); paperCount > 0 {
			authors = append(authors, authorSuggestion{
				Name:       author,
				Path:       authorPath(author),
				PaperCount: paperCount,
			})
		}
	}

	papers, total, err := s.cache.QuickSearch(ctx, query, limit)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "quick search", "search is temporarily unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"papers":      papers,
			"count":       len(papers),
			"total":       total,
			"authors":     authors,
			"authorCount": len(authors),
		},
	})
}

// handleAPISearchStream streams search results via SSE
func (s *server) handleAPISearchStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "query parameter 'q' required",
		})
		return
	}

	category, err := validateSearchCategory(r.URL.Query().Get("category"))
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: err.Error()})
		return
	}
	limit, err := parseLimit(r, 100, 100)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	rawMode := r.URL.Query().Get("mode")
	if rawMode == "" {
		rawMode = r.URL.Query().Get("search-mode")
	}
	mode, err := parseSearchMode(rawMode, searchModeQuick)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: err.Error()})
		return
	}

	setSSEHeaders(w)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "real-time streaming is unavailable", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
		"type":     "start",
		"query":    query,
		"mode":     mode,
		"category": category,
	}))
	flusher.Flush()

	if mode == searchModeSemantic || mode == searchModeDeep {
		if mode == searchModeDeep {
			if _, ok := s.currentUser(r); !ok {
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":  "error",
					"error": "Sign in to use Deep Search.",
				}))
				flusher.Flush()
				return
			}
		}

		ready, readinessErr := s.searchModeReady(ctx, mode)
		if readinessErr != nil || !ready {
			if readinessErr != nil {
				logAPIInternalError("load streaming search readiness", readinessErr)
			}
			if mode != searchModeDeep {
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":          "fallback",
					"requestedMode": searchModeSemantic,
					"effectiveMode": searchModeQuick,
					"reasonCode":    "semantic_not_ready",
					"message":       "Semantic search is not ready; showing Quick results.",
					"retry":         map[string]interface{}{"recommended": false, "afterSeconds": 0},
				}))
				flusher.Flush()
				streamKeywordSearchResults(ctx, w, flusher, s.cache, query, category, limit, searchModeQuick)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":  "error",
				"error": "Search is warming up. Try Quick Search for now.",
			}))
			flusher.Flush()
			return
		}

		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":    "status",
			"message": searchModeProgressMessage(string(mode)),
		}))
		flusher.Flush()

		queryEmbedding, err := s.generateQwenQueryEmbedding(ctx, query)
		if err != nil {
			if errors.Is(err, errQwenQueryEmbeddingQueued) {
				if mode == searchModeDeep {
					fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
						"type":              "status",
						"message":           "Deep Search is being prepared. Try again shortly.",
						"queued":            true,
						"retryAfterSeconds": qwenQueryRetryAfterSeconds,
					}))
					flusher.Flush()
					fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
						"type":              "complete",
						"count":             0,
						"mode":              "deep",
						"queued":            true,
						"retryAfterSeconds": qwenQueryRetryAfterSeconds,
					}))
					flusher.Flush()
					return
				}
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":          "fallback",
					"requestedMode": searchModeSemantic,
					"effectiveMode": searchModeQuick,
					"reasonCode":    "query_embedding_queued",
					"message":       "Semantic results are being prepared; showing Quick results now.",
					"retry":         map[string]interface{}{"recommended": true, "afterSeconds": qwenQueryRetryAfterSeconds},
				}))
				flusher.Flush()
				streamKeywordSearchResults(ctx, w, flusher, s.cache, query, category, limit, searchModeQuick)
				return
			}
			logAPIInternalError("generate streaming search query embedding", err)
			if mode == searchModeDeep {
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":  "error",
					"error": "Failed to understand query.",
				}))
				flusher.Flush()
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":          "fallback",
				"requestedMode": searchModeSemantic,
				"effectiveMode": searchModeQuick,
				"reasonCode":    "semantic_unavailable",
				"message":       "Semantic search is temporarily unavailable; showing Quick results.",
				"retry":         map[string]interface{}{"recommended": false, "afterSeconds": 0},
			}))
			flusher.Flush()
			streamKeywordSearchResults(ctx, w, flusher, s.cache, query, category, limit, searchModeQuick)
			return
		}

		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":    "status",
			"message": "Searching...",
		}))
		flusher.Flush()

		if mode == searchModeDeep {
			candidateLimit := limit
			if category != "" {
				candidateLimit = min(limit*4, 400)
			}
			results, err := s.cache.SearchDeepQwen(ctx, queryEmbedding, candidateLimit)
			if err != nil {
				logAPIInternalError("stream deep search", err)
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":  "error",
					"error": "Deep Search is temporarily unavailable.",
				}))
				flusher.Flush()
				return
			}
			results = filterDeepResultsByCategory(results, category, limit)
			for i, res := range results {
				select {
				case <-ctx.Done():
					return
				default:
					fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
						"type":       "result",
						"index":      i,
						"paper":      res.Paper,
						"paperId":    res.PaperID,
						"similarity": res.Similarity,
						"snippet":    res.Snippet,
						"section":    res.Section,
					}))
					flusher.Flush()
				}
			}
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":     "complete",
				"count":    len(results),
				"mode":     searchModeDeep,
				"metadata": deepMetadataCoverage(results),
			}))
			flusher.Flush()
		} else {
			candidateLimit := limit
			if category != "" {
				candidateLimit = min(limit*4, 400)
			}
			results, err := s.cache.SearchSemanticQwen(ctx, queryEmbedding, candidateLimit)
			if err != nil {
				logAPIInternalError("stream semantic search", err)
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":  "error",
					"error": "Idea search is temporarily unavailable.",
				}))
				flusher.Flush()
				return
			}
			results = filterSemanticResultsByCategory(results, category, limit)
			for i, res := range results {
				select {
				case <-ctx.Done():
					return
				default:
					fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
						"type":       "result",
						"index":      i,
						"paper":      res.Paper,
						"paperId":    res.PaperID,
						"similarity": res.Similarity,
					}))
					flusher.Flush()
				}
			}
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":     "complete",
				"count":    len(results),
				"mode":     searchModeSemantic,
				"metadata": semanticMetadataCoverage(results),
			}))
			flusher.Flush()
		}

	} else {
		streamKeywordSearchResults(ctx, w, flusher, s.cache, query, category, limit, mode)
	}
}

func streamKeywordSearchResults(ctx context.Context, w io.Writer, flusher http.Flusher, cache *arxiv.Cache, query, category string, limit int, mode searchMode) {
	var papers []arxiv.Paper
	var err error
	if mode == searchModeKeyword {
		papers, err = cache.Search(ctx, query, category, limit)
	} else {
		papers, _, err = quickSearchPapers(ctx, cache, query, category, limit)
		mode = searchModeQuick
	}
	if err != nil {
		logAPIInternalError("stream keyword search", err)
		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":  "error",
			"error": "Search is temporarily unavailable.",
		}))
		flusher.Flush()
		return
	}

	for i, paper := range papers {
		select {
		case <-ctx.Done():
			return
		default:
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":    "result",
				"index":   i,
				"paper":   paper,
				"paperId": paper.ID,
			}))
			flusher.Flush()
		}
	}

	fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
		"type":  "complete",
		"count": len(papers),
		"mode":  mode,
	}))
	flusher.Flush()
}

func quickSearchPapers(ctx context.Context, cache *arxiv.Cache, query, category string, limit int) ([]arxiv.Paper, int, error) {
	if strings.TrimSpace(category) != "" {
		papers, err := cache.Search(ctx, query, category, limit)
		return papers, len(papers), err
	}
	return cache.QuickSearch(ctx, query, limit)
}

func (s *server) searchModeReady(ctx context.Context, mode searchMode) (bool, error) {
	if mode == searchModeDeep {
		stats, err := s.cache.QwenPipelineStats(ctx)
		if err != nil {
			return false, err
		}
		return stats.FullPaperChunkEmbeddings > 0, nil
	}
	stats, err := s.cache.Stats(ctx)
	if err != nil {
		return false, err
	}
	return stats.QwenEmbeddingsCount > 0, nil
}

func normalizeSearchMode(r *http.Request) string {
	raw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if raw == "" {
		raw = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search-mode")))
	}
	switch raw {
	case "quick", "keyword", "normal":
		return "quick"
	case "deep", "full", "full-paper", "fullpaper":
		return "deep"
	case "semantic", "search":
		return "semantic"
	default:
		return "quick"
	}
}

func authorQueryCandidate(query string) (string, bool) {
	query = strings.Join(strings.Fields(strings.TrimSpace(query)), " ")
	if len(query) < 3 || len(query) > 80 || strings.ContainsAny(query, "/\\@?=&:") {
		return "", false
	}
	parts := strings.Fields(query)
	if len(parts) < 2 || len(parts) > 5 {
		return "", false
	}
	for _, part := range parts {
		if strings.ContainsAny(part, "0123456789") {
			return "", false
		}
	}
	if !hasAuthorNameCasing(parts) {
		return "", false
	}
	return query, true
}

func hasAuthorNameCasing(parts []string) bool {
	for _, part := range parts {
		part = strings.TrimLeft(part, `("'[`)
		if part == "" {
			continue
		}
		ch := part[0]
		if ch >= 'A' && ch <= 'Z' {
			return true
		}
	}
	return false
}

func searchModeProgressMessage(mode string) string {
	if mode == "deep" {
		return "Reading full-paper matches..."
	}
	return "Understanding the idea..."
}

// handleAPICitations returns citations for a paper
func (s *server) handleAPICitations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	path = strings.TrimSuffix(path, "/citations")
	if path == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}

	ctx := r.Context()
	refs, err := s.cache.References(ctx, path)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load paper citations", "citations unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    refs,
	})
}

// handleAPICitedBy returns papers that cite this paper
func (s *server) handleAPICitedBy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	path = strings.TrimSuffix(path, "/cited-by")
	if path == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}

	limit, err := parseLimit(r, 50, 200)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	ctx := r.Context()
	citedBy, err := s.cache.CitedBy(ctx, path, limit)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load citing papers", "citing papers unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    citedBy,
	})
}

// handleAPICitationGraph returns citation graph JSON
func (s *server) handleAPICitationGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	path = strings.TrimSuffix(path, "/graph")
	if path == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}

	ctx := r.Context()
	graph, err := s.cache.GetCitationGraph(ctx, path)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load citation graph", "citation graph unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    graph,
	})
}

// handleAPICategories returns list of categories
func (s *server) handleAPICategories(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	categories, err := s.cache.ListCategories(ctx)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "list categories", "categories unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    categories,
	})
}

// handleAPIStats returns cache statistics
func (s *server) handleAPIStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	stats, err := s.cache.Stats(ctx)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load API statistics", "statistics unavailable", err)
		return
	}

	// Include live connection count
	coverage := s.coverageSignal(stats)
	response := map[string]interface{}{
		"TotalPapers":                  stats.TotalPapers,
		"PDFsDownloaded":               stats.PDFsDownloaded,
		"SourcesDownloaded":            stats.SourcesDownloaded,
		"QueuedDownloads":              stats.QueuedDownloads,
		"EmbeddingsCount":              stats.EmbeddingsCount,
		"QwenEmbeddingsCount":          stats.QwenEmbeddingsCount,
		"SSEConnections":               s.paperBroadcast.Count(),
		"OfficialArxivPapers":          coverage.OfficialTotal,
		"OfficialArxivPapersAsOf":      coverage.AsOf,
		"OfficialArxivCoveragePercent": coverage.Percent,
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    response,
	})
}

// handleAPIFetch handles paper fetching via API
func (s *server) handleAPIFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	path = strings.TrimSuffix(path, "/fetch")
	if path == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}
	if !isArxivID(path) {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "invalid paper ID",
		})
		return
	}

	// Parse options from query params
	downloadPDF := r.URL.Query().Get("pdf") == "true"
	downloadSource := r.URL.Query().Get("source") != "false" // default true
	generateEmbedding := r.URL.Query().Get("embedding") == "true"

	ctx := r.Context()
	opts := &arxiv.DownloadOptions{
		DownloadPDF:    downloadPDF,
		DownloadSource: downloadSource,
	}

	paper, err := s.cache.FetchAndDownload(ctx, path, opts)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "fetch and download paper", "paper fetch failed", err)
		return
	}

	// Broadcast new paper to all SSE subscribers
	s.publishPaper(paperEvent{
		Paper:        *paper,
		HasEmbedding: s.cache.HasQwenEmbedding(ctx, paper.ID),
	})

	if generateEmbedding {
		if _, err := s.generatePaperEmbedding(ctx, paper); err != nil {
			respondAPIInternalError(w, http.StatusInternalServerError, "generate fetched paper embedding", "failed to generate embedding", err)
			return
		}
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    paper,
	})
}

// handleAPIExport handles paper export (BibTeX, RIS, JSON)
func (s *server) handleAPIExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	parts := strings.Split(path, "/export/")
	if len(parts) != 2 {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "invalid export path",
		})
		return
	}

	paperID := parts[0]
	format := parts[1]

	ctx := r.Context()
	paper, err := s.cache.GetPaper(ctx, paperID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "paper not found",
		})
		return
	}

	switch format {
	case "bibtex":
		w.Header().Set("Content-Type", "application/x-bibtex; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+paperID+`.bib"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(paper.ToBibTeX()))
		return

	case "ris":
		w.Header().Set("Content-Type", "application/x-research-info-systems; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+paperID+`.ris"`)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(paper.ToRIS()))
		return

	case "json":
		respondJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data:    paper,
		})
		return

	default:
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "unsupported format. Use: bibtex, ris, or json",
		})
		return
	}
}

// handleAPISearchPDF handles PDF content search
func (s *server) handleAPISearchPDF(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := s.currentUser(r); !ok {
		respondJSON(w, http.StatusUnauthorized, APIResponse{Success: false, Error: "sign in or provide an API key to use PDF search"})
		return
	}
	if s.pdfSearchLimiter != nil && !s.pdfSearchLimiter.Allow(r) {
		writeRateLimitExceeded(w, r)
		return
	}
	if s.pdfSearchSem != nil {
		select {
		case s.pdfSearchSem <- struct{}{}:
			defer func() { <-s.pdfSearchSem }()
		default:
			respondJSON(w, http.StatusTooManyRequests, APIResponse{Success: false, Error: "PDF search is busy; retry shortly"})
			return
		}
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "query parameter 'q' required",
		})
		return
	}

	limit, err := parseLimit(r, 50, 50)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	fuzzyMode := r.URL.Query().Get("fuzzy") == "true"

	ctx := r.Context()
	results, err := s.cache.SearchPDFs(ctx, query, limit, fuzzyMode)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "search paper PDFs", "PDF search unavailable", err)
		return
	}

	// Enrich results with paper metadata
	type enrichedResult struct {
		PaperID string      `json:"paperId"`
		Paper   interface{} `json:"paper,omitempty"`
		Context string      `json:"context"`
		Match   bool        `json:"match"`
	}
	enriched := make([]enrichedResult, len(results))
	for i, res := range results {
		paper, _ := s.cache.GetPaper(ctx, res.PaperID)
		enriched[i] = enrichedResult{
			PaperID: res.PaperID,
			Paper:   paper,
			Context: res.Context,
			Match:   res.Match,
		}
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"results": enriched,
			"count":   len(enriched),
		},
	})
}

// handleAPIEmbeddings generates embeddings for a paper on-demand
func (s *server) handleAPIEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	path = strings.TrimSuffix(path, "/embeddings")
	if path == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}

	ctx := r.Context()

	paper, err := s.cache.GetPaper(ctx, path)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "paper not found",
		})
		return
	}

	if s.publicEmbeddingLimiter != nil && !s.publicEmbeddingLimiter.Allow(r) {
		respondJSON(w, http.StatusTooManyRequests, APIResponse{
			Success: false,
			Error:   "embedding generation rate limit exceeded - please retry in a moment",
		})
		return
	}

	if !s.qwenExecutionConfigured() {
		respondJSON(w, http.StatusServiceUnavailable, APIResponse{Success: false, Error: "Qwen embedding execution is not configured"})
		return
	}
	if strings.TrimSpace(s.qwenEmbeddingServiceURL) == "" {
		s.respondQwenEmbeddingQueued(w, r, paper, "Qwen embedding work is queued.")
		return
	}

	generator, err := s.generatePaperEmbedding(ctx, paper)
	if err != nil {
		logAPIInternalError("generate paper embedding", err)
		if errors.Is(err, errQwenQueryEmbeddingUnavailable) {
			respondJSON(w, http.StatusServiceUnavailable, APIResponse{Success: false, Error: "Qwen embedding execution is temporarily unavailable"})
			return
		}
		s.respondQwenEmbeddingQueued(w, r, paper, "Qwen worker could not finish synchronously; queued for retry.")
		return
	}
	_ = s.cache.CompleteQwenPaperJob(ctx, path, arxiv.QwenEmbeddingJobKindAbstract)

	mapReady := s.cache.HasQwenEmbedding(ctx, path)
	status, _ := s.cache.QwenPaperEmbeddingStatus(ctx, path)

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paperId":      path,
			"paper":        paper,
			"hasEmbedding": true,
			"mapReady":     mapReady,
			"status":       status,
			"statusUrl":    "/api/v1/papers/" + path + "/embedding-status",
			"generator":    generator,
			"message":      "embedding generated successfully",
		},
	})
}

func (s *server) handleAPIEmbeddingStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	paperID := strings.TrimPrefix(r.URL.Path, "/api/v1/papers/")
	paperID = strings.TrimSuffix(paperID, "/embedding-status")
	if paperID == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper ID required",
		})
		return
	}

	ctx := r.Context()
	paper, err := s.cache.GetPaper(ctx, paperID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "paper not found",
		})
		return
	}

	status, err := s.cache.QwenPaperEmbeddingStatus(ctx, paperID)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load Qwen embedding status", "embedding status unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paperId":   paperID,
			"paper":     summarizePaper(paper),
			"status":    status,
			"statusUrl": "/api/v1/papers/" + paperID + "/embedding-status",
		},
	})
}

func (s *server) respondQwenEmbeddingQueued(w http.ResponseWriter, r *http.Request, paper *arxiv.Paper, message string) {
	status, err := s.cache.EnsureQwenPaperJobs(r.Context(), paper.ID, 100)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "queue Qwen embedding work", "failed to queue embedding work", err)
		return
	}

	respondJSON(w, http.StatusAccepted, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paperId":      paper.ID,
			"paper":        summarizePaper(paper),
			"hasEmbedding": status.AbstractReady,
			"mapReady":     status.MapReady,
			"queued":       true,
			"status":       status,
			"statusUrl":    "/api/v1/papers/" + paper.ID + "/embedding-status",
			"message":      message,
		},
	})
}

func (s *server) handleAPIQwenJobClaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.localMode && !s.requireQwenWorker(w, r) {
		return
	}

	var req qwenWorkerClaimRequest
	if r.Body != nil {
		if err := decodeQwenWorkerBody(w, r, &req, true); err != nil {
			respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: "invalid JSON body"})
			return
		}
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 1
	}
	if limit > 8 {
		limit = 8
	}
	leaseSeconds := normalizeQwenWorkerLeaseSeconds(req.LeaseSeconds)
	leaseOwner := strings.TrimSpace(req.LeaseOwner)
	if leaseOwner == "" {
		leaseOwner = "qwen-api-worker"
	}

	kinds := []string{arxiv.QwenEmbeddingJobKindQuery, arxiv.QwenEmbeddingJobKindAbstract}
	if len(req.Kinds) > 0 {
		kinds = kinds[:0]
		seen := make(map[string]bool, len(req.Kinds))
		for _, rawKind := range req.Kinds {
			kind := strings.TrimSpace(rawKind)
			if kind == "" {
				continue
			}
			if kind != arxiv.QwenEmbeddingJobKindQuery && kind != arxiv.QwenEmbeddingJobKindAbstract {
				respondJSON(w, http.StatusBadRequest, APIResponse{
					Success: false,
					Error:   "remote Qwen worker API supports query and abstract jobs",
				})
				return
			}
			if seen[kind] {
				continue
			}
			seen[kind] = true
			kinds = append(kinds, kind)
		}
		if len(kinds) == 0 {
			respondJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   "at least one supported Qwen job kind is required",
			})
			return
		}
	}

	jobs, err := s.cache.ClaimQwenEmbeddingJobs(r.Context(), kinds, limit, leaseOwner, time.Duration(leaseSeconds)*time.Second)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "claim Qwen jobs", "failed to claim Qwen jobs", err)
		return
	}

	claimed := make([]qwenWorkerClaimedJob, 0, len(jobs))
	for _, job := range jobs {
		var text, queryHash string
		switch job.Kind {
		case arxiv.QwenEmbeddingJobKindAbstract:
			paper, err := s.cache.GetPaper(r.Context(), job.PaperID)
			if err != nil {
				_ = s.cache.FailQwenEmbeddingJob(r.Context(), job.ID, job.LeaseOwner, job.Attempts, err)
				continue
			}
			text = qwenPaperText(paper)
			if text == "" {
				_ = s.cache.FailQwenEmbeddingJob(r.Context(), job.ID, job.LeaseOwner, job.Attempts, fmt.Errorf("paper has no title or abstract to embed"))
				continue
			}
		case arxiv.QwenEmbeddingJobKindQuery:
			queryHash = strings.TrimPrefix(strings.TrimSpace(job.PaperID), "query:")
			var err error
			text, err = s.cache.GetQwenQueryText(r.Context(), queryHash)
			if err != nil {
				_ = s.cache.FailQwenEmbeddingJob(r.Context(), job.ID, job.LeaseOwner, job.Attempts, err)
				continue
			}
			if text == "" {
				_ = s.cache.FailQwenEmbeddingJob(r.Context(), job.ID, job.LeaseOwner, job.Attempts, fmt.Errorf("query text is empty"))
				continue
			}
		default:
			_ = s.cache.FailQwenEmbeddingJob(r.Context(), job.ID, job.LeaseOwner, job.Attempts, fmt.Errorf("unsupported remote Qwen job kind %q", job.Kind))
			continue
		}
		textSum := sha256.Sum256([]byte(text))
		sourceHash := fmt.Sprintf("%x", textSum[:])
		claimed = append(claimed, qwenWorkerClaimedJob{
			ID:              qwenWorkerLeasedJobID(job.ID, job.Attempts, sourceHash),
			PaperID:         job.PaperID,
			QueryHash:       queryHash,
			Kind:            job.Kind,
			Scope:           job.Scope,
			Model:           job.Model,
			Dim:             job.Dim,
			Text:            text,
			TextChars:       len(text),
			TokenEstimate:   max(1, len(text)/4),
			Attempts:        job.Attempts,
			LeaseOwner:      job.LeaseOwner,
			LeaseGeneration: job.Attempts,
			SourceHash:      sourceHash,
		})
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"jobs":  claimed,
			"count": len(claimed),
		},
	})
}

func (s *server) handleAPIQwenJobAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.localMode && !s.requireQwenWorker(w, r) {
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/v1/qwen/jobs/")
	if r.Method == http.MethodGet {
		if path == "" || strings.Contains(path, "/") {
			http.NotFound(w, r)
			return
		}
		job, err := s.cache.GetQwenEmbeddingJob(r.Context(), path)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				respondJSON(w, http.StatusNotFound, APIResponse{Success: false, Error: "job not found"})
				return
			}
			respondAPIInternalError(w, http.StatusInternalServerError, "load Qwen job", "failed to load Qwen job", err)
			return
		}
		respondJSON(w, http.StatusOK, APIResponse{Success: true, Data: map[string]interface{}{"job": job}})
		return
	}
	switch {
	case strings.HasSuffix(path, "/complete"):
		jobID := strings.TrimSuffix(path, "/complete")
		s.handleAPIQwenJobComplete(w, r, jobID)
	case strings.HasSuffix(path, "/fail"):
		jobID := strings.TrimSuffix(path, "/fail")
		s.handleAPIQwenJobFail(w, r, jobID)
	case strings.HasSuffix(path, "/heartbeat"):
		jobID := strings.TrimSuffix(path, "/heartbeat")
		s.handleAPIQwenJobHeartbeat(w, r, jobID)
	default:
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "unknown Qwen job action",
		})
	}
}

func (s *server) handleAPIQwenJobHeartbeat(w http.ResponseWriter, r *http.Request, jobID string) {
	jobID, pathGeneration, _ := parseQwenWorkerLeasedJobID(jobID)
	if jobID == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: "job ID required"})
		return
	}

	var req qwenWorkerHeartbeatRequest
	if err := decodeQwenWorkerBody(w, r, &req, false); err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: "invalid JSON body"})
		return
	}
	leaseOwner := strings.TrimSpace(req.LeaseOwner)
	if leaseOwner == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: "lease owner required"})
		return
	}
	generation := pathGeneration
	if req.LeaseGeneration > 0 {
		if generation > 0 && generation != req.LeaseGeneration {
			respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease no longer active"})
			return
		}
		generation = req.LeaseGeneration
	}
	if generation <= 0 {
		respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease generation required"})
		return
	}
	leaseSeconds := normalizeQwenWorkerLeaseSeconds(req.LeaseSeconds)
	job, err := s.cache.RenewQwenEmbeddingJobLease(r.Context(), jobID, leaseOwner, generation, time.Duration(leaseSeconds)*time.Second)
	if err != nil {
		s.respondQwenWorkerActionError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"jobId":           job.ID,
			"status":          arxiv.QwenEmbeddingJobRunning,
			"leaseOwner":      job.LeaseOwner,
			"leaseGeneration": job.Attempts,
			"leaseUntil":      job.LeaseUntil,
			"leaseSeconds":    leaseSeconds,
		},
	})
}

func (s *server) handleAPIQwenJobComplete(w http.ResponseWriter, r *http.Request, jobID string) {
	jobID, pathGeneration, pathSourceHash := parseQwenWorkerLeasedJobID(jobID)
	if jobID == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "job ID required",
		})
		return
	}

	var req qwenWorkerCompleteRequest
	if err := decodeQwenWorkerBody(w, r, &req, false); err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "invalid JSON body",
		})
		return
	}
	if len(req.Embedding) != arxiv.QwenEmbeddingDim {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("embedding has %d dimensions, want %d", len(req.Embedding), arxiv.QwenEmbeddingDim),
		})
		return
	}
	generation := pathGeneration
	if req.LeaseGeneration > 0 {
		if generation > 0 && generation != req.LeaseGeneration {
			respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease no longer active"})
			return
		}
		generation = req.LeaseGeneration
	}
	if generation <= 0 {
		respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease generation required"})
		return
	}
	sourceHash := pathSourceHash
	if strings.TrimSpace(req.SourceHash) != "" {
		if sourceHash != "" && sourceHash != req.SourceHash {
			respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job input no longer current"})
			return
		}
		sourceHash = req.SourceHash
	}

	job, err := s.cache.GetQwenEmbeddingJob(r.Context(), jobID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "job not found",
		})
		return
	}
	leaseOwner := job.LeaseOwner
	if strings.TrimSpace(req.LeaseOwner) != "" {
		if req.LeaseOwner != leaseOwner {
			respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease no longer active"})
			return
		}
		leaseOwner = req.LeaseOwner
	}

	switch job.Kind {
	case arxiv.QwenEmbeddingJobKindQuery:
		queryHash := strings.TrimPrefix(strings.TrimSpace(job.PaperID), "query:")
		queryText, err := s.cache.GetQwenQueryText(r.Context(), queryHash)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   "query embedding row not found",
			})
			return
		}
		if err := s.cache.CompleteQwenQueryEmbeddingJob(r.Context(), job.ID, leaseOwner, generation, queryHash, req.Embedding); err != nil {
			s.respondQwenWorkerActionError(w, err)
			return
		}
		status, _ := s.cache.QwenQueryEmbeddingStatus(r.Context(), queryText)
		respondJSON(w, http.StatusOK, APIResponse{
			Success: true,
			Data: map[string]interface{}{
				"jobId":     job.ID,
				"queryHash": queryHash,
				"status":    status,
			},
		})
		return

	case arxiv.QwenEmbeddingJobKindAbstract:
		// Continue below.

	default:
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "remote complete supports query and abstract jobs",
		})
		return
	}

	paper, err := s.cache.GetPaper(r.Context(), job.PaperID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{
			Success: false,
			Error:   "paper not found",
		})
		return
	}
	text := qwenPaperText(paper)
	if text == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "paper has no title or abstract to embed",
		})
		return
	}
	sum := sha256.Sum256([]byte(text))
	currentSourceHash := fmt.Sprintf("%x", sum[:])
	if sourceHash == "" {
		respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job input hash required"})
		return
	}
	if sourceHash != currentSourceHash {
		respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job input no longer current"})
		return
	}
	if err := s.cache.CompleteQwenAbstractEmbeddingJob(r.Context(), job.ID, leaseOwner, generation, job.PaperID, sourceHash, len(text), max(1, len(text)/4), req.Embedding); err != nil {
		s.respondQwenWorkerActionError(w, err)
		return
	}
	status, _ := s.cache.QwenPaperEmbeddingStatus(r.Context(), job.PaperID)
	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"jobId":   job.ID,
			"paperId": job.PaperID,
			"status":  status,
		},
	})
}

func (s *server) handleAPIQwenJobFail(w http.ResponseWriter, r *http.Request, jobID string) {
	jobID, pathGeneration, _ := parseQwenWorkerLeasedJobID(jobID)
	if jobID == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "job ID required",
		})
		return
	}
	var req qwenWorkerFailRequest
	if err := decodeQwenWorkerBody(w, r, &req, false); err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: "invalid JSON body"})
		return
	}
	generation := pathGeneration
	if req.LeaseGeneration > 0 {
		if generation > 0 && generation != req.LeaseGeneration {
			respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease no longer active"})
			return
		}
		generation = req.LeaseGeneration
	}
	if generation <= 0 {
		respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease generation required"})
		return
	}
	job, err := s.cache.GetQwenEmbeddingJob(r.Context(), jobID)
	if err != nil {
		respondJSON(w, http.StatusNotFound, APIResponse{Success: false, Error: "job not found"})
		return
	}
	leaseOwner := job.LeaseOwner
	if strings.TrimSpace(req.LeaseOwner) != "" {
		if req.LeaseOwner != leaseOwner {
			respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease no longer active"})
			return
		}
		leaseOwner = req.LeaseOwner
	}
	message := strings.TrimSpace(req.Error)
	if message == "" {
		message = "worker reported failure"
	}
	if err := s.cache.FailQwenEmbeddingJob(r.Context(), jobID, leaseOwner, generation, errors.New(message)); err != nil {
		s.respondQwenWorkerActionError(w, err)
		return
	}
	job, _ = s.cache.GetQwenEmbeddingJob(r.Context(), jobID)
	status := arxiv.QwenEmbeddingJobFailed
	if job != nil {
		status = job.Status
	}
	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"jobId":  jobID,
			"status": status,
		},
	})
}

func decodeQwenWorkerBody(w http.ResponseWriter, r *http.Request, destination any, allowEmpty bool) error {
	const maxBodyBytes int64 = 128 << 10
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(destination); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func (s *server) respondQwenWorkerActionError(w http.ResponseWriter, err error) {
	if errors.Is(err, arxiv.ErrQwenEmbeddingJobLeaseLost) {
		respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "job lease no longer active"})
		return
	}
	respondAPIInternalError(w, http.StatusInternalServerError, "update Qwen job", "failed to update Qwen job", err)
}

func (s *server) generatePaperEmbedding(ctx context.Context, paper *arxiv.Paper) (string, error) {
	if strings.TrimSpace(s.qwenEmbeddingServiceURL) == "" {
		if s.qwenAsyncWorkerEnabled {
			return "", errQwenQueryEmbeddingQueued
		}
		return "", errQwenQueryEmbeddingUnavailable
	}
	if err := s.generatePaperQwenEmbedding(ctx, paper); err != nil {
		return "", err
	}
	return "qwen-service", nil
}

func (s *server) generatePaperQwenEmbedding(ctx context.Context, paper *arxiv.Paper) error {
	text := qwenPaperText(paper)
	if text == "" {
		return fmt.Errorf("paper has no title or abstract to embed")
	}
	embedding, err := s.generateQwenQueryEmbedding(ctx, text)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(text))
	return s.cache.StoreQwenAbstractEmbedding(ctx, paper.ID, fmt.Sprintf("%x", sum[:]), len(text), max(1, len(text)/4), embedding)
}

func qwenPaperText(paper *arxiv.Paper) string {
	title := strings.TrimSpace(paper.Title)
	abstract := strings.Join(strings.Fields(paper.Abstract), " ")
	if title != "" && abstract != "" {
		return title + ". " + abstract
	}
	return title + abstract
}

// handleAPIGenerateEmbeddings is retained only to give legacy clients an explicit migration response.
func (s *server) handleAPIGenerateEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}

	respondJSON(w, http.StatusGone, APIResponse{
		Success: false,
		Error:   "legacy MiniLM bulk generation has been retired; POST /api/v1/papers/{id}/embeddings to generate or queue the canonical Qwen profile",
	})
}

// toJSON helper function to convert interface to JSON string
func toJSON(data interface{}) string {
	jsonBytes, _ := json.Marshal(data)
	return string(jsonBytes)
}

// handleAPIEmbeddingWorkerStatus reports the canonical Qwen pipeline only.
func (s *server) handleAPIEmbeddingWorkerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if s.cache == nil {
		respondJSON(w, http.StatusServiceUnavailable, APIResponse{Success: false, Error: "Qwen pipeline status unavailable"})
		return
	}
	ctx := r.Context()
	stats, err := s.cache.QwenPipelineStats(ctx)
	if err != nil {
		respondAPIInternalError(w, http.StatusServiceUnavailable, "load Qwen pipeline status", "Qwen pipeline status unavailable", err)
		return
	}
	serviceConfigured := strings.TrimSpace(s.qwenEmbeddingServiceURL) != ""
	serviceUp := false
	if serviceConfigured {
		healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req, reqErr := http.NewRequestWithContext(healthCtx, http.MethodGet, strings.TrimRight(s.qwenEmbeddingServiceURL, "/")+"/health", nil)
		if reqErr == nil {
			resp, requestErr := http.DefaultClient.Do(req)
			if resp != nil {
				defer resp.Body.Close()
			}
			if requestErr == nil && resp != nil && resp.StatusCode == http.StatusOK {
				var health struct {
					Ready     bool   `json:"ready"`
					Model     string `json:"model"`
					Dimension int    `json:"dimension"`
				}
				serviceUp = json.NewDecoder(resp.Body).Decode(&health) == nil &&
					health.Ready && health.Model == arxiv.QwenEmbeddingModel && health.Dimension == arxiv.QwenEmbeddingDim
			}
		}
	}
	available := serviceUp || s.qwenAsyncWorkerEnabled
	state := "unavailable"
	if serviceConfigured && serviceUp {
		state = "synchronous-service-ready"
	} else if s.qwenAsyncWorkerEnabled {
		state = "asynchronous-worker-configured"
	} else if serviceConfigured {
		state = "synchronous-service-down"
	}
	response := map[string]interface{}{
		"pipeline":  arxiv.QwenEmbeddingModel,
		"model":     arxiv.QwenEmbeddingModel,
		"dimension": arxiv.QwenEmbeddingDim,
		"state":     state,
		"available": available,
		"execution": map[string]interface{}{
			"synchronousConfigured":  serviceConfigured,
			"synchronousUp":          serviceUp,
			"asynchronousConfigured": s.qwenAsyncWorkerEnabled,
		},
		"coverage": map[string]interface{}{
			"abstractProfiles":         stats.AbstractProfiles,
			"fullPaperChunks":          stats.FullPaperChunks,
			"fullPaperChunkEmbeddings": stats.FullPaperChunkEmbeddings,
		},
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    response,
	})
}

func (s *server) qwenExecutionConfigured() bool {
	return strings.TrimSpace(s.qwenEmbeddingServiceURL) != "" || s.qwenAsyncWorkerEnabled
}

var (
	errQwenQueryEmbeddingQueued      = errors.New("Qwen query embedding queued")
	errQwenQueryEmbeddingUnavailable = errors.New("Qwen query embedding execution unavailable")
)

const qwenQueryRetryAfterSeconds = 75

func (s *server) generateQwenQueryEmbedding(ctx context.Context, query string) ([]float32, error) {
	if embedding, ok, err := s.cache.GetQwenQueryEmbedding(ctx, query); err != nil {
		return nil, err
	} else if ok {
		return embedding, nil
	}

	queueQuery := func(cause error) ([]float32, error) {
		status, err := s.cache.EnsureQwenQueryJob(ctx, query, 1000)
		if err != nil {
			if cause != nil {
				return nil, fmt.Errorf("%w after Qwen service error %q; failed to queue query embedding: %v", errQwenQueryEmbeddingQueued, cause.Error(), err)
			}
			return nil, err
		}
		for _, job := range status.Jobs {
			if job.Status == arxiv.QwenEmbeddingJobFailed && job.Attempts >= arxiv.MaxQwenEmbeddingJobAttempts {
				return nil, fmt.Errorf("%w after %d failed attempts", errQwenQueryEmbeddingUnavailable, job.Attempts)
			}
		}
		if cause != nil {
			return nil, fmt.Errorf("%w: %v", errQwenQueryEmbeddingQueued, cause)
		}
		return nil, errQwenQueryEmbeddingQueued
	}

	if strings.TrimSpace(s.qwenEmbeddingServiceURL) == "" {
		if s.qwenAsyncWorkerEnabled {
			return queueQuery(nil)
		}
		return nil, errQwenQueryEmbeddingUnavailable
	}

	reqBody := map[string][]string{"texts": []string{query}}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, "POST", strings.TrimRight(s.qwenEmbeddingServiceURL, "/")+"/embed/batch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cause := fmt.Errorf("Qwen service request failed: %w", err)
		if s.qwenAsyncWorkerEnabled {
			return queueQuery(cause)
		}
		return nil, fmt.Errorf("%w: %v", errQwenQueryEmbeddingUnavailable, cause)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		cause := fmt.Errorf("Qwen service error (status %d): %s", resp.StatusCode, string(respBody))
		if s.qwenAsyncWorkerEnabled {
			return queueQuery(cause)
		}
		return nil, fmt.Errorf("%w: %v", errQwenQueryEmbeddingUnavailable, cause)
	}

	var result struct {
		Embeddings [][]float32 `json:"embeddings"`
		Dimension  int         `json:"dimension"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode Qwen embedding response: %w", err)
	}
	if len(result.Embeddings) != 1 {
		return nil, fmt.Errorf("Qwen service returned %d embeddings for one query", len(result.Embeddings))
	}
	if len(result.Embeddings[0]) != arxiv.QwenEmbeddingDim {
		return nil, fmt.Errorf("Qwen service returned %d dimensions, want %d", len(result.Embeddings[0]), arxiv.QwenEmbeddingDim)
	}
	if err := s.cache.StoreQwenQueryEmbeddingForQuery(ctx, query, result.Embeddings[0]); err != nil {
		fmt.Printf("could not cache Qwen query embedding: %v\n", err)
	}
	return result.Embeddings[0], nil
}

// Semaphore to limit concurrent SSE initializations (prevents DB overload)
// SQLite can handle ~10-20 concurrent queries before lock contention hurts performance
var sseInitSemaphore = make(chan struct{}, 10) // Max 10 concurrent initializations

// handleAPIRecentPapersStream streams recent papers via SSE with real-time updates
func (s *server) handleAPIRecentPapersStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limit, err := parseLimit(r, 50, 100)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	clientIP := clientIPFromRequest(r, s.trustProxyHeaders)
	sub, ok := s.paperBroadcast.Subscribe(clientIP)
	if !ok {
		http.Error(w, "too many stream connections", http.StatusServiceUnavailable)
		return
	}
	defer s.paperBroadcast.Unsubscribe(sub)

	setSSEHeaders(w)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "real-time streaming is unavailable", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	// Send start event
	fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
		"type": "start",
	}))
	flusher.Flush()

	// Acquire semaphore to limit concurrent DB queries
	select {
	case sseInitSemaphore <- struct{}{}:
		// Got slot
	case <-ctx.Done():
		return
	}

	// Fetch initial recent papers (lite: only ID, Title, Authors, Categories)
	papers, err := s.cache.ListRecentPapersLite(ctx, limit)
	if err != nil {
		<-sseInitSemaphore // Release semaphore
		logAPIInternalError("list recent papers for stream", err)
		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":  "error",
			"error": "Recent papers are temporarily unavailable.",
		}))
		flusher.Flush()
		return
	}

	// Batch fetch embedding IDs only for these papers (not ALL embeddings)
	paperIDs := make([]string, len(papers))
	for i, p := range papers {
		paperIDs[i] = p.ID
	}
	embeddingIDs, _ := s.cache.GetQwenEmbeddingIDsFor(ctx, paperIDs)

	// Release semaphore - DB queries done
	<-sseInitSemaphore

	// Stream initial papers with minimal payload (only fields the client uses)
	for i, paper := range papers {
		select {
		case <-ctx.Done():
			return
		default:
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":  "paper",
				"index": i,
				"paper": map[string]interface{}{
					"ID":           paper.ID,
					"Title":        paper.Title,
					"Authors":      paper.Authors,
					"Categories":   paper.Categories,
					"HasEmbedding": embeddingIDs[paper.ID],
					"hasEmbedding": embeddingIDs[paper.ID],
				},
				"hasEmbedding": embeddingIDs[paper.ID],
			}))
			flusher.Flush()
		}
	}

	// Send complete for initial load
	fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
		"type":  "complete",
		"count": len(papers),
	}))
	flusher.Flush()

	// Keep connection open for 10 minutes max (client will reconnect)
	timeout := time.NewTimer(10 * time.Minute)
	defer timeout.Stop()

	// Send keepalive every 30s to prevent connection timeouts
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client disconnected
			return
		case <-timeout.C:
			// Connection timeout - client will reconnect
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type": "timeout",
			}))
			flusher.Flush()
			return
		case <-keepalive.C:
			// Send keepalive comment to prevent proxy timeouts
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case event := <-sub:
			// New paper fetched - stream it to client
			paper := map[string]interface{}{
				"ID":           event.Paper.ID,
				"Title":        event.Paper.Title,
				"Authors":      event.Paper.Authors,
				"Categories":   event.Paper.Categories,
				"HasEmbedding": event.HasEmbedding,
				"hasEmbedding": event.HasEmbedding,
			}
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":         "new",
				"paper":        paper,
				"hasEmbedding": event.HasEmbedding,
			}))
			flusher.Flush()
		}
	}
}

// respondJSON sends a JSON response
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func respondAPIInternalError(w http.ResponseWriter, status int, operation, publicMessage string, err error) {
	logAPIInternalError(operation, err)
	respondJSON(w, status, APIResponse{Success: false, Error: publicMessage})
}

func logAPIInternalError(operation string, err error) {
	if err != nil {
		log.Printf("api %s: %v", operation, err)
	}
}

func parseLimit(r *http.Request, defaultLimit, maxLimit int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultLimit, nil
	}

	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("invalid limit parameter")
	}
	if limit == 0 {
		return defaultLimit, nil
	}
	if maxLimit > 0 && limit > maxLimit {
		return 0, fmt.Errorf("limit must be <= %d", maxLimit)
	}
	return limit, nil
}

// handleAPIAuthorCollaborators returns collaborators for an author
func (s *server) handleAPIAuthorCollaborators(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	author, ok := parseAuthorParameter(w, r)
	if !ok {
		return
	}

	limit, err := parseLimit(r, 100, 200)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	ctx := r.Context()
	collabs, err := s.cache.GetCollaborators(ctx, author, limit)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load author collaborators", "author collaborators unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"author":        author,
			"collaborators": collabs,
			"count":         len(collabs),
		},
	})
}

// handleAPIAuthorSimilar returns authors with similar research interests
func (s *server) handleAPIAuthorSimilar(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	author, ok := parseAuthorParameter(w, r)
	if !ok {
		return
	}

	limit, err := parseLimit(r, 10, 50)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	ctx := r.Context()
	similar, err := s.cache.GetSimilarAuthors(ctx, author, limit)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load similar authors", "similar authors unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"author":  author,
			"similar": similar,
			"count":   len(similar),
		},
	})
}

// handleAPIAuthorStats returns statistics for an author
func (s *server) handleAPIAuthorStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	author, ok := parseAuthorParameter(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	stats, err := s.cache.GetAuthorStats(ctx, author)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load author statistics", "author statistics unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"author": author,
			"stats":  stats,
		},
	})
}

// handleAPIBuildAuthorGraph triggers building the author collaboration graph
func (s *server) handleAPIBuildAuthorGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}

	if !s.authorGraphBuildMu.TryLock() {
		respondJSON(w, http.StatusConflict, APIResponse{Success: false, Error: "author graph build already running"})
		return
	}
	go func() {
		defer s.authorGraphBuildMu.Unlock()
		bgCtx := context.Background()
		if err := s.cache.BuildAuthorGraph(bgCtx); err != nil {
			log.Printf("build author graph: %v", err)
		}
	}()

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"message": "Author graph build started in background",
		},
	})
}

// handleAPIAuthorGraph returns collaboration graph data for visualization
func (s *server) handleAPIAuthorGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	author, ok := parseAuthorParameter(w, r)
	if !ok {
		return
	}

	depth := 1
	if d := r.URL.Query().Get("depth"); d == "2" {
		depth = 2
	}

	ctx := r.Context()
	graph, err := s.cache.GetAuthorGraph(ctx, author, depth)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load author graph", "author graph unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    graph,
	})
}

// handleAPIAuthorProfile returns comprehensive author profile
func (s *server) handleAPIAuthorProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	author, ok := parseAuthorParameter(w, r)
	if !ok {
		return
	}

	ctx := r.Context()
	profile, err := s.cache.GetAuthorProfile(ctx, author)
	if err != nil {
		respondAPIInternalError(w, http.StatusInternalServerError, "load author profile", "author profile unavailable", err)
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    profile,
	})
}

func parseAuthorParameter(w http.ResponseWriter, r *http.Request) (string, bool) {
	author := strings.TrimSpace(r.URL.Query().Get("author"))
	if author == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: "author parameter required"})
		return "", false
	}
	if len(author) > 256 {
		respondJSON(w, http.StatusBadRequest, APIResponse{Success: false, Error: "author parameter exceeds 256 bytes"})
		return "", false
	}
	return author, true
}
