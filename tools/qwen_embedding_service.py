#!/usr/bin/env python3
"""
Persistent Qwen embedding service for GPU workers.

Run this on the GPU box and call it from the app/backfill host through a
private network or SSH tunnel. The service deliberately has no database access:
it only turns text into normalized vectors, so losing the worker does not risk
the production database.

Example:
    QWEN_EMBEDDING_DEVICE=cuda uvicorn qwen_embedding_service:app --host 127.0.0.1 --port 8010
"""

import logging
import os
import time
from contextlib import asynccontextmanager
from typing import List, Optional, Tuple

import numpy as np
import torch
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field
from sentence_transformers import SentenceTransformer


logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)
logger = logging.getLogger("qwen-embedding-service")

os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")

MODEL_NAME = os.environ.get("QWEN_EMBEDDING_MODEL", "Qwen/Qwen3-Embedding-8B")
EMBEDDING_DIM = int(os.environ.get("QWEN_EMBEDDING_DIM", "1024"))
DEVICE = os.environ.get("QWEN_EMBEDDING_DEVICE", "cuda")
MAX_BATCH_SIZE = int(os.environ.get("QWEN_MAX_BATCH_SIZE", "128"))
MAX_TEXT_CHARS = int(os.environ.get("QWEN_MAX_TEXT_CHARS", "24000"))
TORCH_DTYPE = os.environ.get("QWEN_TORCH_DTYPE", "bfloat16").lower()

model: Optional[SentenceTransformer] = None
started_at = time.time()
requests_total = 0
errors_total = 0
oom_total = 0
last_success_at: Optional[float] = None
last_error_at: Optional[float] = None
last_error = ""


class EmbedBatchRequest(BaseModel):
    texts: List[str] = Field(min_length=1)


class EmbedBatchResponse(BaseModel):
    embeddings: List[List[float]]
    dimension: int
    model: str
    device: str
    count: int
    seconds: float


class HealthResponse(BaseModel):
    status: str
    ready: bool
    model: str
    dimension: int
    device: str
    max_batch_size: int
    max_text_chars: int
    torch_dtype: str
    uptime_seconds: float
    cuda_allocated_gb: float = 0
    cuda_reserved_gb: float = 0
    cuda_free_gb: float = 0
    cuda_total_gb: float = 0
    requests_total: int = 0
    errors_total: int = 0
    oom_total: int = 0
    last_success_seconds_ago: Optional[float] = None
    last_error_seconds_ago: Optional[float] = None
    last_error: str = ""


def cuda_memory_gb() -> Tuple[float, float, float, float]:
    if not torch.cuda.is_available():
        return 0.0, 0.0, 0.0, 0.0
    allocated = torch.cuda.memory_allocated() / 1024**3
    reserved = torch.cuda.memory_reserved() / 1024**3
    free, total = torch.cuda.mem_get_info()
    return allocated, reserved, free / 1024**3, total / 1024**3


def seconds_ago(value: Optional[float]) -> Optional[float]:
    if value is None:
        return None
    return round(time.time() - value, 3)


def resolve_torch_dtype(name: str):
    if name in ("auto", ""):
        return None
    if name in ("float16", "fp16", "half"):
        return torch.float16
    if name in ("bfloat16", "bf16"):
        return torch.bfloat16
    if name in ("float32", "fp32"):
        return torch.float32
    raise ValueError(f"unsupported QWEN_TORCH_DTYPE={name!r}")


def record_error(message: str, is_oom: bool = False) -> None:
    global errors_total, last_error_at, last_error, oom_total
    errors_total += 1
    if is_oom:
        oom_total += 1
    last_error_at = time.time()
    last_error = message[:500]


