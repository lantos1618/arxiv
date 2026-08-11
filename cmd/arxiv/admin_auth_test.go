package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	arxiv "github.com/lantos1618/arxiv.gg"
)

func TestRequireAdminAcceptsExplicitCredentialsAndRejectsQueryToken(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	tests := []struct {
		name   string
		path   string
		setup  func(*http.Request)
		wantOK bool
	}{
		{
			name: "existing cookie",
			path: "/admin/embeddings",
			setup: func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: adminCookieName, Value: "test-admin-token"})
			},
			wantOK: true,
		},
		{
			name: "x admin token header",
			path: "/admin/embeddings",
			setup: func(r *http.Request) {
				r.Header.Set("X-Admin-Token", "test-admin-token")
			},
			wantOK: true,
		},
		{
			name: "bearer token",
			path: "/admin/embeddings",
			setup: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer test-admin-token")
			},
			wantOK: true,
		},
		{
			name: "query token",
			path: "/api/v1/embeddings/generate?admin_token=test-admin-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set("X-Forwarded-Proto", "https")
			if tt.setup != nil {
				tt.setup(req)
			}

			rec := httptest.NewRecorder()
			if ok := (&server{}).requireAdmin(rec, req); ok != tt.wantOK {
				t.Fatalf("requireAdmin returned %v, want %v", ok, tt.wantOK)
			}
		})
	}
}

func TestAdminTokenExplicitHeaderPrecedesCookie(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "header-token")
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.AddCookie(&http.Cookie{Name: adminCookieName, Value: "stale-cookie"})
	req.Header.Set("X-Admin-Token", "header-token")

	if ok := (&server{}).requireAdmin(httptest.NewRecorder(), req); !ok {
		t.Fatal("explicit admin header should take precedence over cookie")
	}
}

func TestRequireAdminRedirectsBrowserToLoginWhenAdminEmailsConfigured(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("ADMIN_EMAILS", "owner@example.com")

	req := httptest.NewRequest(http.MethodGet, "/admin/users", nil)
	rec := httptest.NewRecorder()

	if ok := (&server{}).requireAdmin(rec, req); ok {
		t.Fatal("requireAdmin returned true")
	}
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected %d, got %d", http.StatusSeeOther, rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/login?next=%2Fadmin%2Fusers" {
		t.Fatalf("unexpected redirect: %q", got)
	}
}

func TestRequireAdminKeepsAPITokenGateWhenAdminEmailsConfigured(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("ADMIN_EMAILS", "owner@example.com")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/embeddings/generate", nil)
	rec := httptest.NewRecorder()

	if ok := (&server{}).requireAdmin(rec, req); ok {
		t.Fatal("requireAdmin returned true")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected %d, got %d", http.StatusUnauthorized, rec.Code)
	}
}

func TestAccountAPIKeyDoesNotInheritAdminOrWorkerAccess(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("ADMIN_EMAILS", "owner@example.com")
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("QWEN_WORKER_TOKEN", "worker-secret")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	user, err := cache.FindOrCreateUser(context.Background(), "owner@example.com", "Owner", "", true, "google", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, rawKey, err := cache.CreateUserAPIKey(context.Background(), user.ID, "Agent access")
	if err != nil {
		t.Fatal(err)
	}

	server := &server{cache: cache}
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer "+rawKey)
	if server.requireAdmin(httptest.NewRecorder(), req) {
		t.Fatal("account API key inherited admin access")
	}
	if server.requireQwenWorker(httptest.NewRecorder(), req) {
		t.Fatal("account API key inherited worker access")
	}
}
