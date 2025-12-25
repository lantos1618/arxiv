# Project Structure

## Overview

The arXiv Cache Manager uses a flat package structure (`package arxiv`) at the root, which is idiomatic for Go libraries. The CLI application lives in `cmd/arxiv/`.

## Directory Layout

```
arxiv/
├── cmd/arxiv/              # CLI and web server
│   ├── main.go             # CLI entry point and commands
│   ├── serve.go            # Web server and HTML handlers
│   ├── api.go              # REST API handlers
│   ├── middleware.go       # HTTP middleware (rate limiting, caching)
│   ├── doc.go              # CLI documentation
│   └── templates/          # HTML templates
│
├── docs/                   # Documentation
│   ├── API.md              # REST API reference
│   ├── PLAN.md             # Semantic search roadmap
│   ├── SETUP.md            # System requirements
│   ├── STRUCTURE.md        # This file
│   ├── TESTING.md          # Test documentation
│   └── REVIEW.md           # Project review
│
├── tools/                  # Utility scripts
│   ├── generate_embeddings.py  # Python embedding generator
│   ├── requirements.txt    # Python dependencies
│   └── README.md           # Tools documentation
│
├── *.go                    # Core library files
├── *_test.go               # Test files
├── Makefile                # Build automation
├── CONTRIBUTING.md         # Contribution guidelines
├── README.md               # Project overview
├── go.mod / go.sum         # Go modules
└── Dockerfile              # Container build
```

## Source Files by Category

### Core (5 files)
| File | Purpose |
|------|---------|
| `cache.go` | Cache initialization, database setup, schema management |
| `models.go` | Data models: Paper, Citation, Embedding, SyncState, etc. |
| `lru.go` | Thread-safe LRU cache for in-memory paper caching |
| `paper.go` | Paper utility methods (URLs, categories) |
| `log.go` | Structured logging utilities |

### Search (5 files)
| File | Purpose |
|------|---------|
| `search.go` | FTS5 keyword search, category listing, paper filtering |
| `semantic.go` | Semantic search using vector embeddings |
| `embeddings.go` | Embedding storage, retrieval, and generation interface |
| `fts.go` | FTS5 index management |
| `pdfsearch.go` | PDF text extraction and search |

### Data Operations (4 files)
| File | Purpose |
|------|---------|
| `fetch.go` | Fetch paper metadata from arXiv API |
| `download.go` | Download PDFs and TeX sources |
| `sync.go` | OAI-PMH bulk metadata synchronization |
| `oai.go` | OAI-PMH protocol client |

### Citations (2 files)
| File | Purpose |
|------|---------|
| `citations.go` | Citation graph queries, paper relationships |
| `refs.go` | Reference extraction from TeX/bbl files |

### Export (2 files)
| File | Purpose |
|------|---------|
| `export.go` | BibTeX, RIS, JSON export formats |
| `sitemap.go` | Sitemap XML generation |

### Tests (8 files)
| File | Tests |
|------|-------|
| `cache_test.go` | Cache operations |
| `search_test.go` | FTS5 search |
| `semantic_test.go` | Semantic search |
| `embeddings_test.go` | Embedding storage/generation |
| `citations_test.go` | Citation graph |
| `export_test.go` | Export formats |
| `refs_test.go` | Reference extraction |
| `data_test.go` | Data operations |

## Why This Structure?

1. **Flat is idiomatic**: Go libraries commonly use a single package
2. **Simple imports**: `import "github.com/tmc/arxiv"` gives access to everything
3. **No circular dependencies**: All code in same package
4. **Clear file names**: Each file has single responsibility
5. **Tests alongside code**: Go convention for discoverability

## Key Types

```go
// Core types
type Cache struct { ... }           // Main cache manager
type Paper struct { ... }           // Paper metadata
type Citation struct { ... }        // Citation relationship
type Embedding struct { ... }       // Vector embedding

// Search types
type CategoryCount struct { ... }   // Category statistics
type Reference struct { ... }       // Citation reference

// Graph types
type CitationGraph struct { ... }   // Visualization data
type GraphNode struct { ... }       // Node in graph
type GraphEdge struct { ... }       // Edge in graph
```

## Usage Example

```go
import "github.com/tmc/arxiv"

// Open cache
cache, err := arxiv.Open("/path/to/cache")
defer cache.Close()

// Search papers
papers, err := cache.Search(ctx, "machine learning", "cs.AI", 20)

// Get citation graph
graph, err := cache.GetCitationGraph(ctx, "2301.00001")

// Export to BibTeX
bibtex := paper.ToBibTeX()
```
