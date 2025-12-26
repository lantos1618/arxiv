# Semantic Search Implementation Plan

> **Status:** Placeholder API exists, full implementation needed

## Current State
- API endpoint `/api/v1/search/semantic` exists but returns error
- Database has `embeddings` table ready
- Python script referenced (`generate_embeddings.py`) not in codebase

## Implementation Plan

### Phase 1: Embedding Generation
1. **Choose embedding model:**
   - Open source: `sentence-transformers/all-MiniLM-L6-v2` (384 dims, ~23MB)
   - Commercial: OpenAI `text-embedding-3-small` (1536 dims, cost ~$0.02/1K tokens)

2. **Generate embeddings for existing papers:**
   - Batch process title + abstract for all cached papers
   - Store in `embeddings` table as binary blobs
   - Progress tracking for large datasets

3. **Real-time embedding generation:**
   - Generate embeddings for new papers as they're fetched
   - Add to download pipeline

### Phase 2: Vector Search Implementation
1. **Simple approach (SQLite + cosine similarity):**
   - Load vectors into memory
   - Calculate cosine similarity in Go
   - Suitable for <100K papers

2. **Advanced approach (pgvector):**
   - PostgreSQL with pgvector extension
   - Indexed vector search
   - Scales to millions of papers

### Phase 3: API Integration
1. **Query embedding generation:**
   - Option A: Python subprocess call
   - Option B: Go embedding library
   - Option C: BYOK (Bring Your Own Key) for OpenAI

2. **Search endpoint:**
   - Generate query embedding
   - Find top N similar papers
   - Return ranked results

## Resource Requirements

### Storage
- Open source (384 dims): ~1.5MB per 10K papers
- OpenAI (1536 dims): ~6MB per 10K papers
- 2.4M arXiv papers: ~360MB (open source) to ~1.4GB (OpenAI)

### Memory
- In-memory vectors: ~500MB for 100K papers (open source)
- pgvector: Minimal memory with disk-based indexing

### Processing
- Initial generation: ~2-4 hours for 100K papers (CPU)
- Real-time: ~50ms per query (local) or ~200ms (OpenAI API)

## Development Priority

1. **MVP:** SQLite + cosine similarity + open source model
2. **Scale:** pgvector for larger datasets
3. **Performance:** In-memory caching + hybrid search

## Next Steps

1. Create `tools/generate_embeddings.py` script
2. Implement vector similarity functions in Go
3. Update semantic search API endpoint
4. Add embedding generation to paper fetch pipeline
5. Test with subset of papers (1K-10K)