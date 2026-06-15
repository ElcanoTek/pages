#!/usr/bin/env bash
# scripts/bootstrap.sh — interactive installer for Elcano Pages (Phase 0).
#
# What this does today (Phase 0 — stand up the two-vhost skeleton):
#   1. Installs Node 20+ (dnf, or NodeSource if Fedora's too old)
#   2. Creates the 'pages' system user + /opt/pages
#   3. Syncs source to /opt/pages-src and /opt/pages; npm ci (production)
#   4. Writes /etc/default/pages (AUTH_SIGNING_PUBKEY, the two hostnames,
#      generated PAGE_COOKIE_SECRET / RAW_TOKEN_SECRET / API_TOKEN_PEPPER)
#   5. Installs systemd unit + /usr/local/bin/pages CLI; enables + starts
#   6. Caddy + auto-TLS for BOTH hostnames; health-checks /healthz
#
# Phase 1 will extend this with PostgreSQL setup + migrations + Flag vendoring.
#
# Usage:   sudo bash scripts/bootstrap.sh
# Scripted: PAGES_BOOTSTRAP_NON_INTERACTIVE=1 + AUTH_SIGNING_PUBKEY +
#           PAGES_BOOTSTRAP_DASHBOARD_HOST + PAGES_BOOTSTRAP_CONTENT_HOST

set -euo pipefail
if [[ ! -t 0 && -t 1 ]]; then exec </dev/tty; fi

APP_DIR="${APP_DIR:-/opt/pages}"
APP_USER="${APP_USER:-pages}"
SRC_DIR="${SRC_DIR:-$(cd "$(dirname "$0")/.." && pwd)}"
INSTALL_SRC_DIR="${PAGES_SRC_DIR:-/opt/pages-src}"
ENV_FILE="/etc/default/pages"
CLI_TARGET="/usr/local/bin/pages"
PORT="${PAGES_PORT:-3002}"

if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
  c_reset=$'\033[0m' c_dim=$'\033[2m' c_red=$'\033[0;31m'
  c_green=$'\033[0;32m' c_yellow=$'\033[0;33m' c_cyan=$'\033[0;36m' c_bold=$'\033[1m'
else
  c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_cyan='' c_bold=''
