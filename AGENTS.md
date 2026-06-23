# Pages

## Push policy (strict)

- **pages** repo: direct push to `main` is OK.
- **cutlass** repo: NEVER push to `dev` or `main`. Push to a separate feature branch and open a PR. Canonical branch is `dev`.
- **chat** repo: NEVER push to `main` or `dev`. Push to a separate feature branch and open a PR. Canonical branch is `main`.

Both cutlass and chat have GitHub branch protection — direct pushes to `dev`/`main` are rejected. Don't attempt them.

## Dev / test

- `sudo bash scripts/dev.sh` — one-command local dev (Postgres + migrate + Flag vendor + token + server on :3099)
- To enable the compose (Cutlass) panel: `PAGES_COMPOSE=1 CUTLASS_BIN=<path> CUTLASS_DIR=<path> bash scripts/dev.sh` (dev-only, never prod)
- `npm test` — unit tests (no DB)
- `bash test/run-integration.sh` — full integration suite (API, MCP, admin, view)
