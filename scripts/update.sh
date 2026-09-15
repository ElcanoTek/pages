#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# Build a complete release, then atomically switch the active symlink. Retain
# the predecessor and restore it on startup/readiness failure. Database changes
# are never reversed; only explicitly compatible pending migrations run here.

pages_update() (
  set -euo pipefail
  PAGES_SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  . "$PAGES_SCRIPT_ROOT/scripts/install-config.sh"
  SRC_DIR="$INSTALL_SRC_DIR"
  SERVICE="pages.service"
  SERVICE_FILE="${PAGES_SERVICE_FILE:-/etc/systemd/system/pages.service}"
  RELEASE_ROOT="${APP_DIR}.releases"
  SHARED_ASSETS="${APP_DIR}.assets"
  SHARED_ENV="${APP_DIR}.env"
  candidate=""; previous=""; release=""; recovery=""; switching=0

  info() { printf '%s\n' "$*"; }
  warn() { printf '%s\n' "$*" >&2; }
  die() { warn "$*"; exit 1; }
  render_install check || die "invalid installation settings"
  [[ -d "$SRC_DIR/.git" || -f "$SRC_DIR/.git" ]] || die "no source checkout at $SRC_DIR"
  [[ -d "$APP_DIR" ]] || die "no installed application at $APP_DIR — run bootstrap first"
  [[ -f "$ENV_FILE" ]] || die "missing environment file $ENV_FILE"
  mkdir -p "$RELEASE_ROOT"
  exec 9>"${APP_DIR}.update.lock"
  flock -n 9 || die "another Pages update is running for $APP_DIR"

  switch_link() {
    ln -s "$1" "${APP_DIR}.next.$$"
    mv -Tf "${APP_DIR}.next.$$" "$APP_DIR"
  }
  ready() {
    for ((attempt=0; attempt<10; attempt++)); do
      if curl --max-time 4 -fsS "http://127.0.0.1:$PORT/readyz" >/dev/null 2>&1; then return 0; fi
      sleep 1
    done
    return 1
  }
  app_command() {
    local directory="$1"; shift
    runuser -u "$APP_USER" -- bash -c '
      env_file="$1"; directory="$2"; shift 2
      set -a; . "$env_file"; set +a
      export NODE_ENV=production
      cd "$directory"
      exec "$@"
    ' _ "$ENV_FILE" "$directory" "$@"
  }
  restore_file() {
    if [[ -f "$recovery/$1" ]]; then cp -a "$recovery/$1" "$2"; else rm -f "$2"; fi
  }
  cleanup() {
    local result=$?
    trap - EXIT
    set +e
    if [[ "$switching" == 1 ]]; then
      warn "update failed; restoring the previous compatible release"
      systemctl stop "$SERVICE"
      if [[ -d "$previous" ]]; then
        # Repair the first legacy conversion if it failed between moving a
        # mutable path and installing its shared symlink.
        [[ -e "$SHARED_ASSETS" && ! -e "$previous/assets" ]] && ln -s "$SHARED_ASSETS" "$previous/assets"
        [[ -e "$SHARED_ENV" && ! -e "$previous/.env" ]] && ln -s "$SHARED_ENV" "$previous/.env"
        rm -f "${APP_DIR}.next.$$"
        switch_link "$previous"
      fi
      restore_file service "$SERVICE_FILE"
      restore_file cli "$CLI_TARGET"
      systemctl daemon-reload
      if systemctl start "$SERVICE" && ready; then
        warn "previous release is ready again; the update failed and was not committed"
      else
        warn "automatic recovery needs operator attention; predecessor=$previous; configuration backup=$recovery"
      fi
    fi
    [[ -n "$candidate" && -d "$candidate" ]] && rm -rf "$candidate"
    rm -f "${APP_DIR}.next.$$"
    exit "$result"
  }
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  if [[ "${PAGES_UPDATE_NO_PULL:-0}" != 1 ]]; then
    info "Fetching latest"
    before_sha="$(git -C "$SRC_DIR" rev-parse HEAD)"
    git -C "$SRC_DIR" fetch --quiet origin
    branch="$(git -C "$SRC_DIR" rev-parse --abbrev-ref HEAD)"
    [[ "$branch" == HEAD ]] && branch=main
    git -C "$SRC_DIR" merge --ff-only "origin/$branch" || die "fast-forward failed — resolve the source checkout first"
    after_sha="$(git -C "$SRC_DIR" rev-parse HEAD)"
    if ! git -C "$SRC_DIR" diff --quiet "$before_sha" "$after_sha" -- scripts/update.sh; then
      exec env PAGES_UPDATE_NO_PULL=1 PAGES_UPDATE_YES="${PAGES_UPDATE_YES:-0}" bash "$SRC_DIR/scripts/update.sh"
    fi
  fi
  if [[ "${PAGES_UPDATE_YES:-0}" != 1 && -t 0 ]]; then
    read -rp "Build and activate a new release? [y/N] " answer
    [[ "${answer,,}" =~ ^(y|yes)$ ]] || die "aborted"
  fi
  node "$SRC_DIR/scripts/check-node.js" || die "unsupported Node runtime — installed service untouched"

  info "Building release"
  candidate="$(mktemp -d "$RELEASE_ROOT/.candidate.XXXXXX")"
  chmod 0755 "$candidate"
  rsync -a --delete --exclude='/.git' --exclude='/node_modules' --exclude='/assets' --exclude='/.env' \
    --exclude='/.devdata' --exclude='/test-results' --exclude='/playwright-report' "$SRC_DIR/" "$candidate/"
  chown -R "$APP_USER:$APP_USER" "$candidate"
  app_command "$candidate" npm ci --omit=dev --no-audit --no-fund --loglevel=warn
  app_command "$candidate" node scripts/preflight-install.js
  app_command "$candidate" node scripts/check-migration-compatibility.js
  app_command "$candidate" node lib/migrate.js
  sha="$(git -C "$SRC_DIR" rev-parse --short HEAD)"
  release="$RELEASE_ROOT/${sha}-$(date -u +%Y%m%dT%H%M%SZ)-$$"
  mv "$candidate" "$release"
  candidate=""

  # Snapshot the small installed control files before stopping anything. Release
  # contents are retained; mutable paths live outside the release directories.
  recovery="$(mktemp -d "$RELEASE_ROOT/.recovery.XXXXXX")"
  [[ ! -f "$SERVICE_FILE" ]] || cp -a "$SERVICE_FILE" "$recovery/service"
  [[ ! -f "$CLI_TARGET" ]] || cp -a "$CLI_TARGET" "$recovery/cli"
  if [[ -L "$APP_DIR" ]]; then previous="$(readlink -f "$APP_DIR")";
  else previous="$RELEASE_ROOT/legacy-$(date -u +%Y%m%dT%H%M%SZ)-$$"; fi
  if [[ ! -L "$APP_DIR" ]]; then
    [[ ! -e "$SHARED_ASSETS" ]] || die "$SHARED_ASSETS already exists; reconcile it before converting the legacy install"
    [[ ! -e "$SHARED_ENV" ]] || die "$SHARED_ENV already exists; reconcile it before converting the legacy install"
  fi

  info "Activating $release"
  switching=1
  systemctl stop "$SERVICE"
  if [[ ! -L "$APP_DIR" ]]; then
    mv "$APP_DIR" "$previous"
    if [[ -e "$previous/assets" ]]; then mv "$previous/assets" "$SHARED_ASSETS"; else mkdir "$SHARED_ASSETS"; fi
    ln -s "$SHARED_ASSETS" "$previous/assets"
    if [[ -e "$previous/.env" ]]; then mv "$previous/.env" "$SHARED_ENV"; ln -s "$SHARED_ENV" "$previous/.env"; fi
  fi
  ln -s "$SHARED_ASSETS" "$release/assets"
  [[ ! -e "$SHARED_ENV" ]] || ln -s "$SHARED_ENV" "$release/.env"
  chown -h "$APP_USER:$APP_USER" "$release/assets"
  switch_link "$release"
  PAGES_SCRIPT_ROOT="$release"
  install_rendered service "$SERVICE_FILE" 0644
  install_rendered cli "$CLI_TARGET" 0755
  systemctl daemon-reload
  systemctl start "$SERVICE"
  ready || die "new release failed readiness"
  switching=0
  ln -s "$previous" "$RELEASE_ROOT/.previous.$$"
  mv -Tf "$RELEASE_ROOT/.previous.$$" "$RELEASE_ROOT/previous"
  info "Pages ready on :$PORT; previous release retained at $previous"

  if [[ "${PAGES_SKIP_TEMPLATE_SYNC:-0}" != 1 && -f "$APP_DIR/scripts/template.js" && -d "$APP_DIR/templates" ]]; then
    app_command "$APP_DIR" node scripts/template.js sync || warn "template sync reported problems; previous revisions remain current"
  fi
  info "update complete"
)

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  [[ $EUID -eq 0 ]] || { echo "run as root (use: pages update)" >&2; exit 1; }
  pages_update "$@"
fi
