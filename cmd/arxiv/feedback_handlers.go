package main

import (
	"log"
	"net/http"
	"net/url"
	"strings"

	arxiv "github.com/lantos1618/arxiv.gg"
)

const (
	feedbackCallURL            = "https://cal.com/lyndon-leong-cbsf8h/arxiv.gg"
	feedbackAnchor             = "ideas"
	feedbackFormMaxBytes int64 = 32 << 10
)

func (s *server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleFeedbackPage(w, r)
	case http.MethodPost:
		if !feedbackMutationAllowed(r) {
			http.Error(w, "cross-origin mutation rejected", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, feedbackFormMaxBytes)
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, feedbackPageStateURL("error", ""), http.StatusSeeOther)
			return
		}
		user, ok := s.currentUser(r)
		if !ok {
			http.Redirect(w, r, feedbackLoginURL(), http.StatusSeeOther)
			return
		}
		if s.feedbackLimiter != nil && !s.feedbackLimiter.Allow(r) {
			writeRateLimitExceeded(w, r)
			return
		}
		body := r.FormValue("body")
		anonymous := r.FormValue("anonymous") != ""
		post, err := s.cache.CreateFeedbackPost(r.Context(), user, body, anonymous, false)
		if err != nil {
			log.Printf("create feedback post failed: %v", err)
			http.Redirect(w, r, feedbackPageStateURL(feedbackSubmitErrorState(err), ""), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, feedbackPageStateURL("posted", post.ID), http.StatusSeeOther)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *server) handleFeedbackPage(w http.ResponseWriter, r *http.Request) {
	var currentUser *arxiv.User
	if user, ok := s.currentUser(r); ok {
		currentUser = user
	}
	data := map[string]any{
		"Title":        "Help shape arXiv.gg",
		"Description":  "Share product ideas, vote on community suggestions, and help arXiv.gg decide what to improve next.",
		"CanonicalURL": canonicalURL("/feedback"),
		"CurrentUser":  currentUser,
	}
	s.addFeedbackPageData(r, data, currentUser)
	s.renderTemplate(w, r, "feedback", data)
}

