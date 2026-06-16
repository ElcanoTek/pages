# Pages — Handoff (where this is left off)

> ## ⚠️ DESIGN UPDATE (2026-06-15, Brad) — drop the iframe
> **Don't use an iframe for `/view`.** Instead of the dashboard shell embedding the
> content host in an `<iframe sandbox>`, serve user pages **directly** on the content
> domain and keep the admin/dashboard on its own domain:
> - **`elcano-pages.com`** → user-facing pages (the content itself, served directly).
> - **`pages.elcanotek.com`** → admin / dashboard stuff.
> - **Both still point at the same box** (one process, two vhosts — same as today).
>   Just no iframes.
>
> **An iframe as a preview is still fine.** The admin shell (`/admin/:slug`, Phase 1
> task #4) can keep using a sandboxed iframe to preview a version, and invariant #2's
> "iframe is a preview only" rule stands there. What changes is the **user-facing**
> path: this supersedes Phase 1 task #3 (`/view/:slug` iframing `/raw/...`) — the live
> page is served directly on `elcano-pages.com`, not iframed.
>
> The two-registrable-domain split (invariant #1) is unchanged and still load-bearing —
> untrusted content stays on `elcano-pages.com`, trusted shell/API on
> `pages.elcanotek.com`. We're only removing the iframe from the live user view, not the
> origin separation. Revisit how `/raw` token auth + the page-password gate work when the
> live content is served directly rather than through a signed-token iframe src.

**TL;DR:** Phase 0 (scaffold) and the Phase 1 *data + render foundation* are done and
tested. What remains is the Phase 1 *user-facing half*: the version-mutation state
machine, the `/view` and `/admin` shells, the per-page password, and adding Postgres
to `bootstrap.sh`. Read **[PLAN.md](PLAN.md)** first — it's the full design and the
source of truth for every decision below.

## Read these first, in order
1. `PLAN.md` — architecture, security model, version control, theming, deployment,
   API/MCP. Every "why" lives here.
2. `HANDOFF.md` (this file) — status + the next tasks.
3. `docs/API.md` — the agent-facing guide (stub; fill in during Phase 3).

## The invariants you must NOT break (these were hard-won — see PLAN §7, §5, §8)
1. **Untrusted content lives on a separate registrable domain.** `pages.elcanotek.com`
   = trusted shell/API; `elcano-pages.com` = cookieless content host serving `/raw`.
   Never render agent HTML on the dashboard origin. The split is enforced in
   `server.js` by `Host` header.
2. **Edit the stored *source*, never the rendered DOM.** The iframe is a preview only.
   No `contenteditable`/`outerHTML` round-trip, no GrapesJS — they corrupt chart
   markup. Editing = patch source → new draft → re-render.
3. **Pointer-is-truth.** "Live" = `pages.published_version_id`. Don't add a `published`
   status. publish/approve/rollback are one `UPDATE` under `SELECT … FOR UPDATE`.
4. **`page_versions` content is append-only** (DB trigger enforces it). Every edit is a
   new row. Only `status`/`reviewed_*` mutate.
5. **`/raw` is authorized only by the signed token** (`lib/rawtoken.js`), bound to
   `{pid, vid, purpose, mode, exp, sid}`, constant-time verified. No cookies on that
   host. Set the sandbox CSP headers on *every* `/raw` response (incl. errors).
6. **Agents never approve/disable/toggle-approval** — those are admin-cookie+CSRF only.

## What's DONE and TESTED

### Phase 0 — scaffold (runnable)
- `server.js` — one process, two vhosts (dashboard auth zone / cookieless content
  zone), `/healthz`, auth-gated `/api/me` + `/admin`, feature routes stubbed `501`.
- `lib/auth.js` — Elcano Ed25519 cookie verification copied verbatim from `home`,
  plus `isElcanoAdmin`/`requireAdmin`. **Tested**: valid admin passes, non-staff 403,
  expired/forged cookies bounce to auth.
- `lib/csp.js` — un-frameable shell CSP vs. the `sandbox`+`default-src 'none'`+
  `connect-src 'none'` content CSP. Origins derive from `DASHBOARD_HOST`/`CONTENT_HOST`.
- Deploy: `scripts/bootstrap.sh` (two hostnames, warns on shared eTLD+1),
  `scripts/update.sh` (staging → atomic swap → healthz), `deploy/pages.service`,
  two-vhost `deploy/pages.caddy`, `deploy/pages-cli` (`/usr/local/bin/pages`).

### Phase 1 — data + render foundation (runnable, DB-backed)
- `migrations/001_init.sql` — full schema (PLAN §5): pointer-is-truth, statuses
  `draft|pending|approved|rejected`, **append-only trigger**, themes/assets/
  api_tokens/preview_links/audit_log. Seeds the `flag` theme.
- `lib/db.js` (pg pool, lazy), `lib/migrate.js` (ordered, idempotent runner).
- `lib/rawtoken.js` — mint/verify signed `/raw` tokens.
- `lib/render.js` — `themed` Flag injection (`[data-flag-injected]`) + `raw` verbatim.
- `scripts/sync-flag.sh` — vendors Flag tokens/fonts/icons/theme into
  `public/assets/flag/` (gitignored; pulled at build). Source: `$FLAG_SRC`, a sibling
  `../flag`, or `git clone $FLAG_REPO` (default `ElcanoTek/flag`).
- `/raw/:slug` in `server.js` — verifies token → `db.getRenderable` → renders.

### Phase 1 keystone + Phase 2 REST agent slice — DONE & TESTED (2026-06-16)
The version state machine and the bearer-authenticated agent API now exist, so
agents (and Claude Code) can deploy pages locally end-to-end.
- `lib/versions.js` — the mutation state machine (PLAN §5): `createPage`, `deploy`,
  `publish`, `rollback`, `approve`, `reject` + reads. Each mutation: `SELECT … FOR
  UPDATE` on the page first, content-sha **dedupe**, `expected_version` → **409**
  optimistic concurrency, **pointer-is-truth** moves, and an `audit_log` row in the
  SAME txn. Agent authority is gated: open pages → agent may publish/rollback;
  approval-gated → agent gets `pending` only; approve/reject are admin-only.
- `lib/audit.js` (txn-coupled writes), `lib/apierror.js` (status-carrying errors),
  `db.withTransaction`.
- `lib/tokens.js` — agent API bearer tokens: `pgs_…`, stored as
  `HMAC-SHA256(token, API_TOKEN_PEPPER)`, raw shown once; `requireBearer` middleware.
  `scripts/token.js add|list|revoke` mints them locally.
- `lib/api.js` — `/api/v1` (bearer-only): list/create pages, deploy version, publish,
  rollback, version history/detail. Wired in `server.js` (replaces the 501 stub).
  Admin-cookie+CSRF endpoints (approve/reject/disable/approval-toggle) intentionally
  NOT here yet — they arrive with the `/admin` shell.
- `scripts/dev.sh` — one command: throwaway Postgres (under `/var/tmp/pages-dev`),
  migrate, vendor Flag, mint a token (saved to `.devdata/agent-token`), boot the
  server. This IS the "let Claude Code deploy pages" loop.

### Tests (all green)
- `npm test` → `test/unit.test.js`: 11 tests (token tamper/escalation/expiry; render
  injection preserves charts; theme override; head synthesis; sha + slug validation).
- `bash test/run-integration.sh` → spins a throwaway Postgres, migrates, vendors Flag,
  seeds, asserts the append-only trigger, drives `/raw` end-to-end (themed render 200
  sandboxed+Flag-injected+charts intact, no-token 403, wrong-slug 404, disabled 404),
  AND the full REST agent loop (`test/api.integration.js`): unauth 401, create,
  deploy+publish, pointer-is-truth, /raw render, second deploy moves pointer, history,
  rollback, dedupe, 409 stale-version, approval-gate (agent→pending, publish 403).
  (Requires `postgres` system user + `initdb`.)

## What's LEFT (your next tasks)
`lib/versions.js` (was task #1) is **done** — see above. Remaining, in order; each is
independently testable. Note the **direct-serve** design update at the top of this
file changes how #3 works.

1. **MCP-over-HTTP at `/mcp`** (PLAN §11; was Phase 3, pull forward — it's just a thin
   wrapper). Cutlass's contract (verified in `/root/cutlass`): JSON-RPC 2.0 over
   `POST /mcp`, protocol `"2024-11-05"`, methods `initialize`/`tools/list`/`tools/call`,
   `Authorization: Bearer`, response as JSON **or** SSE, optional `Mcp-Session-Id`.
   Wrap the SAME `lib/versions.js` functions as the REST API (no privileged backdoor).
   Tools: `list_pages`, `get_page`, `deploy_page`, `publish_page`, `rollback_page`,
   `list_versions`. Register in cutlass via a ~4-line `getMCPServerDefinitions()` block
   mirroring `fast_io` (`config.go`), gated on `PAGES_API_TOKEN`. Test by POSTing
   JSON-RPC at `/mcp` directly — no cutlass needed.
2. **`lib/pagecookie.js`** — signed per-page client password session
   (`page_<slug>` cookie, HMAC `PAGE_COOKIE_SECRET`, ~30d). bcrypt for `password_hash`.
3. **`/view/:slug`** (dashboard host) — Elcano admin cookie OR valid page cookie OR
   password form. On access: mint a `view` token for the published version and return
   a shell that iframes `https://<CONTENT_HOST>/raw/<slug>?t=…`
   (`sandbox="allow-scripts"`, `referrerpolicy="no-referrer"`). `password_hash NULL`
   = Elcano-only.
4. **`/admin/:slug`** (`requireAdmin`) — Flag-themed shell: version list w/ the
   `pending` review queue at top; sandboxed preview of any version; buttons for
   publish / rollback / approve / reject; `require_approval` + `disable` toggles;
   theme picker. All mutations go through `/api/v1` (Phase 2) with a CSRF token — or,
   if you wire them directly now, keep them admin-cookie+CSRF and audit-logged.
   Build shells with Flag tokens/components (`public/assets/flag`).
5. **`bootstrap.sh` — add Postgres** (mirror chat/moc §9): install
   `postgresql postgresql-server`, `initdb`, `pg_hba` loopback → `scram-sha-256`,
   create role/db `pages`, write `DATABASE_URL`, run `node lib/migrate.js`, run
   `sync-flag.sh`. Add `After/Requires=postgresql.service` to `pages.service` (already
   commented in). Then `update.sh` should run migrations in staging before the swap.

## Then Phases 2–5 (PLAN §12)
- **2** — REST API `/api/v1/*` + token auth + CSRF + Origin checks + rate limits +
  body caps + signed preview links + backup/`check-integrity`.
- **3** — MCP-over-HTTP at `/mcp`; register in chat (`buildMCPSpecs()`) and cutlass
  (`getMCPServerDefinitions()`); write `docs/API.md`.
- **4** — source editing: raw-source textarea + live preview, then the `postMessage`
  text-node patch ("click the number").
- **5** — asset GC, multi-page UI, `raw` mode if needed.

## Run it locally
```bash
npm install
npm test                                   # unit tests (no DB)
bash test/run-integration.sh               # full loop vs throwaway Postgres

# boot the app (two vhosts on one port; treat localhost as both):
cp .env.example .env                        # set AUTH_SIGNING_PUBKEY to test admin gate
DASHBOARD_HOST=localhost CONTENT_HOST=content.localhost CONTENT_HOST_ALSO=content.localhost \
  RAW_TOKEN_SECRET=dev DATABASE_URL=postgres://… npm start
```

## Open items needing Brad / the auth owner (PLAN §13)
- Content domain `elcano-pages.com` registered (A record, grey-cloud on Cloudflare);
  `pages.elcanotek.com` needs to resolve to the box before bootstrap so Caddy can
  issue both certs.
- Optional: migrate the SSO cookie to `__Host-` estate-wide (defense in depth; Pages
  is not blocked on it).
