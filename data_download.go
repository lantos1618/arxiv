package arxiv

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	maxPDFDownloadBytes    = 100 * 1024 * 1024
	maxSourceDownloadBytes = 500 * 1024 * 1024
	maxSourceExtractBytes  = 1024 * 1024 * 1024
)

// DownloadOptions configures paper downloads.
type DownloadOptions struct {
	// Concurrency is the number of parallel downloads (default 1)
	Concurrency int

	// RateLimit is the delay between downloads (default 3s per arXiv guidelines)
	RateLimit time.Duration

	// DownloadPDF enables PDF downloads
	DownloadPDF bool

	// DownloadSource enables TeX source downloads
	DownloadSource bool

	// GenerateEmbedding generates embeddings in background after download
	GenerateEmbedding bool

	// Progress callback
	Progress func(paperID string, downloaded, total int)
}

func normalizedDownloadOptions(opts *DownloadOptions) DownloadOptions {
	if opts == nil {
		return DownloadOptions{
			Concurrency:    1,
			RateLimit:      3 * time.Second,
			DownloadPDF:    true,
			DownloadSource: true,
		}
	}
	normalized := *opts
	if normalized.Concurrency <= 0 {
		normalized.Concurrency = 1
	}
	if normalized.Concurrency > 16 {
		normalized.Concurrency = 16
	}
	if normalized.RateLimit == 0 {
		normalized.RateLimit = 3 * time.Second
	}
	if normalized.RateLimit < 0 {
		normalized.RateLimit = 0
	}
	return normalized
}

// DownloadPaper downloads PDF and/or source for a single paper.
func (c *Cache) DownloadPaper(ctx context.Context, paperID string, opts *DownloadOptions) error {
	normalized := normalizedDownloadOptions(opts)
	opts = &normalized

	paper, err := c.GetPaper(ctx, paperID)
	if err != nil {
		return fmt.Errorf("get paper: %w", err)
	}

	// Download or repair missing PDF
	if opts.DownloadPDF && (!paper.PDFDownloaded || !validPDFFile(paper.PDFPath)) {
		pdfPath, err := c.downloadPDF(ctx, paper)
		if err != nil {
			return fmt.Errorf("download pdf: %w", err)
		}
		if err := c.db.WithContext(ctx).Model(&Paper{}).Where("id = ?", paperID).
			Updates(map[string]interface{}{"pdf_path": pdfPath, "pdf_downloaded": true}).Error; err != nil {
			return fmt.Errorf("store pdf download state: %w", err)
		}
		// Keep in-memory cache consistent with the DB so handlers don't see stale paths.
		paper.PDFPath = pdfPath
		paper.PDFDownloaded = true
		c.paperLRU.Put(paperID, paper)

	}

	if opts.DownloadSource && (!paper.SourceDownloaded || !validSourceDir(paper.SourcePath)) {
		srcPath, err := c.downloadSource(ctx, paper)
		if err != nil {
			return fmt.Errorf("download source: %w", err)
		}
		if err := c.db.WithContext(ctx).Model(&Paper{}).Where("id = ?", paperID).
			Updates(map[string]interface{}{"src_path": srcPath, "src_downloaded": true}).Error; err != nil {
			return fmt.Errorf("store source download state: %w", err)
		}
		paper.SourcePath = srcPath
		paper.SourceDownloaded = true
		c.paperLRU.Put(paperID, paper)

		// Extract and store citations
		if err := c.UpdateCitations(ctx, paperID, srcPath); err != nil {
			// Non-fatal: log but don't fail the download
			fmt.Printf("Warning: failed to extract citations for %s: %v\n", paperID, err)
		}
	}

	if opts.GenerateEmbedding {
		if err := c.QueueEmbedding(ctx, paperID, 0); err != nil {
			return fmt.Errorf("queue embedding: %w", err)
		}
	}

	return nil
}

