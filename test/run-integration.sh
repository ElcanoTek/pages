#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# test/run-integration.sh — spin a throwaway Postgres, migrate, vendor Flag,
# seed, and run the /raw render integration check. Self-contained; cleans up.
#
# Works in two environments:
#   - Linux as root (prod/CI boxes): the cluster runs as the postgres system
#     user via runuser, since Postgres refuses to run as root.
#   - Anything else (macOS with Homebrew Postgres tools, unprivileged Linux):
#     the cluster runs directly as the current user.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PGPORT=5497
PGUSER=pages
SOCK=/tmp
DATADIR="$(mktemp -d /tmp/pages-pgtest.XXXXXX)"
PGDATA="$DATADIR/data"

# initdb/pg_ctl often aren't on PATH even when the server package is installed
# (Debian/Ubuntu keep them in /usr/lib/postgresql/<ver>/bin, Homebrew in the
# postgresql@<ver> keg). Find them if needed.
if ! command -v initdb >/dev/null 2>&1; then
  for d in /usr/lib/postgresql/*/bin /opt/homebrew/opt/postgresql@*/bin /usr/local/opt/postgresql@*/bin; do
    [ -x "$d/initdb" ] && PATH="$d:$PATH"
  done
  export PATH
fi
command -v initdb >/dev/null 2>&1 \
  || { echo "initdb not found — install PostgreSQL (server tools) first" >&2; exit 1; }

# Root can't run Postgres directly; delegate to the postgres system user.
if [ "$(id -u)" = "0" ] && command -v runuser >/dev/null 2>&1 && id postgres >/dev/null 2>&1; then
  as_pg() { runuser -u postgres -- "$@"; }
  chown -R postgres:postgres "$DATADIR"
else
  as_pg() { "$@"; }
fi

cleanup() {
  as_pg pg_ctl -D "$PGDATA" stop -m immediate >/dev/null 2>&1 || true
  rm -rf "$DATADIR"
}
trap cleanup EXIT

as_pg initdb -D "$PGDATA" -A trust -U "$PGUSER" >/dev/null
# Use a startup-log path OUTSIDE PGDATA: Fedora's PG has logging_collector=on
# with log_directory='log', so a file named PGDATA/log would collide with it.
as_pg pg_ctl -D "$PGDATA" -o "-p $PGPORT -k $SOCK -c listen_addresses=''" -l "$DATADIR/pg-startup.log" -w start >/dev/null 2>&1 \
  || { echo "pg failed to start:"; cat "$DATADIR/pg-startup.log" 2>/dev/null; cat "$PGDATA/log/"*.log 2>/dev/null; exit 1; }
createdb -h "$SOCK" -p "$PGPORT" -U "$PGUSER" pages

export PGHOST="$SOCK" PGPORT="$PGPORT" PGUSER="$PGUSER" PGDATABASE=pages
export RAW_TOKEN_SECRET="integration-secret"
export API_TOKEN_PEPPER="integration-pepper"
export PAGE_COOKIE_SECRET="integration-page-secret"
export DASHBOARD_HOST=localhost CONTENT_HOST=content.localhost
# Short lock bound so the bounded-wait test (api.integration step 19) fails
# fast; no legitimate test transaction holds a page lock anywhere near this.
export PG_LOCK_TIMEOUT_MS=2000
# The MCP suite intentionally exercises a complete per-tool authorization
# denial matrix in addition to protocol/interoperability calls.
export RL_MCP_PER_MIN=500
# Likewise the admin suite: it walks the whole governance surface (versions,
# workspaces, portals, templates, a 35-page switcher) from one loopback address
# in a few seconds, which the default 120/min per-IP ceiling is not meant to
# allow. Without this, adding a test to that suite silently turns an unrelated
# later assertion into a rate-limited response. lib/ratelimit.js's own limits are
# proven by test/ratelimit.integration.js, which sets them in-process.
export RL_API_PER_MIN=600

echo "▸ migrate"
node "$ROOT/lib/migrate.js"

test -f "$ROOT/public/assets/flag/tokens/design-tokens.css" && echo "  flag tokens present" \
  || { echo "  flag tokens missing — public/assets/flag/ not committed?" >&2; exit 1; }

