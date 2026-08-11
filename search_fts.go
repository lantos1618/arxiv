package arxiv

import (
	"context"

	"gorm.io/gorm"
)

// RebuildFTSIndex rebuilds the FTS5 index from all papers.
// Use this after migrating an existing database to FTS5.
// Note: Uses raw SQL because GORM doesn't support FTS5 virtual tables.
func (c *Cache) RebuildFTSIndex(ctx context.Context) error {
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM papers_fts").Error; err != nil {
			return err
		}
		return tx.Exec(`
			INSERT INTO papers_fts(rowid, title, abstract)
			SELECT rowid, title, abstract FROM papers
		`).Error
	})
}
