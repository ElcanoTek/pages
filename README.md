# Pages

Pages is a small, self-hosted platform for **versioned, access-controlled client
dashboards**. AI agents and humans deploy HTML through a REST or MCP interface;
Pages versions it, renders it inside a hard security boundary, puts it behind a
password, and lets a human review, publish or roll back any version at any time.

It exists for teams whose agents generate client-facing reporting — an agency, a
consultancy, an analytics team — and who need somewhere to put that output that
is not a shared drive, does not require a front-end deploy per client, and does
not trust the generated HTML.

![The Pages admin dashboard, light mode: the "Client pages" index listing ten client dashboards with synthetic company names, each row showing its live, draft or approval-gated status, live version number, last update time and workspace, beside a workspace sidebar and a page-count summary.](docs/images/admin-dashboard.png)

## Features

- **Versioned by default.** Every deploy appends an immutable version.
  "Live" is a pointer, so publishing and rolling back are a pointer move, not a
  redeploy. Version content is append-only, enforced by a database trigger.
- **Two-host trust split.** Untrusted, agent-authored HTML is served from a
  *different registrable domain* than the admin shell and the API. It runs
  under a strict sandbox CSP on an origin with no access to a staff session.
- **Access control per dashboard.** A page is staff-only until you give it a
  client password. Partner portals put one shared credential over a curated set
  of dashboards, with live membership as the revocation mechanism.
- **A human review gate, optional per page.** Turn on the approval gate and an
  agent can only queue a version; publishing needs a person.
- **Two agent surfaces, one state machine.** A bearer-token REST API
  (`/api/v1`) and MCP-over-HTTP (`/mcp`) exposing typed tools. Both route
  through the same code as the admin UI — there is no privileged backdoor.
- **Templates.** Store one design; the second dashboard in a family costs only
  the config that differs, not another copy of the design.
- **Data-only updates.** Refresh a dashboard's numbers against a stored JSON
  Schema without re-transmitting or re-storing its HTML.
- **Large-document paths that don't burn context.** Durable staged uploads,
  out-of-band upload tickets, and anchored server-side patching, so a one-line
  fix costs anchors rather than two copies of a dashboard.
- **Deploy-time preflight.** Statically checks a document against the exact CSP
  and sandbox it will be served under. Advisory, never a gate.
- **Flag theming.** Deployed HTML can be served verbatim (`raw`) or wrapped in
  the Flag design system (`themed`), injected at render time and never
  persisted into the stored source.
- **A full audit trail.** Every mutation writes an `audit_log` row in the same
  transaction as the change.
- **One process, one box.** Node/Express and PostgreSQL, behind Caddy, under
  systemd. No build step, no cloud dependency.

## Quick start

You need **Node 18+** (CI runs 20) and PostgreSQL **server** tools — `initdb`
and `pg_ctl`, not just `psql`.

```bash
git clone https://github.com/ElcanoTek/pages.git
cd pages
npm install
```

### Linux

One command stands up a self-contained Postgres cluster, runs migrations, mints
an agent API token, mints a throwaway SSO keypair for local admin login, and
boots the server in the foreground:

```bash
sudo bash scripts/dev.sh
```

### macOS

Against a Homebrew `postgresql@16` you already have running:

```bash
bash scripts/dev-macos.sh
```

Either way you get:

| | |
| --- | --- |
| Dashboard host | `http://localhost:3099` |
| Content host | `http://content.localhost:3099` |
| Admin UI | `http://localhost:3099/__dev/login?next=/admin` |
| Agent token | printed at startup, saved in `.devdata/agent-token` |

Root is needed on Linux because Postgres refuses to run as root, so the script
delegates the cluster to the `postgres` system user. If `initdb` is not on your
`PATH` — Debian and Ubuntu keep it in `/usr/lib/postgresql/<ver>/bin` — add it
first:

```bash
export PATH="/usr/lib/postgresql/16/bin:$PATH"
```

