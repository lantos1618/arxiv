#!/usr/bin/env python3
"""
Generate Qwen v2 embeddings for full-paper chunks.

Expected flow:
  1. Run chunk_full_papers.py on the DB host to fill paper_chunks.
  2. Run qwen_embedding_service.py on the GPU host.
  3. Run this script on the DB host, pointing --service-url at the GPU service.
"""

import argparse
import os
import sys

from qwen_backfill_common import db_connect
from qwen_backfill_common import encode_remote
from qwen_backfill_common import normalize_text
from qwen_backfill_common import run_backfill
from qwen_backfill_common import source_hash
from qwen_backfill_common import vector_literal


DEFAULT_MODEL = "Qwen/Qwen3-Embedding-8B"
DEFAULT_DIM = 1024


def ensure_schema(conn):
    with conn.cursor() as cur:
        cur.execute("CREATE EXTENSION IF NOT EXISTS vector")
        cur.execute(
            """
            CREATE TABLE IF NOT EXISTS chunk_embeddings_v2 (
                chunk_id text NOT NULL,
                model text NOT NULL,
                dim integer NOT NULL,
                source_hash text,
                text_chars integer DEFAULT 0,
                token_estimate integer DEFAULT 0,
                created timestamptz DEFAULT now(),
                updated timestamptz DEFAULT now(),
                vector vector(1024),
                PRIMARY KEY (chunk_id, model, dim)
            )
            """
        )
        cur.execute(
            "CREATE INDEX IF NOT EXISTS idx_chunk_embeddings_v2_lookup "
            "ON chunk_embeddings_v2(model, dim, chunk_id)"
        )
    conn.commit()


def fetch_chunks(conn, model, dim, scope, limit, order):
    order_sql = "c.created DESC, c.paper_id, c.chunk_index"
    if order == "oldest":
        order_sql = "c.created ASC, c.paper_id, c.chunk_index"
    elif order == "random":
        order_sql = "random()"
    query = f"""
        SELECT c.id,
               c.text,
               c.text_hash,
               c.text_chars,
               c.token_estimate,
               CASE WHEN e.chunk_id IS NULL THEN 'missing' ELSE 'stale' END AS reason
        FROM paper_chunks c
        LEFT JOIN chunk_embeddings_v2 e
          ON e.chunk_id = c.id
         AND e.model = %s
         AND e.dim = %s
        WHERE c.scope = %s
          AND COALESCE(c.text, '') <> ''
          AND (
              e.chunk_id IS NULL
              OR e.source_hash IS DISTINCT FROM c.text_hash
          )
        ORDER BY {order_sql}
        LIMIT %s
    """
    with conn.cursor() as cur:
        cur.execute(query, (model, dim, scope, limit))
        return cur.fetchall()


def store_batch(conn, rows, embeddings, model, dim):
    payload = []
    for (chunk_id, text, stored_hash, text_chars, token_estimate, _reason), embedding in zip(rows, embeddings):
        text = normalize_text(text)
        row_hash = stored_hash or source_hash(text)
        try:
            vector = vector_literal(embedding)
        except (TypeError, ValueError) as err:
            print(f"store invalid embedding marker chunk_id={chunk_id}: {err}", flush=True)
            vector = None
        payload.append(
            (
                chunk_id,
                model,
                dim,
                row_hash,
                text_chars or len(text),
                token_estimate or max(1, len(text) // 4),
                vector,
            )
        )
    if not payload:
        print("No embeddings in batch; skipping store.", flush=True)
        return

    with conn.cursor() as cur:
        cur.executemany(
            """
            INSERT INTO chunk_embeddings_v2
                (chunk_id, model, dim, source_hash, text_chars,
                 token_estimate, vector, created, updated)
            VALUES (%s, %s, %s, %s, %s, %s, %s::vector, now(), now())
            ON CONFLICT (chunk_id, model, dim) DO UPDATE SET
                source_hash = EXCLUDED.source_hash,
                text_chars = EXCLUDED.text_chars,
                token_estimate = EXCLUDED.token_estimate,
                vector = EXCLUDED.vector,
                updated = now()
            """,
            payload,
        )
    conn.commit()


def main():
    parser = argparse.ArgumentParser(description="Generate Qwen v2 full-paper chunk embeddings")
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--dim", type=int, default=DEFAULT_DIM)
    parser.add_argument("--scope", default="pdf_text")
    parser.add_argument("--limit", type=int, default=10000)
    parser.add_argument("--batch-size", type=int, default=8)
    parser.add_argument("--order", choices=["recent", "oldest", "random"], default="recent")
    parser.add_argument("--service-url", default=os.environ.get("QWEN_EMBEDDING_SERVICE_URL", ""))
    parser.add_argument("--timeout", type=float, default=300)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    if args.dim != DEFAULT_DIM:
        raise SystemExit("This first v2 table is fixed to 1024d; use --dim 1024.")
    if not args.service_url:
        raise SystemExit("--service-url or QWEN_EMBEDDING_SERVICE_URL is required")

    conn = db_connect()
    ensure_schema(conn)

    print(f"using embedding service={args.service_url} model={args.model} dim={args.dim}")
    reason_totals = {"missing": 0, "stale": 0}

    def fetch_batch(batch_limit):
        rows = fetch_chunks(conn, args.model, args.dim, args.scope, batch_limit, args.order)
        for row in rows:
            reason_totals[row[5]] = reason_totals.get(row[5], 0) + 1
        return rows

    run_backfill(
        args.limit,
        args.batch_size,
        args.dry_run,
        fetch_batch,
        lambda row: normalize_text(row[1]),
        lambda texts: encode_remote(args.service_url, texts, args.timeout),
        lambda rows, embeddings: store_batch(conn, rows, embeddings, args.model, args.dim),
    )
    conn.close()
    print(f"reasons missing={reason_totals.get('missing', 0)} stale={reason_totals.get('stale', 0)}")


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
