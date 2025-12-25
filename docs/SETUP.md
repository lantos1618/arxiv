# System Setup: Can This Box Handle It?

## Your Hardware

- **RAM:** 15GB ✅
- **CPU:** 8 cores ✅
- **Disk:** 495GB free on /data ✅
- **Tools:** Python 3.10, Go 1.18 ✅

## Can You Do It?

**YES.** You have more than enough.

## What You Need

1. **Generate embeddings** - Use open-source model (free) or OpenAI ($240 one-time)
2. **Store embeddings** - ~5-6GB for 2.4M papers (fits easily on /data)
3. **Vector search** - pgvector (PostgreSQL) or simple vector search

## Recommended Setup

- Store everything on `/data/arxiv/`
- Start with subset of papers (10K-100K)
- Test, then scale to full dataset
- Use open-source embeddings (free) or OpenAI (faster)

## Quick Test

```bash
pip3 install sentence-transformers
python3 -c "from sentence_transformers import SentenceTransformer; model = SentenceTransformer('all-MiniLM-L6-v2'); print(len(model.encode('test')))"
```

If that works, you're ready.

