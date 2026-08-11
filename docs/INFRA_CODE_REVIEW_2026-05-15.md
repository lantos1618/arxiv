# arxiv.gg Infra and Code Review

> **ARCHIVED SNAPSHOT — SUPERSEDED.** This review describes the repository and infrastructure as observed on 2026-05-15. Its findings and suggested patch order must not be read as current. Use the active deployment, setup, API, semantic-search, and testing guides, and confirm any historical claim against current code.

Snapshot: 2026-05-15 around 07:20 UTC

Scope: local repository at `/home/ubuntu/arxiv`, live deployment observations from Docker/host inspection, and a review of the Go web/API/database paths, Python embedding service, Dockerfile, compose file, and templates.

No production service changes were made during this review.

## Findings

### P1 - Raw citation SQL uses SQLite placeholders against Postgres

References:

- `citations.go:361-365`
- `citations.go:366-374`
- Related risk: `citations.go:94-102`, `citations.go:138-142`

`GetCitationGraph` uses `sqlDB.QueryContext` directly with `?` placeholders:

```go
rows, err := sqlDB.QueryContext(ctx, `
    SELECT from_id, to_id FROM citations
    WHERE from_id IN (SELECT to_id FROM citations WHERE from_id = ?)
      AND to_id IN (SELECT to_id FROM citations WHERE from_id = ?)
`, paperID, paperID)
```

That is invalid for the Postgres driver, which expects `$1`, `$2`, etc. The live Postgres logs show repeated syntax errors for this exact query. The error is also swallowed because the code only processes rows when `err == nil`, so the UI silently loses graph edges while the DB logs fill with errors.

Fix direction:

- Use GORM query builders for this query, or add a small placeholder helper for direct `database/sql`.
- Return/log the error instead of silently ignoring it.
- Audit the other direct `sqlDB.Query*` calls in `citations.go`, because they also use `?` and can fail under Postgres when reached.

### P1 - Public unauthenticated endpoints can trigger expensive or mutating work

References:

- `cmd/arxiv/serve.go:110`, `cmd/arxiv/serve.go:118`, `cmd/arxiv/serve.go:134-135`
- `cmd/arxiv/api.go:857-890`
- `cmd/arxiv/api.go:1446-1464`
- `cmd/arxiv/middleware.go:190-193`
- `cmd/arxiv/serve.go:477-497`
- `cmd/arxiv/serve.go:668-684`

The app exposes several routes that change state or start heavy jobs without authentication:

- `POST /api/v1/embeddings/generate` can spawn embedding generation for many papers.
- `POST /api/v1/authors/build-graph` starts an all-paper collaboration rebuild in a background goroutine.
- `/admin/embeddings` is only "unlisted", not protected.
- `GET /paper/{id}/fetch` fetches and stores metadata.
- `GET /abs/{id}` auto-fetches and stores metadata for cache misses.

The rate limiter explicitly bypasses paths containing `/generate`, which removes the one guard from the most expensive endpoint.

Fix direction:

- Add real auth for admin/mutation endpoints, even a single `ADMIN_TOKEN` header gate would be a major improvement.
- Remove `/generate` from the limiter bypass; only SSE streaming should bypass the caching wrapper, not rate limiting.
- Make public GET routes read-only where possible. Queue or debounce fetches rather than doing network/database writes inline.
- Consider Cloudflare Access or tunnel-side route protection for `/admin/*` and mutation APIs.

### P1 - Current author endpoints explain the Postgres CPU burn

References:

- `search.go:175-205`
- `authors.go:85-185`
- `authors.go:392-407`
- `authors.go:581-698`
- `cmd/arxiv/api.go:1341-1345`

Production logs show repeated 2.5-4.4 second author queries, and Docker stats sampled Postgres above 300 percent CPU. The code repeatedly runs leading-wildcard `ILIKE` against the denormalized `papers.authors` text column:

```go
Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%")
```

That pattern cannot use a normal B-tree index. It also appears multiple times per page: profile, stats, graph, collaborators, and second-degree graph expansion. The collaborators endpoint allows `limit` up to 5000.

Fix direction:

