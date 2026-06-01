package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lantos1618/arxiv.gg"
)

// getToolsPath returns the path to a tool script.
// In Docker: /app/tools/, locally: ./tools/ or relative to executable
func getToolsPath(script string) string {
	// Check Docker path first
	dockerPath := filepath.Join("/app/tools", script)
	if _, err := os.Stat(dockerPath); err == nil {
		return dockerPath
	}

	// Check relative to current directory
	localPath := filepath.Join("tools", script)
	if _, err := os.Stat(localPath); err == nil {
		return localPath
	}

	// Fallback: relative to executable
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		exePath := filepath.Join(exeDir, "tools", script)
		if _, err := os.Stat(exePath); err == nil {
			return exePath
		}
	}

	// Last resort: assume Docker path
	return dockerPath
}

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

	ctx := r.Context()

	stats, err := s.cache.Stats(ctx)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	if stats.QwenEmbeddingsCount == 0 {
		respondJSON(w, http.StatusServiceUnavailable, APIResponse{
			Success: false,
			Error:   "Search is warming up. Try Quick Search for now.",
		})
		return
	}

	queryEmbedding, err := s.generateQwenQueryEmbedding(ctx, query)
	if err != nil {
		respondJSON(w, http.StatusServiceUnavailable, APIResponse{
			Success: false,
			Error:   "failed to understand query: " + err.Error(),
		})
		return
	}

	results, err := s.cache.SearchSemanticQwen(ctx, queryEmbedding, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"results": results,
			"count":   len(results),
			"query":   query,
			"mode":    "search",
			"model":   "Qwen3-Embedding-8B",
		},
	})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paper":   summarizePaper(paper),
			"results": summarizeSimilarResults(results),
			"map":     semanticMap,
			"count":   len(results),
			"model":   "Qwen3-Embedding-8B",
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

	category := r.URL.Query().Get("category")
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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
	papers, total, err := s.cache.QuickSearch(ctx, query, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"papers": papers,
			"count":  len(papers),
			"total":  total,
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

	category := r.URL.Query().Get("category")
	limit, err := parseLimit(r, 100, 100)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	searchMode := normalizeSearchMode(r)

	setSSEHeaders(w)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
		"type":     "start",
		"query":    query,
		"mode":     searchMode,
		"category": category,
	}))
	flusher.Flush()

	if searchMode == "search" || searchMode == "deep" {
		if searchMode == "deep" {
			if _, ok := s.currentUser(r); !ok {
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":  "error",
					"error": "Sign in to use Deep Search. It is free while we test full-paper tools.",
				}))
				flusher.Flush()
				return
			}
		}

		stats, err := s.cache.Stats(ctx)
		if err != nil || stats.QwenEmbeddingsCount == 0 {
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":  "error",
				"error": "Search is warming up. Try Quick Search for now.",
			}))
			flusher.Flush()
			return
		}

		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":    "status",
			"message": searchModeProgressMessage(searchMode),
		}))
		flusher.Flush()

		queryEmbedding, err := s.generateQwenQueryEmbedding(ctx, query)
		if err != nil {
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":  "error",
				"error": "Failed to understand query: " + err.Error(),
			}))
			flusher.Flush()
			return
		}

		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":    "status",
			"message": "Searching...",
		}))
		flusher.Flush()

		if searchMode == "deep" {
			results, err := s.cache.SearchDeepQwen(ctx, queryEmbedding, limit)
			if err != nil {
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":  "error",
					"error": err.Error(),
				}))
				flusher.Flush()
				return
			}
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
				"type":  "complete",
				"count": len(results),
				"mode":  "deep",
			}))
			flusher.Flush()
		} else {
			results, err := s.cache.SearchSemanticQwen(ctx, queryEmbedding, limit)
			if err != nil {
				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":  "error",
					"error": err.Error(),
				}))
				flusher.Flush()
				return
			}
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
				"type":  "complete",
				"count": len(results),
				"mode":  "search",
			}))
			flusher.Flush()
		}

	} else {
		papers, err := s.cache.Search(ctx, query, category, limit)
		if err != nil {
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":  "error",
				"error": err.Error(),
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
			"mode":  "quick",
		}))
		flusher.Flush()
	}
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
	default:
		return "search"
	}
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	// Broadcast new paper to all SSE subscribers
	s.paperBroadcast.Broadcast(paperEvent{
		Paper:        *paper,
		HasEmbedding: s.cache.HasQwenEmbedding(ctx, paper.ID),
	})

	if generateEmbedding {
		if _, err := s.generatePaperEmbedding(ctx, paper); err != nil {
			respondJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("failed to generate embedding: %v", err),
			})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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

	generator, err := s.generatePaperEmbedding(ctx, paper)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to generate embedding: %v", err),
		})
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paperId":      path,
			"paper":        paper,
			"hasEmbedding": true,
			"generator":    generator,
			"message":      "embedding generated successfully",
		},
	})
}

