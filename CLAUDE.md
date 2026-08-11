# CLAUDE.md

Repository guidance for coding agents and maintainers.

## Commands

```bash
go build -o bin/arxiv ./cmd/arxiv
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

Use `go test ./... -run TestName` for a focused Go test. The module requires Go 1.25.

## Product Boundary

arxiv.gg is a Go CLI and web service for arXiv discovery. It provides metadata search, Qwen idea and full-paper search, paper/author/category/citation exploration, exports, REST/SSE/MCP interfaces, optional Google accounts, signed-in reading history, agent API keys, feedback, and admin operations.

Do not document saved searches, alerts, billing, paid plans, private analytics, or guaranteed suggestion awards as current capabilities. The feedback offer is conditional and discretionary; see `README.md`.

## Database

Production requires PostgreSQL with pgvector and the external `arxiv_postgres_data` volume. An empty `DATABASE_URL` selects SQLite, but SQLite is legacy/test-only and is not a supported production architecture.

Every `arxiv.Open` call runs GORM `AutoMigrate` and backend-specific initialization. Treat application startup as potentially schema-changing. Never remove the production volume, run destructive DDL casually, or deploy without the backup and exact-image rollback procedure in `docs/DEPLOYMENT_RUNBOOK_2026-05-15.md`.

## Semantic Architecture

Current product semantic features use only `Qwen/Qwen3-Embedding-8B` at 1,024 dimensions:

- `embeddings_v2` for title-and-abstract vectors
- `paper_chunks` and `chunk_embeddings_v2` for full-paper retrieval
- `qwen_embedding_jobs` and query-text storage for leased remote/local work
- Qwen vectors for semantic search and related-paper maps

The MiniLM service, `generate_embeddings.py`, original `embeddings` table, and generic embedding worker remain compatibility/migration code. Do not build new product behavior or current docs around them.

## Source Layout

- `cmd/arxiv/main.go` — CLI dispatch and command flags
- `cmd/arxiv/serve.go` — server configuration, routes, pages, health, and SEO handlers
- `cmd/arxiv/api.go` — REST/SSE endpoints and Qwen worker API
- `cmd/arxiv/mcp.go` — Streamable HTTP MCP server
- `cmd/arxiv/auth_handlers.go`, `admin_auth.go` — Google sessions, account keys, admin and worker auth
- `cmd/arxiv/feedback_handlers.go` — public feedback and moderation HTTP flows
- Root package — database models, cache, ingestion, search, citations, exports, accounts, feedback, and Qwen queues
- `tools/` — Python/shell ingestion and GPU workers
- `deploy/` — production SQL and services
- `docs/` — active docs plus archived snapshots

## Safety And Concurrency

- Preserve concurrent work; do not revert unrelated changes.
- Keep public workload limits, body caps, timeouts, cancellation, and safe error redaction.
- Maintain lease owner/generation/source-hash fencing and heartbeat behavior for Qwen jobs.
- Make database mutations atomic and keep in-memory caches consistent with committed state.
- Cookie-authenticated mutations must retain same-origin/CSRF protections.
- Worker and admin secrets belong in headers or secret files, never query strings or process arguments.

## Local And Production Serving

`arxiv serve -local` enables local download behavior, bypasses production admin checks, and binds to loopback. Normal serving binds on all interfaces and requires production auth for privileged operations.

Set `TRUST_PROXY_HEADERS=true` only when a trusted proxy is the only ingress and overwrites forwarding headers. Validate `/health` reports PostgreSQL after every deployment.
