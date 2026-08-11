#!/usr/bin/env python3
"""
Process queued Qwen JIT embedding jobs.

This worker is intentionally bounded. It claims jobs from qwen_embedding_jobs,
does the requested paper-level work, marks each job complete/failed, and exits.
It can run inside the current app container, a systemd timer, or an OVH AI
Training job once ovhai credentials are available.
"""

import argparse
import os
import sys
import time

import chunk_full_papers
import qwen_chunk_embeddings_v2
import qwen_embeddings_v2
from qwen_backfill_common import db_connect
from qwen_backfill_common import encode_remote
from qwen_backfill_common import encode_with_retries
from qwen_backfill_common import normalize_text


DEFAULT_MODEL = "Qwen/Qwen3-Embedding-8B"
DEFAULT_DIM = 1024
DEFAULT_SCOPE = "pdf_text"
JOB_KINDS = ("abstract", "paper_chunks", "chunk_embeddings")


class LeaseLostError(RuntimeError):
    pass


def ensure_schema(conn):
    qwen_embeddings_v2.ensure_schema(conn)
    chunk_full_papers.ensure_schema(conn)
    qwen_chunk_embeddings_v2.ensure_schema(conn)
    with conn.cursor() as cur:
        cur.execute(
            """
            CREATE TABLE IF NOT EXISTS qwen_embedding_jobs (
                id text PRIMARY KEY,
                paper_id text NOT NULL,
                kind text NOT NULL,
                scope text NOT NULL,
                model text NOT NULL,
                dim integer NOT NULL,
                status text DEFAULT 'queued',
                priority integer DEFAULT 0,
                attempts integer DEFAULT 0,
                lease_owner text,
                lease_until timestamptz,
                last_error text,
                created_at timestamptz DEFAULT now(),
                updated_at timestamptz DEFAULT now(),
                completed_at timestamptz,
                UNIQUE (paper_id, kind, scope, model, dim)
            )
            """
        )
        cur.execute(
            "CREATE INDEX IF NOT EXISTS idx_qwen_embedding_jobs_queue "
            "ON qwen_embedding_jobs(status, priority DESC, created_at)"
        )
        cur.execute(
            "CREATE INDEX IF NOT EXISTS idx_qwen_embedding_jobs_lease "
            "ON qwen_embedding_jobs(status, lease_until)"
        )
    conn.commit()


def claim_jobs(conn, kinds, limit, lease_owner, lease_seconds):
    if limit <= 0:
        return []
    with conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT id, paper_id, kind, scope, model, dim
                FROM qwen_embedding_jobs
                WHERE kind = ANY(%s)
                  AND (
                    status = 'queued'
                    OR (status = 'running' AND lease_until IS NOT NULL AND lease_until < now())
                  )
                ORDER BY priority DESC, created_at ASC
                LIMIT %s
                FOR UPDATE SKIP LOCKED
                """,
                (kinds, limit),
            )
            jobs = cur.fetchall()
            if not jobs:
                return []
            job_ids = [row[0] for row in jobs]
            cur.execute(
                """
                UPDATE qwen_embedding_jobs AS jobs
                SET status = 'running',
                    attempts = attempts + 1,
                    lease_owner = %s,
                    lease_until = now() + make_interval(secs => %s),
                    last_error = '',
                    completed_at = NULL,
                    updated_at = now()
                WHERE id = ANY(%s)
                RETURNING jobs.id, jobs.paper_id, jobs.kind, jobs.scope,
                          jobs.model, jobs.dim, jobs.lease_owner, jobs.attempts
                """,
                (lease_owner, lease_seconds, job_ids),
            )
            return cur.fetchall()


def heartbeat_job(conn, job_id, lease_owner, lease_generation, lease_seconds):
    with conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE qwen_embedding_jobs
                SET lease_until = now() + make_interval(secs => %s),
                    updated_at = now()
                WHERE id = %s
                  AND status = 'running'
                  AND lease_owner = %s
                  AND attempts = %s
                  AND lease_until > now()
                """,
                (lease_seconds, job_id, lease_owner, lease_generation),
            )
            if cur.rowcount != 1:
                raise LeaseLostError(f"job {job_id} lease is no longer owned by this worker")