- Quick mitigation: enable `pg_trgm` and add a GIN trigram index on `papers.authors`.
- Better fix: normalize authors into `paper_authors(paper_id, author_normalized, author_display)` with indexes, and make author pages query that table.
- Cache author profile/collaborator responses for a short TTL.
- Reduce or cap expensive second-degree graph fanout.

### P1 - Rate limiting trusts spoofable `X-Forwarded-For`

References:

- `cmd/arxiv/middleware.go:195-203`

The limiter keys directly on the first `X-Forwarded-For` value. Since host port 80 is publicly exposed, a client that bypasses Cloudflare can spoof a new IP per request and avoid limits.

Fix direction:

- Prefer `CF-Connecting-IP` only when the request is known to come from Cloudflare.
- Block direct origin access at firewall/security-group level, or bind the origin to localhost and require the tunnel.
- If direct access remains open, ignore untrusted forwarding headers.

### P2 - Search/API limits are inconsistent and sometimes unbounded

References:

- `cmd/arxiv/api.go:224-230`
- `cmd/arxiv/api.go:313-318`
- `cmd/arxiv/api.go:493-498`
- `cmd/arxiv/api.go:1197-1203`
- `cmd/arxiv/api.go:1341-1345`

Some endpoints cap limits (`quick` caps at 50), but others accept any positive integer and pass it into DB queries or SSE loops. On a public endpoint, `limit=100000` can turn an ordinary request into a large database read and response stream.

Fix direction:

- Add one shared `parseLimit(value, default, max)` helper.
- Apply hard caps consistently by endpoint type.
- Consider rejecting very short high-cardinality searches, such as one-character broad queries.

### P2 - Embedding worker polls expensive "missing embedding" queries every 10 seconds

References:

- `embedding_worker.go:31-39`
- `embedding_worker.go:233-249`
- `embedding_worker.go:251-261`

The worker wakes every 10 seconds, runs a `NOT EXISTS` query against `papers` and `embeddings`, then runs a separate count. Live logs show the query as slow even when there are no rows left to embed. The query selects `p.*`, which includes large columns like `abstract` and potentially `pdf_text`.

Fix direction:

- Use the existing `embedding_jobs` queue instead of repeatedly discovering missing work.
- Back off aggressively when no pending rows are found.
- Select only `id`, `title`, and `abstract`.
- Add a query-specific index or materialized work queue for missing embeddings.

### P2 - Public source/PDF/fetch paths need stricter validation and timeouts

References:

- `data_fetch.go:72-80`
- `data_fetch.go:186-195`
- `data_fetch.go:286-291`
- `cmd/arxiv/api.go:613-634`
- `cmd/arxiv/serve.go:781-790`

Several routes accept a paper ID, then build upstream arXiv URLs by string formatting. Some paths validate IDs (`isArxivID`), but API fetch paths do not. HTTP calls use `http.DefaultClient`, which has no global timeout.

Fix direction:

- Validate all user-supplied paper IDs with one canonical function before fetch/download/export redirects.
- Use `url.Values` or `url.QueryEscape` for arXiv API requests.
- Replace `http.DefaultClient` with a package client that has timeouts.
- Consider a public fetch queue with backpressure rather than inline upstream calls.

### P2 - Docker/deploy config is not production-safe yet

References:

- `docker-compose.yml:5-8`
- `docker-compose.yml:23-24`
- `Dockerfile:1`, `Dockerfile:10`
- `Dockerfile:21-23`
- `Dockerfile:31`
- `start.sh:2-4`

Issues:

- Compose contains hard-coded DB credentials.
- Live containers are not actually managed by Compose, and the live Postgres restart policy did not match the compose file.
- Base images use moving tags (`golang:1.24-alpine`, `python:3.11-slim`, `cloudflared:latest` live).
- Container runs as root.
- One shell script starts two long-running processes; if the embedding service dies, the main container can stay "healthy" from Docker's point of view.
- There is no app-level Docker healthcheck.

Fix direction:

- Move secrets into an env file or secret manager, and rotate the currently exposed values.
- Install/use one deployment manager, ideally Docker Compose plugin or systemd units, and make live state match source.
- Pin image tags/digests for production.
- Add `HEALTHCHECK` for the Go app and embedding service, or split the embedding service into its own container.
- Run as non-root where feasible.

