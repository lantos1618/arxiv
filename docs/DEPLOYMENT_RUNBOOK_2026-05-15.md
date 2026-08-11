# arxiv.gg Production Deployment Runbook

> **ACTIVE RUNBOOK.** The filename preserves its original date, but this procedure is maintained for the current Compose, secret-file, migration-manifest, PostgreSQL, and immutable-image deployment.

## Safety Model

- Production requires PostgreSQL with pgvector.
- Preserve the external `arxiv_postgres_data` volume and existing database container.
- Treat startup as schema-changing because `arxiv.Open` runs `AutoMigrate` and backend initialization.
- Apply reviewed SQL through the ordered manifest before replacing the app.
- Build from a clean exact commit and retain the exact current image for rollback.
- Remember that image rollback does not undo database changes.

Never run `docker compose down -v`, remove `arxiv_postgres_data`, prune volumes, or recreate PostgreSQL on an anonymous/new volume.

## 1. Preflight

From the repository root on the application host:

```bash
set -euo pipefail
test -z "$(git status --porcelain --untracked-files=normal)" || {
  echo 'refusing deployment from a dirty worktree' >&2
  exit 1
}
docker volume inspect arxiv_postgres_data >/dev/null
docker network inspect "${ARXIV_NETWORK_NAME:-arxiv-network}" >/dev/null
```

Prepare `.env` from the example. It contains deployment metadata, not secret values:

```bash
cp -n .env.example .env
set -a
. ./.env
set +a
test -n "${ARXIV_SECRETS_DIR:?set ARXIV_SECRETS_DIR}"
test "$(stat -c '%a' "$ARXIV_SECRETS_DIR")" = 700
for secret in postgres-password database-url admin-token qwen-worker-token google-client-secret; do
  test -r "$ARXIV_SECRETS_DIR/$secret" || {
    echo "missing secret: $ARXIV_SECRETS_DIR/$secret" >&2
    exit 1
  }
done
case "$(cat "$ARXIV_SECRETS_DIR/database-url")" in
  postgres://*|postgresql://*) ;;
  *) echo 'database-url secret is not PostgreSQL' >&2; exit 1 ;;
esac
```

`TRUST_PROXY_HEADERS` must remain `false` unless a trusted proxy is the sole ingress and overwrites forwarding headers. Validate the rendered configuration and secret mounts:

```bash
docker compose config >/tmp/arxiv-compose.rendered.yml
grep -A3 'arxiv_postgres_data:' /tmp/arxiv-compose.rendered.yml
grep -A12 'DATABASE_URL_FILE:' /tmp/arxiv-compose.rendered.yml
```

## 2. Database Backup

Confirm the running database and make both schema and full logical backups:

```bash
tools/compose_psql.sh -Atqc \
  "SELECT current_database(), current_user, current_setting('server_version')"

BACKUP_DIR=/var/backups/arxiv
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
sudo install -d -m 0700 "$BACKUP_DIR"
docker compose exec -T postgres \
  pg_dump -U "${POSTGRES_USER:-arxiv}" -d "${POSTGRES_DB:-arxiv}" --schema-only \
  | sudo tee "$BACKUP_DIR/schema-$STAMP.sql" >/dev/null
docker compose exec -T postgres \
  pg_dump -U "${POSTGRES_USER:-arxiv}" -d "${POSTGRES_DB:-arxiv}" --format=custom \
  | sudo tee "$BACKUP_DIR/database-$STAMP.dump" >/dev/null
sudo test -s "$BACKUP_DIR/schema-$STAMP.sql"
sudo test -s "$BACKUP_DIR/database-$STAMP.dump"
```

Test restoration periodically on a separate PostgreSQL instance. A backup that has never been restored is not a verified recovery plan.

## 3. SQL Migrations

The authoritative order is `deploy/sql/manifest.txt`. Validate files, database prerequisites, and applied-file hashes:

```bash
make migrations-preflight
make migrations-status
```

Review every pending SQL file. In particular, `2026-07-11-qwen-vector-not-null.sql` deletes invalid NULL-vector rows before adding `NOT NULL`; confirm the backup and expected cleanup before applying it.

