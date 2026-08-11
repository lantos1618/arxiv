package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	arxiv "github.com/lantos1618/arxiv.gg"
)

func TestQwenWorkerLeasedJobIDRoundTrip(t *testing.T) {
	leasedID := qwenWorkerLeasedJobID("qjob_example", 7, "abc123")
	jobID, generation, sourceHash := parseQwenWorkerLeasedJobID(leasedID)
	if jobID != "qjob_example" || generation != 7 || sourceHash != "abc123" {
		t.Fatalf("parsed %q as %q/%d/%q", leasedID, jobID, generation, sourceHash)
	}

	jobID, generation, sourceHash = parseQwenWorkerLeasedJobID("qjob_legacy")
	if jobID != "qjob_legacy" || generation != 0 || sourceHash != "" {
		t.Fatalf("legacy ID parsed as %q/%d/%q", jobID, generation, sourceHash)
	}
}

func TestQwenWorkerBodyIsBoundedAndRejectsTrailingJSON(t *testing.T) {
	for _, body := range []string{
		`{"limit":1}{"limit":2}`,
		`{"leaseOwner":"` + strings.Repeat("x", (128<<10)+1) + `"}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/qwen/jobs/claim", strings.NewReader(body))
		rec := httptest.NewRecorder()
		var destination qwenWorkerClaimRequest
		if err := decodeQwenWorkerBody(rec, req, &destination, false); err == nil {
			t.Fatalf("decodeQwenWorkerBody accepted invalid body of %d bytes", len(body))
		}
	}
}

func TestQwenWorkerFailAcceptsExistingPythonPayload(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00005", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{arxiv.QwenEmbeddingJobKindAbstract}, 1, "python-worker", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%d err=%v", len(jobs), err)
	}
	job := jobs[0]

	body, err := json.Marshal(map[string]string{"error": "model failed"})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/qwen/jobs/"+qwenWorkerLeasedJobID(job.ID, job.Attempts, "source-hash")+"/fail", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	(&server{cache: cache, localMode: true}).handleAPIQwenJobAction(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	stored, err := cache.GetQwenEmbeddingJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetQwenEmbeddingJob: %v", err)
	}
	if stored.Status != arxiv.QwenEmbeddingJobFailed || stored.LastError != "model failed" {
		t.Fatalf("unexpected stored job: %#v", stored)
	}
}

func TestQwenWorkerCanResolveAmbiguousCompletionWithGet(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00007", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{arxiv.QwenEmbeddingJobKindAbstract}, 1, "python-worker", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%d err=%v", len(jobs), err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/qwen/jobs/"+jobs[0].ID, nil)
	recorder := httptest.NewRecorder()
	(&server{cache: cache, localMode: true}).handleAPIQwenJobAction(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Job arxiv.QwenEmbeddingJob `json:"job"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success || response.Data.Job.ID != jobs[0].ID || response.Data.Job.Status != arxiv.QwenEmbeddingJobRunning {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestQwenWorkerActionRequiresLeaseGeneration(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/qwen/jobs/qjob_legacy/fail", bytes.NewBufferString(`{"error":"failed"}`))
	rec := httptest.NewRecorder()
	(&server{localMode: true}).handleAPIQwenJobAction(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestQwenWorkerHeartbeatRenewsAndCapsLease(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	cache, err := arxiv.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open cache: %v", err)
	}
	defer cache.Close()

	ctx := context.Background()
	if _, err := cache.EnsureQwenPaperJobs(ctx, "2501.00009", 50); err != nil {
		t.Fatalf("EnsureQwenPaperJobs: %v", err)
	}
	jobs, err := cache.ClaimQwenEmbeddingJobs(ctx, []string{arxiv.QwenEmbeddingJobKindAbstract}, 1, "python-worker", time.Minute)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("claim jobs=%d err=%v", len(jobs), err)
	}
	job := jobs[0]

	body, err := json.Marshal(map[string]interface{}{
		"leaseOwner":      job.LeaseOwner,
		"leaseGeneration": job.Attempts,
		"leaseSeconds":    99999,
	})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	started := time.Now().UTC()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/qwen/jobs/"+qwenWorkerLeasedJobID(job.ID, job.Attempts, "source-hash")+"/heartbeat", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	(&server{cache: cache, localMode: true}).handleAPIQwenJobAction(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var response struct {
		Success bool `json:"success"`
		Data    struct {
			JobID           string                       `json:"jobId"`
			Status          arxiv.QwenEmbeddingJobStatus `json:"status"`
			LeaseOwner      string                       `json:"leaseOwner"`
			LeaseGeneration int                          `json:"leaseGeneration"`
			LeaseUntil      time.Time                    `json:"leaseUntil"`
			LeaseSeconds    int                          `json:"leaseSeconds"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success || response.Data.JobID != job.ID || response.Data.Status != arxiv.QwenEmbeddingJobRunning || response.Data.LeaseOwner != job.LeaseOwner || response.Data.LeaseGeneration != job.Attempts {
		t.Fatalf("unexpected response: %#v", response)
	}
	if response.Data.LeaseSeconds != maxQwenWorkerLeaseSeconds {
		t.Fatalf("leaseSeconds=%d, want %d", response.Data.LeaseSeconds, maxQwenWorkerLeaseSeconds)
	}
	if response.Data.LeaseUntil.Before(started.Add(59*time.Minute)) || response.Data.LeaseUntil.After(started.Add(time.Hour+5*time.Second)) {
		t.Fatalf("leaseUntil=%v, started=%v", response.Data.LeaseUntil, started)
	}
}

func TestQwenWorkerHeartbeatRejectsFenceMismatch(t *testing.T) {
	body := bytes.NewBufferString(`{"leaseOwner":"worker","leaseGeneration":2,"leaseSeconds":60}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/qwen/jobs/qjob_example~1~source/heartbeat", body)
	rec := httptest.NewRecorder()
	(&server{localMode: true}).handleAPIQwenJobAction(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
