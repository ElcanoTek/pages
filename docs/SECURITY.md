# Pages — security model & known follow-ups

## Posture (what's enforced)

- **Two-host origin split.** The trusted dashboard host (`pages.elcanotek.com`)
  reads the Elcano SSO cookie; the content host (`elcano-pages.com`) is cookieless
  and renders untrusted agent HTML under a strict `sandbox` CSP. Confidential
  ("Elcano-only") pages are unreachable on the content host without either a
  per-page password or a short broker token minted after **Elcano-staff** SSO.
- **The `/view` broker is staff-only, and a render token is not a page
  credential.** These are two of the three ways to reach a live page without its
  own client password — a partner portal session is the third, below — so both
  are bound tightly:
  - `/view/<slug>` requires `requireAdmin`, not merely a valid session. The
    `elcano_auth` cookie comes from a shared SSO whose audience is wider than
    Elcano staff (`isElcanoAdmin` exists precisely because of that), and this
    route mints a page-session cookie without asking for the client password —
    so any signed-in outsider would otherwise have read every hosted dashboard.
  - **`purpose` is the token's audience, and the audiences are disjoint.**
    `session` is exchanged for a page-session cookie and is minted *only* by
    that staff-gated broker. `view` renders one page version at `/raw` and
    nothing else; `template` renders one template revision at `/raw-template`
    and nothing else. Each consuming route allow-lists the single purpose it
    serves, so a purpose added later inherits no access.
    This split is load-bearing rather than tidy: `/admin` mints a `view` token
    for whichever version the shell is previewing — which, with nothing pending,
    is the **published** one — so simply opening a page's admin screen produces
    a token naming the live version. A preview URL is exactly the kind of link
    that gets pasted into a chat, and if the content host exchanged it, that
    300-second read-only render URL would become a one-hour session on the whole
    live page with the client password skipped. Binding on the version alone
    would not have separated those two cases at all. The exchange still requires
    the version to be the published one, as defense in depth.
- **A partner portal session opens a page, and live membership is what bounds
  it.** A `pgp<portal_id>` cookie proves knowledge of one portal's shared password
  and authorises whatever that portal contains *at the moment of each request*.
  Five properties make that safe rather than merely convenient:
  - **A portal-authorised render sets no cookie, ever.** Minting a `pgs<page_id>`
    session there is the obvious optimisation and it is a hole: that cookie binds
    to the *page's* own password, so removing the page from the portal — or
    rotating the portal password — would revoke nothing for thirty days.
    Membership is re-read per request instead, and that is the entire revocation
    mechanism. An integration test asserts zero `Set-Cookie` on such a render.
  - **The 404 rules run first, unchanged.** Disabled, unpublished and unknown
    pages 404 before any portal is consulted, so the takedown switch and the
    delete→recreate slug-reuse guard keep working exactly as they did and no
    portal credential can reach a page those rules already closed.
  - **Membership binds to the page id, not the slug.** A page deleted and
    recreated at the same slug is a stranger to the portal.
  - **A page's password form also tries the portals containing it**, so a partner
    who bookmarked one dashboard is not stranded — and a failed attempt charges
    the page's counter *and* every portal counter it tested, with the largest
    delay applied. Without that cross-charge an attacker would get one budget per
    door against a secret worth the same at all of them. It is why
    portals-per-page is capped at four: each candidate is a 30–80 ms scrypt call
    and libuv's threadpool is four threads.
  - **The membership lookup fails safe by construction.** It can only ever grant
    access, so a failure degrades to "no portals" — the page falls back to the
    exact gate it had before portals existed — rather than 500ing the gate for
    every page on the host.
  A page with no password of its own that a human has added to a portal prompts
  for the portal credential instead of showing the staff-only refusal, which would
  tell a partner they are not entitled to a page they are. No gate ever names a
  portal: that a page belongs to one is implied by the prompt; which one is not.
