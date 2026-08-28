# Pages — API & MCP guide for agents

Two agent surfaces, both on the **dashboard host** (`pages.elcanotek.com`), both
authenticated with an agent **bearer token**, and both routing shared mutations
through the same version state machine (no privileged backdoor):

- **REST** — `POST/GET/DELETE https://pages.elcanotek.com/api/v1/*` (PLAN.md §10)
- **MCP-over-HTTP** — `POST https://pages.elcanotek.com/mcp` (PLAN.md §11)

The managed-data execution tools and the read-only dashboard-update prompt
preparer are MCP-only. Pages does not run a scheduler or dispatch agent tasks;
broad deploy/version operations remain available over both transports.

> Agents own **authoring**: create / deploy / update / publish / rollback (on open
> pages), organize pages into workspaces, set a client password, rename, and
> soft-delete open pages. Humans own
> **trust & safety** in the `/admin` shell: approve/reject a version, toggle the
> approval gate, disable a page, set the theme, clear a password, and
> delete/restore approval-gated pages or delete a workspace.

## Auth

`Authorization: Bearer <token>` on every request. Mint tokens with
`pages token add <label>` (prod) or `node scripts/token.js add <label>` (local).
The raw token is shown once. One token per agent so chat/cutlass revoke
independently.

Broad `deploy` tokens retain the authoring and prompt-preparation surface. An
external scheduler may optionally use an operator-created `data_update` token
with explicit slug grants; Pages never creates tokens or grants as a side effect.
See [DATA_UPDATES.md](DATA_UPDATES.md).

## MCP