func (s *server) generatePaperEmbedding(ctx context.Context, paper *arxiv.Paper) (string, error) {
	if strings.TrimSpace(s.qwenEmbeddingServiceURL) != "" {
		if err := s.generatePaperQwenEmbedding(ctx, paper); err != nil {
			return "", err
		}
		return "qwen-service", nil
	}

	if s.embeddingServiceURL != "" {
		if err := s.generatePaperEmbeddingHTTP(ctx, paper); err == nil {
			return "embedding-service", nil
		} else {
			fmt.Printf("Embedding service paper generation error (falling back to Python): %v\n", err)
		}
	}

	if err := s.generatePaperEmbeddingPython(ctx, paper.ID); err != nil {
		return "", err
	}
	return "python-script", nil
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

func (s *server) generatePaperEmbeddingHTTP(ctx context.Context, paper *arxiv.Paper) error {
	reqBody := map[string]string{
		"paper_id": paper.ID,
		"title":    paper.Title,
		"abstract": paper.Abstract,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", s.embeddingServiceURL+"/embed/paper", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("embedding service request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("embedding service error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("failed to decode embedding service response: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("embedding service failed: %s", result.Message)
	}

	return nil
}

func (s *server) generatePaperEmbeddingPython(ctx context.Context, paperID string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", getToolsPath("generate_embeddings.py"), s.cache.Root(), "--paper-id", paperID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v, output: %s", err, string(output))
	}

	outputStr := strings.TrimSpace(string(output))
	if strings.HasPrefix(outputStr, "ERROR:") {
		return fmt.Errorf("%s", outputStr)
	}

	return nil
}

// handleAPIGenerateEmbeddings generates embeddings for all papers
func (s *server) handleAPIGenerateEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}

	limit, err := parseLimit(r, 0, 10000)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	// Check if this is an SSE request
	if r.Header.Get("Accept") == "text/event-stream" {
		s.handleGenerateEmbeddingsSSE(w, r, limit)
		return
	}

	// Original synchronous behavior for non-SSE requests
	args := []string{getToolsPath("generate_embeddings.py"), s.cache.Root()}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}

	cmd := exec.Command("python3", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to generate embeddings: %v, output: %s", err, string(output)),
		})
		return
	}

	outputStr := strings.TrimSpace(string(output))
	if strings.HasPrefix(outputStr, "ERROR:") {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   outputStr,
		})
		return
	}

	ctx := r.Context()
	count, _ := s.cache.CountEmbeddings(ctx)

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"count":   count,
			"message": "embeddings generated successfully",
		},
	})
}

