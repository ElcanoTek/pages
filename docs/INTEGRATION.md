# Connecting to Pages MCP

> **A note on names.** *Chat*, *Cutlass*, *MOC* and *Fleet* are ElcanoTek's own
> internal agent tools and orchestration services — the original consumers of
> this interface. None of them is part of this repository and Pages depends on
> none of them; any MCP client works identically. Where this page says a name is
> "already registered" or "already allowlisted", read it as a note about
> ElcanoTek's deployment, not a requirement of Pages.

Pages exposes a stateless MCP Streamable HTTP server at
`https://pages.elcanotek.com/mcp`. Chat and Cutlass are already registered for
this server. The dashboard-update prompt transition requires **no code or config
change** in Chat, Cutlass, MOC, or Fleet.

The deployed Chat/Cutlass allowlists already include `get_page_refresh` and
`configure_page_refresh`. Pages retains those names as read-only compatibility
tools:

- `get_page_refresh` now reports that scheduling is user-owned and directs the
  client to prompt preparation; it exposes no Pages schedule state.
- `configure_page_refresh` is an alias for the canonical read-only
  `prepare_dashboard_update` operation. It no longer configures or dispatches
  anything.

The former `run_page_refresh_now` and `pause_page_refresh` tools are not
advertised. Extra names in a consumer’s static allowlist are inert when the MCP
server does not publish them. New clients should allow the canonical
`prepare_dashboard_update` name; existing Chat/Cutlass can use the compatibility
alias without a rollout.

See [DATA_UPDATES.md](DATA_UPDATES.md) for the one-time and recurring user flows,
[TEMPLATES.md](TEMPLATES.md) for building many pages from one stored design, and
[API.md](API.md) for the complete tool catalog.

## Connection contract

Every request uses the dashboard host and bearer authentication:

```text
URL: https://pages.elcanotek.com/mcp
Authorization: Bearer <Pages token>
Content-Type: application/json
Accept: application/json, text/event-stream
```

New clients initialize with protocol `2025-11-25`, send
`notifications/initialized`, and include the negotiated
`MCP-Protocol-Version: 2025-11-25` header on later POSTs. Pages is stateless: do
not require an `Mcp-Session-Id`, do not batch JSON-RPC messages, and do not rely
on a server-sent event stream. A compatibility path remains for the already
deployed `2024-11-05` clients.

The existing environment variables are sufficient:

- Chat: `PAGES_API_TOKEN` and optional `PAGES_MCP_URL` override.
- Cutlass: `PAGES_MCP_TOKEN` and optional `PAGES_MCP_URL` override.

There are no `PAGES_MOC_*`, `PAGES_REFRESH_*`, runner-name, or OpenRouter
requirements on the Pages server. Source/LLM credentials belong to the client
that executes a returned prompt, not Pages.

## Test Pages without changing a consumer

Start Pages locally and exercise the exact MCP wire flow:

```bash
sudo bash scripts/dev.sh
bash scripts/mcp-smoke.sh
```

This covers initialize, lifecycle notification, tool discovery/calls, bearer
auth, media types, and protocol headers. It needs no agent and no LLM key.

For interactive inspection:

```bash
npx @modelcontextprotocol/inspector
# Transport: Streamable HTTP
# URL:       http://127.0.0.1:3099/mcp
# Header:    Authorization: Bearer <paste .devdata/agent-token>
```

To test the existing Chat or Cutlass registration against local Pages, set only
its normal Pages URL override and token. No source edits are needed:

```bash
export PAGES_MCP_URL="http://127.0.0.1:3099/mcp"
export PAGES_MCP_TOKEN="$(cat /root/pages/.devdata/agent-token)" # Cutlass
# or PAGES_API_TOKEN for Chat
```

Then ask:

> Update `acme/daily-performance` dashboard with yesterday’s complete source
> data.

For the prompt-only recurring path, ask:

> Give me the reusable prompt to update `acme/daily-performance` every day from
> the complete source data. Do not run it now.

The first request should cause the client to follow the prepared workflow in the
same conversation. The second should return the prompt text to the user and
must not create a Pages task, schedule, token, grant, or version.

## Tokens

Each general-purpose client keeps its existing broad bearer token:

```bash
pages token add cutlass
pages token add chat
pages token list
pages token revoke <id>
```

The raw value is displayed once. Use one token per client so revocation remains
independent.

An external unattended scheduler may optionally use a manually scoped identity:

```bash
pages token add client-daily data_update acme/daily-performance
```

That token can call only `get_page_data` and `update_page_data` for its explicit
slug. Pages does not automatically create or modify these grants. The broad
Chat token is sufficient for user-directed preparation and execution, so no
per-dashboard server token is required for normal team use.

## Static allowlists go stale — this is the failure mode

Chat and Cutlass each register Pages with a **static tool allowlist**, and both
enforce it the same way: a tool whose name is not in the list is never registered
with the LLM at all. Extra names are inert, but a *missing* name is invisible —
the agent simply cannot see the tool, and nothing in the logs says why. Every
tool Pages has added since a consumer's list was written is unreachable there.

So: **when Pages adds a tool, a consumer with a static allowlist does not get it
until its list is updated.** Prefer using the catalog `tools/list` advertises.

**Partner portals add zero tools, deliberately.** The list below is unchanged by
that whole feature, and no consumer needs to touch its allowlist. Membership decides
which client's numbers one partner credential opens, so it is human-admin-only at
every layer — `lib/portals.js` refuses any actor that is not a user, and the only
surface that changes a portal is the `/admin/portals` screen behind the SSO cookie
and CSRF. An agent cannot read a portal either: there is no `list_portals`. If a
future change does add one, this paragraph is the thing to delete, and every
consumer allowlist is the thing to update.

### Current catalog (45 tools)

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

A unit test asserts this block matches the registry exactly — here and in the
mirrored block in [API.md](API.md) — so neither copy can drift. It says nothing
about what a consumer deployed: Pages cannot read those manifests, which is why
this list is worth re-diffing rather than assuming.

`get_page_refresh` and `configure_page_refresh` are compatibility names; new
client prompts should call `prepare_dashboard_update` directly. The former
`run_page_refresh_now` and `pause_page_refresh` are no longer advertised — remove
them from a consumer list when you next touch it.

### The minimum for templates

A consumer that wants the cost win from [TEMPLATES.md](TEMPLATES.md) needs at
least:

```text
list_templates, get_template, create_page_from_template,
get_page_config, update_page_config
```

Plus, to author or revise a design from that client:

```text
create_upload_ticket, register_template_upload, list_template_revisions,
list_template_pages, rerender_page_from_template
```

Without `list_templates` and `get_template` an agent cannot discover that a
design already exists, so it writes the whole document again — which is the exact
cost the feature removes.
