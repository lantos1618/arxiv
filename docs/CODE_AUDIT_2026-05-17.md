# arxiv.gg Code Audit - 2026-05-17

> **ARCHIVED SNAPSHOT — NOT A CURRENT DEFECT LIST.** This document records observations made on 2026-05-17. Many referenced paths and behaviors have since changed. Use `README.md`, the active topic guides in `docs/`, and `CODE_REVIEW_2026-07-11.md` for disposition; verify every historical finding against current code before acting on it.

Scope: tracked source, templates, scripts, deployment config, docs signals, and visible local workspace hygiene. Excluded `.git` internals and binary contents, but noted tracked/untracked binaries and generated files.

Verification run:

- `go test ./...` passed.
- `go vet ./...` passed.
- `python3 -m py_compile tools/*.py` passed.
- `gofmt -l $(git ls-files '*.go')` reported unformatted files: `authors.go`, `cache_lru.go`, `cache_models.go`, `export.go`, `search_pdf.go`.

## Highest Priority Findings

### P0 - OAI sync can erase local download/cache state

`data_sync.go:124-129` calls `Save(papers)` on partial `Paper` structs returned by OAI. Those structs do not include local-only fields like `pdf_path`, `src_path`, `pdf_text`, `pdf_downloaded`, `src_downloaded`, and `fetched_at`, so an incremental metadata sync can overwrite existing rows with zero values.

Impact:

- Papers with source/PDF already cached can be marked as not downloaded.
- Category pages still filter by `src_downloaded = true`, so pages can disappear from category views.
- `fetched_at` ordering can be lost, making homepage/recent behavior confusing.

Fix:

- Replace `Save(papers)` with an upsert that updates only metadata columns: `created`, `updated`, `title`, `abstract`, `authors`, `categories`, `comments`, `journal_ref`, `doi`, `license`, `metadata_updated`.
- Add a regression test where an existing paper with `src_downloaded=true`, paths, and `fetched_at` survives `insertPapers`.

### P1 - Public PDF search can load and scan every PDF text blob

`cmd/arxiv/api.go:746-773` exposes `/api/v1/search/pdf`. `search_pdf.go:73-165` selects `id`, `pdf_path`, and full `pdf_text` for every downloaded PDF, then scans all text in Go for every request. Fuzzy mode is even heavier.

Impact:

- A few public requests can create large DB reads, memory pressure, and CPU spikes.
- This gets worse as PDF coverage grows.

Fix:

- Disable this endpoint publicly until it is backed by Postgres full-text search over `pdf_text`.
- At minimum require a longer query, lower rate limit, and select candidate rows in SQL before loading text.

### P1 - Startup schema changes ignore errors and can lock production tables

`cache.go:199-263` runs many DDL statements at app startup and ignores every `Exec` error. This includes regular `CREATE INDEX`, GIN, and HNSW indexes. On a large production table, non-concurrent index creation can lock or stall deploys; if pgvector/index creation fails, the app still logs success.

Impact:

- Deploy can appear healthy while search/vector indexes failed.
- New or rebuilt deployments can block writes or take a long time during startup.

Fix:

- Move heavy DDL to explicit migrations.
- Use `CREATE INDEX CONCURRENTLY` where Postgres allows it.
- Return/log individual DDL errors clearly.

### P1 - Category pages still hide warmed metadata-only papers

`search.go:300-305` filters `ListPapers` with `src_downloaded = true`. The homepage was fixed to show warmed metadata-only papers, but category pages still exclude them.

Impact:

- Freshly fetched `/abs/{id}` pages can exist, be embedded, and be in the sitemap while not appearing on `/category/{cat}`.
- This weakens internal linking and SEO discovery for the new 100-paper warming flow.

Fix:

- Match the homepage rule: show rows with useful metadata, not only downloaded source.
- For category pages, use something like `title != ''` plus category filter.

## Medium Priority Findings

### P2 - Homepage embedding badges are computed but dropped by the client

`cmd/arxiv/api.go:1343-1353` sends `hasEmbedding` as a top-level SSE field. `cmd/arxiv/templates/index.html:60-91` calls `renderRecentPaper(data.paper)`, and that renderer only checks `p.hasEmbedding` or `p.HasEmbedding`.

Impact:

- Live data says warmed papers have embeddings, but the homepage may not show the embedding badge.

Fix:

- Pass `renderRecentPaper({...data.paper, hasEmbedding: data.hasEmbedding})` for both initial and new events.

### P2 - Batch metadata fetch lacks timeout, User-Agent, and HTML fallback

`data_fetch.go:83-101` builds an arXiv export URL manually and uses `http.DefaultClient.Do` without a client timeout or User-Agent. Single-paper fetch has a 2s Atom timeout and HTML fallback; batch fetch does not.

