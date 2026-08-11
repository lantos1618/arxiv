package main

import (
	"log"
	"net/http"
)

type adminPlanView struct {
	Plan    string
	Who     string
	Access  string
	Billing string
}

func (s *server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	s.recordAdminView(r, "admin.dashboard.view")
	stats, err := s.cache.AdminStats(r.Context())
	if err != nil {
		log.Printf("admin stats failed: %v", err)
		http.Error(w, "admin stats unavailable", http.StatusInternalServerError)
		return
	}

	s.renderTemplate(w, r, "admin", map[string]any{
		"Title": "Admin",
		"Stats": stats,
		"Plans": simplePlanModel(),
	})
}

func (s *server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	s.recordAdminView(r, "admin.users.view")
	stats, err := s.cache.AdminStats(r.Context())
	if err != nil {
		log.Printf("admin user stats failed: %v", err)
		http.Error(w, "admin user stats unavailable", http.StatusInternalServerError)
		return
	}

	s.renderTemplate(w, r, "admin_users", map[string]any{
		"Title": "Admin Users",
		"Stats": stats,
		"Plans": simplePlanModel(),
	})
}

func (s *server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	s.recordAdminView(r, "admin.audit.view")
	stats, err := s.cache.AdminStats(r.Context())
	if err != nil {
		log.Printf("admin audit stats failed: %v", err)
		http.Error(w, "admin audit unavailable", http.StatusInternalServerError)
		return
	}

	s.renderTemplate(w, r, "admin_audit", map[string]any{
		"Title": "Admin Audit",
		"Stats": stats,
	})
}

func redirectAdminTokenURL(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("admin_token") == "" {
		return false
	}
	query := r.URL.Query()
	query.Del("admin_token")
	target := r.URL.Path
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
	return true
}

func (s *server) recordAdminView(r *http.Request, action string) {
	adminEmail := s.adminActorEmail(r)
	if adminEmail == "" {
		adminEmail = "local-mode"
	}
	if err := s.cache.RecordAdminAudit(r.Context(), adminEmail, action, "admin_page", r.URL.Path, "read-only admin page view"); err != nil {
		log.Printf("record admin audit failed: %v", err)
	}
}

func (s *server) adminActorEmail(r *http.Request) string {
	if user, ok := s.currentAdminUser(r); ok {
		return user.Email
	}
	if s.hasAdminAccess(r) {
		return "admin-token"
	}
	if s.localMode {
		return "local-mode"
	}
	return ""
}

func simplePlanModel() []adminPlanView {
	return []adminPlanView{
		{
			Plan:    "anon",
			Who:     "Visitor without login",
			Access:  "Browse, Quick Search, Qwen abstract Search, public paper pages.",
			Billing: "No user row and no billing record.",
		},
		{
			Plan:    "free",
			Who:     "Signed in with Google",
			Access:  "Account identity and Deep Search over full-paper chunks.",
			Billing: "Real plan value on users.plan. No payment required.",
		},
	}
}
