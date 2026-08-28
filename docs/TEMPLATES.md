# Templates — one stored design, many pages

A template is a design Pages stores once. A page built from it carries only its
own **config** (what differs per campaign or client) and **data** (what changes
per refresh). The design itself is never re-transmitted and never re-stored.

## Why

The NWM campaign dashboard is ~62 KB of HTML in which the `CONFIG` object —
~1.4 KB — is everything that differs between campaigns. Before templates, the
second campaign cost the same as the first:

| | Before | With a template |
|---|---|---|
| Second page of a family | 62 KB of HTML: ~21k output tokens of base64, or a hand-edited copy of the file | ~1.4 KB of config |
| Reading what a design needs | the whole document back into context (~16–20k tokens) | `get_template` → two schemas, no HTML |
| A design fix across N pages | N re-uploads that drift | one revision, then a reviewed rerender per page |

## The contract

Four managed script blocks. `lib/page-data.js` is the source of truth for all of
it — templates reuse the same validation, ReDoS pattern screen, size ceilings,
script-terminator escaping, and hashing that the managed-data contract has always
used.

| Block id | `type` | Written by | When |
|---|---|---|---|
| `pages-config-schema` | `application/schema+json` | you, the template author | registration; immutable per revision |
| `pages-config` | `application/json` | Pages, from the `config` argument | `create_page_from_template`, `update_page_config` |
| `pages-data-schema` | `application/schema+json` | you, the template author | registration; immutable per revision |
| `pages-data` | `application/json` | Pages, as the standard envelope | `create_page_from_template`, `update_page_data` |

Both schemas must be **self-contained JSON Schema 2020-12** with an object root.
Local `$ref`s work; external ones are rejected.

A template is a **complete page**, not a fragment: it ships a real reference
config and an empty-state data envelope, and each must satisfy its own schema.
That is what lets registration validate a design instead of trusting it — and it
means the template renders on its own, showing its awaiting-first-ingest states.

```html
<script type="application/schema+json" id="pages-config-schema">{ … }</script>
<script type="application/json"        id="pages-config">{"campaign":"Reference","channel":"display"}</script>
<script type="application/schema+json" id="pages-data-schema">{ … }</script>
<script type="application/json"        id="pages-data">{"contract_version":1,"refreshed_at":"…","source_as_of":"…","data":{"rows":[]}}</script>
<script>
  const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);
  const DATA   = JSON.parse(document.getElementById('pages-data').textContent).data;
  /* the rest of the render layer is unchanged */
</script>
```

Rendering is **block-content substitution only**. There is no placeholder or
template language, deliberately: config and data are untrusted JSON, and the only
place they can ever land is inside a JSON script block that Pages escapes, so
they cannot become markup.

### A block you never write: `#pages-nav`

`pages-nav` is the sixth id in this family and the only one a template must not
declare. Pages **injects** it into `<head>` on a render that a partner's portal
authorised, listing the dashboards their one credential opens — and it is injected
rather than substituted because membership changes without anyone redeploying a
page, and there is deliberately no bulk re-render primitive.

So the id belongs to Pages: a design shipping its own copy has it removed at deploy
time, before the content hash, so a template can never present a second answer to
"which dashboards can this viewer open".

