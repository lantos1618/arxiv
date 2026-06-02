package arxiv

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AdminStats is a compact, read-only snapshot for the web admin dashboard.
type AdminStats struct {
	GeneratedAt    time.Time
	DBType         DBType
	Cache          CacheStats
	Embeddings     AdminEmbeddingStats
	Users          AdminUserStats
	EmbeddingJobs  map[string]int64
	RecentUsers    []AdminUserRow
	RecentViews    []UserPaperViewRow
	RecentAuditLog []AdminAuditRow
}

const (
	adminQwenModel = qwenEmbeddingModel
	adminQwenDim   = qwenEmbeddingDim
	adminStatsTTL  = time.Minute
)

type AdminEmbeddingStats struct {
	MiniLMAbstracts          int64
	QwenAbstracts            int64
	FullAbstracts            int64
	MissingAbstractText      int64
	FullPaperTexts           int64
	PendingFullPaperFetch    int64
	FullPaperFetchProcessing int64
	FullPaperFetchFailed     int64
	FullPaperChunked         int64
	FullPaperChunks          int64
	FullPaperEmbeddings      int64
	PendingMiniLM            int64
	PendingQwenAbstract      int64
	PendingFullPaperText     int64
	PendingFullPaper         int64
}

type AdminUserStats struct {
	TotalUsers     int64
	New24h         int64
	New7d          int64
	New30d         int64
	Active24h      int64
	Active7d       int64
	Active30d      int64
	FreeUsers      int64
	PaidUsers      int64
	UnsetPlanUsers int64
	PaperViews     int64
	Viewers24h     int64
	Viewers7d      int64
}

type AdminUserRow struct {
	ID          string
	Email       string
	Name        string
	Plan        string
	Provider    string
	Verified    bool
	CreatedAt   time.Time
	LastLoginAt *time.Time
	LastSeenAt  *time.Time
}

type AdminAuditRow struct {
	AdminEmail string
	Action     string
	TargetType string
	TargetID   string
	Details    string
	CreatedAt  time.Time
}

// AdminStats returns real dashboard numbers from the database. Anything not
// represented here should be shown by the UI as a placeholder, not invented.
func (c *Cache) AdminStats(ctx context.Context) (*AdminStats, error) {
	c.adminStatsMu.RLock()
	if c.cachedAdminStats != nil && time.Since(c.adminStatsUpdated) < adminStatsTTL {
		stats := cloneAdminStats(c.cachedAdminStats)
		c.adminStatsMu.RUnlock()
		return stats, nil
	}
	var stale *AdminStats
	if c.cachedAdminStats != nil {
		stale = cloneAdminStats(c.cachedAdminStats)
	}
	c.adminStatsMu.RUnlock()

	if stale != nil {
		if c.adminStatsRefreshMu.TryLock() {
			go func() {
				defer c.adminStatsRefreshMu.Unlock()
				if _, err := c.refreshAdminStatsLocked(context.Background()); err != nil {
					fmt.Printf("admin stats background refresh failed: %v\n", err)
				}
			}()
		}
		return stale, nil
	}

	c.adminStatsRefreshMu.Lock()
	defer c.adminStatsRefreshMu.Unlock()

	c.adminStatsMu.RLock()
	if c.cachedAdminStats != nil && time.Since(c.adminStatsUpdated) < adminStatsTTL {
		stats := cloneAdminStats(c.cachedAdminStats)
		c.adminStatsMu.RUnlock()
		return stats, nil
	}
	c.adminStatsMu.RUnlock()

	return c.refreshAdminStatsLocked(ctx)
}

