#!/usr/bin/env python3
"""
Create full-paper text chunks for the full-paper semantic-search layer.

This does not generate embeddings. It writes deterministic chunks to
paper_chunks so a GPU worker can embed chunk_embeddings_v2 later.
"""

import argparse
import hashlib
import os
import re
from urllib.parse import urlparse

import psycopg2

DEFAULT_SCOPE = "pdf_text"


def db_connect():
    db_url = os.environ.get("DATABASE_URL", "")
    if not db_url.startswith("postgres"):
        raise SystemExit("DATABASE_URL must be a PostgreSQL URL")
    parsed = urlparse(db_url)
    return psycopg2.connect(
        host=parsed.hostname,
        port=parsed.port or 5432,
        user=parsed.username,
        password=parsed.password,
        dbname=parsed.path.lstrip("/"),
    )


def ensure_schema(conn):
    with conn.cursor() as cur:
        cur.execute(
            """
            CREATE TABLE IF NOT EXISTS paper_chunks (
                id text PRIMARY KEY,
                paper_id text NOT NULL,
                scope text NOT NULL,
                section text,
                chunk_index integer NOT NULL,
                text text NOT NULL,
                text_hash text,
                text_chars integer DEFAULT 0,
                token_estimate integer DEFAULT 0,
                created timestamptz DEFAULT now(),
                updated timestamptz DEFAULT now()
            )
            """
        )
        cur.execute(
            "CREATE INDEX IF NOT EXISTS idx_paper_chunks_paper_scope "
            "ON paper_chunks(paper_id, scope, chunk_index)"
        )
        cur.execute(
            "CREATE INDEX IF NOT EXISTS idx_paper_chunks_scope_created "
            "ON paper_chunks(scope, created DESC, paper_id, chunk_index)"
        )
    conn.commit()


def normalize_text(text):
    text = re.sub(r"\s+", " ", text or "").strip()
    return text


def chunk_text(text, chunk_chars, overlap_chars):
    text = normalize_text(text)
    if not text:
        return []
    chunks = []
    start = 0
    while start < len(text):
        end = min(len(text), start + chunk_chars)
        if end < len(text):
            boundary = text.rfind(" ", start + int(chunk_chars * 0.75), end)
            if boundary > start:
                end = boundary
        chunk = text[start:end].strip()
        if chunk:
            chunks.append(chunk)
        if end >= len(text):
            break
        start = max(0, end - overlap_chars)
    return chunks


def stable_chunk_id(paper_id, scope, index):
    raw = f"{paper_id}\x00{scope}\x00{index}".encode("utf-8")
    return "chk_" + hashlib.sha256(raw).hexdigest()[:32]


