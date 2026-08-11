#!/usr/bin/env python3
"""
Qwen worker for disposable OVH AI Training jobs.

Unlike qwen_job_worker.py, this script never talks to Postgres. It claims text
from arxiv.gg over HTTPS, embeds on the GPU inside the AI Training job, submits
vectors back, and exits so the GPU allocation is released.
"""

import argparse
import json
import math
import os
import sys
import threading
import time
import urllib.error
import urllib.request


DEFAULT_MODEL = "Qwen/Qwen3-Embedding-8B"
DEFAULT_DIM = 1024


class HTTPStatusError(RuntimeError):
    def __init__(self, method, url, status, detail):
        super().__init__(f"{method} {url} failed: HTTP {status}: {detail}")
        self.status = status


class TransportError(RuntimeError):
    pass


def request_json(method, url, token, payload=None, timeout=120, user_agent="arxiv-qwen-worker/1.0 (+https://arxiv.gg)"):
    data = None
    if payload is not None:
        data = json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=data,
        headers={
            "Authorization": "Bearer " + token,
            "Content-Type": "application/json",
            "Accept": "application/json",
            "User-Agent": user_agent,
        },
        method=method,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
    except urllib.error.HTTPError as err:
        detail = err.read().decode("utf-8", "replace")
        raise HTTPStatusError(method, url, err.code, detail) from err
    except (TimeoutError, urllib.error.URLError) as err:
        raise TransportError(f"{method} {url} transport failed: {err}") from err
    if not body:
        return {}
    data = json.loads(body)
    if not data.get("success", False):
        raise RuntimeError(f"{method} {url} failed: {data.get('error', data)}")
    return data.get("data") or {}


def parse_kinds(value):
    kinds = []
    seen = set()
    for raw in value.split(","):
        kind = raw.strip()
        if not kind or kind in seen:
            continue
        kinds.append(kind)
        seen.add(kind)
    return kinds


def claim(api_base, token, kinds, limit, lease_owner, lease_seconds, timeout):
    return request_json(
        "POST",
        api_base.rstrip("/") + "/qwen/jobs/claim",
        token,
        {
            "kinds": kinds,
            "limit": limit,
            "leaseOwner": lease_owner,
            "leaseSeconds": lease_seconds,
        },
        timeout=timeout,
    ).get("jobs", [])


def fence_payload(job, lease_owner):
    payload = {"leaseOwner": job.get("leaseOwner") or lease_owner}
    if job.get("leaseGeneration") is not None:
        payload["leaseGeneration"] = job["leaseGeneration"]
    if job.get("leaseToken"):
        payload["leaseToken"] = job["leaseToken"]
    return payload


def complete(api_base, token, job, embedding, lease_owner, timeout):
    payload = fence_payload(job, lease_owner)
    payload["embedding"] = [float(x) for x in embedding]
    return request_json(
        "POST",
        api_base.rstrip("/") + f"/qwen/jobs/{job['id']}/complete",
        token,
        payload,
        timeout=timeout,
    )


def fail(api_base, token, job, lease_owner, message, timeout):
    payload = fence_payload(job, lease_owner)
    payload["error"] = str(message)[:1000]
    try:
        request_json(
            "POST",
            api_base.rstrip("/") + f"/qwen/jobs/{job['id']}/fail",
            token,
            payload,
            timeout=timeout,
        )
    except Exception as err:
        print(f"could not mark job {job['id']} failed: {err}", flush=True)


def job_status(api_base, token, job_id, timeout):
    data = request_json(
        "GET",
        api_base.rstrip("/") + f"/qwen/jobs/{job_id}",
        token,
        timeout=timeout,
    )
    job = data.get("job") if isinstance(data.get("job"), dict) else data
    return str(job.get("status", "")).lower()


