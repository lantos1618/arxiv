# arxiv.gg Performance Report - 2026-05-15

> **ARCHIVED MEASUREMENT SNAPSHOT.** Timings, Lighthouse scores, traffic shape, database size, and bottlenecks below were measured on 2026-05-15 and are not current service-level claims. Retain them only as a historical baseline; rerun measurements against the present release before making performance decisions.

## Scope

Measurements were taken against the live public site at `https://arxiv.gg` after the production hardening redeploy on 2026-05-15. These are synthetic checks from the production host through the public Cloudflare route, plus one Lighthouse run. They are useful as a baseline, but they are not a substitute for real-user monitoring from multiple regions and devices.

## Current Status

The site is live and healthy:

- `/health` returns DB-connected status.
- Homepage, paper page, search API, quick-search API, and citation API return successfully.
- `arxiv-container` is healthy.
- `arxiv-postgres` was not recreated; the existing volume and embeddings were preserved.

## Timing Snapshot

Five `curl` samples per endpoint, measured via the public `https://arxiv.gg` route:

| Endpoint | Total Time Range | TTFB Range | Observed Payload |
|---|---:|---:|---:|
| `/` | 65-107 ms | 65-107 ms | 24.2 KB |
| `/health` | 60-94 ms | 60-94 ms | 56 B |
| `/api/v1/stats` | 86-97 ms | 86-96 ms | 159 B |
| `/api/v1/search?q=transformer&limit=10` | 83-95 ms | 82-95 ms | 18.5 KB |
| `/api/v1/search/quick?q=transformer&limit=5` | 75-104 ms | 74-104 ms | 10.7 KB |
| `/abs/2603.27086` | 92-98 ms | 86-94 ms | 29.4 KB |

Browser navigation timings from Playwright:

| Page | Wall Time | Response Start | DOMContentLoaded | Load |
|---|---:|---:|---:|---:|
| `/` | 374 ms | 49 ms | 66 ms | 298 ms |
| `/search?q=transformer` | 608 ms | 355 ms | 438 ms | 548 ms |
| `/abs/2603.27086` | 278 ms | 45 ms | 216 ms | 222 ms |

Lighthouse homepage scores:

| Category | Score |
|---|---:|
| Performance | 88 |
| Accessibility | 91 |
| Best Practices | 96 |
| SEO | 82 |

Important Lighthouse metrics:

| Metric | Value |
|---|---:|
| First Contentful Paint | 0.9 s |
| Largest Contentful Paint | 2.4 s |
| Speed Index | 1.1 s |
| Total Blocking Time | 410 ms |
| Time to Interactive | 4.8 s |
| Cumulative Layout Shift | 0 |
| Root document response time | 30 ms |
| Total byte weight | 475 KiB |

After the follow-up fixes, the homepage Lighthouse baseline improved to:

| Category | Score |
|---|---:|
| Performance | 100 |
| Accessibility | 100 |
| Best Practices | 100 |
| SEO | 91 |

| Metric | Value |
|---|---:|
| First Contentful Paint | 0.8 s |
| Largest Contentful Paint | 1.4 s |
| Speed Index | 0.8 s |
| Total Blocking Time | 60 ms |
| Time to Interactive | 1.4 s |
| Cumulative Layout Shift | 0 |
| Root document response time | 20 ms |
| Total byte weight | 53 KiB |
| DOM size | 786 elements |

## Fix Applied During This Pass

Search logs exposed a real production bug:

```text
expected 2 arguments, got 0
```

The full-text search path and quick-search path were using Postgres `$1` placeholders inside GORM `Raw(...)` calls. GORM expects `?` placeholders and rewrites them for the active dialect. Using `$1` directly caused malformed SQL and forced search to fall back to slower/worse behavior.

Patch:

- `searchPostgres` now uses `?` placeholders and passes the repeated query argument explicitly.
- `QuickSearch` now uses `?` placeholders for the GORM raw query and passes all repeated arguments explicitly.

Observed impact for `q=transformer&limit=10`:

- Before: roughly 126-179 ms total, 371 KB observed response, and production error log noise.
- After: roughly 83-95 ms total, 18.5 KB observed response, with no fallback error in the follow-up log scan.

## Follow-Up Fixes

The next pass applied low-risk fixes from the findings above:

- Lazy-loaded Google Analytics after page load/idle time.
- Lazy-loaded MathJax only when page text appears to contain TeX-style math.
- Added `lang`, meta description, favicon route, and favicon link.
- Increased author/category tap targets.
- Capped author links in list cards to 12 visible authors with a `+N more` indicator.
- Reduced category/list page DB payloads by selecting only fields needed for list rendering.
- Added Postgres indexes for category trigram matching, recent source-backed listing, and pending embedding scans.
- Fixed response-cache header preservation and bypassed response caching for admin/authenticated requests.
- Reordered the Dockerfile runtime stage so Go/template-only rebuilds can reuse the expensive Python dependency layer.

The category query sampled in the original report improved from roughly 630 ms in logs to about 19 ms in `EXPLAIN ANALYZE` after `idx_papers_categories_trgm`.

## Main Findings

### Backend Is Generally Fast

The origin path is not the main bottleneck for common pages. Most public endpoints are returning in about 60-110 ms from the current test location. Paper pages are also quick.

### Search Page Is Slower Than The API

The search API is now fast, but the browser-rendered search page showed a 355 ms response start and 548 ms load. That is still acceptable, but it is the slowest page in this small sample.

Likely contributors:

- Server-rendered search work on page load.
- Client-side stream/search JavaScript initialization.
- Large result markup when abstracts/authors are long.

### Homepage Front-End Weight Is The Main Lighthouse Issue

Lighthouse found the backend response is short, but the page pays front-end cost:

- MathJax loads on the homepage even when only a subset of content needs it.
- Google Analytics loads about 162 KB transferred / 478 KB resource size.
- Lighthouse estimated 174 KiB of unused JavaScript.
- Time to Interactive is 4.8 s, mostly due to script work rather than server latency.

### DOM Is Too Large On Some Result Lists

Lighthouse reported 2,871 DOM elements. The worst case is a paper with a very large author list creating 1,351 child elements inside one `.paper-authors` block.

This hurts:

- Layout cost.
- Tap target spacing.
- Mobile readability.
- Lighthouse DOM-size score.

### Small SEO/Accessibility Fixes Are Available

Low-risk polish items:

- Add `lang` to `<html>`.
- Add a meta description.
- Add a favicon to stop `/favicon.ico` 404 console errors.
- Increase tap target height/spacing for author/category links.
- Review `robots.txt`: Lighthouse flags `Content-Signal: search=yes,ai-train=no` as an unknown directive.

### Database Hot Spots Remain

The author trigram index helped, but author searches still show frequent 200-400 ms queries and occasional multi-second outliers. This is expected while authors remain a denormalized text field.

Other log-side hot spots:

- Category/source queries can be slow, for example category `ILIKE` plus `src_downloaded = true`.
- Embedding-worker pending-paper anti-join can take around 800 ms when nearly all papers already have embeddings. The new idle backoff makes this less noisy, but the query itself can still be improved.

## Recommended Next Work

### Quick Wins

1. Consider removing Google Analytics from the critical path entirely, or rely more on Cloudflare/server analytics.
2. Add cache headers for local static assets; consider self-hosting versioned MathJax if it stays.
3. Re-run Lighthouse from an external region after Cloudflare caches settle.

### Data/DB Work

1. Normalize authors into indexed rows and search against that table.
2. Add a category index or normalized categories table for category pages.
3. Replace the embedding-worker anti-join with a pending queue or indexed status column.
4. Enable `pg_stat_statements` so slow-query research can use real aggregate query stats instead of log sampling.

### Observability

1. Add structured request timing logs: path, status, duration, DB duration if possible.
2. Add uptime/latency monitoring for `/health`, homepage, search, and one paper page.
3. Add real-user timing beacons for navigation/load metrics if we want user-region data.