def complete_job(conn, job_id, lease_owner, lease_generation):
    with conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE qwen_embedding_jobs
                SET status = 'complete',
                    lease_owner = '',
                    lease_until = NULL,
                    last_error = '',
                    completed_at = now(),
                    updated_at = now()
                WHERE id = %s
                  AND status = 'running'
                  AND lease_owner = %s
                  AND attempts = %s
                  AND lease_until > now()
                """,
                (job_id, lease_owner, lease_generation),
            )
            if cur.rowcount != 1:
                raise LeaseLostError(f"job {job_id} completion rejected after lease loss")


def fail_job(conn, job_id, lease_owner, lease_generation, err):
    message = str(err)[:1000]
    with conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE qwen_embedding_jobs
                SET status = 'failed',
                    lease_owner = '',
                    lease_until = NULL,
                    last_error = %s,
                    completed_at = NULL,
                    updated_at = now()
                WHERE id = %s
                  AND status = 'running'
                  AND lease_owner = %s
                  AND attempts = %s
                  AND lease_until > now()
                """,
                (message, job_id, lease_owner, lease_generation),
            )


def fetch_paper(conn, paper_id):
    with conn.cursor() as cur:
        cur.execute("SELECT id, title, abstract FROM papers WHERE id = %s", (paper_id,))
        return cur.fetchone()


def embed_abstract(conn, service_url, timeout, paper_id, model, dim):
    row = fetch_paper(conn, paper_id)
    if not row:
        raise RuntimeError(f"paper not found: {paper_id}")
    text = qwen_embeddings_v2.paper_text(row[1], row[2])
    if not text:
        raise RuntimeError(f"paper has no title or abstract: {paper_id}")
    embeddings = encode_with_retries([text], lambda texts: encode_remote(service_url, texts, timeout))
    qwen_embeddings_v2.store_batch(conn, [row], embeddings, model, "abstract", dim)


def chunk_paper(conn, paper_id, scope, chunk_chars, overlap_chars):
    rows = chunk_full_papers.fetch_papers(
        conn,
        1,
        scope,
        paper_id=paper_id,
        refresh_existing=True,
        offset=0,
    )
    if not rows:
        raise RuntimeError(f"paper has no extracted PDF text: {paper_id}")
    _, pdf_text = rows[0]
    chunks = chunk_full_papers.chunk_text(pdf_text, chunk_chars, overlap_chars)
    stored, stale = chunk_full_papers.store_chunks(conn, paper_id, scope, chunks)
    print(f"{paper_id}: chunks={stored} stale_removed={stale}", flush=True)


def fetch_paper_chunks(conn, paper_id, model, dim, scope, limit):
    with conn.cursor() as cur:
        cur.execute(
            """
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
             AND e.vector IS NOT NULL
            WHERE c.paper_id = %s
              AND c.scope = %s
              AND COALESCE(c.text, '') <> ''
              AND (
                  e.chunk_id IS NULL
                  OR e.source_hash IS DISTINCT FROM c.text_hash
              )
            ORDER BY c.chunk_index
            LIMIT %s
            """,
            (model, dim, paper_id, scope, limit),
        )
        return cur.fetchall()


def embed_chunks(conn, args, job, paper_id, model, dim, scope):
    job_id, _, _, _, _, _, lease_owner, lease_generation = job
    processed = 0
    while processed < args.per_job_chunk_limit:
        heartbeat_job(conn, job_id, lease_owner, lease_generation, args.lease_seconds)
        rows = fetch_paper_chunks(
            conn,
            paper_id,
            model,
            dim,
            scope,
            min(args.batch_size, args.per_job_chunk_limit - processed),
        )
        if not rows:
            break
        texts = [normalize_text(row[1]) for row in rows]
        embeddings = encode_with_retries(texts, lambda batch: encode_remote(args.service_url, batch, args.timeout))
        qwen_chunk_embeddings_v2.store_batch(conn, rows, embeddings, model, dim)
        processed += len(rows)
        print(f"{paper_id}: chunk_embeddings={processed}", flush=True)
    if processed == 0:
        with conn.cursor() as cur:
            cur.execute(
                "SELECT count(*) FROM paper_chunks WHERE paper_id = %s AND scope = %s",
                (paper_id, scope),
            )
            chunk_count = cur.fetchone()[0]
        if chunk_count == 0:
            raise RuntimeError(f"paper has no chunks to embed: {paper_id}")


