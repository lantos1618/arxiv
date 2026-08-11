# arxiv.gg API

The JSON API is rooted at `/api/v1/`; MCP uses `/mcp`. Successful JSON endpoints generally return `{"success":true,"data":...}` and failures return `{"success":false,"error":"..."}`. Public internal failures are redacted; inspect server logs for operational detail.

## Authentication

- Public reads need no credential unless noted.
- Account API keys use `Authorization: Bearer arxivgg_...` and are created/regenerated from `/account` after Google sign-in.
- Admin API operations accept an admin Google session, `X-Admin-Token`, or `Authorization: Bearer <ADMIN_TOKEN>`.
- Remote Qwen workers use `X-Qwen-Worker-Token` or bearer `QWEN_WORKER_TOKEN`; an admin credential is also accepted.
- Query-string admin secrets are not accepted.
- In `serve -local`, privileged checks are bypassed and the server binds to loopback.

Cookie-authenticated mutations require a same-origin request. Use headers, not cookies, for agents and remote workers.

## Paper Endpoints

| Method | Path | Purpose | Access |
|---|---|---|---|
| `GET` | `/api/v1/papers/{id}` | Paper metadata, reference list, cited-by count | Public |
| `GET` | `/api/v1/papers/{id}/citations` | Extracted references | Public |
| `GET` | `/api/v1/papers/{id}/cited-by?limit=50` | Citing papers; max 200 | Public |
| `GET` | `/api/v1/papers/{id}/graph` | Citation graph | Public |
| `GET` | `/api/v1/papers/{id}/similar?limit=80` | Qwen neighbors and semantic map; max 160 | Public |
| `GET` | `/api/v1/papers/{id}/embedding-status` | Qwen abstract/map readiness and job state | Public |
| `POST` | `/api/v1/papers/{id}/fetch` | Fetch metadata/source/PDF | Admin |
| `POST` | `/api/v1/papers/{id}/embeddings` | Generate or queue Qwen paper work | Public, separately rate-limited |
| `GET` | `/api/v1/papers/{id}/export/{format}` | `bibtex`, `ris`, or `json` export | Public |

Fetch query options are `pdf=true`, `source=false` (source defaults true), and `embedding=true`. Fetch is a privileged production mutation even when the paper is already public on arXiv.

`POST /papers/{id}/embeddings` may return HTTP 200 when work finishes synchronously or HTTP 202 when queued:

```json
{
  "success": true,
  "data": {
    "paperId": "1706.03762",
    "hasEmbedding": false,
    "mapReady": false,
    "queued": true,
    "status": {},
    "statusUrl": "/api/v1/papers/1706.03762/embedding-status",
    "message": "Qwen worker is not warm; embedding work queued."
  }
}
```

Poll `statusUrl`. Do not treat HTTP 202 or `queued:true` as completion.

## Search Endpoints

| Method | Path | Parameters | Notes |
|---|---|---|---|
| `GET` | `/api/v1/search` | `q`, `category`, `limit` (default 20, max 100) | Metadata full-text search |
| `GET` | `/api/v1/search/quick` | `q`, `limit` (default 10, max 50) | Fast paper/author suggestions |
| `GET` | `/api/v1/search/semantic` | `q`, `limit` (default 20, max 100) | Qwen abstract search with explicit Quick fallback |
| `GET` | `/api/v1/search/pdf` | `q`, `limit` (max 50), `fuzzy=true` | Signed-in/API-key Deep Search over available extracted PDF text; rate and concurrency limited |
| `GET` | `/api/v1/search/stream` | `q`, `category`, `limit`, `mode` | SSE modes `quick`, `search`, and `deep` |

Semantic success identifies `mode:"semantic"` and `model:"Qwen/Qwen3-Embedding-8B"`. When the Qwen catalog or execution path is unavailable, the same endpoint returns HTTP 206 Quick matches with `mode:"quick"`, `requestedMode:"semantic"`, structured fallback metadata, and no retry recommendation. A retry is recommended only when a monitored asynchronous worker is configured and the query was queued.

```json
{
  "success": true,
  "data": {
    "results": [{"paperId":"...","paper":{},"similarity":null,"fallback":true}],
    "papers": [],
    "count": 1,
    "query": "...",
    "mode": "quick",
    "model": "quick",
    "fallback": true,
    "notice": "Idea search is unavailable; showing Quick matches."
  }
}
```

