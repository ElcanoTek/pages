#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# Capture under the updater's lock without stopping the running application.
set -euo pipefail
PAGES_SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
. "$PAGES_SCRIPT_ROOT/scripts/install-config.sh"
[[ -d "$APP_DIR" && -f "$ENV_FILE" ]] || { echo "installed application and service environment are required" >&2; exit 1; }
exec 9>"${APP_DIR}.update.lock"
flock -sn 9 || { echo "a Pages update is running; retry the backup after it finishes" >&2; exit 1; }
# Snapshot path settings before sourcing the service environment. All arguments
# stay separate shell words, including custom installation paths with spaces.
backup_app_dir="$(readlink -f "$APP_DIR")"
backup_env_file="$ENV_FILE"
backup_app_user="$APP_USER"
backup_env_digest="$(node -e 'process.stdout.write(require("node:crypto").createHash("sha256").update(require("node:fs").readFileSync(process.argv[1])).digest("hex"))' "$backup_env_file")"
backup_install_config="$PAGES_INSTALL_CONFIG"
unset RAW_TOKEN_SECRET API_TOKEN_PEPPER PAGE_COOKIE_SECRET DATABASE_URL
for backup_pg_key in ${!PG@}; do unset "$backup_pg_key"; done
set -a
. "$backup_env_file"
set +a
for backup_secret in RAW_TOKEN_SECRET API_TOKEN_PEPPER PAGE_COOKIE_SECRET; do
  [[ -n "${!backup_secret:-}" ]] || { echo "service environment must define $backup_secret before backup" >&2; exit 1; }
done
export PAGES_BACKUP_APP_USER="$backup_app_user"
export PAGES_BACKUP_ENV_DIGEST="$backup_env_digest"
exec node "$PAGES_SCRIPT_ROOT/scripts/backup.js" create "${1:-/var/backups/pages}" \
  "$backup_app_dir" "$backup_env_file" "$backup_install_config" "${@:2}"
