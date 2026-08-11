#!/usr/bin/env bash
set -euo pipefail

LIMIT="${QWEN_QUEUE_LIMIT:-${1:-128}}"
PRIORITY="${QWEN_QUEUE_PRIORITY:-${2:-10}}"
ORDER="${QWEN_QUEUE_ORDER:-id}"

if ! [[ "$LIMIT" =~ ^[0-9]+$ ]] || (( LIMIT < 1 )); then
  echo "LIMIT must be a positive integer" >&2
  exit 1
fi
if ! [[ "$PRIORITY" =~ ^-?[0-9]+$ ]]; then
  echo "PRIORITY must be an integer" >&2
  exit 1
fi
case "$ORDER" in
  id|recent|random) ;;
  *)
    echo "ORDER must be one of: id, recent, random" >&2
    exit 1
    ;;
esac

ORDER_SQL="p.id ASC"
case "$ORDER" in
  recent) ORDER_SQL="p.created DESC NULLS LAST, p.id ASC" ;;
  random) ORDER_SQL="random()" ;;
esac

"$(dirname "$0")/compose_psql.sh" \
  -v limit="$LIMIT" \
  -v priority="$PRIORITY" \
  -v order_sql="$ORDER_SQL" <<'SQL'
CREATE EXTENSION IF NOT EXISTS pgcrypto;

WITH candidates AS MATERIALIZED (
    SELECT p.id
    FROM papers p
    WHERE COALESCE(p.title, '') <> ''
      AND COALESCE(p.abstract, '') <> ''
      AND NOT EXISTS (
          SELECT 1
          FROM embeddings_v2 e
          WHERE e.paper_id = p.id
            AND e.scope = 'abstract'
            AND e.model = 'Qwen/Qwen3-Embedding-8B'
            AND e.dim = 1024
            AND e.vector IS NOT NULL
            AND e.source_hash = encode(digest(
                trim(p.title) || '. ' || regexp_replace(trim(p.abstract), '\\s+', ' ', 'g'),
                'sha256'
            ), 'hex')
      )
    ORDER BY :order_sql
    LIMIT :limit
),
upserted AS (
    INSERT INTO qwen_embedding_jobs (
        id, paper_id, kind, scope, model, dim, status, priority, attempts,
        lease_owner, lease_until, last_error, created_at, updated_at, completed_at
    )
    SELECT
        'qjob_' || encode(gen_random_bytes(18), 'hex'),
        id,
        'abstract',
        'abstract',
        'Qwen/Qwen3-Embedding-8B',
        1024,
        'queued',
        :priority,
        0,
        '',
        NULL,
        '',
        now(),
        now(),
        NULL
    FROM candidates
    ON CONFLICT (paper_id, kind, scope, model, dim) DO UPDATE SET
        priority = GREATEST(qwen_embedding_jobs.priority, EXCLUDED.priority),
        status = CASE
            WHEN qwen_embedding_jobs.status IN ('failed', 'complete')
              OR (
                  qwen_embedding_jobs.status = 'running'
                  AND qwen_embedding_jobs.lease_until IS NOT NULL
                  AND qwen_embedding_jobs.lease_until < now()
              )
            THEN 'queued'
            ELSE qwen_embedding_jobs.status
        END,
        lease_owner = CASE
            WHEN qwen_embedding_jobs.status IN ('failed', 'complete')
              OR (
                  qwen_embedding_jobs.status = 'running'
                  AND qwen_embedding_jobs.lease_until IS NOT NULL
                  AND qwen_embedding_jobs.lease_until < now()
              )
            THEN ''
            ELSE qwen_embedding_jobs.lease_owner
        END,
        lease_until = CASE
            WHEN qwen_embedding_jobs.status IN ('failed', 'complete')
              OR (
                  qwen_embedding_jobs.status = 'running'
                  AND qwen_embedding_jobs.lease_until IS NOT NULL
                  AND qwen_embedding_jobs.lease_until < now()
              )
            THEN NULL
            ELSE qwen_embedding_jobs.lease_until
        END,
        last_error = CASE
            WHEN qwen_embedding_jobs.status IN ('failed', 'complete')
              OR (
                  qwen_embedding_jobs.status = 'running'
                  AND qwen_embedding_jobs.lease_until IS NOT NULL
                  AND qwen_embedding_jobs.lease_until < now()
              )
            THEN ''
            ELSE qwen_embedding_jobs.last_error
        END,
        completed_at = CASE
            WHEN qwen_embedding_jobs.status IN ('failed', 'complete')
              OR (
                  qwen_embedding_jobs.status = 'running'
                  AND qwen_embedding_jobs.lease_until IS NOT NULL
                  AND qwen_embedding_jobs.lease_until < now()
              )
            THEN NULL
            ELSE qwen_embedding_jobs.completed_at
        END,
        updated_at = now()
    RETURNING paper_id, status
)
SELECT
    (SELECT count(*) FROM candidates) AS candidates,
    count(*) FILTER (WHERE status = 'queued') AS queued_or_requeued,
    count(*) AS touched
FROM upserted;
SQL
