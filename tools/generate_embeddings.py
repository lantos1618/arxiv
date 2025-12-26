#!/usr/bin/env python3
"""
Generate embeddings for arXiv papers using sentence-transformers.

Usage:
    python3 generate_embeddings.py <cache_dir> [--model MODEL] [--limit N] [--batch-size N]

Example:
    python3 generate_embeddings.py ~/.cache/arxiv --limit 1000
"""

import argparse
import sqlite3
import struct
import sys
from pathlib import Path

import numpy as np
from sentence_transformers import SentenceTransformer
from tqdm import tqdm

MODEL_NAME = "all-MiniLM-L6-v2"  # 384 dimensions, fast, good quality


def serialize_embedding(embedding):
    """Serialize numpy array to bytes (little-endian float32)."""
    return embedding.astype('float32').tobytes()


def deserialize_embedding(data):
    """Deserialize bytes to numpy array."""
    import numpy as np
    return np.frombuffer(data, dtype='float32')


def generate_single_embedding(query, model_name=MODEL_NAME):
    """Generate embedding for a single query string."""
    import numpy as np
    print(f"Loading model: {model_name}")
    model = SentenceTransformer(model_name)
    print(f"Model loaded. Embedding dimension: {model.get_sentence_embedding_dimension()}")
    
    # Generate embedding for the query
    embedding = model.encode([query], convert_to_numpy=True)[0]
    print(','.join(map(str, embedding.astype(np.float32))))


def generate_embeddings(cache_dir, model_name=MODEL_NAME, limit=None, batch_size=32, query=None):
    """Generate embeddings for papers in cache."""
    
    # If query is provided, generate single embedding
    if query:
        generate_single_embedding(query, model_name)
        return
    
    cache_path = Path(cache_dir)
    db_path = cache_path / "index.db"
    
    if not db_path.exists():
        print(f"Error: Database not found at {db_path}")
        sys.exit(1)
    
    print(f"Loading model: {model_name}")
    model = SentenceTransformer(model_name)
    print(f"Model loaded. Embedding dimension: {model.get_sentence_embedding_dimension()}")
    
    # Connect to database
    conn = sqlite3.connect(str(db_path))
    cursor = conn.cursor()
    
    # If query is provided, generate single embedding
    if query:
        generate_single_embedding(query, model_name)
        return
    
    # Check if embeddings table exists
    cursor.execute("""
        SELECT name FROM sqlite_master 
        WHERE type='table' AND name='embeddings'
    """)
    if not cursor.fetchone():
        print("Creating embeddings table...")
        cursor.execute("""
            CREATE TABLE embeddings (
                paper_id TEXT PRIMARY KEY,
                model TEXT,
                vector BLOB,
                created TIMESTAMP DEFAULT CURRENT_TIMESTAMP
            )
        """)
        conn.commit()
    
    # Get papers without embeddings
    query = """
        SELECT id, title, abstract 
        FROM papers 
        WHERE title != '' AND abstract != ''
        AND id NOT IN (SELECT paper_id FROM embeddings)
    """
    if limit:
        query += f" LIMIT {limit}"
    
    cursor.execute(query)
    papers = cursor.fetchall()
    
    if not papers:
        print("No papers need embeddings.")
        return
    
    print(f"Found {len(papers)} papers to process")
    
    # Process in batches
    processed = 0
    for i in tqdm(range(0, len(papers), batch_size), desc="Processing batches"):
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
            vector_bytes = serialize_embedding(embedding)
            cursor.execute("""
                INSERT OR REPLACE INTO embeddings (paper_id, model, vector, created)
                VALUES (?, ?, ?, datetime('now'))
            """, (paper_id, model_name, vector_bytes))
        
        processed += len(batch)
        if processed % 100 == 0:
            conn.commit()
    
    conn.commit()
    conn.close()
    
    print(f"\nDone! Generated embeddings for {processed} papers.")


def main():
    parser = argparse.ArgumentParser(description="Generate embeddings for arXiv papers")
    parser.add_argument("cache_dir", help="Path to arXiv cache directory")
    parser.add_argument("--model", default=MODEL_NAME, 
                       help=f"Embedding model to use (default: {MODEL_NAME})")
    parser.add_argument("--limit", type=int, default=None,
                       help="Limit number of papers to process")
    parser.add_argument("--batch-size", type=int, default=32,
                       help="Batch size for embedding generation (default: 32)")
    
    args = parser.parse_args()
    
    generate_embeddings(
        args.cache_dir,
        model_name=args.model,
        limit=args.limit,
        batch_size=args.batch_size
    )


if __name__ == "__main__":
    main()