Pages targets the stable [MCP `2025-11-25` specification](https://modelcontextprotocol.io/specification/2025-11-25)
using `@modelcontextprotocol/sdk` v1.29.0. It is a **stateless Streamable HTTP**
server at one `POST /mcp` endpoint: each request is independent, responses use
`application/json`, and there is no session ID or server-sent event stream to
retain. `GET` and `DELETE` return `405`.

Every POST must carry:

```http
Authorization: Bearer <token>
Content-Type: application/json
Accept: application/json, text/event-stream
```

Initialize with `protocolVersion: "2025-11-25"`, then send
`MCP-Protocol-Version: 2025-11-25` on the initialized notification and every
later request. A missing/invalid bearer token returns `401` with a
`WWW-Authenticate: Bearer realm="pages"` challenge. A browser request with an
`Origin` must match the dashboard origin or an explicit allowlist entry, and the
request hostname must be allowed. JSON-RPC batches are not supported; send one
request or notification per POST.

The server currently retains a compatibility path for the existing chat/cutlass
clients that initialize with `2024-11-05` and omit the follow-on protocol header.
That path is for deployed legacy clients only; new integrations must use the
stable contract above.

`tools/list` publishes a human title, description, strict `inputSchema`,
`outputSchema`, and standard read/destructive/idempotent/open-world annotations
for every tool. Successful calls return the same schema-checked object both as
`structuredContent` and serialized JSON in `content[0].text` for clients that do
not yet consume structured results.

### Tools

| Tool | Args | Does |
|------|------|------|
| `list_pages` | `query?, workspace_id?, client_id?, is_live?, require_approval?, disabled?, limit?, cursor?` | bounded active-page discovery with workspace/theme/access state, URLs, and each row's **`freshness`** so one call ranks the estate by staleness; `workspace_id:null` selects Ungrouped |
| `get_page` | `slug, include_html?` | metadata + routing URLs + page-level `is_live`; published HTML only when `include_html:true` |
| `get_page_data` | `slug` | published managed-data schema/envelope, deterministic data/schema/template hashes, **`data_profile`**, **`freshness`** (coverage/refresh/check stamps plus `days_since_*`), metadata, URLs, and truthful live state |
| `get_page_refresh` | `slug` | read-only compatibility guidance for older static Chat/Cutlass allowlists; returns `scheduling:user_owned` and never reads or creates a Pages schedule |
| `record_refresh_check` | `slug, outcome, detail?, source_as_of_seen?` | record that a refresh looked at this page and what it concluded, **without creating a version**. Moves `freshness.checked_at` only — the published pointer, the data, and every hash are untouched. Call it when a run ends without publishing (`source_not_updated`, `source_unreachable`, `blocked`, `failed`); that outcome otherwise writes nothing anywhere, so a page whose upstream has frozen reads exactly like one nobody runs any more. In `data_update` scope, slug-gated like the other two data tools |
| `prepare_dashboard_update` | `slug, instructions, recurring?, update_type?, publish?, sources?` | prepare a pinned exact-slug one-time workflow or reusable user-owned scheduler prompt; never changes a page or schedules work. `sources` declares which MCP server/tool serves each input, optionally its `path` and date `partition`, so the prompt forbids substitutions and requires enumerating a partitioned folder rather than taking its newest file. **`sources` is required when `recurring` is true** (`update_sources_required`): an unattended run cannot safely re-derive bindings from prose. The response carries `execution_requirements` — the MCP servers, tools, network and model a run needs — so a scheduler can validate the task before accepting it |
| `get_version` | `slug, version_id` | one version's full row **including HTML** (read back a draft, inspect a rollback target). Prefer `preflight_page` when you only want to know whether the page *works* — it costs a few hundred tokens instead of the whole document |
| `find_in_version` | `slug, query, version_id?, max_matches?, ignore_case?` | literal search inside a stored version — bounded line numbers and short excerpts, not the document. Locates anchors for `patch_page` |
| `patch_page` | `slug, edits[{find,replace,count?}], base_version_id?, render_mode?, note?, publish?, expected_version?` | deploy a new version by applying anchored literal edits to the published HTML **server-side**. Each `find` must match exactly `count` times (default 1) or the whole patch is rejected. Concurrency-checked against the live pointer it read; `base_version_id` selects which version to patch from and does not change that check |
| `preflight_page` | `slug, version_id?` | static check of a stored version against the CSP and sandbox it is actually served under. Reports dead controls, blocked subresources, unparseable scripts, and Pages-managed blocks that fail their own contract (`managed_block_invalid` — the render layer `JSON.parse`s those, so a bad one serves a blank page) **without returning the HTML**. Defaults to the published version |
| `list_versions` | `slug, status?, limit?, cursor?` | bounded newest-first history; `is_published` is pointer equality and `is_live` also requires the page to be enabled |
| `list_workspaces` | `query?, limit?, cursor?` | workspaces and active-page counts; returned IDs feed workspace filtering/assignment |
| `create_workspace` | `name` | create reversible, global organization metadata without changing page URLs or serving |
| `rename_workspace` | `workspace_id, name` | rename a workspace without changing its member pages |
| `set_page_workspace` | `slug, workspace_id` | move a page; pass `null` for Ungrouped; page content, URL, and serving state are unchanged |
| `list_themes` | — | list curated themes a human admin may assign; agents cannot mutate themes |
| `create_upload_ticket` | `slug? \| template?, total_bytes, content_sha256` | **preferred for files** — open a staged upload and return a one-shot `upload_url` + `ticket` your shell PUTs the file to directly, so the bytes never pass through model output. Supply exactly one target: `slug` deploys a page, `template` registers a design |
| `start_page_upload` | `slug? \| template?, total_bytes, content_sha256` | begin a durable, token-bound staged upload for a workspace file or HTML over 20,000 UTF-8 bytes. Use only when outbound HTTP is unavailable |
| `append_page_upload` | `upload_id, sequence, chunk_base64` | append one ordered chunk (up to the `max_chunk_bytes` the start call returned — 49,152 by default); an exact sequence replay is idempotent |
| `cancel_page_upload` | `upload_id` | discard an uncommitted upload and free its active-upload slot; never changes a page/version |
| `deploy_page_upload` | `upload_id, title?, render_mode?, note?, publish?, expected_version?, require_approval?, client_id?` | SHA-verify and atomically create-or-update from the staged bytes; exact commit retries return the original result |
| `deploy_page` | `slug, html, title?, render_mode?, note?, publish?, expected_version?, require_approval?, client_id?` | **create-or-update** from small inline HTML. Publishes by default on open pages; gated pages land pending. Creation-only fields are ignored for an existing page |
| `update_page` | `slug, html, render_mode?, note?, publish?, expected_version?` | deploy small inline HTML to an existing page (fails if missing); same publish default and concurrency check |
| `update_page_data` | `slug, data, source_as_of, expected_version, publish?, note?, expect?` | validate and replace only the managed data block; source coverage is monotonic, publish defaults true, and exact retries dedupe. Returns **`data_profile`** (row counts, date extents, numeric totals, distinct values of low-cardinality keys) and **`data_warnings`** (coverage that starts later or ends earlier than what is already published, rows that dropped, dimension values that disappeared, a first payload with no baseline and no `expect` to check it against, or a refresh whose numbers did not move at all). `expect` states what you computed from the source and the write is **refused** (`data_reconciliation_failed`) if the payload disagrees |
| `list_templates` | — | stored designs: name, title, current revision, schema hashes, and how many pages were built from each |
| `get_template` | `template, revision?, include_html?` | **what a design needs**: its config and data JSON Schemas plus the reference config it ships with, in a couple of KB. HTML omitted unless asked. Call this instead of reading a dashboard's HTML. `revision.has_sample_data` says whether the design carries preview-only example rows |
| `list_template_revisions` | `template` | revisions newest-first, marking the current one. Pages pin the revision they were built from |
| `create_template_from_page` | `slug, template, empty_data, example_from_current_data?, title?, description?, note?` | **make a page you already like reusable.** Promotes its published bytes in place — nothing is re-authored and nothing crosses the wire. Requires the page to carry a `#pages-config` block; fails `page_not_template_managed` if it hardcodes its identity |
| `validate_template` | `upload_id? \| html?, name?` | **dry-run the contract, writing nothing.** With `upload_id` it reads staged bytes without consuming them, so the same upload still registers afterwards. Reports the contract, the name, `ships_empty`, example-data presence, and preflight |
| `template_urls` | `template, revision?` | the library URL plus a short-TTL signed preview URL on the sandboxed content host — populated with the design's example data when it has one |
| `delete_template` | `template, force?` | retire a template and free its name. Refused when pages were built from it unless `force`; those pages keep serving but lose their design provenance |
| `register_template_upload` | `upload_id, title?, description?, note?` | register staged bytes as a template revision. Validates the design against its own schemas; identical bytes return the same revision; nothing already deployed moves |
| `create_page_from_template` | `template, slug, config, revision?, data?, source_as_of?, render_mode?, title?, require_approval?, client_id?, publish?, note?` | build a page by sending **only its config** — the design is already stored. `config` must be complete and is never merged with the reference config. Omit `data` for the design's empty state. `page_exists` if the slug is taken; an identical retry dedupes only while that build is still the live version — or while the page has published nothing yet — so replaying a superseded create is refused rather than reverting the page to it |
| `get_page_config` | `slug` | the deploy-time config of a template-built page + its config schema, hashes, and the `live_version_id` to pass as `expected_version` |
| `update_page_config` | `slug, config, expected_version, publish?, note?` | replace the config block only; the data block is left **byte-for-byte** unchanged and source coverage is carried across, so settings changes cannot move numbers. Replaces, does not merge |
| `list_template_pages` | `template` | every live page whose history touches the design: who is serving which revision, which are behind the current one, and which have **drifted** off it (`drifted:true` — a later raw `deploy_page`/`patch_page` detached them, so a design fix no longer reaches them; `last_revision` is what to pull them back onto). Read-only — Pages never re-renders a page on its own |
| `rerender_page_from_template` | `slug, template?, revision?, publish?, expected_version?, note?` | move **one** page onto a template revision, keeping its own config and data and re-validating both against the target schemas. `publish` defaults to **false** so a human previews the design first. No bulk rerender exists |
| `update_page_data_upload` | `upload_id, slug, source_as_of, expected_version, publish?, note?, expect?` | publish a managed-data payload **staged with `create_upload_ticket` (`kind:'data'`)** instead of sending it inline — the JSON never passes through the model's context, so no inline ceiling applies. Identical to `update_page_data` from the parse onward: same schema validation, monotonic `source_as_of`, mandatory `expected_version`, dedupe, `data_profile`/`data_warnings`, and `expect` reconciliation, through one write path. An upload staged as `page` is refused here and one staged as `data` is refused by `deploy_page_upload` |
| `configure_page_refresh` | `slug, instructions?, recurring?, update_type?, publish?, daily_at_utc?, workflow?, run_now?` | read-only compatibility alias for `prepare_dashboard_update`; legacy workflow/cadence input becomes user-owned prompt text and `run_now` never executes work |
| `publish_page` | `slug, version_id, expected_version?` | publish a draft (open pages only) |
| `rollback_page` | `slug, version_id?, expected_version?, note?` | move the live pointer to an approved version (omit id → previous). `note` is recorded in the audit log — a rollback republishes bytes that already exist, so the reason is not inferable from any diff |
| `set_password` | `slug, password` | set/change the non-empty per-page **client password** so an outside client can open it; clearing is admin-only |
| `set_title` | `slug, title` | rename a page |
| `delete_page` | `slug` | **soft-delete** (reversible by an admin; frees the slug). Open pages only for agents |
| `page_urls` | `slug` | admin / view / live URLs |

List tools use filter-bound, opaque keyset cursors. `limit` defaults to 50 and is
capped at 100; pass an unchanged filter set with `next_cursor` to fetch the next
page. Do not inspect or synthesize cursor contents.

Deploy/update/publish/rollback results distinguish two states:

- `version_is_live`: the version returned by this call is the current published
  pointer **and** the page is enabled. `live` is a compatibility alias for this.
- `page_is_live`: some published version is currently serving. This can be true
  while `version_is_live` is false, for example when a new gated version is pending
  and the previously published version remains live.
- `live_version_id`: the version actually at the live pointer, or `null`.

Always follow `next_step`. Share `urls.live` as the new deliverable only when
`version_is_live:true`; for a pending gated version, hand the user `urls.admin`.
Business errors (404/409/403, such as publishing a gated page) return a normal
tool result with `isError:true` and `{ ok:false, error, code, details? }`. Malformed
JSON-RPC, an unknown method/tool, or invalid arguments are protocol/tool
validation errors.

`expected_version` is optimistic concurrency on deploy, update, publish, and
rollback: pass the published version ID you most recently read. If another
writer moved the pointer, the tool returns `409` (`code: stale_version`) instead
of silently overwriting that newer decision. Every tool that accepts it checks
it on **every** path, including the two that do not move the pointer themselves —
`publish:false`, and any write to an approval-gated page.
(`create_page_from_template` does not take the argument; it protects itself by
scoping its retry to the version the page is actually serving.) The gated page is the one that matters
most: a version built from a stale read still reverts the newer one when a human
approves it, and the human has no way to see that from the queue.

### Dashboard update prompts

When the user says “update `<slug>` dashboard with …”, call
`prepare_dashboard_update` with the exact existing slug and request. Follow its
returned prompt immediately for a one-time update. With `recurring:true`, show
the prompt to the user verbatim for installation in their scheduler; do not run
it now or claim Pages configured anything. The result routes to managed data,
full-page update, adaptive classification, or a one-time same-slug managed-data
migration and pins the relevant live version/schema. See
[DATA_UPDATES.md](DATA_UPDATES.md).

Pass `sources` when you know which connector serves each input. A request that
only names its data in prose ("the newest Amazon DSP delivery data") leaves the
executing agent to guess, and that agent may not even have the server loaded — so
every prompt now requires it to enumerate its available tools, bind each input to
a specific server and tool, and stop without writing when one is unreachable
instead of substituting another source or carrying prior totals forward. A
full-page update also deploys with `publish=false`, verifies that version, and
only then publishes, so a truncated or blank render never replaces a working
dashboard.

`get_page_refresh` and `configure_page_refresh` remain only as read-only aliases
for the static Pages allowlists already deployed in Chat and Cutlass. They do
not expose, create, run, or pause a schedule. `run_page_refresh_now` and
`pause_page_refresh` are no longer advertised.

### Large dashboard files

Do not paste a workspace file or more than 20,000 UTF-8 bytes into one
`deploy_page`/`update_page` argument. Model/provider output can truncate before
the MCP request reaches Pages, even though the HTTP endpoint accepts larger
bodies. Never pass a filename, `$(cat ...)`, or placeholder as `html`.

**Preferred — an upload ticket (the bytes never enter your context).** If your
environment can make outbound HTTP requests, this is one tool call and one
shell command regardless of file size:

1. Compute the exact file byte count and lowercase SHA-256.
2. Call `create_upload_ticket` with the target slug and those values.
3. Run the returned `curl` (substituting your file path):
   ```
   curl -fsS -X PUT --data-binary @dashboard.html \
     -H "Authorization: Bearer <ticket>" <upload_url>
   ```
4. Call `deploy_page_upload` with the `upload_id`.

The ticket is write-only, single-upload, expires in minutes, and is pinned to
the byte count and SHA-256 you declared — it can stage exactly the document you
already committed to and nothing else. Deploying still requires your agent
token.

**Fallback — base64 chunks.** Only when outbound HTTP is unavailable. Every one
of these bytes is output your model has to generate perfectly, so prefer the
ticket:

1. Compute the exact file byte count and lowercase SHA-256.
2. Call `start_page_upload` with the target slug and those values.
3. Base64-encode the original bytes in order, at most the returned
   `max_chunk_bytes` (49,152 raw bytes by default) per call. Call
   `append_page_upload` with sequence `0`, then each returned `next_sequence`.
4. When `complete:true`, call `deploy_page_upload`. Do not resend the HTML.

Call `cancel_page_upload` if the local file changes mid-upload or an upload is
abandoned, then start again with the new byte count/hash.

Uploads are stored in PostgreSQL, bound to the bearer token, limited to 2 MiB
and five active handles per token, and expire 24 hours after inactivity. Exact
chunk retries are deduplicated. Byte count, SHA-256, and UTF-8 are verified
before the version, publish pointer, audit row, and upload commit are written in
one transaction. An exact `deploy_page_upload` retry returns its saved result.

### Try it (curl)

After the `initialize` / `notifications/initialized` exchange (the complete
sequence is in `scripts/mcp-smoke.sh`), a tool call looks like:

```bash
TOKEN=...   # from `pages token add`
curl -s https://pages.elcanotek.com/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2025-11-25' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",
       "params":{"name":"deploy_page",
                 "arguments":{"slug":"northwind","title":"Northwind","html":"<h1>hi</h1>"}}}'
# publishes live on the open page; the result's urls.live is the client link.
```

### Registering this server

Both agents already support HTTP MCP servers with headers — registration is
config-only (no code in this repo).

Each consumer pins a **static** tool allowlist, and a name missing from that list
is never registered with the model — silently, with nothing in any log to say
why. So copy this block verbatim. A unit test pins it to the live registry here
and in [INTEGRATION.md](INTEGRATION.md), so *this block* is always current;
nothing in Pages can check what a consumer actually deployed.

<!-- pages:allowlist:start -->
```text
list_pages, get_page, get_page_data, get_page_refresh, record_refresh_check,
prepare_dashboard_update, find_in_version, patch_page, preflight_page,
get_version, list_versions, update_page_data_upload,
list_workspaces, create_workspace, rename_workspace, set_page_workspace,
list_themes, create_upload_ticket, start_page_upload, append_page_upload,
cancel_page_upload, list_templates, get_template, list_template_revisions,
list_template_pages, rerender_page_from_template, register_template_upload,
validate_template, template_urls, delete_template, create_template_from_page,
deploy_page_upload, create_page_from_template, get_page_config,
update_page_config, deploy_page, update_page, update_page_data,
configure_page_refresh, publish_page, rollback_page, set_password, set_title,
delete_page, page_urls
```
<!-- pages:allowlist:end -->

That is the complete **45-tool** catalog. A consumer list written for the
update-prompt transition still works — it carries the two compatibility names —
but any list predating templates, `find_in_version`, `patch_page`,
`preflight_page`, or `create_upload_ticket` does not carry those, and **a name a
consumer does not list is a tool its model never sees**. Pages cannot read a
consumer's manifest, so this is not something a test here can catch: diff each
consumer's list against the block above rather than assuming it is current. New
integrations should take the whole block and prefer `prepare_dashboard_update`
over the compatibility names. The matching examples are in
[INTEGRATION.md](INTEGRATION.md).

- **cutlass** — add a block to `getMCPServerDefinitions()` (mirror `fast_io`):
  URL `https://pages.elcanotek.com/mcp`,
  `Authorization: Bearer <PAGES_MCP_TOKEN>`, the allowlist above. Add
  `PAGES_MCP_TOKEN` to the allowed env vars + a `PagesMCPToken` config field.
- **chat** — registered via `buildMCPSpecs()` (HTTP URL + `Authorization` header,
  gated on `cfg.PagesAPIToken`), Optional + off by default.

## REST (equivalent)

| Method & path | Body | Purpose |
|---------------|------|---------|
| `GET  /api/v1/pages` | — | list pages (+ `has_password`, `is_live`) |
| `POST /api/v1/pages` | `{slug, title?, client_id?, require_approval?}` | create a page |
| `GET  /api/v1/pages/:slug` | — | metadata + published version + `urls` |
| `POST /api/v1/pages/:slug/versions` | `{html, render_mode?, note?, publish?, expected_version?}` | deploy/update (`deploy_page`/`update_page`) |
| `GET  /api/v1/pages/:slug/versions` | — | history |
| `GET  /api/v1/pages/:slug/versions/:id` | — | one version (html + meta) — the REST `get_version` |
| `GET  /api/v1/pages/:slug/preflight` | `?version_id=` | findings for a stored version — the REST `preflight_page` (defaults to the published version) |
| `PUT  /upload/:upload_id` | raw page bytes | send a ticketed upload's content. **Ticket auth, not agent-token auth** (`Authorization: Bearer <ticket>`); write-only and content-pinned. Not under `/api/v1` |
| `POST /api/v1/pages/:slug/publish` | `{version_id, expected_version?}` | publish a draft |
| `POST /api/v1/pages/:slug/rollback` | `{version_id?, expected_version?}` | rollback the pointer |
| `POST /api/v1/pages/:slug/password` | `{password}` | set/change the client password (clearing is admin-only → 403 on bearer) |
| `POST /api/v1/pages/:slug/title` | `{title}` | rename |
| `DELETE /api/v1/pages/:slug` | — | soft-delete (open pages; gated → 403) |

Note `POST /versions` defaults `publish:false`, which is exactly the path that
used to ignore `expected_version`. If you pass one there, it is now enforced: a
stale value returns `409 stale_version` where it previously returned `201`.

A duplicate deploy (same content sha) returns the existing version with
`deduped: true` and HTTP `200` instead of `201`. Note that "the existing
version" may be an **older** one — dedupe keys on content, not on the pointer —
so re-deploying bytes a page served in the past, with `publish` on, moves the
live pointer back to that version. That is the content you asked for, but if
what you meant was "return this page to an earlier state", `rollback_page`
records the reason in the audit log and a diff cannot show it otherwise. Note REST `POST /versions`
defaults `publish:false` (explicit) — the MCP `deploy_page` defaults it true.

### Admin organization API

The admin landing page uses a separate Elcano-admin cookie + CSRF surface for
one-level organization and page governance. Workspaces do not
change slugs, public URLs, approval settings, published pointers, or version
history. Existing pages have a null `workspace_id` and appear in **Ungrouped**
automatically.

| Method & path | Body | Purpose |
|---------------|------|---------|
| `GET /api/v1/admin/pages` | — | page index with `workspace_id`, `workspace_name`, plus `workspaces[]` and response-consistent `page_count` values |
| `GET /api/v1/admin/pages/:slug` | — | page detail, versions, and themes |
| `GET /api/v1/admin/pages/:slug/versions/:id` | — | one version's full row **including `html`** — the detail read above deliberately strips `html` from every entry in `versions` (the whole history would be megabytes), so the admin source editor reads the one version it is opening. `404` if that version belongs to another page |
| `GET /api/v1/admin/workspaces` | — | list workspaces and active-page counts |
| `POST /api/v1/admin/workspaces` | `{name}` | create a workspace (case-insensitive unique name, max 100 characters) |
| `POST /api/v1/admin/workspaces/:id/rename` | `{name}` | rename a workspace |
| `POST /api/v1/admin/workspaces/:id/delete` | `{}` | remove a workspace and move every member to Ungrouped; pages are never deleted |
| `POST /api/v1/admin/pages/:slug/workspace` | `{workspace_id}` | assign/move a page; pass `null` for Ungrouped |

Every admin mutation requires the normal `X-CSRF-Token` and same-origin
`Origin` headers and is written to `audit_log` in the same transaction.
MCP agents have the reversible `list_workspaces`, `create_workspace`,
`rename_workspace`, and `set_page_workspace` subset; only a human admin can
delete a workspace and move all of its members to Ungrouped at once.

Slugs may be flat (`northwind`) or nested (`northwind/q2`); nested slugs work
unencoded in every route above (`GET /api/v1/pages/northwind/q2/versions`).
Creation rejects (`400 reserved_slug`) any slug with a path segment that
collides with a route: `raw`, `assets`, `healthz`, `welcome`, `portal` (the
partner portal entry point on the content host), or a sub-resource action name
(`versions`, `publish`, `rollback`, …). Restore refuses one too — resurrecting a
row that predates a reservation would put a live page at an address the router
owns. Only whole segments count, so `portals` and `my-portal` are fine.

### Partner portals (admin only)

A **portal** is one shared client credential over an admin-curated *set* of
dashboards — the "one link, one password, move between them" surface a partner
needs when they own six campaign pages. It is deliberately **not** organization
like a workspace: membership decides which client's numbers that credential
opens, so there is no agent-facing equivalent of any route below, **portals add
zero MCP tools**, and `lib/portals.js` refuses any actor that is not a human
admin (`403 portal_admin_only`).

| Method & path | Body | Purpose |
|---------------|------|---------|
| `GET /api/v1/admin/portals` | — | live portals with `page_count`, the home page, and the partner `url` |
| `GET /api/v1/admin/portals/:id` | — | one portal, its partner `url`, and its ordered membership |
| `POST /api/v1/admin/portals` | `{slug, name, password?}` | create. `slug` is ONE url-safe segment; omit `password` and Pages generates a ~79-bit one. **The plaintext is in this response and nowhere else** |
| `POST /api/v1/admin/portals/:id/rename` | `{name}` | rename (case-insensitively unique among live portals) |
| `POST /api/v1/admin/portals/:id/password` | `{password?}` | rotate the credential — which is also how every live session for the portal is revoked |
| `POST /api/v1/admin/portals/:id/pages` | `{slug, label?, sort_order?}` | add a page; returns `reclassifies_staff_only` |
| `POST /api/v1/admin/portals/:id/pages/update` | `{slug\|page_id, label?, sort_order?}` | edit the curated label/order. Absent fields are left alone; `label: null` falls back to the page title |
| `POST /api/v1/admin/portals/:id/pages/remove` | `{slug\|page_id}` | remove one page from this portal |
| `POST /api/v1/admin/portals/:id/home` | `{slug}` | the dashboard a partner lands on; `null` clears it. Must already be a member |
| `POST /api/v1/admin/portals/:id/delete` | `{}` | retire: soft delete, frees the slug and name, ends every session for it; member pages are untouched |

Portals are managed on the **`/admin/portals`** screen — the only surface anywhere
that changes which dashboards a credential opens. It shows the partner link to hand
out, warns *before* the click when adding a dashboard that has no client password of
its own (that add is what makes it readable), shows a generated password exactly
once in a dialog that must be dismissed, and flags any member a partner cannot open
or that will show no `Page` menu.

`GET /api/v1/admin/pages/:slug` also returns `portals[]`, every portal that
exposes that page — because a page missing from one audience's portal is
invisible if you only ever look portal-first.

What is worth knowing before using these:

- **The page travels in the body, not the path.** Page slugs nest (`nwm/contoso`)
  and would be ambiguous against an action suffix.
- **Adding a staff-only page to a portal reclassifies it.** A page with no client
  password is Elcano-only; putting it in a portal is what will make it
  client-readable, so the response carries `reclassifies_staff_only: true` and the
  audit row records it. Surface that in the UI at the moment of adding, not after.
- **A page may sit in at most four portals** (`409 portal_fanout_exceeded`,
  enforced at add time): each additional one is another scrypt verification
  against a single password submission once portal sessions exist.
- **`label` exists because `pages.title` is agent-settable.** The partner-facing
  title is curated per membership, so an agent renaming a page cannot rewrite what
  a partner reads.
- **A supplied password must be at least 16 characters** (`400
  portal_password_too_weak`) and is never trimmed — a pasted trailing newline is
  rejected rather than silently changed.
#### The partner-facing side: `<content-host>/portal/<slug>`

The address in each portal's `url` is a real page on the content host, and it is
the surface that always works — Pages' own chrome, not a template rendering a
block:

- **No session → the portal password form.** One credential, submitted back to
  the same path, rate-limited by the same strict brute-force limiter as a page
  password and backed by a progressive per-portal delay shared across every source
  IP. An unknown or retired portal is a plain 404: which portals exist is not
  public.
- **With a session → the list of dashboards that credential opens**, read live on
  every request, so adding or removing one takes effect on the partner's next
  load with no re-login. The list contains only pages that will actually open (a
  taken-down, unpublished, or deleted member is omitted rather than listed as a
  dead link), shows the curated label rather than the agent-settable page title,
  and puts the home page first.
- The index is **not sandboxed** — it is ours, and scriptless — so its links are
  ordinary links. Rotating the password or retiring the portal invalidates every
  live session for it immediately.

A portal session opens the portal's **member pages** too, so following a link from
the index does not ask for anything again. What that does and does not mean:

- Membership is re-read on every request, and a portal-authorised render sets **no
  cookie** — so removing a page from the portal closes it on the partner's next
  load rather than in thirty days.
- Disabled, unpublished and deleted pages still 404 first; membership binds to the
  page id, so a page deleted and recreated at the same slug is a stranger to the
  portal.
- A member page's own password form also accepts the portal password (and mints a
  portal session, not a page one), so a partner who bookmarked one dashboard is
  not stranded. A wrong guess there charges the page's counter *and* every portal
  counter it tested.
