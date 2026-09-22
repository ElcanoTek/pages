#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/doctor.sh — diagnose this Pages box.
#
# `pages doctor` walks every box-level prerequisite Pages depends on and
# reports PASS/WARN/FAIL with the remedy; run as root it also makes the safe
# repairs (env file ownership/mode, starting a stopped service, restarting a
# stale release). `--check` reports without touching anything and needs no
# root. What it never does: git pull, dnf upgrade, migrations or rebuilds —
# doctor makes the box readable; `pages update` lands the code.
#
# Usage:
#   sudo pages doctor             diagnose; fix what is safe (perms, start, restart)
#   pages doctor --check          diagnose only, change nothing, exit 1 on any failure
#   sudo pages doctor --no-restart  fix but leave any service restart to the caller
#   pages doctor --dry-run        print the checklist; touch nothing (no root needed)
#   pages doctor --strict         warnings also exit 1
#
# Exit codes: 0 = healthy (or everything fixed), 1 = problems remain.

pages_doctor() (
  set -euo pipefail
  PAGES_SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  # shellcheck source=scripts/install-config.sh
  . "$PAGES_SCRIPT_ROOT/scripts/install-config.sh"
  SRC_DIR="$INSTALL_SRC_DIR"
  SERVICE="pages.service"
  CADDY_SNIPPET="/etc/caddy/conf.d/pages.caddy"
  # Overridable so the tests can point the lifecycle checks at a scratch file;
  # also handy on non-Fedora dev boxes. Defaults to the OS-owned original.
  OS_RELEASE="${PAGES_OS_RELEASE:-/etc/os-release}"

  if [[ -t 1 && "${TERM:-}" != "dumb" ]]; then
    c_reset=$'\033[0m' c_dim=$'\033[2m' c_red=$'\033[0;31m'
    c_green=$'\033[0;32m' c_yellow=$'\033[0;33m' c_cyan=$'\033[0;36m' c_bold=$'\033[1m'
  else
    c_reset='' c_dim='' c_red='' c_green='' c_yellow='' c_cyan='' c_bold=''
  fi
  info()  { printf '%s» %s%s\n' "$c_dim" "$*" "$c_reset"; }
  step()  { printf '\n%s▸ %s%s\n' "$c_bold" "$*" "$c_reset"; }
  die()   { printf '%s✗ %s%s\n' "$c_red" "$*" "$c_reset" >&2; exit 1; }

  n_ok=0 n_fixed=0 n_warn=0 n_fail=0
  pass()  { printf '%s✓%s %s\n' "$c_green" "$c_reset" "$*"; n_ok=$((n_ok+1)); }
  fixed() { printf '%s↻%s %s\n' "$c_cyan" "$c_reset" "$*"; n_fixed=$((n_fixed+1)); }
  advise(){ printf '%s!%s %s\n' "$c_yellow" "$c_reset" "$*"; n_warn=$((n_warn+1)); }
  fail()  { printf '%s✗%s %s\n' "$c_red" "$c_reset" "$*"; n_fail=$((n_fail+1)); }

  CHECK_ONLY=0 NO_RESTART=0 DRY_RUN=0 STRICT=0
  for arg in "$@"; do
    case "$arg" in
      --check)      CHECK_ONLY=1 ;;
      --no-restart) NO_RESTART=1 ;;
      --dry-run)    DRY_RUN=1 ;;
      --strict)     STRICT=1 ;;
      -h|--help)
        cat <<'EOF'
pages doctor — diagnose this Pages box

USAGE
  sudo pages doctor               diagnose; fix what is safe (env perms, start, restart)
  pages doctor --check            diagnose only, change nothing, exit 1 on any failure
  sudo pages doctor --no-restart  fix but never restart the service
  pages doctor --dry-run          print the checklist; touch nothing (no root needed)
  pages doctor --strict           warnings also exit 1