`bash scripts/dev.sh token` reprints the saved token without booting anything.
State lives in `.devdata/` (gitignored) and is shared between both scripts, so
the token survives restarts.

### Deploy a page as an agent

```bash
TOKEN=$(cat .devdata/agent-token)

curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"slug":"demo","title":"Demo dashboard"}' \
     http://127.0.0.1:3099/api/v1/pages

curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"html":"<!doctype html><html><body><h1>Hello</h1></body></html>","publish":true}' \
     http://127.0.0.1:3099/api/v1/pages/demo/versions
```

A page is **staff-only until you give it a client password** — the content host
answers a passwordless page with a "not shared yet" refusal (`403`), not the
dashboard. Give it one, and it becomes client-reachable behind a password gate:

```bash
curl -H "Host: localhost" -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"password":"demo-password"}' \
     http://127.0.0.1:3099/api/v1/pages/demo/password
```

Now the **content** host serves it — `401` with a password form until the form
is submitted, then a `303` and a page-session cookie scoped to that one page:

```bash
curl -i -H 'Host: content.localhost' http://127.0.0.1:3099/demo        # 401 + gate
curl -i -H 'Host: content.localhost' -d 'password=demo-password' \
     http://127.0.0.1:3099/demo                                        # 303 + Set-Cookie
```

As staff you can skip the password entirely through the broker on the dashboard
host, `http://localhost:3099/view/demo`, which is what the admin UI links to.

`/__dev/login` exists only when `PAGES_DEV_LOGIN=1`, which only the two dev
scripts set. It must never be set in production.

### Running the tests

```bash
npm test                       # unit tests, no database
npm run test:browser           # Playwright: admin shell + accessibility
bash test/run-integration.sh   # full suite against a throwaway Postgres
```

## Configuration

Pages is configured entirely through environment variables. In production they
live in `/etc/default/pages`, read by systemd. [`.env.example`](.env.example)
documents every variable the application reads; the table below is the subset
you cannot ignore.

| Variable | Required | Default | What it does |
| --- | --- | --- | --- |
| `DASHBOARD_HOST` | yes | `pages.elcanotek.com` | Trusted host: admin shell, `/view`, `/api/v1`, `/mcp` |
| `CONTENT_HOST` | yes | `elcano-pages.com` | Cookieless content host. **Must be a different registrable domain** |
| `PORT` | no | `3002` | Listen port, loopback behind a reverse proxy |
| `NODE_ENV` | production | — | Set to `production`: enables fail-closed secret checks, hides stack traces |
| `DATABASE_URL` | yes | libpq `PG*` fallback | PostgreSQL connection string |
| `AUTH_SIGNING_PUBKEY` | yes | — | Ed25519 **public** key (base64, 32 bytes) verifying the SSO cookie |
| `AUTH_LOGIN_URL` | no | `https://auth.elcanotek.com` | Where auth failures redirect |
| `ADMIN_EMAIL_DOMAIN` | no | `elcanotek.com` | Cookie emails in this domain become admins |
| `PAGE_COOKIE_SECRET` | yes | — | HMAC key for client-password sessions. Throws at startup if empty in production |
| `RAW_TOKEN_SECRET` | yes | — | HMAC key for signed `/raw` render tokens. Throws at startup if empty in production |
| `API_TOKEN_PEPPER` | yes | — | Pepper for hashing agent tokens. Throws at startup if empty in production. **Changing it invalidates every issued token** |
| `MCP_ALLOWED_ORIGINS` | no | — | Extra exact origins accepted at `/mcp` |
| `RL_API_PER_MIN` | no | `120` | Per-IP rate limit on `/api/v1` |
| `FLAG_ASSETS_BASE` | no | `/assets/flag` | Where the vendored Flag assets are served from |

`scripts/bootstrap.sh` generates the three secrets and writes the environment
file for you. The full reference — including upload bounds, schema-validation
limits, database timeouts and the development-only variables — is in
[docs/DEPLOYMENT.md §5](docs/DEPLOYMENT.md).

## Architecture

