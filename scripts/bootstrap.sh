#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/bootstrap.sh — install (or reconfigure) Elcano Pages on this box.
#
# Pages is one Go binary; this bridge only exists because the installer must
# run before that binary does. By default it fetches the latest signed release
# tag from origin, verifies SHA256SUMS, release.json and pages-linux-${arch}
# with the cosign pinned in cosign.lock, checks out the release tag, and runs
# `pages bootstrap` — without needing Go or make on the box.
# Pass `--from-source` (or `PAGES_FROM_SOURCE=1`) to build from the local
# checkout instead.
#
# Usage:    sudo bash scripts/bootstrap.sh [--tag vYYYY.MM.DD.N | --from-source]
# Scripted: PAGES_BOOTSTRAP_NON_INTERACTIVE=1 + AUTH_SIGNING_PUBKEY +
#           PAGES_BOOTSTRAP_DASHBOARD_HOST + PAGES_BOOTSTRAP_CONTENT_HOST
#           (see pages bootstrap --help)
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
[[ $EUID -eq 0 ]] || { echo "run as root: sudo bash scripts/bootstrap.sh" >&2; exit 1; }
[[ -f "$root/go.mod" && -d "$root/cmd/pages" ]] || { echo "not an Elcano Pages checkout at $root" >&2; exit 1; }

from_source="${PAGES_FROM_SOURCE:-0}"
req_tag="${PAGES_RELEASE_TAG:-}"
forward=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --from-source)
      from_source=1
      shift
      ;;
    --tag=*)
      req_tag="${1#--tag=}"
      shift
      ;;
    --tag)
      [[ $# -ge 2 ]] || { echo "bootstrap.sh: --tag requires a vYYYY.MM.DD.N value" >&2; exit 1; }
      req_tag="$2"
      shift 2
      ;;
    *)
      forward+=("$1")
      shift
      ;;
  esac
done

build="$(mktemp -d /tmp/pages-bootstrap-XXXXXX)"
chmod 0700 "$build"
trap 'rm -rf "$build"' EXIT

if [[ "$from_source" == "1" ]]; then
  # shellcheck source=scripts/lib/go-toolchain.sh
  . "$root/scripts/lib/go-toolchain.sh"
  pages_ensure_go "$root"
  echo "» building pages from $root"
  make -C "$root" --no-print-directory build BIN="$build/pages" >/dev/null
  PAGES_FROM_SOURCE=1 SRC_DIR="$root" "$build/pages" bootstrap "${forward[@]}"
  exit 0
fi

# Signed release mode: no Go or make required on the box.
# shellcheck source=scripts/lib/release.sh
. "$root/scripts/lib/release.sh"

missing=()
for tool in git curl; do
  command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if (( ${#missing[@]} )) && command -v dnf >/dev/null 2>&1; then
  dnf install -y "${missing[@]}" ca-certificates >/dev/null
fi
command -v git >/dev/null 2>&1 || { echo "git is required" >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 1; }

if [[ "${PAGES_BOOTSTRAP_NO_FETCH:-0}" != "1" ]]; then
  echo "» fetching release tags from origin"
  git -C "$root" fetch --quiet --force --tags origin
fi

tag="$req_tag"
if [[ -z "$tag" ]]; then
  tag="$(pages_release_latest_tag)"
fi
commit="$(git -C "$root" rev-parse --verify "refs/tags/${tag}^{commit}" 2>/dev/null)" \
  || { echo "release tag ${tag} is not present in ${root}" >&2; exit 1; }

cache="${PAGES_COSIGN_CACHE_DIR:-/var/cache/pages-cosign}"
echo "» downloading and verifying signed release ${tag}"
bin="$(pages_release_fetch "$root" "$tag" "$build/stage" "$cache" "$commit")"
if [[ "${PAGES_BOOTSTRAP_NO_CHECKOUT:-0}" != "1" ]]; then
  git -C "$root" checkout --quiet --detach "refs/tags/${tag}"
fi
install -m 0755 "$bin" "$build/pages"

# bootstrap copies the binary into the release it installs, so the build
# directory can go when it returns. Prompts read the TTY even when stdin is a
# pipe.
SRC_DIR="$root" "$build/pages" bootstrap "${forward[@]}"