func (c *Cache) refreshAdminStatsLocked(ctx context.Context) (*AdminStats, error) {
	now := time.Now().UTC()
	cacheStats, err := c.Stats(ctx)
	if err != nil {
		return nil, err
	}

	stats := &AdminStats{
		GeneratedAt:   now,
		DBType:        c.dbType,
		Cache:         *cacheStats,
		EmbeddingJobs: map[string]int64{},
	}

	if err := c.countEmbeddingsForAdmin(ctx, &stats.Embeddings, cacheStats.TotalPapers); err != nil {
		return nil, err
	}
	if err := c.countUsersForAdmin(ctx, &stats.Users, now); err != nil {
		return nil, err
	}
	if jobs, err := c.EmbeddingJobStats(ctx); err != nil {
		return nil, err
	} else {
		stats.EmbeddingJobs = jobs
	}
	recentUsers, err := c.RecentAdminUsers(ctx, 50)
	if err != nil {
		return nil, err
	}
	stats.RecentUsers = recentUsers
	recentViews, err := c.RecentAdminPaperViews(ctx, 50)
	if err != nil {
		return nil, err
	}
	stats.RecentViews = recentViews
	recentAudit, err := c.RecentAdminAudit(ctx, 50)
	if err != nil {
		return nil, err
	}
	stats.RecentAuditLog = recentAudit

	c.adminStatsMu.Lock()
	c.cachedAdminStats = cloneAdminStats(stats)
	c.adminStatsUpdated = time.Now()
	c.adminStatsMu.Unlock()

	return cloneAdminStats(stats), nil
}

// StartAdminStatsRefresh warms the admin dashboard snapshot and refreshes it in
// the background. Admin pages can tolerate a short lag, but they should never
// make every click block on large exact COUNT queries.
func (c *Cache) StartAdminStatsRefresh(ctx context.Context) {
	go func() {
		c.adminStatsRefreshMu.Lock()
		defer c.adminStatsRefreshMu.Unlock()
		if _, err := c.refreshAdminStatsLocked(context.Background()); err != nil {
			fmt.Printf("admin stats warm refresh failed: %v\n", err)
		}
	}()

	go func() {
		ticker := time.NewTicker(adminStatsTTL)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !c.adminStatsRefreshMu.TryLock() {
					continue
				}
				if _, err := c.refreshAdminStatsLocked(context.Background()); err != nil {
					fmt.Printf("admin stats background refresh failed: %v\n", err)
				}
				c.adminStatsRefreshMu.Unlock()
			}
		}
	}()
}

func cloneAdminStats(stats *AdminStats) *AdminStats {
	if stats == nil {
		return nil
	}
	clone := *stats
	if stats.EmbeddingJobs != nil {
		clone.EmbeddingJobs = make(map[string]int64, len(stats.EmbeddingJobs))
		for status, count := range stats.EmbeddingJobs {
			clone.EmbeddingJobs[status] = count
		}
	}
	clone.RecentUsers = append([]AdminUserRow(nil), stats.RecentUsers...)
	clone.RecentViews = append([]UserPaperViewRow(nil), stats.RecentViews...)
	clone.RecentAuditLog = append([]AdminAuditRow(nil), stats.RecentAuditLog...)
	return &clone
}

func (c *Cache) countEmbeddingsForAdmin(ctx context.Context, out *AdminEmbeddingStats, totalPapers int64) error {
	if err := c.db.WithContext(ctx).Model(&Paper{}).
		Where("title IS NULL OR title = '' OR abstract IS NULL OR abstract = ''").
		Count(&out.MissingAbstractText).Error; err != nil {
		return err
	}
	out.FullAbstracts = maxInt64(totalPapers-out.MissingAbstractText, 0)

	if err := c.db.WithContext(ctx).Model(&Embedding{}).Count(&out.MiniLMAbstracts).Error; err != nil {
		return err
	}
	if err := c.db.WithContext(ctx).Model(&EmbeddingV2{}).
		Where("scope = ? AND model = ? AND dim = ?", "abstract", adminQwenModel, adminQwenDim).
		Count(&out.QwenAbstracts).Error; err != nil {
		return err
	}
	if err := c.db.WithContext(ctx).Model(&Paper{}).
		Where("pdf_text IS NOT NULL AND length(pdf_text) > 0").
		Count(&out.FullPaperTexts).Error; err != nil {
		return err
	}
	out.PendingFullPaperFetch = maxInt64(totalPapers-out.FullPaperTexts, 0)
	if err := c.countFullPaperFetchStatus(ctx, &out.FullPaperFetchProcessing, &out.FullPaperFetchFailed); err != nil {
		return err
	}
	if err := c.db.WithContext(ctx).Model(&PaperChunk{}).
		Where("scope = ?", "pdf_text").
		Distinct("paper_id").
		Count(&out.FullPaperChunked).Error; err != nil {
		return err
	}
	if err := c.db.WithContext(ctx).Model(&PaperChunk{}).
		Where("scope = ?", "pdf_text").
		Count(&out.FullPaperChunks).Error; err != nil {
		return err
	}
	if err := c.countFullPaperEmbeddings(ctx, &out.FullPaperEmbeddings); err != nil {
		return err
	}
	if err := c.countPendingFullPaperEmbeddings(ctx, &out.PendingFullPaper); err != nil {
		return err
	}
	out.PendingMiniLM = maxInt64(out.FullAbstracts-out.MiniLMAbstracts, 0)
	out.PendingQwenAbstract = maxInt64(out.FullAbstracts-out.QwenAbstracts, 0)
	out.PendingFullPaperText = maxInt64(out.FullPaperTexts-out.FullPaperChunked, 0)
	return nil
}

