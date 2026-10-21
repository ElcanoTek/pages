#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/bootstrap.sh — install (or reconfigure) Elcano Pages on this box.
#
# Pages is one Go binary; this bridge only exists because the installer must
# run before that binary does. It makes sure git, make and a Go toolchain are
# present (dnf, or the official toolchain go.mod pins), builds `pages` from
# this checkout, and runs `pages bootstrap`, which does the actual work:
# PostgreSQL, the service account, the release, /etc/default/pages, the
# systemd unit, the CLI, the login banner, Cockpit removal, key-only SSH, the
# hostname and (optionally) Caddy + auto-TLS. Safe to re-run.
#
# Usage:    sudo bash scripts/bootstrap.sh
# Scripted: PAGES_BOOTSTRAP_NON_INTERACTIVE=1 + AUTH_SIGNING_PUBKEY +
#           PAGES_BOOTSTRAP_DASHBOARD_HOST + PAGES_BOOTSTRAP_CONTENT_HOST
#           (see pages bootstrap --help)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[[ $EUID -eq 0 ]] || { echo "run as root: sudo bash scripts/bootstrap.sh" >&2; exit 1; }
[[ -f "$root/go.mod" && -d "$root/cmd/pages" ]] || { echo "not an Elcano Pages checkout at $root" >&2; exit 1; }

# shellcheck source=scripts/lib/go-toolchain.sh
. "$root/scripts/lib/go-toolchain.sh"
pages_ensure_go "$root"

build="$(mktemp -d /tmp/pages-bootstrap-XXXXXX)"
trap 'rm -rf "$build"' EXIT
echo "» building pages from $root"
make -C "$root" --no-print-directory build BIN="$build/pages" >/dev/null

# bootstrap copies the binary into the release it installs, so the build
# directory can go when it returns. Prompts read the TTY even when stdin is a
# pipe.
SRC_DIR="$root" "$build/pages" bootstrap "$@"
