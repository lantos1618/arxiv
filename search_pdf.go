package arxiv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const (
	maxPDFSearchPapers    = 2000
	maxPDFSearchTextBytes = 2 * 1024 * 1024
	maxPDFSearchScanBytes = 32 * 1024 * 1024
)

// ExtractPDFText extracts text from a PDF file using pdftotext.
// Returns the extracted text and any error.
func ExtractPDFText(pdfPath string) (string, error) {
	if _, err := os.Stat(pdfPath); err != nil {
		return "", fmt.Errorf("PDF not found: %w", err)
	}

	cmd := exec.Command("pdftotext", pdfPath, "-")
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftotext failed: %w: %s", err, stderr.String())
	}

	return out.String(), nil
}

// EnsurePDFText extracts and stores PDF text if not already cached.
func (c *Cache) EnsurePDFText(ctx context.Context, paperID string) (string, error) {
	paper, err := c.GetPaper(ctx, paperID)
	if err != nil {
		return "", err
	}

	if !paper.PDFDownloaded || paper.PDFPath == "" {
		return "", fmt.Errorf("PDF not downloaded for paper %s", paperID)
	}

	var storedText string
	if err := c.db.WithContext(ctx).Model(&Paper{}).
		Select("pdf_text").Where("id = ?", paperID).Scan(&storedText).Error; err != nil {
		return "", fmt.Errorf("load PDF text: %w", err)
	}
	if storedText != "" {
		return storedText, nil
	}

	// Extract text from PDF
	text, err := ExtractPDFText(paper.PDFPath)
	if err != nil {
		return "", fmt.Errorf("extract PDF text: %w", err)
	}

	// Store in database
	if err := c.db.WithContext(ctx).Model(&Paper{}).Where("id = ?", paperID).Update("pdf_text", text).Error; err != nil {
		return "", fmt.Errorf("store PDF text: %w", err)
	}

	return text, nil
}

// SearchPDFs searches a bounded amount of cached PDF text. Fuzzy mode uses a
// bounded edit-distance comparison at token boundaries; it is not semantic or
// full-corpus search.
// Returns paper IDs that match the query.
func (c *Cache) SearchPDFs(ctx context.Context, query string, limit int, fuzzyMode bool) ([]PDFSearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if len(query) > 256 {
		return nil, fmt.Errorf("PDF search query exceeds 256 bytes")
	}
	if limit <= 0 {
		limit = 50
	}

	// Get all papers with PDFs downloaded
	var papers []Paper
	searchQuery := c.db.WithContext(ctx).
		Select("id", "pdf_path", "substr(pdf_text, 1, ?) AS pdf_text", maxPDFSearchTextBytes).
		Where("pdf_downloaded = ? AND pdf_path IS NOT NULL AND pdf_text IS NOT NULL AND pdf_text != ''", true)
	if !fuzzyMode {
		searchQuery = searchQuery.Where("LOWER(pdf_text) LIKE ?", "%"+strings.ToLower(query)+"%")
	}
	if err := searchQuery.Order("id").Limit(maxPDFSearchPapers).
		Find(&papers).Error; err != nil {
		return nil, err
	}

	var results []PDFSearchResult
	lowerQuery := strings.ToLower(query)

	// Search through cached text
	type scoredResult struct {
		result PDFSearchResult
		score  float64
	}
	var scoredResults []scoredResult

	scannedBytes := 0
	for _, p := range papers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.PDFText == "" {
			continue
		}

		text := p.PDFText
		if scannedBytes+len(text) > maxPDFSearchScanBytes {
			remaining := maxPDFSearchScanBytes - scannedBytes
			if remaining <= 0 {
				break
			}
			text = text[:remaining]
		}
		scannedBytes += len(text)
		lowerText := strings.ToLower(text)

		var match bool
		var score float64
		var matchPos int = -1

		if fuzzyMode {
			bestScore, bestPos, err := bestBoundedMatch(ctx, lowerText, lowerQuery)
			if err != nil {
				return nil, err
			}
			if bestScore >= 0.8 {
				match = true
				score = bestScore
				matchPos = bestPos
			}
		} else {
			// Exact search (case-insensitive)
			matchPos = strings.Index(lowerText, lowerQuery)
			if matchPos != -1 {
				match = true
				score = 1.0
			}
		}

		if match {
			context := extractContextAt(text, matchPos, query, 200)
			scoredResults = append(scoredResults, scoredResult{
				result: PDFSearchResult{
					PaperID: p.ID,
					Context: context,
					Match:   true,
					Score:   score,
				},
				score: score,
			})
		}
	}

	sort.SliceStable(scoredResults, func(i, j int) bool { return scoredResults[i].score > scoredResults[j].score })

	// Take top results
	for i, sr := range scoredResults {
		if i >= limit {
			break
		}
		results = append(results, sr.result)
	}

	return results, nil
}

