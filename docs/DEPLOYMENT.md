# Deploying Pages

This is the authoritative deployment guide. Everything here was written against
the `pages` binary's operator verbs (`pages bootstrap`, `pages update`,
`pages doctor`, `pages backup`, … — `internal/cli` and `internal/ops`),
`install.sh`, `scripts/bootstrap.sh`, `deploy/pages.service` and
`deploy/pages.caddy` as they exist in this repository.

Pages is **one Go binary**. It embeds the front end, the migrations, the shipped
templates and these docs, so a release is a single executable and the box needs
no language runtime — only PostgreSQL, and Caddy if you want automatic TLS.

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

`pages bootstrap` checks for this against the public suffix list and stops
before it writes the environment file or starts anything:

```
✗ CONTENT_HOST (content.example.com) shares the registrable domain example.com with DASHBOARD_HOST (pages.example.com) — that defeats the isolation model (cookie tossing). Use a content hostname on a different registrable domain (eTLD+1).
```

`pages doctor` reports the same condition as a failure.

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

**Operating system.** `pages bootstrap` is written for **Fedora / RHEL 9+
and derivatives** (Rocky, Alma). It uses `dnf`, `postgresql-setup --initdb`,
`firewall-cmd`, and expects PGDATA at `/var/lib/pgsql/data`. It is not portable
to Debian or Ubuntu as written — see [§16 Known gaps](#16-known-gaps). The binary
itself runs anywhere Linux and PostgreSQL do.

**What bootstrap installs for you:** `git`, `make`, `golang`, `postgresql`,
`postgresql-server`, and optionally `caddy`. The box builds its own releases
from the source checkout: `go.mod` pins the exact Go toolchain and the build runs
with `GOTOOLCHAIN=auto`, so any Go from 1.21 on downloads that pin itself. When
dnf's `golang` is missing or older than that, `scripts/bootstrap.sh` (and the
updater bridge) fetch the official toolchain `go.mod` names from go.dev, check
it against its published SHA-256 and unpack it to `/usr/local/go`.

**What you must have before you start:**

- Root (the script refuses to run otherwise).
- systemd.
- Ports 80 and 443 reachable from the internet, if you want automatic TLS.
- Two registrable domains, DNS pointed at this host (§1).
- A **git checkout** of Pages on the machine (`install.sh` makes one).
  Bootstrap will not run from an unpacked tarball — it requires `.git` so
  `pages update` works later.
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
base64url(ed25519_sig)`; `internal/auth` is the verifier, and `pages dev`
mints a throwaway keypair and cookie the same way for local dev
(`internal/ops/devenv`).
Sanity-check a candidate key:

```bash
printf '%s' "$AUTH_SIGNING_PUBKEY" | base64 -d | wc -c   # must print 32
```

Anyone whose cookie email ends in `@$ADMIN_EMAIL_DOMAIN` is automatically an
admin.

### Or: sign in through a password-mode Auth (`PAGES_AUTH_MODE=central`)

A deployment whose Auth runs in password mode issues no shared cookie, so the
default mode above cannot see anyone. Central mode makes Pages an application
of that Auth instead (the contract is Auth's `docs/INTEGRATION.md`, "Password
mode: the application contract"): Pages redirects to Auth's `/authorize`,
exchanges the code at `/token`, keeps its own access list, and mints a
host-only `__Host-pages_session` cookie (24 h absolute, 12 h idle). Everyone
on the access list is an admin; Pages has one human role.
`AUTH_SIGNING_PUBKEY` is still required: it verifies the `id_token` from each
sign-in and Auth's back-channel tokens, and the key is the same one `auth
pubkey` prints.

1. On the Auth host, register Pages (the secret is shown once) and its
   back-channel endpoint, **before** granting anyone, so the grant events
   reach Pages:

   ```bash
   auth app create pages https://<DASHBOARD_HOST>/auth/callback https://<DASHBOARD_HOST>/signed-out
   auth app set-backchannel pages https://<DASHBOARD_HOST>/auth/backchannel-logout
   ```

2. Add to `/etc/default/pages` and `pages restart`:

   ```bash
   PAGES_AUTH_MODE="central"
   AUTH_ISSUER_URL="https://<auth host>"
   AUTH_CLIENT_ID="pages"
   AUTH_CLIENT_SECRET="<from auth app create>"
   ```

   Central mode validates all of this at start and refuses to boot on a
   mistake (`pages logs` names the setting). `AUTH_COOKIE_NAME`,
   `AUTH_LOGIN_URL` and `ADMIN_EMAIL_DOMAIN` are ignored in this mode.

3. Grant people Pages in the Auth console. Auth pushes each grant and revoke
   to Pages as a signed event; a revoke ends that person's Pages sessions.
   `pages access grant|revoke|list <email>` edits the same list directly for
   bootstrap and break-glass.

Sign out from Pages ends the Pages session and hands the browser to Auth's
`/logout`, which signs the person out of every application. Auth's own
sign-out reaches Pages through the back-channel endpoint.

---

## 3. PostgreSQL provisioning

`pages bootstrap` does all of this; this section is what it does, so you can
audit or redo it.

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
7. Runs the migrations embedded in the binary (the same code as `pages migrate`).

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
curl -fsSL https://raw.githubusercontent.com/ElcanoTek/pages/main/install.sh | sudo bash
```

`install.sh` installs `git`, `make`, `golang`, `ca-certificates` and `curl` with
dnf, clones `main` into `/opt/pages-src` (`PAGES_SRC_DIR`), and runs that
checkout's `scripts/bootstrap.sh`. It refuses to run if the directory already
exists — use `pages update` there, or re-run its `scripts/bootstrap.sh` to
reconfigure. The manual equivalent:

```bash
sudo dnf install -y git
sudo git clone https://github.com/ElcanoTek/pages.git /opt/pages-src
cd /opt/pages-src
sudo bash scripts/bootstrap.sh
```

