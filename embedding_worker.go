package arxiv

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EmbeddingWorkerConfig configures the retired MiniLM background worker.
// Deprecated: Qwen embedding jobs are the canonical runtime pipeline.
type EmbeddingWorkerConfig struct {
	// ServiceURL is the URL of the embedding service (e.g., "http://localhost:8001")
	ServiceURL string
	// MutationToken authorizes database-writing embedding service requests.
	MutationToken string
	// BatchSize is the number of papers to process in each batch
	BatchSize int
	// PollInterval is how often to check for new papers to embed
	PollInterval time.Duration
	// MaxRetries is the maximum number of retries for failed embeddings
	MaxRetries int
	// Enabled controls whether the worker runs
	Enabled bool
}

// DefaultEmbeddingWorkerConfig returns a disabled legacy configuration.
// Deprecated: use the Qwen job pipeline.
func DefaultEmbeddingWorkerConfig() EmbeddingWorkerConfig {
	return EmbeddingWorkerConfig{
		ServiceURL:   "http://localhost:8001",
		BatchSize:    32,
		PollInterval: 10 * time.Second,
		MaxRetries:   3,
		Enabled:      false,
	}
}

// EmbeddingWorker processes legacy MiniLM jobs when explicitly enabled.
// Deprecated: retained for offline migration only.
type EmbeddingWorker struct {
	cache               *Cache
	config              EmbeddingWorkerConfig
	client              *http.Client
	mu                  sync.RWMutex
	running             bool
	stopping            bool
	stats               EmbeddingWorkerStats
	pendingCountUpdated time.Time
	stopChan            chan struct{}
	doneChan            chan struct{}
}

// EmbeddingWorkerStats tracks worker statistics.
type EmbeddingWorkerStats struct {
	Processed int64     `json:"processed"`
	Failed    int64     `json:"failed"`
	Pending   int64     `json:"pending"`
	LastRun   time.Time `json:"lastRun"`
	LastError string    `json:"lastError,omitempty"`
	IsRunning bool      `json:"isRunning"`
	ServiceUp bool      `json:"serviceUp"`
}

// QwenPipelineStats reports only the canonical Qwen profile and full-paper pipeline.
type QwenPipelineStats struct {
	AbstractProfiles         int64 `json:"abstractProfiles"`
	FullPaperChunks          int64 `json:"fullPaperChunks"`
	FullPaperChunkEmbeddings int64 `json:"fullPaperChunkEmbeddings"`
}

// QwenPipelineStats returns current Qwen coverage without consulting legacy MiniLM tables.
func (c *Cache) QwenPipelineStats(ctx context.Context) (*QwenPipelineStats, error) {
	if c == nil || c.db == nil {
		return nil, fmt.Errorf("cache unavailable")
	}
	stats := &QwenPipelineStats{}
	if c.dbType != DBTypePostgres {
		return stats, nil
	}
	if err := c.db.WithContext(ctx).Model(&EmbeddingV2{}).
		Where("scope = ? AND model = ? AND dim = ? AND vector IS NOT NULL", "abstract", qwenEmbeddingModel, qwenEmbeddingDim).
		Count(&stats.AbstractProfiles).Error; err != nil {
		return nil, fmt.Errorf("count Qwen abstract profiles: %w", err)
	}
	if err := c.db.WithContext(ctx).Model(&PaperChunk{}).
		Where("scope = ? AND COALESCE(text, '') <> ''", qwenPaperChunkScope).
		Count(&stats.FullPaperChunks).Error; err != nil {
		return nil, fmt.Errorf("count full-paper chunks: %w", err)
	}
	if err := c.db.WithContext(ctx).Table("chunk_embeddings_v2 AS e").
		Joins("JOIN paper_chunks AS c ON c.id = e.chunk_id").
		Where("c.scope = ? AND e.model = ? AND e.dim = ? AND e.vector IS NOT NULL AND e.source_hash = c.text_hash", qwenPaperChunkScope, qwenEmbeddingModel, qwenEmbeddingDim).
		Count(&stats.FullPaperChunkEmbeddings).Error; err != nil {
		return nil, fmt.Errorf("count current full-paper Qwen embeddings: %w", err)
	}
	return stats, nil
}

