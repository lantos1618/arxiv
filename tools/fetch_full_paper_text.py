#!/usr/bin/env python3
"""Fetch arXiv PDFs, extract text, and store it for Deep Search.

This worker intentionally does not keep PDFs on disk. It downloads a paper PDF
to a bounded temporary file, runs pdftotext, stores papers.pdf_text, and records
attempt state in full_paper_fetch_status so failures are auditable and retryable.
"""

import argparse
import contextlib
import os
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import timedelta
from urllib.parse import urlparse

import psycopg2


DEFAULT_USER_AGENT = "arxiv.gg full-paper text fetcher (https://arxiv.gg; mailto:lyndon@lambda.run)"
DEFAULT_CATEGORIES = "cs.AI,cs.LG,cs.CL,cs.CV,stat.ML,cs.RO"


@dataclass
class Candidate:
    paper_id: str
    attempts: int


class FetchError(Exception):
    def __init__(self, message, status_code=None):
        super().__init__(message)
        self.status_code = status_code


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
            CREATE TABLE IF NOT EXISTS full_paper_fetch_status (
                paper_id text PRIMARY KEY,
                status text NOT NULL DEFAULT 'pending',
                attempts integer NOT NULL DEFAULT 0,
                last_error text,
                last_status_code integer,
                pdf_url text,
                pdf_bytes integer DEFAULT 0,
                text_chars integer DEFAULT 0,
                next_attempt_at timestamptz,
                created_at timestamptz NOT NULL DEFAULT now(),
                updated_at timestamptz NOT NULL DEFAULT now()
            )
            """
        )
        cur.execute(
            """
            CREATE INDEX IF NOT EXISTS idx_full_paper_fetch_status_retry
            ON full_paper_fetch_status(status, next_attempt_at, updated_at)
            """
        )
        cur.execute(
            """
            CREATE INDEX IF NOT EXISTS idx_full_paper_fetch_status_updated
            ON full_paper_fetch_status(updated_at DESC)
            """
        )
    conn.commit()


def parse_categories(raw):
    categories = [part.strip() for part in (raw or "").split(",") if part.strip()]
    return categories or None


def order_sql(order):
    if order == "oldest":
        return "p.created ASC NULLS LAST, p.id"
    if order == "random":
        return "random()"
    if order == "viewed":
        return "COALESCE(v.last_viewed_at, p.created) DESC NULLS LAST, p.created DESC NULLS LAST, p.id"
    return "p.created DESC NULLS LAST, p.id"


def claim_candidates(conn, limit, categories, max_attempts, stale_processing_minutes, order, dry_run=False):
    category_clause = ""
    params = []
    if categories:
        category_clause = "AND string_to_array(COALESCE(p.categories, ''), ' ') && %s::text[]"
        params.append(categories)
    params.extend([max_attempts, stale_processing_minutes])
    params.append(limit)

    view_join = ""
    if order == "viewed":
        view_join = """
            LEFT JOIN (
                SELECT paper_id, max(last_viewed_at) AS last_viewed_at
                FROM user_paper_views
                GROUP BY paper_id
            ) v ON v.paper_id = p.id
        """

    candidate_cte = f"""
        WITH candidates AS (
            SELECT p.id, COALESCE(s.attempts, 0) + 1 AS attempts
            FROM papers p
            LEFT JOIN full_paper_fetch_status s ON s.paper_id = p.id
            {view_join}
            WHERE COALESCE(p.abstract, '') <> ''
              AND (p.pdf_text IS NULL OR length(p.pdf_text) = 0)
              {category_clause}
              AND (
                  s.paper_id IS NULL
                  OR (
                      s.status <> 'fetched'
                      AND s.attempts < %s
                      AND COALESCE(s.next_attempt_at, now()) <= now()
                      AND (
                          s.status <> 'processing'
                          OR s.updated_at < now() - (%s * interval '1 minute')
                      )
                  )
              )
            ORDER BY {order_sql(order)}
            LIMIT %s
        )
    """
    if dry_run:
        query = candidate_cte + """
            SELECT id, attempts
            FROM candidates
        """
        with conn.cursor() as cur:
            cur.execute(query, params)
            return [Candidate(row[0], row[1]) for row in cur.fetchall()]

    query = candidate_cte + """
        INSERT INTO full_paper_fetch_status
            (paper_id, status, attempts, created_at, updated_at)
        SELECT id, 'processing', 1, now(), now()
        FROM candidates
        ON CONFLICT (paper_id) DO UPDATE SET
            status = 'processing',
            attempts = full_paper_fetch_status.attempts + 1,
            updated_at = now()
        RETURNING paper_id, attempts
    """
    with conn.cursor() as cur:
        cur.execute(query, params)
        rows = [Candidate(row[0], row[1]) for row in cur.fetchall()]
    conn.commit()
    return rows


def paper_pdf_url(paper_id):
    quoted = urllib.parse.quote(paper_id, safe="/.")
    return f"https://arxiv.org/pdf/{quoted}"


def download_pdf(paper_id, timeout, max_pdf_bytes, user_agent):
    url = paper_pdf_url(paper_id)
    req = urllib.request.Request(url, headers={"User-Agent": user_agent})
    fd, path = tempfile.mkstemp(prefix="arxiv-fullpaper-", suffix=".pdf")
    os.close(fd)
    total = 0
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status_code = getattr(resp, "status", 200)
            if status_code != 200:
                raise FetchError(f"http {status_code}", status_code)
            content_length = resp.headers.get("Content-Length")
            if content_length and int(content_length) > max_pdf_bytes:
                raise FetchError(f"pdf too large: {content_length} bytes", status_code)
            with open(path, "wb") as out:
                while True:
                    chunk = resp.read(1024 * 1024)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > max_pdf_bytes:
                        raise FetchError(f"pdf exceeded limit: {total} bytes", status_code)
                    out.write(chunk)
    except urllib.error.HTTPError as err:
        with contextlib.suppress(FileNotFoundError):
            os.remove(path)
        raise FetchError(f"http {err.code}", err.code) from err
    except Exception:
        with contextlib.suppress(FileNotFoundError):
            os.remove(path)
        raise

    with open(path, "rb") as f:
        if f.read(5) != b"%PDF-":
            with contextlib.suppress(FileNotFoundError):
                os.remove(path)
            raise FetchError("downloaded file is not a PDF", 200)
    return url, path, total


def extract_text(pdf_path, timeout, max_text_chars):
    result = subprocess.run(
        ["pdftotext", "-q", "-enc", "UTF-8", pdf_path, "-"],
        text=True,
        capture_output=True,
        timeout=timeout,
        check=False,
    )
    if result.returncode != 0:
        stderr = (result.stderr or "").strip()
        raise FetchError(f"pdftotext failed: {stderr or result.returncode}")
    text = (result.stdout or "").replace("\x00", "").strip()
    if max_text_chars > 0 and len(text) > max_text_chars:
        text = text[:max_text_chars].rstrip()
    if len(text) < 200:
        raise FetchError(f"too little extracted text: {len(text)} chars")
    return text


def mark_fetched(conn, paper_id, pdf_url, pdf_bytes, text):
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE papers
            SET pdf_text = %s
            WHERE id = %s
            """,
            (text, paper_id),
        )
        cur.execute(
            """
            UPDATE full_paper_fetch_status
            SET status = 'fetched',
                last_error = NULL,
                last_status_code = 200,
                pdf_url = %s,
                pdf_bytes = %s,
                text_chars = %s,
                next_attempt_at = NULL,
                updated_at = now()
            WHERE paper_id = %s
            """,
            (pdf_url, pdf_bytes, len(text), paper_id),
        )
    conn.commit()


