#!/usr/bin/env python3
"""Archived MiniLM embedding service for offline migration only.

Qwen is canonical. This module is excluded from production images and refuses
startup unless ALLOW_LEGACY_MINILM_MIGRATION=1 is explicitly set.

Usage:
    ALLOW_LEGACY_MINILM_MIGRATION=1 \
      uvicorn embedding_service:app --host 127.0.0.1 --port 8000

Environment Variables:
    EMBEDDING_MODEL: Model name (default: all-MiniLM-L6-v2)
    DATABASE_URL: PostgreSQL connection string
"""

import os
import secrets
import sys
import time
import logging
from typing import List, Optional
from contextlib import asynccontextmanager

if os.environ.get("ALLOW_LEGACY_MINILM_MIGRATION") != "1":
    raise RuntimeError("legacy MiniLM service is archived and must not run in production")

import numpy as np
from fastapi import Depends, FastAPI, Header, HTTPException
from pydantic import BaseModel
from sentence_transformers import SentenceTransformer

# Configure logging
logging.basicConfig(
    level=logging.INFO,
    format='%(asctime)s - %(name)s - %(levelname)s - %(message)s'
)
logger = logging.getLogger(__name__)

# Suppress tokenizer parallelism warnings
os.environ['TOKENIZERS_PARALLELISM'] = 'false'

# Configuration
MODEL_NAME = os.environ.get('EMBEDDING_MODEL', 'all-MiniLM-L6-v2')
EMBEDDING_DIM = 384  # all-MiniLM-L6-v2 dimension
MAX_BATCH_SIZE = int(os.environ.get('EMBEDDING_MAX_BATCH_SIZE', '128'))
MUTATION_TOKEN = os.environ.get('EMBEDDING_MUTATION_TOKEN', '').strip()

# Global model instance
model: Optional[SentenceTransformer] = None


class EmbedRequest(BaseModel):
    """Request for single text embedding."""
    text: str


class EmbedBatchRequest(BaseModel):
    """Request for batch text embeddings."""
    texts: List[str]


class EmbedResponse(BaseModel):
    """Response containing embedding vector."""
    embedding: List[float]
    dimension: int
    model: str


class EmbedBatchResponse(BaseModel):
    """Response containing multiple embedding vectors."""
    embeddings: List[List[float]]
    dimension: int
    model: str
    count: int


class HealthResponse(BaseModel):
    """Health check response."""
    status: str
    model: str
    dimension: int
    ready: bool


class PaperEmbedRequest(BaseModel):
    """Request to generate embedding for a paper."""
    paper_id: str
    title: str
    abstract: str


class PaperEmbedBatchRequest(BaseModel):
    """Request to generate embeddings for multiple papers."""
    papers: List[PaperEmbedRequest]


class PaperEmbedResponse(BaseModel):
    """Response after storing paper embedding."""
    paper_id: str
    success: bool
    message: str


