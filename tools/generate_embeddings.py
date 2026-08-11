#!/usr/bin/env python3
"""Archived offline migration utility for legacy MiniLM vectors.

Qwen is the canonical embedding model. This utility is excluded from production
images and requires ALLOW_LEGACY_MINILM_MIGRATION=1 before it will run.

Usage:
    python3 generate_embeddings.py <cache_dir> [--model MODEL] [--limit N] [--batch-size N]

Example:
    python3 generate_embeddings.py ~/.cache/arxiv --limit 1000
"""

import argparse
import os
import sys
from pathlib import Path
from urllib.parse import urlparse

if os.environ.get("ALLOW_LEGACY_MINILM_MIGRATION") != "1":
    raise SystemExit(
        "legacy MiniLM generation is archived; set "
        "ALLOW_LEGACY_MINILM_MIGRATION=1 only for an offline migration"
    )

import numpy as np
from sentence_transformers import SentenceTransformer
from tqdm import tqdm

MODEL_NAME = "all-MiniLM-L6-v2"  # 384 dimensions, fast, good quality


def get_db_connection():
    """Get database connection from DATABASE_URL or fall back to SQLite."""
    db_url = os.environ.get('DATABASE_URL', '')

    if db_url.startswith('postgres'):
        import psycopg2
        # Parse postgres URL
        parsed = urlparse(db_url)
        conn = psycopg2.connect(
            host=parsed.hostname,
            port=parsed.port or 5432,
            user=parsed.username,
            password=parsed.password,
            dbname=parsed.path.lstrip('/')
        )
        return conn, 'postgres'
    else:
        import sqlite3
        cache_dir = os.environ.get('ARXIV_CACHE', '/data/arxiv')
        db_path = Path(cache_dir) / "index.db"
        return sqlite3.connect(str(db_path)), 'sqlite'


def embedding_to_pgvector(embedding):
    """Convert numpy array to pgvector string format: [0.1,0.2,...]"""
    return '[' + ','.join(map(str, embedding.astype(np.float32))) + ']'


def serialize_embedding(embedding):
    """Serialize numpy array to bytes (little-endian float32) for SQLite."""
    return embedding.astype('float32').tobytes()


def generate_single_embedding(query, model_name=MODEL_NAME):
    import numpy as np
    import sys
    import os
    os.environ['TOKENIZERS_PARALLELISM'] = 'false'
    with open(os.devnull, 'w') as devnull:
        old_stderr = sys.stderr
        sys.stderr = devnull
        model = SentenceTransformer(model_name)
        sys.stderr = old_stderr
    embedding = model.encode([query], convert_to_numpy=True)[0]
    print(','.join(map(str, embedding.astype(np.float32))))


def generate_embeddings(cache_dir, model_name=MODEL_NAME, limit=None, batch_size=32, query=None):
    """Generate embeddings for papers in cache."""

    # If query is provided, generate single embedding
    if query:
        generate_single_embedding(query, model_name)
        return

    print(f"Loading model: {model_name}")
    model = SentenceTransformer(model_name)
    print(f"Model loaded. Embedding dimension: {model.get_sentence_embedding_dimension()}")

    # Connect to database
    conn, db_type = get_db_connection()
    cursor = conn.cursor()

    # Get papers without embeddings
    query_sql = """
        SELECT id, title, abstract
        FROM papers
        WHERE title != '' AND abstract != ''
        AND id NOT IN (SELECT paper_id FROM embeddings WHERE vector IS NOT NULL)
    """
    if limit:
        query_sql += f" LIMIT {limit}"

    cursor.execute(query_sql)
    papers = cursor.fetchall()

    if not papers:
        print("No papers need embeddings.")
        return

    total_papers = len(papers)
    print(f"Found {total_papers} papers to process")
    sys.stdout.flush()

    # Process in batches
    processed = 0
    for i in range(0, total_papers, batch_size):
        batch = papers[i:i+batch_size]

        # Prepare texts (title + abstract)
        texts = []
        paper_ids = []
        for paper_id, title, abstract in batch:
            # Combine title and abstract
            text = f"{title}. {abstract}" if title and abstract else (title or abstract)
            texts.append(text)
            paper_ids.append(paper_id)

        # Generate embeddings
        embeddings = model.encode(texts, show_progress_bar=False, convert_to_numpy=True)

        # Store embeddings
        for paper_id, embedding in zip(paper_ids, embeddings):
            if db_type == 'postgres':
                vec_str = embedding_to_pgvector(embedding)
                cursor.execute("""
                    INSERT INTO embeddings (paper_id, model, vector, created)
                    VALUES (%s, %s, %s::vector, NOW())
                    ON CONFLICT (paper_id) DO UPDATE SET model = %s, vector = %s::vector, created = NOW()
                """, (paper_id, model_name, vec_str, model_name, vec_str))
            else:
                vector_bytes = serialize_embedding(embedding)
                cursor.execute("""
                    INSERT OR REPLACE INTO embeddings (paper_id, model, vector, created)
                    VALUES (?, ?, ?, datetime('now'))
                """, (paper_id, model_name, vector_bytes))

        processed += len(batch)

        # Output progress in format expected by SSE handler
        percent = (processed / total_papers) * 100
        print(f"Processed {processed}/{total_papers} papers ({percent:.1f}% complete)")
        sys.stdout.flush()

        if processed % 100 == 0:
            conn.commit()

    conn.commit()
    conn.close()

    print(f"Done! Generated embeddings for {processed} papers.")


