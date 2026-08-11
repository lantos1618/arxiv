# arxiv.gg

arxiv.gg is an arXiv discovery service and self-hostable Go application. It combines metadata search, Qwen semantic retrieval, full-paper search where text has been prepared, citation and author exploration, exports, a JSON API, and an MCP endpoint.

## Product Capabilities

- **Quick search** for arXiv IDs, titles, abstracts, authors, and categories.
- **Idea search** over title-and-abstract embeddings.
- **Deep Search** over prepared full-paper chunks; sign-in is required on the public site.
- Paper, author, category, citation, related-work, and semantic-map pages.
- BibTeX, RIS, and JSON exports.
- REST, Server-Sent Events (SSE), and Streamable HTTP MCP interfaces.
- Optional Google accounts with recently viewed papers, reader-based recommendations, and a rotatable agent API key.
- Signed-in suggestions, voting, deletion, and administrator moderation.

arxiv.gg is not affiliated with or endorsed by arXiv. Metadata and files originate from arXiv; availability and coverage vary by record.

## Architecture

Production requires **PostgreSQL with pgvector**. PostgreSQL stores the paper catalog, search indexes, accounts, feedback, Qwen jobs, abstract vectors, paper chunks, and chunk vectors. The application runs GORM `AutoMigrate` and backend-specific schema initialization whenever it opens the database.

The current semantic architecture is Qwen-only:

- Model: `Qwen/Qwen3-Embedding-8B`
- Stored dimension: 1,024
- Abstract vectors: `embeddings_v2`
- Full-paper chunks and vectors: `paper_chunks` and `chunk_embeddings_v2`
- Query and abstract work: leased `qwen_embedding_jobs`, serviced locally or by authenticated remote GPU workers
- Similar-paper maps: Qwen abstract vectors projected into two dimensions for display

MiniLM code and the original `embeddings` table remain for compatibility and migration work. They are not the current product semantic-search path. SQLite remains a legacy/test backend used by the Go test suite and limited maintenance workflows; it is not a supported production database and cannot provide the PostgreSQL/pgvector product architecture.

See [Semantic Search](docs/SEMANTIC_SEARCH.md) and the [GPU Worker Runbook](docs/GPU_WORKER_RUNBOOK.md).

## Prerequisites

- Go 1.25, matching `go.mod`
- PostgreSQL with the pgvector extension for a supported application deployment
- Python 3 plus the worker dependencies for Qwen ingestion or serving
- `pdftotext` from Poppler for PDF text extraction
- Docker and Docker Compose for the documented production deployment
- A CUDA-capable GPU for self-hosted Qwen inference; remote workers are also supported

## Build And Run

Build and test the Go application:

```bash
go build -o bin/arxiv ./cmd/arxiv
go test ./...
```

Set a PostgreSQL URL before opening the application:

```bash
export ARXIV_CACHE="$HOME/.cache/arxiv"
export DATABASE_URL='postgres://arxiv:password@127.0.0.1:5432/arxiv?sslmode=disable'
./bin/arxiv sync -set cs -from 2024-01-01
./bin/arxiv serve -port 8080
curl -fsS http://127.0.0.1:8080/health
```

The health response must report `"db":"postgres"` for a supported deployment. `serve -local` enables local PDF/source downloads, bypasses production admin checks, and binds only to loopback; do not use it as an internet-facing mode.

For SQLite-only tests, explicitly clear `DATABASE_URL`:

```bash
env -u DATABASE_URL go test ./...
```

See [Setup](docs/SETUP.md) and [CLI](docs/CLI.md).

## Search Interfaces

```bash
# Metadata full-text search
curl --get 'http://127.0.0.1:8080/api/v1/search' \
  --data-urlencode 'q=graph neural networks' \
  --data 'limit=10'

# Qwen idea search; inspect data.fallback before treating scores as semantic
curl --get 'http://127.0.0.1:8080/api/v1/search/semantic' \
  --data-urlencode 'q=robust reasoning under distribution shift' \
  --data 'limit=10'

# SSE search with quick, search, or deep mode
curl -N --get 'http://127.0.0.1:8080/api/v1/search/stream' \
  --data-urlencode 'q=mechanistic interpretability' \
  --data 'mode=semantic'
```

