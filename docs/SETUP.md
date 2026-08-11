# System Setup

## Supported Architecture

A supported arxiv.gg deployment uses PostgreSQL with pgvector. SQLite is retained for unit tests and legacy maintenance only; it does not support the production Qwen vector-search architecture.

Required software:

- Go 1.25
- PostgreSQL and pgvector
- Python 3 for ingestion and Qwen workers
- Poppler's `pdftotext` for PDF text preparation
- Docker/Compose for the production runbook
- CUDA hardware or a remote GPU service for Qwen inference

Storage depends on catalog and file coverage. Metadata alone is much smaller than a corpus containing PDFs, extracted text, chunks, and vectors; capacity-plan from measured ingestion batches rather than historical repository estimates.

## Build

```bash
go mod download
go test ./...
go build -o bin/arxiv ./cmd/arxiv
```

## PostgreSQL

Create a database with pgvector available, then export an explicit URL:

```bash
export ARXIV_CACHE="$PWD/.cache/arxiv"
export DATABASE_URL='postgres://arxiv:password@127.0.0.1:5432/arxiv?sslmode=disable'
./bin/arxiv stats
```

Opening the cache runs GORM `AutoMigrate` and backend-specific schema/index initialization. Use a disposable database for development, and rehearse schema changes against a recent production copy before deployment.

## Catalog Ingestion

Sync a bounded metadata set first:

```bash
./bin/arxiv sync -set cs -from 2024-01-01 -batch 1000
./bin/arxiv search -limit 10 "graph neural network"
```

Fetch selected source/PDF files when needed:

```bash
./bin/arxiv fetch -all 1706.03762
```

Use [SEMANTIC_SEARCH.md](SEMANTIC_SEARCH.md) to populate current Qwen vectors. `reindex -embeddings` now returns an explicit retirement error; `tools/generate_embeddings.py` is gated for isolated legacy migrations only.

## Accounts

Google sign-in is enabled by either environment credentials or a Google OAuth credentials file:

```dotenv
GOOGLE_CLIENT_ID=...
GOOGLE_CLIENT_SECRET=...
GOOGLE_REDIRECT_URL=https://example.com/auth/google/callback
# Alternative when ID/secret are not set:
GOOGLE_OAUTH_CREDENTIALS_FILE=/run/secrets/google-oauth.json
```

Set `ADMIN_EMAILS` to a comma/space-separated allowlist for signed-in administrators. `ADMIN_TOKEN` supports header-based operational access. Account API keys are created from `/account`, stored hashed, shown in full only when created/regenerated, and accepted as `Authorization: Bearer arxivgg_...`.

## Serve

```bash
./bin/arxiv serve -port 8080
curl -fsS http://127.0.0.1:8080/health
```

The health response must report `"db":"postgres"`. Normal mode binds all interfaces. `-local` binds loopback and relaxes privileged checks for a trusted local workstation; never expose local mode through an unauthenticated network path.

## Important Environment Variables

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | Required PostgreSQL URL for supported deployments |
| `ARXIV_CACHE` | PDF/source/meta cache root |
| `PORT` | Container server port used by `start.sh` |
| `ADMIN_TOKEN` | Explicit admin API credential |
| `ADMIN_EMAILS` | Google-account admin allowlist |
| `QWEN_WORKER_TOKEN` | Remote Qwen job API credential |
| `QWEN_EMBEDDING_SERVICE_URL` | Synchronous Qwen service URL |
| `QWEN_ASYNC_WORKER_ENABLED` | Set `true` only while a monitored Qwen queue worker/orchestrator is operational |
| `TRUST_PROXY_HEADERS` | Trust forwarded client IP only behind a sanitizing proxy |
| `INDEXNOW_KEY` | Enables the matching IndexNow key route |
| `SITE_URL` | Canonical base URL for generated sitemap/export links |
| `ARXIV_DB_MAX_OPEN_CONNS` | PostgreSQL pool maximum; default 30 |
| `ARXIV_DB_MAX_IDLE_CONNS` | PostgreSQL idle pool maximum; default 10 |
| `ARXIV_DETAIL_CACHE_SIZE` | Detail LRU entry limit; default 20,000 |

Legacy MiniLM service variables and flags remain in compatibility code but are not part of the production container startup or current semantic setup.

## Proxy Safety

Leave `TRUST_PROXY_HEADERS=false` unless the application is reachable only through a trusted proxy that removes or overwrites `CF-Connecting-IP` and `X-Forwarded-For`. Incorrect trust lets clients spoof the address used by rate limits.

## Legacy SQLite Tests

The Go suite creates temporary SQLite databases. Make backend selection explicit:

```bash
env -u DATABASE_URL go test ./...
```

An application startup that prints `Using SQLite database` is not a successful production fallback. Stop and correct `DATABASE_URL`.

## Production

Use [DEPLOYMENT_RUNBOOK_2026-05-15.md](DEPLOYMENT_RUNBOOK_2026-05-15.md). Preserve the external PostgreSQL volume, build an immutable release from a clean commit, back up schema, apply reviewed SQL, and retain the exact previous image for rollback.
