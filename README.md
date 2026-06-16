# pages

Quick client pages and dashboards — versioned, Flag-themed, password-protected HTML
served on one server, deployable by our agents (chat, cutlass) via MCP/API.

See **[PLAN.md](PLAN.md)** for the full design (architecture, security model, version
control, theming, deployment) and **[docs/API.md](docs/API.md)** for the agent guide.

## Status: Phase 1 complete (core platform working end to end)

A single Node/Express process serves **two registrable domains** (the trust split —
see PLAN.md §7):

- **dashboard host** — trusted auth zone: `/admin/<slug>`, `/view/<slug>`, `/api/v1`,
  `/mcp`. Verifies the Elcano SSO cookie; `@elcanotek.com` auto-admins.
- **content host** — a *separate* registrable domain: serves the live client pages
  directly (`/<slug>`) plus `/raw` (preview) and `/assets`. Untrusted agent HTML,
  sandboxed (CSP `sandbox` header), never touching the SSO cookie. Holds only its own
  page-session cookie (separate jar).

What works today: the version state machine (deploy/publish/rollback/approve/reject,
pointer-is-truth, append-only, audit log); the **bearer-auth REST API** (`/api/v1`)
and **MCP-over-HTTP** (`/mcp`) agent surfaces; the Flag-themed **`/admin` shell**
(review queue, preview, publish/rollback/approve/reject/disable/approval/theme) on an
admin-cookie+CSRF path; the client **`/view`** direct-serve with a per-page password
gate; signed `/raw` rendering; `sync-flag.sh`; and the bootstrap/update/systemd/Caddy
deploy skeleton. Run it all locally with `sudo bash scripts/dev.sh`. Remaining: MCP
registration in chat/cutlass (config-only, see `docs/INTEGRATION.md`), Postgres in
`bootstrap.sh`, and Phase 4 source editing.

## Local dev

Fastest path — one command stands up Postgres, migrates, vendors Flag, mints an
agent token, and boots the server (the "let Claude Code deploy pages" loop):

```bash
npm install
sudo bash scripts/dev.sh                      # server on :3099, both vhosts → localhost
# in another shell:
TOKEN=$(sudo bash scripts/dev.sh token)
curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"slug":"demo","title":"Demo"}' http://127.0.0.1:3099/api/v1/pages
# deploy+publish HTML, then it renders at  Host: content.localhost  /raw/demo?t=<signed>
```

Or wire it up by hand:

```bash
npm install
cp .env.example .env          # set AUTH_SIGNING_PUBKEY to test the admin gate
# Treat localhost as the content host for testing the cookieless zone:
DASHBOARD_HOST=localhost CONTENT_HOST=content.localhost CONTENT_HOST_ALSO=content.localhost \
  npm start
# dashboard:  curl -H 'Host: localhost'        http://127.0.0.1:3002/healthz
# content:    curl -H 'Host: content.localhost' http://127.0.0.1:3002/raw/demo -i
```

## Deploy (Fedora/RHEL 9+)

```bash
sudo bash scripts/bootstrap.sh   # installs Node, user, systemd, Caddy (both hosts)
pages update                     # later: git pull + rebuild + restart
pages logs | status | env | tls
```