One Node/Express process, one PostgreSQL database, two hostnames.

### The two-host trust split

This is the load-bearing design decision, and getting it wrong removes the
security boundary rather than degrading it.

| Host | Trust | Serves |
| --- | --- | --- |
| **dashboard host** | Trusted auth zone. Verifies the SSO cookie. | `/admin`, `/view/<slug>`, `/api/v1`, `/mcp` |
| **content host** | Untrusted render zone. **Cookieless.** | `/<slug>` (live pages), `/raw` (preview), `/assets` |

The content host renders HTML that agents wrote, and that HTML runs scripts.
The safety argument is that it runs them on an origin with no access to the
staff session cookie, no access to the admin API, and no same-origin reach into
anything that has either. `server.js` branches on the `Host` header.

**The two hostnames must be different registrable domains** — different eTLD+1,
not two subdomains of one domain. Cookies are scoped by registrable domain, so
a script on `content.example.com` can write a cookie with
`Domain=.example.com` and have it sent to `pages.example.com`. That is cookie
tossing, and it would let agent HTML forge session state on the trusted host.
`SameSite` does not help; both names are the same site. So a deployment needs
**two registered domains**. `bootstrap.sh` warns if they share a parent, and
that warning should stop you.

The content host serves agent HTML under `sandbox allow-scripts allow-downloads
allow-modals`. `allow-same-origin` is never added: combined with
`allow-scripts` it is a documented sandbox escape and the single token that
would give agent HTML a real origin, storage and cookies.

### The version state machine

Versions are immutable rows and **the live version is a pointer**, not a status.

```
deploy ──▶ draft ──────────────▶ (publish) ──▶ pointer moves
                                       ▲
       on an approval-gated page:       │
deploy ──▶ pending ──▶ approve ────────┘
                  └──▶ reject
```

- `pages.published_version_id` is the only definition of "live". There is no
  `published` status to drift out of sync with it.
- `publish`, `rollback` and `approve` are each one `UPDATE` under `SELECT … FOR
  UPDATE`, so concurrent agents cannot interleave a pointer move.
- Version content is append-only, enforced by a database trigger: every edit is
  a new row, and only `status` and the review columns ever mutate. Rollback is
  therefore always available.
- `delete` is soft and reversible — the row, its versions and its audit trail
  survive, so a hallucinated or injected delete is recoverable.
- Every transition writes an `audit_log` row in the same transaction.
- Optimistic concurrency is available through `expected_version` on the
  mutating calls.

`lib/versions.js` is that state machine, and REST, MCP and the admin UI all
route through it.

### The agent surfaces

Both live on the dashboard host, both authenticate with a `pgs_…` bearer token
stored only as `HMAC-SHA256(token, pepper)`, and both share the state machine.

**REST** (`/api/v1`) — create a page, deploy a version, publish, roll back, set
a client password, rename, soft-delete, read the version list, run preflight.

**MCP-over-HTTP** (`/mcp`) — stateless Streamable HTTP with JSON responses,
built on the official MCP TypeScript SDK and tracking the protocol version that
SDK reports as current (a legacy transport path remains for older clients). It
exposes a typed tool catalog covering the same state machine plus workspace
discovery and organization, theme discovery, filtered cursor pagination,
optimistic-concurrency checks, schema-validated data-only dashboard updates,
durable staged uploads, out-of-band upload tickets, anchored server-side
patching, template lifecycle, read-only update-prompt preparation, and
preflight. Every tool is validated against a strict `inputSchema` before
dispatch, and carries an `outputSchema` and behavioral annotations.

Pages runs **no scheduler** and holds no model credentials. It owns the safe
update contract — versioning, validation, publishing, audit — and nothing else.
See [docs/API.md](docs/API.md) for the wire contract and the full tool catalog.

### Authority split

The split is deliberate, and it is what makes it safe to hand an agent a token.

