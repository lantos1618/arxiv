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
	RecentUsers    []AdminUserRow
	RecentViews    []UserPaperViewRow
	RecentAuditLog []AdminAuditRow
	Availability   map[string]MetricAvailability
}

type MetricAvailability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

const (
	adminQwenModel = qwenEmbeddingModel
	adminQwenDim   = qwenEmbeddingDim
	adminStatsTTL  = time.Minute
)

type AdminEmbeddingStats struct {
	MetadataComplete          int64
	QwenAbstractEmbeddings    int64
	PapersWithPDFText         int64
	PapersWithPDFChunks       int64
	PDFChunks                 int64
	PDFChunkEmbeddings        int64
	MissingPDFText            int64
	MissingPDFChunks          int64
	MissingPDFChunkEmbeddings int64

	// Deprecated aliases retained while admin templates migrate to the precise
	// metric names above.
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
	PendingQwenAbstract      int64
	PendingFullPaperText     int64
	PendingFullPaper         int64
}

type AdminUserStats struct {
	TotalUsers int64
	New24h     int64
	New7d      int64
	New30d     int64
	Active24h  int64
	Active7d   int64
	Active30d  int64
	FreeUsers  int64
	// PaidUsers counts only the literal stored plan label. It does not imply a
	// paid lifecycle, subscription, or billing record.
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

// AdminStats returns dashboard numbers sourced from the application database.
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
				refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
				defer cancel()
				if _, err := c.refreshAdminStatsLocked(refreshCtx); err != nil {
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
		GeneratedAt:  now,
		DBType:       c.dbType,
		Cache:        *cacheStats,
		Availability: map[string]MetricAvailability{},
	}

	if err := c.countEmbeddingsForAdmin(ctx, &stats.Embeddings, cacheStats.TotalPapers, stats.Availability); err != nil {
		return nil, err
	}
	if err := c.countUsersForAdmin(ctx, &stats.Users, now, stats.Availability); err != nil {
		return nil, err
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
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := c.refreshAdminStatsLocked(refreshCtx); err != nil {
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
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, err := c.refreshAdminStatsLocked(refreshCtx)
				cancel()
				if err != nil {
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
	if stats.Availability != nil {
		clone.Availability = make(map[string]MetricAvailability, len(stats.Availability))
		for metric, availability := range stats.Availability {
			clone.Availability[metric] = availability
		}
	}
	clone.RecentUsers = append([]AdminUserRow(nil), stats.RecentUsers...)
	clone.RecentViews = append([]UserPaperViewRow(nil), stats.RecentViews...)
	clone.RecentAuditLog = append([]AdminAuditRow(nil), stats.RecentAuditLog...)
	return &clone
}

func (c *Cache) countEmbeddingsForAdmin(ctx context.Context, out *AdminEmbeddingStats, totalPapers int64, availability map[string]MetricAvailability) error {
	if err := c.db.WithContext(ctx).Model(&Paper{}).
		Where("title IS NULL OR title = '' OR abstract IS NULL OR abstract = ''").
		Count(&out.MissingAbstractText).Error; err != nil {
		return err
	}
	out.FullAbstracts = maxInt64(totalPapers-out.MissingAbstractText, 0)
	out.MetadataComplete = out.FullAbstracts

	if c.dbType == DBTypePostgres {
		if err := c.db.WithContext(ctx).Model(&EmbeddingV2{}).
			Where("scope = ? AND model = ? AND dim = ? AND vector IS NOT NULL", "abstract", adminQwenModel, adminQwenDim).
			Count(&out.QwenAbstracts).Error; err != nil {
			return err
		}
	}
	out.QwenAbstractEmbeddings = out.QwenAbstracts
	setMetricAvailability(availability, "qwen_abstract_embeddings", c.dbType == DBTypePostgres, "requires PostgreSQL with pgvector")
	if err := c.db.WithContext(ctx).Model(&Paper{}).
		Where("pdf_text IS NOT NULL AND length(pdf_text) > 0").
		Count(&out.FullPaperTexts).Error; err != nil {
		return err
	}
	out.PendingFullPaperFetch = maxInt64(totalPapers-out.FullPaperTexts, 0)
	out.PapersWithPDFText = out.FullPaperTexts
	out.MissingPDFText = out.PendingFullPaperFetch
	fetchStatusAvailable, err := c.countFullPaperFetchStatus(ctx, &out.FullPaperFetchProcessing, &out.FullPaperFetchFailed)
	if err != nil {
		return err
	}
	setMetricAvailability(availability, "full_paper_fetch_status", fetchStatusAvailable, "status table is not installed")
	out.PendingFullPaperFetch = maxInt64(out.MissingPDFText-out.FullPaperFetchProcessing-out.FullPaperFetchFailed, 0)
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
	out.PendingQwenAbstract = maxInt64(out.FullAbstracts-out.QwenAbstracts, 0)
	out.PendingFullPaperText = maxInt64(out.FullPaperTexts-out.FullPaperChunked, 0)
	out.PapersWithPDFChunks = out.FullPaperChunked
	out.PDFChunks = out.FullPaperChunks
	out.PDFChunkEmbeddings = out.FullPaperEmbeddings
	out.MissingPDFChunks = out.PendingFullPaperText
	out.MissingPDFChunkEmbeddings = out.PendingFullPaper
	setMetricAvailability(availability, "pdf_chunk_embeddings", c.dbType == DBTypePostgres, "requires PostgreSQL with pgvector")
	return nil
}

func setMetricAvailability(availability map[string]MetricAvailability, metric string, available bool, reason string) {
	if available {
		reason = ""
	}
	availability[metric] = MetricAvailability{Available: available, Reason: reason}
}

func (c *Cache) countFullPaperFetchStatus(ctx context.Context, processing, failed *int64) (bool, error) {
	if c.dbType != DBTypePostgres {
		*processing = 0
		*failed = 0
		return false, nil
	}
	sqlDB, err := c.db.DB()
	if err != nil {
		return false, err
	}
	var tableName *string
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_regclass('public.full_paper_fetch_status')`).Scan(&tableName); err != nil {
		return false, err
	}
	if tableName == nil {
		*processing = 0
		*failed = 0
		return false, nil
	}
	row := sqlDB.QueryRowContext(ctx, `
		SELECT
			count(*) FILTER (WHERE status = 'processing'),
			count(*) FILTER (WHERE status = 'failed')
		FROM full_paper_fetch_status s
		JOIN papers p ON p.id = s.paper_id
		WHERE COALESCE(p.pdf_text, '') = ''
	`)
	return true, row.Scan(processing, failed)
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

func (c *Cache) countUsersForAdmin(ctx context.Context, out *AdminUserStats, now time.Time, availability map[string]MetricAvailability) error {
	// A stored plan label is not evidence of a paid lifecycle.
	setMetricAvailability(availability, "paid_user_lifecycle", false, "no billing or subscription source of truth")
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
	var rows []AdminUserRow
	if err := c.db.WithContext(ctx).Raw(`
		SELECT u.id, u.email, u.name, u.plan, u.auth_provider AS provider,
		       u.email_verified AS verified, u.created_at, u.last_login_at,
		       (SELECT MAX(s.last_seen_at) FROM user_sessions s WHERE s.user_id = u.id) AS last_seen_at
		FROM users u
		ORDER BY u.created_at DESC
		LIMIT ?
	`, limit).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for i := range rows {
		rows[i].Plan = strings.TrimSpace(rows[i].Plan)
		if rows[i].Plan == "" {
			rows[i].Plan = "unset"
		}
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

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
