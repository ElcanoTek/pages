#!/usr/bin/env bash
# scripts/sync-flag.sh — vendor the Flag design system into public/assets/flag
# so the content host can serve tokens/fonts/icons/theme controller locally
# (CSP-clean, no external CDNs). Run by bootstrap.sh and update.sh.
#
# Source resolution (first that exists wins):
#   1. $FLAG_SRC               — a local checkout's design-system/ dir
#   2. a sibling ../flag/design-system (dev convenience)
#   3. git clone $FLAG_REPO (default ElcanoTek/flag) into a temp dir
#
# Pins to $FLAG_REF (default: main) when cloning.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/public/assets/flag"
FLAG_REPO="${FLAG_REPO:-https://github.com/ElcanoTek/flag.git}"
FLAG_REF="${FLAG_REF:-main}"

resolve_src() {
  if [[ -n "${FLAG_SRC:-}" && -d "$FLAG_SRC/tokens" ]]; then echo "$FLAG_SRC"; return; fi
  if [[ -d "$ROOT/../flag/design-system/tokens" ]]; then echo "$ROOT/../flag/design-system"; return; fi
  if [[ -d "/root/flag/design-system/tokens" ]]; then echo "/root/flag/design-system"; return; fi
  return 1
}

if src="$(resolve_src)"; then
  echo "sync-flag: using local source $src"
else
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  echo "sync-flag: cloning $FLAG_REPO ($FLAG_REF)"
  git clone --depth 1 --branch "$FLAG_REF" "$FLAG_REPO" "$tmp/flag" >/dev/null 2>&1 \
    || git clone --depth 1 "$FLAG_REPO" "$tmp/flag" >/dev/null
  src="$tmp/flag/design-system"
  [[ -d "$src/tokens" ]] || { echo "sync-flag: $FLAG_REPO has no design-system/tokens" >&2; exit 1; }
fi

mkdir -p "$DEST"
# Only the runtime assets the content host serves — not the docs/preview.
for sub in tokens fonts icons theme; do
  if [[ -d "$src/$sub" ]]; then
    rsync -a --delete "$src/$sub/" "$DEST/$sub/"
  fi
done
echo "sync-flag: vendored tokens/fonts/icons/theme → public/assets/flag"
