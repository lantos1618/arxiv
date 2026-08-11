#!/usr/bin/env bash
set -euo pipefail

ROOT="${ARXIV_REPOSITORY:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
MANIFEST="${ARXIV_SQL_MIGRATION_MANIFEST:-$ROOT/deploy/sql/manifest.txt}"
COMMAND="${1:-status}"
PSQL="$ROOT/tools/compose_psql.sh"

case "$COMMAND" in
  preflight|status|apply) ;;
  *) echo "usage: $0 {preflight|status|apply}" >&2; exit 2 ;;
esac

declare -a migrations=()
declare -A seen=()
while IFS= read -r migration || [[ -n "$migration" ]]; do
  [[ -z "$migration" || "$migration" == \#* ]] && continue
  if [[ "$migration" == */* || "$migration" != *.sql ]]; then
    echo "invalid migration manifest entry: $migration" >&2
    exit 2
  fi
  if [[ -n "${seen[$migration]:-}" ]]; then
    echo "duplicate migration manifest entry: $migration" >&2
    exit 2
  fi
  if [[ ! -f "$ROOT/deploy/sql/$migration" ]]; then
    echo "missing migration: deploy/sql/$migration" >&2
    exit 2
  fi
  seen[$migration]=1
  migrations+=("$migration")
done < "$MANIFEST"

if (( ${#migrations[@]} == 0 )); then
  echo "migration manifest is empty: $MANIFEST" >&2
  exit 2
fi

"$PSQL" -Atqc "SELECT current_database(), current_user, current_setting('server_version')" >/dev/null
missing_tables=$("$PSQL" -Atqc "SELECT count(*) FROM (VALUES ('papers'), ('embeddings_v2'), ('paper_chunks'), ('chunk_embeddings_v2')) AS required(name) WHERE to_regclass('public.' || name) IS NULL")
if [[ "$missing_tables" != "0" ]]; then
  echo "preflight failed: required application tables are missing ($missing_tables)" >&2
  exit 1
fi

if [[ "$COMMAND" == "preflight" ]]; then
  printf 'preflight ok migrations=%d manifest=%s\n' "${#migrations[@]}" "$MANIFEST"
  exit 0
fi

table_exists=$("$PSQL" -Atqc "SELECT CASE WHEN to_regclass('public.arxiv_schema_migrations') IS NULL THEN 0 ELSE 1 END")
if [[ "$COMMAND" == "apply" && "$table_exists" == "0" ]]; then
  "$PSQL" -qc "CREATE TABLE arxiv_schema_migrations (name text PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())"
  table_exists=1
fi

for migration in "${migrations[@]}"; do
  path="$ROOT/deploy/sql/$migration"
  digest=$(sha256sum "$path" | awk '{print $1}')
  applied=""
  if [[ "$table_exists" == "1" ]]; then
    applied=$("$PSQL" -At --set "migration=$migration" <<'SQL'
SELECT sha256 FROM arxiv_schema_migrations WHERE name = :'migration';
SQL
)
  fi

  if [[ -n "$applied" && "$applied" != "$digest" ]]; then
    echo "DRIFT $migration applied=$applied current=$digest" >&2
    exit 1
  fi
  if [[ -n "$applied" ]]; then
    echo "APPLIED $migration $digest"
    continue
  fi
  if [[ "$COMMAND" == "status" ]]; then
    echo "PENDING $migration $digest"
    continue
  fi

  echo "APPLYING $migration $digest"
  "$PSQL" --file - < "$path"
  "$PSQL" --set "migration=$migration" --set "digest=$digest" <<'SQL'
INSERT INTO arxiv_schema_migrations(name, sha256) VALUES (:'migration', :'digest');
SQL
  echo "APPLIED $migration $digest"
done
