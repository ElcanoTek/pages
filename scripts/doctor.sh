#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/doctor.sh — diagnose this Pages box.
#
# `pages doctor` walks every box-level prerequisite Pages depends on and
# reports PASS/WARN/FAIL with the remedy; run as root it also makes the safe
# repairs (env file ownership/mode, starting a stopped service, restarting a
# stale release). `--check` reports without touching anything (the pages CLI
# runs it as the service user, not root). What it never does: git pull or
# fetch, dnf upgrade, migrations or rebuilds — doctor makes the box readable;
# `pages update` lands the code.
#
# Usage:
#   sudo pages doctor             diagnose; fix what is safe (perms, start, restart)
#   pages doctor --check          diagnose only, change nothing, exit 1 on any failure
#   sudo pages doctor --no-restart  fix but leave any service restart to the caller
#   pages doctor --dry-run        print the checklist; touch nothing (no root needed)
#   pages doctor --strict         warnings also exit 1
#
# Exit codes: 0 = healthy (or everything fixed), 1 = problems remain.

# The trust-split check compares registrable domains (eTLD+1) — the
# cookie-tossing boundary (PLAN.md §7). The computation uses the FULL vendored
# Mozilla Public Suffix List (scripts/lib/public-suffix-list.dat), not a
# shortlist: a shortlist mis-reduces omitted suffixes (e.g. com.ng absent →
# pages.contoso.com.ng and pages.northwind.com.ng both reduce to 'com.ng' and
# produce a false shared-domain failure). See the dat file's header for
# source, license (MPL-2.0) and refresh cadence.

# pages_registrable_domain HOST — print the eTLD+1 of HOST per the Public
# Suffix List algorithm (https://publicsuffix.org/list/): the prevailing rule
# is the longest matching rule (wildcards match exactly one label, exception
# rules starting with '!' drop their leftmost label), the public suffix is the
# labels that match, and the registrable domain is the suffix plus one more
# label to its left. Lowercases and tolerates a trailing dot. Prints nothing
# when the list file is unreadable — the caller treats that as 'cannot
# verify', never as a pass.
pages_registrable_domain() {
  local host="${1,,}"
  host="${host%.}"
  local -a h=()
  IFS='.' read -r -a h <<< "$host"
  local n=${#h[@]}
  if (( n == 0 )) || [[ ! -r "$PSL_FILE" ]]; then
    return 0
  fi
  local line rule rl hl i match best=0 best_is_exception=0
  local -a r=()
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    case "$line" in
      ''|'//'*) continue ;;          # blanks, Mozilla header/section comments
    esac
    rule="$line"                      # dat rules are bare suffixes (no inline comments)
    local is_exception=0
    case "$rule" in
      '!'*) is_exception=1; rule="${rule#!}" ;;
    esac
    r=(); IFS='.' read -r -a r <<< "$rule"
    local rn=${#r[@]}
    (( rn > n )) && continue          # a rule cannot be longer than the host
    match=1
    for (( i=1; i<=rn; i++ )); do
      rl="${r[rn-i]}"; hl="${h[n-i]}"
      if [[ "$rl" == '*' ]]; then
        continue                      # wildcard: exactly one label, always matches
      elif [[ "$rl" != "$hl" ]]; then
        match=0; break
      fi
    done
    # Longest match prevails (per spec); on a TIE an exception rule wins over
    # a wildcard of equal length — for foo.www.ck both '*.ck' and '!www.ck'
    # match at length 2, and the exception is what makes www.ck a normal
    # registrable domain (subdomains of it share one cookie boundary).
    if (( match )) && { (( rn > best )) \
         || { (( rn == best )) && (( is_exception == 1 )) && (( best_is_exception == 0 )); }; }; then
      best=$rn
      best_is_exception=$is_exception
    fi
  done < "$PSL_FILE"
  local suffix_labels
  if (( best == 0 )); then
    suffix_labels=1                   # prevailing rule is the implicit "*"
  elif (( best_is_exception )); then
    suffix_labels=$(( best - 1 ))     # exception: drop the leftmost rule label
    (( suffix_labels < 1 )) && suffix_labels=1
  else
    suffix_labels=$best
  fi
  local reg_labels=$(( suffix_labels + 1 ))
  if (( reg_labels > n )); then
    printf '%s\n' "$host"             # the host itself is inside a public suffix
  else
    local out="" start=$(( n - reg_labels )) j
    for (( j=start; j<n; j++ )); do
      out+="${h[j]}."
    done
    printf '%s\n' "${out%.}"
  fi
}