func (c *Cache) countFullPaperFetchStatus(ctx context.Context, processing, failed *int64) error {
	if c.dbType != DBTypePostgres {
		*processing = 0
		*failed = 0
		return nil
	}
	sqlDB, err := c.db.DB()
	if err != nil {
		return err
	}
	var tableName *string
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_regclass('public.full_paper_fetch_status')`).Scan(&tableName); err != nil {
		return err
	}
	if tableName == nil {
		*processing = 0
		*failed = 0
		return nil
	}
	row := sqlDB.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'processing'),
			count(*) FILTER (WHERE status = 'failed')
		FROM full_paper_fetch_status
	`)
	return row.Scan(processing, failed)
}

func (c *Cache) countFullPaperEmbeddings(ctx context.Context, total *int64) error {
	if c.dbType != DBTypePostgres {
		*total = 0
		return nil
	}
	sqlDB, err := c.db.DB()
	if err != nil {
		return err
	}
	row := sqlDB.QueryRowContext(ctx, `
		SELECT count(*)
		FROM chunk_embeddings_v2 e
		JOIN paper_chunks c ON c.id = e.chunk_id
		WHERE c.scope = 'pdf_text'
		  AND e.model = $1
		  AND e.dim = $2
		  AND e.vector IS NOT NULL
	`, adminQwenModel, adminQwenDim)
	return row.Scan(total)
}

func (c *Cache) countPendingFullPaperEmbeddings(ctx context.Context, pending *int64) error {
	if c.dbType != DBTypePostgres {
		*pending = 0
		return nil
	}
	sqlDB, err := c.db.DB()
	if err != nil {
		return err
	}
	row := sqlDB.QueryRowContext(ctx, `
		SELECT count(*)
		FROM paper_chunks c
		LEFT JOIN chunk_embeddings_v2 e
		  ON e.chunk_id = c.id
		 AND e.model = $1
		 AND e.dim = $2
		WHERE c.scope = 'pdf_text'
		  AND COALESCE(c.text, '') <> ''
		  AND (
		      e.chunk_id IS NULL
		      OR e.vector IS NULL
		      OR e.source_hash IS DISTINCT FROM c.text_hash
		  )
	`, adminQwenModel, adminQwenDim)
	return row.Scan(pending)
}

