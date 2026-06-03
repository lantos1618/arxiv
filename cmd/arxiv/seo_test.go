package main

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCategoryMetadata(t *testing.T) {
	title := categoryTitle("cond-mat")
	if title != "arXiv cond-mat papers: condensed matter" {
		t.Fatalf("unexpected category title: %q", title)
	}

	description := categoryDescription("cs.LG", nil)
	for _, want := range []string{"cs.LG", "machine learning", "semantic search"} {
		if !strings.Contains(description, want) {
			t.Fatalf("description %q missing %q", description, want)
		}
	}
}

func TestHomeMetadata(t *testing.T) {
	description := homeDescription()
	for _, want := range []string{"Search arXiv papers", "related papers", "semantic related-work maps"} {
		if !strings.Contains(description, want) {
			t.Fatalf("home description %q missing %q", description, want)
		}
	}

	structuredData := string(homeStructuredData())
	for _, want := range []string{"\"@type\":\"WebSite\"", "\"@type\":\"SearchAction\"", "/search?q={search_term_string}"} {
		if !strings.Contains(structuredData, want) {
			t.Fatalf("home structured data %q missing %q", structuredData, want)
		}
	}
}

func TestIndexNowKeyValidation(t *testing.T) {
	for _, key := range []string{"34af0c26368622541e3ca8aa555c3ad7", "indexnow-key_2026"} {
		if !isSafeIndexNowKey(key) {
			t.Fatalf("expected key %q to be accepted", key)
		}
	}
	for _, key := range []string{"short", "has/slash", "has.dot", "has space"} {
		if isSafeIndexNowKey(key) {
			t.Fatalf("expected key %q to be rejected", key)
		}
	}
}

func TestNormalizeSearchMode(t *testing.T) {
	tests := []struct {
		target string
		want   string
	}{
		{"/search?q=graph", "search"},
		{"/search?q=graph&mode=quick", "quick"},
		{"/search?q=graph&mode=keyword", "quick"},
		{"/search?q=graph&mode=deep", "deep"},
		{"/search?q=graph&mode=full-paper", "deep"},
		{"/search?q=graph&mode=semantic", "search"},
		{"/search?q=graph&search-mode=semantic", "search"},
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", tt.target, nil)
		if got := normalizeSearchMode(req); got != tt.want {
			t.Fatalf("normalizeSearchMode(%q) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestAuthorQueryCandidate(t *testing.T) {
	author, ok := authorQueryCandidate("  Marcus   Hutter ")
	if !ok || author != "Marcus Hutter" {
		t.Fatalf("expected normalized author query, got %q ok=%v", author, ok)
	}

	for _, query := range []string{"1706.03762", "graph neural networks", "email@example.com", "https://arxiv.org/abs/1706.03762"} {
		if _, ok := authorQueryCandidate(query); ok {
			t.Fatalf("expected %q not to be treated as an author query", query)
		}
	}
}

func TestCleanLatexText(t *testing.T) {
	input := `We use \textit{Transformers} and release code at \url{https://example.com}. See \href{https://paper.example}{paper notes}.`
	got := cleanLatexText(input)
	for _, want := range []string{"Transformers", "https://example.com", "paper notes (https://paper.example)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cleanLatexText(%q) = %q, missing %q", input, got, want)
		}
	}
	for _, unwanted := range []string{`\textit`, `\url`, `\href`} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("cleanLatexText(%q) = %q, still contains %q", input, got, unwanted)
		}
	}
}