`scripts/bootstrap.sh` is a short bridge: it makes sure a Go toolchain is
present, builds `pages` from the checkout into a temporary directory, and runs
`pages bootstrap`, which does the work in six steps — packages; the service
account and the installed checkout; the first release; database, environment
file, migrations and the initial token; the systemd unit, CLI, login banner,
SSH and hostname hardening, then start and wait for `/readyz`; and Caddy. Safe
to re-run: existing secrets, the database role password, data and an active
release are preserved (`pages update` is what changes code).

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
| `/opt/pages-src` | The git checkout, root-owned. `pages update` fetches and builds here as root, and refuses to while anything in it (or above it) is writable by the service account |
| `/opt/pages.releases/<sha>-<UTC>-<pid>/` | One release: the `pages` binary (root-owned, `0755`: the service account runs it but cannot replace it, since root runs it too through the CLI link), plus `assets` (and `.env`, when present) links to the shared paths |
| `/opt/pages` | Symlink to the active release; the service runs `/opt/pages/pages serve` |
| `/opt/pages.releases/previous` | Link to the last successful predecessor (after the first update) |
| `/opt/pages.assets` | Writable asset directory shared by every release, reached as `/opt/pages/assets` (the only `ReadWritePaths`) |
| `/opt/pages.env` | Optional local `.env` shared by every release |
| `/etc/default/pages` | Environment file, mode `0640`, `root:pages` |
| `/etc/default/pages-install` | Non-secret installation defaults shared by bootstrap, updater and CLI |
| `/etc/systemd/system/pages.service` | Rendered from `deploy/pages.service` |
| `/usr/local/bin/pages` | Symlink to `/opt/pages/pages`: the CLI is always the active release's binary |
| `/etc/caddy/conf.d/pages.caddy` | Rendered from `deploy/pages.caddy`, hostnames substituted |
| `/etc/motd` | The login banner, from `deploy/motd` |
| user `pages` | System user, `nologin`, home `/opt/pages` |

A box installed by an older bootstrap with a plain `/opt/pages` directory is
converted on its next run (or first `pages update`): the directory is retained
as a `legacy-…` release, and its `assets/` and `.env` move to the shared paths.

The default port is **3002** (`PAGES_PORT` to change it at bootstrap; `PORT` in
the env file thereafter). The generated Caddy configuration proxies both
hostnames to that port. The systemd unit uses the resolved application directory,
account and environment file; the CLI uses the same saved values.

For a custom installation, set `PAGES_APP_DIR`, `PAGES_APP_USER`, `PAGES_SRC_DIR`
and `PAGES_PORT` when bootstrapping. Legacy `APP_DIR`/`APP_USER` overrides remain
accepted. `PAGES_ENV_FILE`, `PAGES_CLI_TARGET` and `PAGES_INSTALL_CONFIG` may also
override the corresponding paths above. Paths must be absolute and contain
letters, digits, `/`, `_`, `.` or `-`; invalid paths/accounts/ports fail before
installing application files.

The generated installation file contains only defaults, so explicit overrides
take precedence on a rerun; legacy `APP_DIR`/`APP_USER` override their `PAGES_*`
equivalents. Without overrides, bootstrap, `pages update`, `pages rebuild`, token
and template commands all reuse the saved installation. `PORT` in the service
environment supplies update readiness checks unless `PAGES_PORT` was explicitly
set. Rerun bootstrap when changing the port so the Caddy configuration is
regenerated too. Existing default installations work before this file exists.

Bootstrap also names the box after the service: when the static hostname is an
unset cloud-image default (exactly `vultr.guest`, `localhost` or
`localhost.localdomain`) it runs `hostnamectl set-hostname` with the dashboard hostname,
writes `/etc/cloud/cloud.cfg.d/99-preserve-hostname.cfg` so cloud-init does not
reset it, and points the loopback lines in `/etc/hosts` at the new name. Any
other hostname is treated as deliberate and left alone (bootstrap prints the
command to change it). `PAGES_SET_HOSTNAME=1` forces the rename;
`PAGES_SET_HOSTNAME=0` skips the step.

Bootstrap also uninstalls Cockpit (`dnf -y remove 'cockpit*'`) when `cockpit-ws`
is installed: the Vultr Fedora image ships its root-capable web login on :9090,
and these boxes are administered over ssh only. `PAGES_KEEP_COCKPIT=1` keeps it;
without rpm or Cockpit the step does nothing, and a failure only warns.

Bootstrap also makes SSH key-only, as a safety net for the Vultr startup script
`elcano-key-only-ssh` (Vultr's cloud-init writes
`/etc/ssh/sshd_config.d/50-cloud-init.conf` with `PasswordAuthentication yes`).
It writes `/etc/ssh/sshd_config.d/01-no-password-login.conf` (mode 0600:
`PasswordAuthentication no`, `KbdInteractiveAuthentication no`,
`PermitRootLogin prohibit-password`), which sorts first and so wins, checks it
with `sshd -t` (removing it again on failure), and reloads sshd. It refuses to
touch sshd while root and `/home/*` have no authorized key (counted against the
effective `AuthorizedKeysFile`) or while `sshd -T` reports
`pubkeyauthentication no`, since that would lock everyone out; it warns
instead. An identical file already in place is not rewritten, and when `sshd -T`
still reports `passwordauthentication`, `kbdinteractiveauthentication` or
`permitrootlogin yes` afterwards (an earlier directive in `sshd_config`, such as
`PermitRootLogin yes` above its `Include`) it warns rather than editing
`sshd_config`. Without sshd or `sshd_config.d` the step does nothing;
`PAGES_KEEP_SSH_PASSWORDS=1` skips it; it never fails the install. Run as root,
`pages doctor` reports a warning when `sshd -T` still accepts passwords (the
same three settings).

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