func (s *server) handleFeedbackVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !feedbackMutationAllowed(r) {
		http.Error(w, "cross-origin mutation rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, feedbackFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, feedbackPageStateURL("error", ""), http.StatusSeeOther)
		return
	}
	user, ok := s.currentUser(r)
	if !ok {
		http.Redirect(w, r, feedbackLoginURL(), http.StatusSeeOther)
		return
	}
	if s.feedbackLimiter != nil && !s.feedbackLimiter.Allow(r) {
		writeRateLimitExceeded(w, r)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	var voteValue int
	switch strings.TrimSpace(r.FormValue("vote")) {
	case "1":
		voteValue = 1
	case "-1":
		voteValue = -1
	default:
		http.Redirect(w, r, feedbackPageStateURL("vote_error", ""), http.StatusSeeOther)
		return
	}
	if _, err := s.cache.SetFeedbackVote(r.Context(), user.ID, postID, voteValue); err != nil {
		log.Printf("update feedback vote failed: %v", err)
		http.Redirect(w, r, feedbackPageStateURL("vote_error", ""), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, feedbackPageStateURL("voted", postID), http.StatusSeeOther)
}

func (s *server) handleFeedbackDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !feedbackMutationAllowed(r) {
		http.Error(w, "cross-origin mutation rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, feedbackFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, feedbackPageStateURL("delete_error", ""), http.StatusSeeOther)
		return
	}
	user, ok := s.currentUser(r)
	if !ok {
		http.Redirect(w, r, feedbackLoginURL(), http.StatusSeeOther)
		return
	}
	if s.feedbackLimiter != nil && !s.feedbackLimiter.Allow(r) {
		writeRateLimitExceeded(w, r)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	if err := s.cache.DeleteFeedbackPost(r.Context(), user.ID, postID); err != nil {
		log.Printf("delete feedback post failed: %v", err)
		http.Redirect(w, r, feedbackPageStateURL("delete_error", ""), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, feedbackPageStateURL("deleted", ""), http.StatusSeeOther)
}

func (s *server) addFeedbackPageData(r *http.Request, data map[string]any, currentUser *arxiv.User) {
	if data == nil {
		return
	}
	data["FeedbackLoginURL"] = feedbackLoginURL()
	data["FeedbackCallURL"] = feedbackCallURL

	state := ""
	if r != nil && r.URL != nil {
		state = strings.TrimSpace(r.URL.Query().Get("feedback"))
	}
	switch state {
	case "posted":
		data["FeedbackNotice"] = "Suggestion posted. Thank you for helping shape arXiv.gg."
	case "voted":
		data["FeedbackNotice"] = "Vote updated. Thanks for adding signal."
	case "deleted":
		data["FeedbackNotice"] = "Suggestion deleted."
	case "too_short":
		data["FeedbackError"] = "Please add a few words so we understand the problem."
	case "too_long":
		data["FeedbackError"] = "Please keep suggestions under 2,000 characters."
	case "rate_limited":
		data["FeedbackError"] = "You have sent a few suggestions already. Please try again later."
	case "submit_error":
		data["FeedbackError"] = "We could not save that suggestion. Please try again."
	case "vote_error":
		data["FeedbackError"] = "We could not update that vote."
	case "delete_error":
		data["FeedbackError"] = "We could not delete that suggestion."
	case "error":
		data["FeedbackError"] = "Something went wrong. Please try again."
	}

	if _, exists := data["FeedbackPosts"]; exists {
		return
	}
	if s == nil || s.cache == nil || r == nil {
		data["FeedbackPosts"] = []arxiv.FeedbackPostRow{}
		return
	}
	viewerUserID := ""
	if currentUser != nil {
		viewerUserID = currentUser.ID
	}
	posts, err := s.cache.ListFeedbackPosts(r.Context(), viewerUserID, 100)
	if err != nil {
		log.Printf("feedback page posts failed: %v", err)
		posts = []arxiv.FeedbackPostRow{}
		data["FeedbackLoadError"] = "Community suggestions could not be loaded. You can still submit an idea."
	}
	data["FeedbackPosts"] = posts
}

func feedbackSubmitErrorState(err error) string {
	if err == nil {
		return "submit_error"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "few words") || strings.Contains(msg, "little more"):
		return "too_short"
	case strings.Contains(msg, "too long"):
		return "too_long"
	case strings.Contains(msg, "too many"):
		return "rate_limited"
	default:
		return "submit_error"
	}
}

func feedbackLoginURL() string {
	return "/login?next=" + url.QueryEscape("/feedback#share")
}

func feedbackPageStateURL(state, postID string) string {
	u := &url.URL{Path: "/feedback", Fragment: feedbackAnchor}
	q := u.Query()
	if state != "" {
		q.Set("feedback", state)
	}
	if postID != "" {
		q.Set("feedback_post", postID)
	}
	u.RawQuery = q.Encode()
	if postID != "" {
		u.Fragment = "idea-" + postID
	}
	return u.String()
}

func (s *server) handleAdminFeedback(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.recordAdminView(r, "admin.feedback.view")
	viewerUserID := ""
	if user, ok := s.currentUser(r); ok {
		viewerUserID = user.ID
	}
	posts, err := s.cache.ListAdminFeedbackPosts(r.Context(), viewerUserID, 120)
	if err != nil {
		log.Printf("admin feedback posts failed: %v", err)
		http.Error(w, "admin feedback unavailable", http.StatusInternalServerError)
		return
	}
	s.renderTemplate(w, r, "admin_feedback", map[string]any{
		"Title": "Admin Feedback",
		"Posts": posts,
		"Error": adminFeedbackErrorMessage(r.URL.Query().Get("error")),
	})
}

func adminFeedbackErrorMessage(state string) string {
	switch strings.TrimSpace(state) {
	case "form":
		return "The moderation form could not be read. No change was made."
	case "status":
		return "The feedback status could not be updated. Please try again."
	default:
		return ""
	}
}

func (s *server) handleAdminFeedbackStatus(w http.ResponseWriter, r *http.Request) {
	if !s.localMode && !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !feedbackMutationAllowed(r) {
		http.Error(w, "cross-origin mutation rejected", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, feedbackFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/feedback?error=form", http.StatusSeeOther)
		return
	}
	postID := strings.TrimSpace(r.FormValue("post_id"))
	status := strings.TrimSpace(r.FormValue("status"))
	adminEmail := s.adminActorEmail(r)
	if adminEmail == "" {
		adminEmail = "admin"
	}
	if err := s.cache.SetFeedbackPostStatus(r.Context(), postID, status, adminEmail); err != nil {
		log.Printf("admin feedback status failed: %v", err)
		http.Redirect(w, r, "/admin/feedback?error=status", http.StatusSeeOther)
		return
	}
	_ = s.cache.RecordAdminAudit(r.Context(), adminEmail, "admin.feedback.status", "feedback_post", postID, "status="+status)
	http.Redirect(w, r, "/admin/feedback#post-"+postID, http.StatusSeeOther)
}

func feedbackMutationAllowed(r *http.Request) bool {
	if r == nil {
		return false
	}
	return !requestUsesCookieCredentials(r) || requestHasSameOrigin(r)
}
