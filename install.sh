#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
# Public entry point. Bash parses the complete function before installation;
# a truncated download cannot execute a partial function. Prompts use the TTY.
#
# Installs git and curl, clones main into /opt/pages-src, then
# scripts/bootstrap.sh fetches and verifies the latest signed release with
# cosign (or builds from source when PAGES_FROM_SOURCE=1) and runs
# `pages bootstrap`.
set -euo pipefail
main() {
  if [[ "${1:-}" == --help ]]; then
    echo 'Usage: sudo bash install.sh [--help] (installs main into /opt/pages-src)'
    exit 0
  fi
  [[ $# == 0 ]] || { echo 'Unknown argument; try --help' >&2; exit 2; }
  [[ $EUID == 0 ]] || { echo 'Run with sudo bash install.sh' >&2; exit 1; }
  command -v dnf >/dev/null || { echo 'Fedora/RHEL with dnf is required' >&2; exit 1; }
  local src="${PAGES_SRC_DIR:-/opt/pages-src}"
  if [[ -e "$src" ]]; then
    echo "$src already exists. Use pages update, or run its scripts/bootstrap.sh to reconfigure." >&2
    exit 1
  fi
  local pkgs=(git ca-certificates curl)
  if [[ "${PAGES_FROM_SOURCE:-0}" == "1" ]]; then
    pkgs+=(make golang)
  fi
  dnf install -y "${pkgs[@]}"
  git clone --branch main --single-branch https://github.com/ElcanoTek/pages.git "$src"
  exec bash "$src/scripts/bootstrap.sh"
}
main "$@"
