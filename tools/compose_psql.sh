#!/usr/bin/env bash
set -euo pipefail

ROOT="${ARXIV_REPOSITORY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
if [[ -f "$ROOT/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT/.env"
  set +a
fi
DB_SERVICE="${ARXIV_DB_SERVICE:-postgres}"
POSTGRES_USER="${POSTGRES_USER:-arxiv}"
POSTGRES_DB="${POSTGRES_DB:-arxiv}"
POSTGRES_CONTAINER="${ARXIV_POSTGRES_CONTAINER:-}"

cd "$ROOT"
if [[ -n "$POSTGRES_CONTAINER" ]]; then
  exec docker exec -i "$POSTGRES_CONTAINER" \
    psql --no-psqlrc --set ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" "$@"
fi
exec docker compose exec -T "$DB_SERVICE" \
  psql --no-psqlrc --set ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" "$@"