Bootstrap merges its core host/auth/database fields into the existing file.
Other settings (including `RL_*`, database timeouts, `PAGES_DATA_*`, custom
entries and comments) survive reruns unchanged. Existing values win over
defaults; explicit `PAGES_BOOTSTRAP_*` prompt overrides update the corresponding
managed values. Origins follow a changed hostname only when they used the old
default `https://<host>`; an explicit custom origin stays intact.

Generation validates the complete candidate before replacing the environment
file atomically. The prior contents remain at `/etc/default/pages.previous`
with the same ownership and permissions. This backup contains the same
credentials as the original environment; restore it only as an intentional
configuration rollback. A generation or staging failure leaves the active
environment file intact. Bootstrap-managed values must be single-line text;
operator additions may retain quoted multiline values.

### Required in production

| Variable | Default | What it does |
| --- | --- | --- |
| `AUTH_SIGNING_PUBKEY` | — | Ed25519 **public** key (base64, 32 bytes) that verifies the SSO cookie. Empty ⇒ every admin request bounces to login |
| `PAGE_COOKIE_SECRET` | — | HMAC key for per-page client-password session cookies. **Startup fails** if empty in production |
| `RAW_TOKEN_SECRET` | — | HMAC key for signed `/raw` render tokens. **Startup fails** if empty in production |
| `API_TOKEN_PEPPER` | — | Server pepper for hashing agent bearer tokens and upload tickets. **Startup fails** if empty in production. **Changing it invalidates every issued token and ticket** |
| `DATABASE_URL` | libpq `PG*` fallback | Postgres connection string. If unset, `internal/db` falls back to standard `PGHOST`/`PGPORT`/`PGUSER`/`PGDATABASE` |

All three HMAC secrets are generated by bootstrap (32 random bytes, hex) and
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
| `PAGES_TRUST_PROXY` | `loopback` | Trusted immediate proxy IPs/CIDRs, comma-separated; `false` disables forwarded client attribution |
| `PAGES_ENV` | `production` in the unit | The systemd unit sets `Environment=PAGES_ENV=production`. Enables the fail-closed secret checks and refuses the dev-login route. `NODE_ENV=production` is still honoured, so an environment file from the Node release keeps working |

The defaults are ElcanoTek's own deployment. Nothing host-specific is compiled
in — these two values are the whole configuration of the split.

### Authentication

| Variable | Default | What it does |
| --- | --- | --- |
| `AUTH_COOKIE_NAME` | `elcano_auth` | Name of the SSO cookie to verify |
| `AUTH_LOGIN_URL` | `https://auth.elcanotek.com` | Where auth failures redirect |
| `ADMIN_EMAIL_DOMAIN` | `elcanotek.com` | Cookie emails in this domain are automatically admins |
| `ADMIN_CSRF_SECRET` | falls back to `PAGE_COOKIE_SECRET`, then `RAW_TOKEN_SECRET` | HMAC key for admin CSRF tokens. Usually leave unset |
| `PAGES_AUTH_MODE` | `elcano` | `central` signs in through a password-mode Auth (§2). Any other value refuses to start |
| `AUTH_ISSUER_URL` | — | Central mode: the Auth origin (`https://auth.example.com`, no path) |
| `AUTH_CLIENT_ID` | `pages` | Central mode: the client ID registered with `auth app create` |
| `AUTH_CLIENT_SECRET` | — | Central mode: that client's secret, 32–256 bytes |
| `AUTH_SIGNING_PREVIOUS_PUBKEYS` | unset | Central mode: comma-separated older Auth keys. Auth's `/jwks.json` is also read at runtime, so a rotation normally needs no edit here |
| `PAGES_PUBLIC_URL` | `https://$DASHBOARD_HOST` | Central mode: the origin in the registered callback URL |
| `PAGES_AUTH_COOKIE_SECURE` / `AUTH_ALLOW_INSECURE_HTTP` | `1` / `0` | Central mode, **local development only**: plain-HTTP Auth and a non-`Secure` cookie |

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
| `RL_SIGNIN_PER_MIN` | `60` | Central mode: `/auth/login` |
| `RL_SIGNIN_CALLBACK_PER_MIN` | `60` | Central mode: `/auth/callback` |
| `RL_SIGNOUT_PER_MIN` | `30` | Central mode: `/logout`, per session (per address without one) |
| `RL_BACKCHANNEL_PER_MIN` | `600` | Central mode: `/auth/backchannel-logout` |

These sit on top of a per-page progressive backoff that is not configurable and
deliberately delays rather than locks out.

Both serving applications use the same client attribution for these limits and
mutation audit records. By default, Pages trusts exactly one loopback proxy hop,
matching the supplied Caddy deployment. Direct local requests without forwarded
headers continue to use their connecting address. Set `PAGES_TRUST_PROXY=false`
when accessing Pages directly without a proxy; forwarded client addresses are
then ignored.

### Payload and upload bounds

These defaults were raised deliberately in the Go release (the Node release
used a 2 MiB request and staged file, 1 MiB data, a 1.5 MB inline budget and
48 KiB chunks). Those numbers dated from clients whose model emitted every byte
as base64, and they turned ordinary multi-megabyte dashboards and data refreshes
into refusals or dozens of tool calls. Each bound still caps what a single call
can make the server hold in memory, so lower any of them on a small box.
Migration `027_raise_upload_limits.sql` widens the matching database CHECKs.

