package arxiv

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestNormalizeEmail(t *testing.T) {
	got, err := NormalizeEmail("  Researcher <Ada@Example.EDU> ")
	if err != nil {
		t.Fatalf("NormalizeEmail: %v", err)
	}
	if got != "ada@example.edu" {
		t.Fatalf("NormalizeEmail = %q", got)
	}

	if _, err := NormalizeEmail("not an email"); err == nil {
		t.Fatal("expected invalid email error")
	}
}

func TestLoginCodeCreatesUserAndSession(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	_, code, err := cache.CreateLoginCode(ctx, "Ada@Example.EDU", time.Minute)
	if err != nil {
		t.Fatalf("CreateLoginCode: %v", err)
	}

	if _, err := cache.ConsumeLoginCode(ctx, "ada@example.edu", "000000"); err == nil {
		t.Fatal("expected wrong code to fail")
	}

	user, err := cache.ConsumeLoginCode(ctx, "ada@example.edu", code)
	if err != nil {
		t.Fatalf("ConsumeLoginCode: %v", err)
	}
	if user.Email != "ada@example.edu" || user.Plan != "free" || user.AuthProvider != "email" {
		t.Fatalf("unexpected user: %#v", user)
	}

	if _, err := cache.ConsumeLoginCode(ctx, "ada@example.edu", code); err == nil {
		t.Fatal("expected reused code to fail")
	}

	token, err := cache.CreateUserSession(ctx, user.ID, "127.0.0.1", "test-agent", time.Minute)
	if err != nil {
		t.Fatalf("CreateUserSession: %v", err)
	}
	sessionUser, err := cache.UserForSessionToken(ctx, token)
	if err != nil {
		t.Fatalf("UserForSessionToken: %v", err)
	}
	if sessionUser.ID != user.ID {
		t.Fatalf("session user ID = %q, want %q", sessionUser.ID, user.ID)
	}

	if err := cache.RevokeUserSession(ctx, token); err != nil {
		t.Fatalf("RevokeUserSession: %v", err)
	}
	if _, err := cache.UserForSessionToken(ctx, token); err != gorm.ErrRecordNotFound {
		t.Fatalf("expected revoked session to be missing, got %v", err)
	}
}

func TestRecordUserPaperView(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	user, err := cache.FindOrCreateUser(ctx, "reader@example.edu", "Reader", "", true, "google", now)
	if err != nil {
		t.Fatalf("FindOrCreateUser: %v", err)
	}
	if err := cache.db.WithContext(ctx).Create(&Paper{
		ID:         "2601.00001",
		Created:    now,
		Updated:    now,
		Title:      "A Useful Paper",
		Abstract:   "Something worth reading.",
		Authors:    "Ada Example",
		Categories: "cs.AI",
	}).Error; err != nil {
		t.Fatalf("create paper: %v", err)
	}

	if err := cache.RecordUserPaperView(ctx, user.ID, "2601.00001"); err != nil {
		t.Fatalf("RecordUserPaperView first: %v", err)
	}
	if err := cache.RecordUserPaperView(ctx, user.ID, "2601.00001"); err != nil {
		t.Fatalf("RecordUserPaperView second: %v", err)
	}

	views, err := cache.RecentUserPaperViews(ctx, user.ID, 10)
	if err != nil {
		t.Fatalf("RecentUserPaperViews: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("views len = %d, want 1", len(views))
	}
	if views[0].PaperID != "2601.00001" || views[0].PaperTitle != "A Useful Paper" || views[0].ViewCount != 2 {
		t.Fatalf("unexpected view row: %#v", views[0])
	}
}

func TestReaderViewRecommendations(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	userA, err := cache.FindOrCreateUser(ctx, "reader-a@example.edu", "Reader A", "", true, "google", now)
	if err != nil {
		t.Fatalf("FindOrCreateUser A: %v", err)
	}
	userB, err := cache.FindOrCreateUser(ctx, "reader-b@example.edu", "Reader B", "", true, "google", now)
	if err != nil {
		t.Fatalf("FindOrCreateUser B: %v", err)
	}
	userC, err := cache.FindOrCreateUser(ctx, "reader-c@example.edu", "Reader C", "", true, "google", now)
	if err != nil {
		t.Fatalf("FindOrCreateUser C: %v", err)
	}
	papers := []Paper{
		{ID: "2601.00001", Created: now, Updated: now, Title: "Anchor Paper", Abstract: "A", Authors: "Ada", Categories: "cs.AI"},
		{ID: "2601.00002", Created: now, Updated: now, Title: "Peer Paper B", Abstract: "B", Authors: "Ada", Categories: "cs.LG"},
		{ID: "2601.00003", Created: now, Updated: now, Title: "Peer Paper C", Abstract: "C", Authors: "Ada", Categories: "cs.CL"},
	}
	if err := cache.db.WithContext(ctx).Create(&papers).Error; err != nil {
		t.Fatalf("create papers: %v", err)
	}

	for _, view := range []struct {
		userID  string
		paperID string
	}{
		{userA.ID, "2601.00001"},
		{userB.ID, "2601.00001"},
		{userB.ID, "2601.00002"},
		{userC.ID, "2601.00001"},
		{userC.ID, "2601.00003"},
	} {
		if err := cache.RecordUserPaperView(ctx, view.userID, view.paperID); err != nil {
			t.Fatalf("RecordUserPaperView(%s, %s): %v", view.userID, view.paperID, err)
		}
	}

	alsoViewed, err := cache.PaperAlsoViewed(ctx, "2601.00001", "", 10)
	if err != nil {
		t.Fatalf("PaperAlsoViewed: %v", err)
	}
	if !hasPaperView(alsoViewed, "2601.00002") || !hasPaperView(alsoViewed, "2601.00003") {
		t.Fatalf("also viewed missing peer papers: %#v", alsoViewed)
	}

	likeYou, err := cache.ReadersLikeYouViews(ctx, userA.ID, 10)
	if err != nil {
		t.Fatalf("ReadersLikeYouViews: %v", err)
	}
	if !hasPaperView(likeYou, "2601.00002") || !hasPaperView(likeYou, "2601.00003") {
		t.Fatalf("readers-like-you missing peer papers: %#v", likeYou)
	}
}

func hasPaperView(rows []UserPaperViewRow, paperID string) bool {
	for _, row := range rows {
		if row.PaperID == paperID {
			return true
		}
	}
	return false
}