A template that wants a `Page ▾` menu **reads** the block, defensively — it is
absent on a staff view, a page-password view, and any page in no portal, and an
unguarded `JSON.parse` of a missing block takes the rest of the script down with
it. The full contract, including why the payload's `url` must be used as given and
why titles need `textContent`, is in
[AUTHORING.md](AUTHORING.md#giving-a-partner-a-menu-of-their-other-dashboards);
`templates/nwm-campaign-dashboard/template.html` is a working implementation.

One limit worth knowing before promising a client a menu: a template deployed
`render_mode: "raw"` never receives the block, because raw is served byte-for-byte.

### A fifth, optional block: example data for previews

The four blocks above have a consequence worth naming: because a template must
ship an **empty** `#pages-data` so that no page built from it inherits rows,
previewing a design shows a skeleton. That is close to useless when the point of
the library is deciding whether a design is the right one.

So a template may carry one more block:

| Block id | `type` | Contains |
|---|---|---|
| `pages-data-example` | `application/json` | a **bare data object** — not an envelope — satisfying the same `pages-data-schema` |

```html
<script type="application/json" id="pages-data-example">{"rows":[{"dealId":"9000001","date":"2026-06-01", … }]}</script>
```

It is validated against the data schema at registration, exactly like a real
refresh, because an example the schema would reject renders a preview no ingest
could reproduce. Pages then stores it on the revision (`sample_data`) and pours it
into `#pages-data` when it renders a preview, so `/admin/templates` shows the
design **populated**.

**It never reaches a page.** `materializeBlocks` deletes the whole element from
every materialization — page creation, config update, data update, re-render, and
the preview render itself. So:

- a page built from the template contains neither the block nor its rows;
- the previewed bytes are exactly the shape a page would have;
- `ships_empty` still refers to `#pages-data` and must still be true — example
  rows live in their own block, not smuggled into the envelope;
- `template_sha256` for a page is unchanged by the presence of an example block.

Keep it fictional. A template is shared: everyone who opens the library sees its
reference config and its example data, and `get_template` hands the reference
config to agents. Neither is the place for a real client's identity or numbers.

## Where templates come from

Two paths, and both end in the same `page_template_versions` row.

### 0. Promoted from a page you already have — the usual way

You built a dashboard, it is good, and someone wants the same thing for another
client. Nothing needs re-authoring:

```
create_template_from_page  slug="acme-q3"  template="campaign-dashboard"
                           empty_data={"rows":[]}
```

The published bytes are already on this server, so the promotion reads them in
place — nothing crosses the wire in either direction. It replaces the page's live
data with the empty state you name, optionally keeps a copy as preview-only example
rows (`example_from_current_data`, off by default because those are a real client's
numbers), and registers the result.

The only requirement is that the page already carries `#pages-config` — see
**[AUTHORING.md](AUTHORING.md)** → "Structure every page so it can be reused",
which is how every page should be built anyway. Pages derives the config schema, so
that costs no schema authoring. A page that hardcodes its identity is refused with
`page_not_template_managed` rather than promoted into a design whose every instance
names the same client.

Read `hardcoded_config_values` in the result. It lists config values that are
*also* written into the design — a `<title>`, a heading — which the next client's
page would still show. Fix them with `patch_page` and promote again.

### 1. Shipped with the deployment — `templates/`

The set this repo carries lives in [`templates/`](../templates/), one directory per
template:

```
templates/<template-name>/template.html      # required
templates/<template-name>/template.json      # optional: title, description, note, name
```

`pages update` registers them after the health check, so shipping a design fix is
a git push and a deploy. By hand:

```bash
pages template list                     # registered templates + pages using each
pages template sync                     # register everything under templates/
pages template register <file> [--name X] [--title T]
pages template show <name> [--revision N]
```

`register` takes **any** path, so a client bundle or a scratch checkout can supply
a template too — the file does not have to live in this repo. Sync is idempotent:
identical bytes dedupe to the same revision, so re-running is free, and a changed
file becomes revision N+1 while every deployed page stays pinned to the revision
it was built from. A template that fails validation is reported and skipped; it
does not fail a deploy that has already proven the service healthy.

Pages is an Elcano product, so a design carrying a client's identity belongs in
this repo. That is the opposite of fleet, which ships no client content by
construction.

### 2. Registered by an agent — MCP

The bytes go through the same out-of-band upload path a large page uses, so the
design never passes through model output:

```
create_upload_ticket      template="nwm-campaign-dashboard" total_bytes=… content_sha256=…
curl -fsS -X PUT --data-binary @nwm.template.html -H "Authorization: Bearer <ticket>" <upload_url>
register_template_upload  upload_id=… title="NWM Campaign Dashboard"
```

**Read the `preflight` field.** Errors in a template are inherited by every page
built from it, so fix them before creating pages. Registering identical bytes
again returns the same revision rather than inventing a new one.

An upload staged with `template` cannot be consumed by `deploy_page_upload`, and
a page upload cannot be registered as a template. The target is fixed when the
upload starts and checked under a row lock at commit.

## Retiring one

`delete_template` soft-deletes a template: it leaves the library and its **name
becomes reusable**, which is the fix for a mistyped name. Revisions are kept, and
pages already built from it keep serving — a page carries its own materialized
HTML and never reads the template at runtime.

It is refused when pages were built from it unless you pass `force`. That is
deliberate: confusing "I typed the name wrong" with "retire this design" is the
mistake worth preventing. Forcing it costs those pages their design provenance —
they drop out of `list_template_pages` and can no longer be re-rendered from it.

The library offers the same thing as **Retire** on a template's detail panel,
with the page count in the confirmation.

## Building a page

```
get_template               template="nwm-campaign-dashboard"      → schemas + reference config, NO html
create_page_from_template  template="nwm-campaign-dashboard" slug="contoso-allergex-acct00156"
                           config={ … } title="Contoso — Allergex"
```

- **`config` must be complete.** It is validated against the config schema and is
  never merged with the reference config. The reference config is a starting
  point to *read*, not a default to inherit — otherwise one client's identity
  quietly ends up on another client's page.
- **Omit `data`** to deploy the design's empty state and fill it later with
  `update_page_data`. A page created without data claims no source coverage
  (epoch), so the first ingest can never be rejected as a regression however old
  the data it covers.
- Supplying `data` requires `source_as_of`.
- Creating onto an existing slug fails with `page_exists` and points at the tool
  that does what you actually meant. An identical retry is a safe no-op in
  exactly the two states a died-mid-turn retry can leave behind: that build is
  still what the page serves, or the page has published nothing at all (a gated
  build waiting for a human, or a create that died before publishing). Once the
  page has moved on — a data refresh, a config edit, any deploy — replaying the
  original create is `page_exists` too: it would otherwise dedupe onto the first
  version and drag the live pointer *backward*, reverting the client's dashboard
  to its empty state. To return a page to an earlier version deliberately, use
  `rollback_page`, which says so in the audit log.

## Changing one page

| You want to change | Use | What it cannot touch |
|---|---|---|
| the numbers | `update_page_data` | config, layout, schema |
| this campaign's settings | `update_page_config` | data — the block is left byte-for-byte alone |
| the shared design | a new template revision + `rerender_page_from_template` | any page you do not name |

`update_page_config` replaces rather than merges, requires `expected_version`
from `get_page_config`, and carries source coverage across unchanged — renaming a
campaign or retargeting a KPI cannot disturb, restate, or roll back its numbers.

Do **not** deploy or patch HTML into a template-built page. It would fork the
page off its design: the next revision silently stops reaching it and nothing
reports the drift. `prepare_dashboard_update` returns `mode: managed_template`
for these pages and routes each kind of request to the tool that owns it.

## Propagating a design fix

Registering a revision changes nothing that is serving. Pages never re-renders a
page on its own — a bad revision would otherwise be a fleet-wide incident instead
of one page to fix.

```
register_template_upload        → revision 2
list_template_pages             → who is on revision 1 (read-only)
rerender_page_from_template  slug=…        → publish defaults to FALSE: a canary
   … look at it …
publish_page                 slug=… version_id=…
```

A rerender keeps the page's own config and data, taken from its published HTML —
not from a stored copy that could have drifted from what is actually serving —
and re-validates both against the target revision's schemas. A revision that
tightened its contract therefore fails loudly, before anything is written,
instead of producing a page its own schema rejects.

One page per call. There is no bulk rerender.

## Identity and hashes

- `config_sha256` / `data_sha256` — semantic hashes of the two payloads (key
  order independent).
- `template_sha256` — the page's bytes with **only the data block elided**.
  Config contents stay inside it on purpose: to a data update, config is part of
  the immutable template it is pouring rows into. If config were elided too, an
  unchanged refresh could dedupe onto a version from before a config edit and
  silently republish the old config.
- `page_versions.template_version_id` + `config_sha256` record which revision
  produced a version, and survive refreshes and config edits alike.

## Checking a design before registering it

`validate_template` runs the whole contract check and writes nothing. Pass
`upload_id` for bytes you already staged through a ticket — they are verified and
read but **not consumed**, so the same `upload_id` registers afterwards and you
never re-send the file:

```
create_upload_ticket   template="acme-overview" total_bytes=… content_sha256=…
  curl -fsS -X PUT --data-binary @acme-overview.html -H "Authorization: Bearer <ticket>" <upload_url>
validate_template      upload_id=…          → the report, nothing written
register_template_upload  upload_id=…       → revision 1
```

It reports a broken design rather than throwing, and still runs preflight when the
contract fails, so a missing block and a broken chart control surface in one pass.
`template_urls` then mints a sandboxed preview link you can hand to a human.

## Error codes

| Code | Meaning |
|---|---|
| `template_not_found` / `template_revision_not_found` | No such template or revision. |
| `bad_template_name` | Names are flat and url-safe (`a-z 0-9 - _`), no slashes. |
| `template_contract_invalid` | The HTML is not a template: a managed pair is missing entirely. |
| `data_contract_invalid` | Blocks or payloads are malformed, duplicated, mistyped, or fail their own schema. |
| `config_validation_failed` | The proposed config fails the config schema or its size limit. |
| `page_not_template_managed` | The page has no `pages-config` block; it was not built from a template. |
| `page_exists` | `create_page_from_template` will not silently replace an existing page. |
| `template_revision_unchanged` | The page is already on that revision. |
| `upload_target_required` | Supply exactly one of `slug` or `template`. |
| `page_upload_target_mismatch` | A template upload cannot be deployed as a page, or vice versa. |
| `template_source_ambiguous` | `validate_template` takes exactly one of `upload_id` or `html`. |
| `template_has_pages` | `delete_template` refuses a design pages were built from; pass `force`. |
| `page_upload_already_committed` | Nothing left to validate — a committed upload's chunks are gone. |

## Limits

`PAGES_DATA_CONFIG_MAX_BYTES` (256 KiB) bounds a config;
`PAGES_DATA_SCHEMA_MAX_BYTES` (256 KiB) each schema; `PAGES_DATA_MAX_BYTES`
(1 MiB) the data payload and, separately, an example dataset;
`PAGES_DATA_TEMPLATE_MAX_BYTES` (2 MiB) the stored template and the materialized
page. An example dataset counts toward the stored template but not toward any page
built from it, since it is deleted on materialization.
