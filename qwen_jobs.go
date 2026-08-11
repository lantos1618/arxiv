package arxiv

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const qwenPaperChunkScope = "pdf_text"

const maxQwenEmbeddingJobLease = time.Hour

// MaxQwenEmbeddingJobAttempts caps automatic retries before a failed job becomes terminal.
const MaxQwenEmbeddingJobAttempts = 3

var ErrQwenEmbeddingJobLeaseLost = errors.New("qwen embedding job lease lost")

// QwenPaperEmbeddingStatus summarizes all Qwen readiness signals for a paper.
type QwenPaperEmbeddingStatus struct {
	PaperID              string             `json:"paperId"`
	AbstractReady        bool               `json:"abstractReady"`
	PaperChunksReady     bool               `json:"paperChunksReady"`
	ChunkEmbeddingsReady bool               `json:"chunkEmbeddingsReady"`
	MapReady             bool               `json:"mapReady"`
	FullPaperReady       bool               `json:"fullPaperReady"`
	ChunkCount           int64              `json:"chunkCount"`
	ChunkEmbeddingCount  int64              `json:"chunkEmbeddingCount"`
	QueuedJobs           int64              `json:"queuedJobs"`
	RunningJobs          int64              `json:"runningJobs"`
	FailedJobs           int64              `json:"failedJobs"`
	Jobs                 []QwenEmbeddingJob `json:"jobs,omitempty"`
	Model                string             `json:"model"`
	Dim                  int                `json:"dim"`
	UpdatedAt            time.Time          `json:"updatedAt"`
}

// QwenQueryEmbeddingStatus summarizes a cached or queued query embedding.
type QwenQueryEmbeddingStatus struct {
	Query       string             `json:"query"`
	QueryHash   string             `json:"queryHash"`
	Ready       bool               `json:"ready"`
	QueuedJobs  int64              `json:"queuedJobs"`
	RunningJobs int64              `json:"runningJobs"`
	FailedJobs  int64              `json:"failedJobs"`
	Jobs        []QwenEmbeddingJob `json:"jobs,omitempty"`
	Model       string             `json:"model"`
	Dim         int                `json:"dim"`
	UpdatedAt   time.Time          `json:"updatedAt"`
}

type qwenJobTarget struct {
	kind  string
	scope string
	model string
	dim   int
}

// NormalizeQwenQuery returns the canonical text used for query-vector caching.
func NormalizeQwenQuery(query string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(query)), " ")
}

// QwenQueryHash returns the normalized query and its stable cache key.
func QwenQueryHash(query string) (string, string) {
	normalized := NormalizeQwenQuery(query)
	sum := sha256.Sum256([]byte(strings.ToLower(normalized)))
	return normalized, fmt.Sprintf("%x", sum[:])
}

func qwenQueryJobPaperID(queryHash string) string {
	return "query:" + queryHash
}

func qwenQueryHashFromJobPaperID(paperID string) string {
	return strings.TrimPrefix(strings.TrimSpace(paperID), "query:")
}

// QwenQueryEmbeddingStatus returns readiness for a query embedding cache entry.
func (c *Cache) QwenQueryEmbeddingStatus(ctx context.Context, query string) (*QwenQueryEmbeddingStatus, error) {
	normalized, queryHash := QwenQueryHash(query)
	if normalized == "" {
		return nil, fmt.Errorf("query is required")
	}
	status := &QwenQueryEmbeddingStatus{
		Query:     normalized,
		QueryHash: queryHash,
		Model:     qwenEmbeddingModel,
		Dim:       qwenEmbeddingDim,
		UpdatedAt: time.Now().UTC(),
	}

	if _, ok, err := c.GetQwenQueryEmbedding(ctx, normalized); err != nil {
		return nil, err
	} else {
		status.Ready = ok
	}

	if err := c.db.WithContext(ctx).
		Where("paper_id = ? AND kind = ?", qwenQueryJobPaperID(queryHash), QwenEmbeddingJobKindQuery).
		Order("priority DESC, created_at ASC").
		Find(&status.Jobs).Error; err != nil {
		return nil, fmt.Errorf("load qwen query jobs: %w", err)
	}
	for _, job := range status.Jobs {
		switch job.Status {
		case QwenEmbeddingJobQueued:
			status.QueuedJobs++
		case QwenEmbeddingJobRunning:
			status.RunningJobs++
		case QwenEmbeddingJobFailed:
			status.FailedJobs++
		}
	}
	return status, nil
}

