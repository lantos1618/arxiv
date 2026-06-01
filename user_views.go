package arxiv

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type UserPaperViewRow struct {
	UserID           string
	UserEmail        string
	UserName         string
	PaperID          string
	PaperTitle       string
	Categories       string
	ViewCount        int64
	ReaderCount      int64
	SharedPaperCount int64
	LastViewedAt     time.Time
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

func (c *Cache) PaperAlsoViewed(ctx context.Context, paperID, excludeUserID string, limit int) ([]UserPaperViewRow, error) {
	paperID = strings.TrimSpace(paperID)
	excludeUserID = strings.TrimSpace(excludeUserID)
	if paperID == "" {
		return []UserPaperViewRow{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 12
	}

	query := c.db.WithContext(ctx).
		Table("user_paper_views v").
		Select(`
			v.paper_id,
			p.title AS paper_title,
			p.categories,
			COUNT(DISTINCT v.user_id) AS reader_count,
			COALESCE(SUM(v.view_count), 0) AS view_count
		`).
		Joins("JOIN user_paper_views anchor ON anchor.user_id = v.user_id AND anchor.paper_id = ?", paperID).
		Joins("LEFT JOIN papers p ON p.id = v.paper_id").
		Where("v.paper_id <> ?", paperID).
		Group("v.paper_id, p.title, p.categories").
		Order("reader_count DESC, view_count DESC, MAX(v.last_viewed_at) DESC").
		Limit(limit)
	if excludeUserID != "" {
		query = query.Where("v.user_id <> ?", excludeUserID)
	}

	var rows []UserPaperViewRow
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (c *Cache) ReadersLikeYouViews(ctx context.Context, userID string, limit int) ([]UserPaperViewRow, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return []UserPaperViewRow{}, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 12
	}

	query := `
		WITH mine AS (
			SELECT paper_id
			FROM user_paper_views
			WHERE user_id = ?
		),
		peers AS (
			SELECT v.user_id, count(*) AS shared_papers
			FROM user_paper_views v
			JOIN mine m ON m.paper_id = v.paper_id
			WHERE v.user_id <> ?
			GROUP BY v.user_id
		)
		SELECT v.paper_id,
		       p.title AS paper_title,
		   p.categories,
		       COUNT(DISTINCT v.user_id) AS reader_count,
		       COALESCE(SUM(v.view_count), 0) AS view_count,
		       COALESCE(SUM(peers.shared_papers), 0) AS shared_paper_count
		FROM user_paper_views v
		JOIN peers ON peers.user_id = v.user_id
		LEFT JOIN mine already_seen ON already_seen.paper_id = v.paper_id
		LEFT JOIN papers p ON p.id = v.paper_id
		WHERE already_seen.paper_id IS NULL
		GROUP BY v.paper_id, p.title, p.categories
		ORDER BY shared_paper_count DESC, reader_count DESC, view_count DESC, MAX(v.last_viewed_at) DESC
		LIMIT ?
	`

	var rows []UserPaperViewRow
	if err := c.db.WithContext(ctx).Raw(query, userID, userID, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (c *Cache) RecentCommunityPaperViews(ctx context.Context, limit int) ([]UserPaperViewRow, error) {
	if limit <= 0 || limit > 100 {
		limit = 12
	}
	var rows []UserPaperViewRow
	err := c.db.WithContext(ctx).
		Table("user_paper_views v").
		Select(`
			v.paper_id,
			p.title AS paper_title,
			p.categories,
			COUNT(DISTINCT v.user_id) AS reader_count,
			COALESCE(SUM(v.view_count), 0) AS view_count
		`).
		Joins("LEFT JOIN papers p ON p.id = v.paper_id").
		Group("v.paper_id, p.title, p.categories").
		Order("MAX(v.last_viewed_at) DESC").
		Limit(limit).
		Scan(&rows).Error
	return rows, err
}