def generate_paper_embedding(cache_dir, paper_id, model_name=MODEL_NAME):
    """Generate embedding for a single paper by ID."""
    conn, db_type = get_db_connection()
    cursor = conn.cursor()

    if db_type == 'postgres':
        cursor.execute("SELECT id, title, abstract FROM papers WHERE id = %s", (paper_id,))
    else:
        cursor.execute("SELECT id, title, abstract FROM papers WHERE id = ?", (paper_id,))

    row = cursor.fetchone()

    if not row:
        print(f"ERROR: Paper {paper_id} not found")
        conn.close()
        sys.exit(1)

    paper_id, title, abstract = row

    if not title and not abstract:
        print(f"ERROR: Paper {paper_id} has no title or abstract")
        conn.close()
        sys.exit(1)

    model = SentenceTransformer(model_name)
    text = f"{title}. {abstract}" if title and abstract else (title or abstract)
    embedding = model.encode([text], convert_to_numpy=True)[0]

    if db_type == 'postgres':
        vec_str = embedding_to_pgvector(embedding)
        cursor.execute("""
            INSERT INTO embeddings (paper_id, model, vector, created)
            VALUES (%s, %s, %s::vector, NOW())
            ON CONFLICT (paper_id) DO UPDATE SET model = %s, vector = %s::vector, created = NOW()
        """, (paper_id, model_name, vec_str, model_name, vec_str))
    else:
        vector_bytes = serialize_embedding(embedding)
        cursor.execute("""
            INSERT OR REPLACE INTO embeddings (paper_id, model, vector, created)
            VALUES (?, ?, ?, datetime('now'))
        """, (paper_id, model_name, vector_bytes))

    conn.commit()
    conn.close()
    print(f"OK: Generated embedding for {paper_id}")


def main():
    parser = argparse.ArgumentParser(description="Generate embeddings for arXiv papers")
    parser.add_argument("cache_dir", help="Path to arXiv cache directory")
    parser.add_argument("--model", default=MODEL_NAME,
                       help=f"Embedding model to use (default: {MODEL_NAME})")
    parser.add_argument("--limit", type=int, default=None,
                       help="Limit number of papers to process")
    parser.add_argument("--batch-size", type=int, default=32,
                       help="Batch size for embedding generation (default: 32)")
    parser.add_argument("--query", type=str, default=None,
                       help="Generate embedding for a query string (prints comma-separated floats)")
    parser.add_argument("--paper-id", type=str, default=None,
                       help="Generate embedding for a single paper by ID")

    args = parser.parse_args()

    if args.query:
        generate_single_embedding(args.query, args.model)
    elif args.paper_id:
        generate_paper_embedding(args.cache_dir, args.paper_id, args.model)
    else:
        generate_embeddings(
            args.cache_dir,
            model_name=args.model,
            limit=args.limit,
            batch_size=args.batch_size
        )


if __name__ == "__main__":
    main()
