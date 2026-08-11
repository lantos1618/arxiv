# GPU Worker Runbook

This runbook operates the current Qwen semantic pipeline. Keep PostgreSQL on the application host; GPU workers receive bounded text jobs and return vectors through an authenticated API or access PostgreSQL only for an explicitly controlled batch backfill.

## 1. Preconditions

- `QWEN_WORKER_TOKEN` is set on the application and delivered to workers through a secret file/volume when possible.
- The app is reachable over a private or TLS-protected path.
- PostgreSQL has pgvector and the current schema/indexes.
- `deploy/sql/2026-07-11-qwen-vector-not-null.sql` has been reviewed, invalid NULL-vector rows have been cleaned, and the migration has been applied.
- GPU image/runtime supports `Qwen/Qwen3-Embedding-8B` and the selected dtype.

Never pass worker tokens in a URL. Avoid process arguments; `tools/run_ovh_qwen_worker.sh` prefers `QWEN_OVH_TOKEN_VOLUME` and `ARXIV_WORKER_TOKEN_FILE`.

Set `QWEN_ASYNC_WORKER_ENABLED=true` in the app deployment only after the queue worker or orchestrator is installed, monitored, and able to claim jobs. Leave it false when the worker is stopped; this hides Semantic Search and prevents orphaned query jobs.

## 2. Local Service

From the repository root:

```bash
python3 -m venv .venv-qwen
. .venv-qwen/bin/activate
pip install -r requirements-qwen-worker.txt

export QWEN_EMBEDDING_DEVICE=cuda
export QWEN_EMBEDDING_DTYPE=bfloat16
export QWEN_MAX_BATCH_SIZE=128
export QWEN_MAX_TEXT_CHARS=24000
uvicorn tools.qwen_embedding_service:app --host 127.0.0.1 --port 8010
```

Validate service health before starting database work:

```bash
curl -fsS http://127.0.0.1:8010/health
```

Use the exact repository-root module path shown above.

## 3. Authenticated API Worker

The preferred remote pattern claims fenced `query` and `abstract` jobs from the application:

```bash
export ARXIV_API_BASE='https://arxiv.gg/api/v1'
export ARXIV_WORKER_TOKEN_FILE='/run/secrets/QWEN_WORKER_TOKEN'
export QWEN_JOB_KINDS='query,abstract'
export QWEN_JOB_WORKER_NAME="gpu-$(hostname)"
python3 tools/qwen_api_worker.py \
  --limit 32 \
  --claim-size 1 \
  --batch-size 1 \
  --lease-seconds 900
```

The worker loads the model before claiming, renews active leases, returns lease owner/generation/source hash, and resolves ambiguous completion responses before reporting failure. Multiple workers are safe only when all preserve this fencing protocol.

## 4. On-Demand OVH Worker

Configure a mounted secret volume where possible:

```bash
export QWEN_OVH_TOKEN_VOLUME='my-secret-volume@GRA:/run/secrets:ro'
export QWEN_OVH_TOKEN_FILE='/run/secrets/QWEN_WORKER_TOKEN'
export QWEN_OVH_LIMIT=32
export QWEN_OVH_KINDS='query,abstract'
tools/run_ovh_qwen_worker.sh
```

`tools/qwen_jit_orchestrator.sh` uses a non-blocking file lock to prevent duplicate local orchestrators from launching the same burst. Run it through `deploy/qwen-jit-orchestrator.service` or one equivalent supervisor, not several unsynchronized copies.

## 5. Abstract Backfill

Queue a bounded set, then launch bounded workers:

```bash
QWEN_QUEUE_LIMIT=2048 QWEN_QUEUE_PRIORITY=10 \
  tools/qwen_queue_abstract_backfill.sh

QWEN_BACKFILL_TOTAL=2048 QWEN_BACKFILL_WORKERS=1 \
  tools/run_qwen_abstract_backfill_batch.sh
```

For a directly connected private GPU service, `tools/qwen_embeddings_v2.py` is also available. Verify `DATABASE_URL` points to the intended PostgreSQL database before running any direct backfill.

## 6. Full-Paper Pipeline

Run extraction, chunking, then vectorization:

```bash
python3 tools/fetch_full_paper_text.py --limit 100
python3 tools/chunk_full_papers.py --limit 1000
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_chunk_embeddings_v2.py --limit 10000 --batch-size 16
```

Deep Search covers only papers whose current PDF text was chunked and whose chunk vectors match current text hashes. Keep chunk overlap below chunk size. Respect arXiv request rates and storage limits.

## 7. Validation

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_pipeline_check.py \
  --scope both --window-minutes 15 --min-recent 1

curl --get 'https://arxiv.gg/api/v1/search/semantic' \
  --data-urlencode 'q=representation learning'
```

Confirm:

- vectors are non-NULL and 1,024-dimensional;
- model/scope/source hashes are current;
- semantic responses identify Qwen or explicitly mark Quick fallback;
- leases are renewed and stale workers receive conflicts;
- failed jobs retain bounded retry/error state;
- no token appears in process listings or logs.

Stop workers before changing model, dimension, text construction, or schema. Those changes require a new compatibility/backfill plan rather than silently mixing vectors.