def complete_with_resolution(api_base, token, job, embedding, lease_owner, timeout):
    last_error = None
    for attempt in range(1, 4):
        try:
            complete(api_base, token, job, embedding, lease_owner, timeout)
            return "complete"
        except HTTPStatusError as err:
            if err.status < 500:
                raise
            last_error = err
        except (TransportError, json.JSONDecodeError) as err:
            last_error = err
        try:
            status = job_status(api_base, token, job["id"], min(timeout, 30))
        except (HTTPStatusError, TransportError, ValueError, json.JSONDecodeError):
            status = ""
        if status == "complete":
            return "complete"
        if status in ("failed", "queued"):
            raise RuntimeError(f"job {job['id']} has status {status} after ambiguous completion") from last_error
        if attempt < 3:
            time.sleep(attempt * 2)
    print(
        f"job {job['id']} completion is ambiguous after retries; leaving lease state unchanged: {last_error}",
        flush=True,
    )
    return "ambiguous"


class LeaseHeartbeats:
    def __init__(self, api_base, token, jobs, lease_owner, lease_seconds, timeout):
        self.api_base = api_base
        self.token = token
        self.jobs = jobs
        self.lease_owner = lease_owner
        self.lease_seconds = lease_seconds
        self.timeout = min(timeout, 30)
        self.stop_event = threading.Event()
        self.thread = None

    def __enter__(self):
        self.thread = threading.Thread(target=self.run, name="qwen-lease-heartbeats", daemon=True)
        self.thread.start()
        return self

    def __exit__(self, _exc_type, _exc, _traceback):
        self.stop_event.set()
        self.thread.join(timeout=self.timeout + 1)

    def run(self):
        interval = max(10.0, min(self.lease_seconds / 3.0, 60.0))
        while not self.stop_event.wait(interval):
            for job in self.jobs:
                try:
                    request_json(
                        "POST",
                        self.api_base.rstrip("/") + f"/qwen/jobs/{job['id']}/heartbeat",
                        self.token,
                        {**fence_payload(job, self.lease_owner), "leaseSeconds": self.lease_seconds},
                        timeout=self.timeout,
                    )
                except HTTPStatusError as err:
                    if err.status in (404, 405):
                        return
                    print(f"job {job['id']} heartbeat rejected: {err}", flush=True)
                except Exception as err:
                    print(f"job {job['id']} heartbeat failed: {err}", flush=True)


def load_model(model_name, dim, device, dtype_name):
    import torch
    from sentence_transformers import SentenceTransformer

    dtype_by_name = {
        "bfloat16": torch.bfloat16,
        "bf16": torch.bfloat16,
        "float16": torch.float16,
        "fp16": torch.float16,
        "float32": torch.float32,
        "fp32": torch.float32,
        "auto": "auto",
    }
    dtype = dtype_by_name.get(dtype_name.strip().lower())
    if dtype is None:
        raise ValueError(f"unsupported dtype {dtype_name!r}")

    return SentenceTransformer(
        model_name,
        device=device,
        model_kwargs={"torch_dtype": dtype},
        processor_kwargs={"padding_side": "left"},
        truncate_dim=dim,
    )


def encode(model, texts, batch_size):
    return model.encode(
        texts,
        batch_size=batch_size,
        normalize_embeddings=True,
        convert_to_numpy=True,
        show_progress_bar=False,
    )


def validated_embedding(embedding, expected_dim):
    if embedding is None or len(embedding) != expected_dim:
        actual_dim = 0 if embedding is None else len(embedding)
        raise ValueError(f"embedding has dim={actual_dim}; want {expected_dim}")
    values = [float(value) for value in embedding]
    for index, value in enumerate(values):
        if not math.isfinite(value):
            raise ValueError(f"embedding contains non-finite value at index {index}")
    return values