// EnsureQwenQueryJob queues a JIT query embedding when the query vector is absent.
func (c *Cache) EnsureQwenQueryJob(ctx context.Context, query string, priority int) (*QwenQueryEmbeddingStatus, error) {
	normalized, queryHash := QwenQueryHash(query)
	if normalized == "" {
		return nil, fmt.Errorf("query is required")
	}
	if priority < 0 {
		priority = 0
	}
	if _, ok, err := c.GetQwenQueryEmbedding(ctx, normalized); err != nil {
		return nil, err
	} else if ok {
		return c.QwenQueryEmbeddingStatus(ctx, normalized)
	}

	now := time.Now().UTC()
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := c.ensureQwenQueryEmbeddingRow(ctx, tx, normalized, queryHash, now); err != nil {
			return err
		}
		return c.ensureQwenEmbeddingJob(ctx, tx, qwenQueryJobPaperID(queryHash), qwenJobTarget{
			kind:  QwenEmbeddingJobKindQuery,
			scope: "query",
			model: qwenEmbeddingModel,
			dim:   qwenEmbeddingDim,
		}, priority, now)
	})
	if err != nil {
		return nil, err
	}
	return c.QwenQueryEmbeddingStatus(ctx, normalized)
}

func (c *Cache) ensureQwenQueryEmbeddingRow(ctx context.Context, tx *gorm.DB, queryText, queryHash string, now time.Time) error {
	if c.dbType != DBTypePostgres {
		return fmt.Errorf("qwen query embeddings require PostgreSQL with pgvector")
	}
	textChars := len(queryText)
	tokenEstimate := max(1, textChars/4)
	return tx.WithContext(ctx).Exec(`
		INSERT INTO qwen_query_embeddings
			(query_hash, query_text, model, dim, text_chars, token_estimate, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (query_hash, model, dim) DO UPDATE SET
			query_text = EXCLUDED.query_text,
			text_chars = EXCLUDED.text_chars,
			token_estimate = EXCLUDED.token_estimate,
			updated = now()
	`, queryHash, queryText, qwenEmbeddingModel, qwenEmbeddingDim, textChars, tokenEstimate, now, now).Error
}

// GetQwenQueryEmbedding returns a cached Qwen query vector when available.
func (c *Cache) GetQwenQueryEmbedding(ctx context.Context, query string) ([]float32, bool, error) {
	if c.dbType != DBTypePostgres {
		return nil, false, nil
	}
	normalized, queryHash := QwenQueryHash(query)
	if normalized == "" {
		return nil, false, fmt.Errorf("query is required")
	}

	sqlDB, err := c.db.DB()
	if err != nil {
		return nil, false, err
	}
	var vectorText string
	err = sqlDB.QueryRowContext(ctx, `
		SELECT vector::text
		FROM qwen_query_embeddings
		WHERE query_hash = $1
		  AND model = $2
		  AND dim = $3
		  AND vector IS NOT NULL
	`, queryHash, qwenEmbeddingModel, qwenEmbeddingDim).Scan(&vectorText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load qwen query embedding: %w", err)
	}
	values, err := parsePgVectorText(vectorText)
	if err != nil {
		return nil, false, fmt.Errorf("parse qwen query embedding: %w", err)
	}
	if len(values) != qwenEmbeddingDim {
		return nil, false, fmt.Errorf("qwen query embedding has %d dimensions, want %d", len(values), qwenEmbeddingDim)
	}
	embedding := make([]float32, len(values))
	for i, value := range values {
		embedding[i] = float32(value)
	}
	return embedding, true, nil
}

