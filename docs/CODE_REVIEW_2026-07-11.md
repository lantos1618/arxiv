# Repository Code Review — 2026-07-11

> **ARCHIVED REVIEW SNAPSHOT.** This is the dated review and resolution record for the 2026-07-11 hardening pass, not a standing specification or proof of current production state. The findings below are retained for traceability and are marked resolved as of that pass. Current commands, architecture, and operational requirements live in `README.md` and the active topic guides in `docs/`.

## Scope

Six parallel reviewers inspected the repository file by file, including Go source and tests, HTML templates, Python and shell workers, SQL/deployment files, configuration, and documentation. Existing working-tree changes were reviewed in place and were not modified.

Excluded as generated, third-party, runtime, or secret material: compiled binaries (`arxiv`, `arxiv-server`, `migrate`, `bin/arxiv`), `get-pip.py`, `tools/__pycache__`, runtime logs, and `.env`. `cmd/arxiv/robots.txt` was reviewed separately and has no finding.

## Validation

- `go test ./...` passed.
- `go test -race ./...` passed.
- `go vet ./...` passed.
- `git diff --check` passed.
- Python/shell syntax validation performed by the deployment/tools reviewer passed.

Passing tests do not cover the production PostgreSQL, distributed-worker, proxy, OAuth, and deployment failure modes below.

## Resolution Status

All findings below were addressed on 2026-07-11. The fixes include fenced and renewable worker leases, atomic database/cache transitions, safer downloads and migrations, bounded public workloads, local-only development binding, HTTP/CSRF/error-hardening, authenticated embedding mutations, supervised worker startup, corrected deployment tooling, and updated operational documentation.

Regression tests were added for lease fencing and heartbeats, internal-error redaction, redirects and privileged authentication, feedback concurrency and validation, MCP limits and JSON-RPC validation, migration failures, cache/data synchronization, parsing/export behavior, download integrity, embedding worker authorization, and related race-sensitive paths.

Deployment notes:

- Apply `deploy/sql/2026-07-11-qwen-vector-not-null.sql` to PostgreSQL after cleaning any invalid NULL-vector rows.
- Set `QWEN_WORKER_TOKEN` for remote Qwen workers.
- Set `EMBEDDING_MUTATION_TOKEN` whenever `ENABLE_MINILM_EMBEDDING_SERVICE=true`.
- Local mode now binds to loopback only; expose it deliberately through an authenticated proxy if remote access is required.

## Priority 1 Findings (Resolved)

1. **Qwen leases are not fenced.** `qwen_jobs.go:481` and `qwen_jobs.go:524` complete/fail by job ID alone. An expired worker can overwrite work reclaimed by another worker. Require running status, lease owner/generation, and an unexpired lease, and verify one affected row. The Python worker also claims before model load and never renews leases (`tools/qwen_api_worker.py:79`, `tools/qwen_api_worker.py:173`, `tools/qwen_job_worker.py:110`).
2. **Ambiguous worker completion is converted to failure.** A timeout after a committed completion causes the worker to call fail and reverse successful state (`tools/qwen_api_worker.py:197`). Make completion idempotent and query/retry after ambiguous transport errors.
3. **Citation replacement is non-atomic and suppresses database errors.** Delete and insert failures are ignored (`citations.go:22`, `citations.go:30`), allowing stale or partial graphs to be reported as successful.
4. **Stale chunk embeddings are treated as current.** Readiness and search omit the source/text hash match (`qwen_jobs.go:591`, `search_embeddings.go:193`), so changed chunks can continue returning obsolete vectors.
5. **PostgreSQL FTS can silently return no results before vector backfill.** The fallback runs only on SQL errors, not valid zero-row results (`search.go:274`).
6. **Required PostgreSQL migration lock timeouts are treated as success.** Startup can continue without required schema objects (`cache.go:410`).
7. **Fetch can overwrite concurrent paper updates from an LRU snapshot.** A full-row unchecked `Save` can erase concurrent download/text fields and report success after failure (`data_fetch.go:34`).
8. **OAI sync advances its token before durable batch persistence.** A crash or later request error can permanently skip records (`data_sync.go:91`).
9. **Partial downloads are accepted as complete.** Existing PDF/source paths are trusted after crashes or concurrent writers (`data_download.go:127`). Use temporary paths plus atomic rename and integrity checks.
10. **Local mode disables privileged authentication while binding all interfaces.** LAN/public clients can reach admin, fetch, embedding, and Qwen worker operations (`cmd/arxiv/serve.go:259`, `cmd/arxiv/api.go:1003`, `cmd/arxiv/api.go:1333`). Bind local mode to loopback or retain auth.
11. **HTTP server timeouts are incomplete.** Missing `ReadHeaderTimeout` and `IdleTimeout` allow slow-header connection exhaustion (`cmd/arxiv/serve.go:263`).
12. **Migration tooling discards DDL and row errors.** The command can report success after losing schema objects or records (`cmd/migrate/main.go:111`, `cmd/migrate/main.go:176`, `cmd/migrate/main.go:214`, `cmd/migrate/main.go:250`, `cmd/migrate/main.go:273`).
13. **MCP batch size and cost are unbounded.** One rate-limited HTTP request can amplify into thousands of search/embedding operations (`cmd/arxiv/mcp.go:81`, `cmd/arxiv/mcp.go:92`).
14. **Deployment rollback is not immutable.** The runbook builds a dirty worktree and restarts the same mutable image as rollback (`docs/DEPLOYMENT_RUNBOOK_2026-05-15.md:33`, `docs/DEPLOYMENT_RUNBOOK_2026-05-15.md:89`).
15. **The app-only deployment runbook understates schema risk.** Every startup runs `AutoMigrate`, contradicting the runbook's no-schema-rewrite premise (`docs/DEPLOYMENT_RUNBOOK_2026-05-15.md:8`, `cache.go:189`).
16. **Embedding documentation can target production unexpectedly.** The documented cache path does not override an exported `DATABASE_URL`, and one documented flag is unsupported (`tools/README.md:19`, `docs/SETUP.md:66`, `tools/generate_embeddings.py:25`).