Impact:

- Batch reference prefetch can hang under upstream slowness.
- arXiv can throttle or reject requests without a clear site User-Agent.
- 429/503 handling is worse than single-paper fetch.

Fix:

- Use `url.Values`, set `User-Agent`, and wrap the request in a timeout.
- Consider falling back per-ID or retrying gently on 429/503.

### P2 - Export escaping is broken for BibTeX special characters

`export.go:180-188` escapes special characters, then escapes backslashes last. That converts the backslashes it just inserted into `\textbackslash{}` sequences, corrupting fields containing `{`, `$`, `_`, `%`, etc.

Impact:

- BibTeX exports for LaTeX-heavy titles/abstracts can be invalid or ugly.

Fix:

- Escape literal backslashes first, or use a dedicated BibTeX escaping routine.
- Add tests for `$`, `_`, `{}`, `%`, and existing LaTeX macros.

### P2 - SQLite embedding CLI ignores its positional cache dir

`tools/generate_embeddings.py:25-45` accepts a `cache_dir` argument, but `get_db_connection()` ignores it for SQLite and uses `ARXIV_CACHE` or `/data/arxiv`.

Impact:

- Local users can run `python3 tools/generate_embeddings.py /some/cache` and silently write/read the wrong SQLite DB.

Fix:

- Pass `cache_dir` into `get_db_connection(cache_dir)`.

### P2 - Unwanted admin surface still exists

`cmd/arxiv/serve.go:165-166` still mounts `/admin/embeddings`, and `cmd/arxiv/serve.go:1791-1818` renders the page. It is protected by the admin token, but product direction is now public paper embedding actions with no login/admin UI.

Impact:

- Extra secret-bearing code path and template to maintain.
- Confusing with the current “dumb public embed/render site” direction.

Fix:

- Remove the admin route/template if bulk embedding management is not part of the product.
- Keep machine-only protected maintenance endpoints only if truly needed.

## Ops And Repo Hygiene

### Local secrets exist in `.env`

`.env` is ignored by Git, but it contains real local production-looking credentials. Do not quote or copy values into reports. Any token pasted into chat or logs should be rotated.

Fix:

- Rotate the Cloudflare token that was pasted into chat.
- Keep `.env` local only and move production secrets into the deployment secret mechanism.

### Tracked generated/vendor artifacts

`git ls-files` shows `get-pip.py` and `migrate` are tracked. The workspace also contains untracked local binaries: `arxiv`, `arxiv-server`, and `bin/arxiv`.

Impact:

- Repo noise, larger diffs/clones, and stale binaries that do not represent source truth.

Fix:

- Remove tracked binary/bootstrap artifacts from Git history going forward where possible.
- Keep generated binaries ignored and build them in CI/deploy.

### `robots.txt` file is probably dead in Docker

`cmd/arxiv/robots.txt` exists, but `Dockerfile:18-30` only copies tools, built binaries, and `start.sh` into `/app`. `cmd/arxiv/serve.go:1195-1212` serves `robots.txt` only from the runtime working directory, otherwise fallback text.

Impact:

- A repo edit to `cmd/arxiv/robots.txt` will not necessarily change production robots output.

Fix:

- Embed robots text with `go:embed`, copy it to `/app/robots.txt`, or remove the unused file and keep the code fallback as source of truth.

### Compose still does not manage the tunnel

`docker-compose.yml` manages Postgres and the app, but not `cloudflared`. The live `cloudflared` container is still outside this compose file.

Impact:

- Deployment manager is only partial; app/Postgres and tunnel lifecycle can drift.

Fix:

- Add a `cloudflared` service using externalized tunnel credentials, or document the intentional split.

## Lower Priority Cleanups

- Module path and public GitHub links now point at `github.com/lantos1618/arxiv.gg`.
- `SearchSemantic` assumes Postgres/pgvector; local SQLite with embeddings will fail at query time. Gate it by DB type.
- `BuildAuthorGraph` clears `author_collaborations` before a full rebuild. If the rebuild fails mid-run, author graph data is empty. Build into a temp table and swap.
- Several manual O(n^2) sorts exist in author/category/PDF search code. Fine at small sizes, but easy to replace with `sort.Slice`.
- `handleAPIEmbeddingWorkerStatus` can leave a non-200 embedding-service response body unclosed.

## Recommended Fix Order

1. Fix OAI sync upsert to preserve local state, then add the regression test.
2. Make category pages include metadata-only warmed papers with title/category.
3. Disable or SQL-index PDF text search before it becomes a public load problem.
4. Move startup DDL into explicit migrations and stop ignoring DDL errors.
5. Remove the admin page/route if it is no longer product surface.
6. Fix homepage embedding badges and BibTeX escaping.
