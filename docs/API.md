# Pages — API & MCP guide for agents

Two equivalent surfaces, both on the **dashboard host** (`pages.elcanotek.com`),
both authenticated with an agent **bearer token**, both routing every change
through the same version state machine (no privileged backdoor):

- **REST** — `POST/GET https://pages.elcanotek.com/api/v1/*` (PLAN.md §10)
- **MCP-over-HTTP** — `POST https://pages.elcanotek.com/mcp` (PLAN.md §11)

> Status: REST + MCP agent surfaces are live (deploy / publish / rollback / read).
> Admin actions (approve, reject, disable, approval-toggle) are **not** on these
> surfaces — they are human-only in the `/admin` shell.

## Auth

`Authorization: Bearer <token>` on every request. Mint tokens with
`pages token add <label>` (prod) or `node scripts/token.js add <label>` (local).
The raw token is shown once. One token per agent so chat/cutlass revoke
independently.

## MCP

Plain **JSON-RPC 2.0** over a single `POST /mcp`. Protocol version `2024-11-05`.
Methods: `initialize`, `tools/list`, `tools/call` (plus `ping` and the
`notifications/*` no-ops). Responses are `application/json`.

### Tools

| Tool | Args | Does |
|------|------|------|
| `list_pages` | — | all pages + each one's live version id |
| `get_page` | `slug` | metadata + the published version (html) + routing urls |
| `deploy_page` | `slug, html, title?, render_mode?, note?, publish?` | **create-or-update** then deploy a new version. `publish:true` → live on open pages; on gated pages it lands `pending` |
| `update_page` | `slug, html, render_mode?, note?, publish?` | deploy to an existing page (fails if missing) |
| `publish_page` | `slug, version_id, expected_version?` | publish a draft (open pages only) |
| `rollback_page` | `slug, version_id?, expected_version?` | move the live pointer to an approved version (omit id → previous) |
| `list_versions` | `slug` | full history incl. drafts + pending queue |
| `page_urls` | `slug` | admin / view / live URLs (where to send a human) |

`tools/call` returns the result as JSON text in `content[0].text`. Business
errors (404/409/403 — e.g. trying to publish a gated page) come back as a normal
result with `isError: true` and `{ error, code }` so you can react; only
protocol problems (unknown tool/method) are JSON-RPC errors.

`expected_version` is optimistic concurrency: pass the version you believe is
live; a mismatch returns `409` (`code: stale_version`) so you don't clobber a
concurrent change.

### Try it (curl)

```bash
TOKEN=...   # from `pages token add`
curl -s https://pages.elcanotek.com/mcp \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call",
       "params":{"name":"deploy_page",
                 "arguments":{"slug":"omnicom","title":"Omnicom","html":"<h1>hi</h1>","publish":true}}}'
```

### Registering this server

Both agents already support HTTP MCP servers with headers — registration is
config-only (no code in this repo):

- **cutlass** — add a block to `getMCPServerDefinitions()` (mirror `fast_io`):
  ```go
  {
    name:       "pages",
    serverType: mcpServerTypeHTTP,
    URL:        "https://pages.elcanotek.com/mcp",
    isEnabled:  func(c *Config) bool { return c.PagesAPIToken != "" },
    headerBuilder: func(c *Config) map[string]string {
      return map[string]string{"Authorization": "Bearer " + c.PagesAPIToken}
    },
    toolAllowlist: []string{
      "list_pages", "get_page", "deploy_page", "update_page",
      "publish_page", "rollback_page", "list_versions", "page_urls",
    },
  },
  ```
  Add `PAGES_API_TOKEN` to the allowed env vars + a `PagesAPIToken` config field.
- **chat** — register via `buildMCPSpecs()` (HTTP URL + `Authorization` header,
  gated on `cfg.PagesAPIToken`).

## REST (equivalent)

| Method & path | Body | Purpose |
|---------------|------|---------|
| `GET  /api/v1/pages` | — | list pages |
| `POST /api/v1/pages` | `{slug, title?, client_id?, require_approval?}` | create a page |
| `GET  /api/v1/pages/:slug` | — | metadata + published version |
| `POST /api/v1/pages/:slug/versions` | `{html, render_mode?, note?, publish?, expected_version?}` | deploy/update (this is `deploy_page`/`update_page`) |
| `GET  /api/v1/pages/:slug/versions` | — | history |
| `GET  /api/v1/pages/:slug/versions/:id` | — | one version (html + meta) |
| `POST /api/v1/pages/:slug/publish` | `{version_id, expected_version?}` | publish a draft |
| `POST /api/v1/pages/:slug/rollback` | `{version_id?, expected_version?}` | rollback the pointer |

A duplicate deploy (same content sha) returns the existing version with
`deduped: true` and HTTP `200` instead of `201`.

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
3. **Images** go through the assets API (returns an `/assets/<sha>` URL) or, if tiny,
   inline `data:` URIs.
4. **Deploy.** `deploy_page(slug, html, …)` creates a new version. On an open page
   you may `publish=true` to make it live; on an approval-gated page it is forced to
   **pending** and a human approves it — hand the user the `/admin/<slug>` URL
   (`page_urls`).
5. **Update later.** `update_page(slug, html)` is just another version — old versions
   stay for rollback. "Update the omnicom dashboard" → a new version, not a
   regenerate-from-scratch.

### render_mode

- `themed` (default): you write content; Pages injects Flag tokens + the client
  theme. Use this almost always.
- `raw`: a fully self-contained document served verbatim (no Flag injection). Only
  for deliberately bespoke output.

### What agents cannot do

Approve/reject, disable a page, or toggle approval — those are **human/admin**
actions in the dashboard. Your job is to deploy and (on open pages) publish.

See `PLAN.md` for the full design and auth model.
