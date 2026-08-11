package arxiv

import (
	"context"
	"testing"
)

func TestLegacyEmbeddingWorkerDefaultsDisabledAndDoesNotPanicWithoutCache(t *testing.T) {
	config := DefaultEmbeddingWorkerConfig()
	if config.Enabled {
		t.Fatal("legacy MiniLM worker must default to disabled")
	}
	config.Enabled = true
	worker := NewEmbeddingWorker(nil, config)
	worker.Start(context.Background())
	if worker.Stats().LastError == "" {
		t.Fatal("missing-cache start should report why the legacy worker did not run")
	}
	worker.Stop()
}