### P2 - Browser templates insert API data with `innerHTML`

References:

- `cmd/arxiv/templates/author.html:143-151`
- `cmd/arxiv/templates/author.html:179-184`
- `cmd/arxiv/templates/author.html:198-204`
- `cmd/arxiv/templates/paper.html:546-549`

Several template scripts build HTML strings with API data. Paper titles and author strings originate from external metadata, so they should be treated as untrusted even if arXiv is generally well-behaved. `paper.html` also places graph tooltip title text directly into `innerHTML`.

Fix direction:

- Prefer DOM APIs and `textContent` for dynamic text.
- If HTML strings remain, pass all external fields through an `escapeHtml` helper before interpolation.
- Add a minimal CSP after inline scripts are refactored or nonced.

### P2 - Local-mode source file serving has a prefix-check traversal footgun

References:

- `cmd/arxiv/serve.go:873-876`

The code does:

```go
fullPath := filepath.Join(paper.SourcePath, filePath)
fullPath = filepath.Clean(fullPath)
if !strings.HasPrefix(fullPath, paper.SourcePath) { ... }
```

Prefix checks can be fooled by sibling paths that share the same prefix. This is currently gated by `localMode`, but it is still better to make the helper correct.

Fix direction:

- Use `filepath.Rel(paper.SourcePath, fullPath)` and reject paths where `rel == ".."` or starts with `../`.
- Also compare cleaned absolute paths.

### P3 - Cache layer can grow without a memory or entry cap

References:

- `cmd/arxiv/middleware.go:22-38`
- `cmd/arxiv/middleware.go:113-124`
- `cache.go:104-110`
- `cache_models.go:48-49`
- `cache_lru.go:25-28`

The HTTP cache stores successful GET responses in an unbounded map until expiry, with no max entry count or byte budget. The paper LRU is sized for 500k entries and stores full `Paper` structs, which can include `PDFText`.

Fix direction:

- Add byte and entry caps to HTTP cache.
- Do not cache large responses.
- Use a smaller DTO for the paper LRU, or avoid caching `PDFText`.

### P3 - Search/PDF paths use full scans and quadratic sorting

References:

- `search.go:221-251`
- `search_pdf.go:73-184`
- `authors.go:171-178`
- `authors.go:649-656`
- `authors.go:674-681`

There are several hand-written O(n^2) sorts and in-memory full scans. They are acceptable for tiny data, but production has 644k papers.

Fix direction:

- Replace manual sorts with `sort.Slice`.
- Push category counts, author counts, and PDF search into indexed database queries where practical.
- Add cheap caps before expensive fuzzy matching.

### P3 - Code duplication and drift are making fixes harder

Examples:

- `ParseAuthors` in `authors.go:11-24` and `parseAuthors` in `cmd/arxiv/serve.go:930-944`.
- `yearFromID` is duplicated inside `citations.go:270-283` and `citations.go:389-402`.
- Several handlers repeat limit parsing and method checks.
- SQL placeholder handling is ad hoc across GORM and `database/sql`.

Fix direction:

- Centralize author parsing, arXiv ID validation, limit parsing, and DB placeholder handling.
- Add a small service layer for "paper fetch", "author profile", "citation graph" so handlers stay thin.

## File Tree Reviewed

```text
.
- .dockerignore
- .gitignore
- CLAUDE.md
- CONTRIBUTING.md
- Dockerfile
- Makefile
- README.md
- authors.go
- cache.go
- cache_lru.go
- cache_models.go
- cache_paper.go
- citations.go
- citations_refs.go
- cmd/
  - arxiv/
    - api.go
    - doc.go
    - main.go
    - middleware.go
    - robots.txt
    - serve.go
    - templates/
      - admin_embeddings.html
      - api.html
      - author.html
      - categories.html
      - category.html
      - error.html
      - foot.html
      - head.html
      - index.html
      - paper.html
      - search.html
  - migrate/
    - main.go
- data_download.go
- data_fetch.go
- data_oai.go
- data_sync.go
- docker-compose.yml
- docs/
  - API.md
  - REVIEW.md
  - SEMANTIC_SEARCH.md
  - SETUP.md
  - STRUCTURE.md
  - TESTING.md
- embedding_worker.go
- export.go
- export_sitemap.go
- go.mod
- go.sum
- search.go
- search_embeddings.go
- search_fts.go
- search_pdf.go
- start.sh
- tools/
  - README.md
  - embedding_service.py
  - generate_embeddings.py
  - query_embedding.py
  - sse_loadtest.py
```