def retry_delay(attempts, status_code):
    if status_code in (429, 503):
        base = 30 * 60
    else:
        base = 5 * 60
    seconds = min(base * (2 ** max(attempts - 1, 0)), 24 * 60 * 60)
    return timedelta(seconds=seconds)


def mark_failed(conn, paper_id, attempts, error, status_code, pdf_url):
    message = str(error)
    if len(message) > 1000:
        message = message[:1000]
    delay = retry_delay(attempts, status_code)
    with conn.cursor() as cur:
        cur.execute(
            """
            UPDATE full_paper_fetch_status
            SET status = 'failed',
                last_error = %s,
                last_status_code = %s,
                pdf_url = COALESCE(%s, pdf_url),
                next_attempt_at = now() + %s::interval,
                updated_at = now()
            WHERE paper_id = %s
            """,
            (message, status_code, pdf_url, delay, paper_id),
        )
    conn.commit()


def fetch_one(conn, candidate, args):
    pdf_url = paper_pdf_url(candidate.paper_id)
    pdf_path = ""
    try:
        pdf_url, pdf_path, pdf_bytes = download_pdf(
            candidate.paper_id,
            args.fetch_timeout,
            args.max_pdf_bytes,
            args.user_agent,
        )
        text = extract_text(pdf_path, args.extract_timeout, args.max_text_chars)
        if not args.dry_run:
            mark_fetched(conn, candidate.paper_id, pdf_url, pdf_bytes, text)
        print(
            f"fetched {candidate.paper_id} bytes={pdf_bytes} text_chars={len(text)}",
            flush=True,
        )
        return True
    except Exception as err:
        status_code = err.status_code if isinstance(err, FetchError) else None
        if not args.dry_run:
            mark_failed(conn, candidate.paper_id, candidate.attempts, err, status_code, pdf_url)
        print(f"failed {candidate.paper_id} attempts={candidate.attempts} error={err}", flush=True)
        return False
    finally:
        if pdf_path:
            with contextlib.suppress(FileNotFoundError):
                os.remove(pdf_path)


