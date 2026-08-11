package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchDownloadSelection(t *testing.T) {
	tests := []struct {
		name                string
		pdf, source, all    bool
		wantPDF, wantSource bool
	}{
		{name: "default source", wantSource: true},
		{name: "pdf only", pdf: true, wantPDF: true},
		{name: "source only", source: true, wantSource: true},
		{name: "both explicit", pdf: true, source: true, wantPDF: true, wantSource: true},
		{name: "all", all: true, wantPDF: true, wantSource: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotPDF, gotSource := fetchDownloadSelection(test.pdf, test.source, test.all)
			if gotPDF != test.wantPDF || gotSource != test.wantSource {
				t.Fatalf("selection = (%v, %v), want (%v, %v)", gotPDF, gotSource, test.wantPDF, test.wantSource)
			}
		})
	}
}

func TestFetchFailureErrorAggregatesPaperIDs(t *testing.T) {
	if err := fetchFailureError(nil); err != nil {
		t.Fatalf("empty failures returned %v", err)
	}
	err := fetchFailureError([]string{"one", "two"})
	if err == nil || !strings.Contains(err.Error(), "2 paper(s)") || !strings.Contains(err.Error(), "one, two") {
		t.Fatalf("error = %v", err)
	}
}

func TestCacheDirectory(t *testing.T) {
	dir, err := cacheDirectory("/custom/cache", func() (string, error) {
		return "", errors.New("must not be called")
	})
	if err != nil || dir != "/custom/cache" {
		t.Fatalf("configured cache = %q, %v", dir, err)
	}

	dir, err = cacheDirectory("", func() (string, error) { return "/home/tester", nil })
	if err != nil || dir != filepath.Join("/home/tester", ".cache", "arxiv") {
		t.Fatalf("default cache = %q, %v", dir, err)
	}
}

func TestCacheDirectoryReportsHomeError(t *testing.T) {
	errHome := errors.New("home unavailable")
	_, err := cacheDirectory("", func() (string, error) { return "", errHome })
	if !errors.Is(err, errHome) || !strings.Contains(err.Error(), "ARXIV_CACHE") {
		t.Fatalf("error = %v, want actionable home error", err)
	}
}

func TestRunCLIRejectsUnknownCommand(t *testing.T) {
	t.Setenv("ARXIV_CACHE", t.TempDir())
	err := runCLI(t.Context(), []string{"not-a-command"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("error = %v", err)
	}
}

func TestFindToolReportsMissingTool(t *testing.T) {
	_, err := findTool("definitely-not-an-arxiv-tool")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v", err)
	}
}