// StoreQwenQueryEmbedding stores a completed JIT query vector.
func (c *Cache) StoreQwenQueryEmbedding(ctx context.Context, queryHash string, embedding []float32) error {
	if c.dbType != DBTypePostgres {
		return fmt.Errorf("qwen query embeddings require PostgreSQL with pgvector")
	}
	queryHash = strings.TrimSpace(queryHash)
	if queryHash == "" {
		return fmt.Errorf("query hash is required")
	}
	if len(embedding) != qwenEmbeddingDim {
		return fmt.Errorf("qwen query embedding has %d dimensions, want %d", len(embedding), qwenEmbeddingDim)
	}
	sqlDB, err := c.db.DB()
	if err != nil {
		return err
	}
	result, err := sqlDB.ExecContext(ctx, `
		UPDATE qwen_query_embeddings
		SET vector = $1::vector,
		    updated = now()
		WHERE query_hash = $2
		  AND model = $3
		  AND dim = $4
	`, float32SliceToVectorString(embedding), queryHash, qwenEmbeddingModel, qwenEmbeddingDim)
	if err != nil {
		return fmt.Errorf("store qwen query embedding: %w", err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("query embedding row not found")
	}
	return nil
}

// StoreQwenQueryEmbeddingForQuery upserts a direct-service query vector.
func (c *Cache) StoreQwenQueryEmbeddingForQuery(ctx context.Context, query string, embedding []float32) error {
	if c.dbType != DBTypePostgres {
		return fmt.Errorf("qwen query embeddings require PostgreSQL with pgvector")
	}
	normalized, queryHash := QwenQueryHash(query)
	if normalized == "" {
		return fmt.Errorf("query is required")
	}
	if len(embedding) != qwenEmbeddingDim {
		return fmt.Errorf("qwen query embedding has %d dimensions, want %d", len(embedding), qwenEmbeddingDim)
	}
	textChars := len(normalized)
	tokenEstimate := max(1, textChars/4)
	sqlDB, err := c.db.DB()
	if err != nil {
		return err
	}
	_, err = sqlDB.ExecContext(ctx, `
		INSERT INTO qwen_query_embeddings
			(query_hash, query_text, model, dim, text_chars, token_estimate, vector, created, updated)
		VALUES ($1, $2, $3, $4, $5, $6, $7::vector, now(), now())
		ON CONFLICT (query_hash, model, dim) DO UPDATE SET
			query_text = EXCLUDED.query_text,
			text_chars = EXCLUDED.text_chars,
			token_estimate = EXCLUDED.token_estimate,
			vector = EXCLUDED.vector,
			updated = now()
	`, queryHash, normalized, qwenEmbeddingModel, qwenEmbeddingDim, textChars, tokenEstimate, float32SliceToVectorString(embedding))
	if err != nil {
		return fmt.Errorf("store qwen query embedding: %w", err)
	}
	return nil
}

// GetQwenQueryText returns the canonical text for a queued query job.
func (c *Cache) GetQwenQueryText(ctx context.Context, queryHash string) (string, error) {
	queryHash = strings.TrimSpace(queryHash)
	if queryHash == "" {
		return "", fmt.Errorf("query hash is required")
	}
	var row QwenQueryEmbedding
	if err := c.db.WithContext(ctx).
		Where("query_hash = ? AND model = ? AND dim = ?", queryHash, qwenEmbeddingModel, qwenEmbeddingDim).
		First(&row).Error; err != nil {
		return "", err
	}
	return NormalizeQwenQuery(row.QueryText), nil
}

// QwenPaperEmbeddingStatus returns Qwen abstract and full-paper readiness for a paper.
func (c *Cache) QwenPaperEmbeddingStatus(ctx context.Context, paperID string) (*QwenPaperEmbeddingStatus, error) {
	paperID = strings.TrimSpace(paperID)
	if paperID == "" {
		return nil, fmt.Errorf("paper ID is required")
	}

	status := &QwenPaperEmbeddingStatus{
		PaperID:   paperID,
		Model:     qwenEmbeddingModel,
		Dim:       qwenEmbeddingDim,
		UpdatedAt: time.Now().UTC(),
	}
	abstractReady, err := c.hasCurrentQwenAbstractEmbedding(ctx, paperID)
	if err != nil {
		return nil, err
	}
	status.AbstractReady = abstractReady
	status.MapReady = status.AbstractReady

	if err := c.db.WithContext(ctx).Model(&PaperChunk{}).
		Where("paper_id = ? AND scope = ?", paperID, qwenPaperChunkScope).
		Count(&status.ChunkCount).Error; err != nil {
		return nil, fmt.Errorf("count paper chunks: %w", err)
	}
	status.PaperChunksReady = status.ChunkCount > 0

	chunkEmbeddingCount, err := c.countQwenChunkEmbeddings(ctx, paperID)
	if err != nil {
		return nil, err
	}
	status.ChunkEmbeddingCount = chunkEmbeddingCount
	status.ChunkEmbeddingsReady = status.ChunkCount > 0 && status.ChunkEmbeddingCount >= status.ChunkCount
	status.FullPaperReady = status.PaperChunksReady && status.ChunkEmbeddingsReady

	if err := c.db.WithContext(ctx).
		Where("paper_id = ?", paperID).
		Order("priority DESC, created_at ASC").
		Find(&status.Jobs).Error; err != nil {
		return nil, fmt.Errorf("load qwen jobs: %w", err)
	}
	for _, job := range status.Jobs {
		switch job.Status {
		case QwenEmbeddingJobQueued:
			status.QueuedJobs++
		case QwenEmbeddingJobRunning:
			status.RunningJobs++
		case QwenEmbeddingJobFailed:
			status.FailedJobs++
		}
	}

	return status, nil
}

// EnsureQwenPaperJobs queues any Qwen work still needed for a paper.
func (c *Cache) EnsureQwenPaperJobs(ctx context.Context, paperID string, priority int) (*QwenPaperEmbeddingStatus, error) {
	paperID = strings.TrimSpace(paperID)
	if paperID == "" {
		return nil, fmt.Errorf("paper ID is required")
	}
	if priority < 0 {
		priority = 0
	}

	status, err := c.QwenPaperEmbeddingStatus(ctx, paperID)
	if err != nil {
		return nil, err
	}

	targets := make([]qwenJobTarget, 0, 3)
	if !status.AbstractReady {
		targets = append(targets, qwenJobTarget{
			kind:  QwenEmbeddingJobKindAbstract,
			scope: "abstract",
			model: qwenEmbeddingModel,
			dim:   qwenEmbeddingDim,
		})
	}
	if !status.PaperChunksReady {
		targets = append(targets, qwenJobTarget{
			kind:  QwenEmbeddingJobKindPaperChunks,
			scope: qwenPaperChunkScope,
			model: qwenEmbeddingModel,
			dim:   qwenEmbeddingDim,
		})
	}
	if !status.ChunkEmbeddingsReady {
		targets = append(targets, qwenJobTarget{
			kind:  QwenEmbeddingJobKindChunkEmbeddings,
			scope: qwenPaperChunkScope,
			model: qwenEmbeddingModel,
			dim:   qwenEmbeddingDim,
		})
	}
	if len(targets) == 0 {
		return status, nil
	}

	now := time.Now().UTC()
	err = c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, target := range targets {
			if err := c.ensureQwenEmbeddingJob(ctx, tx, paperID, target, priority, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.QwenPaperEmbeddingStatus(ctx, paperID)
}

// ClaimQwenEmbeddingJobs leases queued work for one worker.
func (c *Cache) ClaimQwenEmbeddingJobs(ctx context.Context, kinds []string, limit int, leaseOwner string, leaseFor time.Duration) ([]QwenEmbeddingJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	leaseOwner = trimForStorage(leaseOwner, 120)
	if leaseOwner == "" {
		leaseOwner = "worker"
	}
	if leaseFor <= 0 {
		leaseFor = 10 * time.Minute
	}
	if leaseFor > maxQwenEmbeddingJobLease {
		leaseFor = maxQwenEmbeddingJobLease
	}
	kindSet := normalizeQwenJobKinds(kinds)
	now := time.Now().UTC()
	leaseUntil := now.Add(leaseFor)

	var jobs []QwenEmbeddingJob
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		q := tx.Where("(status = ? OR (status = ? AND lease_until IS NOT NULL AND lease_until < ?))", QwenEmbeddingJobQueued, QwenEmbeddingJobRunning, now).
			Order("priority DESC, created_at ASC").
			Limit(limit)
		if len(kindSet) > 0 {
			q = q.Where("kind IN ?", kindSet)
		}
		if c.dbType == DBTypePostgres {
			q = q.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := q.Find(&jobs).Error; err != nil {
			return fmt.Errorf("claim qwen jobs: %w", err)
		}
		for i := range jobs {
			result := tx.Model(&QwenEmbeddingJob{}).
				Where("id = ? AND attempts = ? AND (status = ? OR (status = ? AND lease_until IS NOT NULL AND lease_until < ?))", jobs[i].ID, jobs[i].Attempts, QwenEmbeddingJobQueued, QwenEmbeddingJobRunning, now).
				Updates(map[string]any{
					"status":       QwenEmbeddingJobRunning,
					"attempts":     jobs[i].Attempts + 1,
					"lease_owner":  leaseOwner,
					"lease_until":  leaseUntil,
					"last_error":   "",
					"completed_at": nil,
					"updated_at":   now,
				})
			if result.Error != nil {
				return fmt.Errorf("lease qwen job: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("lease qwen job: %w", ErrQwenEmbeddingJobLeaseLost)
			}
			jobs[i].Status = QwenEmbeddingJobRunning
			jobs[i].Attempts++
			jobs[i].LeaseOwner = leaseOwner
			jobs[i].LeaseUntil = &leaseUntil
			jobs[i].LastError = ""
			jobs[i].CompletedAt = nil
			jobs[i].UpdatedAt = now
		}
		return nil
	})
	return jobs, err
}

// RenewQwenEmbeddingJobLease extends an active lease when its fence still matches.
func (c *Cache) RenewQwenEmbeddingJobLease(ctx context.Context, jobID, leaseOwner string, generation int, leaseFor time.Duration) (*QwenEmbeddingJob, error) {
	jobID = strings.TrimSpace(jobID)
	leaseOwner = trimForStorage(leaseOwner, 120)
	if jobID == "" || leaseOwner == "" || generation <= 0 {
		return nil, fmt.Errorf("job ID, lease owner, and generation are required")
	}
	if leaseFor <= 0 {
		leaseFor = 10 * time.Minute
	}
	if leaseFor > maxQwenEmbeddingJobLease {
		leaseFor = maxQwenEmbeddingJobLease
	}

	var job QwenEmbeddingJob
	if err := c.db.WithContext(ctx).Where("id = ?", jobID).First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("renew qwen job: %w", ErrQwenEmbeddingJobLeaseLost)
		}
		return nil, fmt.Errorf("load qwen job for renewal: %w", err)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(leaseFor)
	result := c.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).
		Where("id = ? AND status = ? AND lease_owner = ? AND attempts = ? AND lease_until IS NOT NULL AND lease_until >= ?", jobID, QwenEmbeddingJobRunning, leaseOwner, generation, now).
		Updates(map[string]any{
			"lease_until": leaseUntil,
			"updated_at":  now,
		})
	if result.Error != nil {
		return nil, fmt.Errorf("renew qwen job: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, fmt.Errorf("renew qwen job: %w", ErrQwenEmbeddingJobLeaseLost)
	}
	job.Status = QwenEmbeddingJobRunning
	job.LeaseOwner = leaseOwner
	job.Attempts = generation
	job.LeaseUntil = &leaseUntil
	job.UpdatedAt = now
	return &job, nil
}

// GetQwenEmbeddingJob returns a queued-worker job by ID.
func (c *Cache) GetQwenEmbeddingJob(ctx context.Context, jobID string) (*QwenEmbeddingJob, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, fmt.Errorf("job ID is required")
	}
	var job QwenEmbeddingJob
	if err := c.db.WithContext(ctx).Where("id = ?", jobID).First(&job).Error; err != nil {
		return nil, err
	}
	return &job, nil
}

// CompleteQwenEmbeddingJob marks a leased job complete when its lease fence still matches.
func (c *Cache) CompleteQwenEmbeddingJob(ctx context.Context, jobID, leaseOwner string, generation int) error {
	jobID = strings.TrimSpace(jobID)
	leaseOwner = trimForStorage(leaseOwner, 120)
	if jobID == "" || leaseOwner == "" || generation <= 0 {
		return fmt.Errorf("job ID, lease owner, and generation are required")
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, err := completeQwenEmbeddingJobLease(tx, jobID, leaseOwner, generation, time.Now().UTC())
		return err
	})
}