def main():
    parser = argparse.ArgumentParser(description="Run Qwen abstract jobs from arxiv.gg over HTTPS")
    parser.add_argument("--api-base", default=os.environ.get("ARXIV_API_BASE", "https://arxiv.gg/api/v1"))
    parser.add_argument("--token", default=os.environ.get("ARXIV_WORKER_TOKEN", ""))
    parser.add_argument("--token-file", default=os.environ.get("ARXIV_WORKER_TOKEN_FILE", ""))
    parser.add_argument("--model", default=os.environ.get("QWEN_EMBEDDING_MODEL", DEFAULT_MODEL))
    parser.add_argument("--dim", type=int, default=int(os.environ.get("QWEN_EMBEDDING_DIM", str(DEFAULT_DIM))))
    parser.add_argument("--device", default=os.environ.get("QWEN_EMBEDDING_DEVICE", "cuda"))
    parser.add_argument("--dtype", default=os.environ.get("QWEN_EMBEDDING_DTYPE", "bfloat16"))
    parser.add_argument("--kinds", default=os.environ.get("QWEN_JOB_KINDS", "query,abstract"))
    parser.add_argument("--limit", type=int, default=32)
    parser.add_argument("--claim-size", type=int, default=1)
    parser.add_argument("--batch-size", type=int, default=1)
    parser.add_argument("--max-runtime", type=float, default=3300)
    parser.add_argument("--lease-owner", default=os.environ.get("QWEN_JOB_WORKER_NAME", "ovh-ai-training"))
    parser.add_argument("--lease-seconds", type=int, default=3600)
    parser.add_argument("--idle-sleep", type=float, default=0)
    parser.add_argument("--timeout", type=float, default=180)
    parser.add_argument("--claim-only", action="store_true")
    args = parser.parse_args()

    if not args.token and args.token_file:
        with open(args.token_file, encoding="utf-8") as token_file:
            args.token = token_file.read().strip()
    if not args.token:
        raise SystemExit("--token or ARXIV_WORKER_TOKEN is required")
    if args.dim != DEFAULT_DIM:
        raise SystemExit("This worker currently expects 1024d Qwen embeddings")
    kinds = parse_kinds(args.kinds)
    if not kinds:
        raise SystemExit("--kinds must include at least one job kind")
    if args.limit <= 0 or args.claim_size <= 0 or args.batch_size <= 0:
        raise SystemExit("--limit, --claim-size, and --batch-size must be positive")
    if args.lease_seconds < 30:
        raise SystemExit("--lease-seconds must be at least 30")
    if args.max_runtime < 0 or args.idle_sleep < 0 or args.timeout <= 0:
        raise SystemExit("--max-runtime and --idle-sleep must be non-negative; --timeout must be positive")

    started = time.time()
    processed = 0
    model = None
    if not args.claim_only:
        print(f"loading model={args.model} dim={args.dim} device={args.device} dtype={args.dtype}", flush=True)
        model = load_model(args.model, args.dim, args.device, args.dtype)

    while processed < args.limit:
        if args.max_runtime > 0 and time.time() - started >= args.max_runtime:
            break
        jobs = claim(args.api_base, args.token, kinds, min(args.claim_size, args.limit - processed), args.lease_owner, args.lease_seconds, args.timeout)
        if not jobs:
            print("no jobs claimed", flush=True)
            if args.idle_sleep > 0:
                time.sleep(args.idle_sleep)
                continue
            break
        if args.claim_only:
            print(f"claim-only mode claimed {len(jobs)} jobs; leaving them leased", flush=True)
            break
        try:
            with LeaseHeartbeats(
                args.api_base,
                args.token,
                jobs,
                args.lease_owner,
                args.lease_seconds,
                args.timeout,
            ):
                texts = [job["text"] for job in jobs]
                embeddings = encode(model, texts, args.batch_size)
                if len(embeddings) != len(jobs):
                    raise RuntimeError(f"model returned {len(embeddings)} embeddings for {len(jobs)} jobs")
        except Exception as err:
            for job in jobs:
                fail(args.api_base, args.token, job, args.lease_owner, err, args.timeout)
            raise

        for job, embedding in zip(jobs, embeddings):
            job_id = job["id"]
            try:
                embedding = validated_embedding(embedding, args.dim)
            except (TypeError, ValueError) as err:
                fail(args.api_base, args.token, job, args.lease_owner, err, args.timeout)
                print(f"job {job_id} invalid embedding: {err}", flush=True)
                processed += 1
                continue
            try:
                result = complete_with_resolution(
                    args.api_base,
                    args.token,
                    job,
                    embedding,
                    args.lease_owner,
                    args.timeout,
                )
            except Exception as err:
                print(f"job {job_id} failed submit: {err}", flush=True)
            else:
                target = job.get("queryHash") or job.get("paperId", "")
                print(f"job {job_id} {result} kind={job.get('kind')} target={target}", flush=True)
            processed += 1

    elapsed = time.time() - started
    print(f"done processed={processed} seconds={elapsed:.1f}", flush=True)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        sys.exit(130)
