# System Setup Guide

## Requirements

### Hardware
- **RAM:** 8GB+ minimum (15GB+ recommended for large datasets)
- **CPU:** 4+ cores (8+ recommended for embedding generation)
- **Disk:** 100GB+ free space (500GB+ for full arXiv cache)

### Software
- **Go:** 1.18+ 
- **Python:** 3.8+ (for embedding generation)
- **SQLite:** 3.35+ (for FTS5 support)

## Installation

### 1. Build from Source
```bash
git clone https://github.com/tmc/arxiv.git
cd arxiv
make build
sudo make install  # Optional: installs to /usr/local/bin
```

### 2. Dependencies
```bash
# For embedding generation (optional)
pip3 install sentence-transformers numpy

# For advanced features (optional)
# PostgreSQL with pgvector extension for large-scale vector search
```

## Quick Start

### 1. Initialize Cache
```bash
export ARXIV_CACHE=/path/to/cache  # Defaults to ~/.cache/arxiv
arxiv sync -set cs                # Sync computer science papers (metadata only)
```

### 2. Fetch Papers
```bash
arxiv fetch 2301.00001           # Fetch single paper with source
arxiv fetch -pdf 2301.00001      # Fetch with PDF
arxiv fetch -all 2301.00001      # Fetch with source + PDF
```

### 3. Start Web Server
```bash
arxiv serve                      # Starts on http://localhost:8080
arxiv serve -port 3000           # Custom port
```

## Production Setup

### Large-Scale Deployment
```bash
# Use dedicated disk for cache
export ARXIV_CACHE=/data/arxiv

# Sync all categories (metadata only)
arxiv sync

# Generate embeddings for semantic search
python3 tools/generate_embeddings.py --cache $ARXIV_CACHE

# Start server with production settings
arxiv serve -port 8080
```

### Environment Variables
- `ARXIV_CACHE`: Cache directory path
- `ARXIV_RATE_LIMIT`: API rate limit (default: 100 req/min)
- `ARXIV_CACHE_SIZE`: LRU cache size in MB (default: 500)

## Testing Setup

```bash
# Verify installation
arxiv stats
arxiv search "machine learning" -limit 5

# Test web interface
curl http://localhost:8080/api/v1/stats
```

## Troubleshooting

### FTS5 Not Available
If you get "FTS5 not available" errors:
```bash
# Upgrade SQLite or use the included binary
sudo apt-get install sqlite3 libsqlite3-dev
```

### Memory Issues
For large datasets:
```bash
# Reduce cache size
export ARXIV_CACHE_SIZE=200

# Use disk-based storage
export ARXIV_CACHE=/data/arxiv
```

### Embedding Generation
```bash
# Test embedding model
python3 -c "
from sentence_transformers import SentenceTransformer
model = SentenceTransformer('all-MiniLM-L6-v2')
print('Model loaded, embedding dimension:', len(model.encode('test')))
"
```

