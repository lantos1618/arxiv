package arxiv

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFeedbackBoardPostsAndVotes(t *testing.T) {
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

	anonymousPost, err := cache.CreateFeedbackPost(ctx, userA, "I would pay for reliable full-paper search and alerts.", true, true)
	if err != nil {
		t.Fatalf("CreateFeedbackPost anonymous: %v", err)
	}
	visiblePost, err := cache.CreateFeedbackPost(ctx, userB, "I would pay for team libraries and MCP/API workflows.", false, false)
	if err != nil {
		t.Fatalf("CreateFeedbackPost visible: %v", err)
	}

	if _, err := cache.SetFeedbackVote(ctx, userA.ID, anonymousPost.ID, 1); err == nil {
		t.Fatal("expected self-vote to fail")
	}

	vote, err := cache.SetFeedbackVote(ctx, userB.ID, anonymousPost.ID, 1)
	if err != nil {
		t.Fatalf("SetFeedbackVote up: %v", err)
	}
	if vote != 1 {
		t.Fatalf("vote = %d, want 1", vote)
	}

	posts, err := cache.ListFeedbackPosts(ctx, userB.ID, 10)
	if err != nil {
		t.Fatalf("ListFeedbackPosts: %v", err)
	}
	if len(posts) != 2 {
		t.Fatalf("posts len = %d, want 2", len(posts))
	}
	if posts[0].ID != anonymousPost.ID || posts[0].UpVotes != 1 || posts[0].DownVotes != 0 || posts[0].Score != 1 || posts[0].UserVote != 1 {
		t.Fatalf("top upvoted post mismatch: %#v", posts[0])
	}
	if posts[0].DisplayName != "Anonymous researcher" || !posts[0].OpenToCall {
		t.Fatalf("anonymous/open-to-call fields mismatch: %#v", posts[0])
	}
	if posts[0].IsOwn {
		t.Fatalf("post owned by another user marked own: %#v", posts[0])
	}
	if posts[1].ID != visiblePost.ID || posts[1].DisplayName != "Reader B" || !posts[1].IsOwn {
		t.Fatalf("visible post mismatch: %#v", posts[1])
	}

	ownPosts, err := cache.ListUserFeedbackPosts(ctx, userB.ID, 10)
	if err != nil {
		t.Fatalf("ListUserFeedbackPosts: %v", err)
	}
	if len(ownPosts) != 1 || ownPosts[0].ID != visiblePost.ID || !ownPosts[0].IsOwn {
		t.Fatalf("own posts mismatch: %#v", ownPosts)
	}

	vote, err = cache.SetFeedbackVote(ctx, userB.ID, anonymousPost.ID, -1)
	if err != nil {
		t.Fatalf("SetFeedbackVote down: %v", err)
	}
	if vote != -1 {
		t.Fatalf("vote = %d, want -1", vote)
	}

	posts, err = cache.ListFeedbackPosts(ctx, userB.ID, 10)
	if err != nil {
		t.Fatalf("ListFeedbackPosts after downvote: %v", err)
	}
	var sawDownvote bool
	for _, post := range posts {
		if post.ID == anonymousPost.ID {
			sawDownvote = true
			if post.UpVotes != 0 || post.DownVotes != 1 || post.Score != -1 || post.UserVote != -1 {
				t.Fatalf("downvote state mismatch: %#v", post)
			}
		}
	}
	if !sawDownvote {
		t.Fatal("downvoted post missing from public list")
	}

	vote, err = cache.SetFeedbackVote(ctx, userB.ID, anonymousPost.ID, -1)
	if err != nil {
		t.Fatalf("SetFeedbackVote clear: %v", err)
	}
	if vote != 0 {
		t.Fatalf("vote = %d, want 0 after clearing repeated downvote", vote)
	}

	if err := cache.SetFeedbackPostStatus(ctx, anonymousPost.ID, FeedbackPostStatusHidden, "admin@example.edu"); err != nil {
		t.Fatalf("SetFeedbackPostStatus hidden: %v", err)
	}
	posts, err = cache.ListFeedbackPosts(ctx, userB.ID, 10)
	if err != nil {
		t.Fatalf("ListFeedbackPosts after hide: %v", err)
	}
	for _, post := range posts {
		if post.ID == anonymousPost.ID {
			t.Fatalf("hidden post still visible publicly: %#v", post)
		}
	}
	adminPosts, err := cache.ListAdminFeedbackPosts(ctx, userB.ID, 10)
	if err != nil {
		t.Fatalf("ListAdminFeedbackPosts: %v", err)
	}
	var sawHidden bool
	for _, post := range adminPosts {
		if post.ID == anonymousPost.ID {
			sawHidden = true
			if post.Status != FeedbackPostStatusHidden || post.HiddenAt == nil || post.HiddenBy != "admin@example.edu" {
				t.Fatalf("admin hidden fields mismatch: %#v", post)
			}
		}
	}
	if !sawHidden {
		t.Fatal("admin list did not include hidden post")
	}
	if err := cache.SetFeedbackPostStatus(ctx, anonymousPost.ID, FeedbackPostStatusVisible, "admin@example.edu"); err != nil {
		t.Fatalf("SetFeedbackPostStatus visible: %v", err)
	}

	if err := cache.DeleteFeedbackPost(ctx, userA.ID, visiblePost.ID); err == nil {
		t.Fatal("expected non-owner delete to fail")
	}
	if err := cache.DeleteFeedbackPost(ctx, userB.ID, visiblePost.ID); err != nil {
		t.Fatalf("DeleteFeedbackPost owner: %v", err)
	}
	posts, err = cache.ListFeedbackPosts(ctx, userB.ID, 10)
	if err != nil {
		t.Fatalf("ListFeedbackPosts after delete: %v", err)
	}
	for _, post := range posts {
		if post.ID == visiblePost.ID {
			t.Fatalf("deleted post still visible: %#v", post)
		}
	}
	ownPosts, err = cache.ListUserFeedbackPosts(ctx, userB.ID, 10)
	if err != nil {
		t.Fatalf("ListUserFeedbackPosts after delete: %v", err)
	}
	if len(ownPosts) != 0 {
		t.Fatalf("deleted own post still listed: %#v", ownPosts)
	}
}

