# Operational Tools

Qwen3-Embedding-8B at 1024 dimensions is the canonical embedding pipeline.
Production images install only `tools/requirements.txt` and do not package or
start MiniLM.

## Full-paper pipeline

`fullpaper_pipeline.sh` runs one bounded fetch, chunk, and Qwen embed cycle in
the Compose application service. Configure `/etc/arxiv/fullpaper-pipeline.env`
from `deploy/fullpaper-pipeline.env.example`, install the service and timer from
`deploy/systemd/`, then inspect it with:

```bash
systemctl status arxiv-fullpaper-pipeline.timer
journalctl -u arxiv-fullpaper-pipeline.service
```

The fetcher uses temporary PDFs, stores extracted text and retry state, then the
chunker and Qwen consumer process only bounded batches. The timer runs another
cycle until no work remains.

## Qwen pipeline

Run the GPU endpoint on a private interface or authenticated tunnel:

```bash
QWEN_EMBEDDING_DEVICE=cuda \
uvicorn tools.qwen_embedding_service:app --host 127.0.0.1 --port 8010
```

Direct bounded backfills remain available:

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_embeddings_v2.py --limit 10000 --batch-size 16 --refresh-stale

QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_chunk_embeddings_v2.py --limit 10000 --batch-size 16
```

`arxiv-qwen-pipeline-check.timer` verifies service health, source hashes,
configured scopes, vector presence, and recent progress. A check that skips both
the service and database is rejected rather than reported healthy.

Remote OVH jobs require both an explicit immutable `QWEN_OVH_IMAGE` and a
`QWEN_OVH_TOKEN_VOLUME` mounted at `QWEN_OVH_TOKEN_FILE`; tokens are never sent
in command arguments.

## SQL migrations

`deploy/sql/manifest.txt` is the ordered migration manifest. Use:

```bash
tools/sql_migrations.sh preflight
tools/sql_migrations.sh status
tools/sql_migrations.sh apply
```

Applied file hashes are recorded in `arxiv_schema_migrations`; modified applied
migrations stop status/apply with a drift error.

## IndexNow

Append changed or newly public site URLs to `/var/lib/arxiv/indexnow-urls`.
`arxiv-indexnow.timer` drains this queue in bounded batches and restores it after
submission failures. It intentionally does not resubmit the full sitemap.

## Archived MiniLM utility

`generate_embeddings.py`, `query_embedding.py`, and `embedding_service.py` are
retained only for offline legacy migration/inspection and are not installed in
the production image. Use `tools/requirements-minilm-archive.txt` and set
`ALLOW_LEGACY_MINILM_MIGRATION=1` only in an isolated migration environment.