func (c *Cache) downloadPapers(ctx context.Context, ids []string, opts DownloadOptions) error {
	if len(ids) == 0 {
		return nil
	}

	type result struct {
		id  string
		err error
	}
	jobs := make(chan string)
	results := make(chan result)
	var workers sync.WaitGroup

	var starts <-chan struct{}
	var limiterDone chan struct{}
	if opts.RateLimit > 0 {
		startGate := make(chan struct{}, 1)
		startGate <- struct{}{}
		starts = startGate
		limiterDone = make(chan struct{})
		defer close(limiterDone)
		go func() {
			ticker := time.NewTicker(opts.RateLimit)
			defer ticker.Stop()
			for {
				select {
				case <-limiterDone:
					return
				case <-ticker.C:
					select {
					case startGate <- struct{}{}:
					default:
					}
				}
			}
		}()
	}

	worker := func() {
		defer workers.Done()
		for id := range jobs {
			if starts != nil {
				select {
				case <-ctx.Done():
					results <- result{id: id, err: ctx.Err()}
					continue
				case <-starts:
				}
			}
			err := c.DownloadPaper(ctx, id, &opts)
			results <- result{id: id, err: err}
		}
	}
	workers.Add(opts.Concurrency)
	for i := 0; i < opts.Concurrency; i++ {
		go worker()
	}
	go func() {
		defer close(jobs)
		for _, id := range ids {
			select {
			case <-ctx.Done():
				return
			case jobs <- id:
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	completed := 0
	var errs []error
	for result := range results {
		completed++
		if opts.Progress != nil {
			opts.Progress(result.id, completed, len(ids))
		}
		if result.err != nil {
			errs = append(errs, fmt.Errorf("download %s: %w", result.id, result.err))
		}
	}
	if err := ctx.Err(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// GetPaper retrieves a paper by ID, using LRU cache when available.
func (c *Cache) GetPaper(ctx context.Context, id string) (*Paper, error) {
	// Check LRU cache first
	if cached, ok := c.paperLRU.Get(id); ok {
		if paper, ok := cached.(*Paper); ok {
			paperCopy := *paper
			return &paperCopy, nil
		}
	}

	var p Paper
	if err := c.db.WithContext(ctx).Omit("pdf_text").Where("id = ?", id).First(&p).Error; err != nil {
		return nil, err
	}

	// Cache the result
	cached := p
	c.paperLRU.Put(id, &cached)

	return &p, nil
}

// GetPaperFresh bypasses the LRU cache to force a DB read, then refreshes the cache.
func (c *Cache) GetPaperFresh(ctx context.Context, id string) (*Paper, error) {
	var p Paper
	if err := c.db.WithContext(ctx).Omit("pdf_text").Where("id = ?", id).First(&p).Error; err != nil {
		return nil, err
	}
	cached := p
	c.paperLRU.Put(id, &cached)
	return &p, nil
}

func (c *Cache) downloadPDF(ctx context.Context, paper *Paper) (string, error) {
	prefix := paperPrefix(paper.ID)
	dir := filepath.Join(c.root, "pdf", prefix)

	path := filepath.Join(dir, paper.ID+".pdf")
	if validPDFFile(path) {
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}

	resp, err := httpGetWithContext(ctx, paper.PDFURL())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %s", resp.Status)
	}

	f, err := os.CreateTemp(dir, paper.ID+"-*.pdf.part")
	if err != nil {
		return "", err
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)

	written, err := io.Copy(f, io.LimitReader(resp.Body, maxPDFDownloadBytes+1))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if written > maxPDFDownloadBytes {
		return "", fmt.Errorf("PDF exceeds %d bytes", maxPDFDownloadBytes)
	}
	if !validPDFFile(tmpPath) {
		return "", fmt.Errorf("downloaded file is not a valid PDF")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", err
	}

	return path, nil
}

func (c *Cache) downloadSource(ctx context.Context, paper *Paper) (string, error) {
	prefix := paperPrefix(paper.ID)
	dir := filepath.Join(c.root, "src", prefix)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	// Source files are typically gzipped tar archives
	srcDir := filepath.Join(dir, paper.ID)
	if validSourceDir(srcDir) {
		return srcDir, nil // Already exists
	}

	resp, err := httpGetWithContext(ctx, paper.SourceURL())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("http %s", resp.Status)
	}

	// Create temp file to determine content type
	tmpFile, err := os.CreateTemp("", "arxiv-src-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	written, err := io.Copy(tmpFile, io.LimitReader(resp.Body, maxSourceDownloadBytes+1))
	closeErr := tmpFile.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > maxSourceDownloadBytes {
		return "", fmt.Errorf("source exceeds %d bytes", maxSourceDownloadBytes)
	}

	tmpDir, err := os.MkdirTemp(dir, strings.NewReplacer("/", "-", "\\", "-").Replace(paper.ID)+"-*.part")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	// Try to extract as gzipped tar
	if err := extractSource(tmpPath, tmpDir); err != nil {
		// If extraction fails, it might be a single TeX file
		// Copy it directly
		data, err := os.ReadFile(tmpPath)
		if err != nil {
			return "", err
		}
		mainTex := filepath.Join(tmpDir, "main.tex")
		if err := os.WriteFile(mainTex, data, 0644); err != nil {
			return "", err
		}
	}
	if !validSourceDir(tmpDir) {
		return "", fmt.Errorf("downloaded source is empty")
	}
	if _, err := os.Stat(srcDir); err == nil {
		if validSourceDir(srcDir) {
			return srcDir, nil
		}
		if err := os.RemoveAll(srcDir); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmpDir, srcDir); err != nil {
		return "", err
	}

	return srcDir, nil
}

func extractSource(srcPath, dstDir string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	// Try gzip first
	gzr, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gzr.Close()

	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}

	tr := tar.NewReader(gzr)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Security: prevent path traversal
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			continue
		}

		target := filepath.Join(dstDir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > 100*1024*1024 || total+hdr.Size > maxSourceExtractBytes {
				return fmt.Errorf("source archive exceeds extraction limits")
			}
			total += hdr.Size
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			// Limit file size to 100MB
			_, err = io.CopyN(outFile, tr, hdr.Size)
			closeErr := outFile.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}

	return nil
}

func validPDFFile(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() < 8 {
		return false
	}
	header := make([]byte, 5)
	_, err = io.ReadFull(f, header)
	return err == nil && string(header) == "%PDF-"
}

func validSourceDir(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) > 0
}

func paperPrefix(id string) string {
	// Handle both new format (2301.00001) and old format (hep-th/9901001)
	if strings.Contains(id, "/") {
		parts := strings.Split(id, "/")
		return parts[0]
	}
	if len(id) >= 4 {
		return id[:4]
	}
	return id
}

func httpGetWithContext(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}
