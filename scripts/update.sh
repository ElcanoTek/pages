#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/update.sh — bridge to `pages update` for boxes that predate it.
#
# Updates are `pages update` (internal/ops/update): by default it fetches the
# latest signed release tag, verifies SHA256SUMS, release.json and
# pages-linux-${arch} with the cosign pinned in cosign.lock, checks out the
# tag, migrates only declared-compatible migrations, switches the active
# symlink, requires /readyz, and rolls back on failure — without needing `go`
# or `make` on the box. Pass `--from-source` (or `PAGES_FROM_SOURCE=1`) to
# compile locally from the checkout instead.
set -euo pipefail

[[ $EUID -eq 0 ]] || { echo "run as root (use: pages update)" >&2; exit 1; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

from_source="${PAGES_FROM_SOURCE:-0}"
req_tag="${PAGES_RELEASE_TAG:-}"
allow_downgrade="${PAGES_ALLOW_DOWNGRADE:-0}"
for ((i = 1; i <= $#; i++)); do
  arg="${!i}"
  case "$arg" in
    --from-source) from_source=1 ;;
    --allow-downgrade) allow_downgrade=1 ;;
    --tag=*) req_tag="${arg#--tag=}" ;;
    --tag)
      next=$((i + 1))
      [[ $next -le $# ]] || { echo "update.sh: --tag requires a vYYYY.MM.DD.N value" >&2; exit 1; }
      req_tag="${!next}"
      i=$next
      ;;
  esac
done

build="$(mktemp -d /tmp/pages-update-XXXXXX)"
chmod 0700 "$build"

if [[ "$from_source" == "1" ]]; then
  # shellcheck source=scripts/lib/go-toolchain.sh
  . "$root/scripts/lib/go-toolchain.sh"
  pages_ensure_go "$root"
  echo "Building the updater from source"
  if ! make -C "$root" --no-print-directory build BIN="$build/pages" >/dev/null; then
    rm -rf "$build"
    echo "build failed — installed service untouched" >&2
    exit 1
  fi
  exec env PAGES_UPDATE_SHIM_DIR="$build" PAGES_FROM_SOURCE=1 PAGES_SRC_DIR="${PAGES_SRC_DIR:-$root}" "$build/pages" update "$@"
fi

# Signed release mode: no Go or make required on the host.
# shellcheck source=scripts/lib/release.sh
. "$root/scripts/lib/release.sh"

for tool in git curl sha256sum awk; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    if command -v dnf >/dev/null 2>&1; then
      dnf install -y git curl ca-certificates coreutils gawk >/dev/null
      break
    fi
  fi
done

if [[ "${PAGES_UPDATE_NO_PULL:-0}" != "1" ]]; then
  echo "Fetching release tags from origin"
  git -C "$root" fetch --quiet --force --tags origin
fi

tag="$req_tag"
if [[ -z "$tag" ]]; then
  tag="$(pages_release_latest_tag)" || { rm -rf "$build"; exit 1; }
fi

commit="$(git -C "$root" rev-parse --verify "refs/tags/${tag}^{commit}" 2>/dev/null)" || {
  rm -rf "$build"
  echo "release tag ${tag} is not present in ${root}" >&2
  exit 1
}

cache="${PAGES_COSIGN_CACHE_DIR:-/var/cache/pages-cosign}"
stage="$build/stage"
echo "Downloading and verifying signed release ${tag}"
bin="$(pages_release_fetch "$root" "$tag" "$stage" "$cache" "$commit")" || {
  rm -rf "$build"
  echo "release verification failed — installed service untouched" >&2
  exit 1
}
install -m 0755 "$bin" "$build/pages"

# pages update removes PAGES_UPDATE_SHIM_DIR once it has activated its release.
exec env \
  PAGES_UPDATE_SHIM_DIR="$build" \
  PAGES_RELEASE_TAG="$tag" \
  PAGES_ALLOW_DOWNGRADE="$allow_downgrade" \
  PAGES_VERIFIED_STAGE_DIR="$stage" \
  PAGES_SRC_DIR="${PAGES_SRC_DIR:-$root}" \
  "$build/pages" update "$@"
