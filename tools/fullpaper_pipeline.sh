#!/usr/bin/env bash
set -euo pipefail

ROOT="${ARXIV_REPOSITORY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
APP_SERVICE="${ARXIV_APP_SERVICE:-arxiv}"
FETCH_LIMIT="${FULLPAPER_FETCH_LIMIT:-100}"
CHUNK_LIMIT="${FULLPAPER_CHUNK_LIMIT:-1000}"
EMBED_LIMIT="${FULLPAPER_EMBED_LIMIT:-1000}"
BATCH_SIZE="${FULLPAPER_EMBED_BATCH_SIZE:-8}"
CHUNK_SCOPE="${FULLPAPER_CHUNK_SCOPE:-pdf_text}"

for value_name in FETCH_LIMIT CHUNK_LIMIT EMBED_LIMIT BATCH_SIZE; do
  value="${!value_name}"
  if ! [[ "$value" =~ ^[1-9][0-9]*$ ]]; then
    echo "$value_name must be a positive integer" >&2
    exit 2
  fi
done

cd "$ROOT"
compose=(docker compose)

run_phase() {
  local phase="$1"
  shift
  printf '%s phase=%s status=starting\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$phase"
  "${compose[@]}" exec -T "$APP_SERVICE" "$@"
  printf '%s phase=%s status=complete\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$phase"
}

run_phase fetch python3 /app/tools/fetch_full_paper_text.py \
  --limit "$FETCH_LIMIT" \
  --categories "${FULLPAPER_FETCH_CATEGORIES:-cs.AI,cs.LG,cs.CL,cs.CV,stat.ML,cs.RO}" \
  --order "${FULLPAPER_FETCH_ORDER:-recent}" \
  --rate-limit-seconds "${FULLPAPER_FETCH_RATE_LIMIT_SECONDS:-3}"

run_phase chunk python3 /app/tools/chunk_full_papers.py \
  --limit "$CHUNK_LIMIT" \
  --select-batch-size "${FULLPAPER_CHUNK_SELECT_BATCH_SIZE:-100}" \
  --chunk-chars "${FULLPAPER_CHUNK_CHARS:-3000}" \
  --overlap-chars "${FULLPAPER_CHUNK_OVERLAP_CHARS:-300}" \
  --scope "$CHUNK_SCOPE"

run_phase embed python3 /app/tools/qwen_chunk_embeddings_v2.py \
  --limit "$EMBED_LIMIT" \
  --batch-size "$BATCH_SIZE" \
  --scope "$CHUNK_SCOPE" \
  --service-url "${QWEN_EMBEDDING_SERVICE_URL:?QWEN_EMBEDDING_SERVICE_URL is required}"