- A member with no password of its own prompts for the portal credential instead
  of the staff-only refusal. No gate names a portal.

#### Rolling a partner onto a portal, in order

The ordering matters, because two of these steps sign people out:

1. **Create the portal** and copy the password. It is shown once; if you lose it,
   rotate (which signs out anyone already using the old one).
2. **Add their dashboards**, labelling each one the way the partner should read it.
   Set the macro view as the home dashboard so it sorts first. If a dashboard has no
   client password of its own, adding it here is what makes it readable — the screen
   says so at that moment, and the audit log records it.
3. **Send the link and the password separately**, and confirm the partner is in
   before touching anything else.
4. **Only then clear the per-page passwords** you are replacing, if you want the
   portal to be the single way in. Clearing a page password invalidates every live
   page session for it (`credentialDigest`), so doing this before step 3 logs out
   whoever is using the old link — including the partner you are migrating.

To take access away, in increasing order of severity: remove one dashboard from the
portal (effective next request, that dashboard only), rotate the password (signs
everyone out, portal survives), retire the portal (link stops working, memberships
and audit trail kept). None of them touches a page, its versions, or its own
password.

#### The in-page page switcher (`#pages-nav`)

Every **themed** render that a portal authorised carries one extra `<head>` tag:

```html
<script type="application/json" id="pages-nav" data-flag-injected>
{"portal":{"slug":"nwm","name":"Northwind Media Group"},
 "pages":[{"slug":"nwm-contoso","title":"Contoso — Allergex",
           "url":"https://elcano-pages.com/nwm-contoso","current":true}],
 "truncated":false}
</script>
```

