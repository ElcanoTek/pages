#!/usr/bin/env bash
# scripts/dev.sh — one-command local dev environment for Pages.
#
# Stands up a self-contained Postgres cluster (trust auth, loopback socket
# only), runs migrations, vendors Flag, ensures an agent API token exists, and
# boots the server with both vhosts pointed at localhost so you can drive the
# REST API + /raw render loop locally — the "let Claude Code deploy pages" loop
# (HANDOFF.md). Idempotent: re-running reuses the cluster, secrets, and token.
#
# Secrets + the saved token live in .devdata/ (in-repo, gitignored). The
# Postgres cluster + socket live under /var/tmp/pages-dev because the `postgres`
# system user (which must own/run the cluster) cannot traverse a mode-700 /root.
#
#   sudo bash scripts/dev.sh           # boot everything, run server in foreground
#   sudo bash scripts/dev.sh token     # print the saved agent token and exit
#
# Then, from another shell (the saved token is in .devdata/agent-token):
#   TOKEN=$(cat .devdata/agent-token)
#   curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" \
#        -H 'Content-Type: application/json' \
#        -d '{"slug":"demo","title":"Demo"}' http://127.0.0.1:3099/api/v1/pages
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEV="$ROOT/.devdata"                       # secrets + saved token (in-repo, gitignored)
PGROOT="${PAGES_DEV_DIR:-/var/tmp/pages-dev}"  # cluster + socket (postgres-accessible)
PGDATA="$PGROOT/pg"
SOCK="$PGROOT"
PGPORT="${PGPORT:-5499}"
PORT="${PORT:-3099}"
ENVFILE="$DEV/dev.env"
TOKENFILE="$DEV/agent-token"

mkdir -p "$DEV" "$PGROOT"

# Stable dev secrets — generated once and reused so token hashes stay valid
# across restarts (the pepper must not change). NOT for production.
if [ ! -f "$ENVFILE" ]; then
  umask 077
  {
    echo "RAW_TOKEN_SECRET=$(openssl rand -hex 32)"
    echo "API_TOKEN_PEPPER=$(openssl rand -hex 32)"
    echo "PAGE_COOKIE_SECRET=$(openssl rand -hex 32)"
  } > "$ENVFILE"
fi
# shellcheck disable=SC1090
set -a; . "$ENVFILE"; set +a

# `token` subcommand: print the saved agent token and exit (handy from scripts).
# Short-circuits before any setup so stdout carries only the token.
if [ "${1:-}" = "token" ]; then
  if [ -s "$TOKENFILE" ]; then cat "$TOKENFILE"; exit 0; fi
  echo "no token yet — run 'bash scripts/dev.sh' once to create one" >&2
  exit 1
fi

export PGHOST="$SOCK" PGPORT="$PGPORT" PGUSER=pages PGDATABASE=pages
export DASHBOARD_HOST=localhost CONTENT_HOST=content.localhost CONTENT_HOST_ALSO=content.localhost
export PORT
export FLAG_SRC="${FLAG_SRC:-/root/flag/design-system}"

pg() { runuser -u postgres -- "$@"; }

# 1. Initialize the cluster once. Postgres won't run as root → owned by postgres.
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  echo "▸ initdb (trust auth, role 'pages') → $PGDATA"
  chown -R postgres:postgres "$PGROOT"
  pg initdb -D "$PGDATA" -A trust -U pages >/dev/null
fi

# 2. Start it if not already running (socket-only, loopback).
if ! pg pg_ctl -D "$PGDATA" status >/dev/null 2>&1; then
  echo "▸ starting Postgres on socket $SOCK:$PGPORT"
  chown -R postgres:postgres "$PGROOT"
  pg pg_ctl -D "$PGDATA" -o "-p $PGPORT -k $SOCK -c listen_addresses=''" \
    -l "$PGROOT/pg.log" -w start >/dev/null
fi

# 3. Create the database if missing.
if ! psql -h "$SOCK" -p "$PGPORT" -U pages -lqt | cut -d'|' -f1 | grep -qw pages; then
  echo "▸ createdb pages"
  createdb -h "$SOCK" -p "$PGPORT" -U pages pages
fi

# 4. Migrate + vendor Flag.
echo "▸ migrate"
node "$ROOT/lib/migrate.js"
echo "▸ vendor flag (from $FLAG_SRC)"
bash "$ROOT/scripts/sync-flag.sh" >/dev/null

# 5. Ensure an agent token exists; save the raw value (only shown at creation).
if [ ! -s "$TOKENFILE" ]; then
  echo "▸ minting agent token 'dev-agent'"
  node "$ROOT/scripts/token.js" add dev-agent deploy \
    | grep -oE 'pgs_[A-Za-z0-9_-]{20,}' > "$TOKENFILE"
  chmod 600 "$TOKENFILE"
fi

cat <<EOF

  Pages dev environment ready.
    dashboard host : http://127.0.0.1:$PORT      (Host: localhost)
    content host   : http://127.0.0.1:$PORT      (Host: content.localhost)
    agent token    : $(cat "$TOKENFILE")
                     (also saved in .devdata/agent-token)

  Example — deploy a page as the agent:
    TOKEN=\$(cat .devdata/agent-token)
    curl -H "Host: localhost" -H "Authorization: Bearer \$TOKEN" \\
         -H 'Content-Type: application/json' \\
         -d '{"slug":"demo","title":"Demo"}' http://127.0.0.1:$PORT/api/v1/pages

  Ctrl-C stops the server (Postgres keeps running; data persists in .devdata/).
  Stop PG with:  runuser -u postgres -- pg_ctl -D "$PGDATA" stop

EOF

echo "▸ starting server (foreground)"
exec node "$ROOT/server.js"