func TestFeedbackBodyValidation(t *testing.T) {
	if _, err := normalizeFeedbackBody("no"); err == nil {
		t.Fatal("expected short feedback body to fail")
	}

	long := strings.Repeat("x", FeedbackBodyMaxRunes+20)
	if _, err := normalizeFeedbackBody(long); err == nil {
		t.Fatal("expected oversized feedback body to fail")
	}
}

func TestFeedbackPostAccountRateLimit(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	user, err := cache.FindOrCreateUser(ctx, "reader@example.edu", "Reader", "", true, "google", time.Now().UTC())
	if err != nil {
		t.Fatalf("FindOrCreateUser: %v", err)
	}

	for i := 0; i < feedbackPostLimitPerHour; i++ {
		if _, err := cache.CreateFeedbackPost(ctx, user, "I would pay for useful feature detail number "+string(rune('a'+i)), false, false); err != nil {
			t.Fatalf("CreateFeedbackPost %d: %v", i, err)
		}
	}
	if _, err := cache.CreateFeedbackPost(ctx, user, "This extra answer should be rate limited.", false, false); err == nil {
		t.Fatal("expected per-account feedback post rate limit")
	}
}

func TestFeedbackPostAccountRateLimitConcurrent(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	user, err := cache.FindOrCreateUser(ctx, "concurrent@example.edu", "Concurrent Reader", "", true, "google", time.Now().UTC())
	if err != nil {
		t.Fatalf("FindOrCreateUser: %v", err)
	}

	const attempts = 16
	start := make(chan struct{})
	var successes atomic.Int32
	var waitGroup sync.WaitGroup
	for index := 0; index < attempts; index++ {
		waitGroup.Add(1)
		go func(index int) {
			defer waitGroup.Done()
			<-start
			body := "Concurrent feedback suggestion number " + string(rune('a'+index))
			if _, err := cache.CreateFeedbackPost(ctx, user, body, false, false); err == nil {
				successes.Add(1)
			}
		}(index)
	}
	close(start)
	waitGroup.Wait()

	if got := successes.Load(); got != feedbackPostLimitPerHour {
		t.Fatalf("successful concurrent posts = %d, want %d", got, feedbackPostLimitPerHour)
	}
}

func TestFeedbackVoteToggleConcurrent(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	owner, err := cache.FindOrCreateUser(ctx, "owner@example.edu", "Owner", "", true, "google", now)
	if err != nil {
		t.Fatalf("FindOrCreateUser owner: %v", err)
	}
	voter, err := cache.FindOrCreateUser(ctx, "voter@example.edu", "Voter", "", true, "google", now)
	if err != nil {
		t.Fatalf("FindOrCreateUser voter: %v", err)
	}
	post, err := cache.CreateFeedbackPost(ctx, owner, "Please build atomic voting behavior.", false, false)
	if err != nil {
		t.Fatalf("CreateFeedbackPost: %v", err)
	}

	const toggles = 20
	start := make(chan struct{})
	errors := make(chan error, toggles)
	var waitGroup sync.WaitGroup
	for index := 0; index < toggles; index++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			_, err := cache.SetFeedbackVote(ctx, voter.ID, post.ID, 1)
			errors <- err
		}()
	}
	close(start)
	waitGroup.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent SetFeedbackVote: %v", err)
		}
	}

	var count int64
	if err := cache.db.Model(&FeedbackVote{}).Where("user_id = ? AND post_id = ?", voter.ID, post.ID).Count(&count).Error; err != nil {
		t.Fatalf("count feedback votes: %v", err)
	}
	if count != 0 {
		t.Fatalf("votes after even concurrent toggles = %d, want 0", count)
	}
}

func TestFeedbackDeleteAndVoteLeaveNoOrphan(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	owner, _ := cache.FindOrCreateUser(ctx, "delete-owner@example.edu", "Owner", "", true, "google", now)
	voter, _ := cache.FindOrCreateUser(ctx, "delete-voter@example.edu", "Voter", "", true, "google", now)
	post, err := cache.CreateFeedbackPost(ctx, owner, "Delete and vote must remain consistent.", false, false)
	if err != nil {
		t.Fatalf("CreateFeedbackPost: %v", err)
	}

	start := make(chan struct{})
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	go func() {
		defer waitGroup.Done()
		<-start
		_ = cache.DeleteFeedbackPost(ctx, owner.ID, post.ID)
	}()
	go func() {
		defer waitGroup.Done()
		<-start
		_, _ = cache.SetFeedbackVote(ctx, voter.ID, post.ID, 1)
	}()
	close(start)
	waitGroup.Wait()

	var count int64
	if err := cache.db.Model(&FeedbackVote{}).Where("post_id = ?", post.ID).Count(&count).Error; err != nil {
		t.Fatalf("count feedback votes: %v", err)
	}
	if count != 0 {
		t.Fatalf("orphan feedback votes = %d, want 0", count)
	}
}