// CompleteQwenQueryEmbeddingJob atomically stores a query vector and completes its lease.
func (c *Cache) CompleteQwenQueryEmbeddingJob(ctx context.Context, jobID, leaseOwner string, generation int, queryHash string, embedding []float32) error {
	if c.dbType != DBTypePostgres {
		return fmt.Errorf("qwen query embeddings require PostgreSQL with pgvector")
	}
	queryHash = strings.TrimSpace(queryHash)
	if queryHash == "" {
		return fmt.Errorf("query hash is required")
	}
	if len(embedding) != qwenEmbeddingDim {
		return fmt.Errorf("qwen query embedding has %d dimensions, want %d", len(embedding), qwenEmbeddingDim)
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		alreadyComplete, err := completeQwenEmbeddingJobLease(tx, jobID, leaseOwner, generation, time.Now().UTC())
		if err != nil || alreadyComplete {
			return err
		}
		result := tx.Exec(`
			UPDATE qwen_query_embeddings
			SET vector = ?::vector, updated = now()
			WHERE query_hash = ? AND model = ? AND dim = ?
		`, float32SliceToVectorString(embedding), queryHash, qwenEmbeddingModel, qwenEmbeddingDim)
		if result.Error != nil {
			return fmt.Errorf("store qwen query embedding: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("query embedding row not found")
		}
		return nil
	})
}

