package arxiv

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	FeedbackBodyMaxRunes      = 2000
	FeedbackBodyMinRunes      = 3
	feedbackPostLimitPerHour  = 5
	FeedbackPostStatusVisible = "visible"
	FeedbackPostStatusHidden  = "hidden"
)

var feedbackMutationLocks [64]sync.Mutex

// FeedbackPostRow is a feedback-board post enriched with vote state.
type FeedbackPostRow struct {
	ID          string    `gorm:"column:id"`
	Body        string    `gorm:"column:body"`
	DisplayName string    `gorm:"column:display_name"`
	Anonymous   bool      `gorm:"column:anonymous"`
	OpenToCall  bool      `gorm:"column:open_to_call"`
	Status      string    `gorm:"column:status"`
	CreatedAt   time.Time `gorm:"column:created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at"`
	UpVotes     int64     `gorm:"column:up_votes"`
	DownVotes   int64     `gorm:"column:down_votes"`
	Score       int64     `gorm:"column:score"`
	UserVote    int       `gorm:"column:user_vote"`
	IsOwn       bool      `gorm:"column:is_own"`
}

// AdminFeedbackPostRow includes private moderation context for admins.
type AdminFeedbackPostRow struct {
	FeedbackPostRow
	UserID    string     `gorm:"column:user_id"`
	UserEmail string     `gorm:"column:user_email"`
	HiddenAt  *time.Time `gorm:"column:hidden_at"`
	HiddenBy  string     `gorm:"column:hidden_by"`
}