If PostgreSQL is an existing external container rather than this Compose project's `postgres` service, set `ARXIV_POSTGRES_CONTAINER` to its explicit container name before running the migration commands.

Apply pending migrations:

```bash
make migrations-apply
make migrations-status
```

The migration tool records SHA-256 digests in `arxiv_schema_migrations` and stops on drift. Do not edit an applied migration; add a new ordered file instead.

## 4. Immutable Image

Build the exact commit and preserve the currently running image:

```bash
RELEASE="$(git rev-parse --verify HEAD)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
ROLLBACK_RELEASE="rollback-$(date -u +%Y%m%dT%H%M%SZ)"

docker build --pull \
  --build-arg BUILD_COMMIT="$RELEASE" \
  --build-arg BUILD_DATE="$BUILD_DATE" \
  --tag "arxiv.gg:$RELEASE" .
docker image inspect "arxiv.gg:$RELEASE" >/dev/null

APP_CONTAINER="$(docker compose ps -q arxiv)"
test -n "$APP_CONTAINER"
RUNNING_IMAGE_ID="$(docker inspect -f '{{.Image}}' "$APP_CONTAINER")"
docker image tag "$RUNNING_IMAGE_ID" "arxiv.gg:$ROLLBACK_RELEASE"
printf 'RELEASE=%q\nROLLBACK_RELEASE=%q\n' "$RELEASE" "$ROLLBACK_RELEASE" \
  > /tmp/arxiv-release.env
printf 'services:\n  arxiv:\n    image: arxiv.gg:%s\n' "$RELEASE" \
  > /tmp/arxiv-release.override.yml
```

Do not prune either image until verification and the rollback window are complete.

## 5. Application Swap

Replace only the application container. Keep PostgreSQL running:

```bash
docker compose -f docker-compose.yml -f /tmp/arxiv-release.override.yml \
  up -d --no-deps --no-build --force-recreate arxiv
```

The app loads `DATABASE_URL`, `ADMIN_TOKEN`, `QWEN_WORKER_TOKEN`, and `GOOGLE_CLIENT_SECRET` from mounted `*_FILE` secrets in `start.sh`. Startup fails instead of silently selecting SQLite when required production secrets are absent.

## 6. Verification

```bash
. /tmp/arxiv-release.env
APP_CONTAINER="$(docker compose -f docker-compose.yml -f /tmp/arxiv-release.override.yml ps -q arxiv)"
curl -fsS http://127.0.0.1/health
docker inspect -f '{{.Config.Image}}' "$APP_CONTAINER"
docker compose -f docker-compose.yml -f /tmp/arxiv-release.override.yml ps
docker logs --since 10m "$APP_CONTAINER"
make migrations-status
```

Required results:

- `/health` returns `{"success":true,"data":{"db":"postgres","status":"ok"}}`.
- The inspected image is `arxiv.gg:$RELEASE`.
- The app is healthy and PostgreSQL was not recreated.
- Migration status has no unexpected pending or drifted entries.
- Logs show no schema, secret, OAuth, Qwen, or repeated internal failures.

Also smoke-test public metadata search, a semantic response (Qwen or explicitly marked fallback), account sign-in when configured, and authenticated admin/worker access.

## 7. Rollback

If application verification fails and the migrated schema remains backward-compatible, reuse the exact saved image:

```bash
. /tmp/arxiv-release.env
printf 'services:\n  arxiv:\n    image: arxiv.gg:%s\n' "$ROLLBACK_RELEASE" \
  > /tmp/arxiv-release.override.yml
docker compose -f docker-compose.yml -f /tmp/arxiv-release.override.yml \
  up -d --no-deps --no-build --force-recreate arxiv
APP_CONTAINER="$(docker compose -f docker-compose.yml -f /tmp/arxiv-release.override.yml ps -q arxiv)"
curl -fsS http://127.0.0.1/health
docker inspect -f '{{.Config.Image}}' "$APP_CONTAINER"
```

The image must be `arxiv.gg:$ROLLBACK_RELEASE` and health must still identify PostgreSQL. Do not rebuild an old commit and call it rollback.

If a migration is incompatible with the prior image, stop. Image rollback alone is unsafe. Follow the separately reviewed database restore/forward-fix plan using the verified backup; never improvise destructive reversal on the live volume.