// CompleteQwenAbstractEmbeddingJob atomically stores an abstract vector and completes its lease.
func (c *Cache) CompleteQwenAbstractEmbeddingJob(ctx context.Context, jobID, leaseOwner string, generation int, paperID, sourceHash string, textChars, tokenEstimate int, embedding []float32) error {
	if c.dbType != DBTypePostgres {
		return fmt.Errorf("qwen embeddings require PostgreSQL with pgvector")
	}
	if len(embedding) != qwenEmbeddingDim {
		return fmt.Errorf("qwen embedding has %d dimensions, want %d", len(embedding), qwenEmbeddingDim)
	}
	if tokenEstimate <= 0 {
		tokenEstimate = 1
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		alreadyComplete, err := completeQwenEmbeddingJobLease(tx, jobID, leaseOwner, generation, time.Now().UTC())
		if err != nil || alreadyComplete {
			return err
		}
		if err := tx.Exec(`
			INSERT INTO embeddings_v2
				(paper_id, scope, model, dim, source_hash, text_chars,
				 token_estimate, vector, created, updated)
			VALUES (?, 'abstract', ?, ?, ?, ?, ?, ?::vector, now(), now())
			ON CONFLICT (paper_id, scope, model, dim) DO UPDATE SET
				source_hash = EXCLUDED.source_hash,
				text_chars = EXCLUDED.text_chars,
				token_estimate = EXCLUDED.token_estimate,
				vector = EXCLUDED.vector,
				updated = now()
		`, paperID, qwenEmbeddingModel, qwenEmbeddingDim, sourceHash, textChars, tokenEstimate, float32SliceToVectorString(embedding)).Error; err != nil {
			return fmt.Errorf("store qwen abstract embedding: %w", err)
		}
		return nil
	})
}

