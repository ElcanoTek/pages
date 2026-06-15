# pages

Quick client pages and dashboards — versioned, Flag-themed, password-protected HTML
served on one server, deployable by our agents (chat, cutlass) via MCP/API.

See **[PLAN.md](PLAN.md)** for the full design (architecture, security model, version
control, theming, deployment) and **[docs/API.md](docs/API.md)** for the agent guide.

## Status: Phase 0 (scaffold)

A single Node/Express process serves **two registrable domains** (the trust split —
see PLAN.md §7):

- **dashboard host** — trusted auth zone: `/view/<slug>`, `/admin/<slug>`, `/api/v1`,
  `/mcp`. Verifies the Elcano SSO cookie; `@elcanotek.com` auto-admins.
- **content host** — a *separate* registrable domain, cookieless: `/raw`, `/assets`.
  Renders untrusted agent HTML, sandboxed, never touching the SSO cookie.

What works today: the vhost split, Elcano auth gating (`/admin` → 302 to auth when
logged out, 403 for non-staff), per-zone CSP (strict sandbox headers on `/raw`),
`/healthz`, and the bootstrap/update/systemd/Caddy deploy skeleton. The feature
routes (`/view`, `/admin`, `/api/v1`, `/mcp`, `/raw` rendering) are stubbed `501`
pending Phases 1–4.

## Local dev

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
