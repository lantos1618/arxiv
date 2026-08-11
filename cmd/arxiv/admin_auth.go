package main

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/lantos1618/arxiv.gg"
)

const adminCookieName = "arxiv_admin_token"

type adminTokenSource int

const (
	adminTokenMissing adminTokenSource = iota
	adminTokenCookie
	adminTokenHeader
	adminTokenBearer
)

func (s *server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := s.currentAdminUser(r); ok {
		return true
	}

	token := os.Getenv("ADMIN_TOKEN")
	hasAdminEmails := hasConfiguredAdminEmails()
	if strings.TrimSpace(token) == "" && !hasAdminEmails {
		writeAdminDenied(w, r, http.StatusNotFound, "admin endpoints are disabled")
		return false
	}

	provided, _ := adminTokenFromRequest(r)
	if strings.TrimSpace(token) != "" && subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1 {
		return true
	}

	if !strings.HasPrefix(r.URL.Path, "/api/") && hasAdminEmails && provided == "" {
		if _, signedIn := s.currentUser(r); signedIn {
			writeAdminDenied(w, r, http.StatusForbidden, "admin access required")
			return false
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return false
	}

	if provided == "" {
		writeAdminDenied(w, r, http.StatusUnauthorized, "admin token required")
		return false
	}

	writeAdminDenied(w, r, http.StatusUnauthorized, "admin token required")
	return false
}

func (s *server) hasAdminAccess(r *http.Request) bool {
	if _, ok := s.currentAdminUser(r); ok {
		return true
	}
	provided, _ := adminTokenFromRequest(r)
	token := os.Getenv("ADMIN_TOKEN")
	if strings.TrimSpace(token) == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (s *server) hasAdminCookieAccess(r *http.Request) bool {
	provided, source := adminTokenFromRequest(r)
	if source != adminTokenCookie {
		return false
	}
	token := os.Getenv("ADMIN_TOKEN")
	if strings.TrimSpace(token) == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (s *server) requireQwenWorker(w http.ResponseWriter, r *http.Request) bool {
	if s.hasQwenWorkerAccess(r) || s.hasAdminAccess(r) {
		return true
	}
	writeAdminDenied(w, r, http.StatusUnauthorized, "qwen worker token required")
	return false
}

func (s *server) hasQwenWorkerAccess(r *http.Request) bool {
	token := strings.TrimSpace(os.Getenv("QWEN_WORKER_TOKEN"))
	if token == "" {
		return false
	}
	provided := strings.TrimSpace(r.Header.Get("X-Qwen-Worker-Token"))
	if provided == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			provided = strings.TrimSpace(auth[len("bearer "):])
		}
	}
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
}

func (s *server) currentAdminUser(r *http.Request) (*arxiv.User, bool) {
	user, ok := s.sessionUser(r)
	if !ok {
		return nil, false
	}
	if !isConfiguredAdminEmail(user.Email) {
		return nil, false
	}
	return user, true
}

func (s *server) userIsAdmin(user *arxiv.User) bool {
	return user != nil && isConfiguredAdminEmail(user.Email)
}

func hasConfiguredAdminEmails() bool {
	return len(configuredAdminEmailSet()) > 0
}

func isConfiguredAdminEmail(email string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return false
	}
	_, ok := configuredAdminEmailSet()[email]
	return ok
}

func configuredAdminEmailSet() map[string]struct{} {
	values := strings.FieldsFunc(os.Getenv("ADMIN_EMAILS"), func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\t' || r == ' '
	})
	emails := make(map[string]struct{}, len(values))
	for _, value := range values {
		email := strings.ToLower(strings.TrimSpace(value))
		if email != "" {
			emails[email] = struct{}{}
		}
	}
	return emails
}

func adminTokenFromRequest(r *http.Request) (string, adminTokenSource) {
	if provided := r.Header.Get("X-Admin-Token"); provided != "" {
		return provided, adminTokenHeader
	}

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[len("bearer "):]), adminTokenBearer
	}

	if cookie, err := r.Cookie(adminCookieName); err == nil {
		return cookie.Value, adminTokenCookie
	}

	return "", adminTokenMissing
}

func writeAdminDenied(w http.ResponseWriter, r *http.Request, status int, message string) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		respondJSON(w, status, APIResponse{
			Success: false,
			Error:   message,
		})
		return
	}
	http.Error(w, message, status)
}