func completeQwenEmbeddingJobLease(tx *gorm.DB, jobID, leaseOwner string, generation int, now time.Time) (bool, error) {
	jobID = strings.TrimSpace(jobID)
	leaseOwner = trimForStorage(leaseOwner, 120)
	if jobID == "" || leaseOwner == "" || generation <= 0 {
		return false, fmt.Errorf("job ID, lease owner, and generation are required")
	}
	result := tx.Model(&QwenEmbeddingJob{}).
		Where("id = ? AND status = ? AND lease_owner = ? AND attempts = ? AND lease_until IS NOT NULL AND lease_until >= ?", jobID, QwenEmbeddingJobRunning, leaseOwner, generation, now).
		Updates(map[string]any{
			"status":       QwenEmbeddingJobComplete,
			"lease_until":  nil,
			"last_error":   "",
			"completed_at": now,
			"updated_at":   now,
		})
	if result.Error != nil {
		return false, fmt.Errorf("complete qwen job: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return false, nil
	}
	var job QwenEmbeddingJob
	if err := tx.Where("id = ?", jobID).First(&job).Error; err != nil {
		return false, fmt.Errorf("complete qwen job: %w", ErrQwenEmbeddingJobLeaseLost)
	}
	if job.Status == QwenEmbeddingJobComplete && job.LeaseOwner == leaseOwner && job.Attempts == generation {
		return true, nil
	}
	return false, fmt.Errorf("complete qwen job: %w", ErrQwenEmbeddingJobLeaseLost)
}

// CompleteQwenPaperJob marks a paper-level job kind complete.
func (c *Cache) CompleteQwenPaperJob(ctx context.Context, paperID, kind string) error {
	paperID = strings.TrimSpace(paperID)
	kind = strings.TrimSpace(kind)
	if paperID == "" || kind == "" {
		return fmt.Errorf("paper ID and job kind are required")
	}
	now := time.Now().UTC()
	return c.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).
		Where("paper_id = ? AND kind = ? AND model = ? AND dim = ?", paperID, kind, qwenEmbeddingModel, qwenEmbeddingDim).
		Updates(map[string]any{
			"status":       QwenEmbeddingJobComplete,
			"lease_owner":  "",
			"lease_until":  nil,
			"last_error":   "",
			"completed_at": now,
			"updated_at":   now,
		}).Error
}

