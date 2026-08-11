package arxiv

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SyncOptions configures metadata synchronization.
type SyncOptions struct {
	// Set filters to a specific arXiv set (e.g., "cs" for computer science)
	Set string

	// From is the start date for incremental sync
	From time.Time

	// Until is the end date for sync
	Until time.Time

	// Progress callback for reporting sync progress
	Progress func(fetched, total int)

	// BatchSize controls insert statement size (default 1000). Each OAI page and
	// its checkpoint are still committed atomically.
	BatchSize int
}

// SyncMetadata synchronizes paper metadata from arXiv via OAI-PMH.
func (c *Cache) SyncMetadata(ctx context.Context, opts *SyncOptions) error {
	if opts == nil {
		opts = &SyncOptions{}
	}
	options := *opts
	if options.BatchSize <= 0 {
		options.BatchSize = 1000
	}

	client := NewOAIClient()

	lastSyncKey := syncLastSyncKey(options.Set)
	if options.From.IsZero() {
		var lastSyncState SyncState
		if err := c.db.WithContext(ctx).Where("key = ?", lastSyncKey).First(&lastSyncState).Error; err == nil && lastSyncState.Value != "" {
			if parsed, parseErr := time.Parse("2006-01-02", lastSyncState.Value); parseErr == nil {
				options.From = parsed
			}
		}
	}
	resumptionKey := syncResumptionKey(options)

	// Resume only an identical set/date scope. Tokens are opaque and cannot be
	// safely reused across independent harvests.
	var state SyncState
	if err := c.db.WithContext(ctx).Where("key = ?", resumptionKey).First(&state).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("load sync checkpoint: %w", err)
	}
	resumptionToken := state.Value

	totalFetched := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.ListRecords(ctx, options.Set, options.From, options.Until, resumptionToken)
		if err != nil {
			return fmt.Errorf("list records: %w", err)
		}

		if err := c.persistSyncRecords(ctx, resp.Papers, resp.DeletedPaperIDs, resumptionKey, resp.ResumptionToken, options.BatchSize); err != nil {
			return fmt.Errorf("persist sync page: %w", err)
		}
		totalFetched += resp.RecordCount
		if options.Progress != nil {
			options.Progress(totalFetched, resp.CompleteListSize)
		}

		if resp.ResumptionToken != "" {
			resumptionToken = resp.ResumptionToken

			// arXiv rate limit: wait between requests
			timer := time.NewTimer(3 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		} else {
			// No more records
			break
		}
	}

	// Clear resumption token and save last sync date
	if err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("key = ?", resumptionKey).Delete(&SyncState{}).Error; err != nil {
			return err
		}
		return tx.Save(&SyncState{Key: lastSyncKey, Value: time.Now().UTC().Format("2006-01-02")}).Error
	}); err != nil {
		return fmt.Errorf("finalize sync: %w", err)
	}

	log.Printf("sync complete: %d papers", totalFetched)
	return nil
}

func (c *Cache) persistSyncPage(ctx context.Context, papers []Paper, token string) error {
	return c.persistSyncRecords(ctx, papers, nil, "resumption_token", token, 1000)
}

func (c *Cache) persistSyncRecords(ctx context.Context, papers []Paper, deletedIDs []string, stateKey, token string, batchSize int) error {
	if err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := c.insertPapersWithDB(ctx, tx, papers, batchSize); err != nil {
			return err
		}
		if len(deletedIDs) > 0 {
			if err := deleteOAIPapers(ctx, tx, deletedIDs); err != nil {
				return err
			}
		}
		return tx.WithContext(ctx).Save(&SyncState{Key: stateKey, Value: token}).Error
	}); err != nil {
		return err
	}
	for i := range papers {
		c.paperLRU.Delete(papers[i].ID)
	}
	for _, id := range deletedIDs {
		c.paperLRU.Delete(id)
	}
	c.detailLRU.Clear()
	return nil
}

func (c *Cache) insertPapers(ctx context.Context, papers []Paper) error {
	if err := c.insertPapersWithDB(ctx, c.db, papers, 1000); err != nil {
		return err
	}
	for i := range papers {
		c.paperLRU.Delete(papers[i].ID)
	}
	c.detailLRU.Clear()
	return nil
}

func (c *Cache) insertPapersWithDB(ctx context.Context, db *gorm.DB, papers []Paper, batchSize int) error {
	if len(papers) == 0 {
		return nil
	}

	now := time.Now()
	for i := range papers {
		papers[i].MetadataUpdated = &now
		if papers[i].FetchedAt == nil {
			papers[i].FetchedAt = &now
		}
	}

	if batchSize <= 0 {
		batchSize = 1000
	}
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"created",
			"updated",
			"title",
			"abstract",
			"authors",
			"categories",
			"comments",
			"journal_ref",
			"doi",
			"license",
			"metadata_updated",
		}),
	}).CreateInBatches(&papers, batchSize).Error
}

func deleteOAIPapers(ctx context.Context, tx *gorm.DB, ids []string) error {
	for _, model := range []interface{}{&Citation{}, &Embedding{}, &EmbeddingV2{}, &QwenEmbeddingJob{}, &EmbeddingJob{}} {
		query := tx.WithContext(ctx).Where("paper_id IN ?", ids)
		if _, ok := model.(*Citation); ok {
			query = tx.WithContext(ctx).Where("from_id IN ? OR to_id IN ?", ids, ids)
		}
		if err := query.Delete(model).Error; err != nil {
			return err
		}
	}
	var chunkIDs []string
	if err := tx.WithContext(ctx).Model(&PaperChunk{}).Where("paper_id IN ?", ids).Pluck("id", &chunkIDs).Error; err != nil {
		return err
	}
	if len(chunkIDs) > 0 {
		if err := tx.WithContext(ctx).Where("chunk_id IN ?", chunkIDs).Delete(&ChunkEmbeddingV2{}).Error; err != nil {
			return err
		}
	}
	if err := tx.WithContext(ctx).Where("paper_id IN ?", ids).Delete(&PaperChunk{}).Error; err != nil {
		return err
	}
	if err := tx.WithContext(ctx).Where("id IN ?", ids).Delete(&Paper{}).Error; err != nil {
		return err
	}
	return tx.WithContext(ctx).Exec("DELETE FROM category_counts").Error
}

func syncLastSyncKey(set string) string {
	return "last_sync:" + fmt.Sprintf("%x", sha256.Sum256([]byte(set)))[:16]
}

func syncResumptionKey(opts SyncOptions) string {
	scope := opts.Set + "\x00" + opts.From.UTC().Format("2006-01-02") + "\x00" + opts.Until.UTC().Format("2006-01-02")
	return "resumption_token:" + fmt.Sprintf("%x", sha256.Sum256([]byte(scope)))[:16]
}
