# Pages — Build Plan

> **What this document is.** The original design record for Pages, written
> before the code existed and kept because it is the "why" behind almost every
> decision in the codebase — source comments across `lib/`, `deploy/` and
> `migrations/` cite its sections by number (PLAN §5, §7, §9). It is a design
> rationale, **not** a status report: where it describes phases, sequencing or
> open questions, read it as a record of what was planned at the time, and read
> the code and [docs/](docs/) for what is actually true today. Start with the
> [README](README.md) if you want the current picture.
>
> **A note on names.** `chat` and `cutlass` are ElcanoTek's own internal agent
> tools — the original consumers of the API and MCP surfaces described here. They
> are not part of this repository and nothing in Pages depends on them; any MCP
> or bearer-token client works the same way. `flag` is ElcanoTek's design system,
> vendored under `public/assets/flag/`. `home` and `moc` are other internal
> ElcanoTek services whose conventions this design borrowed. Company names in the
> examples are invented.

> Quick client pages and dashboards. A small, single-server platform that lets
> agents and humans **deploy, version, preview, publish, and roll
> back** password-protected, **Flag-themed** HTML pages, with an admin
> review/approval/rollback workflow and an MCP/API surface for agents.

Inspired by [Shopify's "Quick"](https://shopify.engineering/quick): *"drop in a
folder of HTML and get back a secure URL only our people can see."* We borrow the
concept (instant deploys, auth at the edge, versioned content, one cheap VM) but
**not** the GCS/IAP plumbing — one Fedora/RHEL box, our own auth, Postgres-backed
version control, and the Flag design system as the default look.

> **Design decisions settled before build**, after two adversarial design
> reviews and the theming + editing requirements: untrusted content origin
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
later say *"update the northwind dashboard"* instead of regenerating it. That's the
cost saver.

The access paths (note the two domains — see §7):

| URL | Who | Auth |
|-----|-----|------|
| `<content-domain>/<slug>` | Client (e.g. Northwind) | Per-page password (the link an agent shares) |
| `dashboard.elcanotek.com/view/<slug>` | Elcano team | Elcano-**staff** SSO → broker that opens any page *without* its client password (§6b) |
| `dashboard.elcanotek.com/admin/<slug>` | Elcano team | Elcano SSO (`auth.elcanotek.com`) → auto-admin |
| `dashboard.elcanotek.com/api/v1/*` and `/mcp` | Agents (chat, cutlass) | API bearer token |
| `<content-domain>/raw/...` | (internal) rendering target | Signed short-TTL token (no cookies) |

> The first row is the **direct-serve amendment**: the client's page is served on
> the content host, not iframed from `/view` (a later amendment to this plan).
> `/view` ended up as the staff broker, so it is `requireAdmin` — the one path
> that opens a page without its client password.

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
  slug              TEXT UNIQUE NOT NULL,              -- 'northwind' or 'northwind/q2' (see §13)
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
  name         TEXT UNIQUE NOT NULL,                   -- 'flag' (seeded default), 'northwind', ...
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
bcrypt `pages.password_hash`. **As built (direct-serve amendment):
the client URL is `<content-domain>/<slug>`, not `/view`, and the content host
owns the password gate** so the form never sits in the SSO cookie jar. On
`<content-domain>/<slug>`: valid signed `pgs<id>` page-session cookie → render;
else password form → on success set a signed, HttpOnly, content-host-scoped
cookie. `NULL` hash = Elcano-only, and an Elcano-only page shows a "staff-only"
notice instead of a form.

`dashboard/view/<slug>` is the **staff broker** for that case, not a client
entrance: it is `requireAdmin`, and it mints a short `session` token the content
host exchanges for the page-session cookie (§6d). It hands out page access
*without* the client password, which is why it is admin-gated rather than merely
authenticated — `elcano_auth` is issued by a shared SSO whose audience is wider
than Elcano staff.

### 6b-ii. Partner portals — one credential over a SET of pages

*Added after the original plan; the model above is per-page and cannot express
"one link for a partner who owns six dashboards".*

A **portal** (`page_portals`, migrations/017) is a named set of pages behind one
shared password, with membership in a join table (`page_portal_members`) because a
page may belong to several portals — co-branded work is visible to both audiences.
`<content-domain>/portal/<slug>` asks for the password once and then lists the
member dashboards; a `pgp<portal_id>` cookie then opens each of them directly.

What makes this safe rather than merely convenient:

- **Membership is human-admin-only, everywhere.** `lib/portals.js` asserts
  `actorType === "user"` before anything else, and there is no MCP tool and no
  bearer route. Who may see which client's numbers is not a decision an agent can
  reach. (Contrast §workspaces, whose `assertOrganizer` deliberately *admits*
  agents — the exact inverse, hence a distinct `portal_admin_only` error code.)
- **A portal session is a different credential.** Domain-separated from the page
  token in the MAC input (`portal.` ‖ body) and in the credential digest
  (`cred:portal:`), so neither can be replayed as the other; the page token's shape
  is untouched, because `verifySession` fails closed and changing it would log out
  every live client.
- **A portal-authorised render sets no cookie**, and membership is re-read per
  request. That is the whole revocation mechanism: removing a page, rotating the
  password, or retiring the portal is effective on the viewer's next request rather
  than in thirty days.
- **The 404 rules still run first**, so `disabled`, unpublished and unknown are
  unchanged, and membership binds to the page **id** — a page deleted and recreated
  at the same slug inherits no access.
- **`portal` is a reserved slug segment** (and migrations/018 refuses to ship the
  route while a live page holds one), because the hazard runs toward the portal: a
  page created at `portal/<slug>` would otherwise seize a partner's bookmarked URL.

The accepted trade, worth restating: **one shared password per portal.** A
forwarded link and password give the recipient the whole set, and the audit log
records the portal, not the person. Per-person scoping is a second portal holding a
subset ("Fabrikam — West" beside "Fabrikam — All"), not a login.

### 6c. Agent API token
`Authorization: Bearer` checked against `api_tokens` (sha256-hashed); one token per
agent so chat/cutlass revoke independently. Minted via `pages token add <label>`.

### 6d. Content-host access — signed short-TTL tokens
The content host has **no cookies** to authorize with. The dashboard host mints an
HMAC token bound to **`{pid, vid, purpose, mode, exp, sid}`**, verified with a
**constant-time compare**. `purpose` is the token's **audience**, and each
consuming route allow-lists exactly one:

| purpose | minted by | consumed by | buys |
|---------|-----------|-------------|------|
| `view` | `/admin` preview-token, for **any** version | `/raw/<slug>` | render that one version |
| `template` | template preview-token / `template_urls` | `/raw-template/<id>` | render that one template revision |
| `session` | the staff-gated `/view` broker only | `<content-domain>/<slug>?t=` | a page-session cookie |

Signing a claim is not checking it, so each route checks: `/raw` requires
`purpose === "view"` and that the version belongs to the requested slug and its
page is not `disabled`; the exchange requires `purpose === "session"`, the page
id, and that the version is the currently published one.

**Why the audiences are separate.** `session` is the only purpose that is a *page*
credential rather than a *render* credential. A `view` token is short, read-only
and names one version — but with nothing pending the admin shell previews the
**published** version, so opening a page's admin screen mints a `view` token for
the live version, and preview URLs get pasted into chats. If the exchange accepted
`view`, that read-only URL would become an hour-long session on the live page with
the client password skipped. A version check alone would not have separated them.

A purpose named `edit` was reserved here for the Phase 4 editor and never built,
minted, or checked; it has been removed rather than left as a door with no room
behind it. Tokens are short-lived (minutes). The linchpin of the isolation model,
so: constant-time, fully-bound, short-lived, single-audience.

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
vhost on a **separate registrable domain** (placeholder `elcano-pages.io` — the product owner to
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
  This plan assumed agents would get charting libs from the content host
  (`/assets/echarts.<ver>.<hash>.js`, immutable cache) under `script-src 'self'`.
  **Neither half shipped.** `public/assets/` contains only the vendored Flag
  tokens, fonts, icons and theme controller — no chart library — and the paths
  that do exist are stable (`/assets/flag/tokens/design-tokens.css`), overwritten
  in place by `scripts/sync-flag.sh`, so they are served *revalidating* on
  purpose: `immutable` would pin every client to the tokens it first loaded and a
  Flag sync would never reach them. (`docs/API.md`'s authoring guidance still
  tells agents to load Chart.js/ECharts from `/assets/` and is wrong; correcting
  what agents are told to do is a separate change from recording what is true.)
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
`fonts/fonts.css` + IBM Plex Sans woff2, `icons/core-icons.svg`, `theme/theme-controller.js`.
Flag's contract is honored as-is: `html[data-theme]` light/dark, storage key
`flag-theme-preference`, system-default.

**Two render modes per version** (`page_versions.render_mode`):

- **`themed` (default).** The agent writes HTML using Flag **semantic tokens and
  component classes** (`var(--color-primary)`, etc.) and does **not** inline its own
  token definitions. At render time, `lib/render.js` injects into `<head>`, in order:
  1. `<link>` Flag fonts → `<link>` `design-tokens.css` (Flag base),
  2. the page's client theme **override** (a small `<style>` redefining brand tokens),
  3. `theme-controller.js` (+ a default `data-theme` per the theme's `default_mode`).
  Result: with no client theme, the page is pure Flag; with one, the same markup is
  rebranded by overriding a handful of tokens. Injection is safe because content is
  sandboxed on the isolated origin.
- **`raw`.** Agent supplies a fully self-contained document; served **verbatim**, no
  injection. This is the "push some really random stuff, that's OK" path. Still
  sandboxed + CSP'd.

> **Amendment (partner portals).** A `themed` render that a partner's **portal**
> authorised carries one further injected head tag: `#pages-nav`, a JSON list of
> the dashboards that viewer's credential opens (see migrations/017 and
> docs/API.md). Three consequences worth stating where the render model is
> defined:
>
> - **`raw` is verbatim until a portal authorises the view.** Then it gains the
>   switcher — the payload plus, unless the design reads the block itself, a
>   built-in control — and nothing else: no tokens, no fonts, no theme controller.
>   The sentence above is amended in exactly one direction. What `raw` promises is
>   that Pages will not RESTYLE the design; navigation is not styling, and the two
>   were only ever bundled because injection happened to be the same code path.
>   18 of 31 live dashboards are raw, so "raw gets no menu" meant most of a
>   partner's set was a dead end, and "redeploy it themed" would have done the one
>   thing raw exists to prevent.
> - **Served bytes are no longer a pure function of the published version.** A
>   member page's bytes depend on which portal authorised the request, so "what
>   exactly did the client see" is no longer answerable from the version row
>   alone, and the content host cannot be edge-cached. The diff is confined to one
>   head tag derived entirely from admin-curated membership.
> - **The id belongs to Pages.** A deployed document carrying its own `#pages-nav`
>   has it stripped in `prepareDeploy`, before the content hash, so the stored
>   bytes can only ever hold the injected one.

**Per-client theme = a token override.** A `themes` row whose `override_css`
redefines just the brand-relevant tokens, e.g.:
```css
:root[data-theme="dark"], :root[data-theme="light"] {
  --color-primary: #c8102e;          /* client brand */
  --color-accent:  #ffb81c;
  --font-heading:  "IBM Plex Sans", system-ui;
}
```
plus an optional `logo_sha256`. Set via API/MCP/admin (`set_theme`). `theme_id NULL`
⇒ Flag default. (Flag's own dark default is `#1a0b1e` bg / `#7272ab` primary.)

**Library policy (documented in `docs/API.md`, not server-enforced):**
- Prefer Flag tokens/classes for `themed` pages; prefer `/assets/vendor/*` (Chart.js,
  ECharts) for charts — all served from the content host, CSP-clean.
- **No external CDNs** (jsDelivr/unpkg/Google Fonts) — `default-src 'none'` blocks
  them; the page silently loses styling. Flag fonts and all libs are self-hosted.
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
`RAW_TOKEN_SECRET`; optional MCP boundary extensions are `MCP_ALLOWED_HOSTS` and
`MCP_ALLOWED_ORIGINS` (comma-separated hostnames and exact origins).

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

**MCP-over-HTTP from the same app** at `https://pages.elcanotek.com/mcp`, using
the official `@modelcontextprotocol/sdk` v1.29.0. The production contract targets
stable MCP `2025-11-25`: stateless Streamable HTTP, one JSON-RPC message per POST,
JSON responses, no server session or batch extension. Clients send JSON and an
`Accept` value containing both JSON and SSE, then send the negotiated
`MCP-Protocol-Version` after initialization. The same bearer token and **same
gated mutation path as the REST API** apply (no privileged backdoor), with rate
limits and audit logging. Invalid credentials produce a Bearer challenge; Host
and any supplied Origin are allowlisted before body parsing. A temporary
compatibility path accepts the existing chat/cutlass `2024-11-05` behavior.

Every tool advertises a title, description, strict input and output schemas, and
standard behavioral annotations. The catalog covers page authoring and
versioning, targeted server-side search/patch, static preflight, managed
config/data, templates, staged uploads and upload tickets, workspaces, discovery,
and two read-only compatibility names.

**The catalog is not enumerated here, deliberately.** `lib/mcp-tools.js` is the
registry; the pasteable copy is the marked `pages:allowlist` block in
`docs/INTEGRATION.md`, mirrored in `docs/API.md`. Unit tests pin both blocks and
the `docs/API.md` tool table to that registry and screen the agent docs for an
unmarked fenced copy; the integration suite additionally pins
`EXPECTED_TOOLS` in `test/mcp.integration.js` against the live `tools/list`.
Both marked blocks have gone stale before — the `docs/INTEGRATION.md` one by 13
tools, the enumeration that used to live *here* by 17 — and a consumer that
pastes a stale list silently loses those tools with nothing in any log to explain
it. So a hand-maintained copy is either checked or it is deleted, and this one
was deleted.

List tools use bounded, filter-bound keyset cursors. `list_pages` filters by text,
workspace (including `null` for Ungrouped), client, serving state, approval gate,
and disabled state; `list_versions` filters by status. Deploy and update accept
`expected_version`, as do publish and rollback, so an agent can protect every
live-pointer mutation from stale writes.

Dashboard scheduling is caller-owned. `prepare_dashboard_update` generates a
pinned exact-slug one-time or reusable recurring prompt without mutating Pages.
`get_page_refresh` and `configure_page_refresh` are read-only compatibility names
for deployed static client allowlists; they do not persist or dispatch schedules.

> Agents can organize pages with workspaces and discover curated themes, but
> workspace deletion, theme mutation, approve/reject, takedown controls, password
> clearing, and restoration remain human-only. On an approval-gated page,
> deploy/update returns a pending version while an older version may still serve.
> Tool results therefore distinguish `version_is_live` from `page_is_live` and
> include `live_version_id` + `next_step`. Default to `render_mode=themed`; use
> `raw` only for deliberately bespoke pages.

**Registration:** chat → `buildMCPSpecs()` (HTTP URL + `Authorization` header,
gated on `cfg.PagesAPIToken`); cutlass → `getMCPServerDefinitions()`
(`serverType: http`). Both are deployed. Their existing static allowlists remain
compatible through the two read-only legacy names, so the prompt transition
itself needed no consumer-repository change. That is the *only* compatibility
Pages can claim: a name a consumer does not list is never registered with its
model, and Pages cannot read either manifest to check. Any list predating
templates, `find_in_version`, `patch_page`, `preflight_page`, or
`create_upload_ticket` leaves those tools unreachable in that client until it is
diffed against the pinned block. New clients should take the whole block.

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
- **Phase 3 — MCP + agent integration.** `/mcp` and `docs/API.md` are complete;
  register the full tool surface in chat & cutlass. End-to-end: *"make me a
  dashboard"* → Flag-themed live URL.
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

## 13. Decisions made before build, and the two external dependencies

**Decided:**
- **Slug namespacing:** `slug TEXT` allows both flat (`northwind`) and nested
  (`northwind/q2-report`); `client_id` groups them. No separate decision needed — both
  work day one.
- **Theme authoring:** *we* author `themes` (curated `override_css` + logo); agents
  and clients pick by **name** only. Keeps brand quality controlled and avoids agents
  injecting arbitrary CSS into the themed framing.
- **Retention:** keep all versions forever (dataset is tiny; rollback history is the
  product). Revisit only under real disk pressure (Phase 5 GC).
- **Render mode:** ship **`themed` only** in Phases 0–3; add `raw` (verbatim) in
  Phase 5 when a client actually needs non-Flag output. Fewer code paths to test now.

**Two things the design could not settle for itself:**
1. **Content domain — pick & buy the separate registrable domain** for `/raw`
   (e.g. `example-pages.com`). A procurement action, not a code change. Not needed to scaffold
   (Phase 0 uses a configurable `CONTENT_HOST` + `tls internal` locally), but needed
   before a real Phase-1 deploy.
2. **`__Host-` SSO cookie** — a request to the auth-service owner to migrate the
   `elcano_auth` cookie to host-only, to kill cookie-tossing across the whole
   `.elcanotek.com` estate? Pages is unblocked either way (separate origin), but it'd
   harden every subdomain.