| Variable | Default | What it does |
| --- | --- | --- |
| `MAX_HTML_BYTES` | `32mb` | Whole HTTP request limit for dashboard, `/api/v1`, MCP and raw edits: bytes, or a number with `b`/`kb`/`mb`/`gb` (1024-based, case-insensitive, decimals allowed). Any other value stops startup with an error naming it. Includes RPC fields and `expect`; also the inline deploy and deck-save cap |
| `PAGES_UPLOAD_MAX_BYTES` | `32mb` | One staged upload (chunked, or an upload-ticket PUT), in the same forms as `MAX_HTML_BYTES`. At most `64mb`, which a database CHECK also enforces; an unreadable or larger value stops startup with an error naming it |
| `PAGE_UPLOAD_MAX_CHUNK_BYTES` | `1048576` (1 MiB) | Largest raw chunk one `append_page_upload` accepts, returned as `max_chunk_bytes` and advertised as the `chunk_base64` `maxLength`. Accepts 4096–8388608 (8 MiB, which a database CHECK also enforces) and no more than `MAX_HTML_BYTES` can carry as base64; any other value falls back to 1 MiB and logs a startup `WARNING` naming the value, the reason and the size in effect. The default is held to the same `MAX_HTML_BYTES` bound (4 KiB floor). See below |
| `PAGE_UPLOAD_TICKET_TTL_MINUTES` | `60` | Upload-ticket lifetime; covers a slow PUT of a large file |
| `PAGES_MCP_MAX_INLINE_DATA_BYTES` | `8388608` (8 MiB) | Inline compact JSON transport budget only; data and escaped-envelope limits also apply |

**Choosing `PAGE_UPLOAD_MAX_CHUNK_BYTES`.** Each `append_page_upload` is one
model tool call, and every call re-sends the conversation, so the chunk ceiling
sets how many round trips a staged upload costs. Which value is right depends on
who produces the base64:

- **The client reads a `workspace_file` reference host-side** (Fleet). The bytes
  never touch the model; only the call count matters. The 1 MiB default makes a
  ~1 MB managed-data refresh one append instead of 21; up to `8388608` makes
  larger files one append too. An 8 MiB chunk is about 11 MB of base64 inside
  the JSON-RPC body, which fits the default `32mb` `MAX_HTML_BYTES`; lowering
  `MAX_HTML_BYTES` below what a setting needs makes it fall back, with a startup
  warning saying so.
- **The model writes it** (a client with no file references and no outbound
  HTTP). Every byte is model output, and a large argument is slow and can be
  truncated by the provider. Lower the ceiling for such clients — `49152`
  (48 KiB) was the Node release's default.

The setting is per deployment, so lower it where any agent that stages uploads
has its model emit the base64. Smaller chunks stay valid either way:
`max_chunk_bytes` is a ceiling, not a required size. Upload tickets are
unaffected; they PUT the whole file in one request.

### Managed dashboard-data bounds

| Variable | Default |
| --- | --- |
| `PAGES_DATA_SCHEMA_MAX_BYTES` | `1048576` (1 MiB) |
| `PAGES_DATA_MAX_BYTES` | `8388608` (8 MiB) |
| `PAGES_DATA_CONFIG_MAX_BYTES` | `1048576` (1 MiB) |
| `PAGES_DATA_TEMPLATE_MAX_BYTES` | `16777216` (16 MiB); must exceed `PAGES_DATA_MAX_BYTES`, or it reads as that plus 1 MiB with a startup `WARNING` |
| `PAGES_DATA_SCHEMA_MAX_NODES` | `50000` |
| `PAGES_DATA_SCHEMA_MAX_DEPTH` | `100` |
| `PAGES_SOURCE_FUTURE_TOLERANCE_MS` | `300000` |

These are CPU and memory guards on agent-supplied JSON Schemas and data. Raising
them raises the cost of a single hostile call. Leave them alone unless a real
dashboard is hitting a limit.

`PAGES_DATA_MAX_BYTES` separately bounds compact data and its stored HTML-escaped
envelope, including timestamps. Escaping `<`, `>`, `&`, U+2028 and U+2029 can
expand the stored bytes substantially. Staged files have the separate
`PAGES_UPLOAD_MAX_BYTES` limit; staging cannot increase the data/envelope
capacity. Tool descriptions and data-upload responses report these resolved
startup limits, and the managed-data reads and writes report how much of
`PAGES_DATA_MAX_BYTES` a page's data uses (`data_usage`, [API.md](API.md)).