// CreateFeedbackPost creates a community feedback-board answer from a signed-in user.
func (c *Cache) CreateFeedbackPost(ctx context.Context, user *User, body string, anonymous, openToCall bool) (*FeedbackPost, error) {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return nil, fmt.Errorf("sign in required")
	}
	normalized, err := normalizeFeedbackBody(body)
	if err != nil {
		return nil, err
	}
	unlock := lockFeedbackMutation("create:" + user.ID)
	defer unlock()

	now := time.Now().UTC()
	post := &FeedbackPost{
		ID:          "fb_" + mustRandomToken(18),
		UserID:      user.ID,
		Body:        normalized,
		DisplayName: feedbackDisplayName(user, anonymous),
		Anonymous:   anonymous,
		OpenToCall:  openToCall,
		Status:      FeedbackPostStatusVisible,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	err = c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if c.dbType == DBTypePostgres {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtext(?))", "feedback-create:"+user.ID).Error; err != nil {
				return fmt.Errorf("lock feedback account: %w", err)
			}
		}
		var recentCount int64
		if err := tx.Model(&FeedbackPost{}).
			Where("user_id = ? AND created_at > ?", user.ID, now.Add(-time.Hour)).
			Count(&recentCount).Error; err != nil {
			return fmt.Errorf("check feedback rate: %w", err)
		}
		if recentCount >= feedbackPostLimitPerHour {
			return fmt.Errorf("too many answers from this account recently; try again later")
		}
		if err := tx.Create(post).Error; err != nil {
			return fmt.Errorf("create feedback post: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return post, nil
}

// ListFeedbackPosts returns visible feedback posts ordered by net score, then recency.
func (c *Cache) ListFeedbackPosts(ctx context.Context, viewerUserID string, limit int) ([]FeedbackPostRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	viewerUserID = strings.TrimSpace(viewerUserID)
	rows := []FeedbackPostRow{}
	err := c.db.WithContext(ctx).Raw(`
		SELECT
			p.id,
			p.body,
			p.display_name,
			p.anonymous,
			p.open_to_call,
			p.status,
			p.created_at,
			p.updated_at,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL AND COALESCE(NULLIF(v.value, 0), 1) = 1 THEN 1 ELSE 0 END), 0) AS up_votes,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL AND COALESCE(NULLIF(v.value, 0), 1) = -1 THEN 1 ELSE 0 END), 0) AS down_votes,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL THEN COALESCE(NULLIF(v.value, 0), 1) ELSE 0 END), 0) AS score,
			COALESCE(MAX(CASE WHEN v.user_id = ? THEN COALESCE(NULLIF(v.value, 0), 1) ELSE 0 END), 0) AS user_vote,
			CASE WHEN p.user_id = ? THEN 1 ELSE 0 END AS is_own
		FROM feedback_posts p
		LEFT JOIN feedback_votes v ON v.post_id = p.id
		WHERE (p.status = ? OR p.status = '' OR p.status IS NULL) AND p.hidden_at IS NULL
		GROUP BY p.id, p.user_id, p.body, p.display_name, p.anonymous, p.open_to_call, p.status, p.created_at, p.updated_at
		ORDER BY score DESC, up_votes DESC, p.created_at DESC
		LIMIT ?
	`, viewerUserID, viewerUserID, FeedbackPostStatusVisible, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list feedback posts: %w", err)
	}
	return rows, nil
}

// ListUserFeedbackPosts returns the signed-in user's visible feedback posts.
func (c *Cache) ListUserFeedbackPosts(ctx context.Context, userID string, limit int) ([]FeedbackPostRow, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return []FeedbackPostRow{}, nil
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	rows := []FeedbackPostRow{}
	err := c.db.WithContext(ctx).Raw(`
		SELECT
			p.id,
			p.body,
			p.display_name,
			p.anonymous,
			p.open_to_call,
			p.status,
			p.created_at,
			p.updated_at,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL AND COALESCE(NULLIF(v.value, 0), 1) = 1 THEN 1 ELSE 0 END), 0) AS up_votes,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL AND COALESCE(NULLIF(v.value, 0), 1) = -1 THEN 1 ELSE 0 END), 0) AS down_votes,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL THEN COALESCE(NULLIF(v.value, 0), 1) ELSE 0 END), 0) AS score,
			0 AS user_vote,
			1 AS is_own
		FROM feedback_posts p
		LEFT JOIN feedback_votes v ON v.post_id = p.id
		WHERE p.user_id = ? AND (p.status = ? OR p.status = '' OR p.status IS NULL) AND p.hidden_at IS NULL
		GROUP BY p.id, p.body, p.display_name, p.anonymous, p.open_to_call, p.status, p.created_at, p.updated_at
		ORDER BY p.created_at DESC
		LIMIT ?
	`, userID, FeedbackPostStatusVisible, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list user feedback posts: %w", err)
	}
	return rows, nil
}

// ListAdminFeedbackPosts returns public and hidden feedback posts with private author context.
func (c *Cache) ListAdminFeedbackPosts(ctx context.Context, viewerUserID string, limit int) ([]AdminFeedbackPostRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 120
	}
	viewerUserID = strings.TrimSpace(viewerUserID)
	rows := []AdminFeedbackPostRow{}
	err := c.db.WithContext(ctx).Raw(`
		SELECT
			p.id,
			p.user_id,
			COALESCE(u.email, '') AS user_email,
			p.body,
			p.display_name,
			p.anonymous,
			p.open_to_call,
			p.status,
			p.hidden_at,
			p.hidden_by,
			p.created_at,
			p.updated_at,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL AND COALESCE(NULLIF(v.value, 0), 1) = 1 THEN 1 ELSE 0 END), 0) AS up_votes,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL AND COALESCE(NULLIF(v.value, 0), 1) = -1 THEN 1 ELSE 0 END), 0) AS down_votes,
			COALESCE(SUM(CASE WHEN v.user_id IS NOT NULL THEN COALESCE(NULLIF(v.value, 0), 1) ELSE 0 END), 0) AS score,
			COALESCE(MAX(CASE WHEN v.user_id = ? THEN COALESCE(NULLIF(v.value, 0), 1) ELSE 0 END), 0) AS user_vote,
			CASE WHEN p.user_id = ? THEN 1 ELSE 0 END AS is_own
		FROM feedback_posts p
		LEFT JOIN users u ON u.id = p.user_id
		LEFT JOIN feedback_votes v ON v.post_id = p.id
		GROUP BY p.id, p.user_id, u.email, p.body, p.display_name, p.anonymous, p.open_to_call, p.status, p.hidden_at, p.hidden_by, p.created_at, p.updated_at
		ORDER BY p.created_at DESC
		LIMIT ?
	`, viewerUserID, viewerUserID, limit).Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list admin feedback posts: %w", err)
	}
	return rows, nil
}

