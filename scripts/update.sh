#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/update.sh — bridge to `pages update` for boxes that predate it.
#
# Updates are `pages update` (internal/ops/update): build a release from the
# checkout, migrate only declared-compatible migrations, switch the active
# symlink, require /readyz, roll back on failure. A box still running the
# shell updater reaches THIS file after its own fetch — it re-executes the
# new scripts/update.sh whenever that file changed, with
# PAGES_UPDATE_NO_PULL=1 and the installation settings in the environment.
# So this builds the binary from the freshly pulled checkout and hands over;
# `pages update` then builds the release, converts the old layout, installs
# the CLI as a link to the active binary, and keeps the previous release (and
# the old control files) for rollback.
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "run as root (use: pages update)" >&2; exit 1; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=scripts/lib/go-toolchain.sh
. "$root/scripts/lib/go-toolchain.sh"
pages_ensure_go "$root"

build="$(mktemp -d /tmp/pages-update-XXXXXX)"
echo "Building the updater"
if ! make -C "$root" --no-print-directory build BIN="$build/pages" >/dev/null; then
  rm -rf "$build"
  echo "build failed — installed service untouched" >&2
  exit 1
fi
# pages update removes PAGES_UPDATE_SHIM_DIR once it has built its own release.
exec env PAGES_UPDATE_SHIM_DIR="$build" PAGES_SRC_DIR="${PAGES_SRC_DIR:-$root}" "$build/pages" update "$@"