def main():
    parser = argparse.ArgumentParser(description="Fetch and extract arXiv full-paper PDF text")
    parser.add_argument("--limit", type=int, default=100)
    parser.add_argument("--categories", default=DEFAULT_CATEGORIES)
    parser.add_argument("--order", choices=["recent", "oldest", "random", "viewed"], default="recent")
    parser.add_argument("--max-attempts", type=int, default=5)
    parser.add_argument("--stale-processing-minutes", type=int, default=60)
    parser.add_argument("--rate-limit-seconds", type=float, default=3.0)
    parser.add_argument("--fetch-timeout", type=float, default=60.0)
    parser.add_argument("--extract-timeout", type=float, default=120.0)
    parser.add_argument("--max-pdf-bytes", type=int, default=64 * 1024 * 1024)
    parser.add_argument("--max-text-chars", type=int, default=1_000_000)
    parser.add_argument("--user-agent", default=os.environ.get("ARXIV_FULLPAPER_USER_AGENT", DEFAULT_USER_AGENT))
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    if args.limit <= 0:
        raise SystemExit("--limit must be positive")

    categories = parse_categories(args.categories)
    conn = db_connect()
    ensure_schema(conn)
    candidates = claim_candidates(
        conn,
        args.limit,
        categories,
        args.max_attempts,
        args.stale_processing_minutes,
        args.order,
        args.dry_run,
    )
    if not candidates:
        print("No rows need full-paper text.", flush=True)
        conn.close()
        return

    started = time.monotonic()
    ok = 0
    for index, candidate in enumerate(candidates):
        if fetch_one(conn, candidate, args):
            ok += 1
        if index < len(candidates) - 1 and args.rate_limit_seconds > 0:
            time.sleep(args.rate_limit_seconds)
    elapsed = time.monotonic() - started
    conn.close()
    rate = len(candidates) / elapsed if elapsed else 0
    print(
        f"done claimed={len(candidates)} fetched={ok} failed={len(candidates)-ok} "
        f"rate={rate:.2f}/s dry_run={args.dry_run}",
        flush=True,
    )


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