def process_job(conn, args, job):
    job_id, paper_id, kind, scope, model, dim, lease_owner, lease_generation = job
    heartbeat_job(conn, job_id, lease_owner, lease_generation, args.lease_seconds)
    if dim != DEFAULT_DIM:
        raise RuntimeError(f"unsupported dim={dim}; want {DEFAULT_DIM}")
    if kind == "abstract":
        embed_abstract(conn, args.service_url, args.timeout, paper_id, model, dim)
    elif kind == "paper_chunks":
        chunk_paper(conn, paper_id, scope or DEFAULT_SCOPE, args.chunk_chars, args.overlap_chars)
    elif kind == "chunk_embeddings":
        embed_chunks(conn, args, job, paper_id, model, dim, scope or DEFAULT_SCOPE)
    else:
        raise RuntimeError(f"unsupported qwen job kind={kind!r}")
    heartbeat_job(conn, job_id, lease_owner, lease_generation, args.lease_seconds)
    complete_job(conn, job_id, lease_owner, lease_generation)


def parse_kinds(value):
    kinds = []
    for item in value.split(","):
        item = item.strip()
        if item and item not in kinds:
            if item not in JOB_KINDS:
                raise SystemExit(f"unsupported kind {item!r}; choose from {','.join(JOB_KINDS)}")
            kinds.append(item)
    return kinds or list(JOB_KINDS)


def main():
    parser = argparse.ArgumentParser(description="Process queued Qwen JIT embedding jobs")
    parser.add_argument("--service-url", default=os.environ.get("QWEN_EMBEDDING_SERVICE_URL", ""))
    parser.add_argument("--kinds", default=",".join(JOB_KINDS))
    parser.add_argument("--limit", type=int, default=100)
    parser.add_argument("--claim-size", type=int, default=1)
    parser.add_argument("--batch-size", type=int, default=4)
    parser.add_argument("--per-job-chunk-limit", type=int, default=5000)
    parser.add_argument("--max-runtime", type=float, default=0)
    parser.add_argument("--lease-owner", default=os.environ.get("QWEN_JOB_WORKER_NAME", "qwen-job-worker"))
    parser.add_argument("--lease-seconds", type=int, default=900)
    parser.add_argument("--timeout", type=float, default=300)
    parser.add_argument("--chunk-chars", type=int, default=3000)
    parser.add_argument("--overlap-chars", type=int, default=300)
    parser.add_argument("--idle-sleep", type=float, default=0)
    args = parser.parse_args()

    if not args.service_url:
        raise SystemExit("--service-url or QWEN_EMBEDDING_SERVICE_URL is required")
    if args.limit <= 0 or args.claim_size <= 0 or args.batch_size <= 0:
        raise SystemExit("--limit, --claim-size, and --batch-size must be positive")
    if args.per_job_chunk_limit <= 0 or args.timeout <= 0:
        raise SystemExit("--per-job-chunk-limit and --timeout must be positive")
    if args.idle_sleep < 0 or args.max_runtime < 0:
        raise SystemExit("--idle-sleep and --max-runtime must be non-negative")
    if args.lease_seconds < 30:
        raise SystemExit("--lease-seconds must be at least 30")
    if args.chunk_chars <= 0 or args.overlap_chars < 0 or args.overlap_chars >= args.chunk_chars:
        raise SystemExit("chunk overlap must be non-negative and smaller than chunk size")
    kinds = parse_kinds(args.kinds)
    started = time.time()
    processed = 0

    conn = db_connect()
    ensure_schema(conn)
    try:
        while processed < args.limit:
            if args.max_runtime > 0 and time.time() - started >= args.max_runtime:
                break
            jobs = claim_jobs(conn, kinds, min(args.claim_size, args.limit - processed), args.lease_owner, args.lease_seconds)
            if not jobs:
                if args.idle_sleep > 0:
                    time.sleep(args.idle_sleep)
                    continue
                break
            for job in jobs:
                job_id, paper_id, kind, _, _, _, lease_owner, lease_generation = job
                print(f"job {job_id}: paper_id={paper_id} kind={kind} start", flush=True)
                try:
                    process_job(conn, args, job)
                except Exception as err:
                    if not isinstance(err, LeaseLostError):
                        try:
                            fail_job(conn, job_id, lease_owner, lease_generation, err)
                        except Exception as fail_err:
                            print(f"job {job_id}: could not record failure: {fail_err}", flush=True)
                    print(f"job {job_id}: failed: {err}", flush=True)
                else:
                    print(f"job {job_id}: complete", flush=True)
                processed += 1
    finally:
        conn.close()

    elapsed = time.time() - started
    print(f"done processed={processed} seconds={elapsed:.1f}", flush=True)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