| | Agents (bearer token) | Humans (admin cookie + CSRF) |
| --- | --- | --- |
| Create, deploy, publish, roll back | ✅ on open pages | ✅ |
| Set a client password | ✅ | ✅ |
| Rename, soft-delete | ✅ on open pages | ✅ |
| Workspaces | create/rename/assign | ✅ including delete |
| Approve / reject a version | ❌ | ✅ |
| Toggle the approval gate | ❌ | ✅ |
| Disable a page (takedown) | ❌ | ✅ |
| **Clear** a client password | ❌ | ✅ |
| Set a theme | ❌ | ✅ |
| Partner portals | ❌ no agent path at all | ✅ |

On an approval-gated page an agent can only queue a `pending` version. On a
disabled page an agent cannot publish, roll back or delete — which is what
stops a delete-then-recreate bypass of a takedown.

### Flag theming

`public/assets/flag/` holds the Flag design system — tokens, fonts, icons,
logos — committed in-repo, so there is no vendoring or build step. Two
self-hosted typefaces ship, and only two: **Nebula Sans** (SIL OFL 1.1) for UI,
body and headings, and **Hack** (MIT) for code, slugs and tabular output. One
sheet, `fonts/fonts.css`, declares both and binds them to `--font-brand` /
`--font-code-brand`; the token sheet reads those and never names a family, so
page CSS should use `var(--font-body)` / `var(--font-code)` rather than a face
name. Nothing is fetched from a CDN or Google Fonts — see
[`docs/AUTHORING.md`](docs/AUTHORING.md) for what a page may load. A version
deploys with a render mode:

- **`themed`** — Flag is injected at render time under `[data-flag-injected]`.
  The stored source is never rewritten, which is why charts survive edits.
- **`raw`** — served verbatim. Pages will not restyle the design.

Themes are curated server-side and selected by name, so agents pick a look
rather than injecting arbitrary CSS.

What a client sees is just the dashboard — the live page, served from the
content host, with no Pages chrome around it:

![A rendered client dashboard as served from the content host: the heading "Aurora Beverage holiday launch" over four KPI cards reading Impressions 18.4M, Spend $412K, Margin 31.8% and Pacing 103%, above a channel delivery table. All figures are invented demo data.](docs/images/example-dashboard.png)

## Security posture

Pages renders untrusted HTML for a living, so the model is worth reading before
you trust it with anything.

- **Origin isolation** is the primary boundary — a separate registrable domain
  for untrusted content, and a strict `sandbox` CSP on every response that
  renders it, errors included.
- **`/raw` is authorised only by a signed token**, bound to page, version,
  purpose, render mode, expiry and session, verified in constant time. Token
  purposes are disjoint audiences, so a 300-second preview URL cannot be
  exchanged for a page session.
- **Three ways to reach a live page without its own password**, each tightly
  bound: the `/view` broker (staff SSO only), a render token (one version, no
  exchange), and a partner-portal session (live membership re-read per request,
  which is the entire revocation mechanism).
- **Fail-closed secrets.** In `NODE_ENV=production`, an empty
  `PAGE_COOKIE_SECRET`, `RAW_TOKEN_SECRET` or `API_TOKEN_PEPPER` throws at
  startup rather than accepting forgeable tokens.
- **A header floor, not per-route habit.** The content host's security headers
  are applied as its first middleware, so every response it emits carries them
  — 404s, redirects, `/healthz`, and anything the error handler catches.
- **Bounded everywhere.** Per-IP rate limits, a per-page progressive
  brute-force backoff that delays rather than locks out, database
  connect/statement/lock timeouts, capped upload chunks, and CPU-bounded JSON
  Schema validation that rejects catastrophic-backtracking regex shapes at
  compile time.
- **Tokens are hashed with a server pepper** and independently revocable.
- **Everything is audited**, in the same transaction as the change.

[docs/SECURITY.md](docs/SECURITY.md) is the full threat model, including a
ranked list of the limitations we know about and have not closed yet. Read it.

To report a vulnerability, see [SECURITY.md](SECURITY.md) — email
security@elcanotek.com, not a public issue.

