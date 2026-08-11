package arxiv

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEnsureQwenPaperJobsIsIdempotent(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	status, err := cache.EnsureQwenPaperJobs(ctx, "2501.00001", 100)
	if err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	if status.QueuedJobs != 3 {
		t.Fatalf("queued jobs = %d, want 3; status=%#v", status.QueuedJobs, status)
	}

	status, err = cache.EnsureQwenPaperJobs(ctx, "2501.00001", 100)
	if err != nil {
		t.Fatalf("EnsureQwenPaperJobs second call: %v", err)
	}
	if status.QueuedJobs != 3 || len(status.Jobs) != 3 {
		t.Fatalf("second call queued=%d jobs=%d, want 3/3; status=%#v", status.QueuedJobs, len(status.Jobs), status)
	}

	var count int64
	if err := cache.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).
		Where("paper_id = ?", "2501.00001").
		Count(&count).Error; err != nil {
		t.Fatalf("count qwen jobs: %v", err)
	}
	if count != 3 {
		t.Fatalf("stored jobs = %d, want 3", count)
	}
}

func TestClaimCompleteAndFailQwenEmbeddingJobs(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00002", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}

	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindAbstract}, 1, "test-worker", time.Minute)
	if err != nil {
		t.Fatalf("ClaimQwenEmbeddingJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("claimed jobs = %d, want 1", len(jobs))
	}
	if jobs[0].Kind != QwenEmbeddingJobKindAbstract || jobs[0].Status != QwenEmbeddingJobRunning || jobs[0].Attempts != 1 {
		t.Fatalf("unexpected claimed job: %#v", jobs[0])
	}
	if err := cache.CompleteQwenEmbeddingJob(ctx, jobs[0].ID, jobs[0].LeaseOwner, jobs[0].Attempts); err != nil {
		t.Fatalf("CompleteQwenEmbeddingJob: %v", err)
	}
	if err := cache.CompleteQwenEmbeddingJob(ctx, jobs[0].ID, jobs[0].LeaseOwner, jobs[0].Attempts); err != nil {
		t.Fatalf("CompleteQwenEmbeddingJob retry: %v", err)
	}
	if err := cache.FailQwenEmbeddingJob(ctx, jobs[0].ID, jobs[0].LeaseOwner, jobs[0].Attempts, errors.New("ambiguous completion")); err != nil {
		t.Fatalf("FailQwenEmbeddingJob after completion: %v", err)
	}

	jobs, err = cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindPaperChunks}, 1, "test-worker", time.Minute)
	if err != nil {
		t.Fatalf("ClaimQwenEmbeddingJobs paper chunks: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("claimed paper chunk jobs = %d, want 1", len(jobs))
	}
	if err := cache.FailQwenEmbeddingJob(ctx, jobs[0].ID, jobs[0].LeaseOwner, jobs[0].Attempts, errors.New("boom")); err != nil {
		t.Fatalf("FailQwenEmbeddingJob: %v", err)
	}
	if err := cache.FailQwenEmbeddingJob(ctx, jobs[0].ID, jobs[0].LeaseOwner, jobs[0].Attempts, errors.New("boom again")); err != nil {
		t.Fatalf("FailQwenEmbeddingJob retry: %v", err)
	}

	status, err := cache.QwenPaperEmbeddingStatus(ctx, "2501.00002")
	if err != nil {
		t.Fatalf("QwenPaperEmbeddingStatus: %v", err)
	}
	if status.FailedJobs != 1 || status.RunningJobs != 0 {
		t.Fatalf("failed/running jobs = %d/%d, want 1/0; status=%#v", status.FailedJobs, status.RunningJobs, status)
	}
}

