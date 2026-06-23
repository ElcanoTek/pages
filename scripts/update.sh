#!/usr/bin/env bash
# scripts/update.sh — in-place upgrade for Elcano Pages.
#
# Build in a staging dir, then swap into place and restart. If anything fails
# before the swap, the live service keeps running the old code untouched.
# Invoked via `pages update` (which runs this as root). Preserves
# /etc/default/pages and /opt/pages/assets across updates.
#
#   PAGES_UPDATE_YES=1     skip the confirmation prompt
#   PAGES_UPDATE_NO_PULL=1 rebuild current checkout without fetching (rollback)

set -euo pipefail

APP_DIR="${APP_DIR:-/opt/pages}"
APP_USER="${APP_USER:-pages}"
SRC_DIR="${PAGES_SRC_DIR:-/opt/pages-src}"
SERVICE="pages.service"
PORT="${PAGES_PORT:-3002}"

c_dim=$'\033[2m'; c_green=$'\033[0;32m'; c_red=$'\033[0;31m'; c_bold=$'\033[1m'; c_reset=$'\033[0m'
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "run as root (use: pages update)"
[[ -d "$SRC_DIR/.git" ]] || die "no source checkout at $SRC_DIR"

git config --global --add safe.directory "$SRC_DIR" 2>/dev/null || true

if [[ "${PAGES_UPDATE_NO_PULL:-0}" != "1" ]]; then
  step "Fetching latest"
  before_sha="$(git -C "$SRC_DIR" rev-parse HEAD)"
  git -C "$SRC_DIR" fetch --quiet origin
  branch="$(git -C "$SRC_DIR" rev-parse --abbrev-ref HEAD)"
  [[ "$branch" == "HEAD" ]] && branch="main"
  git -C "$SRC_DIR" merge --ff-only "origin/$branch" || die "fast-forward failed (local commits?). Resolve in $SRC_DIR."
  after_sha="$(git -C "$SRC_DIR" rev-parse HEAD)"
  if [[ "$before_sha" == "$after_sha" ]]; then ok "already up to date ($after_sha)"; else ok "updated $before_sha → $after_sha"; fi
  # If update.sh itself changed, re-exec the new version.
  if ! git -C "$SRC_DIR" diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh 2>/dev/null; then
    exec env PAGES_UPDATE_NO_PULL=1 PAGES_UPDATE_YES="${PAGES_UPDATE_YES:-0}" bash "$SRC_DIR/scripts/update.sh"
  fi
fi

if [[ "${PAGES_UPDATE_YES:-0}" != "1" && -t 0 ]]; then
  read -rp "Proceed with rebuild + restart? [y/N] " a; [[ "${a,,}" =~ ^(y|yes)$ ]] || die "aborted"
fi

step "Building in staging"
STAGING="$(mktemp -d)"
trap 'rm -rf "$STAGING"' EXIT
rsync -a --delete \
  --exclude='/.git' --exclude='/node_modules' --exclude='/assets' --exclude='/.env' \
  "$SRC_DIR/" "$STAGING/"
# Reuse live node_modules to speed up the install, then reconcile with npm ci.
cp -a "$APP_DIR/node_modules" "$STAGING/" 2>/dev/null || true
chown -R "$APP_USER:$APP_USER" "$STAGING"
runuser -u "$APP_USER" -- bash -c "cd '$STAGING' && npm ci --omit=dev --no-audit --no-fund --loglevel=warn" \
  || die "npm ci failed — live service untouched"
# Syntax-check the entrypoint before we swap.
runuser -u "$APP_USER" -- node --check "$STAGING/server.js" || die "server.js failed syntax check"
# Run pending DB migrations before the swap. They're idempotent (tracked in
# schema_migrations) and additive, so the still-running old code is unaffected.
ENV_FILE="/etc/default/pages"
if [[ -f "$ENV_FILE" ]] && grep -q '^DATABASE_URL=' "$ENV_FILE"; then
  DB_URL="$(. "$ENV_FILE" >/dev/null 2>&1; printf '%s' "$DATABASE_URL")"
  runuser -u "$APP_USER" -- bash -c "cd '$STAGING' && DATABASE_URL='$DB_URL' node lib/migrate.js" \
    || die "migrations failed — live service untouched"
  ok "migrations applied"
else
  warn "no DATABASE_URL in $ENV_FILE — skipping migrations"
fi
ok "staging build ok"

step "Swapping into place + restarting"
systemctl stop "$SERVICE" || true
rsync -a --delete \
  --exclude='/.git' --exclude='/assets' --exclude='/.env' \
  "$STAGING/" "$APP_DIR/"
chown -R "$APP_USER:$APP_USER" "$APP_DIR"
install -m 0644 "$APP_DIR/deploy/pages.service" /etc/systemd/system/
install -m 0755 "$APP_DIR/deploy/pages-cli" /usr/local/bin/pages
systemctl daemon-reload
systemctl start "$SERVICE"

step "Health check"
healthy=0
for _ in $(seq 1 10); do
  if curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then healthy=1; break; fi
  sleep 1
done
[[ "$healthy" == "1" ]] && ok "pages healthy on :$PORT" || die "pages did not become healthy — check: pages logs"

printf '\n%s✓ update complete%s  %s(rollback: cd %s && git checkout <SHA> && pages rebuild)%s\n' \
  "$c_green" "$c_reset" "$c_dim" "$SRC_DIR" "$c_reset"
