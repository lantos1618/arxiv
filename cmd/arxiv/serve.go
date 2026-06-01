package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lantos1618/arxiv.gg"
)

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"truncate": func(s string, n int) string {
		if len(s) <= n {
			return s
		}
		return s[:n] + "..."
	},
	"formatInt": func(n int64) string {
		return formatInt(n)
	},
	"percent": func(n, total int64) string {
		if total <= 0 {
			return "0.0%"
		}
		return fmt.Sprintf("%.1f%%", float64(n)/float64(total)*100)
	},
	"mapInt": func(m map[string]int64, key string) int64 {
		if m == nil {
			return 0
		}
		return m[key]
	},
	"timeOrDash": func(t *time.Time) string {
		if t == nil || t.IsZero() {
			return "-"
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"timeStamp": func(t time.Time) string {
		if t.IsZero() {
			return "-"
		}
		return t.UTC().Format("2006-01-02 15:04")
	},
	"parseAuthors":    parseAuthors,
	"limitAuthors":    limitAuthors,
	"parseCategories": parseCategories,
	"arxivIDToDate":   arxivIDToDate,
	"authorPath":      authorPath,
	"categoryPath":    categoryPath,
	"paperPath":       paperPath,
	"mul": func(a, b interface{}) float64 {
		var aFloat, bFloat float64
		switch v := a.(type) {
		case int:
			aFloat = float64(v)
		case float64:
			aFloat = v
		case float32:
			aFloat = float64(v)
		}
		switch v := b.(type) {
		case int:
			bFloat = float64(v)
		case float64:
			bFloat = v
		case float32:
			bFloat = float64(v)
		}
		return aFloat * bFloat
	},
}).ParseFS(templateFS, "templates/*.html"))

const defaultIndexNowKey = "34af0c26368622541e3ca8aa555c3ad7"
const defaultOfficialArxivPapers = 3045638
const defaultOfficialArxivPapersAsOf = "2026-05-16"

func cmdServe(ctx context.Context, cacheDir string, args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fs.Int("port", 8080, "Port to listen on")
	localMode := fs.Bool("local", false, "Enable local PDF/source caching (downloads files locally instead of redirecting to arxiv.org)")
	embeddingService := fs.String("embedding-service", "", "URL of embedding service (e.g., http://localhost:8001)")
	enableEmbeddingWorker := fs.Bool("embedding-worker", false, "Enable background embedding worker")
	fs.Parse(args)

	// Check environment variable for embedding service URL if not provided via flag
	embeddingServiceURL := *embeddingService
	if embeddingServiceURL == "" {
		embeddingServiceURL = os.Getenv("EMBEDDING_SERVICE_URL")
	}
	qwenEmbeddingServiceURL := strings.TrimSpace(os.Getenv("QWEN_EMBEDDING_SERVICE_URL"))
	trustProxyHeaders := os.Getenv("TRUST_PROXY_HEADERS") == "true"
	indexNowKey := configuredIndexNowKey()
	officialArxivPapers := configuredOfficialArxivPapers()
	officialArxivPapersAsOf := configuredOfficialArxivPapersAsOf()

	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		log.Fatalf("open cache: %v", err)
	}

	// Start background stats refresh so homepage never blocks on COUNT(*) queries
	cache.StartStatsRefresh(ctx)
	cache.StartAdminStatsRefresh(ctx)

	srv := &server{
		cache:                   cache,
		cacheDir:                cacheDir,
		localMode:               *localMode,
		paperBroadcast:          newPaperBroadcaster(),
		embeddingServiceURL:     embeddingServiceURL,
		qwenEmbeddingServiceURL: qwenEmbeddingServiceURL,
		indexNowKey:             indexNowKey,
		officialArxivPapers:     officialArxivPapers,
		officialArxivAsOf:       officialArxivPapersAsOf,
		trustProxyHeaders:       trustProxyHeaders,
		publicEmbeddingLimiter: newRateLimiter(
			6,
			time.Minute,
			trustProxyHeaders,
		),
		loginLimiter: newRateLimiter(
			12,
			10*time.Minute,
			trustProxyHeaders,
		),
	}

	// Start embedding worker if enabled
	if *enableEmbeddingWorker && embeddingServiceURL != "" {
		config := arxiv.DefaultEmbeddingWorkerConfig()
		config.ServiceURL = embeddingServiceURL
		srv.embeddingWorker = arxiv.NewEmbeddingWorker(cache, config)
		srv.embeddingWorker.Start(ctx)
		defer srv.embeddingWorker.Stop()
	}
	mux := http.NewServeMux()

	// API routes (before other routes for proper matching)
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/api/v1/", srv.handleAPIRoot)
	mux.HandleFunc("/api/v1/papers/", srv.handleAPIPaper)
	mux.HandleFunc("/api/v1/search", srv.handleAPISearch)
	mux.HandleFunc("/api/v1/search/quick", srv.handleAPISearchQuick)
	mux.HandleFunc("/api/v1/search/stream", srv.handleAPISearchStream)
	mux.HandleFunc("/api/v1/search/semantic", srv.handleAPISearchSemantic)
	mux.HandleFunc("/api/v1/search/pdf", srv.handleAPISearchPDF)
	mux.HandleFunc("/api/v1/categories", srv.handleAPICategories)
	mux.HandleFunc("/api/v1/stats", srv.handleAPIStats)
	mux.HandleFunc("/api/v1/embeddings/generate", srv.handleAPIGenerateEmbeddings)
	mux.HandleFunc("/api/v1/embeddings/status", srv.handleAPIEmbeddingWorkerStatus)
	mux.HandleFunc("/api/v1/papers/recent/stream", srv.handleAPIRecentPapersStream)
	mux.HandleFunc("/api/v1/authors/collaborators", srv.handleAPIAuthorCollaborators)
	mux.HandleFunc("/api/v1/authors/similar", srv.handleAPIAuthorSimilar)
	mux.HandleFunc("/api/v1/authors/stats", srv.handleAPIAuthorStats)
	mux.HandleFunc("/api/v1/authors/graph", srv.handleAPIAuthorGraph)
	mux.HandleFunc("/api/v1/authors/profile", srv.handleAPIAuthorProfile)
	mux.HandleFunc("/api/v1/authors/build-graph", srv.handleAPIBuildAuthorGraph)

	// Web routes
	mux.HandleFunc("/", srv.handleIndex)
	mux.HandleFunc("/search", srv.handleSearch)
	mux.HandleFunc("/login", srv.handleLogin)
	mux.HandleFunc("/auth/google/start", srv.handleGoogleOAuthStart)
	mux.HandleFunc("/auth/google/callback", srv.handleGoogleOAuthCallback)
	mux.HandleFunc("/logout", srv.handleLogout)
	mux.HandleFunc("/account", srv.handleAccount)
	mux.HandleFunc("/paper/", srv.handlePaper)
	mux.HandleFunc("/abs/", srv.handleAbs)
	mux.HandleFunc("/author/", srv.handleAuthor)
	mux.HandleFunc("/category/", srv.handleCategory)
	mux.HandleFunc("/categories", srv.handleCategories)
	mux.HandleFunc("/src/", srv.handleSource)
	mux.HandleFunc("/pdf/", srv.handlePDF)
	mux.HandleFunc("/robots.txt", srv.handleRobots)
	mux.HandleFunc("/sitemap.xml", srv.handleSitemap)
	mux.HandleFunc("/sitemap-static.xml", srv.handleStaticSitemap)
	mux.HandleFunc("/sitemaps/", srv.handlePaperSitemap)
	mux.HandleFunc("/BingSiteAuth.xml", srv.handleBingSiteAuth)
	if srv.indexNowKey != "" {
		mux.HandleFunc("/"+srv.indexNowKey+".txt", srv.handleIndexNowKey)
	}
	mux.HandleFunc("/favicon.ico", srv.handleFavicon)
	mux.HandleFunc("/favicon.svg", srv.handleFavicon)
	mux.HandleFunc("/health", srv.handleHealth)

	// Admin routes.
	mux.HandleFunc("/admin", srv.handleAdminDashboard)
	mux.HandleFunc("/admin/users", srv.handleAdminUsers)
	mux.HandleFunc("/admin/audit", srv.handleAdminAudit)
	mux.HandleFunc("/admin/embeddings", srv.handleAdminEmbeddings)

	// Setup middleware
	cacheMW := newCacheMiddleware(5 * time.Minute)                      // Cache for 5 minutes
	rateLimiter := newRateLimiter(1000, time.Minute, trustProxyHeaders) // Allow higher burst per IP

	// Apply middleware: rate limiting first, then caching/security headers.
	handler := securityHeadersMiddleware(rateLimiter.Handler(cacheMW.Handler(mux)))

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Starting server at http://localhost%s", addr)
	log.Printf("API available at http://localhost%s/api/v1/", addr)

	httpServer := &http.Server{Addr: addr, Handler: handler}
	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("server error: %v", err)
	}
}

