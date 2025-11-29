# arxiv

[![Go Reference](https://pkg.go.dev/badge/github.com/tmc/arxiv.svg)](https://pkg.go.dev/github.com/tmc/arxiv)

Package arxiv provides tools for managing a complete offline cache of arXiv papers.

This package implements:

  - OAI-PMH client for harvesting paper metadata
  - PDF and TeX source download functionality
  - Local SQLite-based indexing for fast search
  - Incremental sync to keep the cache up to date

arXiv contains ~2.4 million papers (as of 2024). A full cache requires:

  - Metadata: ~10GB
  - PDFs: ~10TB
  - TeX sources: ~2TB

The cache supports incremental updates via OAI-PMH resumption tokens and tracks download state to resume interrupted syncs.

Basic usage:

	cache, err := arxiv.Open("/path/to/cache")
	if err != nil {
		log.Fatal(err)
	}
	defer cache.Close()

	// Sync metadata from arXiv
	if err := cache.SyncMetadata(ctx); err != nil {
		log.Fatal(err)
	}

	// Download papers for a specific category
	if err := cache.DownloadCategory(ctx, "cs.AI"); err != nil {
		log.Fatal(err)
	}
## Installation

To use this package in your Go project, you'll need [Go](https://go.dev/doc/install) 1.25 or later installed on your system.

```console
go get github.com/tmc/arxiv
```

Then import it in your code:

```go
import "github.com/tmc/arxiv"
```

## Functions

### ExtractReferences

```go
func ExtractReferences []string
```

ExtractReferences extracts arXiv paper IDs from .bbl and .bib files in the source directory.

## Types

### Cache

```go
type Cache struct {
	// contains filtered or unexported fields
}
```

Cache manages a local offline cache of arXiv papers.

#### Methods

##### CitedBy

```go
func (c *Cache) CitedBy 
```

##### CitedByCount

```go
func (c *Cache) CitedByCount 
```

CitedByCount returns the number of cached papers that cite this paper.

##### Close

```go
func (c *Cache) Close error
```

Close closes the cache database.

##### DownloadCategory

```go
func (c *Cache) DownloadCategory error
```

DownloadCategory downloads papers for a category.

##### DownloadPaper

```go
func (c *Cache) DownloadPaper error
```

DownloadPaper downloads PDF and/or source for a single paper.

##### Fetch

```go
func (c *Cache) Fetch 
```

Fetch retrieves a paper's metadata directly from arXiv API and stores it. This is for fetching individual papers without a full OAI-PMH sync.

##### FetchAndDownload

```go
func (c *Cache) FetchAndDownload 
```

FetchAndDownload fetches metadata and downloads source/PDF for a paper.

##### FetchBatch

```go
func (c *Cache) FetchBatch 
```

FetchBatch fetches metadata for multiple papers in a single API call. arXiv API supports up to ~100 IDs per request.

##### FetchMetadataOnly

```go
func (c *Cache) FetchMetadataOnly 
```

FetchMetadataOnly fetches just the metadata (title, authors, abstract) without downloading source. This is cheap and fast - good for populating citation titles.

##### GetCitationGraph

```go
func (c *Cache) GetCitationGraph 
```

GetCitationGraph returns a citation graph centered on the given paper. Includes: the paper itself, its references, papers that cite it, and edges between references if they cite each other.

##### GetPaper

```go
func (c *Cache) GetPaper 
```

GetPaper retrieves a paper by ID.

##### GetPaperList

```go
func (c *Cache) GetPaperList 
```

GetPaperList returns a combined list of references and citing papers for the sidebar.

##### GetPaperWithCitations

```go
func (c *Cache) GetPaperWithCitations 
```

GetPaperWithCitations returns a paper along with its citation count.

##### ListCategories

```go
func (c *Cache) ListCategories 
```

ListCategories returns all categories with their paper counts.

##### ListPapers

```go
func (c *Cache) ListPapers 
```

ListPapers lists papers, optionally filtered by category.

##### ListPapersFiltered

```go
func (c *Cache) ListPapersFiltered 
```

ListPapersFiltered lists papers with various filter options.

##### PaperExists

```go
func (c *Cache) PaperExists bool
```

PaperExists checks if a paper exists in the cache.

##### PrefetchReferenceTitles

```go
func (c *Cache) PrefetchReferenceTitles error
```

PrefetchReferenceTitles fetches metadata for all uncached references of a paper. This populates titles without downloading full sources.

##### RebuildAllCitations

```go
func (c *Cache) RebuildAllCitations error
```

RebuildAllCitations rebuilds the citations table by re-extracting references from all papers.

##### RebuildFTSIndex

```go
func (c *Cache) RebuildFTSIndex error
```

RebuildFTSIndex rebuilds the FTS5 index from all papers. Use this after migrating an existing database to FTS5.

##### References

```go
func (c *Cache) References 
```

##### Root

```go
func (c *Cache) Root string
```

Root returns the cache root directory.

##### Search

```go
func (c *Cache) Search 
```

Search searches papers by title/abstract text using FTS5.

##### SearchByAuthor

```go
func (c *Cache) SearchByAuthor 
```

SearchByAuthor searches papers by author name.

##### Stats

```go
func (c *Cache) Stats 
```

Stats returns cache statistics.

##### SyncMetadata

```go
func (c *Cache) SyncMetadata error
```

SyncMetadata synchronizes paper metadata from arXiv via OAI-PMH.

##### UncachedReferenceCount

```go
func (c *Cache) UncachedReferenceCount 
```

UncachedReferenceCount returns the number of references without metadata.

##### UpdateCitations

```go
func (c *Cache) UpdateCitations error
```

UpdateCitations extracts references from a paper's source and stores citation edges. This should be called after downloading source files.

### CacheStats

```go
type CacheStats struct {
	TotalPapers       int64
	PDFsDownloaded    int64
	SourcesDownloaded int64
	QueuedDownloads   int64
}
```

CacheStats contains statistics about the cache.

### CategoryCount

```go
type CategoryCount struct {
	Name  string
	Count int
}
```

CategoryCount represents a category with its paper count.

### CitationGraph

```go
type CitationGraph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}
```

CitationGraph represents a citation graph for visualization.

### CitingPaper

```go
type CitingPaper struct {
	ID    string
	Title string
}
```

CitedBy returns papers that cite this paper (only cached papers with metadata).

### DownloadOptions

```go
type DownloadOptions struct {
	// Concurrency is the number of parallel downloads (default 1)
	Concurrency int

	// RateLimit is the delay between downloads (default 3s per arXiv guidelines)
	RateLimit time.Duration

	// DownloadPDF enables PDF downloads
	DownloadPDF bool

	// DownloadSource enables TeX source downloads
	DownloadSource bool

	// Progress callback
	Progress func(paperID string, downloaded, total int)
}
```

DownloadOptions configures paper downloads.

### GraphEdge

```go
type GraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
}
```

GraphEdge represents an edge in the citation graph.

### GraphNode

```go
type GraphNode struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Authors   string `json:"authors"`
	Year      int    `json:"year"`
	Citations int    `json:"citations"` // How many papers cite this one
	Cached    bool   `json:"cached"`
}
```

GraphNode represents a node in the citation graph.

### OAIClient

```go
type OAIClient struct {
	// contains filtered or unexported fields
}
```

OAIClient is an OAI-PMH client for arXiv.

#### Methods

##### ListRecords

```go
func (c *OAIClient) ListRecords 
```

ListRecords fetches records from arXiv via OAI-PMH. If resumptionToken is empty, starts from the beginning with the given params. If resumptionToken is non-empty, continues from that point.

### OAIResponse

```go
type OAIResponse struct {
	Papers           []Paper
	ResumptionToken  string
	CompleteListSize int
	Cursor           int
}
```

OAIResponse contains the parsed response from an OAI-PMH ListRecords request.

### Paper

```go
type Paper struct {
	// ID is the arXiv identifier (e.g., "2301.00001" or "hep-th/9901001")
	ID string

	// Created is when the paper was first submitted
	Created time.Time

	// Updated is when the paper was last updated
	Updated time.Time

	// Title of the paper
	Title string

	// Abstract of the paper
	Abstract string

	// Authors as a single string (arXiv format)
	Authors string

	// Categories is a space-separated list of arXiv categories
	Categories string

	// Comments from the submitter (e.g., "10 pages, 3 figures")
	Comments string

	// JournalRef is the journal reference if published
	JournalRef string

	// DOI is the Digital Object Identifier if available
	DOI string

	// License URL
	License string

	// PDFPath is the local path to the PDF (if downloaded)
	PDFPath string

	// SourcePath is the local path to the TeX source (if downloaded)
	SourcePath string

	// PDFDownloaded indicates if the PDF has been downloaded
	PDFDownloaded bool

	// SourceDownloaded indicates if the source has been downloaded
	SourceDownloaded bool
}
```

Paper represents an arXiv paper's metadata.

#### Methods

##### AbstractURL

```go
func (p *Paper) AbstractURL string
```

AbstractURL returns the arXiv abstract page URL.

##### CategoryList

```go
func (p *Paper) CategoryList []string
```

CategoryList returns all categories as a slice.

##### PDFURL

```go
func (p *Paper) PDFURL string
```

PDFURL returns the arXiv PDF download URL.

##### PrimaryCategory

```go
func (p *Paper) PrimaryCategory string
```

PrimaryCategory returns the primary (first) category.

##### SourceURL

```go
func (p *Paper) SourceURL string
```

SourceURL returns the arXiv source download URL.

### PaperListItem

```go
type PaperListItem struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Authors   string `json:"authors"`
	Year      int    `json:"year"`
	Citations int    `json:"citations"`
	Cached    bool   `json:"cached"`
	IsRef     bool   `json:"isRef"`    // True if this paper is a reference
	IsCiting  bool   `json:"isCiting"` // True if this paper cites the main paper
}
```

PaperListItem represents a paper in the sidebar list.

### Reference

```go
type Reference struct {
	ID        string
	Title     string
	HasTitle  bool // True if we have metadata (title available)
	HasSource bool // True if we have source downloaded
}
```

Reference represents a paper that is cited.

### SyncOptions

```go
type SyncOptions struct {
	// Set filters to a specific arXiv set (e.g., "cs" for computer science)
	Set string

	// From is the start date for incremental sync
	From time.Time

	// Until is the end date for sync
	Until time.Time

	// Progress callback for reporting sync progress
	Progress func(fetched, total int)

	// BatchSize is how often to commit (default 1000)
	BatchSize int
}
```

SyncOptions configures metadata synchronization.