// NewEmbeddingWorker creates a legacy MiniLM worker.
// Deprecated: retained for offline migration only.
func NewEmbeddingWorker(cache *Cache, config EmbeddingWorkerConfig) *EmbeddingWorker {
	return &EmbeddingWorker{
		cache:  cache,
		config: config,
		client: &http.Client{
			Timeout: 60 * time.Second, // Allow time for batch processing
		},
		stopChan: make(chan struct{}),
		doneChan: make(chan struct{}),
	}
}

// Start begins the background worker.
func (w *EmbeddingWorker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	if !w.config.Enabled {
		log.Println("Legacy MiniLM embedding worker disabled; Qwen is canonical")
		return
	}
	if w.cache == nil {
		w.mu.Lock()
		w.stats.LastError = "legacy worker requires a cache"
		w.mu.Unlock()
		log.Println("Legacy MiniLM embedding worker not started: cache unavailable")
		return
	}

	w.mu.Lock()
	if w.running || w.stopping {
		w.mu.Unlock()
		return
	}
	w.stopChan = make(chan struct{})
	w.doneChan = make(chan struct{})
	w.running = true
	w.stats.IsRunning = true
	w.mu.Unlock()

	log.Printf("Starting embedding worker (service: %s, batch: %d, poll: %v)",
		w.config.ServiceURL, w.config.BatchSize, w.config.PollInterval)

	go w.run(ctx)
}

// Stop gracefully stops the worker.
func (w *EmbeddingWorker) Stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if !w.running && !w.stopping {
		w.mu.Unlock()
		return
	}
	if !w.stopping {
		close(w.stopChan)
		w.stopping = true
	}
	done := w.doneChan
	w.mu.Unlock()

	<-done

	log.Println("Embedding worker stopped")
}

// Stats returns current worker statistics.
func (w *EmbeddingWorker) Stats() EmbeddingWorkerStats {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.stats
}