Byte overrides must resolve to positive safe integers; invalid or nonpositive
values fall back to the defaults, except `MAX_HTML_BYTES` and
`PAGES_UPLOAD_MAX_BYTES`, which refuse to start on a value they cannot read
rather than guess.
Changing settings requires restarting Pages. See [managed-data size measurements](API.md#managed-data-size-limits)
for preflight checks that preserve complete datasets.

### Database bounds

Milliseconds; `0` disables a bound. These exist so one wedged page-lock
transaction cannot pin every pool client and hang both hosts.

| Variable | Default | What it does |
| --- | --- | --- |
| `PG_CONNECT_TIMEOUT_MS` | `5000` | Wait for a pool connection when Postgres is unreachable |
| `PG_STATEMENT_TIMEOUT_MS` | `15000` | Server-side per-statement cap |
| `PG_LOCK_TIMEOUT_MS` | `10000` | Max wait on a row lock |
| `PG_IDLE_TXN_TIMEOUT_MS` | `30000` | Kill sessions idle mid-transaction |
| `PAGES_MIGRATION_LOCK_TIMEOUT_MS` | `30000` | Wait for another migrator (§8); `0` fails immediately |

### Theming

| Variable | Default | What it does |
| --- | --- | --- |
| `FLAG_ASSETS_BASE` | `/assets/_v/<version>/flag` | Base path themed pages load the vendored Flag assets from. Unset, it is the versioned, immutable-cached mount. Set it only to serve a copy from another path on the content host: it must be a same-origin path of letters, digits and `. _ ~ / -` with no `..` segment (the content CSP allows only `'self'`), and startup refuses anything else |

### Never set these in production

| Variable | Why |
| --- | --- |
| `PAGES_DEV_LOGIN=1` | Exposes `GET /__dev/login`, which sets an admin cookie **with no authentication at all**. Under `PAGES_ENV=production` the server ignores it, logs a warning and does not mount the route |
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
blocks above. Answer `n` to the Caddy prompt. A proxy connecting from outside
loopback also needs its actual connecting IP or CIDR in `PAGES_TRUST_PROXY`, for
example `192.0.2.10/32,2001:db8::10/128`. Use only the proxy's addresses, not a
blanket trusted network.

The immediate proxy must replace `X-Forwarded-For` with the connecting client's
address, or append that address as its last entry. Pages uses that last entry
only when the socket peer is trusted, and never walks earlier forwarded hops.
For a chain of proxies, the immediate proxy must normalize this value to the
verified original client; adding more trusted hops in Pages is unsupported.
`X-Forwarded-Host` does not select the serving zone: preserve the original `Host`.

`firewalld`, if active, gets `http` and `https` opened permanently.

---

## 7. Service management

The unit is `deploy/pages.service`, rendered with the installation's account,
application directory and environment file: `Type=simple`, runs
`/opt/pages/pages serve` as `pages:pages` from `/opt/pages` with
`EnvironmentFile=/etc/default/pages` and `PAGES_ENV=production`,
`Restart=always` with `RestartSec=5`, `After`/`Requires=postgresql.service`,
and a hardening block — `NoNewPrivileges`, `PrivateTmp`, `PrivateDevices`,
`ProtectSystem=strict`, `ProtectHome`, `ProtectControlGroups`,
`ProtectKernelModules`, `ProtectKernelTunables`, `LockPersonality`,
`MemoryDenyWriteExecute` (a Go binary never needs writable and executable
memory), and `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6`.

`ProtectSystem=strict` makes the filesystem read-only apart from
`ReadWritePaths=/opt/pages/assets`. If a future feature needs to write
somewhere else, add a `ReadWritePaths` line rather than relaxing
`ProtectSystem`.

### The operator CLI

`/usr/local/bin/pages` is a link to the active release's binary, so the CLI and
the server are always the same code; `pages help` lists the verbs and
`pages <verb> --help` explains each. Every verb that touches the system
re-invokes itself through `sudo`, keeping the installation settings.

| Command | What it does |
| --- | --- |
| `pages start` / `stop` / `restart` / `status` | `systemctl` on `pages.service` |
| `pages logs` | `journalctl -fu pages.service` (follow) |
| `pages logs <journalctl args…>` | Passes arguments through, e.g. `pages logs -n 200 --since '1 hour ago'` |
| `pages update` | Fetch, fast-forward, build a release, preflight, migrate, switch, require `/readyz`, roll back on failure. §10 |
| `pages rebuild` | The same for the current checkout **without fetching**. §11 |
| `pages doctor` | Diagnose the box; `--check` is read-only and exits 1 on a failure. §13 |
| `pages migrate` | Apply pending migrations. §8 |
| `pages env` | Print `/etc/default/pages` with secrets redacted. §12 |
| `pages tls status` \| `reload` \| `restart` | Caddy state, `caddy reload`, or restart the unit |
| `pages token add <label> [scope] [slug…]` | Mint an agent token. §9 |
| `pages token list` | List tokens: prefix, label, scope, state, last use |
| `pages token revoke <id>` | Revoke; the server 401s it immediately |
| `pages template list` \| `show` \| `register` \| `sync` | Page templates. See [TEMPLATES.md](TEMPLATES.md) |
| `pages access grant` \| `revoke` \| `list <email>` | Central mode's access list (§2) |
| `pages refresh-schedule list [--all]` | Each scheduled page's refresh health, schedule and owner (`--all` adds unscheduled pages) |
| `pages refresh-schedule set <slug> --cron '<cron>' [--owner <email>]` | Set the UTC cron a page's refresh health is judged against, e.g. `--cron '0 15 * * 1-5'`; `--owner` alone changes only the owner. Audited as `pages-cli:<you>` |
| `pages refresh-schedule clear <slug>` | Remove the schedule and owner: the page is no longer judged |
| `pages backup [dest]` | Capture recovery inputs and rehearse a restore before completion |
| `pages check-integrity <backup-dir>` | Verify checksums and repeat an isolated restore/application check |
| `pages version` | The build version, revision and Go version |
| `pages help` | Usage |

`pages token`, `pages template`, `pages access` and `pages refresh-schedule` connect with `DATABASE_URL`
and (for tokens) hash with `API_TOKEN_PEPPER` from the environment they run in,
so they must run with the **service's** environment file, as the `pages` user.
On an installed box they arrange that themselves: when neither `DATABASE_URL`
nor `API_TOKEN_PEPPER` is set and the environment file exists, they re-run as
the service account (`runuser` from root, `sudo -u pages` otherwise) and read
`/etc/default/pages` — parsed, never sourced, and never passed on a command
line. So this is all it takes:

```bash
sudo pages token add ci-publisher deploy
sudo pages template sync
```

With `DATABASE_URL` or `API_TOKEN_PEPPER` already in the environment (`pages
dev`, the test cluster, bootstrap and `pages update`'s own calls) they use it
as is. That wiring is load-bearing: a token minted with a different
`API_TOKEN_PEPPER` than the server verifies with is a token the server rejects.

---

## 8. Migrations

`migrations/*.sql`, embedded in the binary and applied in filename order by
`pages migrate` (`internal/migrate`), tracked in a
`schema_migrations` table by filename. Idempotent: already-applied files are
skipped. Per-file transactions protect partial changes, but do not imply that
every migration is compatible with running predecessor code. Automatic updates
require explicit classifications in `migrations/compatibility.json` (§10).

Bootstrap and `pages update` both run them. To run them by hand:

One database-wide advisory lock serializes migration runners before they read
or create the tracking table, and stays held across each file's transaction.
A concurrent runner waits up to 30 seconds, then exits with `MIGRATION_BUSY`
and a retry instruction. Set `PAGES_MIGRATION_LOCK_TIMEOUT_MS` to adjust this
wait (`0` means fail immediately). Ordinary page reads and writes do not take
this lock. A failed file rolls back and releases the lock; already committed
files remain applied and are skipped on retry.

```bash
sudo runuser -u pages -- bash -c \
  'set -a; . /etc/default/pages; set +a; exec /opt/pages/pages migrate'
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
pages update            # prompts before the switch
PAGES_UPDATE_YES=1 pages update
```

`pages update` serializes updates to one installation with a file lock
(`/opt/pages.update.lock`, which `pages backup` takes shared), fetches and
fast-forwards the source checkout, and builds a complete candidate release
under `/opt/pages.releases` (`make build` into the release directory). It then
hands over to the **candidate** binary, so the code that preflights, migrates,
renders the systemd unit and judges readiness is always the code being
installed. Before the active service is touched, the candidate runs, as the
`pages` user with the service environment:

1. `pages preflight` — loads the production configuration the way `serve`
   would (secrets, database settings, limits, the embedded trees) without
   listening or writing;
2. `pages check-migrations` — only pending migrations explicitly marked
   backward-compatible in `migrations/compatibility.json` may run
   automatically;
3. `pages migrate`.

Any failure there leaves the installed service untouched. After success the
updater snapshots the installed service and CLI files, stops Pages, and
atomically switches `/opt/pages` to the new release. It starts Pages and
requires `/readyz` on the configured port. A failed activation, service start or
readiness check switches back to the predecessor, restores the service/CLI
files and restarts it. The update still exits nonzero so automation records the
failure. If the machine cannot restart even the predecessor, the error names
the retained release and configuration backup for manual recovery.

A legacy `/opt/pages` directory (including a Node-era installation) is
converted into a retained release on the first update. Writable assets move to
`/opt/pages.assets`; an existing local `.env` moves to `/opt/pages.env`. Every
release links to those shared paths; the service environment file stays in
`/etc/default/pages`. Releases and failed candidates are retained for
inspection, not pruned automatically. The last successful predecessor is also
linked at `/opt/pages.releases/previous`. Backups must keep the shared
assets/environment and database, not only the active release link. Paths follow
the configured application directory for non-default installs.

A box still running the Node release's shell updater reaches the new
`scripts/update.sh` after its own fetch; that bridge builds the binary from the
freshly pulled checkout and hands over to `pages update`, which performs the
conversion above.

There is a brief stop/start outage at activation. Template sync happens only
after readiness and remains nonfatal (`PAGES_SKIP_TEMPLATE_SYNC=1` skips it).
After a successful update the login banner `/etc/motd` is re-installed from the
source checkout's `deploy/motd` when it differs; bootstrap installs it on every
run, replacing whatever the image shipped.
Rerunning bootstrap on this release layout refreshes instance configuration,
service/CLI and Caddy settings while preserving the active code release; use
`pages update` or `pages rebuild` to change code. If the active release's
binary (or a directory above it) is not root's alone, bootstrap stops instead:
root runs that binary, including as `pages update`, so do not run it. Remove
the application link (`sudo rm /opt/pages`) and re-run
`sudo bash scripts/bootstrap.sh` from the root-owned checkout, which installs
a fresh root-owned release built from that checkout.

## 11. Rollback

Automatic rollback restores code and installed control files; it does **not**
undo database migrations, uploaded assets, template registrations or authored
page data. The automatic update path requires the already-applied
`022_page_upload_attempts.sql` baseline and a reviewed backward-compatible
classification for every pending later migration. Unknown or incompatible
migrations stop the updater before it changes the service.

For an older installation or an incompatible migration, take a database dump,
back up assets and environment, and perform a planned manual upgrade with the
service stopped. Its recovery boundary is restoration of that matching database
backup and predecessor code together. Do not start predecessor code against an
unreviewed newer schema; no down-migration is inferred or attempted.

After a successful compatible update, the retained predecessor can also be
restored manually. First verify that no later manual migration has changed its
compatibility boundary. Using the default paths:

```bash
pages stop
sudo ln -s "$(readlink -f /opt/pages.releases/previous)" /opt/pages.restore
sudo mv -Tf /opt/pages.restore /opt/pages
pages start
curl -fsS http://127.0.0.1:3002/readyz
```

For a predecessor from before `/readyz` existed, its liveness endpoint is
`/healthz`; confirm database connectivity/schema separately. Automatic recovery
handles this legacy case with the old HTTP liveness endpoint plus the new
release's bounded database readiness probe (`pages ready-check`). If installation settings changed,
restore the corresponding service/CLI files from the `.recovery.*` directory
reported by the updater before starting, and run `systemctl daemon-reload`.

`pages rebuild` still builds the current source checkout without fetching. It
uses the same preflight, compatibility checks, retained releases and rollback
behavior; it is not a database downgrade command.

---

## 12. Inspecting configuration

```bash
pages env
```

Prints `/etc/default/pages` with secrets redacted (`internal/ops/envfile`,
line by line, never evaluating the file): any key matching
`TOKEN|KEY|SECRET|PASSWORD|PEPPER`, plus `DEV_ADMIN_COOKIE`,
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

Both hosts also answer `GET /readyz`: `200 {"status":"ready"}` after PostgreSQL
connects, core rendering tables can be read and every migration shipped with
this release is recorded. A failed database or incomplete schema returns
`503 {"status":"not_ready"}` without database details. Responses are not cached.
Probes share one separate database connection, coalesce simultaneous requests,
and bound connection/query waits to one second each (up to three seconds total).
Bootstrap and update require readiness before reporting deployment success;
liveness remains appropriate for detecting whether the HTTP process is running.

```bash
curl -fsS http://127.0.0.1:3002/healthz                      # local
curl -fsS https://pages.example.com/healthz                  # through Caddy
curl -fsS https://example-pages.com/healthz
curl -fsS http://127.0.0.1:3002/readyz                      # database/schema ready
```

For a monitor that actually proves the app is serving, add an authenticated
read:

```bash
curl -fsS -H "Authorization: Bearer $PAGES_TOKEN" \
  https://pages.example.com/api/v1/pages >/dev/null
```

That one touches the database. `pages mcp-smoke` does the equivalent for the
MCP surface (initialize, tools/list, a `deploy_page` and a `get_page`; set
`PAGES_MCP_URL`, `TOKEN` and `SLUG` — it really deploys that slug). For the box
as a whole, `pages doctor --check`
walks every prerequisite — the binary and CLI link, the environment file's
presence, mode and required keys, the two-domain split, PostgreSQL, the service
(active, enabled, running the current release), `/readyz`, Caddy and verified
TLS for both hostnames, free disk, the OS support window, pending updates and
reboots, and the source checkout — and exits 1 on a failure (`--strict`: on a
warning too). It runs as the service user and changes nothing. Plain
`sudo pages doctor` also makes the safe repairs: environment-file ownership and
mode, starting a stopped service, restarting a stale release. It never pulls,
upgrades, migrates or rebuilds; that stays `pages update`.

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

`pages backup [dest]` captures a complete recovery set and rehearses a restore
before reporting success. `pages check-integrity <backup-dir>` verifies every
captured file and repeats the same restore check. It first copies the backup
directory into a private, root-only temporary snapshot (regular files and
directories only, never following links) and verifies and restores that
snapshot, so a file swapped in the backup after it verifies is never read; allow
free space for one temporary copy of the backup under `$TMPDIR` (default
`/tmp`). Both commands work with the
persisted installation paths, including the shared assets and local `.env`
symlinks used by managed releases.

### Backup and verification

```bash
pages backup /var/backups/pages
# JSON result: {"backup_dir":"/var/backups/pages/pages-backup-..."}
pages check-integrity /var/backups/pages/pages-backup-...
# JSON result: {"status":"verified","backup_dir":"...","restored":{...}}
```

Omitting the destination uses `/var/backups/pages`. Each success creates a new
private directory with these inputs:

| Input | Captured contents |
| --- | --- |
| `database.dump` | PostgreSQL custom-format dump: all tables, immutable versions, templates, tokens, portals, audit and sequences; ownership/ACLs omitted for explicit assignment on recovery |
| `assets/` | Contents of the resolved writable assets directory, including files referenced by `assets.sha256` |
| `application/` | The actual installed release: the `pages` binary, which carries its own front end, migrations and templates. Mutable paths (`assets`, `.env`) are archived in their own sections |
| `config/service.env` | Required service environment, preserved byte for byte |
| `config/install.env`, `config/local.env` | Installation settings and resolved local `.env`, when present; absence is recorded |
| `manifest.json` | Format/version, capture interval, runtime limits, file lengths/SHA-256, database counts/migrations and restore-probe results |

The command takes a shared lock against `pages update`, snapshots configuration,
dumps the database **before** copying assets, and checks that configuration did
not change during capture. It does not stop Pages or mutate live database rows.
Normal PostgreSQL dump reads still use database/storage resources; schedule
accordingly. Do not run manual migrations, asset garbage collection or external
configuration changes during capture. Source checkout HEAD is not treated as
proof of the installed revision: the actual release files are retained.

Files are written under `.pages-partial-*`, checked, flushed and renamed only
after successful verification. Failure exits nonzero and removes its incomplete
capture. The destination must be outside the application and assets directories,
and it and every directory above it must be owned by root (or the invoking
account) and not writable by other accounts — sticky directories such as `/tmp`
excepted above it — so the service account cannot redirect what root writes; the
partial tree is created `0700`. Only the documented top-level assets/local-env
symlinks are resolved; nested symlinks and special files fail explicitly instead
of creating incomplete copies. Each entry of the service-writable assets tree is
opened relative to its already-open parent without following links, checked to
be a regular file or directory by the opened descriptor, and copied for exactly
the length it had when opened, so an entry swapped for a link, FIFO or device
mid-capture is refused rather than read.
An abrupt machine loss may leave a hidden partial directory; it is never a
completed backup and can be removed after checking that no capture is running.

The restore rehearsal requires PostgreSQL server/client tools (`initdb`,
`pg_ctl`, `createuser`, `createdb`, `pg_dump`, `pg_restore`) compatible with the
source database, and enough local disk for an additional restored database.
Root delegates its temporary server to the `postgres` account; an unprivileged
operator runs it as their own account. It uses a private short `/tmp` Unix socket,
no TCP listener, a new database and synthetic application secrets. It never
loads the archived environment or accepts a destination database URL. The
scratch cluster's bootstrap superuser only creates an ordinary login role (no
superuser, role or database creation, and none of `pg_execute_server_program`,
`pg_read_server_files` or `pg_write_server_files`) and a database it owns; the
dump is restored, migrated and probed as that role, so a crafted
`database.dump` cannot run server programs or read server files.

Nothing from a backup is ever executed. A backup's files are only as
trustworthy as its `manifest.json`, which is just as writable, and the
commands run as root. When the captured binary's SHA-256 equals a **trusted
installed binary** — the running `pages`, or a release binary under
`/opt/pages.releases` that only root can change — that installed binary runs
the probe (`<binary> backup-probe`), so the rehearsal uses exactly the release
that was backed up, and the result names it in `"probed_with"`. Otherwise (the
release was since removed, or the backup was altered) the restored snapshot is
inventoried as captured, the scratch copy is migrated, and **this** release
serves it; the result carries `"notice":"captured binary not executed; served
with this release"` and `"served_with"`. The live and recovered databases are
never migrated by the rehearsal.

Verification checks every page/template source hash, referenced asset bytes,
page/version and template/revision ownership, schema readiness, and representative
HTTP page, version, template-preview and portal/member requests (a
passwordless page that belongs to a live portal answers the portal gate's
`401`, as it does in production). Active content
is preferred in the bounded HTTP sample; hashes cover all immutable source rows.
Retired/disabled content, published drafts and a portal home outside its membership
remain valid states. Browser JavaScript is not executed by this recovery check.
The returned counts and migration inventory come from the restored snapshot,
so concurrent authoring cannot make them disagree with the dump.

The recovery directory contains credentials and is created with mode `0700`.
Keep encrypted off-host copies, retention and scheduling under operator control;
no remote destination or cron entry is installed automatically. For example:

```cron
0 3 * * * root /usr/local/bin/pages backup /var/backups/pages
```

The service environment must define `RAW_TOKEN_SECRET`, `API_TOKEN_PEPPER` and
`PAGE_COOKIE_SECRET`; an incomplete file is refused even when the calling shell
has values. Preserve these values when restoring, so token hashes and client
sessions keep working. Configuration files are literal copies: any external
files they source, TLS/proxy configuration and remote services remain separate
operator recovery inputs.

### Production recovery

First run `pages check-integrity` on the selected backup. Provision a matching
PostgreSQL version and the application role, and restore into a **new** database
so the previous database remains available until the recovery is accepted:

```bash
sudo runuser -u postgres -- createdb -O pages pages_recovered
sudo cat /path/to/database.dump | sudo runuser -u postgres -- pg_restore \
  --exit-on-error --single-transaction --no-owner --no-acl --role=pages --dbname=pages_recovered
```

The pipeline streams the private dump through standard input without opening
the recovery directory to the PostgreSQL account. Use the configured application
role for a non-default installation.

Do not install `application/pages` from the backup unless `check-integrity`
reported `"probed_with"`: only then is it byte-identical to a trusted installed
release, and that release directory (root-owned, under `/opt/pages.releases`)
is the one to activate. Otherwise reinstall the release from source — check out
the revision `manifest.json` records in `pages_version` in `/opt/pages-src` and
run `pages rebuild` — or recover with the current release as for a Node-era
backup below. Stop Pages before switching the
active release, assets and environment together. Restore `assets/` into the
configured shared assets directory, link the staged release to it, restore the
optional shared `.env`, and restore installation/service configuration
(`config/service.env`, `install.env`, `local.env`) with its original ownership
and permissions (normally service env `0640`, `root:pages`). Change only the
recovered database target as needed; retain the original HMAC secrets.
Reinstall the captured release's service/CLI settings if those changed, then
switch the application link and restart.

Verify `/readyz` and representative page/portal reads before retiring the previous
release or database. Recovery does not automatically upgrade or downgrade schema;
perform any later migration through the separately reviewed update procedure.

### Node-era backups

A backup taken by the Node release records `node_version` in `manifest.json`,
and its `application/` holds `server.js` and `package-lock.json` instead of a
binary. `pages check-integrity` accepts it (reporting `"format":"node"`): it
verifies the application files against the manifest only and rehearses the
restore by serving the restored data with the **current** release.

To recover one, restore the database, `assets/` and `config/` exactly as above,
but **do not** stage `application/` or run npm: keep the current Go release
installed, point `DATABASE_URL` in the restored service environment at the
recovered database, and run `pages migrate` with that environment to add the
newer migrations (`/readyz` answers `503` until then); then restart. Keep the
Node-era backup until a new `pages backup` of the recovered installation has
verified. `pages backup --help` carries the same procedure.

---

## 15. Troubleshooting

### `pages status` shows the service restarting in a loop

`pages logs -n 50`. In production the server refuses to start on an empty
secret, and the message names the variable:

```
pages serve: RAW_TOKEN_SECRET is required in production (an empty key lets anyone forge /raw access tokens)
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

`/assets` sets its cache policy explicitly: `/assets/_v/<version>/…` (what
Pages links) is `immutable` for a year, and the stable `/assets/flag/…` paths
revalidate. If you put a CDN or another proxy in front of the content host,
make sure it is not stripping `Cache-Control` or the CSP. Check
`FLAG_ASSETS_BASE` is unset (or is a same-origin path to a copy you maintain).

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

### `pages update` dies with "fast-forward failed — resolve the source checkout first"

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

1. **`pages bootstrap` is Fedora/RHEL-only.** It hard-codes `dnf`,
   `postgresql-setup --initdb`, `/var/lib/pgsql/data` and `firewall-cmd`. On
   Debian or Ubuntu it fails at the first `dnf`. The application itself is
   portable — the packaging is not. Provision by hand there, following §3 and §4
   as a specification.
2. **No signing service.** `AUTH_SIGNING_PUBKEY` is mandatory for the admin UI
   and nothing in this repository can produce a matching cookie for production.
   Plan for that before you deploy.
3. **No token rotation verb.** Mint, cut over, revoke (§9).
4. **`pages update` has a brief outage** at the swap. Not zero-downtime.
5. **No down-migrations.** Code rollback across a migration is not always safe
   (§11).
6. **Readiness does not exercise every application operation.** `/readyz`
   checks connectivity and schema; monitor specific read/write flows separately
   when needed (§13).
7. **The `assets` table has no upload API.** `ReadWritePaths=/opt/pages/assets`
   exists for a feature that is not finished.

---

## Related

- [SECURITY.md](SECURITY.md) — the threat model. Read before operating
- [../SECURITY.md](../SECURITY.md) — how to report a vulnerability
- [API.md](API.md) — REST and MCP agent surfaces
- [LICENSING.md](LICENSING.md) — what you may and may not deploy
- [../CONTRIBUTING.md](../CONTRIBUTING.md) — local development

### Application-route collision check

Migration `023_reserve_application_routes.sql` checks active pages for the newly
reserved `raw-template`, `preflight`, `edit-token` and `readyz` segments before
rollout. It stops with affected slugs and changes no pages or versions. Creation
and restoration reject these namespaces consistently; ordinary nested slugs keep
their URLs.

If it reports an old conflicting slug, retain the previous release while you
copy its source to an ordinary new slug, verify publication and client access,
and update shared links. Soft-delete the old page only after verification, then
retry the update. This is an explicit operator migration of a URL; Pages never
silently renames it. Keep the old page history and previous release until the
move is accepted so the previous installation can restore it if necessary.