// handleGenerateEmbeddingsSSE provides real-time progress for embedding generation
func (s *server) handleGenerateEmbeddingsSSE(w http.ResponseWriter, r *http.Request, limit int) {
	// Set SSE headers
	setSSEHeaders(w)
	w.Header().Set("Access-Control-Allow-Headers", "Cache-Control")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	// Send initial message
	fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
		"type":    "start",
		"message": "Starting embedding generation...",
	}))
	flusher.Flush()

	// Prepare command with progress output
	args := []string{getToolsPath("generate_embeddings.py"), s.cache.Root()}
	if limit > 0 {
		args = append(args, "--limit", strconv.Itoa(limit))
	}

	cmd := exec.CommandContext(ctx, "python3", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":  "error",
			"error": err.Error(),
		}))
		flusher.Flush()
		return
	}

	// Start the command
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":  "error",
			"error": err.Error(),
		}))
		flusher.Flush()
		return
	}

	// Read output line by line
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()

		// Parse progress from Python script output
		// Expected format: "Processed X/Y papers (Z% complete)"
		if strings.Contains(line, "Processed") && strings.Contains(line, "papers") {
			// Try to extract progress numbers
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				var current, total int
				if strings.Contains(parts[1], "/") {
					splitParts := strings.Split(parts[1], "/")
					if len(splitParts) == 2 {
						fmt.Sscanf(splitParts[0], "%d", &current)
						fmt.Sscanf(splitParts[1], "%d", &total)
					}
				}

				fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
					"type":    "progress",
					"current": current,
					"total":   total,
					"message": strings.TrimSpace(line),
				}))
				flusher.Flush()
			}
		} else {
			// Send any other output as status message
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":    "status",
				"message": strings.TrimSpace(line),
			}))
			flusher.Flush()
		}
	}

	// Wait for command to finish
	err = cmd.Wait()

	// Get final embedding count
	count, _ := s.cache.CountEmbeddings(ctx)

	if err != nil {
		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":  "error",
			"error": err.Error(),
			"count": count,
		}))
	} else {
		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":    "complete",
			"count":   count,
			"message": "Embedding generation completed successfully",
		}))
	}
	flusher.Flush()

	// Send final close event
	fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
		"type": "close",
	}))
	flusher.Flush()
}

// toJSON helper function to convert interface to JSON string
func toJSON(data interface{}) string {
	jsonBytes, _ := json.Marshal(data)
	return string(jsonBytes)
}

// handleAPIEmbeddingWorkerStatus returns the embedding worker status
func (s *server) handleAPIEmbeddingWorkerStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Get basic embedding stats
	embeddingCount, _ := s.cache.CountEmbeddings(ctx)
	stats, _ := s.cache.Stats(ctx)
	pendingCount := stats.TotalPapers - embeddingCount
	if pendingCount < 0 {
		pendingCount = 0
	}

	response := map[string]interface{}{
		"embeddingCount": embeddingCount,
		"totalPapers":    stats.TotalPapers,
		"pendingCount":   pendingCount,
		"serviceURL":     s.embeddingServiceURL,
	}

	// Add worker stats if worker is running
	if s.embeddingWorker != nil {
		workerStats := s.embeddingWorker.Stats()
		response["worker"] = workerStats
	} else {
		response["worker"] = nil
		response["workerEnabled"] = false
	}

	// Check embedding service health
	if s.embeddingServiceURL != "" {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", s.embeddingServiceURL+"/health", nil)
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			defer resp.Body.Close()
		}
		if err != nil || resp.StatusCode != http.StatusOK {
			response["serviceUp"] = false
		} else {
			response["serviceUp"] = true
		}
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    response,
	})
}

var queryEmbeddingPythonSemaphore = make(chan struct{}, 1)

func (s *server) generateQueryEmbedding(ctx context.Context, query string) ([]float32, error) {
	// Try HTTP embedding service first (fast path)
	if s.embeddingServiceURL != "" {
		embedding, err := s.generateQueryEmbeddingHTTP(ctx, query)
		if err == nil {
			return embedding, nil
		}
		// Log error but fall through to Python fallback
		fmt.Printf("Embedding service error (falling back to Python): %v\n", err)
	}

	// Fallback to Python script (slow path - loads model each time)
	return s.generateQueryEmbeddingPython(ctx, query)
}

func (s *server) generateQwenQueryEmbedding(ctx context.Context, query string) ([]float32, error) {
	if strings.TrimSpace(s.qwenEmbeddingServiceURL) == "" {
		return nil, fmt.Errorf("Qwen search is warming up; try Quick Search for now")
	}

	reqBody := map[string][]string{"texts": []string{query}}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 75*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(s.qwenEmbeddingServiceURL, "/")+"/embed/batch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Qwen service request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Qwen service error (status %d): %s", resp.StatusCode, string(respBody))
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
	return result.Embeddings[0], nil
}

