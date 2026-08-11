#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TOTAL="${QWEN_BACKFILL_TOTAL:-${1:-2048}}"
WORKERS="${QWEN_BACKFILL_WORKERS:-${2:-1}}"
FLAVOR="${QWEN_BACKFILL_FLAVOR:-ai1-1-gpu}"
CLAIM_SIZE="${QWEN_BACKFILL_CLAIM_SIZE:-4}"
BATCH_SIZE="${QWEN_BACKFILL_BATCH_SIZE:-4}"
PRIORITY="${QWEN_BACKFILL_PRIORITY:-10}"
ORDER="${QWEN_BACKFILL_ORDER:-id}"
TIMEOUT="${QWEN_BACKFILL_TIMEOUT:-75m}"
MAX_RUNTIME="${QWEN_BACKFILL_MAX_RUNTIME:-4200}"
NAME_PREFIX="${QWEN_BACKFILL_NAME_PREFIX:-arxiv-qwen-jit-backfill}"

if ! [[ "$TOTAL" =~ ^[0-9]+$ ]] || (( TOTAL < 1 )); then
  echo "TOTAL must be a positive integer" >&2
  exit 1
fi
if ! [[ "$WORKERS" =~ ^[0-9]+$ ]] || (( WORKERS < 1 )); then
  echo "WORKERS must be a positive integer" >&2
  exit 1
fi

QWEN_QUEUE_LIMIT="$TOTAL" \
  QWEN_QUEUE_PRIORITY="$PRIORITY" \
  QWEN_QUEUE_ORDER="$ORDER" \
  tools/qwen_queue_abstract_backfill.sh

worker_limit=$(( (TOTAL + WORKERS - 1) / WORKERS ))
stamp="$(date -u +%Y%m%d%H%M%S)"

for worker in $(seq 1 "$WORKERS"); do
  QWEN_OVH_FLAVOR="$FLAVOR" \
    QWEN_OVH_LIMIT="$worker_limit" \
    QWEN_OVH_CLAIM_SIZE="$CLAIM_SIZE" \
    QWEN_OVH_BATCH_SIZE="$BATCH_SIZE" \
    QWEN_OVH_KINDS="query,abstract" \
    QWEN_OVH_TIMEOUT="$TIMEOUT" \
    QWEN_OVH_MAX_RUNTIME="$MAX_RUNTIME" \
    QWEN_OVH_JOB_NAME="${NAME_PREFIX}-${stamp}-${worker}" \
    tools/run_ovh_qwen_worker.sh
done