- **The author declares nothing, and does not have to read it either.** Pages
  injects the block, and also injects a small built-in `Page ▾` control unless the
  document already references `#pages-nav` — in which case the design owns the menu
  and Pages adds only the data. That fallback is what makes portals work on
  dashboards that predate the feature; without it the switcher only ever appeared
  on pages authored after it shipped, which is almost none of them. `#pages-nav` is
  Pages' id — a document shipping its own copy has it stripped at deploy time, so
  exactly one answer is ever served.
- **The built-in control cannot move a design it lands in.** It is `position: fixed`
  top-right, width-capped, hidden in print, and asserted to change neither the
  scroll width nor the height of the dashboard at 390/768/1440. It builds real
  anchors with `textContent`, closes on Escape and on a click away, and stays away
  when the portal holds only the page you are already on.
- **The list is the authorising portal's**, never the union of the portals holding
  the page. A co-branded dashboard shows two different lists to two partners, and
  neither learns of the other. A viewer holding two portal cookies gets the lowest
  portal id, every time.
- **`url` is ready-made and absolute.** Never build the href in a template: slugs
  nest, so on `/a/b/c` a relative `d` resolves to `/a/b/d`, and `base-uri 'none'`
  means a `<base>` tag cannot correct it.
- **Read it defensively and render titles with `textContent`.** The block is
  optional by design, so `JSON.parse(document.getElementById("pages-nav")…)` with
  no null guard is a `TypeError` that halts your whole script. And a sibling's
  title is set by whoever owns that page — `escapedJson` guarantees it cannot
  break out of the block, but `innerHTML = title` would execute it inside your
  dashboard.
