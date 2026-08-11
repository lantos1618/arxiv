package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arxiv "github.com/lantos1618/arxiv.gg"
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
	for _, want := range []string{"Search papers cached by arXiv.gg", "official PDF links", "semantic related-work maps where prepared"} {
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

func TestSearchRejectsWhitespaceOnlyQuery(t *testing.T) {
	rec := httptest.NewRecorder()
	(&server{}).handleSearch(rec, httptest.NewRequest(http.MethodGet, "/search?q=%20%20%20", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Fatalf("whitespace search status=%d location=%q", rec.Code, rec.Header().Get("Location"))
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
		{"/search?q=graph", "quick"},
		{"/search?q=graph&mode=quick", "quick"},
		{"/search?q=graph&mode=keyword", "quick"},
		{"/search?q=graph&mode=deep", "deep"},
		{"/search?q=graph&mode=full-paper", "deep"},
		{"/search?q=graph&mode=semantic", "semantic"},
		{"/search?q=graph&mode=search", "semantic"},
		{"/search?q=graph&search-mode=semantic", "semantic"},
	}

	for _, tt := range tests {
		req := httptest.NewRequest("GET", tt.target, nil)
		if got := normalizeSearchMode(req); got != tt.want {
			t.Fatalf("normalizeSearchMode(%q) = %q, want %q", tt.target, got, tt.want)
		}
	}
}

func TestResponseCacheVariesByCookieForPaperPages(t *testing.T) {
	cm := newCacheMiddleware(time.Minute)
	handler := cm.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("paper page"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/abs/2501.00001", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !headerHasToken(rec.Result().Header, "Vary", "Cookie") {
		t.Fatalf("anonymous paper response missing Vary: Cookie: %q", rec.Result().Header.Values("Vary"))
	}
	if got := rec.Result().Header.Get("Cache-Control"); got != "public, max-age=60" {
		t.Fatalf("anonymous cache-control = %q, want public max-age", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/abs/2501.00001", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "session-token"})
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if !headerHasToken(rec.Result().Header, "Vary", "Cookie") {
		t.Fatalf("signed-in paper response missing Vary: Cookie: %q", rec.Result().Header.Values("Vary"))
	}
	if got := rec.Result().Header.Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("signed-in cache-control = %q, want private no-store", got)
	}
}

func TestRenderTemplateAdminNavRequiresPersistentAdmin(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	req := httptest.NewRequest(http.MethodGet, "/search?admin_token=test-admin-token", nil)
	rec := httptest.NewRecorder()
	(&server{}).renderTemplate(rec, req, "head", map[string]any{"Title": "Search"})
	if strings.Contains(rec.Body.String(), `href="/admin"`) {
		t.Fatalf("query-token request rendered persistent admin nav:\n%s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: "test-admin-token"})
	rec = httptest.NewRecorder()
	(&server{}).renderTemplate(rec, req, "head", map[string]any{"Title": "Home"})
	if !strings.Contains(rec.Body.String(), `href="/admin"`) {
		t.Fatalf("admin-cookie request did not render admin nav:\n%s", rec.Body.String())
	}
}

func TestPaperTemplateUsesCurrentUserInNav(t *testing.T) {
	paper := &arxiv.Paper{
		ID:         "2501.00001",
		Created:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Updated:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Title:      "A Test Paper",
		Abstract:   "A short abstract.",
		Authors:    "Ada Lovelace",
		Categories: "cs.AI",
	}

	var out bytes.Buffer
	err := templates.ExecuteTemplate(&out, "paper", map[string]any{
		"Title":              paper.Title,
		"Description":        paper.Abstract,
		"CanonicalURL":       "https://arxiv.gg/abs/" + paper.ID,
		"Paper":              paper,
		"PaperList":          []arxiv.PaperListItem{},
		"CitedByCount":       0,
		"HasEmbedding":       false,
		"LocalMode":          false,
		"CurrentUser":        &arxiv.User{Email: "reader@example.com"},
		"CurrentUserIsAdmin": false,
	})
	if err != nil {
		t.Fatalf("render paper template: %v", err)
	}

	html := out.String()
	for _, want := range []string{`href="/account">reader@example.com</a>`, `action="/logout"`, `Sign out`} {
		if !strings.Contains(html, want) {
			t.Fatalf("paper nav missing %q in:\n%s", want, html)
		}
	}
	if strings.Contains(html, `href="/login">Sign in</a>`) {
		t.Fatalf("paper nav rendered signed-out link for signed-in user")
	}
	if strings.Contains(html, `class="reader-cta"`) {
		t.Fatalf("paper template rendered the signup prompt for a signed-in user")
	}
}

func TestPaperTemplateAutoLoadsRelatedWorkMapForEmbeddedPapers(t *testing.T) {
	paper := &arxiv.Paper{
		ID:         "2501.00001",
		Created:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Updated:    time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		Title:      "A Test Paper",
		Abstract:   "A short abstract.",
		Authors:    "Ada Lovelace",
		Categories: "cs.AI",
	}

	var out bytes.Buffer
	err := templates.ExecuteTemplate(&out, "paper", map[string]any{
		"Title":              paper.Title,
		"Description":        paper.Abstract,
		"CanonicalURL":       "https://arxiv.gg/abs/" + paper.ID,
		"Paper":              paper,
		"PaperList":          []arxiv.PaperListItem{},
		"CitedByCount":       0,
		"HasEmbedding":       true,
		"LocalMode":          false,
		"CurrentUser":        nil,
		"CurrentUserIsAdmin": false,
	})
	if err != nil {
		t.Fatalf("render paper template: %v", err)
	}

	html := out.String()
	for _, want := range []string{
		`Loading map...`,
		`Loading related work map...`,
		`scheduleSemanticMapAutoload();`,
		`let hasExistingEmbedding = true;`,
		`Keep this paper in your research trail`,
		`href="/login?next=%2Fabs%2F2501.00001"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("embedded paper template missing %q in:\n%s", want, html)
		}
	}
	if strings.Contains(html, `Ready. Use Find Similar to load the related-work map.`) {
		t.Fatalf("embedded paper template still renders the obsolete click-to-load prompt")
	}
}

func headerHasToken(headers http.Header, key, want string) bool {
	for _, value := range headers.Values(key) {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), want) {
				return true
			}
		}
	}
	return false
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

func TestAdminTemplatesDoNotExposeRoadmapScaffolding(t *testing.T) {
	for _, name := range []string{"admin.html", "admin_users.html", "admin_audit.html"} {
		body, err := templateFS.ReadFile("templates/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		content := strings.ToLower(string(body))
		for _, forbidden := range []string{"placeholder", "not wired yet", "future paid user", "reserved for higher limits"} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("%s contains unfinished UI copy %q", name, forbidden)
			}
		}
	}
}

func TestApprovedUICopyIsTruthfulAndScoped(t *testing.T) {
	tests := []struct {
		name      string
		required  []string
		forbidden []string
	}{
		{
			name:      "head.html",
			required:  []string{"Help shape arXiv.gg"},
			forbidden: []string{"community-ask", "Selected ideas may qualify for $100", "Delete permanently"},
		},
		{
			name:      "feedback.html",
			required:  []string{"Share real friction", "Add community signal", "We assess the fit", "Selected work ships", "does not guarantee selection or payment", "Administrators can still identify my account"},
			forbidden: []string{"people use it, you get $100"},
		},
		{
			name:      "account.html",
			required:  []string{"immediately revokes the current key", "PeerViewsSource", "Global signed-in reading activity", "Copy failed"},
			forbidden: []string{"What This Unlocks Next", "Saved searches and paper maps", "category alerts"},
		},
		{
			name:      "paper.html",
			required:  []string{"waitForPaperPreparation", "Checking queue status every 3 seconds", "Reference metadata could not be loaded", "Metadata not cached"},
			forbidden: []string{"Encoding: \"", "recommendation-service"},
		},
		{
			name:      "search.html",
			required:  []string{"value=\"quick\"", "<span>Semantic</span>", "retryRecommended", "No cached papers matched this Quick Search"},
			forbidden: []string{"Prepare Catalog", "reindex --embeddings", "generate-embeddings-btn", "GPU is starting", "Starting the search GPU"},
		},
		{
			name:      "index.html",
			required:  []string{"value=\"quick\"", "<span>Semantic</span>", "retryRecommended", "doFullSearch(input.value, 'quick')"},
			forbidden: []string{"GPU is starting", "Starting the search GPU"},
		},
	}
	for _, tt := range tests {
		body, err := templateFS.ReadFile("templates/" + tt.name)
		if err != nil {
			t.Fatal(err)
		}
		content := string(body)
		for _, want := range tt.required {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q", tt.name, want)
			}
		}
		for _, unwanted := range tt.forbidden {
			if strings.Contains(content, unwanted) {
				t.Errorf("%s contains %q", tt.name, unwanted)
			}
		}
	}
}

func TestObsoleteAdminEmbeddingsTemplateRemoved(t *testing.T) {
	if _, err := templateFS.ReadFile("templates/admin_embeddings.html"); err == nil {
		t.Fatal("obsolete admin embeddings template is still embedded")
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