func TestFailedQwenJobsUseBackoffAndStopAfterThreeAttempts(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00012", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindAbstract}, 1, "test-worker", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%d err=%v", len(jobs), err)
	}
	job := jobs[0]
	if err := cache.FailQwenEmbeddingJob(ctx, job.ID, job.LeaseOwner, job.Attempts, errors.New("boom")); err != nil {
		t.Fatalf("fail job: %v", err)
	}
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00012", 50); err != nil {
		t.Fatalf("ensure during backoff: %v", err)
	}
	stored, err := cache.GetQwenEmbeddingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get backed-off job: %v", err)
	}
	if stored.Status != QwenEmbeddingJobFailed {
		t.Fatalf("status=%q, want failed during backoff", stored.Status)
	}

	if err := cache.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).Where("id = ?", job.ID).Updates(map[string]any{
		"attempts":   MaxQwenEmbeddingJobAttempts,
		"updated_at": time.Now().UTC().Add(-time.Hour),
	}).Error; err != nil {
		t.Fatalf("prepare exhausted job: %v", err)
	}
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00012", 50); err != nil {
		t.Fatalf("ensure exhausted job: %v", err)
	}
	stored, err = cache.GetQwenEmbeddingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get exhausted job: %v", err)
	}
	if stored.Status != QwenEmbeddingJobFailed || stored.Attempts != MaxQwenEmbeddingJobAttempts {
		t.Fatalf("exhausted job was recycled: %#v", stored)
	}
}

func TestQwenEmbeddingJobLeaseFenceRejectsStaleWorker(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00003", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}

	oldJobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindAbstract}, 1, "old-worker", time.Minute)
	if err != nil || len(oldJobs) != 1 {
		t.Fatalf("old claim jobs=%d err=%v", len(oldJobs), err)
	}
	oldJob := oldJobs[0]
	if err := cache.CompleteQwenEmbeddingJob(ctx, oldJob.ID, "other-worker", oldJob.Attempts); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("wrong-owner completion error = %v, want lease lost", err)
	}

	past := time.Now().UTC().Add(-time.Minute)
	if err := cache.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).Where("id = ?", oldJob.ID).Update("lease_until", past).Error; err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	newJobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindAbstract}, 1, "new-worker", time.Minute)
	if err != nil || len(newJobs) != 1 {
		t.Fatalf("new claim jobs=%d err=%v", len(newJobs), err)
	}
	newJob := newJobs[0]
	if newJob.Attempts != oldJob.Attempts+1 {
		t.Fatalf("new generation = %d, want %d", newJob.Attempts, oldJob.Attempts+1)
	}

	if err := cache.CompleteQwenEmbeddingJob(ctx, oldJob.ID, oldJob.LeaseOwner, oldJob.Attempts); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("stale completion error = %v, want lease lost", err)
	}
	if err := cache.FailQwenEmbeddingJob(ctx, oldJob.ID, oldJob.LeaseOwner, oldJob.Attempts, errors.New("late failure")); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("stale failure error = %v, want lease lost", err)
	}
	if err := cache.CompleteQwenEmbeddingJob(ctx, newJob.ID, newJob.LeaseOwner, newJob.Attempts); err != nil {
		t.Fatalf("new-worker completion: %v", err)
	}

	stored, err := cache.GetQwenEmbeddingJob(ctx, newJob.ID)
	if err != nil {
		t.Fatalf("GetQwenEmbeddingJob: %v", err)
	}
	if stored.Status != QwenEmbeddingJobComplete || stored.Attempts != newJob.Attempts {
		t.Fatalf("unexpected stored job: %#v", stored)
	}
}

func TestQwenEmbeddingJobLeaseMustBeUnexpired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00004", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindAbstract}, 1, "worker", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%d err=%v", len(jobs), err)
	}
	job := jobs[0]
	past := time.Now().UTC().Add(-time.Minute)
	if err := cache.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).Where("id = ?", job.ID).Update("lease_until", past).Error; err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if err := cache.CompleteQwenEmbeddingJob(ctx, job.ID, job.LeaseOwner, job.Attempts); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("expired completion error = %v, want lease lost", err)
	}
}