- **Bounded:** ≤50 entries (with `truncated: true` when clipped), ≤200 characters
  per title and slug, ≤16 KiB serialised. It ships on every render.
- **`raw` pages get the switcher too — and still no theming.** A raw page in a
  portal receives the payload and the built-in control, but never Flag tokens,
  fonts or the theme controller: what `raw` guarantees is that Pages will not
  restyle the design, and that is untouched. With no portal authorising the view it
  is byte-for-byte as before. (This matters more than it sounds: 18 of 31 live
  dashboards are raw, so the earlier "deploy it themed instead" advice would have
  restyled more than half the fleet to add a menu.) Preflight warns
  (`nav_block_ignored`) when a raw page reads the block, and
  `GET /api/v1/admin/portals/:id` reports `shows_switcher` (does this member show a
  menu at all — false only for `raw`) and `switcher_is_own` (does the design render
  its own, rather than using the built-in control) per member.
- Returning from a sibling is a **full reload**: `Cache-Control: no-store`
  suppresses bfcache, so a template's date range and filter selections reset.
  There is no fix without storage, and storage throws in the sandbox.

---

## Authoring a page (the part that matters most)

1. **Write a Flag-themed page.** Use Flag's semantic CSS tokens
   (`var(--color-primary)`, `var(--color-bg)`, `var(--font-heading)`, …) and
   component classes — do **not** redefine the tokens yourself. Pages renders in the
   Flag design system by default; per-client brand themes override a few tokens at
   render time. See the Flag `AGENT_GUIDE.md`.