- **The page switcher is injected, and scoped to the portal that authorised the
  request.** A themed portal-authorised render carries one extra `<head>` tag: a
  JSON block (`#pages-nav`) listing that portal's current membership. Four
  properties keep it honest:
  - **The list is a function of the authorising portal, never of "every portal
    containing this page".** A co-branded dashboard sits in two partners' portals,
    and the union would hand each of them the other's dashboard titles. Holding a
    valid `pgp<N>` is what proves entitlement to N's list; with two such cookies
    the lowest portal id wins, deterministically, so the answer cannot flip
    between loads.
  - **Injected, not substituted, and `#pages-nav` is Pages' id.** Nothing is
    located or parsed on the serve path (`parse5` with source locations on a 2 MiB
    document is ~369 ms of blocked event loop; the head splice is ~0.006 ms), and a
    document that ships its own copy of the block has it stripped at deploy time —
    before the content hash — so the served bytes can only ever carry ours. `<head>`
    precedes `<body>`, so `getElementById` returns ours regardless.
  - **It cannot break a render.** The membership query is caught and degrades to
    "no switcher": a lookup that times out must not turn a live client dashboard
    into a 500. Asserted by forcing it to throw.
  - **Bounded on every axis** — ≤50 entries with a `truncated` flag, ≤200 characters
    per field, ≤16 KiB serialised — because it ships on every render of every
    member page, and titles are agent-settable. Serialised with `escapedJson`, so a
    sibling title of `</script><img src=x onerror=…>` cannot end the block.
  `render_mode: "raw"` receives the switcher and no theming, so the guarantee it
  actually makes — Pages will not restyle this design — is intact while navigation
  works on the majority of the fleet that is raw. A raw page with no portal
  authorising the view is byte-for-byte as before.
- **Served bytes are no longer a pure function of the published version.** A member
  page's bytes now depend on which portal authorised the request. This is a real
  weakening of the mental model behind `content_sha256`: "what exactly did the
  client see" is no longer answerable from the version row alone, and it forecloses
  edge-caching the content host. The diff is confined to one injected `<head>` tag
  whose content is derived entirely from admin-curated membership, and `themed`
  renders were already not byte-identical to their stored source.
- **The content-host sandbox allow-list is a deliberate, minimal set.**
  `sandbox allow-scripts allow-downloads allow-modals` (`lib/csp.js`,
  `sandboxTokens()`). `allow-scripts` is what makes a dashboard a dashboard; the
  other two exist because without them an "Export CSV" click produced no file
  and a "Download PDF" button was answered with Chromium's
  `Ignored call to print()` — both failing silently, in production. Neither
  grants reach the document did not already have: a download saves a blob the
  page itself built (with `connect-src 'none'` there is nothing to fetch into
  one), and modals are an annoyance channel with no data access.
  **`allow-same-origin` must never be added.** Combined with `allow-scripts` it
  is a documented sandbox escape — Chromium warns about the pair by name — and
  it is the single token that would hand agent HTML a real origin, storage and
  cookies. `allow-popups`, `allow-forms` and `allow-top-navigation` are withheld
  as navigation/redirect surfaces; add one only against a concrete need.
  `lib/preflight.js` reads `sandboxTokens()` directly, so authoring guidance and
  the served header cannot drift apart.
- **One state machine, no backdoor.** REST, MCP, and the admin UI all route through
  `lib/versions.js`. Every mutation runs `SELECT … FOR UPDATE` first, honors
  optimistic concurrency (`expected_version`), and writes an `audit_log` row in the
  same transaction.
- **Authority split.**
  - *Agents (bearer):* create, deploy, publish/rollback **open** pages, **set**
    (not clear) a client password, rename, soft-delete **open** pages, and create/
    rename/assign one-level workspaces.
  - *Humans (admin cookie + CSRF):* approve/reject, toggle the approval gate,
    disable, set theme, **clear** a password, and delete/restore **approval-gated**
    pages. Workspace deletion is also human-only because it changes organization
    for every member at once.
  - *Humans only, with no agent equivalent at all:* **partner portals**
    (`lib/portals.js`). A portal is one shared client credential over a curated
    set of dashboards, so its membership decides which client's numbers a given
    password opens — the one question a mis-scoped or injected agent must never be
    able to answer. Every mutation asserts `actorType === "user"` first
    (`403 portal_admin_only`, deliberately not workspaces' `admin_only`, since
    `assertOrganizer` there *admits* agents), and no bearer or MCP route reaches
    the tables. Adding a page whose `password_hash IS NULL` reclassifies it from
    staff-only to client-readable, so that fact is returned to the caller and
    stamped into the audit row rather than left implicit. Portals-per-page is
    capped at four at add time, because each one is another scrypt verification
    against a single password submission.
  - The approval gate governs all destructive/live-affecting agent actions on a
    page: on a gated page, agents may only queue a `pending` version — publish,
    rollback, and delete require a human.
  - The `disabled` **takedown** likewise locks a page against agents: an agent
    cannot publish, roll back, or **delete** a disabled page (`disabled_takedown`).
    Blocking delete is what prevents a delete→recreate slug-reuse bypass of the
    takedown; re-enabling is admin-only.
