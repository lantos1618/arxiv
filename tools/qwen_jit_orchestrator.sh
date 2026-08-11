#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

export PATH="${QWEN_JIT_PATH:-/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin}:${PATH:-}"

INTERVAL_SECONDS="${QWEN_JIT_INTERVAL_SECONDS:-20}"
MAX_LIMIT="${QWEN_JIT_MAX_LIMIT:-8}"
CLAIM_SIZE="${QWEN_JIT_CLAIM_SIZE:-1}"
BATCH_SIZE="${QWEN_JIT_BATCH_SIZE:-1}"
KINDS="${QWEN_JIT_KINDS:-query,abstract}"
NAME_PREFIX="${QWEN_JIT_JOB_PREFIX:-arxiv-qwen-jit}"
TIMEOUT="${QWEN_JIT_TIMEOUT:-20m}"
LOCK_FILE="${QWEN_JIT_LOCK_FILE:-/tmp/arxiv-qwen-jit-orchestrator.lock}"

exec 9>"$LOCK_FILE"
flock 9

for value_name in INTERVAL_SECONDS MAX_LIMIT CLAIM_SIZE BATCH_SIZE; do
  value="${!value_name}"
  if ! [[ "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "$value_name must be a positive integer" >&2
    exit 1
  fi
done

log() {
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*"
}

queued_count() {
  tools/compose_psql.sh -At -c "
    SELECT count(*)
    FROM qwen_embedding_jobs
    WHERE kind IN ('query', 'abstract')
      AND (
        status = 'queued'
        OR (
          status = 'running'
          AND lease_until IS NOT NULL
          AND lease_until < now()
        )
      );
  " | tr -d '[:space:]'
}

active_job_count() {
  ovhai job list 2>/dev/null | awk -v prefix="$NAME_PREFIX" '
    NR == 1 { next }
    $2 ~ ("^" prefix) && $3 !~ /^(DONE|FAILED|INTERRUPTED|CANCELLED)$/ { count++ }
    END { print count + 0 }
  '
}

launch_worker() {
  local queued="$1"
  local limit="$queued"
  if (( limit > MAX_LIMIT )); then
    limit="$MAX_LIMIT"
  fi
  if (( limit < 1 )); then
    return 0
  fi

  local stamp
  stamp="$(date -u +%Y%m%d%H%M%S)"
  log "launching OVH Qwen worker queued=$queued limit=$limit claim_size=$CLAIM_SIZE batch_size=$BATCH_SIZE kinds=$KINDS timeout=$TIMEOUT"
  QWEN_OVH_LIMIT="$limit" \
    QWEN_OVH_CLAIM_SIZE="$CLAIM_SIZE" \
    QWEN_OVH_BATCH_SIZE="$BATCH_SIZE" \
    QWEN_OVH_KINDS="$KINDS" \
    QWEN_OVH_TIMEOUT="$TIMEOUT" \
    QWEN_OVH_JOB_NAME="${NAME_PREFIX}-${stamp}" \
    tools/run_ovh_qwen_worker.sh
}

log "Qwen JIT orchestrator starting interval=${INTERVAL_SECONDS}s max_limit=$MAX_LIMIT claim_size=$CLAIM_SIZE batch_size=$BATCH_SIZE kinds=$KINDS"

while true; do
  queued="0"
  if ! queued="$(queued_count 2>/dev/null)"; then
    log "could not read qwen queue; retrying"
    sleep "$INTERVAL_SECONDS"
    continue
  fi

  if (( queued > 0 )); then
    active="1"
    if active="$(active_job_count)"; then
      if (( active == 0 )); then
        if ! launch_worker "$queued"; then
          log "OVH worker launch failed; retrying later"
        fi
      else
        log "queued=$queued but active OVH Qwen job count=$active"
      fi
    else
      log "could not list OVH jobs; retrying"
    fi
  fi

  sleep "$INTERVAL_SECONDS"
done