// run is the main worker loop.
func (w *EmbeddingWorker) run(ctx context.Context) {
	defer func() {
		w.mu.Lock()
		w.running = false
		w.stopping = false
		w.stats.IsRunning = false
		done := w.doneChan
		w.mu.Unlock()
		close(done)
	}()

	for {
		processed := w.processBatch(ctx)
		nextPoll := w.config.PollInterval
		if !processed {
			nextPoll = 5 * time.Minute
		}

		timer := time.NewTimer(nextPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-w.stopChan:
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// processBatch processes a batch of papers without embeddings.
func (w *EmbeddingWorker) processBatch(ctx context.Context) bool {
	// Check if embedding service is available
	if !w.checkServiceHealth() {
		w.mu.Lock()
		w.stats.ServiceUp = false
		w.stats.LastError = "embedding service unavailable"
		w.mu.Unlock()
		return false
	}

	w.mu.Lock()
	w.stats.ServiceUp = true
	w.stats.LastRun = time.Now()
	w.mu.Unlock()

	// Claim queued papers in priority order.
	papers, err := w.getPendingPapers(ctx, w.config.BatchSize)
	if err != nil {
		log.Printf("Error getting pending papers: %v", err)
		w.mu.Lock()
		w.stats.LastError = err.Error()
		w.mu.Unlock()
		return false
	}

	if len(papers) == 0 {
		w.mu.Lock()
		w.stats.Pending = 0
		w.stats.LastError = ""
		w.mu.Unlock()
		return false
	}

	pendingCount := w.currentPendingEstimate(ctx)
	w.mu.Lock()
	w.stats.Pending = pendingCount
	w.mu.Unlock()

	// Send to embedding service
	reportedSuccess, reportedFailed := w.embedPapers(ctx, papers)
	success, failed := w.finishEmbeddingJobs(ctx, papers, reportedSuccess, reportedFailed)

	w.mu.Lock()
	w.stats.Processed += int64(success)
	w.stats.Failed += int64(failed)
	if w.stats.Pending > 0 {
		w.stats.Pending = maxInt64(w.stats.Pending-int64(success), 0)
	}
	if failed > 0 {
		w.stats.LastError = fmt.Sprintf("%d papers failed in last batch", failed)
	} else {
		w.stats.LastError = ""
	}
	w.mu.Unlock()

	if success > 0 {
		log.Printf("Embedded %d papers (%d failed, %d pending)", success, failed, pendingCount-int64(success))
	}
	return true
}

func (w *EmbeddingWorker) currentPendingEstimate(ctx context.Context) int64 {
	w.mu.RLock()
	pending := w.stats.Pending
	fresh := !w.pendingCountUpdated.IsZero() && time.Since(w.pendingCountUpdated) < 10*time.Minute
	w.mu.RUnlock()
	if fresh && pending > 0 {
		return pending
	}

	count, err := w.countPendingPapers(ctx)
	if err != nil {
		return pending
	}
	w.mu.Lock()
	w.pendingCountUpdated = time.Now()
	w.mu.Unlock()
	return count
}

// checkServiceHealth checks if the embedding service is responding.
func (w *EmbeddingWorker) checkServiceHealth() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", w.config.ServiceURL+"/health", nil)
	if err != nil {
		return false
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	var health struct {
		Ready bool `json:"ready"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return false
	}

	return health.Ready
}

// getPendingPapers gets papers that need embeddings.
func (w *EmbeddingWorker) getPendingPapers(ctx context.Context, limit int) ([]Paper, error) {
	if w == nil || w.cache == nil {
		return nil, fmt.Errorf("legacy embedding worker cache unavailable")
	}
	var papers []Paper
	err := w.cache.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		staleBefore := time.Now().Add(-10 * time.Minute)
		if err := tx.Model(&EmbeddingJob{}).
			Where("status = ? AND updated_at < ?", EmbeddingJobProcessing, staleBefore).
			Updates(map[string]any{"status": EmbeddingJobFailed, "last_error": "legacy worker lease expired"}).Error; err != nil {
			return err
		}
		retryBefore := time.Now().Add(-time.Minute)
		query := `
			SELECT p.id, p.title, p.abstract
			FROM embedding_jobs j
			JOIN papers p ON p.id = j.paper_id
			WHERE j.status IN ('pending', 'failed') AND j.attempts < ?
			  AND (j.status = 'pending' OR j.updated_at <= ?)
			  AND p.title != '' AND p.abstract != ''
			  AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.paper_id = p.id)
			ORDER BY j.priority DESC, j.created_at, j.paper_id
			LIMIT ?`
		if w.cache.dbType == DBTypePostgres {
			query += " FOR UPDATE OF j SKIP LOCKED"
		}
		if err := tx.Raw(query, w.config.MaxRetries, retryBefore, limit).Scan(&papers).Error; err != nil {
			return err
		}
		if len(papers) == 0 {
			return nil
		}
		ids := make([]string, len(papers))
		for i := range papers {
			ids[i] = papers[i].ID
		}
		return tx.Model(&EmbeddingJob{}).Where("paper_id IN ?", ids).Updates(map[string]any{
			"status": EmbeddingJobProcessing, "attempts": gorm.Expr("attempts + 1"), "last_error": "",
		}).Error
	})
	return papers, err
}

func (w *EmbeddingWorker) finishEmbeddingJobs(ctx context.Context, papers []Paper, reportedSuccess, reportedFailed int) (success, failed int) {
	now := time.Now()
	for i := range papers {
		updates := map[string]any{"updated_at": now}
		if w.cache.HasEmbedding(ctx, papers[i].ID) {
			updates["status"] = EmbeddingJobCompleted
			updates["last_error"] = ""
			success++
		} else {
			updates["status"] = EmbeddingJobFailed
			updates["last_error"] = fmt.Sprintf("legacy service reported %d processed/%d skipped but no embedding postcondition was found", reportedSuccess, reportedFailed)
			failed++
		}
		if err := w.cache.db.WithContext(ctx).Model(&EmbeddingJob{}).Where("paper_id = ?", papers[i].ID).Updates(updates).Error; err != nil {
			log.Printf("Error updating embedding job %s: %v", papers[i].ID, err)
		}
	}
	return success, failed
}

// countPendingPapers counts papers without embeddings.
func (w *EmbeddingWorker) countPendingPapers(ctx context.Context) (int64, error) {
	if w == nil || w.cache == nil {
		return 0, fmt.Errorf("legacy embedding worker cache unavailable")
	}
	var count int64
	err := w.cache.db.WithContext(ctx).
		Raw(`
			SELECT COUNT(*)
			FROM embedding_jobs j
			JOIN papers p ON p.id = j.paper_id
			WHERE j.status IN ('pending', 'failed') AND j.attempts < ?
			  AND p.title != '' AND p.abstract != ''
			  AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.paper_id = p.id)
		`, w.config.MaxRetries).
		Scan(&count).Error
	return count, err
}

// paperEmbedRequest matches the FastAPI request schema.
type paperEmbedRequest struct {
	PaperID  string `json:"paper_id"`
	Title    string `json:"title"`
	Abstract string `json:"abstract"`
}

type papersEmbedRequest struct {
	Papers []paperEmbedRequest `json:"papers"`
}

type papersEmbedResponse struct {
	Success   bool   `json:"success"`
	Processed int    `json:"processed"`
	Skipped   int    `json:"skipped"`
	Message   string `json:"message"`
}

// embedPapers sends papers to the embedding service for processing.
func (w *EmbeddingWorker) embedPapers(ctx context.Context, papers []Paper) (success, failed int) {
	if len(papers) == 0 {
		return 0, 0
	}

	// Prepare request
	req := papersEmbedRequest{
		Papers: make([]paperEmbedRequest, len(papers)),
	}
	for i, p := range papers {
		req.Papers[i] = paperEmbedRequest{
			PaperID:  p.ID,
			Title:    p.Title,
			Abstract: p.Abstract,
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		log.Printf("Error marshaling embed request: %v", err)
		return 0, len(papers)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", w.config.ServiceURL+"/embed/papers", bytes.NewReader(body))
	if err != nil {
		log.Printf("Error creating embed request: %v", err)
		return 0, len(papers)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if w.config.MutationToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+w.config.MutationToken)
	}

	resp, err := w.client.Do(httpReq)
	if err != nil {
		log.Printf("Error calling embed service: %v", err)
		return 0, len(papers)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("Embed service error (status %d): %s", resp.StatusCode, string(body))
		return 0, len(papers)
	}

	var result papersEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("Error decoding embed response: %v", err)
		return 0, len(papers)
	}

	return result.Processed, result.Skipped
}

// QueueEmbedding adds a paper to the retired MiniLM queue.
// Deprecated: use EnsureQwenPaperJobs.
func (c *Cache) QueueEmbedding(ctx context.Context, paperID string, priority int) error {
	job := EmbeddingJob{
		PaperID:  paperID,
		Status:   EmbeddingJobPending,
		Priority: priority,
	}

	return c.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "paper_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"priority", "updated_at"}),
		}).
		Create(&job).Error
}

// QueueAllPendingEmbeddings queues retired MiniLM work.
// Deprecated: use Qwen jobs.
func (c *Cache) QueueAllPendingEmbeddings(ctx context.Context) (int64, error) {
	result := c.db.WithContext(ctx).Exec(`
		INSERT INTO embedding_jobs (paper_id, status, priority, created_at, updated_at)
		SELECT p.id, 'pending', 0, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
		FROM papers p
		WHERE p.title != '' AND p.abstract != ''
		AND NOT EXISTS (SELECT 1 FROM embeddings e WHERE e.paper_id = p.id)
		AND NOT EXISTS (SELECT 1 FROM embedding_jobs j WHERE j.paper_id = p.id)
	`)
	return result.RowsAffected, result.Error
}

// EmbeddingJobStats returns retired MiniLM queue statistics.
// Deprecated: use Qwen pipeline status.
func (c *Cache) EmbeddingJobStats(ctx context.Context) (map[string]int64, error) {
	type statusCount struct {
		Status string
		Count  int64
	}

	var counts []statusCount
	err := c.db.WithContext(ctx).
		Model(&EmbeddingJob{}).
		Select("status, COUNT(*) as count").
		Group("status").
		Scan(&counts).Error

	if err != nil {
		return nil, err
	}

	result := make(map[string]int64)
	for _, c := range counts {
		result[c.Status] = c.Count
	}

	return result, nil
}