# pages_latest_fedora — newest numeric Fedora stable from releases.json on
# stdin (used by the Host packages step).
pages_latest_fedora() {
  python3 -c 'import json,sys; print(max(int(r["version"]) for r in json.load(sys.stdin) if r.get("version", "").isascii() and r.get("version", "").isdigit()))'
}

pages_doctor() (
  set -euo pipefail

  # Installation layout. These mirror scripts/install-config.sh — KEEP IN SYNC
  # — but doctor deliberately does NOT source that file (or the env file) to
  # learn them: sourcing executes file content as the doctor's user (root),
  # and the repair path exists precisely for when those files' permissions
  # have gone wrong. Root-run doctor must never evaluate service-readable
  # files before validating them.
  APP_DIR="${APP_DIR:-${PAGES_APP_DIR:-/opt/pages}}"
  APP_USER="${APP_USER:-${PAGES_APP_USER:-pages}}"
  SRC_DIR="${PAGES_SRC_DIR:-/opt/pages-src}"
  ENV_FILE="${PAGES_ENV_FILE:-/etc/default/pages}"
  SERVICE="pages.service"
  CADDY_SNIPPET="/etc/caddy/conf.d/pages.caddy"
  DOCTOR_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  # Full Mozilla Public Suffix List, vendored next to this script (see the
  # dat file's header for source/license/refresh cadence). Overridable for tests.
  PSL_FILE="${PAGES_PSL_FILE:-$DOCTOR_SCRIPT_DIR/lib/public-suffix-list.dat}"
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

  # env_get KEY [FILE] — read one key without sourcing the file (it holds
  # secrets; sourcing would execute arbitrary content on a tampered box).
  # Last assignment wins; surrounding quotes are stripped and the
  # shell-style escapes write-env.js puts INSIDE double quotes (\" \\ \$
  # \`) are decoded — systemd decodes them when loading the file, so the
  # doctor must too or it validates a value the service never uses. Never
  # prints a value.
  env_get() {
    local key="$1" file="${2:-$ENV_FILE}" value
    [[ -r "$file" ]] || return 0
    value="$(grep -E "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2-)" || return 0
    case "$value" in
      \"*\")
        value="${value#\"}"; value="${value%\"}"
        # One left-to-right pass over non-overlapping pairs: \\ and \" pair
        # correctly because sed resumes scanning after each replacement.
        value="$(printf '%s' "$value" | sed -e 's/\\\(["\\$`]\)/\1/g')" ;;
      \'*\')
        value="${value#\'}"; value="${value%\'}" ;;   # single quotes: no escapes
    esac
    printf '%s' "$value"
  }

  # PORT comes from the env file via the non-evaluating parser — never sourced.
  PORT="${PAGES_PORT:-$(env_get PORT)}"
  PORT="${PORT:-3002}"

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

Via the pages CLI, read-only modes run as the service user (sudo -u pages),
not root; the plain command runs as root because repairs need it.

Checks: Node runtime (scripts/check-node.js), the environment file (presence,
0640 root:pages, required keys — values are never printed), the
DASHBOARD_HOST/CONTENT_HOST eTLD+1 split, PostgreSQL, the systemd service
(active, enabled, running the current release), /readyz, Caddy + chain and
hostname verified TLS for BOTH hostnames, free disk, the OS support window,
pending dnf updates, a pending reboot, Fedora vs latest stable, and the
source checkout (clean, on main, compared against origin via ls-remote — no
fetch, the checkout is never mutated).
Repairs are limited to: env file ownership/mode, starting a stopped service,
restarting a stale release. Doctor never pulls, fetches, upgrades or
rebuilds — that stays `pages update`.
EOF
        exit 0 ;;
      *) echo "error: unknown argument: $arg (try --help)" >&2; exit 2 ;;
    esac
  done

  if [[ "$DRY_RUN" == 1 ]]; then
    step "pages doctor --dry-run (app=$APP_DIR, src=$SRC_DIR, service=$SERVICE)"
    info "[dry-run] Runtime: node present and passing $SRC_DIR/scripts/check-node.js (20.19+/22.12+); npm present"
    info "[dry-run] Configuration: $ENV_FILE exists, mode 0640 root:$APP_USER, with AUTH_SIGNING_PUBKEY (base64 32-byte), PAGE_COOKIE_SECRET, RAW_TOKEN_SECRET, API_TOKEN_PEPPER, DATABASE_URL, DASHBOARD_HOST and CONTENT_HOST set; content host on a DIFFERENT registrable domain (eTLD+1) than the dashboard host"
    info "[dry-run] Database: postgresql.service active; psql probes DATABASE_URL as $APP_USER"
    info "[dry-run] Service: $SERVICE active + enabled; the running release matches $APP_DIR (stale releases restart, unless --no-restart); filesystem repairs happen before any unit start"
    info "[dry-run] Readiness: /readyz → 200 on :$PORT (15 tries after a repair start/restart)"
    info "[dry-run] Caddy/TLS: caddy active, configuration valid, certificates for BOTH hostnames chain + hostname verified and more than 14 days from expiry"
    info "[dry-run] Disk: at least 1 GiB free on $APP_DIR"
    info "[dry-run] Host: OS support window, pending dnf updates, reboot-needed kernel check, Fedora vs latest stable"
    info "[dry-run] Source: checkout clean, on main, compared with origin via git ls-remote (read-only — no fetch)"
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
  env_readable=1
  step "Configuration ($ENV_FILE)"
  if [[ ! -f "$ENV_FILE" ]]; then
    fail "$ENV_FILE missing — run scripts/bootstrap.sh"
  elif [[ ! -r "$ENV_FILE" ]]; then
    if [[ $EUID -eq 0 ]]; then
      fail "$ENV_FILE unreadable even as root — inspect its ownership and parent directories"
    fi
    # The read-only run (service user via the pages CLI) cannot read a
    # root-only file (e.g. one tightened to 0600 root:root). Report the
    # limited validation instead of cascading false 'key unset' failures.
    advise "$ENV_FILE is not readable as $(id -un) — key, hostname and database checks are limited; run sudo pages doctor for full validation"
    env_readable=0
  fi
  if [[ "$env_readable" == 1 && -f "$ENV_FILE" ]]; then
    if [[ -L "$ENV_FILE" ]]; then
      # Privileged chown/chmod follows symlinks; refuse rather than repair
      # through one — a symlinked credential file is drift worth an operator.
      fail "$ENV_FILE is a symlink — refusing to touch it; investigate and replace with a regular file"
    else
      perms="$(stat -c '%a %U:%G' "$ENV_FILE" 2>/dev/null || echo 'unknown')"
      want="640 root:$APP_USER"
      if [[ "$perms" == "$want" ]]; then
        pass "$ENV_FILE is $perms"
      elif [[ "$perms" == "600 root:root" ]]; then
        # Stricter than the shipped state: never downgrade it. The pages CLI
        # (token/template/backup) reads this file as $APP_USER, so warn
        # instead of loosening group access.
        pass "$ENV_FILE is $perms (stricter than $want)"
        advise "pages CLI commands that read $ENV_FILE as $APP_USER will fail while it is root-only — set $want if you use them"
      elif [[ "$CHECK_ONLY" == 1 ]]; then
        fail "$ENV_FILE is $perms — want $want (it holds every credential)"
      elif chown root:"$APP_USER" "$ENV_FILE" 2>/dev/null && chmod 0640 "$ENV_FILE" 2>/dev/null; then
        fixed "$ENV_FILE set to $want"
      else
        fail "could not set $ENV_FILE to $want — fix ownership/mode by hand"
      fi
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
      # The decoder itself must succeed: GNU base64 -d prints the decoded
      # prefix before erroring on a bad character, so a length check alone
      # would accept "valid32bytes!".
      if pub_len="$(printf '%s' "$pub" | base64 -d 2>/dev/null | wc -c | tr -d ' ')" && [[ "$pub_len" == 32 ]]; then
        pass "AUTH_SIGNING_PUBKEY is a base64 32-byte Ed25519 key"
      else
        fail "AUTH_SIGNING_PUBKEY is not a base64 32-byte Ed25519 key — copy the auth service's key (printed by its bootstrap)"
      fi
    fi
    # The trust split is a security boundary, not a preference: two hostnames
    # on one registrable domain lets agent HTML toss cookies onto the trusted
    # host (PLAN.md §7). Compare eTLD+1 (see pages_registrable_domain), not
    # first-label-stripped parents — us.example.com vs eu.example.com is NOT
    # a split even though the naive parent check passes.
    dashboard_host="$(env_get DASHBOARD_HOST)"
    content_host="$(env_get CONTENT_HOST)"
    if [[ -n "$dashboard_host" && -n "$content_host" ]]; then
      dashboard_reg="$(pages_registrable_domain "$dashboard_host")"
      content_reg="$(pages_registrable_domain "$content_host")"
      if [[ -z "$dashboard_reg" || -z "$content_reg" ]]; then
        advise "public suffix list unreadable at $PSL_FILE — cannot verify the trust split"
      elif [[ "$content_reg" == "$dashboard_reg" ]]; then
        fail "CONTENT_HOST ($content_host) shares the registrable domain $content_reg with DASHBOARD_HOST ($dashboard_host) — agent HTML can toss cookies onto the trusted host (PLAN.md §7)"
      else
        pass "content host registrable domain ($content_reg) differs from the dashboard's ($dashboard_reg)"
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
    else
      # The password never goes on psql's argv (world-readable via ps and
      # /proc/<pid>/cmdline while the probe runs): split the userinfo, hand
      # it to libpq through PGPASSWORD — readable only to the owner and root
      # in /proc/<pid>/environ — and pass a redacted URL positionally, like
      # bootstrap's psql calls minus the credential exposure.
      pg_password=""
      db_arg="$db_url"
      if [[ "$db_url" == *"@"* ]]; then
        db_userinfo="${db_url#*://}"
        db_userinfo="${db_userinfo%%@*}"
        if [[ "$db_userinfo" == *:* ]]; then
          pg_password="${db_userinfo#*:}"
          db_arg="${db_url/"$db_userinfo"@/"${db_userinfo%%:*}"@}"
        fi
      fi
      if [[ $EUID -eq 0 ]]; then
        if PGPASSWORD="$pg_password" runuser -u "$APP_USER" -- psql "$db_arg" -tAc "SELECT 1" 2>/dev/null | grep -q 1; then
          pass "database accepts the '$APP_USER' role (DATABASE_URL)"
        else
          fail "database probe failed — check DATABASE_URL in $ENV_FILE and pg_hba.conf"
        fi
      elif PGPASSWORD="$pg_password" psql "$db_arg" -tAc "SELECT 1" 2>/dev/null | grep -q 1; then
        pass "database accepts the '$APP_USER' role (DATABASE_URL)"
      else
        # The read-only run probes as the service user (via the pages CLI), so
        # a failed probe means the configured URL is unusable even though the
        # still-running service may be healthy on its old environment — the
        # next restart is what breaks. That is a failure, not a rerun-with-root
        # suggestion.
        fail "database probe failed — DATABASE_URL in $ENV_FILE is not usable as $(id -un); the running service may still be on its old environment, but the next restart will take Pages down"
      fi
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
      restarted=1   # readiness below polls like after a restart: Type=simple
                    # means `start` can return before the port is listening.
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
    # Invariant: every filesystem repair above runs BEFORE any unit start.
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
  elif [[ "$env_readable" == 0 && -z "${PAGES_PORT:-}" ]]; then
    fail "/readyz not ready on :$PORT — inspect: pages logs (PORT is the built-in default; $ENV_FILE was unreadable, so the real port is unknown — run sudo pages doctor)"
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
      # -verify_hostname checks the peer name; -verify_return_error makes
      # chain/hostname verification failure abort the handshake AND fail the
      # command — gating on s_client's exit means a self-signed, untrusted,
      # or wrong-host certificate cannot yield a parsed expiry. The one
      # supported deployment that is intentionally not publicly trusted is a
      # bootstrap with USE_LETSENCRYPT=n (`tls internal`, local CA): that one
      # downgrades to an advisory; every other verification failure is FAIL.
      if cert="$( { echo | openssl s_client -verify_hostname "$tls_host" -verify_return_error \
            -servername "$tls_host" -connect "$tls_host:443" 2>/dev/null; } )" \
         && [[ -n "$cert" ]]; then
        enddate="$(printf '%s\n' "$cert" | openssl x509 -noout -enddate 2>/dev/null | cut -d= -f2- || true)"
      else
        enddate=""
      fi
      if [[ -z "$enddate" ]]; then
        if grep -qE '^[[:space:]]*tls[[:space:]]+internal([[:space:]]|$)' "$CADDY_SNIPPET" 2>/dev/null; then
          advise "https://$tls_host does not verify against public trust (Caddy serves 'tls internal') — expected for this self-signed install; browsers will warn"
        else
          fail "no verifiable certificate for https://$tls_host (chain or hostname verification failed) — check DNS, firewalld and: journalctl -u caddy"
        fi
        continue
      fi
      if end_s="$(date -d "$enddate" +%s 2>/dev/null)"; then
        days=$(( ( end_s - $(date +%s) ) / 86400 ))
        if (( days < 0 )); then
          fail "certificate for $tls_host expired on $enddate"
        elif (( days < 14 )); then
          advise "certificate for $tls_host expires in $days day(s) ($enddate)"
        else
          pass "https://$tls_host — certificate verified (chain + hostname), $days days left ($enddate)"
        fi
      else
        pass "https://$tls_host serves a verified certificate (expiry unreadable: $enddate)"
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
    # safe.directory is required because the checkout is root-owned while the
    # read-only run is the service user; core.fsmonitor=false keeps a
    # tampered checkout's config from running a command as this user.
    git=(git -c "safe.directory=$SRC_DIR" -c core.fsmonitor=false -C "$SRC_DIR")
    branch="$("${git[@]}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo '?')"
    if [[ "$branch" == main ]]; then
      pass "checkout on main"
    else
      advise "checkout is on '$branch', not main — 'pages update' follows the checked-out branch"
    fi
    if ! dirty="$("${git[@]}" status --porcelain 2>/dev/null)"; then
      # A failed status (missing git, corrupt index, unreadable metadata) is
      # not a clean checkout — 'pages update' cannot use this tree either.
      advise "could not read checkout status (git error) — inspect: git -C $SRC_DIR status"
    elif [[ -z "$dirty" ]]; then
      pass "checkout clean"
    else
      advise "checkout has local changes — resolve them before pages update"
    fi
    # Freshness WITHOUT mutating the checkout: fetch writes objects,
    # remote-tracking refs and .git/FETCH_HEAD, which even a diagnostic must
    # not do. ls-remote reads the remote tip only; compare it to local HEAD.
    # A failed or blocked probe reads as unknown, never current. git has no
    # --max-time; coreutils timeout ships on every Fedora/RHEL box.
    ls_remote=( "${git[@]}" ls-remote origin main )
    if command -v timeout >/dev/null 2>&1; then
      ls_remote=( timeout 10 "${ls_remote[@]}" )
    fi
    remote_main="$( "${ls_remote[@]}" 2>/dev/null | awk '$2 == "refs/heads/main" { print $1; exit }' )" || true
    if [[ -z "$remote_main" ]]; then
      advise "could not reach origin — remote freshness unknown (network, auth, or timeout)"
    else
      head_sha="$( "${git[@]}" rev-parse HEAD 2>/dev/null || true )"
      if [[ -n "$head_sha" && "$remote_main" == "$head_sha" ]]; then
        pass "checkout current with origin/main"
      else
        behind="$( "${git[@]}" rev-list --count HEAD.."$remote_main" 2>/dev/null )" || true
        if [[ "$behind" =~ ^[0-9]+$ ]] && (( behind > 0 )); then
          advise "checkout is $behind commit(s) behind origin/main — run: sudo pages update"
        else
          advise "checkout differs from origin/main — run: sudo pages update"
        fi
      fi
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

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  # --check/--dry-run/--help need no root; repairs do. (Via the pages CLI the
  # read-only modes run as the service user — see deploy/pages-cli.)
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
