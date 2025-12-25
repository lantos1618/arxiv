# Tools

External tools and scripts for the arXiv Cache Manager.

## generate_embeddings.py

Generates vector embeddings for arXiv papers using sentence-transformers.

### Setup

```bash
pip3 install -r requirements.txt
```

### Usage

```bash
# Generate embeddings for all papers without embeddings
python3 generate_embeddings.py ~/.cache/arxiv

# Generate embeddings for first 1000 papers
python3 generate_embeddings.py ~/.cache/arxiv --limit 1000

# Use different model
python3 generate_embeddings.py ~/.cache/arxiv --model sentence-transformers/all-mpnet-base-v2

# Adjust batch size
python3 generate_embeddings.py ~/.cache/arxiv --batch-size 64
```

### How It Works

1. Connects to SQLite database in cache directory
2. Finds papers without embeddings
3. Generates embeddings using sentence-transformers
4. Stores embeddings in `embeddings` table

### Models

- `all-MiniLM-L6-v2` (default) - 384 dims, fast, good quality
- `all-mpnet-base-v2` - 768 dims, slower, better quality
- `all-MiniLM-L12-v2` - 384 dims, slower than L6, better quality

### Performance

- ~100-200 papers/second (depends on model and hardware)
- For 10K papers: ~1-2 minutes
- For 100K papers: ~10-20 minutes
- For 2.4M papers: ~4-8 hours

### Notes

- Embeddings are stored as BLOB in SQLite
- Each embedding is ~1.5KB (384 dims × 4 bytes)
- 2.4M papers = ~3.6GB storage

