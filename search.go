package arxiv

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Search searches papers by title/abstract text using full-text search.
// Uses FTS5 for SQLite or tsvector for PostgreSQL.
func (c *Cache) Search(ctx context.Context, query, category string, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 20
	}

	if c.dbType == DBTypePostgres {
		return c.searchPostgres(ctx, query, category, limit)
	}
	return c.searchSQLite(ctx, query, category, limit)
}

// QuickSearch does a fast multi-field search for dropdown/autocomplete.
// Searches ID, title, authors, and categories with LIKE matching.
func (c *Cache) QuickSearch(ctx context.Context, query string, limit int) ([]Paper, int, error) {
	if limit <= 0 {
		limit = 10
	}

	query = strings.TrimSpace(query)
	if query == "" {
		return nil, 0, nil
	}

	if c.dbType == DBTypePostgres {
		return c.quickSearchPostgres(ctx, query, limit)
	}

	likePattern := "%" + query + "%"

	// Use raw SQL for proper LIKE and ordering
	var sql string
	var countSQL string
	var args []any

	if c.dbType == DBTypePostgres {
		countSQL = `SELECT COUNT(*) FROM papers WHERE id ILIKE $1 OR title ILIKE $1 OR authors ILIKE $1 OR categories ILIKE $1`
		sql = `SELECT * FROM papers
			WHERE id ILIKE ? OR title ILIKE ? OR authors ILIKE ? OR categories ILIKE ?
			ORDER BY CASE WHEN id ILIKE ? THEN 0 WHEN title ILIKE ? THEN 1 ELSE 2 END, created DESC
			LIMIT ?`
		args = []any{likePattern, likePattern, likePattern, likePattern, likePattern, likePattern, limit}
	} else {
		countSQL = `SELECT COUNT(*) FROM papers WHERE id LIKE ? OR title LIKE ? OR authors LIKE ? OR categories LIKE ?`
		sql = `SELECT * FROM papers
			WHERE id LIKE ? OR title LIKE ? OR authors LIKE ? OR categories LIKE ?
			ORDER BY CASE WHEN id LIKE ? THEN 0 WHEN title LIKE ? THEN 1 ELSE 2 END, created DESC
			LIMIT ?`
		args = []any{likePattern, likePattern, likePattern, likePattern, likePattern, likePattern, limit}
	}

	// Get count
	var total int64
	sqlDB, _ := c.db.DB()
	if c.dbType == DBTypePostgres {
		sqlDB.QueryRowContext(ctx, countSQL, likePattern).Scan(&total)
	} else {
		sqlDB.QueryRowContext(ctx, countSQL, likePattern, likePattern, likePattern, likePattern).Scan(&total)
	}

	// Get results
	var papers []Paper
	err := c.db.WithContext(ctx).Raw(sql, args...).Scan(&papers).Error

	return papers, int(total), err
}

func (c *Cache) quickSearchPostgres(ctx context.Context, query string, limit int) ([]Paper, int, error) {
	if papers, total, ok, err := c.quickSearchPostgresID(ctx, query, limit); ok || err != nil {
		return papers, total, err
	}
	if looksLikeAuthorSearchQuery(query) {
		total, err := c.CountPapersByAuthorResult(ctx, query)
		if err != nil {
			return nil, 0, fmt.Errorf("count author search results: %w", err)
		}
		if total > 0 {
			papers, err := c.SearchByAuthor(ctx, query, limit)
			return papers, int(total), err
		}
	}

	fetchLimit := limit + 1
	var papers []Paper
	err := c.withPostgresSearchLimit(ctx, query, func(tx *gorm.DB) error {
		return tx.Raw(`
			SELECT id, created, updated, title, authors, categories, pdf_downloaded, src_downloaded,
			       ts_rank(search_vector, plainto_tsquery('english', ?)) AS rank
			FROM papers
			WHERE search_vector @@ plainto_tsquery('english', ?)
			ORDER BY rank DESC, created DESC NULLS LAST
			LIMIT ?
		`, query, query, fetchLimit).Scan(&papers).Error
	})
	if len(papers) == 0 {
		return c.quickSearchPostgresFallback(ctx, query, limit)
	}
	if err != nil {
		return c.quickSearchPostgresFallback(ctx, query, limit)
	}
	var total int64
	if err := c.withPostgresSearchLimit(ctx, query, func(tx *gorm.DB) error {
		return tx.Raw(`SELECT count(*) FROM papers WHERE search_vector @@ plainto_tsquery('english', ?)`, query).Scan(&total).Error
	}); err != nil {
		return c.quickSearchPostgresFallback(ctx, query, limit)
	}
	if len(papers) > limit {
		papers = papers[:limit]
	}
	return papers, int(total), nil
}