type server struct {
	cache     *arxiv.Cache
	cacheDir  string
	localMode bool // Enable local PDF/source caching instead of redirecting to arxiv.org

	// Real-time paper broadcast
	paperBroadcast *paperBroadcaster

	// Embedding service configuration
	embeddingServiceURL     string
	qwenEmbeddingServiceURL string
	embeddingWorker         *arxiv.EmbeddingWorker
	indexNowKey             string
	officialArxivPapers     int64
	officialArxivAsOf       string
	trustProxyHeaders       bool
	publicEmbeddingLimiter  *rateLimiter
	loginLimiter            *rateLimiter
}

func configuredIndexNowKey() string {
	key := strings.TrimSpace(os.Getenv("INDEXNOW_KEY"))
	if key == "" {
		key = defaultIndexNowKey
	}
	if !isSafeIndexNowKey(key) {
		log.Printf("INDEXNOW_KEY contains unsupported characters; IndexNow key file route disabled")
		return ""
	}
	return key
}

func isSafeIndexNowKey(key string) bool {
	if len(key) < 8 || len(key) > 128 {
		return false
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func configuredOfficialArxivPapers() int64 {
	value := strings.TrimSpace(os.Getenv("ARXIV_OFFICIAL_TOTAL_PAPERS"))
	if value == "" {
		return defaultOfficialArxivPapers
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("ARXIV_OFFICIAL_TOTAL_PAPERS is invalid; using default %d", defaultOfficialArxivPapers)
		return defaultOfficialArxivPapers
	}
	return n
}

func configuredOfficialArxivPapersAsOf() string {
	value := strings.TrimSpace(os.Getenv("ARXIV_OFFICIAL_TOTAL_PAPERS_AS_OF"))
	if value == "" {
		return defaultOfficialArxivPapersAsOf
	}
	return value
}

func formatInt(n int64) string {
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return sign + s
	}
	var b strings.Builder
	b.WriteString(sign)
	first := len(s) % 3
	if first == 0 {
		first = 3
	}
	b.WriteString(s[:first])
	for i := first; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

type coverageSignal struct {
	LocalTotal    int64
	OfficialTotal int64
	Percent       string
	AsOf          string
}

func (s *server) coverageSignal(stats *arxiv.CacheStats) coverageSignal {
	coverage := coverageSignal{
		OfficialTotal: s.officialArxivPapers,
		AsOf:          s.officialArxivAsOf,
	}
	if stats != nil {
		coverage.LocalTotal = stats.TotalPapers
	}
	if coverage.OfficialTotal > 0 {
		coverage.Percent = fmt.Sprintf("%.1f", float64(coverage.LocalTotal)/float64(coverage.OfficialTotal)*100)
	}
	return coverage
}

// paperBroadcaster manages real-time paper update subscriptions
type paperBroadcaster struct {
	subscribers   map[chan paperEvent]struct{}
	subscriberIPs map[chan paperEvent]string
	byIP          map[string]int
	mu            sync.RWMutex
}

type paperEvent struct {
	Paper        arxiv.Paper `json:"paper"`
	HasEmbedding bool        `json:"hasEmbedding"`
}

func newPaperBroadcaster() *paperBroadcaster {
	return &paperBroadcaster{
		subscribers:   make(map[chan paperEvent]struct{}),
		subscriberIPs: make(map[chan paperEvent]string),
		byIP:          make(map[string]int),
	}
}

const (
	maxSSEConnections      = 1000
	maxSSEConnectionsPerIP = 6
)

func (b *paperBroadcaster) Subscribe(ip string) (chan paperEvent, bool) {
	ch := make(chan paperEvent, 10)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subscribers) >= maxSSEConnections {
		return nil, false
	}
	if b.byIP[ip] >= maxSSEConnectionsPerIP {
		return nil, false
	}
	b.subscribers[ch] = struct{}{}
	b.subscriberIPs[ch] = ip
	b.byIP[ip]++
	return ch, true
}

func (b *paperBroadcaster) Unsubscribe(ch chan paperEvent) {
	b.mu.Lock()
	if _, ok := b.subscribers[ch]; ok {
		if ip := b.subscriberIPs[ch]; ip != "" {
			b.byIP[ip]--
			if b.byIP[ip] <= 0 {
				delete(b.byIP, ip)
			}
			delete(b.subscriberIPs, ch)
		}
		delete(b.subscribers, ch)
		close(ch)
	}
	b.mu.Unlock()
}

func (b *paperBroadcaster) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

func (b *paperBroadcaster) Broadcast(event paperEvent) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			// Skip if channel is full
		}
	}
}

// paperWithEmbedding wraps a paper with its embedding status for templates
type paperWithEmbedding struct {
	arxiv.Paper
	HasEmbedding bool
}

