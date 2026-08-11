package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lantos1618/arxiv.gg"
)

func main() {
	log.SetFlags(0)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runCLI(ctx, os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func runCLI(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		usage()
		return fmt.Errorf("command required")
	}

	cacheDir, err := cacheDirectory(os.Getenv("ARXIV_CACHE"), os.UserHomeDir)
	if err != nil {
		return err
	}

	cmd := argv[0]
	args := argv[1:]

	switch cmd {
	case "fetch":
		return cmdFetch(ctx, cacheDir, args)
	case "sync":
		return cmdSync(ctx, cacheDir, args)
	case "stats":
		return cmdStats(ctx, cacheDir, args)
	case "search":
		return cmdSearch(ctx, cacheDir, args)
	case "get":
		return cmdGet(ctx, cacheDir, args)
	case "list", "ls":
		return cmdList(ctx, cacheDir, args)
	case "reindex":
		return cmdReindex(ctx, cacheDir, args)
	case "serve":
		cmdServe(ctx, cacheDir, args)
		return nil
	case "help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

func cacheDirectory(configured string, userHomeDir func() (string, error)) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return configured, nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine home directory (set ARXIV_CACHE to override): %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("determine home directory: empty path (set ARXIV_CACHE to override)")
	}
	return filepath.Join(home, ".cache", "arxiv"), nil
}

func usage() {
	fmt.Print(usageText)
}

func cmdFetch(ctx context.Context, cacheDir string, args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	pdf := fs.Bool("pdf", false, "Download PDF")
	source := fs.Bool("source", false, "Download TeX source")
	all := fs.Bool("all", false, "Download both PDF and source")
	withEmbedding := fs.Bool("with-embedding", false, "Queue the canonical Qwen paper profile")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() == 0 {
		return fmt.Errorf("usage: arxiv fetch [options] <paper-id> [paper-id...]")
	}

	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	downloadPDF, downloadSource := fetchDownloadSelection(*pdf, *source, *all)
	opts := &arxiv.DownloadOptions{
		DownloadPDF:    downloadPDF,
		DownloadSource: downloadSource,
	}

	var failures []string
	for _, id := range fs.Args() {
		fmt.Printf("Fetching %s...\n", id)

		paper, err := cache.FetchAndDownload(ctx, id, opts)
		if err != nil {
			log.Printf("  error: %v", err)
			failures = append(failures, id)
			continue
		}

		fmt.Printf("  Title: %s\n", paper.Title)
		fmt.Printf("  Authors: %s\n", paper.Authors)
		if paper.SourcePath != "" {
			fmt.Printf("  Source: %s\n", paper.SourcePath)
		}
		if paper.PDFPath != "" {
			fmt.Printf("  PDF: %s\n", paper.PDFPath)
		}
		if opts.DownloadPDF {
			text, err := cache.EnsurePDFText(ctx, id)
			if err != nil {
				log.Printf("  PDF text: %v", err)
			} else {
				fmt.Printf("  PDF text: %d chars\n", len(text))
			}
		}
		if *withEmbedding {
			fmt.Printf("  Qwen profile: queueing...\n")
			status, err := cache.EnsureQwenPaperJobs(ctx, id, 100)
			if err != nil {
				log.Printf("  embedding error: %v", err)
				failures = append(failures, id)
				continue
			}
			fmt.Printf("  Qwen profile: %d queued, %d running\n", status.QueuedJobs, status.RunningJobs)
		}
		fmt.Println()

		// Rate limit between papers
		if len(fs.Args()) > 1 {
			timer := time.NewTimer(3 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fetchFailureError(failures)
}

func fetchDownloadSelection(pdf, source, all bool) (downloadPDF, downloadSource bool) {
	return pdf || all, source || all || (!pdf && !all)
}

func fetchFailureError(paperIDs []string) error {
	if len(paperIDs) == 0 {
		return nil
	}
	return fmt.Errorf("fetch failed for %d paper(s): %s", len(paperIDs), strings.Join(paperIDs, ", "))
}

func cmdSync(ctx context.Context, cacheDir string, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	set := fs.String("set", "", "arXiv set to sync (e.g., cs, physics)")
	from := fs.String("from", "", "Start date (YYYY-MM-DD)")
	batchSize := fs.Int("batch", 1000, "Metadata records to insert per database batch")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *batchSize <= 0 {
		return fmt.Errorf("batch size must be greater than zero")
	}

	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	opts := &arxiv.SyncOptions{
		Set:       *set,
		BatchSize: *batchSize,
		Progress: func(fetched, total int) {
			if total > 0 {
				fmt.Printf("\rSyncing: %d / %d papers (%.1f%%)", fetched, total, float64(fetched)/float64(total)*100)
			} else {
				fmt.Printf("\rSyncing: %d papers", fetched)
			}
		},
	}

	if *from != "" {
		opts.From, err = time.Parse("2006-01-02", *from)
		if err != nil {
			return fmt.Errorf("invalid date: %w", err)
		}
	}

	fmt.Println("Starting metadata sync; record count depends on the selected set, date range, and resume state.")
	fmt.Printf("Database batch size: %d\n", *batchSize)
	fmt.Println("Press Ctrl+C to stop; sync will resume from where it left off.")
	if err := cache.SyncMetadata(ctx, opts); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	fmt.Println()
	fmt.Println("Sync complete!")
	return nil
}

func cmdStats(ctx context.Context, cacheDir string, args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: arxiv stats")
	}
	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	stats, err := cache.Stats(ctx)
	if err != nil {
		return fmt.Errorf("stats: %w", err)
	}

	fmt.Printf("Cache: %s\n", cacheDir)
	fmt.Printf("Total papers:       %d\n", stats.TotalPapers)
	fmt.Printf("PDFs downloaded:    %d\n", stats.PDFsDownloaded)
	fmt.Printf("Sources downloaded: %d\n", stats.SourcesDownloaded)
	fmt.Printf("Queued downloads:   %d\n", stats.QueuedDownloads)
	return nil
}

func cmdSearch(ctx context.Context, cacheDir string, args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	category := fs.String("category", "", "Filter by category")
	limit := fs.Int("limit", 20, "Max results")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() == 0 {
		return fmt.Errorf("usage: arxiv search [options] <query>")
	}

	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	query := strings.Join(fs.Args(), " ")
	results, err := cache.Search(ctx, query, *category, *limit)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return nil
	}

	for _, p := range results {
		fmt.Printf("[%s] %s\n", p.ID, p.Title)
		fmt.Printf("  %s\n", p.Authors)
		fmt.Printf("  Categories: %s\n", p.Categories)
		if p.SourceDownloaded {
			fmt.Printf("  [source cached]")
		}
		if p.PDFDownloaded {
			fmt.Printf(" [pdf cached]")
		}
		fmt.Println()
		fmt.Println()
	}
	return nil
}

func cmdGet(ctx context.Context, cacheDir string, args []string) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fetch := fs.Bool("fetch", false, "Fetch from arXiv if not cached")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if fs.NArg() == 0 {
		return fmt.Errorf("usage: arxiv get [-fetch] <paper-id>")
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("get accepts exactly one paper ID")
	}

	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	var paper *arxiv.Paper
	if *fetch {
		paper, err = cache.Fetch(ctx, fs.Arg(0))
	} else {
		paper, err = cache.GetPaper(ctx, fs.Arg(0))
	}
	if err != nil {
		return fmt.Errorf("get paper: %w", err)
	}

	fmt.Printf("ID:         %s\n", paper.ID)
	fmt.Printf("Title:      %s\n", paper.Title)
	fmt.Printf("Authors:    %s\n", paper.Authors)
	fmt.Printf("Categories: %s\n", paper.Categories)
	fmt.Printf("Created:    %s\n", paper.Created.Format("2006-01-02"))
	fmt.Printf("Updated:    %s\n", paper.Updated.Format("2006-01-02"))
	fmt.Printf("PDF:        %v\n", paper.PDFDownloaded)
	fmt.Printf("Source:     %v\n", paper.SourceDownloaded)
	if paper.PDFPath != "" {
		fmt.Printf("PDF Path:   %s\n", paper.PDFPath)
	}
	if paper.SourcePath != "" {
		fmt.Printf("Source Path: %s\n", paper.SourcePath)
	}
	fmt.Printf("\nAbstract:\n%s\n", paper.Abstract)
	return nil
}

func cmdList(ctx context.Context, cacheDir string, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	category := fs.String("cat", "", "Filter by category (e.g., cs.AI)")
	limit := fs.Int("n", 0, "Max results (0 = all)")
	srcOnly := fs.Bool("src", false, "Only papers with source downloaded")
	all := fs.Bool("a", false, "Show all (including metadata-only)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("list accepts at most one category")
	}

	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	// Use remaining arg as category if provided
	if fs.NArg() > 0 && *category == "" {
		*category = fs.Arg(0)
	}

	papers, err := cache.ListPapersFiltered(ctx, *category, *srcOnly, *all, *limit)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	if len(papers) == 0 {
		fmt.Println("No papers cached.")
		return nil
	}

	for _, p := range papers {
		status := ""
		if p.SourceDownloaded {
			status += "[src]"
		}
		if p.PDFDownloaded {
			status += "[pdf]"
		}
		fmt.Printf("%s\t%s\t%s\n", p.ID, p.Title, status)
	}
	fmt.Fprintf(os.Stderr, "\n%d papers\n", len(papers))
	return nil
}

func cmdReindex(ctx context.Context, cacheDir string, args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	embeddings := fs.Bool("embeddings", false, "Deprecated: legacy MiniLM generation has been retired")
	_ = fs.Int("limit", 0, "Deprecated with -embeddings")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("reindex does not accept positional arguments")
	}
	if *embeddings {
		return fmt.Errorf("-embeddings has been retired; use the Qwen worker pipeline documented in docs/SEMANTIC_SEARCH.md")
	}

	cache, err := arxiv.Open(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer cache.Close()

	fmt.Println("Rebuilding FTS index...")
	if err := cache.RebuildFTSIndex(ctx); err != nil {
		return fmt.Errorf("reindex fts: %w", err)
	}

	fmt.Println("Rebuilding citations...")
	if err := cache.RebuildAllCitations(ctx); err != nil {
		return fmt.Errorf("reindex citations: %w", err)
	}

	fmt.Println("Done.")
	return nil
}

func findTool(name string) (string, error) {
	candidates := []string{filepath.Join("/app/tools", name), filepath.Join("tools", name)}
	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		candidates = append(candidates, filepath.Join(dir, "tools", name), filepath.Join(dir, "..", "tools", name))
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			path, absErr := filepath.Abs(candidate)
			if absErr != nil {
				return "", fmt.Errorf("resolve tool path %q: %w", candidate, absErr)
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("tool %q not found; run from the repository root or install it under /app/tools", name)
}
