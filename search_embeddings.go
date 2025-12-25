package arxiv

import (
	"context"
)

// CountEmbeddings returns the number of papers with embeddings.
func (c *Cache) CountEmbeddings(ctx context.Context) (int64, error) {
	var count int64
	err := c.db.WithContext(ctx).Model(&Embedding{}).Count(&count).Error
	return count, err
}