func (c *Cache) quickSearchPostgresID(ctx context.Context, query string, limit int) ([]Paper, int, bool, error) {
	if !looksLikePaperIDPrefix(query) {
		return nil, 0, false, nil
	}
	upper, ok := nextStringPrefix(query)
	if !ok {
		return nil, 0, false, nil
	}

	matching := c.db.WithContext(ctx).Model(&Paper{}).Where("id >= ? AND id < ?", query, upper)
	var total int64
	if err := matching.Count(&total).Error; err != nil {
		return nil, 0, true, err
	}
	if total == 0 {
		return nil, 0, false, nil
	}
	var papers []Paper
	err := matching.
		Select("id, created, updated, title, authors, categories, pdf_downloaded, src_downloaded").
		Order("id DESC").
		Limit(limit).
		Find(&papers).Error
	return papers, int(total), true, err
}

func (c *Cache) quickSearchPostgresFallback(ctx context.Context, query string, limit int) ([]Paper, int, error) {
	likePattern := "%" + query + "%"
	matching := c.db.WithContext(ctx).Model(&Paper{}).
		Where("id ILIKE ? OR title ILIKE ? OR authors ILIKE ? OR categories ILIKE ?", likePattern, likePattern, likePattern, likePattern)
	var total int64
	if err := matching.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var papers []Paper
	err := matching.
		Select("id, created, updated, title, authors, categories, pdf_downloaded, src_downloaded").
		Order("created DESC NULLS LAST").
		Limit(limit).
		Find(&papers).Error
	return papers, int(total), err
}

func looksLikePaperIDPrefix(query string) bool {
	if query == "" || strings.ContainsAny(query, " \t\r\n") || len(query) > 32 {
		return false
	}
	return strings.ContainsAny(query, "0123456789./")
}

func looksLikeAuthorSearchQuery(query string) bool {
	query = strings.TrimSpace(query)
	if len(query) < 3 || len(query) > 80 || strings.ContainsAny(query, "/\\@?=&:") {
		return false
	}
	parts := strings.Fields(query)
	if len(parts) < 2 || len(parts) > 5 {
		return false
	}
	for _, part := range parts {
		if strings.ContainsAny(part, "0123456789") {
			return false
		}
	}
	return hasNameCasing(parts)
}

func hasNameCasing(parts []string) bool {
	for _, part := range parts {
		part = strings.TrimLeft(part, `("'[`)
		if part == "" {
			continue
		}
		ch := part[0]
		if ch >= 'A' && ch <= 'Z' {
			return true
		}
	}
	return false
}

