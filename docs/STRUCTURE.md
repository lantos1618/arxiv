# Project Structure

This map reflects the current source tree. It intentionally avoids line/file-count claims that become stale.

## Root Package

| Area | Files |
|---|---|
| Database and cache | `cache.go`, `cache_models.go`, `cache_paper.go`, `cache_lru.go`, `detail_cache.go`, `sql_helpers.go` |
| Ingestion | `data_fetch.go`, `data_download.go`, `data_oai.go`, `data_sync.go` |
| Search | `search.go`, `search_fts.go`, `search_pdf.go`, `search_embeddings.go`, `embedding_models.go` |
| Qwen queues | `qwen_jobs.go` |
| Legacy embeddings | `embedding_worker.go` |
| Citations/authors | `citations.go`, `citations_refs.go`, `authors.go` |
| Accounts/community | `auth.go`, `api_keys.go`, `user_views.go`, `feedback.go` |
| Admin/export | `admin_stats.go`, `export.go`, `export_sitemap.go` |

## Commands

- `cmd/arxiv/` contains the user CLI, HTTP server, pages, templates, REST/SSE API, MCP endpoint, auth, feedback, admin, middleware, SEO handlers, and tests.
- `cmd/migrate/` contains the SQLite-to-PostgreSQL migration command and tests.

The executable imports the root package as `github.com/lantos1618/arxiv.gg`.

## Operations

- `tools/` contains Qwen services/workers, queue and backfill scripts, full-paper extraction/chunking, pipeline checks, load/IndexNow helpers, and legacy MiniLM utilities.
- `deploy/sql/` contains reviewed PostgreSQL indexes and corrective migrations.
- `deploy/systemd/` and `deploy/qwen-jit-orchestrator.service` contain worker service assets.
- `Dockerfile`, `Dockerfile.qwen-api-worker`, `docker-compose.yml`, `start.sh`, and `Makefile` support builds and operations.

## Documentation

Active references are `README.md`, `CONTRIBUTING.md`, `CLAUDE.md`, and the topic guides in `docs/`. Date-stamped audits, reviews, performance measurements, SEO reports, and marketing research are archived snapshots and are not current specifications.

## Data Flow

```text
arXiv OAI/API/files
        |
        v
PostgreSQL + filesystem cache
        |
        +--> metadata FTS / author / citation queries
        |
        +--> PDF text --> chunks --> Qwen chunk vectors
        |
        +--> title + abstract --> Qwen abstract vectors
        |
        v
CLI / web pages / REST / SSE / MCP
```

Accounts, sessions, signed-in paper views, API-key hashes, feedback, moderation audit data, and Qwen leases are stored in PostgreSQL alongside the catalog.
