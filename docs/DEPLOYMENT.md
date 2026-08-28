# Deploying Pages

This is the authoritative deployment guide. Everything here was written against
`scripts/bootstrap.sh`, `scripts/update.sh`, `deploy/pages.service`,
`deploy/pages.caddy` and `deploy/pages-cli` as they exist in this repository.

> **Licence.** Pages is source-available under BUSL-1.1 with **no production use
> grant**. Deploying it to serve real dashboards is production use and needs a
> commercial licence — see [LICENSING.md](LICENSING.md), or
> licensing@elcanotek.com. This guide is here because non-production deployment
> (evaluation, staging, security research) is permitted, and because a licensee
> needs it.

---

## 1. Read this first: you need TWO registrable domains

**This is the single thing an outside deployer gets wrong, and getting it wrong
silently removes the security boundary the whole design rests on.**

Pages is one process serving two hostnames:

| Host | Role | What it serves |
| --- | --- | --- |
| **dashboard host** | Trusted auth zone. Reads the SSO cookie. | `/admin`, `/view/<slug>`, `/api/v1`, `/mcp` |
| **content host** | Untrusted render zone. **Cookieless.** | `/<slug>` (live client pages), `/raw`, `/assets` |

The content host renders HTML that agents wrote. That HTML runs scripts. The
whole safety argument is that it runs them on an origin that has no access to
the staff SSO cookie, no access to the admin API, and no same-origin reach into
anything that does.

**The two hostnames must be different registrable domains (different eTLD+1).**

```
✅  dashboard: pages.example.com        content: example-pages.com
❌  dashboard: pages.example.com        content: content.example.com
```

A subdomain is **not** a security boundary. Cookies are scoped by registrable
domain, not by origin: a script on `content.example.com` can write a cookie
with `Domain=.example.com` and have it sent to `pages.example.com`. That is
cookie tossing, and it lets untrusted agent HTML forge or overwrite session
state on the trusted host. Same-site cookie attributes do not help — both names
are the same site.

So you need to **register and pay for two domains**. If you only own one, do not
deploy Pages to serve anything you care about.

`bootstrap.sh` checks for this and warns:

```
! CONTENT_HOST (content.example.com) looks like it shares a registrable domain
  with DASHBOARD_HOST (pages.example.com).
! That defeats the isolation model (cookie-tossing). Use a different eTLD+1.
  Continuing, but fix before production.
```

It is a warning, not a hard stop. Do not continue past it.

### DNS

Both names must resolve to the same host, and both need to be publicly
reachable on 80 and 443 before you run bootstrap, because Caddy solves an ACME
HTTP challenge for each.

```
pages.example.com.     A     203.0.113.10
example-pages.com.     A     203.0.113.10
```

`AAAA` records too if you serve IPv6. No `CNAME` at a zone apex.

Verify before proceeding:

```bash
dig +short pages.example.com
dig +short example-pages.com
```

The rest of the design is in [SECURITY.md](SECURITY.md), which is worth reading
before you operate this.

---

## 2. Prerequisites

