package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	arxiv "github.com/lantos1618/arxiv.gg"
)

func TestFeedbackPageStateTargetsIdea(t *testing.T) {
	got := feedbackPageStateURL("posted", "fb_test")
	want := "/feedback?feedback=posted&feedback_post=fb_test#idea-fb_test"
	if got != want {
		t.Fatalf("state URL = %q, want %q", got, want)
	}
}

func TestFeedbackPageExplainsHowIdeasShapeTheProduct(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	req := httptest.NewRequest(http.MethodGet, "https://arxiv.gg/feedback", nil)
	rec := httptest.NewRecorder()
	(&server{cache: cache}).handleFeedback(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, expected := range []string{"Help shape arXiv.gg", "Share real friction", "Add community signal", "We assess the fit", "Selected work ships", "does not guarantee selection or payment"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("feedback page missing %q", expected)
		}
	}
}

func TestFeedbackMutationsRejectCrossOriginRequests(t *testing.T) {
	srv := &server{localMode: true}
	tests := []struct {
		path    string
		handler http.HandlerFunc
	}{
		{path: "/feedback", handler: srv.handleFeedback},
		{path: "/feedback/vote", handler: srv.handleFeedbackVote},
		{path: "/feedback/delete", handler: srv.handleFeedbackDelete},
		{path: "/admin/feedback/status", handler: srv.handleAdminFeedbackStatus},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg"+test.path, strings.NewReader("post_id=fb_test"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", "https://evil.example")
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "session"})
			rec := httptest.NewRecorder()

			test.handler(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
		})
	}
}

func TestFeedbackMutationOriginValidation(t *testing.T) {
	for _, source := range []string{"https://arxiv.gg", "https://arxiv.gg/search?q=test"} {
		req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg/feedback", nil)
		if strings.Contains(source, "/search") {
			req.Header.Set("Referer", source)
		} else {
			req.Header.Set("Origin", source)
		}
		if !feedbackMutationAllowed(req) {
			t.Fatalf("expected source %q to be allowed", source)
		}
	}
	req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg/feedback", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "session"})
	if feedbackMutationAllowed(req) {
		t.Fatal("request without Origin or Referer was allowed")
	}
}

func TestFeedbackVoteRejectsInvalidVoteValue(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	owner, _ := cache.FindOrCreateUser(ctx, "handler-owner@example.edu", "Owner", "", true, "google", now)
	voter, _ := cache.FindOrCreateUser(ctx, "handler-voter@example.edu", "Voter", "", true, "google", now)
	post, err := cache.CreateFeedbackPost(ctx, owner, "Strict vote parsing is important.", false, false)
	if err != nil {
		t.Fatalf("CreateFeedbackPost: %v", err)
	}
	session, err := cache.CreateUserSession(ctx, voter.ID, "127.0.0.1", "test", time.Hour)
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}

	form := url.Values{"post_id": {post.ID}, "vote": {"up"}, "return_to": {"/"}}
	req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg/feedback/vote", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://arxiv.gg")
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rec := httptest.NewRecorder()

	(&server{cache: cache}).handleFeedbackVote(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
	if location := rec.Header().Get("Location"); !strings.Contains(location, "feedback=vote_error") {
		t.Fatalf("redirect = %q, want vote_error", location)
	}
	posts, err := cache.ListFeedbackPosts(ctx, voter.ID, 10)
	if err != nil {
		t.Fatalf("ListFeedbackPosts: %v", err)
	}
	if len(posts) != 1 || posts[0].UserVote != 0 {
		t.Fatalf("invalid value changed vote state: %#v", posts)
	}
}

func TestFeedbackHandlerRejectsOversizedForm(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://arxiv.gg/feedback", io.LimitReader(strings.NewReader(strings.Repeat("x", int(feedbackFormMaxBytes)+1)), feedbackFormMaxBytes+1))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://arxiv.gg")
	rec := httptest.NewRecorder()

	(&server{}).handleFeedback(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusSeeOther)
	}
}

func TestAdminFeedbackErrorMessages(t *testing.T) {
	if got := adminFeedbackErrorMessage("form"); !strings.Contains(got, "No change") {
		t.Fatalf("form error = %q", got)
	}
	if got := adminFeedbackErrorMessage("status"); !strings.Contains(got, "could not be updated") {
		t.Fatalf("status error = %q", got)
	}
	if got := adminFeedbackErrorMessage("unknown"); got != "" {
		t.Fatalf("unknown error = %q", got)
	}
}
