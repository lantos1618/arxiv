package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lantos1618/arxiv.gg"
)

func TestPublishPaperQueuesIndexNowURL(t *testing.T) {
	queuePath := filepath.Join(t.TempDir(), "indexnow-urls")
	t.Setenv("INDEXNOW_QUEUE_FILE", queuePath)
	t.Setenv("SITE_URL", "https://example.test")
	server := &server{paperBroadcast: newPaperBroadcaster()}
	server.publishPaper(paperEvent{Paper: arxiv.Paper{ID: "2501.00001"}})
	content, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(content)); got != "https://example.test/abs/2501.00001" {
		t.Fatalf("queued URL = %q", got)
	}
}

func TestAPIKeyRotationRequiresExplicitConfirmation(t *testing.T) {
	for _, values := range []url.Values{
		{},
		{"confirm_rotation": {"yes"}},
		{"confirm_rotation": {"rotate"}},
	} {
		req := httptest.NewRequest(http.MethodPost, "/account/api-key/regenerate", strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		got := apiKeyRotationConfirmed(req)
		want := values.Get("confirm_rotation") == "rotate"
		if got != want {
			t.Fatalf("apiKeyRotationConfirmed(%q) = %v, want %v", values.Encode(), got, want)
		}
	}
}

func TestAccountAPIKeyCannotRotateItself(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	user, err := cache.FindOrCreateUser(context.Background(), "reader@example.com", "Reader", "", true, "google", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, rawKey, err := cache.CreateUserAPIKey(context.Background(), user.ID, "Agent access")
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/account/api-key/regenerate", strings.NewReader("confirm_rotation=rotate"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+rawKey)
	rec := httptest.NewRecorder()
	(&server{cache: cache}).handleRegenerateAPIKey(rec, req)

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next=/account" {
		t.Fatalf("status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
	_, activeKey, err := cache.UserForAPIKey(context.Background(), rawKey)
	if err != nil {
		t.Fatalf("original key was revoked: %v", err)
	}
	if activeKey.ID != key.ID {
		t.Fatalf("active key ID = %q, want %q", activeKey.ID, key.ID)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestRenderTemplateBuffersAndDoesNotMutateInput(t *testing.T) {
	data := map[string]any{"Title": "Test"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	(&server{}).renderTemplate(rec, req, "missing-template", data)

	if rec.Code != http.StatusInternalServerError || rec.Body.String() != "page unavailable\n" {
		t.Fatalf("unexpected render failure: status=%d body=%q", rec.Code, rec.Body.String())
	}
	if len(data) != 1 {
		t.Fatalf("renderTemplate mutated caller data: %#v", data)
	}
}

func TestAnalyticsIdentifiersAreConfiguredTemplateData(t *testing.T) {
	rec := httptest.NewRecorder()
	(&server{googleAnalyticsID: "G-TEST123", bingSiteVerificationID: "bing-test"}).renderTemplate(rec, httptest.NewRequest(http.MethodGet, "/", nil), "head", map[string]any{"Title": "Home"})
	body := rec.Body.String()
	for _, want := range []string{"G-TEST123", "bing-test", "mathjax@3.2.2", "sha384-AHAnt9ZhGeHIrydA1Kp1L7FN"} {
		if !strings.Contains(body, want) {
			t.Fatalf("configured head missing %q", want)
		}
	}
	if strings.Contains(body, "G-SNGD2K7DPC") {
		t.Fatal("head still contains a hard-coded analytics ID")
	}
}

func TestSensitivePagesDoNotLoadThirdPartyAnalytics(t *testing.T) {
	for _, path := range []string{"/login", "/account", "/api/v1/", "/admin", "/auth/google/callback"} {
		rec := httptest.NewRecorder()
		(&server{googleAnalyticsID: "G-TEST123"}).renderTemplate(rec, httptest.NewRequest(http.MethodGet, path, nil), "head", map[string]any{"Title": "Sensitive"})
		if strings.Contains(rec.Body.String(), "G-TEST123") || strings.Contains(rec.Body.String(), "googletagmanager") {
			t.Fatalf("sensitive path %s loaded analytics", path)
		}
	}
}

func TestSignedInPagesDoNotLoadThirdPartyAnalytics(t *testing.T) {
	rec := httptest.NewRecorder()
	user := &arxiv.User{ID: "user_test", Email: "reader@example.com"}
	(&server{googleAnalyticsID: "G-TEST123"}).renderTemplate(rec, httptest.NewRequest(http.MethodGet, "/abs/2501.00001", nil), "head", map[string]any{
		"Title":       "Paper",
		"CurrentUser": user,
	})
	if strings.Contains(rec.Body.String(), "G-TEST123") || strings.Contains(rec.Body.String(), "googletagmanager") {
		t.Fatal("signed-in page loaded third-party analytics")
	}
}

func TestConfiguredPublicIDValidation(t *testing.T) {
	t.Setenv("PUBLIC_ID_TEST", "G-TEST_123")
	if got := configuredPublicID("PUBLIC_ID_TEST"); got != "G-TEST_123" {
		t.Fatalf("configured ID = %q", got)
	}
	t.Setenv("PUBLIC_ID_TEST", `bad\"id`)
	if got := configuredPublicID("PUBLIC_ID_TEST"); got != "" {
		t.Fatalf("unsafe configured ID = %q", got)
	}
}

func TestForwardedClientIPRequiresPrivateOrLoopbackProxy(t *testing.T) {
	publicRequest := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	publicRequest.RemoteAddr = "203.0.113.10:443"
	publicRequest.Header.Set("CF-Connecting-IP", "198.51.100.20")
	if got := clientIPFromRequest(publicRequest, true); got != "203.0.113.10" {
		t.Fatalf("public origin trusted spoofed client IP %q", got)
	}

	proxyRequest := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	proxyRequest.RemoteAddr = "172.19.0.2:443"
	proxyRequest.Header.Set("CF-Connecting-IP", "198.51.100.20")
	if got := clientIPFromRequest(proxyRequest, true); got != "198.51.100.20" {
		t.Fatalf("private proxy client IP = %q", got)
	}
}

func TestSafeNextPathRejectsBackslashes(t *testing.T) {
	for _, next := range []string{`/\evil.example`, `/account\..\evil`} {
		if got := safeNextPath(next); got != "/" {
			t.Fatalf("safeNextPath(%q) = %q, want /", next, got)
		}
	}
}

func TestWriteServerErrorDoesNotExposeInternalDetails(t *testing.T) {
	rec := httptest.NewRecorder()
	writeServerError(rec, http.StatusInternalServerError, "search unavailable", "test search", errors.New("database password=secret"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if body := rec.Body.String(); body != "search unavailable\n" || strings.Contains(body, "secret") {
		t.Fatalf("unexpected response body %q", body)
	}
}

func TestConfiguredGoogleOAuthDoesNotMixCredentialSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "google.json")
	data := `{"web":{"client_id":"file-id","client_secret":"file-secret","redirect_uris":["https://file.example/callback"]}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_OAUTH_CREDENTIALS_FILE", path)
	t.Setenv("GOOGLE_CLIENT_ID", "env-id")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")
	t.Setenv("GOOGLE_REDIRECT_URL", "https://env.example/callback")

	cfg, ok := configuredGoogleOAuth(httptest.NewRequest(http.MethodGet, "https://env.example/login", nil))
	if ok {
		t.Fatal("partial environment credentials must not be completed from the file")
	}
	if cfg.ClientID != "env-id" || cfg.ClientSecret != "" {
		t.Fatalf("unexpected mixed credentials: %#v", cfg)
	}
}

func TestAuthenticationCookiesUseHostPrefixAndSecureScope(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://localhost/login", nil)
	setSessionCookie(rec, req, "session")
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %d", len(cookies))
	}
	cookie := cookies[0]
	if !strings.HasPrefix(cookie.Name, "__Host-") || !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/" || cookie.Domain != "" {
		t.Fatalf("unsafe session cookie: %#v", cookie)
	}
}

func TestAPIKeyAuthorizationPrecedesXAPIKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/", nil)
	req.Header.Set("Authorization", "Bearer bearer-key")
	req.Header.Set("X-API-Key", "header-key")
	if got := apiKeyFromRequest(req); got != "bearer-key" {
		t.Fatalf("apiKeyFromRequest = %q, want bearer-key", got)
	}
}

func TestCacheMiddlewareAddsPublicHeadersOnlyToSuccess(t *testing.T) {
	cache := newCacheMiddleware(time.Minute)
	handler := cache.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/categories" {
			http.Error(w, "failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/categories", nil))
	if got := rec.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("error response Cache-Control = %q, want empty", got)
	}
}

func TestCSRFProtectionRejectsCrossOriginCookieMutation(t *testing.T) {
	handler := csrfProtectionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "session"})
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestCSRFProtectionRejectsCrossSchemeCookieMutation(t *testing.T) {
	handler := csrfProtectionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "session"})
	req.Header.Set("Origin", "http://arxiv.gg")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestCSRFProtectionAllowsSameOriginCookieMutation(t *testing.T) {
	handler := csrfProtectionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "session"})
	req.Header.Set("Origin", "https://arxiv.gg")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestLocalModeBindsLoopbackAndServerHasTimeouts(t *testing.T) {
	if got := serveAddress(8080, true); got != "127.0.0.1:8080" {
		t.Fatalf("local address = %q", got)
	}
	server := newHTTPServer(":8080", http.NotFoundHandler())
	if server.ReadHeaderTimeout != httpReadHeaderTimeout || server.IdleTimeout != httpIdleTimeout {
		t.Fatalf("unexpected HTTP timeouts: read header %v, idle %v", server.ReadHeaderTimeout, server.IdleTimeout)
	}
}

func TestLocalBrowserProtectionRejectsRebindingAndCrossOriginMutation(t *testing.T) {
	handler := localBrowserProtectionMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rebinding := httptest.NewRequest(http.MethodGet, "http://attacker.example/admin", nil)
	rebinding.Host = "attacker.example"
	rebindingRecorder := httptest.NewRecorder()
	handler.ServeHTTP(rebindingRecorder, rebinding)
	if rebindingRecorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding status = %d", rebindingRecorder.Code)
	}

	crossOrigin := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/admin/feedback/status", nil)
	crossOrigin.Header.Set("Origin", "https://attacker.example")
	crossOriginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(crossOriginRecorder, crossOrigin)
	if crossOriginRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d", crossOriginRecorder.Code)
	}

	cliRequest := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/api/v1/qwen/jobs/claim", nil)
	cliRecorder := httptest.NewRecorder()
	handler.ServeHTTP(cliRecorder, cliRequest)
	if cliRecorder.Code != http.StatusNoContent {
		t.Fatalf("CLI status = %d", cliRecorder.Code)
	}
}
