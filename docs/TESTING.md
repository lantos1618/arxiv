# Testing

## Go Suite

The current tree contains root-package tests for auth/API keys, fetch and sync behavior, exports/sitemaps, feedback, Qwen queues, semantic search, and cross-cutting review regressions. `cmd/arxiv` tests admin auth, API error handling, Qwen HTTP behavior, feedback handlers, MCP, security regressions, and SEO. `cmd/migrate` tests migration failures and data handling.

List the authoritative current inventory instead of relying on a copied count:

```bash
find . -name '*_test.go' -not -path './.git/*' -print | sort
go test -list . ./...
```

## Validation Commands

```bash
# Focused package/test
go test ./... -run TestName

# Full unit/HTTP suite
go test ./...

# Concurrency checks
go test -race ./...

# Static analysis and patch hygiene
go vet ./...
git diff --check

# Python and shell syntax used by workers
python3 -m compileall -q tools
bash -n start.sh tools/*.sh deploy/systemd/*
```

Most Go database tests explicitly clear `DATABASE_URL` and use temporary SQLite databases. That is useful for deterministic unit tests but does not establish production PostgreSQL correctness.

## Required Integration Coverage

Before deploying changes in these areas, add focused validation:

- **PostgreSQL/pgvector:** fresh schema, upgrade from a production-like copy, required extensions/indexes, lock timeout/failure behavior, and `deploy/sql/2026-07-11-qwen-vector-not-null.sql` preconditions.
- **Qwen:** service health, query fallback markers, abstract and chunk hash freshness, non-NULL dimensions, leased claim/heartbeat/complete/fail fencing, ambiguous completion retry, and multiple workers.
- **HTTP/proxy:** real reverse proxy, trusted/untrusted forwarding headers, rate limits, request/body limits, SSE disconnects, SIGTERM shutdown, and public error redaction.
- **Accounts/security:** Google OAuth callback, session cookies, API-key rotation, same-origin mutation checks, admin allowlists/tokens, and worker tokens.
- **Deployment:** clean immutable image, schema backup, PostgreSQL health identity, preserved volume, exact-image rollback, and schema compatibility after rollback.

## Qwen Smoke Test

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_pipeline_check.py \
  --scope both --window-minutes 15 --min-recent 1

curl --get 'http://127.0.0.1:8080/api/v1/search/semantic' \
  --data-urlencode 'q=causal representation learning'
```

Assert the response either identifies Qwen semantic mode or explicitly sets `fallback:true`; never accept an unmarked mode change.

## Adding Tests

Prefer table-driven tests for validation boundaries and regression tests for every fixed failure mode. Use `t.TempDir`, `t.Setenv`, request contexts, and deterministic fixtures. Do not depend on live arXiv, OAuth, or GPU services in the default unit suite; put those checks in explicit integration workflows.