- **Delete is soft + reversible.** `delete_page` sets `deleted_at` and NULLs the
  live pointer; the row, its versions, and its audit trail are kept, and an admin
  can `restore`. A hallucinated/injected delete is recoverable, not destructive.
- **Password mutation is asymmetric by risk.** Setting/changing a client password
  is agent-capable (the "make this dashboard client-accessible" step). **Clearing**
  a password is admin-only — on a live client page a clear silently flips it back
  to staff-only and 403s every real client, so a stray agent must not be able to.
- **Tokens.** `pgs_…` bearer tokens are stored only as `HMAC-SHA256(token, pepper)`
  with a short display prefix; independently revocable. Missing, invalid, or
  revoked credentials at `/mcp` return a standard
  `WWW-Authenticate: Bearer realm="pages"` challenge.
- **Fail-closed secrets.** In `NODE_ENV=production`, an empty `PAGE_COOKIE_SECRET`,
  `API_TOKEN_PEPPER`, or `RAW_TOKEN_SECRET` throws at startup (an empty signing key
  would let anyone forge page-session/`/raw` tokens or brute token hashes offline).
- **The content zone's header contract is a floor, not a per-route habit.**
  `contentBaseHeaders()` (`lib/csp.js`) is applied as the content host's first
  middleware, so every response Express emits on that host carries
  `Referrer-Policy: no-referrer`, `X-Content-Type-Options`, `X-Robots-Tag`, a
  CSP, and `Cache-Control: no-store`. Routes that render layer `rawHeaders()` or
  `gateHeaders()` on top; `/assets` widens the CSP and sets a revalidating
  `Cache-Control` explicitly — `express.static` only supplies its own when the
  header is absent, so relying on it would have left every asset `no-store` and
  made each dashboard load re-download the fonts. `sandbox` covers everything that renders
  untrusted HTML — the gate pages omit it so the password form can submit, and an
  asset omits it because `default-src 'none'` already neutralises a *directly
  navigated* asset document (an SVG can carry script), which is the only case
  where a CSP on a subresource response applies at all.
  **The floor exists because per-route headers kept missing shapes nobody had
  decided about:** the terminal 404 was bare `text/plain`; `/assets` carried only
  `nosniff`, and the 301 `express.static` emits for a directory never ran its
  `setHeaders` hook at all; the two `Set-Cookie` redirects that hand out a page
  session carried nothing; `/healthz` carried nothing; the read limiter answered a
  client's browser with a JSON error; and a thrown handler fell through to
  Express's default error document — which outside `NODE_ENV=production` includes
  the stack trace, on a public URL. Each was an accident of how a middleware was
  mounted, and an exception nobody chose is one nobody can find.
  **The honest limit:** a request Node rejects before Express sees it (a malformed
  request line, a truncated body) is answered by the HTTP server itself and
  carries none of this; Caddy's own header block covers that layer in production.
  An integration test asserts the contract per response shape, and pins the asset
  CSP and cache policy by *value* — a presence check would accept a wide-open CSP.
- **Rate limits + DoS guards.** Per-IP limits on `/api/v1`, `/mcp`, `/raw`, and a
  strict brute-force guard on the password form, backed by a **progressive
  per-page backoff** (one shared failure counter per page across all source
  IPs, delaying only the 401 — never a lockout attackers could turn on real
  viewers). The MCP boundary validates Host,
  validates any supplied Origin, rate-limits, and authenticates **before** parsing
  the HTML-sized JSON body. JSON-RPC batches are rejected, so one HTTP request
  cannot fan out into many database operations.
- **MCP transport boundary.** `/mcp` accepts POST only, requires JSON content and
  an `Accept` value containing JSON plus SSE per Streamable HTTP, and rejects an
  unsupported `MCP-Protocol-Version`. It is stateless and returns JSON responses;
  malformed JSON and body-limit errors stay in JSON-RPC form rather than falling
  through to an HTML error page. Browser Origins default to the dashboard origin;
  exceptional origins/hosts must be added explicitly with `MCP_ALLOWED_ORIGINS`
  and `MCP_ALLOWED_HOSTS`.