Quick Search is the default. Semantic Search uses Qwen only and is shown in the browser only when a synchronous service or monitored asynchronous worker is configured. If Qwen execution becomes unavailable during a request, the semantic endpoint returns HTTP 206 with Quick matches, structured fallback metadata, and retries only when an asynchronous worker is genuinely configured. Clients must not interpret fallback results as vector similarities. Deep Search requires a signed-in browser session or account API key and only covers papers with current prepared chunks.

See [API](docs/API.md) for endpoints, authentication, limits, and queued embedding responses.

## Accounts And Feedback

Accounts are optional and currently use Google OAuth when configured. The application stores the account profile, sessions, signed-in paper-view counts/timestamps, a hashed API key, and feedback activity. It does not currently provide saved searches, alerts, billing, or a guaranteed private reading mode. Public paper and search routes generally work without an account; Deep Search and account-specific MCP behavior require authentication.

The dedicated `/feedback` page explains how community ideas inform product decisions and advertises a conditional `$100` suggestion offer. A post alone does not earn an award. Eligibility requires a qualifying suggestion that arxiv.gg selects, actually ships, and accepts as meeting the offer; the submitter must be contactable and legally/payment-eligible. Acceptance and payment remain discretionary, and no award is guaranteed for every post, vote leader, similar idea, partial implementation, or feature independently planned or shipped. Anonymous display does not make the submission anonymous to operators because the account remains associated in the database.

## Production Deployment

Copy `.env.example` to `.env`, set the non-secret deployment values, and create the root-readable secret directory named by `ARXIV_SECRETS_DIR`. The Compose deployment expects files named `postgres-password`, `database-url`, `admin-token`, `qwen-worker-token`, and `google-client-secret`; it does not put these values directly in `.env`.

```dotenv
ARXIV_SECRETS_DIR=/etc/arxiv/secrets
ADMIN_EMAILS=you@example.com
TRUST_PROXY_HEADERS=false
INDEXNOW_KEY=...
```

Set `TRUST_PROXY_HEADERS=true` only when a trusted reverse proxy is the sole ingress and overwrites client-supplied forwarding headers. Preserve the external `arxiv_postgres_data` volume. Build a clean, commit-addressed image, back up the schema, retain the exact running image for rollback, and replace only the application container.

After deployment, verify:

```bash
curl -fsS http://127.0.0.1/health
APP_CONTAINER="$(docker compose ps -q arxiv)"
docker inspect -f '{{.Config.Image}}' "$APP_CONTAINER"
docker logs --since 5m "$APP_CONTAINER"
```

The health response must identify PostgreSQL and the inspected image must be the intended immutable release. Follow the complete [Production Deployment Runbook](docs/DEPLOYMENT_RUNBOOK_2026-05-15.md); container rollback does not reverse schema changes.

## Repository Guide

- `cmd/arxiv/` — CLI, HTTP server, templates, REST, SSE, MCP, auth, admin, and feedback handlers
- Root Go package — cache, synchronization, downloads, search, citations, exports, accounts, feedback, and Qwen queues
- `tools/` — Qwen services, workers, backfills, full-paper preparation, checks, and legacy MiniLM utilities
- `deploy/` — SQL and systemd assets
- `docs/` — active technical documentation and clearly marked historical snapshots
- `cmd/migrate/` — SQLite-to-PostgreSQL migration command

## Validation

```bash
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

These commands do not validate production PostgreSQL migrations, pgvector indexes, OAuth, proxy behavior, GPU workers, or distributed queue failures. Use [Testing](docs/TESTING.md) for the required integration checks.

## License

MIT. See [LICENSE](LICENSE).
