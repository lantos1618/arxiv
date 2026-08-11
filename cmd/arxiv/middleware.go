package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := w.Header()
		setHeaderIfEmpty(headers, "X-Content-Type-Options", "nosniff")
		setHeaderIfEmpty(headers, "X-Frame-Options", "DENY")
		setHeaderIfEmpty(headers, "Referrer-Policy", "strict-origin-when-cross-origin")
		setHeaderIfEmpty(headers, "Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		setHeaderIfEmpty(headers, "Cross-Origin-Opener-Policy", "same-origin")
		setHeaderIfEmpty(headers, "Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		setHeaderIfEmpty(headers, "Content-Security-Policy", strings.Join([]string{
			"default-src 'self'",
			"base-uri 'self'",
			"object-src 'none'",
			"frame-ancestors 'none'",
			"form-action 'self'",
			"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://d3js.org https://www.googletagmanager.com",
			"style-src 'self' 'unsafe-inline'",
			"img-src 'self' data: https:",
			"font-src 'self' data: https://cdn.jsdelivr.net",
			"connect-src 'self' https://www.google-analytics.com https://region1.google-analytics.com",
			"upgrade-insecure-requests",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

func setHeaderIfEmpty(headers http.Header, key, value string) {
	if headers.Get(key) == "" {
		headers.Set(key, value)
	}
}

func csrfProtectionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUnsafeMethod(r.Method) && requestUsesCookieCredentials(r) && !requestHasSameOrigin(r) {
			http.Error(w, "cross-origin request denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func localBrowserProtectionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if parsedHost, _, err := net.SplitHostPort(r.Host); err == nil {
			host = parsedHost
		}
		host = strings.Trim(host, "[]")
		if !strings.EqualFold(host, "localhost") && !net.ParseIP(host).IsLoopback() {
			http.Error(w, "loopback host required", http.StatusMisdirectedRequest)
			return
		}
		hasBrowserOrigin := strings.TrimSpace(r.Header.Get("Origin")) != "" ||
			strings.TrimSpace(r.Header.Get("Referer")) != "" ||
			strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")) != ""
		if isUnsafeMethod(r.Method) && hasBrowserOrigin && !requestHasSameOrigin(r) {
			http.Error(w, "cross-origin request denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions && method != http.MethodTrace
}

func requestUsesCookieCredentials(r *http.Request) bool {
	_, sessionErr := r.Cookie(sessionCookieName)
	_, adminErr := r.Cookie(adminCookieName)
	return sessionErr == nil || adminErr == nil
}

func requestHasSameOrigin(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		if referer := strings.TrimSpace(r.Header.Get("Referer")); referer != "" {
			parsed, err := url.Parse(referer)
			if err != nil {
				return false
			}
			origin = parsed.Scheme + "://" + parsed.Host
		}
	}
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	expectedScheme := "http"
	if requestIsHTTPS(r) {
		expectedScheme = "https"
	}
	return strings.EqualFold(parsed.Scheme, expectedScheme) && strings.EqualFold(parsed.Host, r.Host)
}

// cacheEntry holds cached response data
type cacheEntry struct {
	etag      string
	lastMod   time.Time
	data      []byte
	headers   http.Header
	expiresAt time.Time
	size      int64
}

// cacheMiddleware provides HTTP caching with ETag support
type cacheMiddleware struct {
	cache       map[string]*cacheEntry
	mu          sync.RWMutex
	maxAge      time.Duration
	cleanupInt  time.Duration
	maxEntries  int
	maxBytes    int64
	maxItemSize int64
	bytesUsed   int64
}

const (
	responseCacheMaxEntries  = 5000
	responseCacheMaxBytes    = 64 << 20
	responseCacheMaxItemSize = 2 << 20
)

// newCacheMiddleware creates a new cache middleware
func newCacheMiddleware(maxAge time.Duration) *cacheMiddleware {
	cm := &cacheMiddleware{
		cache:       make(map[string]*cacheEntry),
		maxAge:      maxAge,
		cleanupInt:  time.Minute,
		maxEntries:  responseCacheMaxEntries,
		maxBytes:    responseCacheMaxBytes,
		maxItemSize: responseCacheMaxItemSize,
	}
	go cm.cleanup()
	return cm
}

// cleanup removes expired entries periodically
func (cm *cacheMiddleware) cleanup() {
	ticker := time.NewTicker(cm.cleanupInt)
	defer ticker.Stop()
	for range ticker.C {
		cm.mu.Lock()
		now := time.Now()
		for key, entry := range cm.cache {
			if now.After(entry.expiresAt) {
				cm.deleteLocked(key, entry)
			}
		}
		cm.mu.Unlock()
	}
}

func (cm *cacheMiddleware) deleteLocked(key string, entry *cacheEntry) {
	delete(cm.cache, key)
	cm.bytesUsed -= entry.size
	if cm.bytesUsed < 0 {
		cm.bytesUsed = 0
	}
}

func (cm *cacheMiddleware) enforceLimitsLocked(now time.Time) {
	for key, entry := range cm.cache {
		if now.After(entry.expiresAt) {
			cm.deleteLocked(key, entry)
		}
	}
	for len(cm.cache) > cm.maxEntries || cm.bytesUsed > cm.maxBytes {
		var oldestKey string
		var oldestEntry *cacheEntry
		for key, entry := range cm.cache {
			if oldestEntry == nil || entry.lastMod.Before(oldestEntry.lastMod) {
				oldestKey = key
				oldestEntry = entry
			}
		}
		if oldestEntry == nil {
			return
		}
		cm.deleteLocked(oldestKey, oldestEntry)
	}
}

// generateETag creates an ETag from data
func generateETag(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf(`"%x"`, hash[:16])
}

// Handler wraps an http.Handler with caching
func (cm *cacheMiddleware) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only cache GET requests
		if r.Method != http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}

		// SSE endpoints require http.Flusher - skip caching wrapper
		if strings.Contains(r.URL.Path, "/stream") || strings.Contains(r.URL.Path, "/generate") {
			next.ServeHTTP(w, r)
			return
		}

		key, ok := responseCacheKey(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		variesByCookie := responseCacheVariesByCookie(r.URL.Path)

		if shouldBypassResponseCache(r) {
			if variesByCookie {
				appendVary(w.Header(), "Cookie")
				w.Header().Set("Cache-Control", "private, no-store")
			}
			next.ServeHTTP(w, r)
			return
		}

		// Check for a valid cached response
		cm.mu.RLock()
		entry, exists := cm.cache[key]
		cm.mu.RUnlock()

		if exists && time.Now().Before(entry.expiresAt) {
			// Conditional request - return 304 Not Modified
			if r.Header.Get("If-None-Match") == entry.etag {
				w.Header().Set("ETag", entry.etag)
				if variesByCookie {
					appendVary(w.Header(), "Cookie")
				}
				w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cm.maxAge.Seconds())))
				w.WriteHeader(http.StatusNotModified)
				return
			}

			// Serve directly from cache
			copyHeader(w.Header(), entry.headers)
			w.Header().Set("ETag", entry.etag)
			w.Header().Set("Last-Modified", entry.lastMod.Format(http.TimeFormat))
			if variesByCookie {
				appendVary(w.Header(), "Cookie")
			}
			if w.Header().Get("Cache-Control") == "" {
				w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cm.maxAge.Seconds())))
			}
			w.Write(entry.data)
			return
		}
		if exists {
			cm.mu.Lock()
			if current, ok := cm.cache[key]; ok && time.Now().After(current.expiresAt) {
				cm.deleteLocked(key, current)
			}
			cm.mu.Unlock()
		}

		// Cache miss - capture response while writing to client
		cw := &cachingResponseWriter{header: make(http.Header)}
		if variesByCookie {
			appendVary(cw.Header(), "Cookie")
		}

		next.ServeHTTP(cw, r)
		if cw.statusCode == 0 {
			cw.statusCode = http.StatusOK
		}
		if cw.statusCode == http.StatusOK && cw.Header().Get("Cache-Control") == "" {
			cw.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cm.maxAge.Seconds())))
		}
		copyHeader(w.Header(), cw.Header())
		w.WriteHeader(cw.statusCode)
		_, _ = w.Write(cw.data)

		// Store successful responses in cache for future requests
		if cw.statusCode == http.StatusOK && len(cw.data) > 0 {
			if int64(len(cw.data)) > cm.maxItemSize {
				return
			}
			etag := generateETag(cw.data)
			now := time.Now()
			headers := cw.Header().Clone()
			if headers.Get("Content-Type") == "" {
				headers.Set("Content-Type", detectContentType(cw.data))
			}
			entry := &cacheEntry{
				etag:      etag,
				lastMod:   now,
				data:      append([]byte(nil), cw.data...),
				headers:   headers,
				expiresAt: now.Add(cm.maxAge),
				size:      int64(len(cw.data)),
			}

			cm.mu.Lock()
			if existing, ok := cm.cache[key]; ok {
				cm.deleteLocked(key, existing)
			}
			cm.cache[key] = entry
			cm.bytesUsed += entry.size
			cm.enforceLimitsLocked(now)
			cm.mu.Unlock()
		}
	})
}

