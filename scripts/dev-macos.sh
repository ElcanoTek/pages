#!/usr/bin/env bash
# scripts/dev-macos.sh — one-command local dev for Pages on macOS.
#
# The macOS counterpart to scripts/dev.sh (which is Linux-only: it uses
# `runuser`, a `postgres` system user, and initdb-as-root). On a Mac you
# already have a Homebrew Postgres running as your own user, so this script
# just creates the `pages` database in it, migrates, vendors Flag, mints an
# agent token, and boots the server with both vhosts pointed at localhost.
#
# Secrets + the saved token live in .devdata/ (in-repo, gitignored) and are
# SHARED with dev.sh so token hashes stay valid across either entrypoint.
#
#   bash scripts/dev-macos.sh           # boot everything, server in foreground
#   bash scripts/dev-macos.sh token     # print the saved agent token and exit
#
# Requires: Homebrew postgresql@16 running (`brew services start postgresql@16`),
# node, and a sibling ../flag/design-system (sync-flag resolves it automatically).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEV="$ROOT/.devdata"                       # secrets + saved token (in-repo, gitignored)
ENVFILE="$DEV/dev.env"
TOKENFILE="$DEV/agent-token"

PGHOST="${PGHOST:-127.0.0.1}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-$(whoami)}"              # Homebrew PG superuser = your macOS user
PGDB="${PGDB:-pages}"
PORT="${PORT:-3099}"

mkdir -p "$DEV"

# Stable dev secrets — generated once, reused (the pepper must not change or it
# invalidates every issued token). Shared with dev.sh. NOT for production.
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

export DATABASE_URL="${DATABASE_URL:-postgres://$PGUSER@$PGHOST:$PGPORT/$PGDB?sslmode=disable}"

# `token` subcommand: print the saved agent token and exit.
if [ "${1:-}" = "token" ]; then
  if [ -s "$TOKENFILE" ]; then cat "$TOKENFILE"; exit 0; fi
  echo "no token yet — run 'bash scripts/dev-macos.sh' once to create one" >&2
  exit 1
fi

export DASHBOARD_HOST=localhost CONTENT_HOST=content.localhost CONTENT_HOST_ALSO=content.localhost
export CONTENT_ORIGIN="http://content.localhost:${PORT}" DASHBOARD_ORIGIN="http://localhost:${PORT}"
export PORT

# Local dev admin auth: no real auth service locally, so mint a throwaway
# Ed25519 keypair + admin cookie (persisted in .devdata) and enable the gated
# dev-login route so /admin works in a browser. NEVER set PAGES_DEV_LOGIN in prod.
eval "$(node "$ROOT/scripts/dev-auth.js" "$DEV")"
export AUTH_SIGNING_PUBKEY DEV_ADMIN_COOKIE DEV_ADMIN_EMAIL
export PAGES_DEV_LOGIN=1
export AUTH_LOGIN_URL="http://localhost:${PORT}/__dev/login"

# DEV-only "compose a page with Cutlass" panel: enable it if the cutlass binary
# is built next door. pages spawns it; cutlass reads its own .env (OpenRouter key
# + PAGES_MCP_*). NEVER set PAGES_COMPOSE=1 in production.
export CUTLASS_DIR="${CUTLASS_DIR:-$(cd "$ROOT/../cutlass" 2>/dev/null && pwd)}"
export CUTLASS_BIN="${CUTLASS_BIN:-$CUTLASS_DIR/bin/cutlass}"
if [ -x "$CUTLASS_BIN" ]; then
  export PAGES_COMPOSE=1
else
  echo "▸ compose panel OFF (no cutlass binary at $CUTLASS_BIN — run 'make build' in cutlass to enable)"
fi

# 0. Postgres reachable?
if ! psql "$DATABASE_URL" -tAc "SELECT 1" >/dev/null 2>&1; then
  # DB may simply not exist yet — try to create it (server itself must be up).
  if psql "postgres://$PGUSER@$PGHOST:$PGPORT/postgres" -tAc "SELECT 1" >/dev/null 2>&1; then
    echo "▸ createdb $PGDB"
    createdb -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" "$PGDB"
  else
    echo "✗ cannot reach Postgres at $PGHOST:$PGPORT as '$PGUSER'." >&2
    echo "  Start it with:  brew services start postgresql@16" >&2
    exit 1
  fi
fi

# 1. Migrate + vendor Flag.
echo "▸ migrate"
node "$ROOT/lib/migrate.js"
echo "▸ vendor flag"
bash "$ROOT/scripts/sync-flag.sh" >/dev/null

# 2. Ensure an agent token exists; save the raw value (only shown at creation).
if [ ! -s "$TOKENFILE" ]; then
  echo "▸ minting agent token 'dev-agent'"
  node "$ROOT/scripts/token.js" add dev-agent deploy \
    | grep -oE 'pgs_[A-Za-z0-9_-]{20,}' > "$TOKENFILE"
  chmod 600 "$TOKENFILE"
fi

cat <<EOF

  Pages dev environment ready (macOS).
    dashboard host : http://localhost:$PORT       (Host: localhost)
    content host   : http://content.localhost:$PORT
    database       : $DATABASE_URL
    agent token    : $(cat "$TOKENFILE")
                     (also saved in .devdata/agent-token)

  MCP endpoint for cutlass:
    PAGES_MCP_URL=http://127.0.0.1:$PORT/mcp
    PAGES_MCP_TOKEN=$(cat "$TOKENFILE")

  Admin GUI (local dev-login, then pick any page slug):
    http://localhost:$PORT/__dev/login?next=/admin/<slug>

  Ctrl-C stops the server. The Homebrew Postgres keeps running; data persists.

EOF

echo "▸ starting server (foreground)"
exec node "$ROOT/server.js"
