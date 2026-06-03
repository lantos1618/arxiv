package main

import (
	"log"
	"net/http"
)

type adminPlanView struct {
	Plan        string
	Who         string
	Access      string
	Billing     string
	Placeholder bool
}

type adminPlaceholderView struct {
	Area string
	What string
	Why  string
}

func (s *server) handleAdminDashboard(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	if redirectAdminTokenURL(w, r) {
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
		"Title":        "Admin",
		"Stats":        stats,
		"Plans":        simplePlanModel(),
		"Placeholders": adminPlaceholders(),
	})
}

func (s *server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	if redirectAdminTokenURL(w, r) {
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
	if redirectAdminTokenURL(w, r) {
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
	http.Redirect(w, r, r.URL.Path, http.StatusSeeOther)
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
			Access:  "Saved account identity plus Deep Search over full-paper chunks while it is free during testing.",
			Billing: "Real plan value on users.plan. No payment required.",
		},
		{
			Plan:        "paid",
			Who:         "Future paid user",
			Access:      "Reserved for higher limits, batch tools, and full-paper GPU-heavy features.",
			Billing:     "PLACEHOLDER: Stripe/payment provider is not connected yet.",
			Placeholder: true,
		},
	}
}

func adminPlaceholders() []adminPlaceholderView {
	return []adminPlaceholderView{
		{
			Area: "Traffic",
			What: "PLACEHOLDER: Cloudflare, Google Search Console, Bing, and GA numbers are not pulled into this app.",
			Why:  "Use the external dashboards for now; this page only shows database-backed app state.",
		},
		{
			Area: "Payments",
			What: "PLACEHOLDER: no Stripe tables, webhooks, or billing provider are wired.",
			Why:  "The app currently supports simple plan labels only: anon, free, paid.",
		},
		{
			Area: "GPU worker host",
			What: "PLACEHOLDER: remote V100S/Qwen worker health is not attached to this app yet.",
			Why:  "The worker is checked through ops tools today; expose a small signed status endpoint before showing it as an app metric.",
		},
	}
}