// fuzzyMatch calculates similarity between two strings (simple Levenshtein-like)
func fuzzyMatch(text, query string) float64 {
	score, _ := fuzzyMatchContext(context.Background(), text, query)
	return score
}

func fuzzyMatchContext(ctx context.Context, text, query string) (float64, error) {
	score, _, err := bestBoundedMatch(ctx, text, query)
	return score, err
}

func bestBoundedMatch(ctx context.Context, text, query string) (float64, int, error) {
	if len(query) == 0 {
		return 0, -1, nil
	}
	if len(text) < len(query) {
		return 0, -1, nil
	}
	if exact := strings.Index(text, query); exact >= 0 {
		return 1, exact, nil
	}
	if len(query) < 4 {
		return 0, -1, nil
	}

	maxDist := len(query) / 5
	if maxDist < 1 {
		maxDist = 1
	}

	bestMatch := 0.0
	bestPos := -1
	for i := 0; i+len(query) <= len(text); {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, -1, err
			}
		}
		if i == 0 || isPDFTokenBoundary(text[i-1]) {
			distance := boundedLevenshtein(text[i:i+len(query)], query, maxDist)
			if distance <= maxDist {
				score := 1 - float64(distance)/float64(max(len(query), 1))
				if score > bestMatch {
					bestMatch = score
					bestPos = i
				}
			}
		}
		i++
	}
	return bestMatch, bestPos, nil
}

func isPDFTokenBoundary(b byte) bool {
	return b == ' ' || b == '\n' || b == '\r' || b == '\t' || strings.ContainsRune(".,;:!?()[]{}\"'", rune(b))
}

func boundedLevenshtein(a, b string, limit int) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		rowMin := current[0]
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = minInt(current[j-1]+1, previous[j]+1, previous[j-1]+cost)
			if current[j] < rowMin {
				rowMin = current[j]
			}
		}
		if rowMin > limit {
			return limit + 1
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func minInt(values ...int) int {
	minimum := values[0]
	for _, value := range values[1:] {
		if value < minimum {
			minimum = value
		}
	}
	return minimum
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// extractContext extracts text around a match for display.
func extractContext(text, query string, contextLen int) string {
	lowerText := strings.ToLower(text)
	lowerQuery := strings.ToLower(query)
	idx := strings.Index(lowerText, lowerQuery)
	return extractContextAt(text, idx, query, contextLen)
}

// extractContextAt extracts text around a specific position.
func extractContextAt(text string, pos int, query string, contextLen int) string {
	if pos == -1 {
		// Return first part of text
		if len(text) > contextLen {
			return text[:contextLen] + "..."
		}
		return text
	}

	start := pos - contextLen/2
	if start < 0 {
		start = 0
	}
	end := pos + len(query) + contextLen/2
	if end > len(text) {
		end = len(text)
	}

	context := text[start:end]
	if start > 0 {
		context = "..." + context
	}
	if end < len(text) {
		context = context + "..."
	}

	return context
}

// PDFSearchResult represents a PDF search result.
type PDFSearchResult struct {
	PaperID string  `json:"paperId"`
	Context string  `json:"context"`
	Match   bool    `json:"match"`
	Score   float64 `json:"score,omitempty"` // Match quality score (0.0 to 1.0)
}
