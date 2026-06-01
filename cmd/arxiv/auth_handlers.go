package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lantos1618/arxiv.gg"
	"gorm.io/gorm"
)

const sessionCookieName = "arxiv_session"
const googleOAuthStateCookieName = "arxiv_google_oauth_state"
const googleOAuthNextCookieName = "arxiv_google_oauth_next"

type googleOAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

type googleCredentialsFile struct {
	Web struct {
		ClientID     string   `json:"client_id"`
		ClientSecret string   `json:"client_secret"`
		RedirectURIs []string `json:"redirect_uris"`
	} `json:"web"`
}

type googleTokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

type googleUserInfo struct {
	Email         string `json:"email"`
	Name          string `json:"name"`
	Picture       string `json:"picture"`
	EmailVerified bool   `json:"email_verified"`
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	next := safeNextPath(r.URL.Query().Get("next"))
	if _, ok := s.currentUser(r); ok {
		if next == "/" || next == "/login" {
			next = "/account"
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}

	_, googleEnabled := configuredGoogleOAuth(r)
	data := map[string]any{
		"Title":         "Sign in",
		"Next":          next,
		"GoogleEnabled": googleEnabled,
	}
	if strings.TrimSpace(r.URL.Query().Get("error")) != "" {
		data["Error"] = "Google sign-in could not start. Try again in a moment."
	}
	s.renderTemplate(w, r, "login", data)
}

func (s *server) handleGoogleOAuthStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.loginLimiter != nil && !s.loginLimiter.Allow(r) {
		writeRateLimitExceeded(w, r)
		return
	}
	cfg, ok := configuredGoogleOAuth(r)
	if !ok {
		http.Redirect(w, r, "/login?error=google-not-configured", http.StatusSeeOther)
		return
	}
	state, err := randomCookieToken(32)
	if err != nil {
		http.Error(w, "could not start login", http.StatusInternalServerError)
		return
	}
	next := safeNextPath(r.URL.Query().Get("next"))
	setShortCookie(w, r, googleOAuthStateCookieName, state, 10*time.Minute)
	setShortCookie(w, r, googleOAuthNextCookieName, next, 10*time.Minute)

	values := url.Values{}
	values.Set("client_id", cfg.ClientID)
	values.Set("redirect_uri", cfg.RedirectURL)
	values.Set("response_type", "code")
	values.Set("scope", "openid email profile")
	values.Set("state", state)
	values.Set("prompt", "select_account")
	http.Redirect(w, r, "https://accounts.google.com/o/oauth2/v2/auth?"+values.Encode(), http.StatusFound)
}

func (s *server) handleGoogleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	cfg, ok := configuredGoogleOAuth(r)
	if !ok {
		http.Error(w, "Google sign-in is not configured", http.StatusServiceUnavailable)
		return
	}
	if msg := strings.TrimSpace(r.URL.Query().Get("error")); msg != "" {
		s.renderTemplate(w, r, "login", map[string]any{
			"Title":         "Sign in",
			"GoogleEnabled": true,
			"Error":         "Google sign-in was cancelled or denied.",
		})
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	stateCookie, err := r.Cookie(googleOAuthStateCookieName)
	if err != nil || state == "" || stateCookie.Value != state {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	clearNamedCookie(w, r, googleOAuthStateCookieName)
	next := "/"
	if nextCookie, err := r.Cookie(googleOAuthNextCookieName); err == nil {
		next = safeNextPath(nextCookie.Value)
		clearNamedCookie(w, r, googleOAuthNextCookieName)
	}
	if code == "" {
		http.Error(w, "missing OAuth code", http.StatusBadRequest)
		return
	}

	token, err := exchangeGoogleCode(r.Context(), cfg, code)
	if err != nil {
		log.Printf("google oauth token exchange failed: %v", err)
		http.Error(w, "Google sign-in failed", http.StatusBadGateway)
		return
	}
	profile, err := fetchGoogleUserInfo(r.Context(), token.AccessToken)
	if err != nil {
		log.Printf("google userinfo failed: %v", err)
		http.Error(w, "Google profile lookup failed", http.StatusBadGateway)
		return
	}
	if !profile.EmailVerified {
		http.Error(w, "Google email is not verified", http.StatusForbidden)
		return
	}

	now := time.Now().UTC()
	user, err := s.cache.FindOrCreateUser(r.Context(), profile.Email, profile.Name, profile.Picture, true, "google", now)
	if err != nil {
		log.Printf("google account create failed: %v", err)
		http.Error(w, "could not create account", http.StatusInternalServerError)
		return
	}
	sessionToken, err := s.cache.CreateUserSession(r.Context(), user.ID, requestIP(r), r.UserAgent(), 30*24*time.Hour)
	if err != nil {
		log.Printf("create google session failed: %v", err)
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	setSessionCookie(w, r, sessionToken)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		_ = s.cache.RevokeUserSession(r.Context(), cookie.Value)
	}
	clearSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleAccount(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login?next=/account", http.StatusSeeOther)
		return
	}
	views, err := s.cache.RecentUserPaperViews(r.Context(), user.ID, 25)
	if err != nil {
		log.Printf("account recent views failed: %v", err)
		views = []arxiv.UserPaperViewRow{}
	}
	s.renderTemplate(w, r, "account", map[string]any{
		"Title":       "Account",
		"User":        user,
		"RecentViews": views,
	})
}

func (s *server) currentUser(r *http.Request) (*arxiv.User, bool) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	user, err := s.cache.UserForSessionToken(ctx, cookie.Value)
	if err != nil {
		if err != gorm.ErrRecordNotFound {
			log.Printf("session lookup failed: %v", err)
		}
		return nil, false
	}
	return user, true
}

