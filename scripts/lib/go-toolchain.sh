# shellcheck shell=bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# scripts/lib/go-toolchain.sh — make sure this box can build pages.
#
# Sourced by scripts/bootstrap.sh and scripts/update.sh (the bash bridges that
# run before the pages binary exists). The Makefile sets GOTOOLCHAIN=auto, so
# any Go >= 1.21 builds the tree: it downloads go.mod's exact pin itself.
# Fedora/RHEL's golang is normally new enough for that; when it is missing or
# older, the official toolchain go.mod names is fetched from go.dev, checked
# against its published SHA-256, and unpacked to /usr/local/go.

pages_go_ok() {
  command -v go >/dev/null 2>&1 || return 1
  local v
  v="$(go env GOVERSION 2>/dev/null || true)"   # go1.22.5
  [[ "$v" =~ ^go1\.([0-9]+) ]] || return 1
  (( BASH_REMATCH[1] >= 21 ))
}

# pages_fetch_go ROOT — install go.mod's toolchain from go.dev.
pages_fetch_go() {
  local root="$1" version arch url sum tmp
  version="$(awk '$1 == "go" { print $2; exit }' "$root/go.mod")"
  [[ "$version" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || { echo "cannot read the Go version from $root/go.mod" >&2; return 1; }
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported architecture $(uname -m) for the official Go toolchain" >&2; return 1 ;;
  esac
  url="https://dl.google.com/go/go${version}.linux-${arch}.tar.gz"
  tmp="$(mktemp -d)"
  echo "» fetching Go ${version} (${arch}) from go.dev" >&2
  if ! curl -fsSL --retry 3 -o "$tmp/go.tgz" "$url" || ! sum="$(curl -fsSL --retry 3 "$url.sha256")"; then
    rm -rf "$tmp"; echo "could not download $url" >&2; return 1
  fi
  if [[ "$(sha256sum "$tmp/go.tgz" | awk '{print $1}')" != "${sum%% *}" ]]; then
    rm -rf "$tmp"; echo "Go toolchain checksum mismatch for $url" >&2; return 1
  fi
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmp/go.tgz"
  rm -rf "$tmp"
  export PATH="/usr/local/go/bin:$PATH"
}

# pages_ensure_go ROOT — git, make and a usable go on PATH (root only).
pages_ensure_go() {
  local root="$1"
  [[ -x /usr/local/go/bin/go ]] && export PATH="/usr/local/go/bin:$PATH"
  local missing=()
  command -v git >/dev/null 2>&1 || missing+=(git)
  command -v make >/dev/null 2>&1 || missing+=(make)
  pages_go_ok || missing+=(golang)
  if (( ${#missing[@]} )) && command -v dnf >/dev/null 2>&1; then
    dnf install -y "${missing[@]}" ca-certificates curl >/dev/null
  fi
  command -v git >/dev/null 2>&1 || { echo "git is required" >&2; return 1; }
  command -v make >/dev/null 2>&1 || { echo "make is required" >&2; return 1; }
  pages_go_ok || pages_fetch_go "$root" || return 1
  pages_go_ok || { echo "no usable Go toolchain (need go >= 1.21 on PATH)" >&2; return 1; }
  export GOTOOLCHAIN=auto
}
