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

## Qwen v2 GPU Pipeline

The v2 pipeline keeps production MiniLM embeddings untouched while backfilling
1024d Qwen vectors into separate pgvector tables.

Run the model once on a GPU worker:

```bash
cd ~/arxiv-embedding-worker
source .venv/bin/activate
QWEN_EMBEDDING_DEVICE=cuda uvicorn qwen_embedding_service:app --host 127.0.0.1 --port 8010
```

Create an SSH tunnel from the DB/app box:

```bash
ssh -N -L 8010:127.0.0.1:8010 ubuntu@GPU_HOST
```

Backfill abstract embeddings from the DB/app box:

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_embeddings_v2.py --limit 10000 --batch-size 16
```

Refresh Qwen abstracts whose stored source hash no longer matches the title +
abstract text:

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_embeddings_v2.py --limit 10000 --batch-size 16 --refresh-stale
```

Fetch/extract full-paper text, then backfill chunks separately:

```bash
python3 tools/fetch_full_paper_text.py \
  --limit 100 \
  --categories cs.AI,cs.LG,cs.CL,cs.CV,stat.ML,cs.RO \
  --rate-limit-seconds 3
python3 tools/chunk_full_papers.py --limit 1000
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_chunk_embeddings_v2.py --limit 10000 --batch-size 16
```

`fetch_full_paper_text.py` downloads PDFs into a temporary file only, extracts
text with `pdftotext`, stores `papers.pdf_text`, and tracks retry state in
`full_paper_fetch_status`. PDFs are not persisted by this worker.

Check the GPU service and recent DB progress:

```bash
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_pipeline_check.py --scope both --window-minutes 15 --min-recent 1
```

Backfills retry transient failures and split oversized batches, so a CUDA OOM
should reduce batch size automatically instead of stopping the whole run.

Refresh already-chunked papers after changing chunk size or text extraction:

```bash
python3 tools/chunk_full_papers.py --limit 1000 --refresh-existing
QWEN_EMBEDDING_SERVICE_URL=http://127.0.0.1:8010 \
python3 tools/qwen_chunk_embeddings_v2.py --limit 10000 --batch-size 16
```
