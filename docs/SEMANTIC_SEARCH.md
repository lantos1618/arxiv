# Semantic Search

## Current Architecture

arxiv.gg has one current semantic architecture: `Qwen/Qwen3-Embedding-8B` truncated to 1,024 dimensions and stored in PostgreSQL with pgvector.

| Feature | Source | Storage | Retrieval |
|---|---|---|---|
| Idea search | Paper title + abstract | `embeddings_v2`, scope `abstract` | Cosine nearest neighbors |
| Related papers/map | Anchor and neighboring abstract vectors | `embeddings_v2` | Neighbors plus a two-dimensional display projection |
| Deep Search | Prepared PDF-text chunks | `paper_chunks`, `chunk_embeddings_v2` | Best current chunk per paper |
| Query embedding | Search text | Qwen query cache/jobs | Synchronous service or leased worker queue |

Stored vectors are valid only when model, dimension, scope, non-NULL vector, and source hash match the current source. Deep Search joins chunk embeddings to current chunk text hashes so stale vectors are excluded.

The original `embeddings` table, MiniLM service, generic embedding worker, and `tools/generate_embeddings.py` remain for compatibility/migration. They are not an alternate supported product search stack.

## Request Behavior

`GET /api/v1/search/semantic` embeds the query and searches Qwen abstract vectors. Quick Search remains the browser default. If no Qwen catalog or execution path is available, the endpoint returns HTTP 206 Quick results with a non-retryable fallback. When a monitored asynchronous worker is configured and a query is queued, the response recommends a bounded retry without claiming that a GPU has started:

```json
{
  "success": true,
  "data": {
    "mode": "quick",
    "model": "quick",
    "fallback": true,
    "notice": "..."
  }
}
```

Clients must check `fallback`; `similarity` is `null` in fallback results.

`GET /api/v1/papers/{id}/similar` requires a current Qwen abstract vector and returns HTTP 409 when none exists. `POST /api/v1/papers/{id}/embeddings` may complete synchronously or return HTTP 202 with `queued`, `status`, and `statusUrl`. Poll the status URL rather than assuming a 202 response means an embedding exists.

Deep Search is the `deep` mode on `/api/v1/search/stream`. It requires a signed-in session or account API key and only searches prepared, hash-current full-paper chunks.

## Local Qwen Service

Install the worker requirements in an isolated environment, then start the service from the repository root:

```bash
python3 -m venv .venv-qwen
. .venv-qwen/bin/activate
pip install -r requirements-qwen-worker.txt
QWEN_EMBEDDING_DEVICE=cuda \
QWEN_EMBEDDING_DTYPE=bfloat16 \
uvicorn tools.qwen_embedding_service:app --host 127.0.0.1 --port 8010
```

Point the Go service or batch tools at `http://127.0.0.1:8010`. Keep inference bound to loopback or a private authenticated network.

## Abstract Backfill

With `DATABASE_URL` explicitly set to the intended PostgreSQL database:

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_embeddings_v2.py --limit 10000 --batch-size 16
```

Use `--refresh-stale` to regenerate rows whose source hash no longer matches title + abstract. For queued remote workers, use `tools/qwen_queue_abstract_backfill.sh` and the authenticated worker flow described in [GPU_WORKER_RUNBOOK.md](GPU_WORKER_RUNBOOK.md).

## Full-Paper Preparation

The pipeline is ordered:

```bash
python3 tools/fetch_full_paper_text.py --limit 100
python3 tools/chunk_full_papers.py --limit 1000
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_chunk_embeddings_v2.py --limit 10000 --batch-size 16
```

All three tools require PostgreSQL. Fetching obeys arXiv access constraints and does not guarantee every paper has an extractable PDF. Chunk settings must keep overlap smaller than chunk size.

## Validation

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_pipeline_check.py \
  --scope both --window-minutes 15 --min-recent 1
```

Also exercise:

```bash
curl --get 'http://127.0.0.1:8080/api/v1/search/semantic' \
  --data-urlencode 'q=causal representation learning'
curl 'http://127.0.0.1:8080/api/v1/papers/1706.03762/similar?limit=10'
```

Verify response model/fallback fields, non-NULL vectors, current hashes, queue lease behavior, and the pgvector indexes in production. Throughput and GPU memory depend on hardware, dtype, text length, and batch size; measure the target worker rather than relying on archived estimates.
