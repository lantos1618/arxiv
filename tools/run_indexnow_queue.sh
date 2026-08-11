#!/usr/bin/env bash
set -euo pipefail

ROOT="${ARXIV_REPOSITORY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
QUEUE_FILE="${INDEXNOW_QUEUE_FILE:-/var/lib/arxiv/indexnow-urls}"
LOCK_FILE="${INDEXNOW_LOCK_FILE:-${QUEUE_FILE}.lock}"
PROCESSING_FILE="${QUEUE_FILE}.processing"

mkdir -p "$(dirname "$QUEUE_FILE")"
touch "$QUEUE_FILE"
exec 9>"$LOCK_FILE"
flock -n 9 || exit 0

if [[ ! -s "$QUEUE_FILE" ]]; then
  echo "IndexNow queue is empty"
  exit 0
fi

if [[ -e "$PROCESSING_FILE" ]]; then
  cat "$PROCESSING_FILE" >> "$QUEUE_FILE"
  rm -f "$PROCESSING_FILE"
fi
mv "$QUEUE_FILE" "$PROCESSING_FILE"
touch "$QUEUE_FILE"

restore_queue() {
  cat "$PROCESSING_FILE" >> "$QUEUE_FILE"
  rm -f "$PROCESSING_FILE"
}
trap restore_queue ERR INT TERM

python3 "$ROOT/tools/submit_indexnow.py" \
  --file "$PROCESSING_FILE" \
  --site-url "${SITE_URL:?SITE_URL is required}" \
  --key "${INDEXNOW_KEY:?INDEXNOW_KEY is required}" \
  --batch-size "${INDEXNOW_BATCH_SIZE:-1000}" \
  --timeout "${INDEXNOW_TIMEOUT_SECONDS:-30}"

rm -f "$PROCESSING_FILE"
trap - ERR INT TERM