## Priority 2 Findings (Resolved)

1. Author parsing splits comma-formatted names into bogus people and collaboration edges (`authors.go:17`, `authors.go:290`).
2. Public fuzzy PDF search loads all text and performs expensive comparisons without cancellation checks (`search_pdf.go:75`, `search_pdf.go:132`).
3. Concurrent API-key rotation can commit multiple active replacement keys (`api_keys.go:59`).
4. Reference extraction uses the scanner's 64 KiB default and ignores `Scanner.Err` (`citations_refs.go:94`, `citations_refs.go:106`).
5. SQLite FTS rebuild deletes and reinserts outside a transaction (`search_fts.go:10`, `search_fts.go:15`).
6. Email-code login records a proven mailbox as unverified (`auth.go:133`).
7. Download-state DB errors are ignored before publishing success to the LRU (`data_download.go:54`).
8. OAI metadata upserts do not invalidate `paperLRU` (`data_sync.go:142`).
9. Deleted OAI records are not modeled or skipped correctly (`data_oai.go:95`).
10. The embedding worker ignores the `embedding_jobs` queue and its priority/retry state (`embedding_worker.go:277`).
11. A SQLite-accessible embedding update uses PostgreSQL-only `NOW()` (`embedding_worker.go:394`).
12. BibTeX escaping re-escapes inserted backslashes (`export.go:178`).
13. RIS author export does not match stored comma-separated author data (`export.go:209`).
14. Public cache headers are assigned before handler status is known, allowing downstream caches to retain failures (`cmd/arxiv/middleware.go:218`).
15. `safeNextPath` accepts backslash-based external redirects (`cmd/arxiv/auth_handlers.go:525`).
16. Reusable admin secrets are accepted in query parameters (`cmd/arxiv/admin_auth.go:183`).
17. Embedding subprocesses ignore request cancellation and have no deadline (`cmd/arxiv/api.go:1773`).
18. Graceful shutdown ignores `SIGTERM` and uses an unbounded shutdown context (`cmd/arxiv/main.go:32`, `cmd/arxiv/serve.go:266`).
19. Public API handlers expose raw DB/service/subprocess errors (`cmd/arxiv/api.go:445` and repeated call sites).
20. Feedback account limits use a count-then-insert race (`feedback.go:58`, `feedback.go:77`).
21. Feedback vote toggle/delete operations are non-atomic and lack cascade protection (`feedback.go:270`, `feedback.go:285`).
22. Invalid vote strings default to an upvote (`cmd/arxiv/feedback_handlers.go:69`).
23. The admin embedding Stop button does not abort its fetch or backend work (`cmd/arxiv/templates/admin_embeddings.html:286`, `cmd/arxiv/templates/admin_embeddings.html:362`).
24. Invalid vectors are stored as hash-current NULL rows and later counted as complete (`tools/qwen_embeddings_v2.py:165`, `tools/qwen_chunk_embeddings_v2.py:90`, `tools/qwen_pipeline_check.py:67`).
25. Full-paper worker claims are not atomic across workers (`tools/fetch_full_paper_text.py:125`).
26. The MiniLM service binds publicly and exposes unauthenticated DB-writing endpoints (`tools/embedding_service.py:212`, `tools/embedding_service.py:321`).
27. Chunk settings with overlap greater than or equal to size can loop forever (`tools/chunk_full_papers.py:86`).
28. The Qwen worker token is passed in process arguments (`tools/run_ovh_qwen_worker.sh:27`).
29. GPU launch gating is a check-then-launch race across orchestrators (`tools/qwen_jit_orchestrator.sh:21`, `tools/qwen_jit_orchestrator.sh:45`).
30. Startup sleeps for three seconds instead of supervising embedding service readiness/liveness (`start.sh:5`).
31. `.env.example` enables trusted proxy headers without documenting the trusted-origin requirement (`.env.example:7`).
32. Go prerequisites conflict across docs and `go.mod` (`CONTRIBUTING.md:13`, `docs/SETUP.md:11`, `CLAUDE.md:41`, `go.mod:3`).
33. Documented API rate limits differ by a factor of ten from runtime configuration (`docs/API.md:489`).
34. Testing docs describe missing tests and omit production-backend validation (`docs/TESTING.md:7`).
35. The documented Qwen uvicorn module path is wrong from the repository root (`tools/README.md:67`).