func responseCacheKey(r *http.Request) (string, bool) {
	if r.URL.RawQuery != "" {
		return "", false
	}
	path := r.URL.Path
	if path == "/" ||
		path == "/categories" ||
		path == "/api/v1/categories" ||
		path == "/robots.txt" ||
		path == "/security.txt" ||
		path == "/.well-known/security.txt" ||
		path == "/sitemap.xml" ||
		path == "/sitemap-static.xml" ||
		path == "/BingSiteAuth.xml" ||
		path == "/favicon.ico" ||
		path == "/favicon.svg" ||
		strings.HasPrefix(path, "/abs/") ||
		strings.HasPrefix(path, "/paper/") ||
		strings.HasPrefix(path, "/author/") ||
		strings.HasPrefix(path, "/category/") ||
		strings.HasPrefix(path, "/sitemaps/") {
		return path, true
	}
	return "", false
}

func responseCacheVariesByCookie(path string) bool {
	return path == "/" ||
		path == "/categories" ||
		strings.HasPrefix(path, "/abs/") ||
		strings.HasPrefix(path, "/paper/") ||
		strings.HasPrefix(path, "/author/") ||
		strings.HasPrefix(path, "/category/")
}

func shouldBypassResponseCache(r *http.Request) bool {
	if strings.HasPrefix(r.URL.Path, "/admin") {
		return true
	}
	if r.URL.Path == "/login" ||
		r.URL.Path == "/auth/google/start" ||
		r.URL.Path == "/auth/google/callback" ||
		r.URL.Path == "/logout" ||
		r.URL.Path == "/account" {
		return true
	}
	if r.URL.Query().Get("admin_token") != "" ||
		r.Header.Get("Authorization") != "" ||
		r.Header.Get("X-Admin-Token") != "" {
		return true
	}
	if _, err := r.Cookie(adminCookieName); err == nil {
		return true
	}
	if _, err := r.Cookie(sessionCookieName); err == nil {
		return true
	}
	return false
}