func nextStringPrefix(prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

// searchSQLite uses FTS5 for full-text search
func (c *Cache) searchSQLite(ctx context.Context, query, category string, limit int) ([]Paper, error) {
	sql := `
		SELECT p.id, p.created, p.updated, p.title, p.abstract, p.authors, p.categories,
		       p.comments, p.journal_ref, p.doi, p.license, p.pdf_downloaded, p.src_downloaded
		FROM papers p
		JOIN papers_fts fts ON p.rowid = fts.rowid
		WHERE papers_fts MATCH ?
	`
	args := []any{query}

	if category != "" {
		sql += " AND p.categories LIKE '%' || ? || '%'"
		args = append(args, category)
	}

	sql += " ORDER BY rank LIMIT ?"
	args = append(args, limit)

	sqlDB, _ := c.db.DB()
	rows, err := sqlDB.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var papers []Paper
	for rows.Next() {
		var p Paper
		var created, updated string
		var pdfDl, srcDl int

		err := rows.Scan(
			&p.ID, &created, &updated, &p.Title, &p.Abstract, &p.Authors,
			&p.Categories, &p.Comments, &p.JournalRef, &p.DOI, &p.License,
			&pdfDl, &srcDl,
		)
		if err != nil {
			return nil, err
		}

		p.Created, _ = time.Parse("2006-01-02", created)
		p.Updated, _ = time.Parse("2006-01-02", updated)
		p.PDFDownloaded = pdfDl == 1
		p.SourceDownloaded = srcDl == 1

		papers = append(papers, p)
	}

	return papers, rows.Err()
}

// searchPostgres uses tsvector for full-text search
func (c *Cache) searchPostgres(ctx context.Context, query, category string, limit int) ([]Paper, error) {
	sql := `
		SELECT id, created, updated, title, abstract, authors, categories,
		       comments, journal_ref, doi, license, pdf_downloaded, src_downloaded,
		       ts_rank(search_vector, plainto_tsquery('english', ?)) AS rank
		FROM papers
		WHERE search_vector @@ plainto_tsquery('english', ?)
	`
	args := []any{query, query}

	if category != "" {
		sql += " AND categories ILIKE '%' || ? || '%'"
		args = append(args, category)
	}

	sql += " ORDER BY rank DESC LIMIT ?"
	args = append(args, limit)

	var papers []Paper
	err := c.withPostgresSearchLimit(ctx, query, func(tx *gorm.DB) error {
		return tx.Raw(sql, args...).Scan(&papers).Error
	})
	if err != nil || len(papers) == 0 {
		// Fallback to ILIKE search if tsvector not populated
		return c.searchPostgresFallback(ctx, query, category, limit)
	}

	return papers, nil
}

// searchPostgresFallback uses ILIKE for searching when tsvector not available
func (c *Cache) searchPostgresFallback(ctx context.Context, query, category string, limit int) ([]Paper, error) {
	q := c.db.WithContext(ctx).Model(&Paper{}).
		Where("title ILIKE ? OR abstract ILIKE ?", "%"+query+"%", "%"+query+"%")

	if category != "" {
		q = q.Where("categories ILIKE ?", "%"+category+"%")
	}

	var papers []Paper
	err := q.Order("created DESC").Limit(limit).Find(&papers).Error
	return papers, err
}

func (c *Cache) withPostgresSearchLimit(ctx context.Context, query string, fn func(*gorm.DB) error) error {
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		searchLimit := 1000
		if len(strings.Fields(query)) > 1 {
			searchLimit = 5000
		}
		searchLimitSQL := "SET LOCAL gin_fuzzy_search_limit = 1000"
		if searchLimit == 5000 {
			searchLimitSQL = "SET LOCAL gin_fuzzy_search_limit = 5000"
		}
		if err := tx.Exec(searchLimitSQL).Error; err != nil {
			return err
		}
		if err := tx.Exec("SET LOCAL jit = off").Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

// SearchByAuthor searches papers by author name.
func (c *Cache) SearchByAuthor(ctx context.Context, author string, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 100
	}
	cacheKey := detailKey("author_search", author, fmt.Sprint(limit))
	if cached, ok := c.getDetailCache(cacheKey); ok {
		if papers, ok := cached.([]Paper); ok {
			return clonePapers(papers), nil
		}
	}

	value, err, _ := c.detailFlights.Do(cacheKey, func() (interface{}, error) {
		if cached, ok := c.getDetailCache(cacheKey); ok {
			if papers, ok := cached.([]Paper); ok {
				return clonePapers(papers), nil
			}
		}
		papers, err := c.searchByAuthorUncached(ctx, author, limit)
		if err != nil {
			return nil, err
		}
		c.putDetailCache(cacheKey, authorSearchTTL, clonePapers(papers))
		return papers, nil
	})
	if err != nil {
		return nil, err
	}
	papers, _ := value.([]Paper)
	return clonePapers(papers), nil
}

func (c *Cache) searchByAuthorUncached(ctx context.Context, author string, limit int) ([]Paper, error) {
	// Use ILIKE for PostgreSQL, LIKE for SQLite (SQLite LIKE is case-insensitive by default)
	likeOp := "LIKE"
	if c.dbType == DBTypePostgres {
		likeOp = "ILIKE"
	}

	// Search for both "First Last" and "Last, First" formats
	flipped := flipAuthorName(author)

	var papers []Paper
	var err error
	query := c.db.WithContext(ctx).
		Model(&Paper{}).
		Select("id, created, updated, title, authors, categories, pdf_downloaded, src_downloaded, fetched_at")
	err = c.withAuthorQuery(ctx, func() error {
		if flipped != "" {
			return query.
				Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%").
				Order("created DESC").
				Limit(limit).
				Find(&papers).Error
		}
		return query.
			Where("authors "+likeOp+" ?", "%"+author+"%").
			Order("created DESC").
			Limit(limit).
			Find(&papers).Error
	})
	return papers, err
}

// PaperExists checks if a paper exists in the cache.
func (c *Cache) PaperExists(ctx context.Context, id string) bool {
	var count int64
	err := c.db.WithContext(ctx).Model(&Paper{}).Where("id = ?", id).Count(&count).Error
	return err == nil && count > 0
}

// CategoryCount represents a category with its paper count.
type CategoryCount struct {
	Name  string
	Count int
}

// ListCategories returns all categories with their paper counts.
func (c *Cache) ListCategories(ctx context.Context) ([]CategoryCount, error) {
	if c.dbType == DBTypePostgres {
		categories, err := c.listStoredCategories(ctx)
		fresh, freshnessErr := c.categoryCountsFresh(ctx)
		if err == nil && freshnessErr == nil && fresh && len(categories) > 0 {
			return categories, nil
		}
	}

	categories, err := c.listCategoriesFromPapers(ctx)
	if err != nil {
		return nil, err
	}
	if c.dbType == DBTypePostgres {
		_ = c.storeCategoryCounts(ctx, categories)
	}
	return categories, nil
}

func (c *Cache) categoryCountsFresh(ctx context.Context) (bool, error) {
	var latestPaper, latestCategory *time.Time
	if err := c.db.WithContext(ctx).Raw(`SELECT MAX(metadata_updated) FROM papers`).Scan(&latestPaper).Error; err != nil {
		return false, err
	}
	if err := c.db.WithContext(ctx).Raw(`SELECT MAX(updated_at) FROM category_counts`).Scan(&latestCategory).Error; err != nil {
		return false, err
	}
	if latestCategory == nil {
		return false, nil
	}
	return latestPaper == nil || !latestCategory.Before(*latestPaper), nil
}

func (c *Cache) listStoredCategories(ctx context.Context) ([]CategoryCount, error) {
	var categories []CategoryCount
	err := c.db.WithContext(ctx).
		Table("category_counts").
		Select("name, count").
		Order("count DESC, name ASC").
		Scan(&categories).Error
	return categories, err
}

func (c *Cache) listCategoriesFromPapers(ctx context.Context) ([]CategoryCount, error) {
	// Categories are space-separated in the categories column
	// We need to split and count each individual category
	var papers []Paper
	if err := c.db.WithContext(ctx).Select("categories").Where("categories != ?", "").Find(&papers).Error; err != nil {
		return nil, err
	}

	counts := make(map[string]int)
	for _, p := range papers {
		for _, cat := range strings.Fields(p.Categories) {
			counts[cat]++
		}
	}

	var result []CategoryCount
	for name, count := range counts {
		result = append(result, CategoryCount{Name: name, Count: count})
	}

	sort.Slice(result, func(i, j int) bool {
		if result[i].Count == result[j].Count {
			return result[i].Name < result[j].Name
		}
		return result[i].Count > result[j].Count
	})

	return result, nil
}

func (c *Cache) storeCategoryCounts(ctx context.Context, categories []CategoryCount) error {
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM category_counts").Error; err != nil {
			return err
		}
		for _, category := range categories {
			if err := tx.Create(&CategoryStat{
				Name:  category.Name,
				Count: category.Count,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ListRecentPapersLite lists recently fetched papers with only the columns needed
// for homepage display (ID, Title, Authors, Categories). This avoids fetching
// large text fields like Abstract and PDFText, dramatically reducing DB I/O.
func (c *Cache) ListRecentPapersLite(ctx context.Context, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 50
	}

	var papers []Paper
	orderClause := "fetched_at DESC, id DESC"
	if c.dbType == DBTypePostgres {
		orderClause = "fetched_at DESC NULLS LAST, id DESC"
	}

	err := c.db.WithContext(ctx).
		Model(&Paper{}).
		Select("id, title, authors, categories").
		Order(orderClause).
		Limit(limit).
		Find(&papers).Error

	return papers, err
}

// ListPapers lists papers, optionally filtered by category.
// Sorted by FetchedAt (most recently fetched first), with fallback to ID for legacy papers.
func (c *Cache) ListPapers(ctx context.Context, category string, offset, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 100
	}

	query := c.db.WithContext(ctx).Model(&Paper{}).
		Select("id, created, updated, title, authors, categories, pdf_downloaded, src_downloaded, fetched_at")
	if category != "" {
		// Use ILIKE for PostgreSQL
		if c.dbType == DBTypePostgres {
			query = query.Where("categories ILIKE ?", "%"+category+"%")
		} else {
			query = query.Where("categories LIKE ?", "%"+category+"%")
		}
	}

	var papers []Paper
	orderClause := "fetched_at DESC, id DESC"
	if c.dbType == DBTypePostgres {
		orderClause = "fetched_at DESC NULLS LAST, id DESC"
	}

	err := query.
		Where("src_downloaded = ?", true).
		Order(orderClause).
		Limit(limit).
		Offset(offset).
		Find(&papers).Error

	return papers, err
}

// CountSitemapPapers counts paper pages that should be exposed to crawlers.
func (c *Cache) CountSitemapPapers(ctx context.Context) (int64, error) {
	var count int64
	err := c.db.WithContext(ctx).Model(&Paper{}).Count(&count).Error
	return count, err
}

// ListSitemapPapers lists only the fields needed to generate paper sitemaps.
func (c *Cache) ListSitemapPapers(ctx context.Context, offset, limit int) ([]Paper, error) {
	if limit <= 0 {
		limit = 50000
	}

	var papers []Paper
	err := c.db.WithContext(ctx).Raw(`
		SELECT id, updated
		FROM papers
		ORDER BY id ASC
		LIMIT ? OFFSET ?
	`, limit, offset).Scan(&papers).Error
	return papers, err
}

// ListPapersFiltered lists papers with various filter options.
func (c *Cache) ListPapersFiltered(ctx context.Context, category string, srcOnly, all bool, limit int) ([]Paper, error) {
	query := c.db.WithContext(ctx).Model(&Paper{})

	if category != "" {
		if c.dbType == DBTypePostgres {
			query = query.Where("categories ILIKE ?", "%"+category+"%")
		} else {
			query = query.Where("categories LIKE ?", "%"+category+"%")
		}
	}

	if srcOnly {
		query = query.Where("src_downloaded = ?", true)
	} else if !all {
		// Default: show papers with source OR title (exclude metadata-only without useful info)
		query = query.Where("src_downloaded = ? OR title != ?", true, "")
	}

	query = query.Order("id DESC")

	if limit > 0 {
		query = query.Limit(limit)
	}

	var papers []Paper
	err := query.Find(&papers).Error
	return papers, err
}

// DownloadCategory downloads papers for a category.
func (c *Cache) DownloadCategory(ctx context.Context, category string, limit int, opts *DownloadOptions) error {
	normalized := normalizedDownloadOptions(opts)
	opts = &normalized
	// Use parameter placeholders appropriate for database type
	placeholder := "?"
	if c.dbType == DBTypePostgres {
		placeholder = "$"
	}

	var sql string
	var args []any

	if c.dbType == DBTypePostgres {
		sql = `
			SELECT id FROM papers
			WHERE categories ILIKE '%' || $1 || '%'
			AND (
				($2 = 1 AND pdf_downloaded = false) OR
				($3 = 1 AND src_downloaded = false)
			)
			ORDER BY created DESC
		`
	} else {
		sql = `
			SELECT id FROM papers
			WHERE categories LIKE '%' || ? || '%'
			AND (
				(? = 1 AND pdf_downloaded = 0) OR
				(? = 1 AND src_downloaded = 0)
			)
			ORDER BY created DESC
		`
	}

	args = []any{category}

	dlPDF := 0
	dlSrc := 0
	if opts.DownloadPDF {
		dlPDF = 1
	}
	if opts.DownloadSource {
		dlSrc = 1
	}
	args = append(args, dlPDF, dlSrc)

	if limit > 0 {
		if c.dbType == DBTypePostgres {
			sql += " LIMIT $4"
		} else {
			sql += " LIMIT " + placeholder
		}
		args = append(args, limit)
	}

	sqlDB, _ := c.db.DB()
	rows, err := sqlDB.QueryContext(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	return c.downloadPapers(ctx, ids, *opts)
}
