# Pages — API & MCP guide for agents

> Stub. The contract is specified in [`../PLAN.md`](../PLAN.md) §10 (REST), §11
> (MCP), and §8 (theming + library policy). This file gets fleshed out in Phase 3,
> when the API and MCP land. Below is the orientation an authoring agent needs.

## How to make a page (the short version)

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
4. **Deploy.** `deploy_page(slug, html, …)` creates a new **draft** version. On an
   open page you may `publish=true` to make it live; on an approval-gated page it is
   forced to **pending** and a human approves it — hand the user the `/admin/<slug>`
   URL (`page_urls`).
5. **Update later.** `update_page(slug, html)` is just another version — old versions
   stay for rollback. "Update the omnicom dashboard" → a new draft/version, not a
   regenerate-from-scratch.

## render_mode

- `themed` (default): you write content; Pages injects Flag tokens + the client
  theme. Use this almost always.
- `raw`: a fully self-contained document served verbatim (no Flag injection). Only
  for deliberately bespoke output.

## What agents cannot do

Approve/reject, disable a page, or toggle approval — those are **human/admin**
actions in the dashboard. Your job is to deploy and (on open pages) publish.

See `PLAN.md` for the full endpoint/tool tables and auth model.