- **MCP typed tools.** The official SDK validates each call against a strict
  `inputSchema` (`additionalProperties:false`) before dispatch. Every advertised
  tool also has a title, `outputSchema`, and standard behavioral annotations;
  successful `structuredContent` is validated against that output contract and
  mirrored as JSON text for older consumers.
- **Read-only update preparation.** `prepare_dashboard_update` pins an existing
  slug, live version, and managed schema without mutating Pages. It rejects
  credential-shaped instruction values, forbids recurring layout rewrites, and
  tells callers to fail closed on incomplete sources. Pages holds no scheduler
  or MOC credentials. The legacy-named compatibility tools are also read-only.
- **CPU-bounded managed-data validation.** Embedded JSON Schemas compile once
  per process (bounded LRU keyed by schema hash) instead of per call, and
  `pattern` regexes with two catastrophic-backtracking shapes are rejected at
  compile time — a quantifier nested inside another (`(a+)+`, `(\d+)*`), and a
  repeated group with two identical alternatives (`(a|a)+`, 14.5 seconds of
  blocked event loop on a 29-character non-matching input). No database timeout
  would catch either. The screen errs conservative and keeps the delimited idiom
  `(-[a-z0-9]+)*`, where every pass must consume a literal the inner repetition
  cannot, so matches partition linearly.
- **Bounded staged page uploads.** Large MCP-authored HTML uses explicit upload
  handles stored in PostgreSQL, not transport sessions or server-local files.
  Handles are bearer-token-bound, expire after 24 hours of inactivity, and are
  capped at five active/2 MiB each. Every canonical-base64 chunk is limited to
  48 KiB (`PAGE_UPLOAD_MAX_CHUNK_BYTES`, hard-bounded at 256 KiB by both the
  application clamp and a `page_content_upload_chunks` CHECK) and ordered with
  idempotent sequence retries. Pages verifies the exact
  byte count, SHA-256, and UTF-8 before atomically committing the immutable
  version, pointer/audit writes, and saved retry result; committed chunks are
  deleted. An explicit token-bound cancellation frees abandoned active uploads
  without touching a page/version; when a caller is *at* the five-upload cap, a
  fresh `start_page_upload` first reaps only that caller's own uploads that have
  been idle for an hour, so an abandoned turn cannot hold the cap for a day
  while an in-flight upload is never destroyed underneath its owner. Broad
  inline MCP deploys also reject literal
  file placeholders such as `$(cat ...)` so an agent cannot accidentally
  publish them as a blank page.
- **Upload tickets are the weakest credential in the system, by construction.**
  `create_upload_ticket` lets an agent's sandbox `PUT` a page's bytes straight to
  `/upload/:id` so a document never has to be re-emitted as model output. The
  ticket necessarily passes through a model context, so it is scoped to make a
  leak uninteresting: **write-only** (it cannot deploy, publish, read, or list —
  committing still needs the agent token); **content-pinned** to the
  `total_bytes` + `content_sha256` the authenticated agent declared at mint time,
  so the only byte string it will ever accept is the one already committed to;
  bound to one `upload_id`; spent on first use, with a re-send of the identical
  document treated as an idempotent no-op; and expiring in
  `PAGE_UPLOAD_TICKET_TTL_MINUTES` (15). Stored as
  `HMAC-SHA256(ticket, API_TOKEN_PEPPER)` like an `api_tokens` row, looked up by
  hash, with one non-revealing 401 for a wrong, unknown, foreign, or expired
  ticket. The endpoint sits behind the same per-IP limiter as `/api/v1` and
  authenticates on the header **before** the body parser buffers anything.
- **Deploy-time preflight is advisory, never a gate.** `lib/preflight.js`
  statically checks each deployed document against the exact CSP/sandbox it will
  be served under and returns findings on the deploy result. It parses with
  `parse5` and syntax-checks inline scripts with `node:vm`'s **compile-only**
  path (no evaluation, bounded to 1 MiB per script). It cannot block a publish —
  humans own that, and a false positive must never be able to wedge an agent.

## Known follow-ups (ranked)