func appendVary(headers http.Header, value string) {
	for _, part := range strings.Split(headers.Get("Vary"), ",") {
		if strings.EqualFold(strings.TrimSpace(part), value) {
			return
		}
	}
	headers.Add("Vary", value)
}

func copyHeader(dst, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func detectContentType(data []byte) string {
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("<!DOCTYPE html")) ||
		bytes.HasPrefix(trimmed, []byte("<html")) {
		return "text/html; charset=utf-8"
	}
	return http.DetectContentType(data)
}

// cachingResponseWriter captures response data
type cachingResponseWriter struct {
	header     http.Header
	statusCode int
	data       []byte
}

func (cw *cachingResponseWriter) Header() http.Header {
	return cw.header
}

func (cw *cachingResponseWriter) WriteHeader(code int) {
	if cw.statusCode != 0 {
		return
	}
	cw.statusCode = code
}

func (cw *cachingResponseWriter) Write(b []byte) (int, error) {
	if cw.statusCode == 0 {
		cw.statusCode = http.StatusOK
	}
	cw.data = append(cw.data, b...)
	return len(b), nil
}

// rateLimiter provides simple rate limiting
type rateLimiter struct {
	visitors          map[string]*visitor
	mu                sync.RWMutex
	rate              int           // requests per window
	window            time.Duration // time window
	trustProxyHeaders bool
}

type visitor struct {
	count    int
	lastSeen time.Time
}

// newRateLimiter creates a new rate limiter
func newRateLimiter(rate int, window time.Duration, trustProxyHeaders bool) *rateLimiter {
	rl := &rateLimiter{
		visitors:          make(map[string]*visitor),
		rate:              rate,
		window:            window,
		trustProxyHeaders: trustProxyHeaders,
	}
	go rl.cleanup()
	return rl
}

// cleanup removes old visitors periodically
func (rl *rateLimiter) cleanup() {
	ticker := time.NewTicker(time.Minute * 5)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		now := time.Now()
		for ip, v := range rl.visitors {
			if now.Sub(v.lastSeen) > rl.window*2 {
				delete(rl.visitors, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// Handler wraps an http.Handler with rate limiting
func (rl *rateLimiter) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow(r) {
			writeRateLimitExceeded(w, r)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (rl *rateLimiter) Allow(r *http.Request) bool {
	ip := rl.clientIP(r)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	v, exists := rl.visitors[ip]
	now := time.Now()

	if !exists || now.Sub(v.lastSeen) > rl.window {
		rl.visitors[ip] = &visitor{count: 1, lastSeen: now}
		return true
	}

	if v.count >= rl.rate {
		return false
	}

	v.count++
	v.lastSeen = now
	return true
}

func writeRateLimitExceeded(w http.ResponseWriter, r *http.Request) {
	// Render a friendlier rate limit response.
	// For API routes, return structured JSON.
	if strings.HasPrefix(r.URL.Path, "/api/") {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(APIResponse{
			Success: false,
			Error:   "rate limit exceeded - please slow down and retry in a moment",
		})
		return
	}

	// For HTML routes, render an error template if available.
	w.WriteHeader(http.StatusTooManyRequests)
	data := map[string]any{
		"Title":   "Too Many Requests",
		"Message": "You've hit the per-IP rate limit. Please wait a bit and try again.",
	}
	if err := templates.ExecuteTemplate(w, "error", data); err != nil {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
	}
}

func (rl *rateLimiter) clientIP(r *http.Request) string {
	return clientIPFromRequest(r, rl.trustProxyHeaders)
}

func clientIPFromRequest(r *http.Request, trustProxyHeaders bool) string {
	host := r.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsedHost
	}

	remoteIP := net.ParseIP(host)
	if !trustProxyHeaders || remoteIP == nil || (!remoteIP.IsLoopback() && !remoteIP.IsPrivate()) {
		return host
	}

	if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
		if net.ParseIP(cfIP) != nil {
			return cfIP
		}
	}

	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(parts) > 0 {
			ip := strings.TrimSpace(parts[0])
			if net.ParseIP(ip) != nil {
				return ip
			}
		}
	}

	return host
}