func (c *Cache) countUsersForAdmin(ctx context.Context, out *AdminUserStats, now time.Time) error {
	if err := c.db.WithContext(ctx).Model(&User{}).Count(&out.TotalUsers).Error; err != nil {
		return err
	}
	for _, window := range []struct {
		since time.Time
		count *int64
	}{
		{now.Add(-24 * time.Hour), &out.New24h},
		{now.Add(-7 * 24 * time.Hour), &out.New7d},
		{now.Add(-30 * 24 * time.Hour), &out.New30d},
	} {
		if err := c.db.WithContext(ctx).Model(&User{}).Where("created_at >= ?", window.since).Count(window.count).Error; err != nil {
			return err
		}
	}
	for _, window := range []struct {
		since time.Time
		count *int64
	}{
		{now.Add(-24 * time.Hour), &out.Active24h},
		{now.Add(-7 * 24 * time.Hour), &out.Active7d},
		{now.Add(-30 * 24 * time.Hour), &out.Active30d},
	} {
		if err := c.db.WithContext(ctx).Model(&UserSession{}).
			Where("last_seen_at >= ? AND expires_at > ?", window.since, now).
			Distinct("user_id").
			Count(window.count).Error; err != nil {
			return err
		}
	}
	if err := c.db.WithContext(ctx).Model(&User{}).Where("plan = ?", "free").Count(&out.FreeUsers).Error; err != nil {
		return err
	}
	if err := c.db.WithContext(ctx).Model(&User{}).Where("plan = ?", "paid").Count(&out.PaidUsers).Error; err != nil {
		return err
	}
	if err := c.db.WithContext(ctx).Model(&User{}).Where("plan = '' OR plan IS NULL").Count(&out.UnsetPlanUsers).Error; err != nil {
		return err
	}
	if err := c.db.WithContext(ctx).Model(&UserPaperView{}).
		Select("COALESCE(SUM(view_count), 0)").
		Scan(&out.PaperViews).Error; err != nil {
		return err
	}
	for _, window := range []struct {
		since time.Time
		count *int64
	}{
		{now.Add(-24 * time.Hour), &out.Viewers24h},
		{now.Add(-7 * 24 * time.Hour), &out.Viewers7d},
	} {
		if err := c.db.WithContext(ctx).Model(&UserPaperView{}).
			Where("last_viewed_at >= ?", window.since).
			Distinct("user_id").
			Count(window.count).Error; err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) RecentAdminUsers(ctx context.Context, limit int) ([]AdminUserRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var users []User
	if err := c.db.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&users).Error; err != nil {
		return nil, err
	}

	rows := make([]AdminUserRow, 0, len(users))
	for _, user := range users {
		var lastSeen *time.Time
		var session UserSession
		err := c.db.WithContext(ctx).
			Where("user_id = ?", user.ID).
			Order("last_seen_at DESC").
			Limit(1).
			First(&session).Error
		if err == nil {
			lastSeen = &session.LastSeenAt
		}
		rows = append(rows, AdminUserRow{
			ID:          user.ID,
			Email:       user.Email,
			Name:        user.Name,
			Plan:        normalizedPlan(user.Plan),
			Provider:    user.AuthProvider,
			Verified:    user.EmailVerified,
			CreatedAt:   user.CreatedAt,
			LastLoginAt: user.LastLoginAt,
			LastSeenAt:  lastSeen,
		})
	}
	return rows, nil
}

func (c *Cache) RecentAdminAudit(ctx context.Context, limit int) ([]AdminAuditRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var logs []AdminAuditLog
	if err := c.db.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&logs).Error; err != nil {
		return nil, err
	}
	rows := make([]AdminAuditRow, 0, len(logs))
	for _, log := range logs {
		rows = append(rows, AdminAuditRow{
			AdminEmail: log.AdminEmail,
			Action:     log.Action,
			TargetType: log.TargetType,
			TargetID:   log.TargetID,
			Details:    log.Details,
			CreatedAt:  log.CreatedAt,
		})
	}
	return rows, nil
}

func (c *Cache) RecordAdminAudit(ctx context.Context, adminEmail, action, targetType, targetID, details string) error {
	adminEmail = trimForStorage(strings.TrimSpace(adminEmail), 320)
	if adminEmail == "" {
		adminEmail = "admin-token"
	}
	log := &AdminAuditLog{
		ID:         "audit_" + mustRandomToken(18),
		AdminEmail: adminEmail,
		Action:     trimForStorage(action, 128),
		TargetType: trimForStorage(targetType, 128),
		TargetID:   trimForStorage(targetID, 256),
		Details:    trimForStorage(details, 2048),
		CreatedAt:  time.Now().UTC(),
	}
	if log.Action == "" {
		return fmt.Errorf("audit action is required")
	}
	return c.db.WithContext(ctx).Create(log).Error
}

func normalizedPlan(plan string) string {
	plan = strings.ToLower(strings.TrimSpace(plan))
	switch plan {
	case "paid":
		return "paid"
	case "free":
		return "free"
	default:
		return "free"
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