fi
say()  { printf '%s\n' "$*"; }
info() { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
step() { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
ok()   { printf '%s✓ %s%s\n' "$c_green" "$*" "$c_reset"; }
warn() { printf '%s! %s%s\n' "$c_yellow" "$*" "$c_reset" >&2; }
die()  { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

NON_INTERACTIVE="${PAGES_BOOTSTRAP_NON_INTERACTIVE:-0}"
ask()  { printf '%s?%s %s ' "$c_cyan" "$c_reset" "$*" >&2; }
prompt() {
  local varname="$1" label="$2" default="${3:-}" answer=""
  if [[ -n "${!varname:-}" ]]; then printf '%s' "${!varname}"; return; fi
  if [[ "$NON_INTERACTIVE" == "1" ]]; then
    [[ -n "$default" ]] || die "non-interactive + missing: set $varname"
    printf '%s' "$default"; return
  fi
  if [[ -n "$default" ]]; then ask "$label ${c_dim}[$default]${c_reset}:"; else ask "$label:"; fi
  read -r answer
  [[ -z "$answer" ]] && answer="$default"
  printf '%s' "$answer"
}

[[ $EUID -eq 0 ]] || die "run as root: sudo bash scripts/bootstrap.sh"
[[ -f "$SRC_DIR/server.js" ]] || die "not an Elcano Pages checkout at $SRC_DIR"

cat <<EOF
${c_bold}Elcano Pages — bootstrap (Phase 0)${c_reset}
${c_dim}Fedora / RHEL 9+  •  systemd  •  Node 20+  •  two-vhost skeleton${c_reset}

Safe to re-run: existing env is preserved; only missing values are prompted.
EOF

step "1/6  Installing Node + system dependencies"
dnf install -y git curl jq rsync openssl >/dev/null
if ! command -v node >/dev/null 2>&1 || [[ "$(node -v | cut -dv -f2 | cut -d. -f1)" -lt 20 ]]; then
  info "installing Node 20 via NodeSource"
  curl -fsSL https://rpm.nodesource.com/setup_20.x | bash - >/dev/null
  dnf install -y nodejs >/dev/null
fi
ok "node $(node -v)"

step "2/6  Preparing $APP_DIR + '$APP_USER' user"
if ! id -u "$APP_USER" >/dev/null 2>&1; then
  useradd --system --home-dir "$APP_DIR" --shell /usr/sbin/nologin "$APP_USER"
fi
mkdir -p "$APP_DIR" "$APP_DIR/assets"
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

if [[ ! -d "$INSTALL_SRC_DIR/.git" ]]; then
  [[ -d "$SRC_DIR/.git" ]] || die "bootstrap must run from a git checkout (no .git at $SRC_DIR)"
  mkdir -p "$INSTALL_SRC_DIR"
  rsync -a --exclude='/node_modules' "$SRC_DIR/" "$INSTALL_SRC_DIR/"
fi
rsync -a --delete \
  --exclude='/.git' --exclude='/node_modules' --exclude='/assets' --exclude='/.env' \
  "$INSTALL_SRC_DIR/" "$APP_DIR/"
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

step "3/6  npm ci (production)"
runuser -u "$APP_USER" -- bash -c "cd '$APP_DIR' && npm ci --omit=dev --no-audit --no-fund --loglevel=warn" \
  || die "npm ci failed"
ok "node_modules installed"

step "4/6  Configuring the instance"
if [[ -f "$ENV_FILE" ]]; then
  info "found existing $ENV_FILE — re-using values"
  # shellcheck disable=SC1090
  set -a; . "$ENV_FILE"; set +a
fi

AUTH_SIGNING_PUBKEY="$(prompt AUTH_SIGNING_PUBKEY "Auth service public signing key (AUTH_SIGNING_PUBKEY from the auth host)" "${AUTH_SIGNING_PUBKEY:-}")"
_pub_bytes="$(printf '%s' "$AUTH_SIGNING_PUBKEY" | base64 -d 2>/dev/null | wc -c | tr -d ' ')"
if [[ -z "$AUTH_SIGNING_PUBKEY" ]]; then
  die "AUTH_SIGNING_PUBKEY is required — copy it from the auth host. Without it Pages rejects every elcano_auth cookie and bounces all admin requests to login."
elif [[ "$_pub_bytes" != "32" ]]; then
  die "AUTH_SIGNING_PUBKEY must be a base64-encoded 32-byte Ed25519 public key (decoded to ${_pub_bytes:-0} bytes)."
fi
unset _pub_bytes

# Defaults are Elcano's deployment; an operator deploying for someone else just
# answers the prompts (or sets PAGES_BOOTSTRAP_*_HOST). Nothing host-specific is
# baked into the app — these two values flow into /etc/default/pages.
DASHBOARD_HOST="$(prompt PAGES_BOOTSTRAP_DASHBOARD_HOST "Dashboard hostname (trusted auth zone)" "${DASHBOARD_HOST:-pages.elcanotek.com}")"
CONTENT_HOST="$(prompt PAGES_BOOTSTRAP_CONTENT_HOST "Content hostname (SEPARATE registrable domain, cookieless)" "${CONTENT_HOST:-elcano-pages.com}")"
[[ -n "$CONTENT_HOST" ]] || die "CONTENT_HOST is required — it MUST be a different registrable domain from the dashboard (PLAN.md §7)."
if [[ "$CONTENT_HOST" == *".${DASHBOARD_HOST#*.}" || "${CONTENT_HOST#*.}" == "${DASHBOARD_HOST#*.}" ]]; then
  warn "CONTENT_HOST ($CONTENT_HOST) looks like it shares a registrable domain with DASHBOARD_HOST ($DASHBOARD_HOST)."
  warn "That defeats the isolation model (cookie-tossing). Use a different eTLD+1. Continuing, but fix before production."
fi

# Generate per-instance secrets once; preserve across re-runs.
PAGE_COOKIE_SECRET="${PAGE_COOKIE_SECRET:-$(openssl rand -hex 32)}"
RAW_TOKEN_SECRET="${RAW_TOKEN_SECRET:-$(openssl rand -hex 32)}"
API_TOKEN_PEPPER="${API_TOKEN_PEPPER:-$(openssl rand -hex 32)}"

umask 077
cat > "$ENV_FILE" <<EOF
# Auto-generated by Elcano Pages bootstrap.sh on $(date -Iseconds)
PORT="$PORT"
DASHBOARD_HOST="$DASHBOARD_HOST"
CONTENT_HOST="$CONTENT_HOST"
DASHBOARD_ORIGIN="https://$DASHBOARD_HOST"
CONTENT_ORIGIN="https://$CONTENT_HOST"
AUTH_SIGNING_PUBKEY="$AUTH_SIGNING_PUBKEY"
AUTH_COOKIE_NAME="${AUTH_COOKIE_NAME:-elcano_auth}"
AUTH_LOGIN_URL="${AUTH_LOGIN_URL:-https://auth.elcanotek.com}"
ADMIN_EMAIL_DOMAIN="${ADMIN_EMAIL_DOMAIN:-elcanotek.com}"
PAGE_COOKIE_SECRET="$PAGE_COOKIE_SECRET"
RAW_TOKEN_SECRET="$RAW_TOKEN_SECRET"
API_TOKEN_PEPPER="$API_TOKEN_PEPPER"
# DATABASE_URL set in Phase 1 (PostgreSQL).
EOF
chmod 0640 "$ENV_FILE"
ok "env seeded at $ENV_FILE"

step "5/6  Installing systemd unit + CLI + starting"
install -m 0644 "$APP_DIR/deploy/pages.service" /etc/systemd/system/
install -m 0755 "$APP_DIR/deploy/pages-cli" "$CLI_TARGET"
systemctl daemon-reload
systemctl enable pages.service >/dev/null 2>&1 || true
systemctl restart pages.service

healthy=0
for _ in $(seq 1 15); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz" 2>/dev/null || echo 000)
  if [[ "$code" == "200" ]]; then healthy=1; break; fi
  sleep 1
done
if [[ "$healthy" == "1" ]]; then ok "/healthz → 200 (server up)"
else warn "pages didn't answer /healthz in 15s — check: pages logs"; fi

step "6/6  Reverse proxy / TLS (both hostnames)"
SETUP_CADDY_ANS="$(prompt PAGES_BOOTSTRAP_SETUP_CADDY "Set up Caddy + auto-TLS for $DASHBOARD_HOST and $CONTENT_HOST? (Y/n)" Y)"
if [[ "${SETUP_CADDY_ANS,,}" =~ ^(y|yes)$ ]]; then
  LE_ANS="$(prompt PAGES_BOOTSTRAP_USE_LETSENCRYPT "Use Let's Encrypt (needs public 80/443 + DNS for both names)? (Y/n)" Y)"
  USE_LE="n"; [[ "${LE_ANS,,}" =~ ^(y|yes)$ ]] && USE_LE="y"
  LE_EMAIL=""
  [[ "$USE_LE" == "y" ]] && LE_EMAIL="$(prompt PAGES_BOOTSTRAP_LE_EMAIL "LE contact email (blank to skip)" "${PAGES_BOOTSTRAP_LE_EMAIL:-}")"

  dnf install -y caddy >/dev/null
  install -d /etc/caddy/conf.d
  if [[ ! -f /etc/caddy/Caddyfile ]] || ! grep -qE '^[[:space:]]*import[[:space:]]+conf\.d/' /etc/caddy/Caddyfile; then
    { echo ""; echo "# Managed by Elcano service bootstraps"; echo "import conf.d/*.caddy"; } >> /etc/caddy/Caddyfile
  fi
  if [[ -n "$LE_EMAIL" ]] && ! grep -qE '^[[:space:]]*email[[:space:]]' /etc/caddy/Caddyfile; then
    tmp=$(mktemp); printf '{\n\temail %s\n}\n\n' "$LE_EMAIL" > "$tmp"
    cat /etc/caddy/Caddyfile >> "$tmp"; install -m 0644 "$tmp" /etc/caddy/Caddyfile; rm -f "$tmp"
  fi

  tmp=$(mktemp)
  sed -e "s/{{DASHBOARD_HOST}}/$DASHBOARD_HOST/g" -e "s/{{CONTENT_HOST}}/$CONTENT_HOST/g" \
    "$APP_DIR/deploy/pages.caddy" > "$tmp"
  if [[ "$USE_LE" != "y" ]]; then
    awk '/^[^[:space:]{}#].*\{$/ { print; print "\ttls internal"; next } { print }' "$tmp" > "$tmp.2" && mv "$tmp.2" "$tmp"
  fi
  install -m 0644 "$tmp" /etc/caddy/conf.d/pages.caddy; rm -f "$tmp"

  if systemctl is-active --quiet firewalld 2>/dev/null; then
    firewall-cmd --add-service=http --permanent >/dev/null 2>&1 || true
    firewall-cmd --add-service=https --permanent >/dev/null 2>&1 || true
    firewall-cmd --reload >/dev/null 2>&1 || true
    ok "firewalld: http + https opened"
  fi

  systemctl enable caddy >/dev/null 2>&1 || true
  if systemctl is-active --quiet caddy; then systemctl reload caddy || die "caddy reload failed"
  else systemctl start caddy || die "caddy failed to start"; fi
  ok "Caddy running for both hostnames (auto-renew, no cron needed)"
else
  info "skipping Caddy — reach pages at http://127.0.0.1:$PORT (front with your own proxy)"
fi

say
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
printf '%s ✓ Elcano Pages installed (Phase 0)%s\n' "$c_bold" "$c_reset"
printf '%s═══════════════════════════════════════════════%s\n' "$c_green" "$c_reset"
say
say "  Dashboard   ${c_bold}https://$DASHBOARD_HOST${c_reset}   ${c_dim}(staff: /admin/<slug>, clients: /view/<slug>)${c_reset}"
say "  Content     ${c_bold}https://$CONTENT_HOST${c_reset}   ${c_dim}(cookieless render zone)${c_reset}"
say "  CLI         ${c_dim}pages start|stop|restart|status|update|env|tls${c_reset}"
say "  Logs        ${c_dim}pages logs${c_reset}"
say
say "  ${c_dim}Login is owned by ${AUTH_LOGIN_URL:-https://auth.elcanotek.com}; @${ADMIN_EMAIL_DOMAIN:-elcanotek.com} auto-admins.${c_reset}"
say "  ${c_dim}Next: Phase 1 — Postgres + Flag theming + the /view & /admin UI.${c_reset}"
say