// FailQwenEmbeddingJob marks a job failed when its lease fence still matches.
func (c *Cache) FailQwenEmbeddingJob(ctx context.Context, jobID, leaseOwner string, generation int, err error) error {
	jobID = strings.TrimSpace(jobID)
	leaseOwner = trimForStorage(leaseOwner, 120)
	if jobID == "" || leaseOwner == "" || generation <= 0 {
		return fmt.Errorf("job ID, lease owner, and generation are required")
	}
	message := ""
	if err != nil {
		message = trimForStorage(err.Error(), 1000)
	}
	now := time.Now().UTC()
	result := c.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).
		Where("id = ? AND status = ? AND lease_owner = ? AND attempts = ? AND lease_until IS NOT NULL AND lease_until >= ?", jobID, QwenEmbeddingJobRunning, leaseOwner, generation, now).
		Updates(map[string]any{
			"status":       QwenEmbeddingJobFailed,
			"lease_until":  nil,
			"last_error":   message,
			"completed_at": nil,
			"updated_at":   now,
		})
	if result.Error != nil {
		return fmt.Errorf("fail qwen job: %w", result.Error)
	}
	if result.RowsAffected == 1 {
		return nil
	}
	var job QwenEmbeddingJob
	if err := c.db.WithContext(ctx).Where("id = ?", jobID).First(&job).Error; err == nil && job.LeaseOwner == leaseOwner && job.Attempts == generation && (job.Status == QwenEmbeddingJobFailed || job.Status == QwenEmbeddingJobComplete) {
		return nil
	}
	return fmt.Errorf("fail qwen job: %w", ErrQwenEmbeddingJobLeaseLost)
}