## Current Repo State

`git status --short --branch` reported:

```text
## main...origin/main [ahead 3]
 M CLAUDE.md
 M cache.go
 M cmd/arxiv/api.go
 M cmd/arxiv/middleware.go
 M cmd/arxiv/serve.go
 M search.go
```

Important: the live image is about 3 months old, while local `main` is ahead of `origin/main` and modified. Any redeploy should first decide whether these local changes are intended production changes.

## Verification

Commands run:

```text
go test ./...
go vet ./...
```

Results:

```text
? github.com/lantos1618/arxiv.gg             [no test files]
? github.com/lantos1618/arxiv.gg/cmd/arxiv   [no test files]
? github.com/lantos1618/arxiv.gg/cmd/migrate [no test files]
```

`go vet ./...` completed without output.

There are currently no Go test files. The test pass is mostly a compile check, not behavioral coverage.

## Suggested Patch Order

### First hour

1. Fix `citations.go` Postgres placeholders and stop swallowing the query error.
2. Add a real app `/health` endpoint, separate from the embedding service health endpoint.
3. Put auth in front of `/admin/*`, `/api/v1/embeddings/generate`, and `/api/v1/authors/build-graph`.
4. Remove rate-limit bypass for `/generate`.

### Today

1. Add `pg_trgm` and a GIN trigram index on `papers.authors` as a quick production mitigation.
2. Add hard caps to all `limit` query parameters.
3. Make the embedding worker back off when no work remains and select only needed columns.
4. Validate and escape arXiv IDs before upstream fetches.

### This week

1. Normalize authors into their own indexed table.
2. Split or supervise the embedding service separately.
3. Install/use a single deployment manager and make the source-controlled deployment match live containers.
4. Rotate exposed Cloudflare and DB credentials.
5. Add focused tests for:
   - Postgres placeholder behavior.
   - arXiv ID validation.
   - rate limiting with forwarded headers.
   - author query path.
   - citation graph endpoint.

## Box And Docker Update Plan For Approval

Do not run these blindly on production. Proposed safe sequence:

1. Snapshot current state:
   - Record `docker ps`, `docker inspect`, image IDs, env/mounts, and open ports.
   - Confirm DB volume backup/snapshot exists for `arxiv_postgres_data`.
2. Patch and test code locally:
   - Fix P1 bugs.
   - Add health endpoint and auth gate.
   - Run `go test ./...` and a smoke test against a disposable/local DB if possible.
3. Build a new image with a unique tag:
   - Avoid only using `latest`.
   - Keep the old image available for rollback.
4. Deploy with one manager:
   - Prefer Docker Compose plugin or systemd units.
   - Ensure app and Postgres restart policies are explicit.
   - Ensure `DATABASE_URL`, `ARXIV_CACHE`, network, ports, volumes, and healthchecks are captured in source-controlled deployment config, with secrets externalized.
5. Smoke test:
   - `/health`
   - `/`
   - `/api/v1/stats`
   - a known `/abs/{id}`
   - a known author profile
   - citation graph endpoint
6. Only then update host packages and Docker:
   - Review pending apt updates.
   - Avoid Docker daemon upgrades during peak traffic unless there is a maintenance window.
   - Reboot only after confirming container restart behavior.

## Good Things Already Present

- Clear Go package split by concern, despite some large files.
- `html/template` is used for server-rendered pages.
- Source downloading is disabled in public mode by default.
- Postgres has pgvector/HNSW indexes already.
- Recent changes are moving in the right direction: stats caching and lighter recent-paper SSE payloads reduce avoidable DB pressure.