def text_hash(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def fetch_papers(conn, limit, scope, paper_id=None, refresh_existing=False, offset=0):
    clauses = [
        "p.pdf_text IS NOT NULL",
        "length(p.pdf_text) > 0",
    ]
    params = []
    if paper_id:
        clauses.append("p.id = %s")
        params.append(paper_id)
    elif not refresh_existing:
        clauses.append(
            """
            NOT EXISTS (
                SELECT 1 FROM paper_chunks c
                WHERE c.paper_id = p.id AND c.scope = %s
            )
            """
        )
        params.append(scope)
    params.append(limit)
    params.append(offset)

    with conn.cursor() as cur:
        cur.execute(
            f"""
            SELECT id, pdf_text
            FROM papers p
            WHERE {" AND ".join(clauses)}
            ORDER BY p.fetched_at DESC NULLS LAST, p.created DESC NULLS LAST, p.id
            LIMIT %s
            OFFSET %s
            """,
            params,
        )
        return cur.fetchall()


def store_chunks(conn, paper_id, scope, chunks):
    rows = []
    for i, chunk in enumerate(chunks):
        rows.append(
            (
                stable_chunk_id(paper_id, scope, i),
                paper_id,
                scope,
                "body",
                i,
                chunk,
                text_hash(chunk),
                len(chunk),
                max(1, len(chunk) // 4),
            )
        )
    new_ids = [row[0] for row in rows]
    with conn.cursor() as cur:
        if rows:
            cur.executemany(
                """
                INSERT INTO paper_chunks
                    (id, paper_id, scope, section, chunk_index, text, text_hash,
                     text_chars, token_estimate, created, updated)
                VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, now(), now())
                ON CONFLICT (id) DO UPDATE SET
                    section = EXCLUDED.section,
                    chunk_index = EXCLUDED.chunk_index,
                    text = EXCLUDED.text,
                    text_hash = EXCLUDED.text_hash,
                    text_chars = EXCLUDED.text_chars,
                    token_estimate = EXCLUDED.token_estimate,
                    updated = CASE
                        WHEN paper_chunks.text_hash IS DISTINCT FROM EXCLUDED.text_hash THEN now()
                        ELSE paper_chunks.updated
                    END
                """,
                rows,
            )

        if new_ids:
            cur.execute(
                """
                SELECT id FROM paper_chunks
                WHERE paper_id = %s AND scope = %s AND NOT (id = ANY(%s))
                """,
                (paper_id, scope, new_ids),
            )
        else:
            cur.execute(
                "SELECT id FROM paper_chunks WHERE paper_id = %s AND scope = %s",
                (paper_id, scope),
            )
        stale_ids = [row[0] for row in cur.fetchall()]
        if stale_ids:
            cur.execute("SELECT to_regclass('public.chunk_embeddings_v2')")
            if cur.fetchone()[0]:
                cur.execute("DELETE FROM chunk_embeddings_v2 WHERE chunk_id = ANY(%s)", (stale_ids,))
            cur.execute("DELETE FROM paper_chunks WHERE id = ANY(%s)", (stale_ids,))
    conn.commit()
    return len(rows), len(stale_ids)


def main():
    parser = argparse.ArgumentParser(description="Chunk extracted full-paper text")
    parser.add_argument("--limit", type=int, default=1000)
    parser.add_argument("--select-batch-size", type=int, default=100)
    parser.add_argument("--chunk-chars", type=int, default=3000)
    parser.add_argument("--overlap-chars", type=int, default=300)
    parser.add_argument("--scope", default=DEFAULT_SCOPE)
    parser.add_argument("--paper-id", default="")
    parser.add_argument("--refresh-existing", action="store_true")
    args = parser.parse_args()

    if args.limit <= 0:
        raise SystemExit("--limit must be positive")
    if args.select_batch_size <= 0:
        raise SystemExit("--select-batch-size must be positive")
    if args.chunk_chars <= 0:
        raise SystemExit("--chunk-chars must be positive")
    if args.overlap_chars < 0 or args.overlap_chars >= args.chunk_chars:
        raise SystemExit("--overlap-chars must be non-negative and smaller than --chunk-chars")
    if not args.scope.strip():
        raise SystemExit("--scope must not be empty")

    conn = db_connect()
    ensure_schema(conn)
    processed = 0
    total_chunks = 0
    total_stale = 0
    target_limit = 1 if args.paper_id else args.limit
    while processed < target_limit:
        remaining = target_limit - processed
        if remaining <= 0:
            break
        batch_limit = min(args.select_batch_size, remaining)
        offset = processed if args.refresh_existing and not args.paper_id else 0
        papers = fetch_papers(
            conn,
            batch_limit,
            args.scope,
            paper_id=args.paper_id or None,
            refresh_existing=args.refresh_existing,
            offset=offset,
        )
        if not papers:
            break
        for paper_id, pdf_text in papers:
            chunks = chunk_text(pdf_text, args.chunk_chars, args.overlap_chars)
            stored, stale = store_chunks(conn, paper_id, args.scope, chunks)
            total_chunks += stored
            total_stale += stale
            processed += 1
            print(f"{paper_id}: chunks={stored} stale_removed={stale}", flush=True)
        if len(papers) < batch_limit:
            break
    conn.close()
    print(f"done papers={processed} chunks={total_chunks} stale_removed={total_stale}")


if __name__ == "__main__":
    main()
