package arxiv

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type UserPaperViewRow struct {
	UserID       string
	UserEmail    string
	UserName     string
	PaperID      string
	PaperTitle   string
	Categories   string
	ViewCount    int64
	LastViewedAt time.Time
}

// RecordUserPaperView upserts a signed-in user's compact paper reading history.
func (c *Cache) RecordUserPaperView(ctx context.Context, userID, paperID string) error {
	userID = strings.TrimSpace(userID)
	paperID = strings.TrimSpace(paperID)
	if userID == "" || paperID == "" {
		return nil
	}
	now := time.Now().UTC()
	view := UserPaperView{
		UserID:        userID,
		PaperID:       paperID,
		ViewCount:     1,
		FirstViewedAt: now,
		LastViewedAt:  now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	return c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "paper_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"view_count":     gorm.Expr("view_count + ?", 1),
			"last_viewed_at": now,
			"updated_at":     now,
		}),
	}).Create(&view).Error
}

func (c *Cache) RecentUserPaperViews(ctx context.Context, userID string, limit int) ([]UserPaperViewRow, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return []UserPaperViewRow{}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var rows []UserPaperViewRow
	err := c.db.WithContext(ctx).
		Table("user_paper_views v").
		Select("v.user_id, v.paper_id, p.title AS paper_title, p.categories, v.view_count, v.last_viewed_at").
		Joins("LEFT JOIN papers p ON p.id = v.paper_id").
		Where("v.user_id = ?", userID).
		Order("v.last_viewed_at DESC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}

func (c *Cache) RecentAdminPaperViews(ctx context.Context, limit int) ([]UserPaperViewRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows []UserPaperViewRow
	err := c.db.WithContext(ctx).
		Table("user_paper_views v").
		Select("v.user_id, u.email AS user_email, u.name AS user_name, v.paper_id, p.title AS paper_title, p.categories, v.view_count, v.last_viewed_at").
		Joins("LEFT JOIN users u ON u.id = v.user_id").
		Joins("LEFT JOIN papers p ON p.id = v.paper_id").
		Order("v.last_viewed_at DESC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}
