#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
#
# SessionStart hook for Claude Code cloud sessions: installs what `make ci`,
# the front-end tests, the Playwright suite and the database suites need.
# Synchronous, idempotent and non-interactive; a no-op outside the cloud.
set -euo pipefail

[ "${CLAUDE_CODE_REMOTE:-}" = "true" ] || exit 0

cd "${CLAUDE_PROJECT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

# CI's golangci-lint pin (.github/workflows/ci.yml). Built with the module's own
# toolchain, because a golangci-lint built with an older Go cannot load packages
# that declare a newer `go` directive.
GOLANGCI_LINT_VERSION=v2.15.0

export GOTOOLCHAIN=auto
toolchain="$(sed -n 's/^toolchain //p' go.mod)"
toolchain="${toolchain:-go$(sed -n 's/^go //p' go.mod)}"

# Go: fetch the pinned toolchain and every module.
go mod download

# golangci-lint at CI's version, into GOPATH/bin.
gobin="$(GOTOOLCHAIN="$toolchain" go env GOPATH)/bin"
if ! "$gobin/golangci-lint" version 2>/dev/null | grep -q "version ${GOLANGCI_LINT_VERSION#v} built with ${toolchain}"; then
  GOTOOLCHAIN="$toolchain" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$GOLANGCI_LINT_VERSION"
fi

# npm: test tooling only (Playwright, axe). `npm install`, not `npm ci`, so a
# cached node_modules survives. Browsers are preinstalled in the cloud image
# (PLAYWRIGHT_BROWSERS_PATH); never `playwright install` here.
npm install --no-audit --no-fund --loglevel=error

# The image's Chromium may not be the build this Playwright pins;
# playwright.config.js launches PLAYWRIGHT_CHROMIUM_EXECUTABLE when set.
chromium=""
for c in "${PLAYWRIGHT_BROWSERS_PATH:-/opt/pw-browsers}"/chromium-*/chrome-linux/chrome; do
  [ -x "$c" ] && chromium="$c"
done

if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
  {
    echo "export PATH=\"$gobin:\$PATH\""
    echo "export GOTOOLCHAIN=auto"
    [ -n "$chromium" ] && echo "export PLAYWRIGHT_CHROMIUM_EXECUTABLE=\"$chromium\""
  } >> "$CLAUDE_ENV_FILE"
fi

# A throwaway PostgreSQL 16 for the database suites (make test-db), when the
# server tools are present. Under /var/tmp because, as root, the cluster runs
# as the postgres account, which cannot reach a private home directory.
if [ -x /usr/lib/postgresql/16/bin/pg_ctl ] || command -v pg_ctl >/dev/null 2>&1; then
  make test-db TEST_PG_DIR=/var/tmp/pages-testpg || echo "session-start: could not start the test Postgres; database suites will skip" >&2
fi
