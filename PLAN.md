# Pages — Build Plan

> Quick client pages and dashboards. A small, single-server platform that lets our
> agents (chat, cutlass) and humans **deploy, version, preview, publish, and roll
> back** password-protected, **Flag-themed** HTML pages, with an Elcano-admin
> review/approval/rollback workflow and an MCP/API surface for agents.

Inspired by [Shopify's "Quick"](https://shopify.engineering/quick): *"drop in a
folder of HTML and get back a secure URL only our people can see."* We borrow the
concept (instant deploys, auth at the edge, versioned content, one cheap VM) but
**not** the GCS/IAP plumbing — one Fedora/RHEL box, our own auth, Postgres-backed
version control, and the Flag design system as the default look.

> **Status:** revised after two adversarial reviews (OpenRouter Fusion panel) and
> Brad's theming + editing requirements. Settled decisions: untrusted content origin
> is **mandatory day one**; version model is **pointer-is-truth**; **GrapesJS and
> contenteditable editing are both rejected** in favor of *source* editing; we add an
> **audit log, backup-integrity checks, signed preview links, rate limits, a takedown
> switch**, and **per-client theming defaulting to Flag**.
>
> **The governing invariant (do not violate):** *the editor edits stored source HTML;
> the rendered iframe is only a preview.* Flag theming is injected at render time and
> never persisted. This single rule is why both GrapesJS and contenteditable are out
> (they save rendered DOM, corrupting agent chart markup) and why charts survive edits.

---

## 1. What we're building (the product)

The next step after *"make me an email"* and *"make me a slide deck."* It's *"make
me a dashboard (in HTML)"* — but **persistent, themed, and editable**, so we can
later say *"update the omnicom dashboard"* instead of regenerating it. That's the
cost saver.

Three access paths (note the two domains — see §7):

| URL | Who | Auth |
|-----|-----|------|
| `dashboard.elcanotek.com/view/<slug>` | Client (e.g. Omnicom) | Per-page password **or** Elcano SSO |
| `dashboard.elcanotek.com/admin/<slug>` | Elcano team | Elcano SSO (`auth.elcanotek.com`) → auto-admin |
| `dashboard.elcanotek.com/api/v1/*` and `/mcp` | Agents (chat, cutlass) | API bearer token |
| `<content-domain>/raw/...` | (internal) rendering target | Signed short-TTL token (no cookies) |

**Admin mode** (`/admin/<slug>`): list all versions (incl. drafts/pending),
preview any version in a sandboxed frame, publish, **roll back**, **approve/reject
pending edits**, **fix small things inline** (Quick Edit — §8), set the client
theme, set/clear the page password, and **take a page down**.

**Approval workflow (optional, per page).** A page can be marked
`require_approval`. When on, anything an agent (or future editor) submits lands as a
**`pending`** version that does *not* auto-publish — it queues for an Elcano admin.
Approve → publish; reject → terminal. Off (default) → agents may publish directly.

**Theming.** Every page renders in the **Flag design system** by default (§8). Each
client can get a brand theme (a small token override + logo). Agents that push
fully self-contained "random" HTML are fine too — that's an explicit `raw` mode.

---

## 2. Decisions (locked)

1. **Stack: Node/Express**, copying `home`'s Elcano auth **verbatim** (auth is the
   riskiest code to re-implement; match home's framework exactly so it drops in).
2. **Versioning store: Postgres** (no GitHub middleman), **pointer-is-truth** model
   (§5). Postgres over SQLite specifically for per-page row locking + partial unique
   indexes.
3. **Default theme: Flag** (`/root/flag/design-system`), vendored locally; per-client
   brand overrides; agents may opt into self-contained `raw` mode (§8).
4. **JS/CSS in pages: curated local vendored palette, no external CDNs.** Rendering
   libs only in the untrusted zone (Chart.js / ECharts / Flag tokens+fonts). **htmx
   stays out of the untrusted zone** (it's a request-engine gadget — review §7).
5. **Editing: edit the stored *source*, never the rendered DOM.** The author is the
   agent (via MCP); admins need to fix small things (a wrong number, a typo). v1 ships
   a **raw-source textarea with live preview** (edits the version's stored HTML,
   lossless, saves a new draft). A nicer **server-side text-node patch** ("click the
   number, server patches just that one text node in the parsed source") comes next.
   **No `contenteditable`/`outerHTML` round-trip** (it serializes post-script DOM and
   corrupts charts) and **no drag-drop builder (GrapesJS)** — both rejected in review
   because they mutate rendered output instead of source (§8).

---

## 3. Architecture overview

```
        dashboard.elcanotek.com                      <content-domain>  (separate
        (Caddy vhost, auto-TLS)                       registrable domain, own cookie
                 │                                     jar — e.g. elcano-pages.io)
                 ▼                                              │
   ┌──────────────────────────────┐                            ▼
   │   pages (Node/Express)        │  one process, 127.0.0.1:3002, two vhosts
   │                               │
   │  Host: dashboard.*            │   Host: <content-domain>
   │   auth (copied from home)     │    /raw/<slug>?t=<signed>   ← sandboxed render
   │   /view  /admin  shells       │    /assets/*  (flag tokens, fonts, chart libs)
   │   /api/v1/*   REST            │    • verifies HMAC token (slug+version+exp)
   │   /mcp        MCP-over-HTTP   │    • NEVER reads the SSO cookie
   │   mints signed /raw URLs ─────┼──▶ • sets CSP `sandbox` HEADER + frame-ancestors
   └───────────────┬───────────────┘    • cookieless ⇒ cookie-tossing can't reach SSO
                   │
                   ▼
         ┌──────────────┐    /opt/pages/assets/<sha256>  (content-addressed, on disk)
         │  Postgres    │
         │  pages db    │
         └──────────────┘
```

- **One server, one process — but two registrable domains.** Caddy serves both
  hostnames to the same Node app; the app branches on `Host`. The dashboard host is
  the trusted auth zone; the content host serves only `/raw` + `/assets`, never
  touches the SSO cookie, and lives in a **different cookie jar** (different
  eTLD+1). This is what makes serving untrusted agent HTML safe (§7). We keep
  "one box" while getting a real origin boundary.
- **Assets** are content-addressed files under `/opt/pages/assets/<sha256>`; no
  external object store.

---

## 4. Repo layout

```
pages/
  server.js                 # Express app; vhost split (dashboard vs content host)
  lib/
    auth.js                 # verifyElcanoSession, requireAuth, requireAdmin  (← home)
    db.js                   # pg pool + query helpers; transactional mutations
    versions.js             # deploy/publish/approve/reject/rollback (pointer-is-truth)
    pagecookie.js           # signed per-page password sessions
    rawtoken.js             # mint/verify signed short-TTL /raw access tokens
    render.js               # inject flag tokens + client theme into served HTML
    themes.js               # theme CRUD; flag is the seeded default
    csp.js                  # per-zone CSP (shell vs content) builders
    audit.js                # txn-coupled audit writes
    mcp.js                  # MCP-over-HTTP (tools → same internal fns as REST)
    migrate.js
  migrations/               # numbered raw .sql (like moc)
    001_init.sql
  shell/                    # trusted Flag-themed shell UI (dashboard host only)
    view.html  admin.html  admin.js  shell.css
  vendor/flag/              # pinned copy of /root/flag/design-system (see scripts/sync-flag.sh)
    tokens/ fonts/ icons/ theme/ ...
  public/assets/vendor/     # rendering libs for the content zone
    chart.umd.min.js  echarts.min.js
  scripts/
    bootstrap.sh  update.sh  sync-flag.sh  lib/envfile.sh
  deploy/
    pages.service  pages-cli  Caddyfile   # Caddyfile has BOTH vhosts
  docs/
    API.md                  # REST + MCP + theming guide for agents
  .env.example  package.json  PLAN.md
```

Tech: Node 20+, Express 5, `helmet`, `pg`, `@modelcontextprotocol/sdk`
(MCP-over-HTTP), `express-rate-limit`. No ORM — raw SQL. No client build step
(Flag tokens are plain CSS; shell is hand-written).

---

## 5. Data model & version control (Postgres) — *pointer-is-truth*

No GitHub middleman. **"Live now" = whatever `pages.published_version_id` points
at** (pointer-is-truth — removes the "two live rows" race by construction). Stored
`status` tracks the review lifecycle: `draft | pending | approved | rejected`. The
pointer may only reference an **`approved`** version, so "blessed/publishable" is an
explicit, queryable state (not implied). Content (`html`) is immutable per row —
every edit/deploy inserts a **new** row — so the table is naturally append-only; we
enforce that at the DB (below). Only `status`/review columns ever mutate.

```sql
-- 001_init.sql
CREATE TYPE version_status AS ENUM ('draft','pending','approved','rejected');
CREATE TYPE render_mode    AS ENUM ('themed','raw');

CREATE TABLE pages (
  id                BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  slug              TEXT UNIQUE NOT NULL,              -- 'omnicom' or 'omnicom/q2' (see §13)
  title             TEXT NOT NULL DEFAULT '',
  client_id         TEXT,                              -- tenancy hook (no UI in v1)
  theme_id          BIGINT REFERENCES themes(id),      -- NULL ⇒ Flag default
  password_hash     TEXT,                              -- bcrypt; NULL = Elcano-only
  require_approval  BOOLEAN NOT NULL DEFAULT false,
  disabled          BOOLEAN NOT NULL DEFAULT false,    -- takedown kill switch (§7)
  published_version_id BIGINT,                         -- FK → page_versions.id; the ONLY truth
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at        TIMESTAMPTZ
);

CREATE TABLE page_versions (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  page_id     BIGINT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
  html        TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,                        -- dedupe / integrity
  status      version_status NOT NULL DEFAULT 'draft', -- draft|pending|approved|rejected
  render_mode render_mode NOT NULL DEFAULT 'themed',   -- themed (flag injected) | raw (verbatim)
  author      TEXT NOT NULL,                           -- email or agent token label
  source      TEXT NOT NULL DEFAULT 'api',             -- api | mcp | admin
  note        TEXT,
  reviewed_by TEXT, reviewed_at TIMESTAMPTZ,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON page_versions (page_id, created_at DESC);
-- real FK, deferrable so pointer-set + insert can share a txn; RESTRICT so a live
-- version can't be deleted out from under the pointer.
ALTER TABLE pages ADD CONSTRAINT pages_pubver_fk
  FOREIGN KEY (published_version_id) REFERENCES page_versions(id)
  ON DELETE RESTRICT DEFERRABLE INITIALLY IMMEDIATE;
-- DB-enforced append-only on content: block any UPDATE to html/content_sha256/page_id,
-- and any DELETE, via a BEFORE trigger. (Status/review columns remain mutable.)
-- "version belongs to page" is enforced in app code on every pointer move.

CREATE TABLE themes (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name         TEXT UNIQUE NOT NULL,                   -- 'flag' (seeded default), 'omnicom', ...
  override_css TEXT NOT NULL DEFAULT '',               -- redefines brand tokens (§8)
  logo_sha256  TEXT,                                   -- optional brand logo asset
  default_mode TEXT NOT NULL DEFAULT 'system',         -- light|dark|system (flag contract)
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE assets (
  sha256 TEXT PRIMARY KEY, content_type TEXT NOT NULL, bytes BIGINT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (                               -- agent credentials (NOT bcrypt)
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  label TEXT NOT NULL,
  prefix TEXT NOT NULL,                                 -- e.g. 'agt_live_ab12' for lookup/display
  token_hash TEXT NOT NULL,                             -- HMAC-SHA256(token, server pepper)
  scope TEXT NOT NULL DEFAULT 'deploy',                 -- agents create draft/pending only; never move pointer
  last_used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(), revoked_at TIMESTAMPTZ
);

CREATE TABLE preview_links (                            -- client review w/o SSO (§7,§10)
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  page_id BIGINT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
  version_id BIGINT NOT NULL REFERENCES page_versions(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL,                             -- store hash, not token
  expires_at TIMESTAMPTZ NOT NULL, revoked_at TIMESTAMPTZ,
  created_by TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (                                -- written in the SAME txn as each change
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor TEXT NOT NULL, actor_type TEXT NOT NULL,        -- user | agent | system
  action TEXT NOT NULL,                                 -- deploy|publish|approve|reject|rollback|...
  page_id BIGINT, version_id BIGINT, ip TEXT,
  metadata JSONB, created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

### Version state machine (explicit)

| Action | Legal source | Effect |
|--------|--------------|--------|
| deploy (no approval) | — | INSERT `draft` |
| deploy (require_approval) | — | INSERT `pending` (publish flag ignored) |
| publish | `draft` | set `approved`, move pointer → this version |
| approve (admin) | `pending` | stamp reviewer, set `approved`, move pointer → this version |
| reject (admin) | `draft`/`pending` | set `rejected` (**terminal** — clone to reuse) |
| rollback (admin) | any **`approved`** version of this page | move pointer → that version (no status change) |

- **Publish/approve/rollback all move the pointer in one txn:** `UPDATE pages SET
  published_version_id = $v …` after validating the version belongs to the page and
  is/becomes `approved`. The **pointer only ever references an `approved` version**;
  no two-live-rows window.
- **`rejected` is terminal** (audit/governance hygiene for agency clients); to ship
  rejected content, clone it into a new draft.
- **Agent authority depends on the gate.** On an **approval-gated** page an agent can
  only create `pending` — it cannot publish/approve/rollback (human-only). On an
  **open** page an agent may publish and rollback (the "make/update the dashboard and
  it's live" fast path). `approve`/`reject`/`disable`/`approval-toggle` are **always**
  admin-only, regardless of gate.
- **"Archived"/history** is derived: any `approved` version that isn't the current
  pointer is a rollback target.
- **Concurrency:** every mutation runs in a txn that does `SELECT … FROM pages WHERE
  id=$page FOR UPDATE` first, serializing writers per page. Append-only deploys
  never conflict; they only contend at publish, which the row lock serializes.
- **Optimistic concurrency:** mutation requests carry the version the caller thinks
  is live; mismatch ⇒ `409` "reload," so a rollback can't silently clobber a
  colleague's concurrent action.

> **Why not git-on-server?** (Confirmed by review.) Git buys diffs/history but adds
> moving parts, concurrency hazards, and an awkward admin-query story. The pointer
> model gives identical rollback guarantees against a schema the admin UI reads
> directly. Diffs, if wanted, are `diff(htmlA, htmlB)` over two rows.

---

## 6. Auth (layers)

### 6a. Elcano SSO → auto-admin  (copied from `home`)
Copy `home/server.js` auth into `lib/auth.js`: `parseCookies`,
`verifyElcanoSession` (Ed25519 verify of `elcano_auth`), `currentSession`,
`loginRedirectURL`, `requireAuth`. Add `isElcanoAdmin` (email domain ==
`elcanotek.com`) and `requireAdmin`. Env identical to home (`AUTH_SIGNING_PUBKEY`,
`AUTH_COOKIE_NAME`, `AUTH_LOGIN_URL`).

> **Cookie hardening (review):** if/when the auth service can, migrate the SSO
> cookie to a **`__Host-` host-only cookie** (Secure, Path=/, **no Domain**) so a
> sibling subdomain can't shadow/fixate it. Coordinate with the auth-service owner;
> Pages itself is unblocked by the separate content origin (§7) regardless.

### 6b. Per-page client password
bcrypt `pages.password_hash`. On `/view/<slug>`: Elcano admin cookie → allow; else
valid signed `page_<slug>` cookie → allow; else password form → on success set a
signed, HttpOnly, dashboard-scoped cookie (~30d). `NULL` hash = Elcano-only.

### 6c. Agent API token
`Authorization: Bearer` checked against `api_tokens` (sha256-hashed); one token per
agent so chat/cutlass revoke independently. Minted via `pages token add <label>`.

### 6d. `/raw` access (content host) — signed short-TTL tokens
The content host has **no cookies**. After the dashboard host authorizes a viewer
(SSO / page password / preview link), it mints an HMAC token bound to
**`{page_id, version_id, purpose: view|edit, render_mode, exp, sid}`** and points the
sandboxed iframe at `https://<content-domain>/raw/<slug>?t=…`. The content host
verifies the token with a **constant-time compare** (and checks `disabled`/
revocation), then renders. Critically, a `view` token **cannot** be replayed as
`edit=1` or for a different version — `purpose` and `version_id` are signed in. Edit
and preview tokens get short TTLs (minutes). This is the same mechanism as signed
**preview links** (§10) — one code path. The linchpin of the isolation model, so:
constant-time, fully-bound, short-lived.

---

## 7. Security: serving untrusted agent HTML  ⚠️ (rewritten per review)

**The core decision, settled: untrusted content lives on a separate registrable
domain from day one.** A shared parent domain is *not* a security boundary, for two
reasons the first draft underweighted:

1. **Cookie-tossing.** Any document on any `*.elcanotek.com` host can write
   `document.cookie = "elcano_auth=…; Domain=.elcanotek.com"`. The `Cookie:` header
   the server receives carries no Domain/Path, so auth can't reliably tell the real
   cookie from a shadow → session fixation/poisoning. Ed25519 signing doesn't help
   (attacker replays their own validly-signed token). HttpOnly doesn't help (this is
   a *write*, not a read). **A subdomain like `usercontent.elcanotek.com` does NOT
   fix this — it's the same cookie jar.** A different eTLD+1 does.
2. **Direct `/raw` hits.** The `sandbox` *attribute* is on our `<iframe>` tag, not on
   the HTTP response. An attacker can navigate a victim straight to the `/raw` URL
   (or frame it themselves); without an isolating origin it would run as a
   first-party `dashboard.elcanotek.com` document with full cookie-write access.

**Resolution (keeps "one server"):** run the same Node process behind a second Caddy
vhost on a **separate registrable domain** (placeholder `elcano-pages.io` — Brad to
pick/buy, ~$10). The content host serves only `/raw` + `/assets`, never reads the
SSO cookie, and is in its own cookie jar — so cookie-tossing can't reach SSO and a
sandbox escape's blast radius is a worthless content-origin cookie. **Buy the domain
before Phase 1.**

**Defense in depth (still do all of these):**
- **Sandboxed iframe**, tightened: `sandbox="allow-scripts"` only — **drop
  `allow-popups`** (tabnabbing), never `allow-same-origin`/`allow-top-navigation`.
  `referrerpolicy="no-referrer"`.
- **CSP `sandbox` as a real response HEADER on `/raw`** (so even direct top-level
  navigation runs opaque), plus an explicit allowlist. `default-src 'none'` is the
  base, but charts need script/style — so we **deliberately** allow `'self'` +
  `'unsafe-inline'` for script/style. That's safe *here precisely because* the content
  is fully isolated on a null-origin, cookieless domain with `connect-src 'none'`
  (no exfiltration) — document this tradeoff. Full `/raw` response headers:
  ```
  Content-Security-Policy: sandbox allow-scripts; default-src 'none';
    script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline';
    img-src 'self' data:; font-src 'self'; connect-src 'none';
    form-action 'none'; base-uri 'none'; object-src 'none';
    frame-ancestors https://dashboard.elcanotek.com
  Referrer-Policy: no-referrer
  X-Content-Type-Options: nosniff
  X-Robots-Tag: noindex, noarchive
  Cache-Control: no-store
  ```
  (`data:` never in `script-src`. `frame-ancestors` pins exact scheme+host, no port.)
  Agents get charting libs from the content host (`/assets/echarts.<ver>.<hash>.js`,
  immutable cache) under `script-src 'self'` — they don't inline a megabyte of lib.
- **Protect the *shell*, not just content:** `X-Frame-Options: DENY` /
  `frame-ancestors 'none'` on `/admin` and `/view` (anti-clickjacking — the danger is
  *our* authed UI being framed).
- **Mutations:** require bearer token (agents) **or** CSRF token in a custom header
  (admin shell) — **never SSO-cookie-alone**; reject `Origin` missing/`null`/≠
  dashboard host. Agent (bearer) path is CSRF-immune but must never be reachable via
  ambient cookies.
- **htmx stays out of the untrusted zone** — it's a request-engine gadget. The
  content-host vendor set is rendering-only (Chart.js/ECharts + Flag tokens/fonts).
  With `connect-src 'none'` it couldn't fetch anyway, but we don't ship the gadget.
- `postMessage`: treat all inbound as hostile (origin is `"null"` for sandbox);
  match `event.source === iframe.contentWindow` + strict schema. v1 needs at most a
  clamped resize message; ideally none.
- **Takedown kill switch:** `pages.disabled` → content host refuses to render; admin
  one-click "disable page" + a global "stop serving raw" lever (agencies can publish
  phishing-looking content; agents can misfire).

Other: `helmet`, bcrypt, parameterized SQL, body-size caps (HTML ≤ ~1–2 MB/version,
asset ≤ ~10–25 MB), rate limits (§9).

---

## 8. Theming (Flag default + per-client) & library policy

**Default look = Flag.** We vendor a pinned copy of `/root/flag/design-system` into
`pages/vendor/flag/` (refreshed by `scripts/sync-flag.sh`) and serve it from the
**content host** at `/assets/flag/`: `tokens/design-tokens.css`,
`fonts/dubai-fonts.css` + Dubai woff2, `icons/core-icons.svg`, `theme/theme-controller.js`.
Flag's contract is honored as-is: `html[data-theme]` light/dark, storage key
`flag-theme-preference`, system-default.

**Two render modes per version** (`page_versions.render_mode`):

- **`themed` (default).** The agent writes HTML using Flag **semantic tokens and
  component classes** (`var(--color-primary)`, etc.) and does **not** inline its own
  token definitions. At render time, `lib/render.js` injects into `<head>`, in order:
  1. `<link>` Dubai fonts → `<link>` `design-tokens.css` (Flag base),
  2. the page's client theme **override** (a small `<style>` redefining brand tokens),
  3. `theme-controller.js` (+ a default `data-theme` per the theme's `default_mode`).
  Result: with no client theme, the page is pure Flag; with one, the same markup is
  rebranded by overriding a handful of tokens. Injection is safe because content is
  sandboxed on the isolated origin.
- **`raw`.** Agent supplies a fully self-contained document; served **verbatim**, no
  injection. This is the "push some really random stuff, that's OK" path. Still
  sandboxed + CSP'd.

**Per-client theme = a token override.** A `themes` row whose `override_css`
redefines just the brand-relevant tokens, e.g.:
```css
:root[data-theme="dark"], :root[data-theme="light"] {
  --color-primary: #c8102e;          /* client brand */
  --color-accent:  #ffb81c;
  --font-heading:  "Dubai", system-ui;
}
```
plus an optional `logo_sha256`. Set via API/MCP/admin (`set_theme`). `theme_id NULL`
⇒ Flag default. (Flag's own dark default is `#1a0b1e` bg / `#7272ab` primary.)

**Library policy (documented in `docs/API.md`, not server-enforced):**
- Prefer Flag tokens/classes for `themed` pages; prefer `/assets/vendor/*` (Chart.js,
  ECharts) for charts — all served from the content host, CSP-clean.
- **No external CDNs** (jsDelivr/unpkg/Google Fonts) — `default-src 'none'` blocks
  them; the page silently loses styling. Dubai fonts and all libs are self-hosted.
- Images: upload via the assets API → reference `/assets/<sha>`; or small `data:` URIs.

**Shells use Flag too.** `/view` and `/admin` are built with Flag tokens/components
so the whole surface is on-brand.

**Editing — fix small things without regenerating the page.** Governed by the
invariant: *edit stored source, never rendered DOM.* Two approaches, neither using
`contenteditable`/`outerHTML` (which serialize the live post-script DOM and corrupt
charts — rejected in review):

1. **Raw-source textarea + live preview (v1 primary).** Admin opens the version's
   stored HTML in a textarea; a live preview renders it through the **real `/raw`
   pipeline** (preview == production); Save writes the edited source as a new draft.
   Lossless, trivial, handles both "wrong number" and bigger edits. Ships first.
2. **Text-node patch (next, nicer UX).** Server-side, parse the version's *stored
   source* (`linkedom`/`node-html-parser`) and assign stable text-node paths,
   skipping text inside `<script>/<style>/<canvas>/<svg>/<pre>/<template>`. Render
   read-only in the iframe; a click maps the element to its node path (via
   `postMessage`, validated by `event.source === iframe.contentWindow` + an
   unguessable `editSessionId`; origin is `"null"`, never trusted). The human edits
   one string → `{path, oldText, newText}`; the server verifies `oldText` still
   matches the current source node (stale ⇒ 409), patches **only that text node** in
   the parsed source, re-serializes, saves a draft. "Click the number, type the right
   one" with zero risk to scripts/charts/layout. If we ever make a span editable in
   place, use `contenteditable="plaintext-only"` + a paste handler forcing
   `insertText` — but the textarea ships first and is enough.

Both produce a normal `draft` (`source=admin`); publish/approval/rollback unchanged.
GrapesJS and DOM-serializing editors stay out.

---

## 9. Deployment (follows home/chat/moc conventions)

Single box, Fedora/RHEL 9+, `dnf`. Same shape as `home` (Node) + Postgres step from
`chat`/`moc`.

**`bootstrap.sh`** (interactive, idempotent): require root; `dnf install nodejs npm
postgresql postgresql-server caddy openssl`; init PG + `pg_hba` loopback →
`scram-sha-256` + create role/db `pages`; `useradd --system --home-dir /opt/pages`;
prompt for **both hostnames** (`dashboard.elcanotek.com` + content domain),
`AUTH_SIGNING_PUBKEY` (32-byte b64 Ed25519, validated like home), TLS plan; generate
`PAGE_COOKIE_SECRET`, `RAW_TOKEN_SECRET`, initial API token (`openssl rand`);
`sync-flag.sh` to vendor Flag; `rsync` → `/opt/pages` (exclude `.git`,
`node_modules`, `assets`, `.env`); `npm ci --omit=dev`; `node lib/migrate.js` (seeds
the `flag` theme); write `/opt/pages/.env` (0640); install systemd unit + `pages`
CLI; install **two-vhost Caddyfile** (rewrite both hostnames; `tls internal` if not
LE); open firewall; `systemctl enable --now postgresql caddy pages`.

**`update.sh`** (staging build + atomic swap, from chat/moc): `git fetch` +
`--ff-only`, re-exec self if changed; build in `mktemp -d` (`rsync`, `npm ci`, run
pending migrations, `sync-flag.sh`); `systemctl stop pages`; `rsync` staging → live
(preserve `.env`, `assets/`); reinstall unit + CLI; start; **poll
`127.0.0.1:3002/healthz`**. **Deploy-level rollback** = `git checkout <SHA> && pages
update` (distinct from *page* rollback in §5).

**`pages.service`** — `Type=simple`, `User=pages`, `EnvironmentFile=/opt/pages/.env`,
`ExecStart=/usr/bin/node /opt/pages/server.js`, `Restart=on-failure`,
`After=/Requires=postgresql.service`, `ProtectSystem=strict`,
`ReadWritePaths=/opt/pages/assets /tmp`, `NoNewPrivileges=true`.

**Caddyfile** — two site blocks (`dashboard.*` and the content domain) both
`reverse_proxy 127.0.0.1:3002`, security headers; bootstrap rewrites both names.

**Backup/restore correctness (review — highest-value reliability item):**
- **Write order: file-before-row.** Write asset to tmp → fsync → rename to `<sha>` →
  fsync dir → *then* commit the DB row referencing it. So a reference always has its
  file.
- **Backup order: `pg_dump` first, then `rsync`/tar `assets/`.** A new asset mid-backup
  yields at worst a harmless orphan file, never a missing one. Never GC during backup.
- **`pages check-integrity`:** every referenced `content_sha256`/asset exists on disk
  with matching hash; `published_version_id` points at a same-page version. Run
  restore-into-throwaway + this check on a cron. (A backup you haven't restored is a
  hope.)

**Rate limits / caps:** `express-rate-limit` on `/api/*`, `/mcp`, the password form,
and `/raw` token minting; Caddy edge limits; body-size caps as in §7. One small box
serving untrusted content is trivially DoS'd, and a stuck agent will hammer deploy.

**`/usr/local/bin/pages` CLI:**
```
pages start|stop|restart|status|logs
pages update                      # git pull + rebuild + restart
pages token add <label> | list | revoke <id>
pages page list | passwd <slug> | disable <slug> | enable <slug>
pages theme set <slug> <theme> | theme list
pages backup [dest]               # pg_dump THEN tar assets/
pages check-integrity             # verify refs ↔ files, pointer sanity
pages env show|edit
```

**`.env`:** `PORT`, `DASHBOARD_HOST`, `CONTENT_HOST`, `DATABASE_URL`,
`AUTH_SIGNING_PUBKEY`, `AUTH_COOKIE_NAME`, `AUTH_LOGIN_URL`, `PAGE_COOKIE_SECRET`,
`RAW_TOKEN_SECRET`.

---

## 10. REST API (`/api/v1`)

| Method & path | Auth | Purpose |
|---------------|------|---------|
| `GET    /pages` | bearer | list pages |
| `POST   /pages` | bearer | create `{slug, title?, password?, theme?, client_id?}` |
| `GET    /pages/:slug` | bearer | metadata + current published version |
| `DELETE /pages/:slug` | bearer | soft-delete |
| `POST   /pages/:slug/versions` | bearer | **deploy/update** `{html, render_mode?, note?, author?, publish?, expected_version?}` → draft/pending (*this is `/update`*) |
| `GET    /pages/:slug/versions` | bearer | history (newest first) |
| `GET    /pages/:slug/versions/:id` | bearer | one version's html + metadata |
| `POST   /pages/:slug/publish` | bearer | publish `{version_id, expected_version?}` |
| `POST   /pages/:slug/rollback` | bearer | rollback `{version_id?, expected_version?}` |
| `GET    /pages/:slug/pending` | bearer | review queue |
| `POST   /pages/:slug/versions/:id/approve` | **admin cookie+CSRF** | approve → publish |
| `POST   /pages/:slug/versions/:id/reject` | **admin cookie+CSRF** | reject `{note?}` |
| `POST   /pages/:slug/approval` | **admin** | toggle `require_approval` |
| `POST   /pages/:slug/disable` / `/enable` | **admin** | takedown kill switch |
| `POST   /pages/:slug/theme` | bearer | set `{theme}` (NULL ⇒ Flag) |
| `POST   /pages/:slug/password` | bearer | set/clear `{password\|null}` |
| `POST   /pages/:slug/preview-links` | bearer/admin | mint a signed expiring client preview link for a version |
| `GET/POST /themes` | bearer | list / create-update theme `{name, override_css, default_mode, logo?}` |
| `POST   /assets` | bearer | upload (multipart) → `{sha256, url}` |

JSON; idempotent where sensible (same-sha deploy returns the existing version).
`expected_version` enables 409 optimistic-concurrency. Every state change writes an
`audit_log` row in the same txn. `approve/reject/approval/disable/enable` are
**admin-cookie+CSRF only** — an agent submits; a human approves.

---

## 11. MCP for chat & cutlass

**MCP-over-HTTP from the same app** at `https://dashboard.elcanotek.com/mcp`
(`@modelcontextprotocol/sdk` Streamable HTTP), same bearer token, **same gated
mutation path as the REST API** (no privileged backdoor that skips the state
machine), rate-limited, audit-logged. chat/cutlass already support HTTP MCP servers
with headers.

**Tools:** `list_pages`, `get_page`, `deploy_page` (`{slug, html, render_mode?,
title?, theme?, note?, publish?}`), `update_page`, `publish_page`, `rollback_page`,
`list_versions`, `list_pending`, `set_theme`, `set_page_password`,
`create_preview_link`, `page_urls`.

> Agents can deploy/publish and set themes, but **cannot approve** (human-only). On
> an approval-gated page, deploy/update returns a `pending` version and the tool
> response hands the agent the `/admin` URL to route to a human. The guide tells
> agents: default to `render_mode=themed` with Flag tokens; use `raw` only for
> deliberately bespoke pages.

**Registration:** chat → `buildMCPSpecs()` (HTTP URL + `Authorization` header, gated
on `cfg.PagesAPIToken`, tool allowlist excludes approve/disable); cutlass →
`getMCPServerDefinitions()` (`serverType: http`). Add `PAGES_API_TOKEN` to both
repos' config/env + `.env.example`.

**`docs/API.md`** = the agent guide: how to write a Flag-themed page (tokens/classes,
the Flag `AGENT_GUIDE.md` contract), `themed` vs `raw`, no external CDNs, assets API,
draft→preview→approve→publish flow.

---

## 12. Phasing

- **Phase 0 — Scaffold + the origin boundary.** Repo skeleton, `package.json`,
  `.env.example`, `/healthz`, two-vhost Caddyfile, bootstrap/update/systemd skeletons
  standing up an empty authed dashboard host + a cookieless content host. **Buy the
  content domain.**
- **Phase 1 — Core serving + auth + theming.** Postgres schema/migrations (pointer-is-
  truth + seed `flag` theme); copy home auth; vendor Flag (`sync-flag.sh`) + render-
  time injection; `/view` (password + SSO) → signed `/raw` render on content host;
  `/admin` shell with version list + preview + publish + **rollback** + **approve/
  reject queue** + `require_approval`/`disable` toggles + theme picker. **audit log**
  from the first mutation.
- **Phase 2 — REST API + safety rails.** `/api/v1/*` + token auth + CSRF + Origin
  checks; rate limits + body caps; signed **preview links**; backup +
  **check-integrity**.
- **Phase 3 — MCP + agent integration.** `/mcp`; register in chat & cutlass; write
  `docs/API.md`. End-to-end: *"make me a dashboard"* → Flag-themed live URL.
- **Phase 4 — Source editing.** Raw-source textarea + live preview (edits stored
  source → new draft). Then the **text-node patch** ("click the number") on top of
  the same `/raw` render path. Covers the "fix the wrong number" case, losslessly.
- **Phase 5 — Polish / optional.** Asset GC (only if disk pressure; reference-count
  across *all* statuses, grace-period quarantine), `client_id` multi-page UI, `raw`
  render mode if a client needs verbatim non-Flag output, and — only if clients
  demand it — a constrained block builder (never GrapesJS-on-arbitrary-HTML).

Phases 0–3 deliver the core value; Phase 4 covers your "fix the wrong number" case.
Phase 5 is genuinely YAGNI until signaled.

---

## 13. Decisions made (so we can build) + the two real asks

**Decided (my call, per "make up your mind"):**
- **Slug namespacing:** `slug TEXT` allows both flat (`omnicom`) and nested
  (`omnicom/q2-report`); `client_id` groups them. No separate decision needed — both
  work day one.
- **Theme authoring:** *we* author `themes` (curated `override_css` + logo); agents
  and clients pick by **name** only. Keeps brand quality controlled and avoids agents
  injecting arbitrary CSS into the themed framing.
- **Retention:** keep all versions forever (dataset is tiny; rollback history is the
  product). Revisit only under real disk pressure (Phase 5 GC).
- **Render mode:** ship **`themed` only** in Phases 0–3; add `raw` (verbatim) in
  Phase 5 when a client actually needs non-Flag output. Fewer code paths to test now.

**Two real asks (one needs you, one needs the auth owner):**
1. **Content domain — pick & buy the separate registrable domain** for `/raw`
   (e.g. `elcano-pages.io`). External action only you can do. Not needed to scaffold
   (Phase 0 uses a configurable `CONTENT_HOST` + `tls internal` locally), but needed
   before a real Phase-1 deploy.
2. **`__Host-` SSO cookie** — worth asking the auth-service owner to migrate the
   `elcano_auth` cookie to host-only, to kill cookie-tossing across the whole
   `.elcanotek.com` estate? Pages is unblocked either way (separate origin), but it'd
   harden every subdomain.
