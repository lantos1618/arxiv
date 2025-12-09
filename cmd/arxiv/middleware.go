package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// cacheEntry holds cached response data
type cacheEntry struct {
	etag       string
	lastMod    time.Time
	data       []byte
	expiresAt  time.Time
}

// cacheMiddleware provides HTTP caching with ETag support
type cacheMiddleware struct {
	cache      map[string]*cacheEntry
	mu         sync.RWMutex
	maxAge     time.Duration
	cleanupInt time.Duration
}

// newCacheMiddleware creates a new cache middleware
func newCacheMiddleware(maxAge time.Duration) *cacheMiddleware {
	cm := &cacheMiddleware{
		cache:      make(map[string]*cacheEntry),
		maxAge:     maxAge,
		cleanupInt: time.Hour,
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
				delete(cm.cache, key)
			}
		}
		cm.mu.Unlock()
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

		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}

		// Check if client has cached version
		ifNoneMatch := r.Header.Get("If-None-Match")
		if ifModifiedSince := r.Header.Get("If-Modified-Since"); ifModifiedSince != "" {
			cm.mu.RLock()
			entry, exists := cm.cache[key]
			cm.mu.RUnlock()

			if exists {
				if ifNoneMatch == entry.etag {
					w.Header().Set("ETag", entry.etag)
					w.Header().Set("Last-Modified", entry.lastMod.Format(http.TimeFormat))
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}

		// Create a response writer that captures the response
		cw := &cachingResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
			data:           make([]byte, 0),
		}

		next.ServeHTTP(cw, r)

		// Cache successful GET responses
		if cw.statusCode == http.StatusOK && len(cw.data) > 0 {
			etag := generateETag(cw.data)
			now := time.Now()
			entry := &cacheEntry{
				etag:      etag,
				lastMod:   now,
				data:      cw.data,
				expiresAt: now.Add(cm.maxAge),
			}

			cm.mu.Lock()
			cm.cache[key] = entry
			cm.mu.Unlock()

			// Set cache headers
			w.Header().Set("ETag", etag)
			w.Header().Set("Last-Modified", now.Format(http.TimeFormat))
			w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(cm.maxAge.Seconds())))
		}
	})
}

// cachingResponseWriter captures response data
type cachingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	data       []byte
}

func (cw *cachingResponseWriter) WriteHeader(code int) {
	cw.statusCode = code
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *cachingResponseWriter) Write(b []byte) (int, error) {
	cw.data = append(cw.data, b...)
	return cw.ResponseWriter.Write(b)
}

// rateLimiter provides simple rate limiting
type rateLimiter struct {
	visitors map[string]*visitor
	mu       sync.RWMutex
	rate     int           // requests per window
	window   time.Duration // time window
}

type visitor struct {
	count    int
	lastSeen time.Time
}

// newRateLimiter creates a new rate limiter
func newRateLimiter(rate int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		visitors: make(map[string]*visitor),
		rate:     rate,
		window:   window,
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
		ip := r.RemoteAddr
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			if len(parts) > 0 {
				ip = strings.TrimSpace(parts[0])
			}
		} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			ip = host
		}

		rl.mu.Lock()
		v, exists := rl.visitors[ip]
		now := time.Now()

		if !exists || now.Sub(v.lastSeen) > rl.window {
			v = &visitor{count: 1, lastSeen: now}
			rl.visitors[ip] = v
			rl.mu.Unlock()
			next.ServeHTTP(w, r)
			return
		}

		if v.count >= rl.rate {
			rl.mu.Unlock()

			// Render a friendlier rate limit response.
			// For API routes, return structured JSON.
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(APIResponse{
					Success: false,
					Error:   "rate limit exceeded – please slow down and retry in a moment",
				})
				return
			}

			// For HTML routes, render an error template if available.
			w.WriteHeader(http.StatusTooManyRequests)
			data := map[string]any{
				"Title":   "Too Many Requests",
				"Message": "You’ve hit the per-IP rate limit. Please wait a bit and try again.",
			}
			if err := templates.ExecuteTemplate(w, "error", data); err != nil {
				http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			}
			return
		}

		v.count++
		v.lastSeen = now
		rl.mu.Unlock()

		next.ServeHTTP(w, r)
	})
}