def require_mutation_auth(authorization: str = Header(default="")):
    if not MUTATION_TOKEN:
        raise HTTPException(status_code=503, detail="Mutation endpoints are disabled")
    scheme, _, credential = authorization.partition(" ")
    if scheme.lower() != "bearer" or not secrets.compare_digest(credential, MUTATION_TOKEN):
        raise HTTPException(status_code=401, detail="Bearer token required")


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Load model on startup, cleanup on shutdown."""
    global model

    logger.info(f"Loading embedding model: {MODEL_NAME}")
    start = time.time()

    # Suppress model loading messages
    import warnings
    with warnings.catch_warnings():
        warnings.simplefilter("ignore")
        model = SentenceTransformer(MODEL_NAME)

    elapsed = time.time() - start
    logger.info(f"Model loaded in {elapsed:.2f}s. Dimension: {model.get_sentence_embedding_dimension()}")

    yield

    # Cleanup
    logger.info("Shutting down embedding service")
    model = None


app = FastAPI(
    title="Embedding Service",
    description="Fast embedding generation with persistent model",
    version="1.0.0",
    lifespan=lifespan
)


@app.get("/health", response_model=HealthResponse)
async def health_check():
    """Check service health and model status."""
    return HealthResponse(
        status="healthy" if model is not None else "loading",
        model=MODEL_NAME,
        dimension=EMBEDDING_DIM,
        ready=model is not None
    )


@app.post("/embed", response_model=EmbedResponse)
async def embed_text(request: EmbedRequest):
    """Generate embedding for a single text."""
    if model is None:
        raise HTTPException(status_code=503, detail="Model not loaded")

    if not request.text.strip():
        raise HTTPException(status_code=400, detail="Text cannot be empty")

    embedding = model.encode([request.text], convert_to_numpy=True)[0]

    return EmbedResponse(
        embedding=embedding.astype(np.float32).tolist(),
        dimension=len(embedding),
        model=MODEL_NAME
    )


@app.post("/embed/batch", response_model=EmbedBatchResponse)
async def embed_batch(request: EmbedBatchRequest):
    """Generate embeddings for multiple texts."""
    if model is None:
        raise HTTPException(status_code=503, detail="Model not loaded")

    if not request.texts:
        raise HTTPException(status_code=400, detail="Texts list cannot be empty")
    if len(request.texts) > MAX_BATCH_SIZE:
        raise HTTPException(status_code=413, detail=f"At most {MAX_BATCH_SIZE} texts are allowed")

    # Filter empty texts
    texts = [t.strip() for t in request.texts if t.strip()]
    if not texts:
        raise HTTPException(status_code=400, detail="All texts are empty")

    embeddings = model.encode(texts, convert_to_numpy=True, show_progress_bar=False)

    return EmbedBatchResponse(
        embeddings=[e.astype(np.float32).tolist() for e in embeddings],
        dimension=EMBEDDING_DIM,
        model=MODEL_NAME,
        count=len(embeddings)
    )


def embedding_to_pgvector(embedding: np.ndarray) -> str:
    """Convert numpy array to pgvector string format: [0.1,0.2,...]"""
    return '[' + ','.join(map(str, embedding.astype(np.float32))) + ']'


def get_db_connection():
    """Get PostgreSQL database connection."""
    import psycopg2
    from urllib.parse import urlparse

    db_url = os.environ.get('DATABASE_URL', '')
    if not db_url:
        raise HTTPException(status_code=500, detail="DATABASE_URL not configured")

    parsed = urlparse(db_url)
    conn = psycopg2.connect(
        host=parsed.hostname,
        port=parsed.port or 5432,
        user=parsed.username,
        password=parsed.password,
        dbname=parsed.path.lstrip('/')
    )
    return conn


@app.post("/embed/paper", response_model=PaperEmbedResponse)
async def embed_paper(request: PaperEmbedRequest, _auth=Depends(require_mutation_auth)):
    """Generate and store embedding for a single paper."""
    if model is None:
        raise HTTPException(status_code=503, detail="Model not loaded")

    # Combine title and abstract
    text = f"{request.title}. {request.abstract}" if request.title and request.abstract else (request.title or request.abstract)

    if not text.strip():
        return PaperEmbedResponse(
            paper_id=request.paper_id,
            success=False,
            message="No title or abstract available"
        )

    try:
        # Generate embedding
        embedding = model.encode([text], convert_to_numpy=True)[0]
        vec_str = embedding_to_pgvector(embedding)

        # Store in database
        conn = get_db_connection()
        cursor = conn.cursor()

        cursor.execute("""
            INSERT INTO embeddings (paper_id, model, vector, created)
            VALUES (%s, %s, %s::vector, NOW())
            ON CONFLICT (paper_id) DO UPDATE SET model = %s, vector = %s::vector, created = NOW()
        """, (request.paper_id, MODEL_NAME, vec_str, MODEL_NAME, vec_str))

        conn.commit()
        cursor.close()
        conn.close()

        return PaperEmbedResponse(
            paper_id=request.paper_id,
            success=True,
            message="Embedding generated and stored"
        )
    except Exception as e:
        logger.error(f"Failed to embed paper {request.paper_id}: {e}")
        return PaperEmbedResponse(
            paper_id=request.paper_id,
            success=False,
            message=str(e)
        )


@app.post("/embed/papers")
async def embed_papers(request: PaperEmbedBatchRequest, _auth=Depends(require_mutation_auth)):
    """Generate and store embeddings for multiple papers."""
    if model is None:
        raise HTTPException(status_code=503, detail="Model not loaded")

    if not request.papers:
        raise HTTPException(status_code=400, detail="Papers list cannot be empty")
    if len(request.papers) > MAX_BATCH_SIZE:
        raise HTTPException(status_code=413, detail=f"At most {MAX_BATCH_SIZE} papers are allowed")

    # Prepare texts and filter empty ones
    valid_papers = []
    texts = []
    for paper in request.papers:
        text = f"{paper.title}. {paper.abstract}" if paper.title and paper.abstract else (paper.title or paper.abstract)
        if text.strip():
            valid_papers.append(paper)
            texts.append(text)

    if not texts:
        return {
            "success": True,
            "processed": 0,
            "skipped": len(request.papers),
            "message": "All papers have empty title/abstract"
        }

    try:
        # Generate embeddings in batch
        embeddings = model.encode(texts, convert_to_numpy=True, show_progress_bar=False)

        # Store in database
        conn = get_db_connection()
        cursor = conn.cursor()

        for paper, embedding in zip(valid_papers, embeddings):
            vec_str = embedding_to_pgvector(embedding)
            cursor.execute("""
                INSERT INTO embeddings (paper_id, model, vector, created)
                VALUES (%s, %s, %s::vector, NOW())
                ON CONFLICT (paper_id) DO UPDATE SET model = %s, vector = %s::vector, created = NOW()
            """, (paper.paper_id, MODEL_NAME, vec_str, MODEL_NAME, vec_str))

        conn.commit()
        cursor.close()
        conn.close()

        return {
            "success": True,
            "processed": len(valid_papers),
            "skipped": len(request.papers) - len(valid_papers),
            "message": f"Generated embeddings for {len(valid_papers)} papers"
        }
    except Exception as e:
        logger.error(f"Failed to embed papers batch: {e}")
        raise HTTPException(status_code=500, detail=str(e))


if __name__ == "__main__":
    import uvicorn
    port = int(os.environ.get("EMBEDDING_PORT", 8001))
    host = os.environ.get("EMBEDDING_HOST", "127.0.0.1")
    uvicorn.run(app, host=host, port=port)