// generateQueryEmbeddingHTTP calls the FastAPI embedding service.
func (s *server) generateQueryEmbeddingHTTP(ctx context.Context, query string) ([]float32, error) {
	reqBody := map[string]string{"text": query}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", s.embeddingServiceURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding service request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding service error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode embedding response: %w", err)
	}

	return result.Embedding, nil
}

// generateQueryEmbeddingPython falls back to the Python script.
func (s *server) generateQueryEmbeddingPython(ctx context.Context, query string) ([]float32, error) {
	select {
	case queryEmbeddingPythonSemaphore <- struct{}{}:
		defer func() { <-queryEmbeddingPythonSemaphore }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", getToolsPath("generate_embeddings.py"), s.cache.Root(), "--query", query)

	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("python embedding timed out: %w", ctx.Err())
		}
		return nil, fmt.Errorf("python embedding failed: %v, output: %s", err, string(output))
	}

	outputStr := strings.TrimSpace(string(output))
	if outputStr == "" {
		return nil, fmt.Errorf("empty output from python embedding")
	}

	if strings.HasPrefix(outputStr, "ERROR:") {
		return nil, fmt.Errorf("embedding script error: %s", outputStr)
	}

	// Parse comma-separated float values
	parts := strings.Split(outputStr, ",")
	embedding := make([]float32, len(parts))

	for i, part := range parts {
		val, err := strconv.ParseFloat(strings.TrimSpace(part), 32)
		if err != nil {
			return nil, fmt.Errorf("failed to parse embedding value %q: %v", part, err)
		}
		embedding[i] = float32(val)
	}

	return embedding, nil
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
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
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
		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":  "error",
			"error": err.Error(),
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
					"ID":         paper.ID,
					"Title":      paper.Title,
					"Authors":    paper.Authors,
					"Categories": paper.Categories,
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
	timeout := time.After(10 * time.Minute)

	// Send keepalive every 30s to prevent connection timeouts
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			// Client disconnected
			return
		case <-timeout:
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
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":         "new",
				"paper":        event.Paper,
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

	author := r.URL.Query().Get("author")
	if author == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "author parameter required",
		})
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
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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

	author := r.URL.Query().Get("author")
	if author == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "author parameter required",
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
	similar, err := s.cache.GetSimilarAuthors(ctx, author, limit)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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

	author := r.URL.Query().Get("author")
	if author == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "author parameter required",
		})
		return
	}

	ctx := r.Context()
	stats, err := s.cache.GetAuthorStats(ctx, author)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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

	ctx := r.Context()

	// Run in background
	go func() {
		bgCtx := context.Background()
		if err := s.cache.BuildAuthorGraph(bgCtx); err != nil {
			fmt.Printf("Error building author graph: %v\n", err)
		}
		if err := s.cache.BuildAuthorEmbeddings(bgCtx); err != nil {
			fmt.Printf("Error building author embeddings: %v\n", err)
		}
	}()

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"message": "Author graph build started in background",
		},
	})
	_ = ctx // context used for request
}

// handleAPIAuthorGraph returns collaboration graph data for visualization
func (s *server) handleAPIAuthorGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	author := r.URL.Query().Get("author")
	if author == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "author parameter required",
		})
		return
	}

	depth := 1
	if d := r.URL.Query().Get("depth"); d == "2" {
		depth = 2
	}

	ctx := r.Context()
	graph, err := s.cache.GetAuthorGraph(ctx, author, depth)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
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

	author := r.URL.Query().Get("author")
	if author == "" {
		respondJSON(w, http.StatusBadRequest, APIResponse{
			Success: false,
			Error:   "author parameter required",
		})
		return
	}

	ctx := r.Context()
	profile, err := s.cache.GetAuthorProfile(ctx, author)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    profile,
	})
}