const sitemapPaperURLLimit = 50000

// staticSitemapURLs collects public, crawlable non-paper URLs.
func (s *server) staticSitemapURLs(ctx context.Context) (arxiv.SitemapURLSet, error) {
	base := arxiv.SiteBaseURL()

	var urls arxiv.SitemapURLSet

	// Static top-level pages
	now := time.Now()
	urls = append(urls,
		arxiv.SitemapURL{
			Loc:        base + "/",
			LastMod:    &now,
			ChangeFreq: "daily",
			Priority:   1.0,
		},
		arxiv.SitemapURL{
			Loc:        base + "/categories",
			LastMod:    &now,
			ChangeFreq: "daily",
			Priority:   0.8,
		},
	)

	// Categories and a slice of recent papers per category
	categories, err := s.cache.ListCategories(ctx)
	if err != nil {
		return nil, err
	}

	for _, c := range categories {
		// Category listing page
		urls = append(urls, arxiv.SitemapURL{
			Loc:        fmt.Sprintf("%s/category/%s", base, c.Name),
			ChangeFreq: "daily",
			Priority:   0.7,
		})
	}

	return urls, nil
}

func (s *server) sitemapIndex(ctx context.Context) (arxiv.SitemapIndex, error) {
	base := arxiv.SiteBaseURL()
	count, err := s.cache.CountSitemapPapers(ctx)
	if err != nil {
		return nil, err
	}

	index := arxiv.SitemapIndex{
		{Loc: base + "/sitemap-static.xml"},
	}

	chunks := int((count + sitemapPaperURLLimit - 1) / sitemapPaperURLLimit)
	for i := 1; i <= chunks; i++ {
		index = append(index, arxiv.SitemapIndexEntry{
			Loc: fmt.Sprintf("%s/sitemaps/papers-%d.xml", base, i),
		})
	}

	return index, nil
}

func (s *server) paperSitemapURLs(ctx context.Context, page int) (arxiv.SitemapURLSet, error) {
	if page < 1 {
		return nil, fmt.Errorf("invalid sitemap page")
	}

	papers, err := s.cache.ListSitemapPapers(ctx, (page-1)*sitemapPaperURLLimit, sitemapPaperURLLimit)
	if err != nil {
		return nil, err
	}
	if len(papers) == 0 {
		return nil, os.ErrNotExist
	}

	base := arxiv.SiteBaseURL()
	urls := make(arxiv.SitemapURLSet, 0, len(papers))
	for _, p := range papers {
		lastMod := p.Updated
		urls = append(urls, arxiv.SitemapURL{
			Loc:        fmt.Sprintf("%s/abs/%s", base, p.ID),
			LastMod:    &lastMod,
			ChangeFreq: "weekly",
			Priority:   0.6,
		})
	}
	return urls, nil
}

func writeXML(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func rejectNonGetHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	return false
}

