#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

IMAGE="${QWEN_OVH_IMAGE:?QWEN_OVH_IMAGE must name an accessible immutable worker image}"
API_BASE="${ARXIV_API_BASE:-https://arxiv.gg/api/v1}"
FLAVOR="${QWEN_OVH_FLAVOR:-ai1-1-gpu}"
FLAVOR_COUNT="${QWEN_OVH_FLAVOR_COUNT:-1}"
TIMEOUT="${QWEN_OVH_TIMEOUT:-1h}"
LIMIT="${QWEN_OVH_LIMIT:-32}"
KINDS="${QWEN_OVH_KINDS:-query,abstract}"
CLAIM_SIZE="${QWEN_OVH_CLAIM_SIZE:-1}"
BATCH_SIZE="${QWEN_OVH_BATCH_SIZE:-1}"
MAX_RUNTIME="${QWEN_OVH_MAX_RUNTIME:-3300}"
DTYPE="${QWEN_EMBEDDING_DTYPE:-fp16}"
LEASE_OWNER="${QWEN_JOB_WORKER_NAME:-ovh-qwen-jit-$(date -u +%Y%m%d%H%M%S)}"
LEASE_SECONDS="${QWEN_OVH_LEASE_SECONDS:-3600}"
NAME="${QWEN_OVH_JOB_NAME:-arxiv-qwen-jit-v100}"
TOKEN_VOLUME="${QWEN_OVH_TOKEN_VOLUME:-}"
TOKEN_FILE="${QWEN_OVH_TOKEN_FILE:-/run/secrets/QWEN_WORKER_TOKEN}"

if [[ -z "$TOKEN_VOLUME" ]]; then
  echo "QWEN_OVH_TOKEN_VOLUME is required for file-based worker token delivery" >&2
  exit 1
fi
secret_args=(--volume "$TOKEN_VOLUME" --env "ARXIV_WORKER_TOKEN_FILE=$TOKEN_FILE")

ovhai job run \
  --name "$NAME" \
  --flavor "$FLAVOR" \
  --flavor-count "$FLAVOR_COUNT" \
  --timeout "$TIMEOUT" \
  "${secret_args[@]}" \
  --env "ARXIV_API_BASE=$API_BASE" \
  --env "QWEN_EMBEDDING_DTYPE=$DTYPE" \
  --env "QWEN_JOB_KINDS=$KINDS" \
  --env "HF_XET_HIGH_PERFORMANCE=1" \
  --env "PYTHONUNBUFFERED=1" \
  "$IMAGE" \
  -- python /workspace/qwen_api_worker.py \
    --kinds "$KINDS" \
    --limit "$LIMIT" \
    --claim-size "$CLAIM_SIZE" \
    --batch-size "$BATCH_SIZE" \
    --max-runtime "$MAX_RUNTIME" \
    --lease-owner "$LEASE_OWNER" \
    --lease-seconds "$LEASE_SECONDS" \
    --dtype "$DTYPE" \
  | sed -n -E '/^(Created At:|Id:|  Name:|  Image:|    Flavor:|    Gpu Model:|  Timeout:|  State:|Updated At:|⚠)/p'