func (s *server) renderTemplate(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	var currentUser *arxiv.User
	if _, exists := data["CurrentUser"]; !exists {
		if user, ok := s.currentUser(r); ok {
			data["CurrentUser"] = user
			currentUser = user
		}
	} else if user, ok := data["CurrentUser"].(*arxiv.User); ok {
		currentUser = user
	}
	if _, exists := data["CurrentUserIsAdmin"]; !exists {
		data["CurrentUserIsAdmin"] = s.userIsAdmin(currentUser) || s.hasAdminAccess(r)
	}
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render template %s failed: %v", name, err)
	}
}

func configuredGoogleOAuth(r *http.Request) (googleOAuthConfig, bool) {
	cfg := googleOAuthConfig{
		ClientID:     strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID")),
		ClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
		RedirectURL:  strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL")),
	}
	if file := strings.TrimSpace(os.Getenv("GOOGLE_OAUTH_CREDENTIALS_FILE")); file != "" {
		fileCfg, err := readGoogleCredentialsFile(file)
		if err != nil {
			log.Printf("could not read GOOGLE_OAUTH_CREDENTIALS_FILE: %v", err)
		} else {
			if cfg.ClientID == "" {
				cfg.ClientID = fileCfg.ClientID
			}
			if cfg.ClientSecret == "" {
				cfg.ClientSecret = fileCfg.ClientSecret
			}
			if cfg.RedirectURL == "" {
				cfg.RedirectURL = chooseGoogleRedirectURL(r, fileCfg.RedirectURL)
			}
		}
	}
	if cfg.RedirectURL == "" {
		cfg.RedirectURL = defaultGoogleRedirectURL(r)
	}
	return cfg, cfg.ClientID != "" && cfg.ClientSecret != "" && cfg.RedirectURL != ""
}

func readGoogleCredentialsFile(path string) (googleOAuthConfig, error) {
	var raw googleCredentialsFile
	f, err := os.Open(path)
	if err != nil {
		return googleOAuthConfig{}, err
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		return googleOAuthConfig{}, err
	}
	cfg := googleOAuthConfig{
		ClientID:     raw.Web.ClientID,
		ClientSecret: raw.Web.ClientSecret,
	}
	if len(raw.Web.RedirectURIs) > 0 {
		cfg.RedirectURL = raw.Web.RedirectURIs[0]
	}
	return cfg, nil
}

func chooseGoogleRedirectURL(r *http.Request, fallback string) string {
	current := defaultGoogleRedirectURL(r)
	if current != "" {
		return current
	}
	return fallback
}

func defaultGoogleRedirectURL(r *http.Request) string {
	scheme := "http"
	if requestIsHTTPS(r) {
		scheme = "https"
	}
	host := r.Host
	if strings.EqualFold(host, "arxiv.gg") || strings.EqualFold(host, "www.arxiv.gg") {
		return "https://arxiv.gg/auth/google/callback"
	}
	if strings.HasPrefix(host, "localhost:") || strings.HasPrefix(host, "127.0.0.1:") {
		return "http://localhost:8080/auth/google/callback"
	}
	return scheme + "://" + host + "/auth/google/callback"
}

func exchangeGoogleCode(ctx context.Context, cfg googleOAuthConfig, code string) (*googleTokenResponse, error) {
	values := url.Values{}
	values.Set("client_id", cfg.ClientID)
	values.Set("client_secret", cfg.ClientSecret)
	values.Set("code", code)
	values.Set("grant_type", "authorization_code")
	values.Set("redirect_uri", cfg.RedirectURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var token googleTokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || token.Error != "" {
		return nil, fmt.Errorf("token status %d: %s %s", resp.StatusCode, token.Error, token.ErrorDesc)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("empty access token")
	}
	return &token, nil
}

func fetchGoogleUserInfo(ctx context.Context, accessToken string) (*googleUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openidconnect.googleapis.com/v1/userinfo", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var profile googleUserInfo
	if err := json.Unmarshal(body, &profile); err != nil {
		return nil, err
	}
	if _, err := arxiv.NormalizeEmail(profile.Email); err != nil {
		return nil, err
	}
	return &profile, nil
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	clearNamedCookie(w, r, sessionCookieName)
}

func setShortCookie(w http.ResponseWriter, r *http.Request, name, value string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func clearNamedCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   requestIsHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func requestIP(r *http.Request) string {
	host := r.RemoteAddr
	if parsedHost, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsedHost
	}
	if cfIP := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cfIP != "" {
		if net.ParseIP(cfIP) != nil {
			return cfIP
		}
	}
	return host
}

func safeNextPath(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return "/"
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func randomCookieToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
