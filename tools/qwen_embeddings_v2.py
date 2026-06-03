#!/usr/bin/env python3
"""
Generate Qwen v2 embeddings into the Postgres embeddings_v2 table.

This script is intended for GPU workers. It keeps the current production
embeddings table untouched and writes 1024d paper-level abstract vectors to a
separate table for evaluation/backfill.
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
        cur.execute("CREATE EXTENSION IF NOT EXISTS pgcrypto")
        cur.execute(
            """
            CREATE TABLE IF NOT EXISTS embeddings_v2 (
                paper_id text NOT NULL,
                scope text NOT NULL,
                model text NOT NULL,
                dim integer NOT NULL,
                source_hash text,
                text_chars integer DEFAULT 0,
                token_estimate integer DEFAULT 0,
                created timestamptz DEFAULT now(),
                updated timestamptz DEFAULT now(),
                vector vector(1024),
                PRIMARY KEY (paper_id, scope, model, dim)
            )
            """
        )
        cur.execute("CREATE INDEX IF NOT EXISTS idx_embeddings_v2_lookup ON embeddings_v2(scope, model, dim, paper_id)")
    conn.commit()


def fetch_papers(conn, model, scope, dim, limit, order, refresh_stale=False, after_id=""):
    if order == "id":
        return fetch_papers_by_id(conn, model, scope, dim, limit, refresh_stale, after_id)

    order_sql = "src.fetched_at DESC NULLS LAST, src.created DESC NULLS LAST"
    if order == "random":
        order_sql = "random()"
    stale_clause = "e.paper_id IS NULL"
    if refresh_stale:
        stale_clause = """
            e.paper_id IS NULL
            OR e.source_hash IS DISTINCT FROM encode(digest(
                CASE
                    WHEN src.title_text <> '' AND src.abstract_text <> '' THEN src.title_text || '. ' || src.abstract_text
                    ELSE src.title_text || src.abstract_text
                END,
                'sha256'
            ), 'hex')
        """
    query = f"""
        WITH src AS (
            SELECT p.id,
                   p.title,
                   p.abstract,
                   trim(COALESCE(p.title, '')) AS title_text,
                   regexp_replace(trim(COALESCE(p.abstract, '')), '\\s+', ' ', 'g') AS abstract_text,
                   p.fetched_at,
                   p.created
            FROM papers p
            WHERE COALESCE(p.title, '') <> ''
              AND COALESCE(p.abstract, '') <> ''
        )
        SELECT src.id, src.title, src.abstract
        FROM src
        LEFT JOIN embeddings_v2 e
          ON e.paper_id = src.id
         AND e.scope = %s
         AND e.model = %s
         AND e.dim = %s
        WHERE ({stale_clause})
        ORDER BY {order_sql}
        LIMIT %s
    """
    with conn.cursor() as cur:
        cur.execute(query, (scope, model, dim, limit))
        return cur.fetchall()


def fetch_papers_by_id(conn, model, scope, dim, limit, refresh_stale=False, after_id=""):
    if not refresh_stale:
        query = """
            SELECT p.id, p.title, p.abstract
            FROM papers p
            WHERE p.id > %s
              AND COALESCE(p.title, '') <> ''
              AND COALESCE(p.abstract, '') <> ''
              AND NOT EXISTS (
                  SELECT 1
                  FROM embeddings_v2 e
                  WHERE e.paper_id = p.id
                    AND e.scope = %s
                    AND e.model = %s
                    AND e.dim = %s
              )
            ORDER BY p.id
            LIMIT %s
        """
        with conn.cursor() as cur:
            cur.execute(query, (after_id, scope, model, dim, limit))
            return cur.fetchall()

    query = f"""
        SELECT p.id, p.title, p.abstract
        FROM papers p
        LEFT JOIN embeddings_v2 e
          ON e.paper_id = p.id
         AND e.scope = %s
         AND e.model = %s
         AND e.dim = %s
        WHERE p.id > %s
          AND COALESCE(p.title, '') <> ''
          AND COALESCE(p.abstract, '') <> ''
          AND (
              e.paper_id IS NULL
              OR e.source_hash IS DISTINCT FROM encode(digest(
                  CASE
                      WHEN trim(COALESCE(p.title, '')) <> ''
                       AND regexp_replace(trim(COALESCE(p.abstract, '')), '\\s+', ' ', 'g') <> ''
                      THEN trim(COALESCE(p.title, '')) || '. ' || regexp_replace(trim(COALESCE(p.abstract, '')), '\\s+', ' ', 'g')
                      ELSE trim(COALESCE(p.title, '')) || regexp_replace(trim(COALESCE(p.abstract, '')), '\\s+', ' ', 'g')
                  END,
                  'sha256'
              ), 'hex')
          )
        ORDER BY p.id
        LIMIT %s
    """
    with conn.cursor() as cur:
        cur.execute(query, (scope, model, dim, after_id, limit))
        return cur.fetchall()


def paper_text(title, abstract):
    title = (title or "").strip()
    abstract = normalize_text(abstract)
    if title and abstract:
        return f"{title}. {abstract}"
    return title or abstract


def store_batch(conn, rows, embeddings, model, scope, dim):
    payload = []
    for (paper_id, title, abstract), embedding in zip(rows, embeddings):
        text = paper_text(title, abstract)
        try:
            vector = vector_literal(embedding)
        except (TypeError, ValueError) as err:
            print(f"store invalid embedding marker paper_id={paper_id}: {err}", flush=True)
            vector = None
        payload.append(
            (
                paper_id,
                scope,
                model,
                dim,
                source_hash(text),
                len(text),
                max(1, len(text) // 4),
                vector,
            )
        )
    if not payload:
        print("No embeddings in batch; skipping store.", flush=True)
        return

    with conn.cursor() as cur:
        cur.executemany(
            """
            INSERT INTO embeddings_v2
                (paper_id, scope, model, dim, source_hash, text_chars,
                 token_estimate, vector, created, updated)
            VALUES (%s, %s, %s, %s, %s, %s, %s, %s::vector, now(), now())
            ON CONFLICT (paper_id, scope, model, dim) DO UPDATE SET
                source_hash = EXCLUDED.source_hash,
                text_chars = EXCLUDED.text_chars,
                token_estimate = EXCLUDED.token_estimate,
                vector = EXCLUDED.vector,
                updated = now()
            """,
            payload,
        )
    conn.commit()


def load_local_model(model_name, dim):
    import torch
    from sentence_transformers import SentenceTransformer

    return SentenceTransformer(
        model_name,
        device="cuda",
        model_kwargs={"torch_dtype": torch.bfloat16},
        processor_kwargs={"padding_side": "left"},
        truncate_dim=dim,
    )


def encode_local(model, texts, batch_size):
    return model.encode(
        texts,
        batch_size=batch_size,
        normalize_embeddings=True,
        convert_to_numpy=True,
        show_progress_bar=False,
    )


def main():
    parser = argparse.ArgumentParser(description="Generate Qwen v2 abstract embeddings")
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--dim", type=int, default=DEFAULT_DIM)
    parser.add_argument("--scope", default="abstract")
    parser.add_argument("--limit", type=int, default=10000)
    parser.add_argument("--batch-size", type=int, default=8)
    parser.add_argument("--order", choices=["id", "recent", "random"], default="id")
    parser.add_argument("--service-url", default=os.environ.get("QWEN_EMBEDDING_SERVICE_URL", ""))
    parser.add_argument("--timeout", type=float, default=300)
    parser.add_argument("--refresh-stale", action="store_true")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    if args.dim != DEFAULT_DIM:
        raise SystemExit("This first v2 table is fixed to 1024d; use --dim 1024.")

    local_model = None
    if args.service_url:
        print(f"using embedding service={args.service_url} model={args.model} dim={args.dim}")
    else:
        print(f"loading local model={args.model} dim={args.dim}")
        local_model = load_local_model(args.model, args.dim)

    conn = db_connect()
    ensure_schema(conn)

    def embed_batch(texts):
        if args.service_url:
            return encode_remote(args.service_url, texts, args.timeout)
        return encode_local(local_model, texts, args.batch_size)

    last_id = ""

    def fetch_batch(batch_limit):
        nonlocal last_id
        rows = fetch_papers(
            conn,
            args.model,
            args.scope,
            args.dim,
            batch_limit,
            args.order,
            refresh_stale=args.refresh_stale,
            after_id=last_id,
        )
        if args.order == "id" and rows:
            last_id = rows[-1][0]
        return rows

    run_backfill(
        args.limit,
        args.batch_size,
        args.dry_run,
        fetch_batch,
        lambda row: paper_text(row[1], row[2]),
        embed_batch,
        lambda rows, embeddings: store_batch(conn, rows, embeddings, args.model, args.scope, args.dim),
    )

    conn.close()


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
