#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# Shared bootstrap/updater settings. The persisted file contains non-secret
# defaults and deliberately preserves explicit caller overrides.
PAGES_INSTALL_CONFIG="${PAGES_INSTALL_CONFIG:-/etc/default/pages-install}"
if [[ -f "$PAGES_INSTALL_CONFIG" ]]; then . "$PAGES_INSTALL_CONFIG"; fi
APP_DIR="${APP_DIR:-${PAGES_APP_DIR:-/opt/pages}}"
APP_USER="${APP_USER:-${PAGES_APP_USER:-pages}}"
INSTALL_SRC_DIR="${PAGES_SRC_DIR:-/opt/pages-src}"
ENV_FILE="${PAGES_ENV_FILE:-/etc/default/pages}"
CLI_TARGET="${PAGES_CLI_TARGET:-/usr/local/bin/pages}"
PORT="${PAGES_PORT:-}"
if [[ -z "$PORT" && -f "$ENV_FILE" ]]; then
  PORT="$(. "$ENV_FILE"; printf '%s' "${PORT:-3002}")"
fi
PORT="${PORT:-3002}"

render_install() {
  APP_DIR="$APP_DIR" APP_USER="$APP_USER" INSTALL_SRC_DIR="$INSTALL_SRC_DIR" \
    ENV_FILE="$ENV_FILE" CLI_TARGET="$CLI_TARGET" PORT="$PORT" \
    PAGES_INSTALL_CONFIG="$PAGES_INSTALL_CONFIG" \
    node "$PAGES_SCRIPT_ROOT/scripts/render-install.js" "$@"
}
