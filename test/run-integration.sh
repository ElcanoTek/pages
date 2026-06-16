#!/usr/bin/env bash
# test/run-integration.sh — spin a throwaway Postgres, migrate, vendor Flag,
# seed, and run the /raw render integration check. Self-contained; cleans up.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PGPORT=5497
PGUSER=pages
SOCK=/tmp
DATADIR="$(mktemp -d /tmp/pages-pgtest.XXXXXX)"
PGDATA="$DATADIR/data"

cleanup() {
  runuser -u postgres -- pg_ctl -D "$PGDATA" stop -m immediate >/dev/null 2>&1 || true
  rm -rf "$DATADIR"
}
trap cleanup EXIT

# Postgres refuses to run as root → run the cluster as the postgres system user.
chown -R postgres:postgres "$DATADIR"
runuser -u postgres -- initdb -D "$PGDATA" -A trust -U "$PGUSER" >/dev/null
# Use a startup-log path OUTSIDE PGDATA: Fedora's PG has logging_collector=on
# with log_directory='log', so a file named PGDATA/log would collide with it.
runuser -u postgres -- pg_ctl -D "$PGDATA" -o "-p $PGPORT -k $SOCK -c listen_addresses=''" -l "$DATADIR/pg-startup.log" -w start >/dev/null 2>&1 \
  || { echo "pg failed to start:"; cat "$DATADIR/pg-startup.log" 2>/dev/null; cat "$PGDATA/log/"*.log 2>/dev/null; exit 1; }
createdb -h "$SOCK" -p "$PGPORT" -U "$PGUSER" pages

export PGHOST="$SOCK" PGPORT="$PGPORT" PGUSER="$PGUSER" PGDATABASE=pages
export RAW_TOKEN_SECRET="integration-secret"
export API_TOKEN_PEPPER="integration-pepper"
export PAGE_COOKIE_SECRET="integration-page-secret"
export DASHBOARD_HOST=localhost CONTENT_HOST=content.localhost

echo "▸ migrate"
node "$ROOT/lib/migrate.js"

echo "▸ vendor flag"
FLAG_SRC=/root/flag/design-system bash "$ROOT/scripts/sync-flag.sh" >/dev/null
test -f "$ROOT/public/assets/flag/tokens/design-tokens.css" && echo "  flag tokens vendored"

echo "▸ seed"
HTML='<!doctype html><html><head><title>Omnicom</title></head><body><h1>Q2</h1><canvas id=c></canvas><script>chart()</script></body></html>'
VID_OMNICOM=$(psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -tAX -v html="$HTML" <<'SQL'
WITH p AS (INSERT INTO pages(slug,title) VALUES('omnicom','Omnicom') RETURNING id),
v AS (INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author)
      SELECT id, :'html','sha-omnicom','approved','themed','t@elcanotek.com' FROM p RETURNING id,page_id),
u AS (UPDATE pages SET published_version_id=v.id FROM v WHERE pages.id=v.page_id RETURNING pages.id)
SELECT v.id FROM v;
SQL
)
VID_DOWN=$(psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -tAX <<'SQL'
WITH p AS (INSERT INTO pages(slug,title,disabled) VALUES('down','Down',true) RETURNING id),
v AS (INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author)
      SELECT id,'<html><body>down</body></html>','sha-down','approved','themed','t@elcanotek.com' FROM p RETURNING id,page_id),
u AS (UPDATE pages SET published_version_id=v.id FROM v WHERE pages.id=v.page_id RETURNING pages.id)
SELECT v.id FROM v;
SQL
)
echo "  omnicom version=$VID_OMNICOM  down version=$VID_DOWN"

# Prove the content-immutability trigger actually blocks tampering.
echo "▸ append-only trigger"
if psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -tAX \
     -c "UPDATE page_versions SET html='hacked' WHERE id=$VID_OMNICOM" >/dev/null 2>&1; then
  echo "  ✗ trigger did NOT block content mutation"; exit 1
else
  echo "  ✓ content mutation blocked by trigger"
fi

echo "▸ render integration"
VID_OMNICOM="$VID_OMNICOM" VID_DOWN="$VID_DOWN" node "$ROOT/test/integration.js"

echo "▸ api integration"
node "$ROOT/test/api.integration.js"

echo "▸ mcp integration"
node "$ROOT/test/mcp.integration.js"

echo "▸ admin integration"
node "$ROOT/test/admin.integration.js"

echo "▸ view integration"
node "$ROOT/test/view.integration.js"

echo "✓ integration passed"