@asynccontextmanager
async def lifespan(app: FastAPI):
    global model

    torch_dtype = resolve_torch_dtype(TORCH_DTYPE)
    logger.info("loading model=%s dim=%s device=%s dtype=%s", MODEL_NAME, EMBEDDING_DIM, DEVICE, TORCH_DTYPE)
    start = time.time()
    model_kwargs = {}
    if torch_dtype is not None:
        model_kwargs["torch_dtype"] = torch_dtype
    model = SentenceTransformer(
        MODEL_NAME,
        device=DEVICE,
        model_kwargs=model_kwargs,
        processor_kwargs={"padding_side": "left"},
        truncate_dim=EMBEDDING_DIM,
    )
    logger.info("model loaded in %.1fs", time.time() - start)
    yield
    model = None
    if torch.cuda.is_available():
        torch.cuda.empty_cache()


app = FastAPI(
    title="arXiv.gg Qwen Embedding Service",
    version="1.0.0",
    lifespan=lifespan,
)


@app.get("/health", response_model=HealthResponse)
async def health():
    allocated, reserved, free, total = cuda_memory_gb()
    return HealthResponse(
        status="healthy" if model is not None else "loading",
        ready=model is not None,
        model=MODEL_NAME,
        dimension=EMBEDDING_DIM,
        device=DEVICE,
        max_batch_size=MAX_BATCH_SIZE,
        max_text_chars=MAX_TEXT_CHARS,
        torch_dtype=TORCH_DTYPE,
        uptime_seconds=time.time() - started_at,
        cuda_allocated_gb=round(allocated, 3),
        cuda_reserved_gb=round(reserved, 3),
        cuda_free_gb=round(free, 3),
        cuda_total_gb=round(total, 3),
        requests_total=requests_total,
        errors_total=errors_total,
        oom_total=oom_total,
        last_success_seconds_ago=seconds_ago(last_success_at),
        last_error_seconds_ago=seconds_ago(last_error_at),
        last_error=last_error,
    )


@app.post("/embed/batch", response_model=EmbedBatchResponse)
async def embed_batch(request: EmbedBatchRequest):
    global requests_total, last_success_at

    if model is None:
        raise HTTPException(status_code=503, detail="model not ready")
    if len(request.texts) > MAX_BATCH_SIZE:
        raise HTTPException(status_code=413, detail=f"batch too large; max {MAX_BATCH_SIZE}")

    texts = [" ".join(text.split())[:MAX_TEXT_CHARS] for text in request.texts]
    if any(text == "" for text in texts):
        raise HTTPException(status_code=400, detail="texts cannot be empty")

    requests_total += 1
    start = time.time()
    try:
        embeddings = model.encode(
            texts,
            batch_size=len(texts),
            normalize_embeddings=True,
            convert_to_numpy=True,
            show_progress_bar=False,
        )
    except torch.cuda.OutOfMemoryError as err:
        record_error(f"cuda out of memory batch={len(texts)} chars={MAX_TEXT_CHARS}: {err}", is_oom=True)
        logger.exception("cuda out of memory while embedding batch_size=%s", len(texts))
        if torch.cuda.is_available():
            torch.cuda.empty_cache()
        raise HTTPException(status_code=507, detail="cuda out of memory; retry with a smaller batch") from err
    except RuntimeError as err:
        if "out of memory" in str(err).lower():
            record_error(f"cuda out of memory batch={len(texts)} chars={MAX_TEXT_CHARS}: {err}", is_oom=True)
            logger.exception("cuda out of memory while embedding batch_size=%s", len(texts))
            if torch.cuda.is_available():
                torch.cuda.empty_cache()
            raise HTTPException(status_code=507, detail="cuda out of memory; retry with a smaller batch") from err
        record_error(f"embedding failed batch={len(texts)}: {err}")
        logger.exception("embedding failed while embedding batch_size=%s", len(texts))
        raise HTTPException(status_code=500, detail="embedding failed") from err
    embeddings = np.asarray(embeddings, dtype=np.float32)
    seconds = time.time() - start
    last_success_at = time.time()

    return EmbedBatchResponse(
        embeddings=[row.tolist() for row in embeddings],
        dimension=int(embeddings.shape[1]),
        model=MODEL_NAME,
        device=DEVICE,
        count=len(texts),
        seconds=seconds,
    )