func TestRenewQwenEmbeddingJobLeaseIsFencedAndCapped(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00007", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindAbstract}, 1, "heartbeat-worker", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%d err=%v", len(jobs), err)
	}
	job := jobs[0]

	if _, err := cache.RenewQwenEmbeddingJobLease(ctx, job.ID, "other-worker", job.Attempts, time.Minute); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("wrong-owner renewal error = %v, want lease lost", err)
	}
	if _, err := cache.RenewQwenEmbeddingJobLease(ctx, job.ID, job.LeaseOwner, job.Attempts+1, time.Minute); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("wrong-generation renewal error = %v, want lease lost", err)
	}

	started := time.Now().UTC()
	renewed, err := cache.RenewQwenEmbeddingJobLease(ctx, job.ID, job.LeaseOwner, job.Attempts, 2*time.Hour)
	if err != nil {
		t.Fatalf("RenewQwenEmbeddingJobLease: %v", err)
	}
	if renewed.Status != QwenEmbeddingJobRunning || renewed.LeaseUntil == nil {
		t.Fatalf("unexpected renewed job: %#v", renewed)
	}
	if renewed.LeaseUntil.Before(started.Add(59*time.Minute)) || renewed.LeaseUntil.After(started.Add(time.Hour+5*time.Second)) {
		t.Fatalf("capped lease until = %v, started=%v", renewed.LeaseUntil, started)
	}

	stored, err := cache.GetQwenEmbeddingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetQwenEmbeddingJob: %v", err)
	}
	if stored.LeaseUntil == nil || !stored.LeaseUntil.Equal(*renewed.LeaseUntil) {
		t.Fatalf("stored lease=%v, renewed lease=%v", stored.LeaseUntil, renewed.LeaseUntil)
	}
	if err := cache.CompleteQwenEmbeddingJob(ctx, job.ID, job.LeaseOwner, job.Attempts); err != nil {
		t.Fatalf("CompleteQwenEmbeddingJob: %v", err)
	}
	if _, err := cache.RenewQwenEmbeddingJobLease(ctx, job.ID, job.LeaseOwner, job.Attempts, time.Minute); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("terminal renewal error = %v, want lease lost", err)
	}
}

func TestRenewQwenEmbeddingJobLeaseRejectsExpiredLease(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00008", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{QwenEmbeddingJobKindAbstract}, 1, "heartbeat-worker", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%d err=%v", len(jobs), err)
	}
	job := jobs[0]
	past := time.Now().UTC().Add(-time.Minute)
	if err := cache.db.WithContext(ctx).Model(&QwenEmbeddingJob{}).Where("id = ?", job.ID).Update("lease_until", past).Error; err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	if _, err := cache.RenewQwenEmbeddingJobLease(ctx, job.ID, job.LeaseOwner, job.Attempts, time.Minute); !errors.Is(err, ErrQwenEmbeddingJobLeaseLost) {
		t.Fatalf("expired renewal error = %v, want lease lost", err)
	}
}

func TestQwenPaperEmbeddingStatusRejectsStaleChunkHash(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	chunk := PaperChunk{ID: "chunk-1", PaperID: "2501.00006", Scope: qwenPaperChunkScope, Text: "current text", TextHash: "current"}
	if err := cache.db.WithContext(ctx).Create(&chunk).Error; err != nil {
		t.Fatalf("create chunk: %v", err)
	}
	embedding := ChunkEmbeddingV2{ChunkID: chunk.ID, Model: qwenEmbeddingModel, Dim: qwenEmbeddingDim, SourceHash: "stale"}
	if err := cache.db.WithContext(ctx).Create(&embedding).Error; err != nil {
		t.Fatalf("create chunk embedding: %v", err)
	}

	status, err := cache.QwenPaperEmbeddingStatus(ctx, chunk.PaperID)
	if err != nil {
		t.Fatalf("QwenPaperEmbeddingStatus stale: %v", err)
	}
	if status.ChunkEmbeddingCount != 0 || status.ChunkEmbeddingsReady {
		t.Fatalf("stale chunk embedding reported ready: %#v", status)
	}

	if err := cache.db.WithContext(ctx).Model(&ChunkEmbeddingV2{}).Where("chunk_id = ?", chunk.ID).Update("source_hash", chunk.TextHash).Error; err != nil {
		t.Fatalf("refresh chunk embedding hash: %v", err)
	}
	status, err = cache.QwenPaperEmbeddingStatus(ctx, chunk.PaperID)
	if err != nil {
		t.Fatalf("QwenPaperEmbeddingStatus current: %v", err)
	}
	if status.ChunkEmbeddingCount != 1 || !status.ChunkEmbeddingsReady {
		t.Fatalf("current chunk embedding not ready: %#v", status)
	}
}
