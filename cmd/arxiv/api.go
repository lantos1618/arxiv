package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tmc/arxiv"
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

	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	ctx := r.Context()

	// Check if embeddings exist
	count, err := s.cache.CountEmbeddings(ctx)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   err.Error(),
		})
		return
	}

	if count == 0 {
		respondJSON(w, http.StatusServiceUnavailable, APIResponse{
			Success: false,
			Error:   "semantic search requires embeddings to be generated first. Run tools/generate_embeddings.py",
		})
		return
	}

	// Generate query embedding using Python script
	queryEmbedding, err := s.generateQueryEmbedding(query)
	if err != nil {
		respondJSON(w, http.StatusServiceUnavailable, APIResponse{
			Success: false,
			Error:   "failed to generate query embedding: " + err.Error(),
		})
		return
	}

	// Perform semantic search
	results, err := s.cache.SearchSemantic(ctx, queryEmbedding, limit)
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
	limitStr := r.URL.Query().Get("limit")
	limit := 20
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
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
	limitStr := r.URL.Query().Get("limit")
	limit := 100
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	searchMode := r.URL.Query().Get("mode")
	isSemantic := searchMode == "semantic"

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

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

	if isSemantic {
		count, err := s.cache.CountEmbeddings(ctx)
		if err != nil || count == 0 {
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":  "error",
				"error": "Semantic search requires embeddings. Generate them first.",
			}))
			flusher.Flush()
			return
		}

		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":    "status",
			"message": "Generating query embedding...",
		}))
		flusher.Flush()

		queryEmbedding, err := s.generateQueryEmbedding(query)
		if err != nil {
			fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
				"type":  "error",
				"error": "Failed to generate query embedding: " + err.Error(),
			}))
			flusher.Flush()
			return
		}

		fmt.Fprintf(w, "data: %s\n\n", toJSON(map[string]interface{}{
			"type":    "status",
			"message": "Searching...",
		}))
		flusher.Flush()

		results, err := s.cache.SearchSemantic(ctx, queryEmbedding, limit)
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
			"mode":  "semantic",
		}))
		flusher.Flush()

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
			"mode":  "keyword",
		}))
		flusher.Flush()
	}
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

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
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

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data:    stats,
	})
}

// handleAPIFetch handles paper fetching via API
func (s *server) handleAPIFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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

	if generateEmbedding {
		cmd := exec.Command("python3", getToolsPath("generate_embeddings.py"), s.cache.Root(), "--paper-id", path)
		output, err := cmd.CombinedOutput()
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, APIResponse{
				Success: false,
				Error:   fmt.Sprintf("failed to generate embedding: %v, output: %s", err, string(output)),
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

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
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

	cmd := exec.Command("python3", getToolsPath("generate_embeddings.py"), s.cache.Root(), "--paper-id", path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, APIResponse{
			Success: false,
			Error:   fmt.Sprintf("failed to generate embedding: %v, output: %s", err, string(output)),
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

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]interface{}{
			"paperId": path,
			"paper":   paper,
			"message": "embedding generated successfully",
		},
	})
}

// handleAPIGenerateEmbeddings generates embeddings for all papers
func (s *server) handleAPIGenerateEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 0
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil {
			respondJSON(w, http.StatusBadRequest, APIResponse{
				Success: false,
				Error:   "invalid limit parameter",
			})
			return
		}
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
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

func (s *server) generateQueryEmbedding(query string) ([]float32, error) {
	cmd := exec.Command("python3", getToolsPath("generate_embeddings.py"), s.cache.Root(), "--query", query)

	output, err := cmd.CombinedOutput()
	if err != nil {
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

// respondJSON sends a JSON response
func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