// DeleteFeedbackPost permanently removes a user's own feedback post and its votes.
func (c *Cache) DeleteFeedbackPost(ctx context.Context, userID, postID string) error {
	userID = strings.TrimSpace(userID)
	postID = strings.TrimSpace(postID)
	if userID == "" {
		return fmt.Errorf("sign in required")
	}
	if postID == "" {
		return fmt.Errorf("feedback post is required")
	}
	unlock := lockFeedbackMutation("post:" + postID)
	defer unlock()

	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var post FeedbackPost
		query := tx.Where("id = ? AND user_id = ?", postID, userID)
		if c.dbType == DBTypePostgres {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		result := query.Limit(1).Find(&post)
		if result.Error != nil {
			return fmt.Errorf("load feedback post: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("feedback post not found")
		}
		if err := tx.Where("post_id = ?", postID).Delete(&FeedbackVote{}).Error; err != nil {
			return fmt.Errorf("delete feedback votes: %w", err)
		}
		if err := tx.Delete(&post).Error; err != nil {
			return fmt.Errorf("delete feedback post: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// SetFeedbackPostStatus changes public visibility for a feedback post.
func (c *Cache) SetFeedbackPostStatus(ctx context.Context, postID, status, adminEmail string) error {
	postID = strings.TrimSpace(postID)
	status = strings.TrimSpace(status)
	adminEmail = trimForStorage(adminEmail, 200)
	if postID == "" {
		return fmt.Errorf("feedback post is required")
	}
	if status != FeedbackPostStatusVisible && status != FeedbackPostStatusHidden {
		return fmt.Errorf("invalid feedback status")
	}
	updates := map[string]any{
		"status":     status,
		"updated_at": time.Now().UTC(),
	}
	if status == FeedbackPostStatusHidden {
		now := time.Now().UTC()
		updates["hidden_at"] = &now
		updates["hidden_by"] = adminEmail
	} else {
		updates["hidden_at"] = nil
		updates["hidden_by"] = ""
	}
	result := c.db.WithContext(ctx).Model(&FeedbackPost{}).Where("id = ?", postID).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("update feedback status: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("feedback post not found")
	}
	return nil
}

// SetFeedbackVote sets the signed-in user's up/down vote for a post.
func (c *Cache) SetFeedbackVote(ctx context.Context, userID, postID string, value int) (int, error) {
	userID = strings.TrimSpace(userID)
	postID = strings.TrimSpace(postID)
	if userID == "" {
		return 0, fmt.Errorf("sign in required")
	}
	if postID == "" {
		return 0, fmt.Errorf("feedback post is required")
	}
	if value != 1 && value != -1 {
		return 0, fmt.Errorf("invalid feedback vote")
	}
	unlock := lockFeedbackMutation("post:" + postID)
	defer unlock()

	resultValue := 0
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var post FeedbackPost
		postQuery := tx.Where("id = ?", postID)
		if c.dbType == DBTypePostgres {
			postQuery = postQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		postResult := postQuery.Limit(1).Find(&post)
		if postResult.Error != nil {
			return fmt.Errorf("load feedback post: %w", postResult.Error)
		}
		if postResult.RowsAffected == 0 {
			return fmt.Errorf("feedback post not found")
		}
		if post.UserID == userID {
			return fmt.Errorf("you cannot vote for your own idea")
		}
		if post.Status == FeedbackPostStatusHidden || post.HiddenAt != nil {
			return fmt.Errorf("feedback post is hidden")
		}

		var existing FeedbackVote
		voteQuery := tx.Where("user_id = ? AND post_id = ?", userID, postID)
		if c.dbType == DBTypePostgres {
			voteQuery = voteQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		existingResult := voteQuery.Limit(1).Find(&existing)
		if existingResult.Error != nil {
			return fmt.Errorf("load feedback vote: %w", existingResult.Error)
		}
		if existingResult.RowsAffected > 0 {
			existingValue := existing.Value
			if existingValue == 0 {
				existingValue = 1
			}
			if existingValue == value {
				if err := tx.Delete(&existing).Error; err != nil {
					return fmt.Errorf("remove feedback vote: %w", err)
				}
				return nil
			}
			if err := tx.Model(&existing).Update("value", value).Error; err != nil {
				return fmt.Errorf("update feedback vote: %w", err)
			}
			resultValue = value
			return nil
		}

		vote := FeedbackVote{UserID: userID, PostID: postID, Value: value, CreatedAt: time.Now().UTC()}
		if err := tx.Create(&vote).Error; err != nil {
			return fmt.Errorf("create feedback vote: %w", err)
		}
		resultValue = value
		return nil
	})
	if err != nil {
		return 0, err
	}
	return resultValue, nil
}

func normalizeFeedbackBody(body string) (string, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	body = strings.TrimSpace(body)
	if utf8.RuneCountInString(body) < FeedbackBodyMinRunes {
		return "", fmt.Errorf("please add a few words so we know what to build")
	}
	if utf8.RuneCountInString(body) > FeedbackBodyMaxRunes {
		return "", fmt.Errorf("feedback is too long; keep it under %d characters", FeedbackBodyMaxRunes)
	}
	return body, nil
}

func lockFeedbackMutation(key string) func() {
	var hash uint32 = 2166136261
	for index := 0; index < len(key); index++ {
		hash ^= uint32(key[index])
		hash *= 16777619
	}
	mutex := &feedbackMutationLocks[hash%uint32(len(feedbackMutationLocks))]
	mutex.Lock()
	return mutex.Unlock
}

func feedbackDisplayName(user *User, anonymous bool) string {
	if anonymous || user == nil {
		return "Anonymous researcher"
	}
	if name := strings.TrimSpace(user.Name); name != "" {
		return trimForStorage(name, 120)
	}
	email := strings.TrimSpace(user.Email)
	if at := strings.Index(email, "@"); at > 0 {
		return trimForStorage(email[:at], 120)
	}
	return "Signed-in reader"
}