## Priority 3 Findings (Resolved)

1. Admin refreshes use unbounded background contexts while serializing refreshes (`admin_stats.go:103`, `admin_stats.go:197`).
2. Recent-user statistics perform one session query per user (`admin_stats.go:423`).
3. Sitemap URLs can contain double slashes when `SITE_URL` has a trailing slash (`export_sitemap.go:34`).
4. Cookie-authenticated key rotation, logout, feedback, and moderation lack CSRF/origin validation (`cmd/arxiv/auth_handlers.go:213`, `cmd/arxiv/feedback_handlers.go:17`, `cmd/arxiv/feedback_handlers.go:284`).
5. Invalid admin cookies override otherwise valid explicit credentials (`cmd/arxiv/admin_auth.go:169`).
6. Disconnected SSE clients retain ten-minute `time.After` timers (`cmd/arxiv/api.go:2259`).
7. MCP accepts missing JSON-RPC versions and empty batches (`cmd/arxiv/mcp.go:94`, `cmd/arxiv/mcp.go:140`).
8. Oversized feedback is silently truncated rather than rejected (`feedback.go:324`).
9. The conditional reindex script still fails when the index is absent (`deploy/sql/2026-05-29-papers-categories-reindex.sql:7`).
10. Make targets omit a required argument, mask install failures, and map the wrong container port (`Makefile:74`, `Makefile:83`).
11. API docs omit production auth and queued-response behavior for privileged embedding/fetch endpoints (`docs/API.md:162`, `docs/API.md:386`).

## File-by-File Checklist

### Root Go and Tests

- [x] `admin_stats.go`, `api_keys.go`, `auth.go`, `authors.go`, `cache.go`, `cache_lru.go`, `cache_models.go`, `cache_paper.go`, `citations.go`, `citations_refs.go`, `data_download.go`, `data_fetch.go`, `data_oai.go`, `data_sync.go`, `detail_cache.go`, `doc.go`, `embedding_models.go`, `embedding_worker.go`, `export.go`, `export_sitemap.go`, `feedback.go`, `qwen_jobs.go`, `search.go`, `search_embeddings.go`, `search_fts.go`, `search_pdf.go`, `sql_helpers.go`, `user_views.go`
- [x] `auth_test.go`, `data_fetch_test.go`, `data_sync_test.go`, `export_sitemap_test.go`, `feedback_test.go`, `qwen_jobs_test.go`, `search_embeddings_test.go`

### Commands and Templates

- [x] `cmd/arxiv/admin_auth.go`, `cmd/arxiv/admin_handlers.go`, `cmd/arxiv/api.go`, `cmd/arxiv/auth_handlers.go`, `cmd/arxiv/build_info.go`, `cmd/arxiv/doc.go`, `cmd/arxiv/feedback_handlers.go`, `cmd/arxiv/main.go`, `cmd/arxiv/mcp.go`, `cmd/arxiv/middleware.go`, `cmd/arxiv/serve.go`, `cmd/migrate/main.go`
- [x] `cmd/arxiv/admin_auth_test.go`, `cmd/arxiv/feedback_handlers_test.go`, `cmd/arxiv/mcp_test.go`, `cmd/arxiv/seo_test.go`
- [x] `cmd/arxiv/templates/account.html`, `admin.html`, `admin_audit.html`, `admin_embeddings.html`, `admin_feedback.html`, `admin_users.html`, `api.html`, `author.html`, `categories.html`, `category.html`, `error.html`, `foot.html`, `head.html`, `index.html`, `login.html`, `paper.html`, `search.html`
- [x] `cmd/arxiv/robots.txt`

### Tools, SQL, and Deployment

- [x] All source files under `tools/` except vendored `get-pip.py`; all SQL under `deploy/sql/`; both systemd units/scripts; `deploy/qwen-jit-orchestrator.service`
- [x] `Dockerfile`, `Dockerfile.qwen-api-worker`, `docker-compose.yml`, `Makefile`, `start.sh`, `requirements-qwen-worker.txt`, `tools/requirements.txt`

### Documentation and Configuration

- [x] `README.md`, `CONTRIBUTING.md`, `CLAUDE.md`, `LICENSE`, `go.mod`, `.env.example`, `.dockerignore`, `.gitignore`, `tools/README.md`
- [x] All Markdown files under `docs/`, plus `docs/SITE_MAP.dot`