Always inspect `data.fallback` before consuming similarity scores.

The SSE endpoint emits `start`, `status`, `fallback`, `result`, `error`, and `complete` events as JSON in `data:` lines. `mode=semantic` uses Qwen abstract search; `mode=deep` uses prepared Qwen full-paper chunks and requires an authenticated session or account API key. Omitted mode defaults to Quick. The legacy `search` alias remains accepted, but clients should use `quick`, `semantic`, or `deep`.

```bash
curl -N --get 'https://arxiv.gg/api/v1/search/stream' \
  --data-urlencode 'q=robust control' \
  --data 'mode=semantic&limit=20'
```

## Catalog And Author Endpoints

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/categories` | Category list |
| `GET` | `/api/v1/stats` | Catalog/download/Qwen/SSE coverage signals |
| `GET` | `/api/v1/authors/profile?author=...` | Author profile |
| `GET` | `/api/v1/authors/collaborators?author=...&limit=100` | Collaborators; max 200 |
| `GET` | `/api/v1/authors/similar?author=...&limit=10` | Similar authors; max 50 |
| `GET` | `/api/v1/authors/stats?author=...` | Author statistics |
| `GET` | `/api/v1/authors/graph?author=...&depth=1` | Collaboration graph; depth 1 or 2 |
| `POST` | `/api/v1/authors/build-graph` | Rebuild author graph/embeddings; admin only |
| `GET` | `/api/v1/papers/recent/stream` | SSE stream of newly fetched papers |

## Pipeline Status And Retired Endpoint

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/embeddings/generate` | Retired MiniLM bulk route; authenticated calls return HTTP 410 |
| `GET` | `/api/v1/embeddings/status` | Current Qwen model, dimension, service state, and coverage |

The retired POST route exists only to give old clients an explicit migration response. Generate or queue a canonical profile with `POST /api/v1/papers/{id}/embeddings`.

## Qwen Worker API

Remote workers use:

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/qwen/jobs/claim` | Claim up to 8 `query`/`abstract` jobs |
| `POST` | `/api/v1/qwen/jobs/{leased-id}/heartbeat` | Renew an active fenced lease |
| `POST` | `/api/v1/qwen/jobs/{leased-id}/complete` | Store a vector for the active lease/source hash |
| `POST` | `/api/v1/qwen/jobs/{leased-id}/fail` | Record failure for the active lease |

Claim bodies include `kinds`, `limit`, `leaseOwner`, and `leaseSeconds` (maximum 3,600). Completion, failure, and heartbeat must return the claimed lease owner/generation; completion also validates source hash. A stale or ambiguous worker must retry/query safely and must not overwrite a reclaimed job.

Use `tools/qwen_api_worker.py` rather than implementing this protocol ad hoc.

## MCP

`/mcp` implements Streamable HTTP JSON-RPC with protocol version `2025-06-18`. `GET /mcp` returns discovery metadata; `POST /mcp` handles `initialize`, `ping`, `tools/list`, and `tools/call`.

Tools currently exposed are:

- `arxiv_api_overview`
- `arxiv_account`
- `arxiv_search`
- `arxiv_get_paper`
- `arxiv_related_papers`

Public search/paper tools work without an account. Deep Search and account details require an authenticated cookie or bearer account API key.

Example Codex configuration:

```toml
[mcp_servers.arxiv_gg]
url = "https://arxiv.gg/mcp"
http_headers = { Authorization = "Bearer arxivgg_..." }
```

Requests are capped at 256 KiB. Batches contain at most 20 requests and a maximum aggregate cost of 24. Empty batches and requests without JSON-RPC `2.0` are rejected.

## Limits And Caching

- Global HTTP limit: 1,000 requests per minute per client IP.
- Public single-paper embedding requests: 6 per minute per client IP.
- Login: 12 attempts per 10 minutes per client IP.
- Feedback mutations: 30 per minute per client IP, plus per-account post limits.
- Limit violations return HTTP 429.

Client IP comes from the socket unless `TRUST_PROXY_HEADERS=true` and the direct peer is loopback or a private-network proxy. Enable header trust only behind a sanitizing trusted proxy.

Selected successful GET responses are cached with ETags. Query-bearing, authenticated, personalized, SSE, mutation, and error responses are not treated as interchangeable public cache entries. Clients should still honor response `Cache-Control` and `Vary` headers.