func (c *Cache) ensureQwenEmbeddingJob(ctx context.Context, tx *gorm.DB, paperID string, target qwenJobTarget, priority int, now time.Time) error {
	var job QwenEmbeddingJob
	err := tx.WithContext(ctx).
		Where("paper_id = ? AND kind = ? AND scope = ? AND model = ? AND dim = ?", paperID, target.kind, target.scope, target.model, target.dim).
		First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		job = QwenEmbeddingJob{
			ID:        "qjob_" + mustRandomToken(18),
			PaperID:   paperID,
			Kind:      target.kind,
			Scope:     target.scope,
			Model:     target.model,
			Dim:       target.dim,
			Status:    QwenEmbeddingJobQueued,
			Priority:  priority,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&job).Error; err != nil {
			return fmt.Errorf("create qwen job: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("load qwen job: %w", err)
	}

	updates := map[string]any{}
	if priority > job.Priority {
		updates["priority"] = priority
	}
	shouldRequeue := job.Status == QwenEmbeddingJobComplete ||
		(job.Status == QwenEmbeddingJobRunning && job.LeaseUntil != nil && job.LeaseUntil.Before(now))
	if job.Status == QwenEmbeddingJobFailed && job.Attempts < MaxQwenEmbeddingJobAttempts {
		retryDelay := time.Minute * time.Duration(1<<max(0, job.Attempts-1))
		shouldRequeue = !now.Before(job.UpdatedAt.Add(retryDelay))
	}
	if shouldRequeue {
		updates["status"] = QwenEmbeddingJobQueued
		updates["lease_owner"] = ""
		updates["lease_until"] = nil
		updates["last_error"] = ""
		updates["completed_at"] = nil
		updates["updated_at"] = now
	}
	if len(updates) == 0 {
		return nil
	}
	if err := tx.WithContext(ctx).Model(&QwenEmbeddingJob{}).Where("id = ?", job.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("update qwen job: %w", err)
	}
	return nil
}

func (c *Cache) countQwenChunkEmbeddings(ctx context.Context, paperID string) (int64, error) {
	if c.dbType == DBTypePostgres {
		sqlDB, err := c.db.DB()
		if err != nil {
			return 0, err
		}
		var count int64
		err = sqlDB.QueryRowContext(ctx, `
			SELECT count(*)
			FROM chunk_embeddings_v2 e
			JOIN paper_chunks c ON c.id = e.chunk_id
			WHERE c.paper_id = $1
			  AND c.scope = $2
			  AND e.model = $3
			  AND e.dim = $4
			  AND e.vector IS NOT NULL
			  AND e.source_hash = c.text_hash
		`, paperID, qwenPaperChunkScope, qwenEmbeddingModel, qwenEmbeddingDim).Scan(&count)
		if err != nil {
			return 0, fmt.Errorf("count qwen chunk embeddings: %w", err)
		}
		return count, nil
	}

	var count int64
	err := c.db.WithContext(ctx).
		Table("chunk_embeddings_v2 AS e").
		Joins("JOIN paper_chunks AS c ON c.id = e.chunk_id").
		Where("c.paper_id = ? AND c.scope = ? AND e.model = ? AND e.dim = ? AND e.source_hash = c.text_hash", paperID, qwenPaperChunkScope, qwenEmbeddingModel, qwenEmbeddingDim).
		Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("count qwen chunk embeddings: %w", err)
	}
	return count, nil
}

func (c *Cache) hasCurrentQwenAbstractEmbedding(ctx context.Context, paperID string) (bool, error) {
	if c.dbType != DBTypePostgres {
		return false, nil
	}
	var paper Paper
	if err := c.db.WithContext(ctx).Select("id", "title", "abstract").Where("id = ?", paperID).First(&paper).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("load paper for qwen readiness: %w", err)
	}
	title := strings.TrimSpace(paper.Title)
	abstract := strings.Join(strings.Fields(paper.Abstract), " ")
	text := title + abstract
	if title != "" && abstract != "" {
		text = title + ". " + abstract
	}
	if text == "" {
		return false, nil
	}
	sum := sha256.Sum256([]byte(text))
	var count int64
	if err := c.db.WithContext(ctx).Model(&EmbeddingV2{}).
		Where("paper_id = ? AND scope = ? AND model = ? AND dim = ? AND source_hash = ? AND vector IS NOT NULL", paperID, "abstract", qwenEmbeddingModel, qwenEmbeddingDim, fmt.Sprintf("%x", sum[:])).
		Count(&count).Error; err != nil {
		return false, fmt.Errorf("check qwen abstract readiness: %w", err)
	}
	return count > 0, nil
}

func normalizeQwenJobKinds(kinds []string) []string {
	allowed := map[string]bool{
		QwenEmbeddingJobKindAbstract:        true,
		QwenEmbeddingJobKindPaperChunks:     true,
		QwenEmbeddingJobKindChunkEmbeddings: true,
		QwenEmbeddingJobKindQuery:           true,
	}
	seen := make(map[string]bool, len(kinds))
	result := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if !allowed[kind] || seen[kind] {
			continue
		}
		seen[kind] = true
		result = append(result, kind)
	}
	return result
}