echo "▸ seed"
HTML='<!doctype html><html><head><title>Northwind</title></head><body><h1>Q2</h1><canvas id=c></canvas><script>chart()</script></body></html>'
VID_NORTHWIND=$(psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -tAX -v html="$HTML" <<'SQL'
WITH p AS (INSERT INTO pages(slug,title) VALUES('northwind','Northwind') RETURNING id),
v AS (INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author)
      SELECT id, :'html','sha-northwind','approved','themed','t@elcanotek.com' FROM p RETURNING id,page_id),
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
# An UNPUBLISHED draft row: the pages_pubver_fk RESTRICT only protects the
# published row, so this one proves the DELETE-blocking trigger on its own.
VID_DRAFT=$(psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -tAX <<SQL
WITH v AS (
  INSERT INTO page_versions (page_id, html, content_sha256, status, render_mode, author)
  SELECT page_id, '<html><body>draft</body></html>', 'sha-draft', 'draft', 'themed', 't@elcanotek.com'
    FROM page_versions WHERE id = $VID_NORTHWIND
  RETURNING id
)
SELECT id FROM v;
SQL
)
echo "  northwind version=$VID_NORTHWIND  down version=$VID_DOWN  draft version=$VID_DRAFT"

# Prove the append-only triggers actually block tampering: content UPDATEs
# and ANY DELETE (both halves of PLAN §5, and CONTRIBUTING.md invariant #4).
echo "▸ append-only trigger"
if psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -tAX \
     -c "UPDATE page_versions SET html='hacked' WHERE id=$VID_NORTHWIND" >/dev/null 2>&1; then
  echo "  ✗ trigger did NOT block content mutation"; exit 1
else
  echo "  ✓ content mutation blocked by trigger"
fi
if psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -tAX \
     -c "DELETE FROM page_versions WHERE id=$VID_DRAFT" >/dev/null 2>&1; then
  echo "  ✗ trigger did NOT block version DELETE"; exit 1
else
  echo "  ✓ version DELETE blocked by trigger"
fi

# migrations/018 is the only check that can see a page predating the `portal`
# reservation, and a guard that never fires is indistinguishable from one that
# cannot. Drive it through the REAL runner (not a copy of the SQL) so the operator
# message — which carries the only remedy, and which a bare err.message would drop
# — is proven too.
echo "▸ portal route guard (migration 018)"
sql() { psql -h "$SOCK" -p "$PGPORT" -U "$PGUSER" -d pages -qtAX -c "$1" >/dev/null; }
replay_018() { sql "DELETE FROM schema_migrations WHERE filename='018_reserve_portal_slug.sql'"; }

sql "INSERT INTO pages (slug,title) VALUES ('myportal/q2','Lookalike')"
replay_018
node "$ROOT/lib/migrate.js" >/dev/null 2>&1 \
  && echo "  ✓ a lookalike slug does not trip the guard" \
  || { echo "  ✗ guard fired on a slug with no 'portal' segment"; exit 1; }

sql "INSERT INTO pages (slug,title) VALUES ('portal/seized','Seized')"
replay_018
if out=$(node "$ROOT/lib/migrate.js" 2>&1); then
  echo "  ✗ guard did NOT fire on a live page holding /portal/*"; exit 1
fi
echo "$out" | grep -q "portal/seized" \
  || { echo "  ✗ the failure does not name the colliding page: $out"; exit 1; }
echo "$out" | grep -q "slug-rename" \
  || { echo "  ✗ the operator is not told how to fix it (HINT dropped): $out"; exit 1; }
echo "  ✓ a live page holding /portal/* blocks the migration, named, with the remedy"

sql "UPDATE pages SET deleted_at=now() WHERE slug='portal/seized'"
node "$ROOT/lib/migrate.js" >/dev/null 2>&1 \
  && echo "  ✓ a soft-deleted row serves nothing, so it does not block" \
  || { echo "  ✗ guard fired on a soft-deleted row"; exit 1; }
sql "DELETE FROM pages WHERE slug IN ('portal/seized','myportal/q2')"

echo "▸ render integration"
VID_NORTHWIND="$VID_NORTHWIND" VID_DOWN="$VID_DOWN" node "$ROOT/test/integration.js"

echo "▸ api integration"
node "$ROOT/test/api.integration.js"

echo "▸ ratelimit integration"
node "$ROOT/test/ratelimit.integration.js"

echo "▸ mcp integration"
node "$ROOT/test/mcp.integration.js"

echo "▸ admin integration"
node "$ROOT/test/admin.integration.js"

echo "▸ view integration"
node "$ROOT/test/view.integration.js"

echo "▸ template CLI integration"
node "$ROOT/test/template-cli.integration.js"

echo "✓ integration passed"
