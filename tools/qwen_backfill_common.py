#!/usr/bin/env python3
"""Shared helpers for Qwen embedding backfill scripts."""

import hashlib
import json
import math
import os
import time
import urllib.error
import urllib.request
from urllib.parse import urlparse

import psycopg2


class EmbeddingServiceError(RuntimeError):
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


def normalize_text(text):
    return " ".join((text or "").split())


def source_hash(text):
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def vector_literal(embedding):
    values = []
    for index, value in enumerate(embedding):
        if value is None:
            raise ValueError(f"embedding contains null at index {index}")
        number = float(value)
        if not math.isfinite(number):
            raise ValueError(f"embedding contains non-finite value at index {index}")
        values.append(str(number))
    return "[" + ",".join(values) + "]"


def encode_remote(service_url, texts, timeout):
    payload = json.dumps({"texts": texts}).encode("utf-8")
    req = urllib.request.Request(
        service_url.rstrip("/") + "/embed/batch",
        data=payload,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
    except urllib.error.HTTPError as err:
        detail = err.read().decode("utf-8", "replace")
        raise EmbeddingServiceError(f"embedding service http {err.code}: {detail}", err.code) from err
    except (TimeoutError, urllib.error.URLError) as err:
        raise EmbeddingServiceError(f"embedding service connection failed: {err}") from err
    data = json.loads(body)
    embeddings = data.get("embeddings", [])
    if len(embeddings) != len(texts):
        raise RuntimeError(f"embedding service returned {len(embeddings)} vectors for {len(texts)} texts")
    return embeddings


def should_split_batch(err):
    if isinstance(err, EmbeddingServiceError):
        if err.status_code in (413, 429, 500, 502, 503, 504, 507):
            return True
    message = str(err).lower()
    return "out of memory" in message or "batch too large" in message or "timeout" in message


def encode_with_retries(texts, embed_batch, max_attempts=3, retry_delay=2.0, depth=0):
    try:
        return embed_batch(texts)
    except Exception as err:
        if len(texts) > 1 and should_split_batch(err):
            midpoint = max(1, len(texts) // 2)
            indent = "  " * depth
            print(
                f"{indent}embedding batch failed for {len(texts)} texts; "
                f"splitting into {midpoint}+{len(texts) - midpoint}: {err}",
                flush=True,
            )
            left = encode_with_retries(texts[:midpoint], embed_batch, max_attempts, retry_delay, depth + 1)
            right = encode_with_retries(texts[midpoint:], embed_batch, max_attempts, retry_delay, depth + 1)
            return left + right

        last_err = err

    for attempt in range(2, max_attempts + 1):
        delay = min(30.0, retry_delay * (2 ** (attempt - 2)))
        print(
            f"embedding attempt {attempt}/{max_attempts} for {len(texts)} texts "
            f"after error: {last_err}; sleeping {delay:.1f}s",
            flush=True,
        )
        time.sleep(delay)
        try:
            return embed_batch(texts)
        except Exception as err:
            last_err = err

    raise last_err


def run_backfill(limit, batch_size, dry_run, fetch_batch, text_for_row, embed_batch, store_batch):
    started = time.time()
    processed = 0
    while processed < limit:
        remaining = limit - processed
        batch_limit = min(batch_size, remaining)
        batch = fetch_batch(batch_limit)
        if not batch:
            if processed == 0:
                print("No rows need embeddings.")
            break

        texts = [text_for_row(row) for row in batch]
        embeddings = encode_with_retries(texts, embed_batch)
        if not dry_run:
            store_batch(batch, embeddings)

        processed += len(batch)
        elapsed = time.time() - started
        rate = processed / elapsed if elapsed else 0
        print(
            f"processed={processed}/{limit} rate={rate:.2f}/s "
            f"elapsed={elapsed:.1f}s dry_run={dry_run}",
            flush=True,
        )
        if dry_run:
            print("dry-run stops after one streamed batch to avoid reselecting unchanged rows.")
            break

    total_elapsed = time.time() - started
    print(f"done processed={processed} seconds={total_elapsed:.1f}")