**Operating system.** `scripts/bootstrap.sh` is written for **Fedora / RHEL 9+
and derivatives** (Rocky, Alma). It uses `dnf`, `postgresql-setup --initdb`,
`firewall-cmd`, and expects PGDATA at `/var/lib/pgsql/data`. It is not portable
to Debian or Ubuntu as written — see [§13 Known gaps](#13-known-gaps).

**What bootstrap installs for you:** `git`, `curl`, `jq`, `rsync`, `openssl`,
`postgresql`, `postgresql-server`, Node 20 (from `dnf`, or NodeSource if the
distro package is older than 20), and optionally `caddy`.

**What you must have before you start:**

- Root (the script refuses to run otherwise).
- systemd.
- Ports 80 and 443 reachable from the internet, if you want automatic TLS.
- Two registrable domains, DNS pointed at this host (§1).
- A **git checkout** of Pages on the machine. Bootstrap will not run from an
  unpacked tarball — it requires `.git` so `pages update` works later.
- **An Ed25519 SSO signing public key.** See below.

### The SSO public key is mandatory

Pages does not implement login. It **verifies** a cookie signed by an external
Ed25519 signing service, and `AUTH_SIGNING_PUBKEY` is that service's public
key: base64, exactly 32 bytes decoded. Bootstrap validates the length and dies
if it is wrong or missing:

```
✗ AUTH_SIGNING_PUBKEY must be a base64-encoded 32-byte Ed25519 public key
  (decoded to 0 bytes).
```

Without a valid key, every `elcano_auth` cookie is rejected and every admin
request bounces to the login URL — the app runs but no human can administer it.
Agent bearer tokens still work, so `/api/v1` and `/mcp` are usable, but nothing
that needs a human (approve, disable, clear a password, partner portals) is.

There is no signing service in this repository. To deploy Pages you must either
run a compatible Ed25519 cookie signer or accept that the admin UI is
unreachable. The cookie is `base64url(JSON{email,tenant,iat,exp}) + "." +
base64url(ed25519_sig)`; `lib/auth.js` is the verifier, and
`scripts/dev-auth.js` mints a throwaway keypair the same way for local dev.
Sanity-check a candidate key:

```bash
printf '%s' "$AUTH_SIGNING_PUBKEY" | base64 -d | wc -c   # must print 32
```

Anyone whose cookie email ends in `@$ADMIN_EMAIL_DOMAIN` is automatically an
admin.

---

## 3. PostgreSQL provisioning

Bootstrap does all of this; this section is what it does, so you can audit or
redo it.

1. Installs `postgresql` and `postgresql-server`.
2. `postgresql-setup --initdb` if `$PGDATA/PG_VERSION` is absent. Default
   `PGDATA` is `/var/lib/pgsql/data`; override by exporting `PGDATA` before
   running.
3. Rewrites the two loopback lines in `$PGDATA/pg_hba.conf` to
   `scram-sha-256` — Pages connects over `127.0.0.1` with a password:
   ```
   host  all  all  127.0.0.1/32  scram-sha-256
   host  all  all  ::1/128       scram-sha-256
   ```
4. `systemctl enable --now postgresql`.
5. Creates or alters role `pages` with a generated 24-byte hex password, and
   creates database `pages` owned by it.
6. Writes `DATABASE_URL` into `/etc/default/pages`.
7. Runs `node lib/migrate.js` as the `pages` user.

**Re-running is safe.** Bootstrap parses the existing password back out of the
`DATABASE_URL` already in `/etc/default/pages` and reuses it, so a re-run never
locks the app out. The generated password is hex, so it needs no URL escaping.

Pages does not use TLS to Postgres (`sslmode=disable`) because both ends are on
loopback. If you move the database off-box, change `DATABASE_URL` and require
TLS — nothing in the app depends on the loopback assumption beyond that string.

Using a managed Postgres instead: skip nothing, but set `DATABASE_URL` in the
environment before the run so bootstrap reuses it rather than provisioning a
local cluster. Pages needs a database it can create tables and triggers in.
The schema uses nothing newer than identity columns, triggers and partial
indexes, so any reasonably current major should work; PostgreSQL 16 is what CI
actually exercises.

---

## 4. The bootstrap run

```bash
sudo dnf install -y git
sudo git clone https://github.com/ElcanoTek/pages.git /opt/pages-src
cd /opt/pages-src
sudo bash scripts/bootstrap.sh
```

It prompts for:

| Prompt | Notes |
| --- | --- |
| Auth service public signing key | `AUTH_SIGNING_PUBKEY`, required, validated |
| Dashboard hostname | trusted auth zone |
| Content hostname | **separate registrable domain** |
| Set up Caddy + auto-TLS? | `Y`/`n` |
| Use Let's Encrypt? | `n` gives `tls internal` (self-signed) |
| LE contact email | blank to skip |

### Non-interactive

```bash
sudo env \
  PAGES_BOOTSTRAP_NON_INTERACTIVE=1 \
  AUTH_SIGNING_PUBKEY='<base64 32-byte key>' \
  PAGES_BOOTSTRAP_DASHBOARD_HOST='pages.example.com' \
  PAGES_BOOTSTRAP_CONTENT_HOST='example-pages.com' \
  PAGES_BOOTSTRAP_SETUP_CADDY=Y \
  PAGES_BOOTSTRAP_USE_LETSENCRYPT=Y \
  PAGES_BOOTSTRAP_LE_EMAIL='ops@example.com' \
  bash scripts/bootstrap.sh
```

In non-interactive mode a prompt with no default and no environment value is a
fatal error, which is what you want in automation.

### What it puts where

| Path | What |
| --- | --- |
| `/opt/pages-src` | The git checkout. `pages update` pulls here |
| `/opt/pages` | The running install (rsync of the checkout + `node_modules`) |
| `/opt/pages/assets` | Writable asset directory (the only `ReadWritePaths`) |
| `/etc/default/pages` | Environment file, mode `0640`, `root:pages` |
| `/etc/systemd/system/pages.service` | From `deploy/pages.service` |
| `/usr/local/bin/pages` | From `deploy/pages-cli` |
| `/etc/caddy/conf.d/pages.caddy` | From `deploy/pages.caddy`, hostnames substituted |
| user `pages` | System user, `nologin`, home `/opt/pages` |

The default port is **3002** (`PAGES_PORT` to change it at bootstrap; `PORT` in
the env file thereafter). Caddy proxies both hostnames to `127.0.0.1:3002`.

### The initial agent token

On the **first** bootstrap only — specifically, when the `api_tokens` table has
zero unrevoked rows — bootstrap mints a token labelled `initial` and prints it:

```
  Agent token  pgs_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
               (label 'initial' — shown ONCE, not recoverable)
```

Copy it immediately. Only an HMAC of it is stored. Re-runs skip minting, so a
re-run will not give you a new one — use `pages token add` (§9).

---

## 5. Environment variable reference

`/etc/default/pages`, read by systemd via `EnvironmentFile`. Restart the service
after editing: `pages restart`.

### Required in production

| Variable | Default | What it does |
| --- | --- | --- |
| `AUTH_SIGNING_PUBKEY` | — | Ed25519 **public** key (base64, 32 bytes) that verifies the SSO cookie. Empty ⇒ every admin request bounces to login |
| `PAGE_COOKIE_SECRET` | — | HMAC key for per-page client-password session cookies. **Throws at startup** if empty when `NODE_ENV=production` |
| `RAW_TOKEN_SECRET` | — | HMAC key for signed `/raw` render tokens. **Throws at startup** if empty when `NODE_ENV=production` |
| `API_TOKEN_PEPPER` | — | Server pepper for hashing agent bearer tokens and upload tickets. **Throws at startup** if empty when `NODE_ENV=production`. **Changing it invalidates every issued token and ticket** |
| `DATABASE_URL` | libpq `PG*` fallback | Postgres connection string. If unset, `lib/db.js` falls back to standard `PGHOST`/`PGPORT`/`PGUSER`/`PGDATABASE` |

All three HMAC secrets are generated by bootstrap (`openssl rand -hex 32`) and
preserved across re-runs. Treat them as unrotatable in practice: rotating
`PAGE_COOKIE_SECRET` logs every client out, rotating `RAW_TOKEN_SECRET`
invalidates outstanding preview links, and rotating `API_TOKEN_PEPPER` breaks
every agent integration at once.

### Hosts and origins

| Variable | Default | What it does |
| --- | --- | --- |
| `DASHBOARD_HOST` | `pages.elcanotek.com` | Trusted host. Requests with this `Host` get the auth zone |
| `CONTENT_HOST` | `elcano-pages.com` | Cookieless content host. **Must be a different registrable domain** |
| `DASHBOARD_ORIGIN` | `https://$DASHBOARD_HOST` | Set only to override the scheme |
| `CONTENT_ORIGIN` | `https://$CONTENT_HOST` | Set only to override the scheme |
| `CONTENT_HOST_ALSO` | unset | An extra hostname treated as the content host. **Local dev only** |
| `PORT` | `3002` | Listen port, loopback behind Caddy |
| `NODE_ENV` | unset | Set to `production`. Enables the fail-closed secret checks and suppresses stack traces in error pages |

The defaults are ElcanoTek's own deployment. Nothing host-specific is compiled
in — these two values are the whole configuration of the split.

### Authentication

| Variable | Default | What it does |
| --- | --- | --- |
| `AUTH_COOKIE_NAME` | `elcano_auth` | Name of the SSO cookie to verify |
| `AUTH_LOGIN_URL` | `https://auth.elcanotek.com` | Where auth failures redirect |
| `ADMIN_EMAIL_DOMAIN` | `elcanotek.com` | Cookie emails in this domain are automatically admins |
| `ADMIN_CSRF_SECRET` | falls back to `PAGE_COOKIE_SECRET`, then `RAW_TOKEN_SECRET` | HMAC key for admin CSRF tokens. Usually leave unset |

### MCP transport boundary

| Variable | Default | What it does |
| --- | --- | --- |
| `MCP_ALLOWED_HOSTS` | unset | Comma-separated extra **hostnames** accepted at `/mcp` (no scheme, no port). `DASHBOARD_HOST` is always allowed |
| `MCP_ALLOWED_ORIGINS` | unset | Comma-separated extra exact **origins** (scheme + host + optional port). `DASHBOARD_ORIGIN` is always allowed |

Add entries only for a deliberate reverse proxy or browser client. Each one
widens a boundary that is checked before the body is parsed.

### Rate limits (per IP, per minute unless noted)

| Variable | Default | What it does |
| --- | --- | --- |
| `RL_API_PER_MIN` | `120` | `/api/v1` and `/upload/:id` |
| `RL_MCP_PER_MIN` | `120` | `/mcp` |
| `RL_CONTENT_PER_MIN` | `240` | Content-host reads |
| `RL_PASSWORD_TRIES` | `20` | Password-form attempts per window |
| `RL_PASSWORD_WINDOW_MIN` | `15` | Password window, minutes |

These sit on top of a per-page progressive backoff that is not configurable and
deliberately delays rather than locks out.

### Payload and upload bounds

| Variable | Default | What it does |
| --- | --- | --- |
| `MAX_HTML_BYTES` | `2mb` | JSON body limit for the dashboard app and `/api/v1` (an Express size string) |
| `PAGE_UPLOAD_MAX_CHUNK_BYTES` | `49152` (48 KiB) | Max base64 chunk in a staged upload. Hard-clamped at 256 KiB by the app **and** a database CHECK |
| `PAGE_UPLOAD_TICKET_TTL_MINUTES` | `15` | Upload-ticket lifetime |
| `PAGES_MCP_MAX_INLINE_DATA_BYTES` | `1500000` | Largest inline data payload an MCP tool call may carry |

### Managed dashboard-data bounds

| Variable | Default |
| --- | --- |
| `PAGES_DATA_SCHEMA_MAX_BYTES` | `262144` |
| `PAGES_DATA_MAX_BYTES` | `1048576` |
| `PAGES_DATA_CONFIG_MAX_BYTES` | `262144` |
| `PAGES_DATA_TEMPLATE_MAX_BYTES` | `2097152` |
| `PAGES_DATA_SCHEMA_MAX_NODES` | `10000` |
| `PAGES_DATA_SCHEMA_MAX_DEPTH` | `100` |
| `PAGES_SOURCE_FUTURE_TOLERANCE_MS` | `300000` |

These are CPU and memory guards on agent-supplied JSON Schemas and data. Raising
them raises the cost of a single hostile call. Leave them alone unless a real
dashboard is hitting a limit.

### Database bounds

Milliseconds; `0` disables a bound. These exist so one wedged page-lock
transaction cannot pin every pool client and hang both hosts.

| Variable | Default | What it does |
| --- | --- | --- |
| `PG_CONNECT_TIMEOUT_MS` | `5000` | `pool.connect()` wait when Postgres is unreachable |
| `PG_STATEMENT_TIMEOUT_MS` | `15000` | Server-side per-statement cap |
| `PG_LOCK_TIMEOUT_MS` | `10000` | Max wait on a row lock |
| `PG_IDLE_TXN_TIMEOUT_MS` | `30000` | Kill sessions idle mid-transaction |

### Theming

| Variable | Default | What it does |
| --- | --- | --- |
| `FLAG_ASSETS_BASE` | `/assets/flag` | Base URL for the vendored Flag assets. Change only if you serve them from elsewhere |

### Never set these in production

| Variable | Why |
| --- | --- |
| `PAGES_DEV_LOGIN=1` | Exposes `GET /__dev/login`, which sets an admin cookie **with no authentication at all** |
| `DEV_ADMIN_COOKIE`, `DEV_ADMIN_EMAIL` | The value that route hands out |
| `PAGES_COMPOSE=1`, `COMPOSE_DRIVER`, `CUTLASS_BIN`, `CUTLASS_DIR` | Enable a local "compose" panel that spawns an external authoring CLI as a subprocess. Development only |
| `MOC_URL`, `MOC_API_KEY`, `MOC_MODEL`, `MOC_FALLBACK_MODEL`, `MOC_TARGET_NODE_NAME`, `COMPOSE_TIMEOUT_MS`, `COMPOSE_POLL_MS` | Configuration for that same optional panel's remote driver, an ElcanoTek-internal orchestration service. Irrelevant to an outside deployment; Pages runs no scheduler of its own |

`/__dev/login` requires **both** `PAGES_DEV_LOGIN=1` and `DEV_ADMIN_COOKIE`, and
bootstrap never writes either. Confirm with `pages env`.

---

## 6. TLS via Caddy

Bootstrap installs Caddy, ensures `/etc/caddy/Caddyfile` contains
`import conf.d/*.caddy`, and writes `/etc/caddy/conf.d/pages.caddy` from
`deploy/pages.caddy` with `{{DASHBOARD_HOST}}` and `{{CONTENT_HOST}}`
substituted. Certificates are obtained and renewed automatically — no cron.

Both site blocks reverse-proxy `127.0.0.1:3002` and set HSTS
(`max-age=31536000; includeSubDomains`) plus `X-Content-Type-Options: nosniff`.

The **dashboard** block additionally sets `X-Frame-Options: DENY` and
`Referrer-Policy: strict-origin-when-cross-origin`. The **content** block
deliberately does **not** set `X-Frame-Options`, because `/raw` is meant to be
framed by the admin shell as a preview; the app sets a per-response `sandbox`
CSP and `frame-ancestors` instead. Do not "fix" this by adding
`X-Frame-Options` to the content block — it breaks admin preview.

If you declined Let's Encrypt, bootstrap injects `tls internal` into each site
block (self-signed, browsers will warn). To switch to real certificates later,
remove those two lines and reload:

```bash
sudo sed -i '/^\ttls internal$/d' /etc/caddy/conf.d/pages.caddy
pages tls reload
```

Fronting with nginx, HAProxy or an ALB instead is fine — proxy both hostnames to
`127.0.0.1:3002`, preserve the `Host` header (the app branches on it; get this
wrong and every request lands in the wrong zone), and reproduce the header
blocks above. Answer `n` to the Caddy prompt.

`firewalld`, if active, gets `http` and `https` opened permanently.

---

## 7. Service management

The unit is `deploy/pages.service`: `Type=simple`, runs `/usr/bin/node
server.js` as `pages:pages` from `/opt/pages`, `Restart=always` with
`RestartSec=5`, `After`/`Requires=postgresql.service`, and a hardening block —
`NoNewPrivileges`, `PrivateTmp`, `PrivateDevices`, `ProtectSystem=strict`,
`ProtectHome`, `ProtectControlGroups`, `ProtectKernelModules`,
`ProtectKernelTunables`, `LockPersonality`, and
`RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`.

`ProtectSystem=strict` makes the filesystem read-only apart from
`ReadWritePaths=/opt/pages/assets`. If a future feature needs to write
somewhere else, add a `ReadWritePaths` line rather than relaxing
`ProtectSystem`.

### The operator CLI

`/usr/local/bin/pages` (from `deploy/pages-cli`). Every subcommand that touches
the system re-invokes through `sudo`.

| Command | What it does |
| --- | --- |
| `pages start` / `stop` / `restart` / `status` | `systemctl` on `pages.service` |
| `pages logs` | `journalctl -fu pages.service` (follow) |
| `pages logs <journalctl args…>` | Passes arguments through, e.g. `pages logs -n 200 --since '1 hour ago'` |
| `pages update` | Fetch, fast-forward, rebuild in staging, migrate, swap, restart. §10 |
| `pages rebuild` | Rebuild the current checkout **without fetching**. The rollback verb. §11 |
| `pages env` | Print `/etc/default/pages` with secrets redacted. §12 |
| `pages tls status` \| `reload` \| `restart` | Caddy state, `caddy reload`, or restart the unit |
| `pages token add <label> [scope] [slug…]` | Mint an agent token. §9 |
| `pages token list` | List tokens: prefix, label, scope, state, last use |
| `pages token revoke <id>` | Revoke; the server 401s it immediately |
| `pages template list` \| `show` \| `register` \| `sync` | Page templates. See [TEMPLATES.md](TEMPLATES.md) |
| `pages help` | Usage |

`pages token` and `pages template` both run the installed app's script as the
`pages` user with the **service's** environment file sourced. That wiring is
load-bearing: a token minted with a different `API_TOKEN_PEPPER` than the server
verifies with is a token the server rejects. Do not run `node scripts/token.js`
by hand in production.

`pages page`, `pages theme`, `pages backup` and `pages check-integrity` are
**stubs**. They print a notice and exit 2. In particular there is no working
`pages backup` — use §14.

---

## 8. Migrations

`migrations/*.sql`, applied in filename order by `lib/migrate.js`, tracked in a
`schema_migrations` table by filename. Idempotent: already-applied files are
skipped. Additive by design, so they can run against a schema the still-running
old code is using — which is exactly what `pages update` does.

Bootstrap and `pages update` both run them. To run them by hand:

```bash
sudo runuser -u pages -- bash -c \
  'set -a; . /etc/default/pages; set +a; cd /opt/pages && node lib/migrate.js'
```

**Never edit an applied migration file.** The runner keys on filename, so an
edit is silently ignored on machines that already ran it and silently applied on
machines that have not — the two diverge with no error. Add a new file.

`018_reserve_portal_slug.sql` has an operator-facing guard: it refuses to apply
if a live page occupies a slug under `portal/`, names the colliding page, and
tells you to rename it. Rename the page, then re-run.

---

## 9. Agent API tokens

Tokens authenticate `/api/v1` and `/mcp` as `Authorization: Bearer pgs_…`.
Only `HMAC-SHA256(token, API_TOKEN_PEPPER)` is stored, alongside a short
display prefix. Independently revocable.

```bash
pages token add ci-publisher deploy
pages token add nightly-refresh data_update acme/weekly acme/monthly
pages token list
pages token revoke 3
```

Scopes:

| Scope | Grants |
| --- | --- |
| `deploy` (default) | Broad authoring: create, deploy, publish/rollback open pages, set a password, rename, soft-delete open pages, workspaces |
| `data_update` | Narrow: data-only updates, **restricted to the exact slugs listed at mint time** |
| `admin` | Accepted by the minter and treated as a broad scope alongside `deploy`. Nothing provisions it, and it grants no extra authority — the human-only actions need an admin cookie and CSRF, not a token |

**The raw token is printed once and is not recoverable.** Capture it at mint
time.

### Rotation

There is no rotate verb. Overlap, then revoke:

```bash
pages token add ci-publisher-2026q3 deploy   # 1. mint the replacement
# 2. deploy the new value to the consumer and confirm it works
pages token list                             # 3. find the old token's id
pages token revoke 7                         # 4. revoke the old one
```

Rotate broad `deploy` tokens on a schedule. A leaked one can rewrite,
re-password, rename or delete every open page — this is a known limitation,
ranked first in [SECURITY.md](SECURITY.md). Never install a broad token on an
unattended scheduler; mint a slug-scoped `data_update` token for that.

Do **not** rotate `API_TOKEN_PEPPER` as a way of revoking tokens. It invalidates
every token and every outstanding upload ticket at once, with no staged
migration path.

---

## 10. Updating

```bash
pages update            # prompts before the swap
PAGES_UPDATE_YES=1 pages update
```

`scripts/update.sh` runs as root and, in order:

1. `git -C /opt/pages-src fetch` then **`merge --ff-only origin/<branch>`**. It
   dies on a non-fast-forward — local commits in `/opt/pages-src` block the
   update. Keep that checkout pristine.
2. If `scripts/update.sh` itself changed, re-execs the new version.
3. Prompts (unless `PAGES_UPDATE_YES=1` or stdin is not a TTY).
4. Builds in a `mktemp -d` staging directory: rsync the source, copy the live
   `node_modules` as a warm cache, `npm ci --omit=dev`, and `node --check
   server.js`.
5. **Runs pending migrations from staging.** They are additive, so the live old
   code is unaffected.
6. `systemctl stop`, rsync staging over `/opt/pages`, reinstall the unit and the
   CLI, `daemon-reload`, `systemctl start`.
7. Polls `http://127.0.0.1:$PORT/healthz` for up to 10 seconds and dies if it
   never answers.
8. Runs `pages template sync` against the now-healthy service.

**Anything that fails before step 6 leaves the live service running the old code
untouched.** `/etc/default/pages`, `/opt/pages/assets` and `/opt/pages/.env` are
excluded from every rsync and survive.

There is a short outage at step 6 — `stop`, rsync, `start`. Not zero-downtime.
For a fleet, update one host at a time behind a load balancer.

Template sync deliberately does not fail the update: a template is design data,
so a broken one reports and leaves the previous revision current rather than
failing a deploy that already proved the service healthy. Skip it with
`PAGES_SKIP_TEMPLATE_SYNC=1`.

---

## 11. Rollback

Code rollback is checkout-plus-rebuild:

```bash
cd /opt/pages-src
git log --oneline -20
sudo git checkout <good-sha>
pages rebuild                # = PAGES_UPDATE_NO_PULL=1 PAGES_UPDATE_YES=1 update.sh
```

`pages rebuild` skips the fetch and rebuilds whatever is checked out, so it does
not undo your `git checkout`. To get back onto the branch afterwards:

```bash
cd /opt/pages-src && sudo git checkout main && pages update
```

**Migrations do not roll back.** There are no down-migrations. Rolling code back
across a migration leaves the newer schema in place. Because migrations are
additive this is usually fine — the older code ignores columns it does not know
— but verify the specific migration before relying on it. If it is not
backward-compatible, restore the database (§14) instead.

**Content rollback is a different thing entirely, and it is a first-class
feature.** Page versions are append-only and "live" is a pointer, so reverting a
dashboard is a pointer move, not a deploy: `pages`' own rollback in the admin
UI, `POST /api/v1/pages/<slug>/rollback`, or the `rollback_page` MCP tool. That
needs no operator involvement.

---

## 12. Inspecting configuration

```bash
pages env
```

Prints `/etc/default/pages` with secrets redacted by an embedded `awk` program:
any key matching `TOKEN|KEY|SECRET|PASSWORD|PEPPER`, plus `DEV_ADMIN_COOKIE`,
becomes `[REDACTED]`; and the userinfo password inside any URL-shaped value is
replaced, so `DATABASE_URL` prints as
`postgres://pages:[REDACTED]@127.0.0.1:5432/pages?sslmode=disable`. Comments are
dropped. The redactor has its own unit test.

Safe to paste into a ticket. Verify that yourself the first time.

---

## 13. Health check

Both hosts answer `GET /healthz` with `200` and the body `ok`. It is a liveness
check on the HTTP listener, **not** a database check — Pages can answer
`/healthz` with Postgres down.

```bash
curl -fsS http://127.0.0.1:3002/healthz                      # local
curl -fsS https://pages.example.com/healthz                  # through Caddy
curl -fsS https://example-pages.com/healthz
```

For a monitor that actually proves the app is serving, add an authenticated
read:

```bash
curl -fsS -H "Authorization: Bearer $PAGES_TOKEN" \
  https://pages.example.com/api/v1/pages >/dev/null
```

That one touches the database. `scripts/mcp-smoke.sh` does the equivalent for
the MCP surface.

Confirm the host split is actually wired — this is the check that catches a
proxy dropping the `Host` header:

```bash
# content host must NOT serve /admin
curl -s -o /dev/null -w '%{http_code}\n' https://example-pages.com/admin      # expect 404
# dashboard host must not serve live pages
curl -s -o /dev/null -w '%{http_code}\n' https://pages.example.com/some-slug  # expect 404
```

---

## 14. Backup and restore

`pages backup` is a stub. Do it yourself. Two things need backing up: the
Postgres database (pages, versions, passwords, tokens, portals, audit log) and
`/opt/pages/assets`.

### Backup

```bash
sudo install -d -m 0700 /var/backups/pages
sudo runuser -u postgres -- pg_dump -Fc pages \
  > /var/backups/pages/pages-$(date -u +%Y%m%dT%H%M%SZ).dump
sudo tar czf /var/backups/pages/assets-$(date -u +%Y%m%dT%H%M%SZ).tar.gz \
  -C /opt/pages assets
```

Dump the database **before** the assets, so a referenced asset can never be
missing from the pair.

Also back up **`/etc/default/pages`**, separately and encrypted. It holds the
three HMAC secrets and the database password. Without `API_TOKEN_PEPPER` a
restored database has unusable tokens; without `PAGE_COOKIE_SECRET` every client
session is void. A database backup alone is not a recoverable backup.

`/opt/pages` and `/opt/pages-src` need no backup — they are reproducible from
git plus `npm ci`.

A nightly cron:

```
0 3 * * * root /usr/local/sbin/pages-backup.sh
```

with retention you actually enforce, and at least one copy off-box.

### Restore

```bash
pages stop
sudo runuser -u postgres -- dropdb pages
sudo runuser -u postgres -- createdb -O pages pages
sudo runuser -u postgres -- pg_restore -d pages /var/backups/pages/pages-<ts>.dump
sudo tar xzf /var/backups/pages/assets-<ts>.tar.gz -C /opt/pages
sudo chown -R pages:pages /opt/pages/assets
# restore /etc/default/pages if lost — mode 0640, root:pages
sudo runuser -u pages -- bash -c \
  'set -a; . /etc/default/pages; set +a; cd /opt/pages && node lib/migrate.js'
pages start
curl -fsS http://127.0.0.1:3002/healthz
```

`pg_restore` will emit ownership warnings if the role names differ; harmless as
long as role `pages` exists and owns the database.

Test a restore into a scratch database before you need it. An untested backup is
not a backup.

---

## 15. Troubleshooting

### `pages status` shows the service restarting in a loop

`pages logs -n 50`. In `NODE_ENV=production` the app throws at startup on an
empty secret, and the message names the variable:

```
Error: RAW_TOKEN_SECRET is required in production (an empty key lets anyone
forge /raw access tokens).
```

Same shape for `PAGE_COOKIE_SECRET` and `API_TOKEN_PEPPER`. `pages env` and
confirm all three are present (they will show `[REDACTED]`, which is what
present looks like).

### Every `/admin` visit redirects to the login URL

`AUTH_SIGNING_PUBKEY` is missing, wrong, or not the key that signed your cookie.
Pages verifies; it does not sign. Check:

```bash
sudo grep AUTH_SIGNING_PUBKEY /etc/default/pages | \
  sed 's/.*="\(.*\)"/\1/' | base64 -d | wc -c      # must be 32
```

If it decodes to 32 bytes and you still bounce, the signing service rotated its
key, your cookie has expired, or your cookie email's domain does not match
`ADMIN_EMAIL_DOMAIN`.

### A signed-in admin gets 403 rather than a redirect

The cookie verified but the email is not in `ADMIN_EMAIL_DOMAIN`. The SSO
audience is wider than your staff; that check is the difference. Fix the domain
or the account.

### A client page 404s on the content host

In order:

1. Never published. A page with no live version 404s by design.
2. Disabled — the takedown switch. 404s before anything else is consulted, and
   re-enabling is admin-only.
3. Soft-deleted. Restore it from the admin UI.
4. Wrong host. Live pages are on the **content** host; `/admin` and `/view` are
   on the **dashboard** host.
5. The proxy is not preserving `Host`, so the request landed in the wrong zone.
   Test the app directly: `curl -H 'Host: example-pages.com' -i
   http://127.0.0.1:3002/<slug>`. If that works and the public URL does not, the
   fault is in the proxy.

### The admin shell loads but the page preview iframe is blank

The preview frames the **content** host. Check that the content hostname
resolves, that its certificate is valid (a self-signed `tls internal`
certificate blocks the frame silently), and that nothing added
`X-Frame-Options` to the content site block.

### Every asset request re-downloads the fonts, or theming looks wrong

`/assets` sets its cache policy explicitly. If you put a CDN or another proxy in
front of the content host, make sure it is not stripping `Cache-Control` or the
CSP. Check `FLAG_ASSETS_BASE` is still `/assets/flag`.

### `429` from `/api/v1` or `/mcp`

Per-IP rate limits (§5). If your agents share one egress IP they share one
budget. Raise `RL_API_PER_MIN` / `RL_MCP_PER_MIN` and restart — but if a single
consumer is hitting 120/min, look at the consumer first.

### The password form rejects a password you know is right

The per-page progressive backoff delays the `401` after repeated failures — one
shared counter per page across all source IPs, deliberately, so an attacker
cannot lock a real viewer out. It delays, never locks. Wait, then retry. A
correct password resets the counter. Also check whether the page's password was
rotated (that voids existing sessions) or cleared (which makes it staff-only and
403s real clients — an admin-only action, and audited).

### `pages update` dies with "fast-forward failed (local commits?)"

Something committed to `/opt/pages-src`. Inspect with `git -C /opt/pages-src
status` and `log`. Never patch production in place; that checkout must stay a
clean mirror of the branch.

### Migration fails naming a page under `portal/`

Migration 018 reserving the `/portal/*` route space. It names the colliding
page and tells you to rename it. Rename the page, then re-run `pages update`.

### `pg` errors under load: connect timeouts or lock timeouts

The bounds in §5 firing on purpose — they fail fast instead of letting one
wedged transaction pin the pool and hang both hosts. Look for a long-running
transaction:

```sql
SELECT pid, state, wait_event_type, now() - xact_start AS age, query
  FROM pg_stat_activity
 WHERE datname = 'pages' AND state <> 'idle'
 ORDER BY xact_start;
```

Raising the timeouts hides the symptom. Find the query.

### An agent got a `403` it did not expect

By design. Agents cannot approve, reject, disable, toggle the approval gate,
clear a password, set a theme, or touch partner portals — those are
human-admin-plus-CSRF only. On an approval-gated page an agent can only queue a
`pending` version. On a disabled page an agent cannot publish, roll back or
delete. The error code names the rule (`admin_only`, `portal_admin_only`,
`disabled_takedown`).

### Caddy will not obtain a certificate

Both names need public DNS and reachable 80/443 before Caddy asks. `pages tls
status`, then `journalctl -u caddy -n 100`. A firewall or a CDN in front
intercepting `/.well-known/acme-challenge/` is the usual cause. `tls internal`
gets you running while you fix DNS.

---

## 16. Known gaps

Honest limitations of the deploy path as shipped:

1. **`scripts/bootstrap.sh` is Fedora/RHEL-only.** It hard-codes `dnf`,
   `postgresql-setup --initdb`, `/var/lib/pgsql/data` and `firewall-cmd`. On
   Debian or Ubuntu it fails at the first `dnf`. The application itself is
   portable — the packaging is not. Provision by hand there, following §3 and §4
   as a specification.
2. **No signing service.** `AUTH_SIGNING_PUBKEY` is mandatory for the admin UI
   and nothing in this repository can produce a matching cookie for production.
   Plan for that before you deploy.
3. **`pages backup`, `pages check-integrity`, `pages page` and `pages theme` are
   stubs** that exit 2. Use §14 for backups.
4. **No token rotation verb.** Mint, cut over, revoke (§9).
5. **`pages update` has a brief outage** at the swap. Not zero-downtime.
6. **No down-migrations.** Code rollback across a migration is not always safe
   (§11).
7. **`/healthz` does not check the database.** It can return `200` with Postgres
   down (§13).
8. **The `assets` table has no upload API.** `ReadWritePaths=/opt/pages/assets`
   exists for a feature that is not finished.

---

## Related

- [SECURITY.md](SECURITY.md) — the threat model. Read before operating
- [../SECURITY.md](../SECURITY.md) — how to report a vulnerability
- [API.md](API.md) — REST and MCP agent surfaces
- [LICENSING.md](LICENSING.md) — what you may and may not deploy
- [../CONTRIBUTING.md](../CONTRIBUTING.md) — local development