// handleSitemap serves the sitemap index at /sitemap.xml.
func (s *server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	if rejectNonGetHead(w, r) {
		return
	}
	ctx := r.Context()
	index, err := s.sitemapIndex(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := arxiv.BuildSitemapIndexXML(index)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeXML(w, r, data)
}

// handleStaticSitemap serves top-level and category URLs.
func (s *server) handleStaticSitemap(w http.ResponseWriter, r *http.Request) {
	if rejectNonGetHead(w, r) {
		return
	}

	urls, err := s.staticSitemapURLs(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := arxiv.BuildSitemapXML(urls)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeXML(w, r, data)
}

// handlePaperSitemap serves chunked paper sitemap files under /sitemaps/.
func (s *server) handlePaperSitemap(w http.ResponseWriter, r *http.Request) {
	if rejectNonGetHead(w, r) {
		return
	}

	name := strings.TrimPrefix(r.URL.Path, "/sitemaps/")
	if !strings.HasPrefix(name, "papers-") || !strings.HasSuffix(name, ".xml") {
		http.NotFound(w, r)
		return
	}

	pageText := strings.TrimSuffix(strings.TrimPrefix(name, "papers-"), ".xml")
	page, err := strconv.Atoi(pageText)
	if err != nil || page < 1 {
		http.NotFound(w, r)
		return
	}

	urls, err := s.paperSitemapURLs(r.Context(), page)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data, err := arxiv.BuildSitemapXML(urls)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeXML(w, r, data)
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	if err := s.cache.Ping(ctx); err != nil {
		respondJSON(w, http.StatusServiceUnavailable, APIResponse{
			Success: false,
			Error:   "database unavailable",
		})
		return
	}

	respondJSON(w, http.StatusOK, APIResponse{
		Success: true,
		Data: map[string]any{
			"status": "ok",
			"db":     s.cache.DBType(),
		},
	})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()
	stats, err := s.cache.Stats(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Papers are now loaded via SSE in the client
	data := map[string]any{
		"Title":    "Home",
		"Stats":    stats,
		"Coverage": s.coverageSignal(stats),
		"Query":    "",
	}
	s.renderTemplate(w, r, "index", data)
}

// handleAPIRoot renders a simple HTML overview for /api/v1/.
// The actual JSON endpoints live under /api/v1/papers, /api/v1/search, etc.
func (s *server) handleAPIRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/v1/" {
		http.NotFound(w, r)
		return
	}

	base := arxiv.SiteBaseURL()
	data := map[string]any{
		"Title":   "API",
		"BaseURL": base,
	}
	s.renderTemplate(w, r, "api", data)
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Check if query looks like an arXiv ID or URL - redirect to abs page
	if arxivID := extractArxivID(query); arxivID != "" {
		http.Redirect(w, r, "/abs/"+arxivID, http.StatusFound)
		return
	}

	ctx := r.Context()
	searchMode := normalizeSearchMode(r)

	if r.URL.Query().Get("format") == "json" {
		papers, err := s.cache.Search(ctx, query, "", 100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		type searchResult struct {
			ID         string `json:"id"`
			Title      string `json:"title"`
			Authors    string `json:"authors"`
			Categories string `json:"categories"`
			Src        bool   `json:"src"`
			PDF        bool   `json:"pdf"`
		}
		results := make([]searchResult, len(papers))
		for i, p := range papers {
			results[i] = searchResult{
				ID:         p.ID,
				Title:      p.Title,
				Authors:    p.Authors,
				Categories: p.Categories,
				Src:        p.SourceDownloaded,
				PDF:        p.PDFDownloaded,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
		return
	}

	var data map[string]any

	if searchMode == "search" || searchMode == "deep" {
		if searchMode == "deep" {
			if _, ok := s.currentUser(r); !ok {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
		}

		stats, err := s.cache.Stats(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if stats.QwenEmbeddingsCount == 0 {
			data = map[string]any{
				"Title":           "Search",
				"Query":           query,
				"SearchMode":      searchMode,
				"IsSemantic":      true,
				"IsDeep":          searchMode == "deep",
				"NoEmbeddings":    true,
				"Papers":          []arxiv.Paper{},
				"SemanticResults": []arxiv.SemanticResult{},
				"DeepResults":     []arxiv.DeepSearchResult{},
			}
			s.renderTemplate(w, r, "search", data)
			return
		}

		queryEmbedding, err := s.generateQwenQueryEmbedding(ctx, query)
		if err != nil {
			http.Error(w, "Failed to understand query: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		if searchMode == "deep" {
			deepResults, err := s.cache.SearchDeepQwen(ctx, queryEmbedding, 100)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			data = map[string]any{
				"Title":           "Search",
				"Query":           query,
				"SearchMode":      searchMode,
				"IsSemantic":      true,
				"IsDeep":          true,
				"Papers":          []arxiv.Paper{},
				"SemanticResults": []arxiv.SemanticResult{},
				"DeepResults":     deepResults,
			}
		} else {
			semanticResults, err := s.cache.SearchSemanticQwen(ctx, queryEmbedding, 100)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			data = map[string]any{
				"Title":           "Search",
				"Query":           query,
				"SearchMode":      searchMode,
				"IsSemantic":      true,
				"IsDeep":          false,
				"Papers":          []arxiv.Paper{},
				"SemanticResults": semanticResults,
				"DeepResults":     []arxiv.DeepSearchResult{},
			}
		}
	} else {
		papers, err := s.cache.Search(ctx, query, "", 100)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		data = map[string]any{
			"Title":           "Search",
			"Query":           query,
			"SearchMode":      searchMode,
			"IsSemantic":      false,
			"IsDeep":          false,
			"Papers":          papers,
			"SemanticResults": []arxiv.SemanticResult{},
			"DeepResults":     []arxiv.DeepSearchResult{},
		}
	}

	s.renderTemplate(w, r, "search", data)
}

type refInfo struct {
	ID        string
	Title     string
	HasTitle  bool // Has metadata (title available)
	HasSource bool // Has full source downloaded
	CitedBy   int
}

func (s *server) handlePaper(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/paper/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	// Handle /paper/:id/fetch - fetch paper on demand
	if strings.HasSuffix(path, "/fetch") {
		paperID := strings.TrimSuffix(path, "/fetch")
		if r.Method != http.MethodPost {
			http.Redirect(w, r, "/paper/"+paperID, http.StatusSeeOther)
			return
		}
		if !s.localMode && !s.requireAdmin(w, r) {
			return
		}
		if !isArxivID(paperID) {
			http.NotFound(w, r)
			return
		}

		// Fetch metadata only (source only in local mode for arXiv ToS compliance)
		opts := &arxiv.DownloadOptions{DownloadPDF: false, DownloadSource: s.localMode}
		paper, err := s.cache.FetchAndDownload(ctx, paperID, opts)
		if err != nil {
			http.Error(w, "failed to fetch paper: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Broadcast new paper to all SSE subscribers
		s.paperBroadcast.Broadcast(paperEvent{
			Paper:        *paper,
			HasEmbedding: s.cache.HasQwenEmbedding(ctx, paper.ID),
		})

		// Redirect to paper page
		http.Redirect(w, r, "/paper/"+paperID, http.StatusSeeOther)
		return
	}

	// Handle /paper/:id/graph - return citation graph JSON
	if strings.HasSuffix(path, "/graph") {
		paperID := strings.TrimSuffix(path, "/graph")
		graph, err := s.cache.GetCitationGraph(ctx, paperID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(graph)
		return
	}

	// Handle /paper/:id/prefetch-refs - prefetch reference titles
	if strings.HasSuffix(path, "/prefetch-refs") {
		paperID := strings.TrimSuffix(path, "/prefetch-refs")
		if r.Method == http.MethodPost {
			if !s.localMode && !s.requireAdmin(w, r) {
				return
			}
			// Synchronous prefetch - blocks until all titles are fetched
			err := s.cache.PrefetchReferenceTitles(ctx, paperID)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			// Return JSON for AJAX requests
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		// GET returns status of uncached references
		uncached, _ := s.cache.UncachedReferenceCount(ctx, paperID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"uncached": uncached})
		return
	}

	// Handle /paper/:id/fetch-source - fetch source and extract citations (local mode only)
	if strings.HasSuffix(path, "/fetch-source") {
		if !s.localMode {
			http.Error(w, "source downloading disabled on public site", http.StatusForbidden)
			return
		}
		paperID := strings.TrimSuffix(path, "/fetch-source")
		if r.Method == http.MethodPost {
			// Download source only (not PDF)
			opts := &arxiv.DownloadOptions{DownloadPDF: false, DownloadSource: true}
			if err := s.cache.DownloadPaper(ctx, paperID, opts); err != nil {
				http.Error(w, "failed to fetch source: "+err.Error(), http.StatusInternalServerError)
				return
			}
			http.Redirect(w, r, "/paper/"+paperID, http.StatusSeeOther)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Handle /paper/:id/status - return paper status JSON (for polling)
	if strings.HasSuffix(path, "/status") {
		paperID := strings.TrimSuffix(path, "/status")
		paper, err := s.cache.GetPaper(ctx, paperID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		refs, _ := s.cache.References(ctx, paperID)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"sourceDownloaded": paper.SourceDownloaded,
			"hasReferences":    len(refs) > 0,
			"refCount":         len(refs),
		})
		return
	}

	// Handle /paper/:id/refs - return references JSON (for live updates)
	if strings.HasSuffix(path, "/refs") {
		paperID := strings.TrimSuffix(path, "/refs")
		dbRefs, err := s.cache.References(ctx, paperID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		type refJSON struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			HasTitle  bool   `json:"hasTitle"`
			HasSource bool   `json:"hasSource"`
			CitedBy   int    `json:"citedBy"`
		}
		refs := make([]refJSON, len(dbRefs))
		for i, r := range dbRefs {
			refs[i] = refJSON{
				ID:        r.ID,
				Title:     r.Title,
				HasTitle:  r.HasTitle,
				HasSource: r.HasSource,
			}
			if r.HasTitle {
				refs[i].CitedBy, _ = s.cache.CitedByCount(ctx, r.ID)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(refs)
		return
	}

	// Handle /paper/:id/export/:format - export paper (bibtex, ris, json)
	if strings.Contains(path, "/export/") {
		parts := strings.Split(path, "/export/")
		if len(parts) == 2 {
			paperID := parts[0]
			format := parts[1]
			paper, err := s.cache.GetPaper(ctx, paperID)
			if err != nil {
				http.Error(w, "paper not found", http.StatusNotFound)
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
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("Content-Disposition", `attachment; filename="`+paperID+`.json"`)
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(paper)
				return
			}
		}
	}

	id := path

	// Redirect /paper/{id} to /abs/{id} for canonical viewing.
	// All action routes (/fetch, /graph, /refs, etc.) are handled above,
	// so anything reaching here is a plain paper ID - including old-style
	// IDs with slashes like "astro-ph/0510857".
	http.Redirect(w, r, "/abs/"+id, http.StatusMovedPermanently)
}

// handleAbs serves arXiv-style abstract URLs at /abs/{id}, mirroring arxiv.org.
// This allows users to switch between arxiv.org and arxiv.gg by only changing
// the domain part of the URL.
func (s *server) handleAbs(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/abs/")
	if id == "" {
		http.NotFound(w, r)
		return
	}

	canonicalID := strings.TrimRight(id, "/")
	canonicalID = stripArxivVersion(canonicalID)
	if canonicalID != id {
		http.Redirect(w, r, paperPath(canonicalID), http.StatusMovedPermanently)
		return
	}

	s.renderPaper(w, r, id)
}

// renderPaper contains the core logic for rendering a paper page given an ID.
func (s *server) renderPaper(w http.ResponseWriter, r *http.Request, id string) {
	if r.URL.Query().Get("admin_token") != "" {
		if !s.requireAdmin(w, r) {
			return
		}
		http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
		return
	}

	ctx := r.Context()

	paper, err := s.cache.GetPaper(ctx, id)
	if err != nil {
		// Paper not in cache - check if it looks like a valid arXiv ID and auto-fetch
		if isArxivID(id) {
			// Fetch metadata only (source downloading disabled for public site)
			opts := &arxiv.DownloadOptions{DownloadPDF: false, DownloadSource: s.localMode}
			paper, err = s.cache.FetchAndDownload(ctx, id, opts)
			if err != nil {
				http.Error(w, "Paper not found: "+err.Error(), http.StatusNotFound)
				return
			}
			// Broadcast new paper to all SSE subscribers
			s.paperBroadcast.Broadcast(paperEvent{
				Paper:        *paper,
				HasEmbedding: s.cache.HasQwenEmbedding(ctx, paper.ID),
			})
		} else {
			http.NotFound(w, r)
			return
		}
	}

	// Get citation count for this paper
	citedByCount, _ := s.cache.CitedByCount(ctx, id)

	var files []string
	if paper.SourceDownloaded && paper.SourcePath != "" {
		filepath.WalkDir(paper.SourcePath, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(paper.SourcePath, path)
			files = append(files, rel)
			return nil
		})
	}

	// Get paper list for sidebar
	paperList, _ := s.cache.GetPaperList(ctx, id)

	// Count uncached references
	uncachedCount := 0
	for _, p := range paperList {
		if !p.Cached && p.IsRef {
			uncachedCount++
		}
	}

	// Source downloading disabled for public site (arXiv ToS compliance)
	// Only available in local mode for personal use
	fetchingSource := false
	// Note: Client handles prefetch via /prefetch-refs endpoint

	hasEmbedding := s.cache.HasQwenEmbedding(ctx, id)

	data := map[string]any{
		"Title":          paper.Title,
		"Description":    summaryText(paper.Abstract, 160),
		"CanonicalURL":   canonicalURL(paperPath(paper.ID)),
		"StructuredData": paperStructuredData(paper),
		"Paper":          paper,
		"Files":          files,
		"PaperList":      paperList,
		"UncachedCount":  uncachedCount,
		"CitedByCount":   citedByCount,
		"FetchingSource": fetchingSource,
		"HasEmbedding":   hasEmbedding,
		"LocalMode":      s.localMode,
	}
	s.renderTemplate(w, r, "paper", data)
}

func (s *server) handleAuthor(w http.ResponseWriter, r *http.Request) {
	author := strings.TrimPrefix(r.URL.Path, "/author/")
	if author == "" {
		http.NotFound(w, r)
		return
	}
	canonicalAuthor := strings.TrimRight(author, "/")
	if canonicalAuthor != author {
		http.Redirect(w, r, authorPath(canonicalAuthor), http.StatusMovedPermanently)
		return
	}

	ctx := r.Context()
	papers, err := s.cache.SearchByAuthor(ctx, author, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Only load basic stats - collaborators/similar loaded via JS for faster page load
	data := map[string]any{
		"Title":          "Author: " + author,
		"Description":    authorDescription(author, papers),
		"CanonicalURL":   canonicalURL(authorPath(author)),
		"StructuredData": authorStructuredData(author, papers),
		"Author":         author,
		"Papers":         papers,
		"PaperCount":     len(papers),
	}
	s.renderTemplate(w, r, "author", data)
}

func (s *server) handlePDF(w http.ResponseWriter, r *http.Request) {
	// Routes: GET /pdf/{id} - serve PDF, POST /pdf/{id}/fetch - fetch PDF
	path := strings.TrimPrefix(r.URL.Path, "/pdf/")
	if path == "" {
		http.NotFound(w, r)
		return
	}

	ctx := r.Context()

	// Check if this is a fetch request
	if strings.HasSuffix(path, "/fetch") {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !s.localMode && !s.requireAdmin(w, r) {
			return
		}

		paperID := strings.TrimSuffix(path, "/fetch")
		returnTo := r.URL.Query().Get("return")

		// First ensure paper metadata exists (fetch if needed)
		paper, err := s.cache.Fetch(ctx, paperID)
		if err != nil {
			http.Error(w, "failed to fetch paper: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Download PDF
		opts := &arxiv.DownloadOptions{DownloadPDF: true, DownloadSource: false}
		if err := s.cache.DownloadPaper(ctx, paper.ID, opts); err != nil {
			http.Error(w, "failed to download PDF: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Redirect back
		if returnTo != "" {
			http.Redirect(w, r, "/paper/"+returnTo, http.StatusSeeOther)
		} else {
			http.Redirect(w, r, "/pdf/"+paperID, http.StatusSeeOther)
		}
		return
	}

	// Serve PDF: GET /pdf/{id}
	paperID := path

	paper, err := s.cache.Fetch(ctx, paperID)
	if err != nil {
		http.Error(w, "paper not found: "+err.Error(), http.StatusNotFound)
		return
	}

	// Local mode: download and serve PDFs locally
	if s.localMode {
		if !paper.PDFDownloaded || paper.PDFPath == "" {
			opts := &arxiv.DownloadOptions{DownloadPDF: true, DownloadSource: false}
			if err := s.cache.DownloadPaper(ctx, paper.ID, opts); err != nil {
				http.Error(w, "failed to download PDF: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if p2, err := s.cache.GetPaperFresh(ctx, paperID); err == nil {
				paper = p2
			}
		}

		if paper.PDFPath == "" {
			http.Error(w, "PDF path missing", http.StatusInternalServerError)
			return
		}

		if _, err := os.Stat(paper.PDFPath); err != nil {
			http.Error(w, "PDF file not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/pdf")
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", paperID+".pdf"))
		http.ServeFile(w, r, paper.PDFPath)
		return
	}

	// Gateway mode: redirect to arXiv.org
	arxivURL := fmt.Sprintf("https://arxiv.org/pdf/%s.pdf", paperID)
	http.Redirect(w, r, arxivURL, http.StatusFound)
}

func (s *server) handleSource(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/src/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}

	// Local mode: serve source files from local cache
	if s.localMode {
		ctx := r.Context()
		paperID := parts[0]
		filePath := strings.Join(parts[1:], "/")

		paper, err := s.cache.GetPaper(ctx, paperID)
		if err != nil && len(parts) >= 3 {
			paperID = parts[0] + "/" + parts[1]
			filePath = strings.Join(parts[2:], "/")
			paper, err = s.cache.GetPaper(ctx, paperID)
		}

		if err != nil || !paper.SourceDownloaded || paper.SourcePath == "" {
			http.NotFound(w, r)
			return
		}

		sourceRoot, err := filepath.Abs(paper.SourcePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		fullPath, err := filepath.Abs(filepath.Join(sourceRoot, filePath))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		relPath, err := filepath.Rel(sourceRoot, fullPath)
		if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(os.PathSeparator)) {
			http.NotFound(w, r)
			return
		}

		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}

		http.ServeFile(w, r, fullPath)
		return
	}

	// Gateway mode: redirect to arXiv.org
	arxivURL := fmt.Sprintf("https://arxiv.org/src/%s", strings.Join(parts, "/"))
	http.Redirect(w, r, arxivURL, http.StatusFound)
}

const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64"><rect width="64" height="64" rx="12" fill="#0066cc"/><text x="32" y="41" text-anchor="middle" font-family="Arial,sans-serif" font-size="32" font-weight="700" fill="#fff">a</text></svg>`

func (s *server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write([]byte(faviconSVG))
}

// handleRobots serves a static robots.txt file from the project root
// if it exists, otherwise returns a minimal default that points to
// the sitemap.
func (s *server) handleRobots(w http.ResponseWriter, r *http.Request) {
	if rejectNonGetHead(w, r) {
		return
	}

	// Try to serve robots.txt from the working directory
	if _, err := os.Stat("robots.txt"); err == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		http.ServeFile(w, r, "robots.txt")
		return
	}

	// Fallback minimal robots.txt
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if r.Method != http.MethodHead {
		fmt.Fprintf(w, "User-agent: *\nDisallow:\n\nSitemap: %s/sitemap.xml\n", arxiv.SiteBaseURL())
	}
}

// handleBingSiteAuth serves the Bing Webmaster Tools verification file.
func (s *server) handleBingSiteAuth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(`<?xml version="1.0"?>
<users>
    <user>5D13814D95915D6874F1138BE444F2ED</user>
</users>
`))
}

func (s *server) handleIndexNowKey(w http.ResponseWriter, r *http.Request) {
	if rejectNonGetHead(w, r) {
		return
	}
	if s.indexNowKey == "" {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Method != http.MethodHead {
		fmt.Fprint(w, s.indexNowKey)
	}
}

// parseAuthors splits an author string into individual author names.
// arXiv author format varies but is typically comma-separated or "and"-separated.
func parseAuthors(authors string) []string {
	// Replace " and " with comma for consistent splitting
	authors = strings.ReplaceAll(authors, " and ", ", ")

	var result []string
	for _, a := range strings.Split(authors, ",") {
		a = strings.TrimSpace(a)
		if a != "" {
			result = append(result, a)
		}
	}
	return result
}

type authorList struct {
	Names []string
	Extra int
}

func limitAuthors(authors string, limit int) authorList {
	names := parseAuthors(authors)
	if limit <= 0 || len(names) <= limit {
		return authorList{Names: names}
	}
	return authorList{
		Names: names[:limit],
		Extra: len(names) - limit,
	}
}

func stripArxivVersion(id string) string {
	vIdx := strings.LastIndex(id, "v")
	if vIdx <= 0 || vIdx == len(id)-1 {
		return id
	}
	for _, c := range id[vIdx+1:] {
		if c < '0' || c > '9' {
			return id
		}
	}
	base := id[:vIdx]
	if isArxivID(base) {
		return base
	}
	return id
}

func arxivIDPath(id string) string {
	return strings.ReplaceAll(url.PathEscape(id), "%2F", "/")
}

func paperPath(id string) string {
	return "/abs/" + arxivIDPath(id)
}

func authorPath(author string) string {
	return "/author/" + url.PathEscape(strings.TrimSpace(author))
}

func categoryPath(category string) string {
	return "/category/" + url.PathEscape(strings.TrimSpace(category))
}

func canonicalURL(path string) string {
	return strings.TrimRight(arxiv.SiteBaseURL(), "/") + path
}

func summaryText(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes-1]) + "..."
}

func paperStructuredData(paper *arxiv.Paper) template.JS {
	authors := parseAuthors(paper.Authors)
	authorData := make([]map[string]any, 0, len(authors))
	for _, author := range authors {
		authorData = append(authorData, map[string]any{
			"@type": "Person",
			"name":  author,
			"url":   canonicalURL(authorPath(author)),
		})
	}

	data := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "ScholarlyArticle",
		"headline":    paper.Title,
		"name":        paper.Title,
		"description": summaryText(paper.Abstract, 300),
		"abstract":    paper.Abstract,
		"identifier":  "arXiv:" + paper.ID,
		"url":         canonicalURL(paperPath(paper.ID)),
		"sameAs":      "https://arxiv.org/abs/" + arxivIDPath(paper.ID),
		"author":      authorData,
		"about":       parseCategories(paper.Categories),
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"contentUrl":     "https://arxiv.org/pdf/" + arxivIDPath(paper.ID) + ".pdf",
				"encodingFormat": "application/pdf",
			},
		},
	}
	if paper.License != "" {
		data["license"] = paper.License
	}
	if !paper.Created.IsZero() {
		data["datePublished"] = paper.Created.UTC().Format("2006-01-02")
	}
	if !paper.Updated.IsZero() {
		data["dateModified"] = paper.Updated.UTC().Format("2006-01-02")
	}
	return jsonScript(data)
}

func authorDescription(author string, papers []arxiv.Paper) string {
	topics := topAuthorCategories(papers, 3)
	if len(topics) > 0 {
		return fmt.Sprintf("Explore recent arXiv papers by %s in %s, with publication history, collaborators, and related researchers.", author, strings.Join(topics, ", "))
	}
	return fmt.Sprintf("Explore recent arXiv papers by %s, with publication history, collaborators, and related researchers.", author)
}

func authorStructuredData(author string, papers []arxiv.Paper) template.JS {
	limit := len(papers)
	if limit > 20 {
		limit = 20
	}
	items := make([]map[string]any, 0, limit)
	for i := 0; i < limit; i++ {
		paper := papers[i]
		items = append(items, map[string]any{
			"@type":    "ListItem",
			"position": i + 1,
			"url":      canonicalURL(paperPath(paper.ID)),
			"name":     paper.Title,
		})
	}

	data := map[string]any{
		"@context": "https://schema.org",
		"@type":    "ProfilePage",
		"name":     author + " - arXiv.gg",
		"url":      canonicalURL(authorPath(author)),
		"mainEntity": map[string]any{
			"@type": "Person",
			"name":  author,
		},
		"hasPart": map[string]any{
			"@type":           "ItemList",
			"numberOfItems":   len(papers),
			"itemListElement": items,
		},
	}
	return jsonScript(data)
}

var categoryDisplayNames = map[string]string{
	"astro-ph":           "astrophysics",
	"astro-ph.CO":        "cosmology and nongalactic astrophysics",
	"astro-ph.EP":        "earth and planetary astrophysics",
	"astro-ph.GA":        "galaxies",
	"astro-ph.HE":        "high energy astrophysical phenomena",
	"astro-ph.IM":        "instrumentation and methods for astrophysics",
	"astro-ph.SR":        "solar and stellar astrophysics",
	"cond-mat":           "condensed matter",
	"cond-mat.dis-nn":    "disordered systems and neural networks",
	"cond-mat.mes-hall":  "mesoscale and nanoscale physics",
	"cond-mat.mtrl-sci":  "materials science",
	"cond-mat.other":     "other condensed matter",
	"cond-mat.quant-gas": "quantum gases",
	"cond-mat.soft":      "soft condensed matter",
	"cond-mat.stat-mech": "statistical mechanics",
	"cond-mat.str-el":    "strongly correlated electrons",
	"cond-mat.supr-con":  "superconductivity",
	"cs.AI":              "artificial intelligence",
	"cs.AR":              "hardware architecture",
	"cs.CC":              "computational complexity",
	"cs.CE":              "computational engineering, finance, and science",
	"cs.CG":              "computational geometry",
	"cs.CL":              "computation and language",
	"cs.CR":              "cryptography and security",
	"cs.CV":              "computer vision",
	"cs.CY":              "computers and society",
	"cs.DB":              "databases",
	"cs.DC":              "distributed, parallel, and cluster computing",
	"cs.DL":              "digital libraries",
	"cs.DM":              "discrete mathematics",
	"cs.DS":              "data structures and algorithms",
	"cs.ET":              "emerging technologies",
	"cs.FL":              "formal languages and automata theory",
	"cs.GL":              "general literature",
	"cs.GR":              "graphics",
	"cs.GT":              "computer science and game theory",
	"cs.HC":              "human-computer interaction",
	"cs.IR":              "information retrieval",
	"cs.IT":              "information theory",
	"cs.LG":              "machine learning",
	"cs.LO":              "logic in computer science",
	"cs.MA":              "multiagent systems",
	"cs.MM":              "multimedia",
	"cs.MS":              "mathematical software",
	"cs.NA":              "numerical analysis",
	"cs.NE":              "neural and evolutionary computing",
	"cs.NI":              "networking and internet architecture",
	"cs.OH":              "other computer science",
	"cs.OS":              "operating systems",
	"cs.PF":              "performance",
	"cs.PL":              "programming languages",
	"cs.RO":              "robotics",
	"cs.SC":              "symbolic computation",
	"cs.SD":              "sound",
	"cs.SE":              "software engineering",
	"cs.SI":              "social and information networks",
	"cs.SY":              "systems and control",
	"econ":               "economics",
	"eess":               "electrical engineering and systems science",
	"gr-qc":              "general relativity and quantum cosmology",
	"hep-ex":             "high energy physics experiment",
	"hep-lat":            "high energy physics lattice",
	"hep-ph":             "high energy physics phenomenology",
	"hep-th":             "high energy physics theory",
	"math":               "mathematics",
	"math-ph":            "mathematical physics",
	"nlin":               "nonlinear sciences",
	"nucl-ex":            "nuclear experiment",
	"nucl-th":            "nuclear theory",
	"physics":            "physics",
	"q-bio":              "quantitative biology",
	"q-fin":              "quantitative finance",
	"quant-ph":           "quantum physics",
	"stat":               "statistics",
	"stat.ML":            "machine learning statistics",
}

func categoryDisplayName(category string) string {
	if name, ok := categoryDisplayNames[category]; ok {
		return name
	}
	if idx := strings.LastIndex(category, "."); idx > 0 {
		if name, ok := categoryDisplayNames[category[:idx]]; ok {
			return name
		}
	}
	return category
}

func categoryTitle(category string) string {
	displayName := categoryDisplayName(category)
	if displayName == category {
		return fmt.Sprintf("arXiv %s papers", category)
	}
	return fmt.Sprintf("arXiv %s papers: %s", category, displayName)
}

func categoryDescription(category string, papers []arxiv.Paper) string {
	displayName := categoryDisplayName(category)
	paperCount := len(papers)
	if paperCount <= 0 {
		if displayName == category {
			return fmt.Sprintf("Browse arXiv %s papers on arXiv.gg with authors, abstracts, PDFs, categories, and semantic search.", category)
		}
		return fmt.Sprintf("Browse arXiv %s (%s) papers on arXiv.gg with authors, abstracts, PDFs, categories, and semantic search.", category, displayName)
	}
	if displayName == category {
		return fmt.Sprintf("Browse %d arXiv %s papers on arXiv.gg with authors, abstracts, PDFs, categories, and semantic search.", paperCount, category)
	}
	return fmt.Sprintf("Browse %d arXiv %s (%s) papers on arXiv.gg with authors, abstracts, PDFs, categories, and semantic search.", paperCount, category, displayName)
}

func categoryStructuredData(category string, papers []arxiv.Paper) template.JS {
	limit := len(papers)
	if limit > 20 {
		limit = 20
	}
	items := make([]map[string]any, 0, limit)
	for i := 0; i < limit; i++ {
		paper := papers[i]
		items = append(items, map[string]any{
			"@type":    "ListItem",
			"position": i + 1,
			"url":      canonicalURL(paperPath(paper.ID)),
			"name":     paper.Title,
		})
	}

	data := map[string]any{
		"@context":    "https://schema.org",
		"@type":       "CollectionPage",
		"name":        categoryTitle(category) + " - arXiv.gg",
		"description": categoryDescription(category, papers),
		"url":         canonicalURL(categoryPath(category)),
		"about":       []string{category, categoryDisplayName(category)},
		"mainEntity": map[string]any{
			"@type":           "ItemList",
			"numberOfItems":   len(papers),
			"itemListElement": items,
		},
	}
	return jsonScript(data)
}

func jsonScript(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

func topAuthorCategories(papers []arxiv.Paper, limit int) []string {
	counts := make(map[string]int)
	for _, paper := range papers {
		for _, category := range parseCategories(paper.Categories) {
			counts[category]++
		}
	}

	type categoryCount struct {
		name  string
		count int
	}
	categories := make([]categoryCount, 0, len(counts))
	for name, count := range counts {
		categories = append(categories, categoryCount{name: name, count: count})
	}
	sort.Slice(categories, func(i, j int) bool {
		if categories[i].count == categories[j].count {
			return categories[i].name < categories[j].name
		}
		return categories[i].count > categories[j].count
	})

	if limit > len(categories) {
		limit = len(categories)
	}
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, categories[i].name)
	}
	return out
}

// parseCategories splits a space-separated category string.
func parseCategories(categories string) []string {
	return strings.Fields(categories)
}

// arxivIDToDate parses an arXiv ID and returns a date string like "Feb 2023".
// New format: YYMM.NNNNN (e.g., 2302.13971 -> Feb 2023)
// Old format: category/YYMMNNN (e.g., hep-th/9901001 -> Jan 1999)
func arxivIDToDate(id string) string {
	months := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

	// Check for old format: category/YYMMNNN
	if idx := strings.Index(id, "/"); idx >= 0 {
		numPart := id[idx+1:]
		if len(numPart) >= 4 {
			yy := numPart[0:2]
			mm := numPart[2:4]
			year, month := parseYYMM(yy, mm)
			if month >= 1 && month <= 12 {
				return months[month-1] + " " + fmt.Sprintf("%d", year)
			}
		}
		return ""
	}

	// New format: YYMM.NNNNN or YYMM.NNNNNN
	if idx := strings.Index(id, "."); idx >= 0 {
		yymm := id[:idx]
		if len(yymm) == 4 {
			yy := yymm[0:2]
			mm := yymm[2:4]
			year, month := parseYYMM(yy, mm)
			if month >= 1 && month <= 12 {
				return months[month-1] + " " + fmt.Sprintf("%d", year)
			}
		}
	}

	return ""
}

func parseYYMM(yy, mm string) (year, month int) {
	// Parse year: 91-99 -> 1991-1999, 00-90 -> 2000-2090
	if len(yy) == 2 && yy[0] >= '0' && yy[0] <= '9' && yy[1] >= '0' && yy[1] <= '9' {
		y := int(yy[0]-'0')*10 + int(yy[1]-'0')
		if y >= 91 {
			year = 1900 + y
		} else {
			year = 2000 + y
		}
	}
	// Parse month
	if len(mm) == 2 && mm[0] >= '0' && mm[0] <= '1' && mm[1] >= '0' && mm[1] <= '9' {
		month = int(mm[0]-'0')*10 + int(mm[1]-'0')
	}
	return
}

// isArxivID checks if a string looks like a valid arXiv ID.
// Matches: YYMM.NNNNN, YYMM.NNNNNN, or category/NNNNNNN (e.g., hep-th/9901001)
func isArxivID(s string) bool {
	s = strings.TrimSpace(s)
	// New format: YYMM.NNNNN or YYMM.NNNNNN
	if idx := strings.Index(s, "."); idx == 4 {
		yymm := s[:4]
		rest := s[5:]
		// Check YYMM is numeric
		for _, c := range yymm {
			if c < '0' || c > '9' {
				return false
			}
		}
		// Check rest is numeric and reasonable length (5-6 digits)
		if len(rest) < 4 || len(rest) > 6 {
			return false
		}
		for _, c := range rest {
			if c < '0' || c > '9' {
				return false
			}
		}
		return true
	}
	// Old format: category/NNNNNNN (e.g., hep-th/9901001)
	if idx := strings.Index(s, "/"); idx > 0 {
		rest := s[idx+1:]
		if len(rest) >= 7 {
			for _, c := range rest {
				if c < '0' || c > '9' {
					return false
				}
			}
			return true
		}
	}
	return false
}

// extractArxivID extracts an arXiv ID from various input formats:
// - Plain ID: "2301.00001" -> "2301.00001"
// - URL: "https://arxiv.org/abs/2301.00001" -> "2301.00001"
// - URL with version: "https://arxiv.org/abs/2301.00001v2" -> "2301.00001"
// - PDF URL: "https://arxiv.org/pdf/2301.00001.pdf" -> "2301.00001"
// Returns empty string if no valid ID found.
func extractArxivID(input string) string {
	input = strings.TrimSpace(input)

	// Check if it's already a valid ID
	if isArxivID(input) {
		return input
	}

	// Try to extract from URL
	// Patterns: arxiv.org/abs/ID, arxiv.org/pdf/ID, export.arxiv.org/abs/ID
	for _, pattern := range []string{"/abs/", "/pdf/"} {
		if idx := strings.Index(input, pattern); idx >= 0 {
			id := input[idx+len(pattern):]
			// Remove trailing .pdf if present
			id = strings.TrimSuffix(id, ".pdf")
			// Remove version suffix (v1, v2, etc.)
			if vIdx := strings.LastIndex(id, "v"); vIdx > 0 {
				// Check if everything after 'v' is numeric
				allDigits := true
				for _, c := range id[vIdx+1:] {
					if c < '0' || c > '9' {
						allDigits = false
						break
					}
				}
				if allDigits && len(id[vIdx+1:]) > 0 {
					id = id[:vIdx]
				}
			}
			// Remove any query params or fragments
			if qIdx := strings.IndexAny(id, "?#"); qIdx >= 0 {
				id = id[:qIdx]
			}
			if isArxivID(id) {
				return id
			}
		}
	}

	return ""
}

func (s *server) handleCategory(w http.ResponseWriter, r *http.Request) {
	category := strings.TrimPrefix(r.URL.Path, "/category/")
	if category == "" {
		http.NotFound(w, r)
		return
	}
	canonicalCategory := strings.TrimRight(category, "/")
	if canonicalCategory != category {
		http.Redirect(w, r, categoryPath(canonicalCategory), http.StatusMovedPermanently)
		return
	}

	ctx := r.Context()
	papers, err := s.cache.ListPapers(ctx, category, 0, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Title":          categoryTitle(category),
		"Description":    categoryDescription(category, papers),
		"CanonicalURL":   canonicalURL(categoryPath(category)),
		"StructuredData": categoryStructuredData(category, papers),
		"Category":       category,
		"DisplayName":    categoryDisplayName(category),
		"Papers":         papers,
	}
	s.renderTemplate(w, r, "category", data)
}

func (s *server) handleCategories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	categories, err := s.cache.ListCategories(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Title":        "arXiv categories",
		"Description":  fmt.Sprintf("Browse %d arXiv categories on arXiv.gg, including machine learning, quantum physics, condensed matter, astrophysics, math, and more.", len(categories)),
		"CanonicalURL": canonicalURL("/categories"),
		"Categories":   categories,
	}
	s.renderTemplate(w, r, "categories", data)
}

func (s *server) handleAdminEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}