2. **No external CDNs.** They are blocked by CSP and the page will silently lose
   styling/scripts. Charting libs (Chart.js, ECharts) and Flag fonts are served
   locally from the content host's `/assets/`. Reference those, or inline small JS.
3. **Images**: there is **no asset upload API** — inline small images as `data:` URIs.
4. **Deploy.** For small inline HTML, `deploy_page(slug, html, …)` creates the
   page if needed. For a workspace file or content over 20,000 UTF-8 bytes, use
   `create_upload_ticket` → one `curl -X PUT` → `deploy_page_upload` (or the
   `append_page_upload` chunk fallback where outbound HTTP is unavailable);
   never shrink a dashboard merely to fit one tool call.
   Both paths publish an open page by default — share the result's `urls.live`. On
   an approval-gated page the version is forced to **pending**; hand the user the
   `urls.admin` URL for a human to approve. Always read `live`/`next_step`.
5. **Make it client-accessible.** `set_password(slug, password)` — the client
   opens `urls.live` and enters the password. Without a password a page is
   Elcano-staff-only (outside visitors get a "staff-only" gate).
6. **Update later.** `update_page(slug, html)` is just another version — old versions
   stay for rollback. "Update the northwind dashboard" → a new version, not a
   regenerate-from-scratch. For a *targeted* change — a CSS rule, a handler name,
   a label — prefer `find_in_version` to locate the anchor and `patch_page` to
   change it: the document never leaves the server, so the edit costs anchors
   rather than two full copies of the dashboard. Read a prior version's source
   with `get_version` only when you genuinely need the whole thing.
7. **Remove.** `delete_page(slug)` soft-deletes (reversible by an admin; frees the
   slug). Confirm with the user first.

### render_mode

- `themed` (default): you write content; Pages injects Flag tokens + the client
  theme. Use this almost always.
- `raw`: a fully self-contained document served verbatim (no Flag injection). Only
  for a deliberately bespoke external/client brand palette.

### What agents cannot do

Approve/reject a version, toggle the approval gate, disable a page, set the theme,
**clear** a password, or delete/restore an **approval-gated** page — those are
**human/admin** actions in the dashboard. Deleting a workspace is also human-only;
agents may create/rename workspaces and move pages because those operations are
reversible and do not alter serving. Agents also cannot publish, roll back, or
delete a page an admin has **disabled** (taken down).

See `PLAN.md` for the full design and `docs/SECURITY.md` for the auth model and
known follow-ups.