Checks: Node runtime (scripts/check-node.js), the environment file (presence,
0640 root:pages, required keys — values are never printed), the
DASHBOARD_HOST/CONTENT_HOST registrable-domain split, PostgreSQL, the systemd
service (active, enabled, running the current release), /readyz, Caddy + the
TLS certificates for BOTH hostnames (validity + expiry), free disk, the OS
support window, pending dnf updates, a pending reboot, Fedora vs latest stable,
and the source checkout (clean, on main, current with origin).
Repairs are limited to: env file ownership/mode, starting a stopped service,
restarting a stale release. Doctor never pulls, upgrades or rebuilds — that
stays `pages update`.
EOF
        exit 0 ;;
      *) echo "error: unknown argument: $arg (try --help)" >&2; exit 2 ;;
    esac
  done

  # env_get KEY [FILE] — read one key without sourcing the file (it holds
  # secrets; sourcing would execute arbitrary content on a tampered box).
  # Last assignment wins, surrounding quotes stripped. Never prints a value.
  env_get() {
    local key="$1" file="${2:-$ENV_FILE}"
    [[ -r "$file" ]] || return 0
    grep -E "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2- \
      | sed -e 's/^["'\'']//' -e 's/["'\'']$//' || true
  }

  if [[ "$DRY_RUN" == 1 ]]; then
    step "pages doctor --dry-run (app=$APP_DIR, src=$SRC_DIR, service=$SERVICE)"
    info "[dry-run] Runtime: node present and passing $SRC_DIR/scripts/check-node.js (20.19+/22.12+); npm present"
    info "[dry-run] Configuration: $ENV_FILE exists, mode 0640 root:$APP_USER, with AUTH_SIGNING_PUBKEY (base64 32-byte), PAGE_COOKIE_SECRET, RAW_TOKEN_SECRET, API_TOKEN_PEPPER, DATABASE_URL, DASHBOARD_HOST and CONTENT_HOST set; content host on a DIFFERENT registrable domain than the dashboard host"
    info "[dry-run] Database: postgresql.service active; DATABASE_URL accepts a probe as $APP_USER"
    info "[dry-run] Service: $SERVICE active + enabled; the running release matches $APP_DIR (stale releases restart, unless --no-restart)"
    info "[dry-run] Readiness: /readyz → 200 on :$PORT"
    info "[dry-run] Caddy/TLS: caddy active, configuration valid, certificates for BOTH hostnames valid and more than 14 days from expiry"
    info "[dry-run] Disk: at least 1 GiB free on $APP_DIR"
    info "[dry-run] Host: OS support window, pending dnf updates, reboot-needed kernel check, Fedora vs latest stable"
    info "[dry-run] Source: checkout clean, on main, current with origin/main (never pulled — that stays pages update)"
    exit 0
  fi

  [[ -f "$ENV_FILE" || -d "$SRC_DIR/.git" || -e "$APP_DIR" ]] \
    || die "no Pages install at $APP_DIR and no checkout at $SRC_DIR (run scripts/bootstrap.sh first)"

  restarted=0

  step "Runtime"
  if command -v node >/dev/null 2>&1; then
    if node "$SRC_DIR/scripts/check-node.js" >/dev/null 2>&1; then
      pass "node $(node -v 2>/dev/null || echo unknown) passes scripts/check-node.js"
    else
      fail "node $(node -v 2>/dev/null || echo '?') is unsupported — Pages needs 20.19+/22.12+; rerun scripts/bootstrap.sh"
    fi
  else
    fail "node not on PATH — rerun scripts/bootstrap.sh"
  fi
  if command -v npm >/dev/null 2>&1; then
    pass "npm present"
  else
    fail "npm missing — rerun scripts/bootstrap.sh"
  fi

  dashboard_host=""; content_host=""
  step "Configuration ($ENV_FILE)"
  if [[ ! -f "$ENV_FILE" ]]; then
    fail "$ENV_FILE missing — run scripts/bootstrap.sh"
  else
    perms="$(stat -c '%a %U:%G' "$ENV_FILE" 2>/dev/null || echo 'unknown')"
    want="640 root:$APP_USER"
    if [[ "$perms" == "$want" ]]; then
      pass "$ENV_FILE is $perms"
    elif [[ "$CHECK_ONLY" == 1 ]]; then
      fail "$ENV_FILE is $perms — want $want (it holds every credential)"
    elif chown root:"$APP_USER" "$ENV_FILE" 2>/dev/null && chmod 0640 "$ENV_FILE" 2>/dev/null; then
      fixed "$ENV_FILE set to $want"
    else
      fail "could not set $ENV_FILE to $want — fix ownership/mode by hand"
    fi
    for key in AUTH_SIGNING_PUBKEY PAGE_COOKIE_SECRET RAW_TOKEN_SECRET API_TOKEN_PEPPER DATABASE_URL DASHBOARD_HOST CONTENT_HOST; do
      if [[ -n "$(env_get "$key")" ]]; then
        pass "$key set"
      else
        fail "$key unset in $ENV_FILE"
      fi
    done
    pub="$(env_get AUTH_SIGNING_PUBKEY)"
    if [[ -n "$pub" ]]; then
      if [[ "$(printf '%s' "$pub" | base64 -d 2>/dev/null | wc -c | tr -d ' ')" == 32 ]]; then
        pass "AUTH_SIGNING_PUBKEY is a base64 32-byte Ed25519 key"
      else
        fail "AUTH_SIGNING_PUBKEY is not a base64 32-byte Ed25519 key — copy the auth service's key (printed by its bootstrap)"
      fi
    fi
    # The trust split is a security boundary, not a preference: two hostnames
    # on one registrable domain lets agent HTML toss cookies onto the trusted
    # host (PLAN.md §7). Same parent-domain heuristic bootstrap.sh warns with.
    dashboard_host="$(env_get DASHBOARD_HOST)"
    content_host="$(env_get CONTENT_HOST)"
    if [[ -n "$dashboard_host" && -n "$content_host" ]]; then
      if [[ "$content_host" == *".${dashboard_host#*.}" || "${content_host#*.}" == "${dashboard_host#*.}" ]]; then
        fail "CONTENT_HOST ($content_host) shares a registrable domain with DASHBOARD_HOST ($dashboard_host) — agent HTML can toss cookies onto the trusted host (PLAN.md §7)"
      else
        pass "content host is a separate registrable domain from the dashboard host"
      fi
    fi
  fi

  step "Database"
  db_url="$(env_get DATABASE_URL)"
  if command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet postgresql.service 2>/dev/null; then
      pass "postgresql.service active"
    else
      fail "postgresql.service not active — pages.service Requires it: systemctl status postgresql"
    fi
  fi
  if [[ -n "$db_url" ]]; then
    if ! command -v psql >/dev/null 2>&1; then
      advise "psql missing — skipping the direct database probe (/readyz below still covers schema)"
    elif [[ $EUID -eq 0 ]]; then
      if runuser -u "$APP_USER" -- env "DATABASE_URL=$db_url" psql -tAc "SELECT 1" 2>/dev/null | grep -q 1; then
        pass "database accepts the '$APP_USER' role"
      else
        fail "database probe failed — check DATABASE_URL in $ENV_FILE and pg_hba.conf"
      fi
    elif env "DATABASE_URL=$db_url" psql -tAc "SELECT 1" 2>/dev/null | grep -q 1; then
      pass "database accepts the '$APP_USER' role"
    else
      advise "database probe failed as uid $EUID — re-run with sudo to probe as $APP_USER"
    fi
  fi

  step "Service"
  if ! command -v systemctl >/dev/null 2>&1; then
    advise "no systemd on this host — skipping service checks (a dev box, or an unusual supervisor)"
  else
    if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
      pass "$SERVICE active"
    elif [[ "$CHECK_ONLY" == 1 ]]; then
      fail "$SERVICE not active — inspect: pages logs"
    elif systemctl start "$SERVICE" 2>/dev/null && systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
      fixed "started $SERVICE"
    else
      fail "$SERVICE failed to start — inspect: pages logs"
    fi
    if systemctl is-enabled --quiet "$SERVICE" 2>/dev/null; then
      pass "$SERVICE enabled"
    else
      advise "$SERVICE not enabled — it will not start on boot: systemctl enable pages"
    fi
    # A release switch is only complete once the service restarts onto it;
    # update.sh normally does this, but a box that skipped it keeps serving the
    # old release while every file on disk claims the new one. Compare the
    # process cwd (the release dir) against the APP_DIR symlink target.
    pid="$(systemctl show -p MainPID --value "$SERVICE" 2>/dev/null || true)"
    if [[ "$pid" =~ ^[1-9][0-9]*$ ]]; then
      if [[ ! -L "$APP_DIR" ]]; then
        pass "$APP_DIR is a legacy (non-release) install — nothing to compare"
      elif running="$(readlink -f "/proc/$pid/cwd" 2>/dev/null)" && current="$(readlink -f "$APP_DIR" 2>/dev/null)" \
           && [[ -n "$running" && -n "$current" ]]; then
        if [[ "$running" == "$current" ]]; then
          pass "running release matches $APP_DIR"
        elif [[ "$CHECK_ONLY" == 1 ]]; then
          fail "service still runs $running while $APP_DIR points at $current — the last release switch never restarted: sudo systemctl restart pages.service"
        elif [[ "$NO_RESTART" == 1 ]]; then
          advise "service runs the previous release — apply it: sudo pages restart"
        elif systemctl restart "$SERVICE" 2>/dev/null; then
          fixed "restarted $SERVICE onto the current release"
          restarted=1
        else
          fail "restart failed — inspect: pages logs"
        fi
      else
        advise "cannot read the service's working directory as uid $EUID — re-run with sudo to compare the running release"
      fi
    fi
  fi

  step "Readiness"
  healthy=0
  tries=1
  if [[ "$restarted" == 1 ]]; then tries=15; fi
  for ((i=0; i<tries; i++)); do
    if curl -fsS --max-time 4 "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1; then
      healthy=1
      break
    fi
    if (( i < tries - 1 )); then sleep 1; fi
  done
  if [[ "$healthy" == 1 ]]; then
    pass "/readyz → 200 (database + migrations ready)"
  else
    fail "/readyz not ready on :$PORT — inspect: pages logs"
  fi

  step "Caddy / TLS"
  if ! command -v caddy >/dev/null 2>&1; then
    advise "caddy not installed — fronting the service is the operator's proxy's job; skipping TLS checks"
  elif [[ ! -f "$CADDY_SNIPPET" ]]; then
    advise "no $CADDY_SNIPPET — Caddy runs without a Pages site block; rerun bootstrap to set up TLS"
  else
    pass "$(caddy version 2>/dev/null | head -n1 || echo 'caddy present')"
    if systemctl is-active --quiet caddy.service 2>/dev/null; then
      pass "caddy.service active"
    else
      fail "caddy.service inactive — inspect: journalctl -u caddy"
    fi
    if [[ ! -f /etc/caddy/Caddyfile ]]; then
      fail "/etc/caddy/Caddyfile missing — caddy cannot serve the $CADDY_SNIPPET site block"
    elif caddy validate --config /etc/caddy/Caddyfile >/dev/null 2>&1; then
      pass "Caddy configuration valid"
    else
      fail "Caddy configuration invalid — run: caddy validate --config /etc/caddy/Caddyfile"
    fi
    if [[ -z "$dashboard_host" && -z "$content_host" ]]; then
      advise "no hostnames in $ENV_FILE — skipping certificate probes"
    fi
    for tls_host in "$dashboard_host" "$content_host"; do
      [[ -n "$tls_host" ]] || continue
      enddate="$( { echo | openssl s_client -servername "$tls_host" -connect "$tls_host:443" 2>/dev/null \
        | openssl x509 -noout -enddate 2>/dev/null; } || true)"
      enddate="${enddate#notAfter=}"
      if [[ -z "$enddate" ]]; then
        fail "no certificate answers https://$tls_host — check DNS, firewalld and: journalctl -u caddy"
        continue
      fi
      if end_s="$(date -d "$enddate" +%s 2>/dev/null)"; then
        days=$(( ( end_s - $(date +%s) ) / 86400 ))
        if (( days < 0 )); then
          fail "certificate for $tls_host expired on $enddate"
        elif (( days < 14 )); then
          advise "certificate for $tls_host expires in $days day(s) ($enddate)"
        else
          pass "https://$tls_host — certificate valid, $days days left ($enddate)"
        fi
      else
        pass "https://$tls_host serves a certificate (expiry unreadable: $enddate)"
      fi
    done
  fi

  step "Disk"
  if [[ ! -d "$APP_DIR" ]]; then
    fail "application directory missing: $APP_DIR — restore the install or rerun scripts/bootstrap.sh"
  elif ! free_kib="$(df -Pk "$APP_DIR" 2>/dev/null | awk 'END {print $4}')" || [[ ! "$free_kib" =~ ^[0-9]+$ ]]; then
    fail "cannot read free space for $APP_DIR — inspect its filesystem/mount"
  elif [[ "$free_kib" -ge 1048576 ]]; then
    pass "at least 1 GiB free on $APP_DIR"
  else
    fail "less than 1 GiB free on $APP_DIR — reclaim space (updates keep predecessors under ${APP_DIR}.releases)"
  fi

  step "Host packages"
  support_end="$(env_get SUPPORT_END "$OS_RELEASE")"
  if [[ -n "$support_end" ]]; then
    if expiry_s="$(date -d "$support_end" +%s 2>/dev/null)"; then
      days=$(( ( expiry_s - $(date +%s) ) / 86400 ))
      if (( days < 30 )); then
        fail "OS support ends $support_end ($days days) — schedule a distro upgrade"
      else
        pass "OS support ends $support_end ($days days)"
      fi
    else
      advise "OS support end '$support_end' is not parseable — verify the vendor lifecycle"
    fi
  else
    advise "OS does not publish SUPPORT_END — verify the vendor lifecycle"
  fi
  if command -v dnf >/dev/null 2>&1; then
    # `dnf check-update` exits 100 when updates are available; that is a
    # report, not an error. Updating stays the operator's `dnf upgrade`.
    if dnf check-update >/dev/null 2>&1; then
      pass "dnf packages current"
    else
      rc=$?
      if [[ "$rc" == 100 ]]; then
        advise "dnf updates pending — run: sudo dnf upgrade --refresh"
      else
        advise "dnf check-update failed (rc=$rc) — inspect repos and network"
      fi
    fi
  else
    advise "no dnf on this host — keep packages current with your package manager"
  fi
  if command -v rpm >/dev/null 2>&1; then
    # Reboot-needed without the dnf-utils dependency: the running kernel is
    # older than the newest installed one. /proc/PID/exe-style tricks do not
    # exist for the kernel; rpm vs uname is the cheap honest probe.
    latest_kernel="$(rpm -q kernel --qf '%{VERSION}-%{RELEASE}.%{ARCH}\n' 2>/dev/null | sort -V | tail -n 1 || true)"
    if [[ -n "$latest_kernel" && "$latest_kernel" != "$(uname -r)" ]]; then
      advise "reboot pending: running kernel $(uname -r), installed $latest_kernel"
    elif [[ -n "$latest_kernel" ]]; then
      pass "running kernel matches the newest installed"
    fi
  fi
  os_id="$(env_get ID "$OS_RELEASE")"
  os_version="$(env_get VERSION_ID "$OS_RELEASE")"
  if [[ "$os_id" == fedora && "$os_version" =~ ^[0-9]+$ ]]; then
    if latest="$( { curl -fsS --connect-timeout 5 --max-time 30 https://fedoraproject.org/releases.json \
        | pages_latest_fedora; } 2>/dev/null )" && [[ "$latest" =~ ^[0-9]+$ ]]; then
      if (( latest <= os_version )); then
        pass "Fedora $os_version (latest stable: $latest)"
      else
        advise "Fedora $os_version is behind latest stable ($latest) — plan a system upgrade (two releases per hop maximum)"
      fi
    else
      advise "could not read the Fedora release feed — skipping the version check"
    fi
  fi

  step "Source checkout"
  if ! command -v git >/dev/null 2>&1; then
    advise "git missing — cannot inspect the source checkout"
  elif [[ ! -d "$SRC_DIR/.git" ]]; then
    advise "no git checkout at $SRC_DIR — 'pages update' will not work on this box"
  else
    git=(git -c "safe.directory=$SRC_DIR" -C "$SRC_DIR")
    branch="$("${git[@]}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
    if [[ "$branch" == main ]]; then
      pass "checkout on main"
    else
      advise "checkout is on '$branch', not main — 'pages update' follows the checked-out branch"
    fi
    if dirty="$("${git[@]}" status --porcelain 2>/dev/null)" && [[ -z "$dirty" ]]; then
      pass "checkout clean"
    else
      advise "checkout has local changes — resolve them before pages update"
    fi
    if "${git[@]}" fetch --quiet origin 2>/dev/null; then
      behind="$("${git[@]}" rev-list --count HEAD..origin/main 2>/dev/null || echo 0)"
      if [[ "$behind" =~ ^[0-9]+$ ]] && (( behind > 0 )); then
        advise "checkout is $behind commit(s) behind origin/main — run: sudo pages update"
      else
        pass "checkout current with origin/main"
      fi
    else
      advise "could not fetch origin — network or auth issue"
    fi
  fi

  echo
  problems=$n_fail
  if [[ "$STRICT" == 1 ]]; then
    problems=$(( problems + n_warn ))
  fi
  if (( problems > 0 )); then
    printf '%s✗ doctor: %d ok, %d fixed, %d advisories, %d PROBLEM(S)%s\n' \
      "$c_red" "$n_ok" "$n_fixed" "$n_warn" "$problems" "$c_reset"
    exit 1
  elif (( n_fixed > 0 )); then
    printf '%s✓ doctor: %d ok, %d fixed, %d advisories — box repaired%s\n' \
      "$c_green" "$n_ok" "$n_fixed" "$n_warn" "$c_reset"
  else
    printf '%s✓ doctor: %d ok, %d advisories — box healthy%s\n' \
      "$c_green" "$n_ok" "$n_warn" "$c_reset"
  fi
  exit 0
)

pages_latest_fedora() {
  python3 -c 'import json,sys; print(max(int(r["version"]) for r in json.load(sys.stdin) if r.get("version", "").isascii() and r.get("version", "").isdigit()))'
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  # --check/--dry-run/--help need no root; repairs do.
  needs_root=1
  for arg in "$@"; do
    case "$arg" in
      --check|--dry-run|--help|-h) needs_root=0 ;;
    esac
  done
  if [[ $EUID -ne 0 && "$needs_root" == 1 ]]; then
    echo "run as root: sudo pages doctor (or: pages doctor --check)" >&2
    exit 1
  fi
  pages_doctor "$@"
fi