## Documentation

| Document | What it covers |
| --- | --- |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | The authoritative deployment guide: DNS, Postgres, bootstrap, TLS, systemd, tokens, migrations, updates, rollback, backup, troubleshooting |
| [docs/SECURITY.md](docs/SECURITY.md) | The threat model — what is enforced, why, and the known follow-ups |
| [docs/API.md](docs/API.md) | REST and MCP agent surfaces: the wire contract and full tool catalog |
| [docs/AUTHORING.md](docs/AUTHORING.md) | What actually works inside a published page. Every rule is a real response header |
| [docs/TEMPLATES.md](docs/TEMPLATES.md) | One stored design, many pages: templates, config and revisions |
| [docs/DATA_UPDATES.md](docs/DATA_UPDATES.md) | Data-only dashboard refreshes and update-prompt preparation |
| [docs/INTEGRATION.md](docs/INTEGRATION.md) | Connecting an MCP client to Pages |
| [docs/UI_COPY.md](docs/UI_COPY.md) | The admin UI's vocabulary — one word per thing |
| [docs/LICENSING.md](docs/LICENSING.md) | The licence in plain English: what you may and may not do |
| [PLAN.md](PLAN.md) | The original design record. The "why" behind most decisions, cited by section from the source |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Setup, tests, branch and PR conventions, and the invariants |

## Deployment

`scripts/bootstrap.sh` is an interactive installer for **Fedora / RHEL 9+**. It
installs Node and PostgreSQL, creates the service user and `/opt/pages`,
provisions the database, generates secrets, runs migrations, mints the initial
agent token, installs the systemd unit and the `pages` operator CLI, and sets up
Caddy with automatic TLS for both hostnames.

```bash
sudo git clone https://github.com/ElcanoTek/pages.git /opt/pages-src
cd /opt/pages-src
sudo bash scripts/bootstrap.sh

pages status | logs | env | tls
pages update            # git pull → staging build → migrate → swap → restart
pages token add <label> [scope] [slug…]
```

Two things to know before you start: you need **two registrable domains**
(above), and `AUTH_SIGNING_PUBKEY` is **mandatory** — Pages verifies an
externally signed SSO cookie and does not implement login, so without a
compatible signing service the admin UI is unreachable.

**Read [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) first.** It documents the DNS
requirement, the complete environment reference, TLS, service management,
migrations, token rotation, the update and rollback paths, backup and restore, a
health check, a troubleshooting section, and an honest list of the deploy path's
known gaps.

## Contributing

Contributions are welcome under BUSL-1.1. Open an issue before anything
substantial — Pages has a small number of load-bearing invariants, and a patch
that crosses one is hard to accept however well written it is.

See [CONTRIBUTING.md](CONTRIBUTING.md) for setup, the test commands CI actually
runs, the branch and commit conventions, and the invariants themselves.
[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) applies.

## License

Pages is **source available**, not open source.

- Licensed under the **Business Source License 1.1** — see [LICENSE](LICENSE).
- **Non-production use only.** The Additional Use Grant is **None**, so the
  licence grants no production use at all. Reading, modifying, redistributing,
  local development, evaluation, testing and security research are all
  permitted; running it to serve real dashboards is not.
- **Each version converts to MIT two years after it is published.** BSL applies
  per version, so the copy you hold converts two years after *its* commit
  date — and a version already published keeps its own date and converts on
  schedule regardless of later commits.

To compute the Change Date for the exact copy you are holding:

```bash
./scripts/bsl-change-date.sh          # for HEAD
./scripts/bsl-change-date.sh <ref>    # for any commit, tag or branch
```

[docs/LICENSING.md](docs/LICENSING.md) explains all of this in plain English —
what "non-production" means, how the rolling Change Date works, and BSL's own
four-year cap.

For **commercial licensing** and production rights, email
**licensing@elcanotek.com**.

Third-party attribution is in [NOTICE](NOTICE).

Copyright © 2026 ElcanoTek, Inc.