1. **Broad manual-token scoping remains a follow-up.** An external unattended
   scheduler can use a manually provisioned `data_update` token exposing only
   two MCP tools with exact slug grants. Existing `deploy` tokens retain
   broad Chat/manual authoring compatibility and can still write, re-password,
   rename, or delete every open page. A leaked broad token is therefore still a
   full-tenant risk. **Plan:** add an optional client/exact-slug policy for broad
   authoring tokens without weakening legacy behavior. Until then: never install
   a broad token on an unattended scheduler, rotate it regularly, and revoke
   promptly. Pages never provisions or broadens a scheduler identity itself.
2. **`set_password` confidential→public residual.** With password *setting*
   exposed to agents, a compromised token that also knows a confidential slug could
   set a password it controls and then read that page on the content host. The
   silent-lockout half (clearing) is already closed (admin-only); this half is
   fully mitigated only by token scoping (#1). Every password mutation is audited.
3. **The ReDoS screen covers accidents, not a hostile author, and misses the
   bounded-outer shape.** Two gaps, both measured:
   - It compares alternation branches as *source text*, so `(a|[a])+`,
     `((a)|(a))+` and `(a|(b|b))+` are accepted and each still blocks for 14–21
     seconds. Anyone who can write a schema can write one of those.
   - A bounded outer repetition around an unbounded inner one — `(.*a){20}`
     (12.9s), `(a{1,10})*` (10.9s), `(\d+){5}` (15.4s at n=150) — is not
     screened. The obvious fix (treat any `{n,m}` with max>1 as a repetition)
     was implemented, measured, and reverted: it also files a *bounded* inner
     quantifier as unbounded and so rejects the grouped-repetition idiom every
     real id format uses — IPv4, IPv6, MAC, UUID, semver, cron, time ranges.
     Nineteen false positives, none catastrophic; a client schema that stops
     compiling breaks their dashboard.
   **Plan:** separate "this group repeats" from "this atom is an unbounded inner
   quantifier", and extend the delimiter proof to trailing literals and
   complementary classes, so `(.*a){20}` can be told from `(\d{1,3}\.){3}`.
   Until then a `pattern` is authored by whoever writes the page's schema, which
   today is an Elcano agent, not the client.
4. **The `session` broker token is not single-use.** It is a 120-second bearer
   value carried in a query string, so for that window it sits in the content
   host's proxy access log and the browser's redirect chain, and it is worth a
   one-hour page session. The exposure is small (staff-minted, two minutes, one
   page) and it predates the audience split, which narrowed what such a token can
   do but not how long it can be replayed. **Plan:** spend it on first exchange —
   `sid` is already minted and unused, so it is the natural nonce. Until then,
   treat a `/view` redirect URL as a credential, not a link.
5. **Dead schema / plumbing** (`preview_links`, the unused `api_tokens.scope`
   `admin` tier, the `assets` table with no upload API): either wire up or drop, to
   remove the "mid-refactor" ambiguity. Low risk, cleanliness only.

## Audit vocabulary

Actions written to `audit_log`:

- **Pages and versions** — `create_page`, `deploy`, `data_update`, `config_update`,
  `publish`, `rollback`, `approve`, `reject`, `disable`/`enable`, `set_approval`,
  `set_theme`, `set_password`/`clear_password`, `set_title`, `delete_page`,
  `restore_page`, `record_refresh_check`.
- **Templates** — `create_template`, `register_template`, `delete_template`,
  `template_build`, `template_rerender`.
- **Workspaces** — `create_workspace`, `rename_workspace`, `delete_workspace`,
  `set_workspace`.
- **Partner portals** (human admins only) — `create_portal`, `rename_portal`,
  `set_portal_password`, `add_portal_page`, `update_portal_page`,
  `remove_portal_page`, `set_portal_home`, `delete_portal`.

Those bullets are the machine-checked part: a unit test compares them against the
`action:` literals in `lib/` in both directions, because the list had already
drifted by six actions (the whole template and config set) before the test
existed. Keep prose out of the bullets so the comparison stays exact. The three
actions movePointer receives as an argument (publish, rollback, approve) are named
in the test explicitly, since no grep of a call site can see them.

Portal-level rows carry the portal identity in metadata with a null page id; the
three membership actions carry the page id as well, so "who exposed this page, and
to whom" is answerable from the page's own trail. A portal password is never
written to the log, and an integration assertion greps the log for it.

Older installations may retain historical `configure_refresh`,
`run_refresh_now`, and `pause_refresh` rows from the retired dispatcher; prompt
preparation is read-only and creates no audit mutation.
