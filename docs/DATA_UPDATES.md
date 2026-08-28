# Dashboard updates through Pages MCP

> **A note on names.** *Chat*, *Cutlass*, *MOC* and *Fleet* are ElcanoTek's own
> internal agent tools and orchestration services — the original consumers of
> this interface. None of them is part of this repository and Pages depends on
> none of them; any MCP client works identically. Where this page says a name is
> "already registered" or "already allowlisted", read it as a note about
> ElcanoTek's deployment, not a requirement of Pages.

Pages owns the safe update contract, versioning, validation, publishing, and
audit trail. It does **not** own an agent runtime or recurring scheduler. Pages
does not dispatch to MOC and needs no MOC, OpenRouter, Fleet, Chat, or Cutlass
credentials for dashboard updates.

Any general-purpose MCP client can ask Pages for the exact workflow, then either
execute it once or give the text to a user for their scheduler of choice. Source
API and mailbox access stays in that client’s existing MCP configuration; secret
values never enter Pages or the generated prompt.

## What a teammate does

For a one-time change, say for example:

> Update `acme/daily-performance` dashboard with yesterday’s complete Google Ads
> spend, impressions, clicks, and conversions.

The client calls:

```json
{
  "name": "prepare_dashboard_update",
  "arguments": {
    "slug": "acme/daily-performance",
    "instructions": "Use yesterday's complete Google Ads spend, impressions, clicks, and conversions.",
    "update_type": "data"
  }
}
```

It then follows the returned `prompt` in the current conversation. The tool
itself is read-only: it creates no page, version, task, token, or schedule.

For an unattended recurring update, ask for a reusable prompt. The client calls
the same tool with `recurring:true` **and `sources`** and shows `prompt` to the
user verbatim. The user installs that text in whatever scheduler already has the
relevant source MCPs and Pages MCP. Each scheduled invocation is one bounded
update run. Pages never claims that the schedule is configured and does not
execute a recurring prompt while preparing it.

Because Pages does not dispatch, the response also carries
`execution_requirements` — the MCP servers and tools the run needs, whether it
needs network, and that it needs a model assigned. A scheduler can check that
before accepting the task instead of discovering it at dispatch. (Five real
autoupdate tasks were accepted as opaque prompt blobs and dead-lettered on their
first run with no model configured, having executed nothing.)

No per-dashboard server setup is required. In particular, do not create a Pages
runner token or add `PAGES_MOC_*` / `PAGES_REFRESH_*` environment variables.

## `prepare_dashboard_update`

Arguments:

- `slug` — exact existing dashboard slug. Missing pages fail; Pages never makes
  a replacement or companion data page.
- `instructions` — the user’s complete source/data/design request in plain
  language. Name configured MCP accounts or credential locations, never secret
  values.
- `recurring` — default `false`. When true, only a reusable data workflow is
  returned for a user-owned scheduler.
- `update_type` — `data`, `layout`, or `auto` (default). Use `data` for values
  only, `layout` for design/schema/JavaScript, and `auto` only when classification
  is genuinely unclear.
- `publish` — whether the eventual update should publish; defaults to `true`.
- `sources` — exact source bindings. **Required when `recurring` is true**;
  optional (but recommended) otherwise. Naming an input in prose ("the newest
  Amazon DSP delivery data") does not tell the executing agent which connector
  serves it, and that agent may have the server unloaded, gated off, or absent.
  A recurring prompt is worse still: it runs unattended weeks later, with no
  channel back to the conversation that wrote it, and it writes to a live
  client-visible dashboard — so Pages refuses to build one from prose alone
  (`update_sources_required`). Supply one entry per input and the prompt forbids
  substituting any other source:

  ```json
  {
    "source_id": "amazon_dsp",
    "mcp_server": "s3_feeds",
    "account": "nwm",
    "required_tools": ["feeds_run_query"],
    "retrieval_instructions": "Delivery rows for the two included deal ids, complete days only."
  }
  ```

  `path` and `partition` describe *where* the data sits and *what shape* it has:

  ```json
  {
    "source_id": "amazon_dsp",
    "mcp_server": "fastio_helpers",
    "path": "NWM_keel/Meridian/daily",
    "partition": { "by": "date", "format": "YYYY-MM-DD", "since": "source_as_of" },
    "required_tools": ["list_partitions"]
  }
  ```

  A declared `partition` changes the prompt's retrieval instruction from "find
  the file" to "enumerate EVERY partition in range and aggregate them; never take
  only the newest". That distinction is not cosmetic: against a real workspace an
  exact-filename search for one daily report returns ~100 hits, one per day, and
  the default newest-wins pick publishes a single day of a multi-week dataset.

  Names only — the same credential screen as `instructions` applies, so a secret
  value can never arrive this way. The vocabulary matches the `workflow.sources`
  shape client bundles already author, and the legacy `configure_page_refresh`
  compatibility path lifts bindings out of `workflow.sources` automatically when
  they carry both `source_id` and `mcp_server`.

Every mode requires the executing agent to establish source access *before*
retrieving: list the tools actually available, bind each input to a specific
server and tool, load or enable a server if its client supports on-demand
loading, and **stop without writing to Pages** when a required source is
unreachable — never substituting another source, re-using a stale artifact, or
carrying prior totals forward. The final report must name the server and tool
actually used per source, so a wrong binding is visible in the run output rather
than hidden inside a plausible-looking dashboard.

The result pins the current live version and, when available, managed schema:

- `mode: managed_data` — use `get_page_data` and `update_page_data`; layout and
  schema bytes cannot change.
- `mode: full_page` — read the existing HTML and update that same slug, using
  staged upload tools for files or content over 20,000 UTF-8 bytes. Deploying and
  publishing are separated: the prompt deploys with `publish=false`, reads that
  version back with `get_version` to confirm nothing came out blank, truncated,
  or duplicated, and only then calls `publish_page`. A failed check leaves the
  previous version serving, so a broken render never reaches the client.
- `mode: managed_template` — the dashboard was built from a stored template
  ([TEMPLATES.md](TEMPLATES.md)). The prompt routes a settings change to
  `update_page_config` and a design change to a new template revision plus a
  reviewed per-page rerender. Deploying HTML into such a page is forbidden: it
  forks the page off its shared design, so the next fix silently stops reaching
  it and nothing reports the drift.
- `mode: adaptive` — the prompt first classifies a mixed/unclear one-time request
  and then follows exactly one of the two safe paths.
- `mode: migration_required` — the live dashboard lacks the managed-data blocks.
  Run the returned same-slug migration once, then call
  `prepare_dashboard_update` again for the actual data update.

The result also includes `prompt_sha256`, `schema_sha256`, `sources` (the parsed
bindings, or `null`), `live_version_id`, `page_is_live`, and an explicit
`next_step`. Prompt preparation rejects credential-shaped values and is bounded
in size.

## Managed-data contract

A dashboard opts in to data-only updates through exactly one schema block and
one data block:

```html
<script id="pages-data-schema" type="application/schema+json">
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["spend", "impressions"],
  "properties": {
    "spend": {"type": "number", "minimum": 0},
    "impressions": {"type": "integer", "minimum": 0}
  }
}
</script>
<script id="pages-data" type="application/json">
{"contract_version":1,"refreshed_at":"2026-07-17T09:05:00Z","source_as_of":"2026-07-17T08:00:00Z","data":{"spend":125.5,"impressions":45000}}
</script>
```

The schema validates only `data`. It must use JSON Schema 2020-12, have an
object root, and be self-contained. Local fragment references are supported;
external references are rejected. The envelope contains exactly:

- `contract_version`: currently `1`;
- `refreshed_at`: generated by Pages when it materializes a version;
- `source_as_of`: latest source coverage represented by the complete payload,
  as timezone-qualified RFC3339; and
- `data`: the complete schema-valid object.

Pages replaces only the existing data-block contents. Layout, JavaScript,
schema, theme, password, and access settings remain byte-for-byte outside that
block. JSON is escaped so values cannot terminate the script block. Default
limits are 256 KiB for schema, 1 MiB for data, and 2 MiB for materialized HTML;
deployments can lower them with `PAGES_DATA_SCHEMA_MAX_BYTES`,
`PAGES_DATA_MAX_BYTES`, and `PAGES_DATA_TEMPLATE_MAX_BYTES`.

## Safe managed update execution

`get_page_data({slug})` returns the published schema/envelope, semantic hashes,
URLs, and truthful live-state fields. A generated managed prompt requires the
caller to:

1. read the exact slug and verify the pinned schema hash;
2. retrieve every required source read-only and establish identity, coverage,
   freshness, completeness, row counts, and reconciliation evidence;
3. stop without writing if anything is missing, stale, partial, ambiguous, or
   inconsistent;
4. build one complete schema-valid data object; and
5. call `update_page_data` with the read `live_version_id` as
   `expected_version`, the latest represented `source_as_of`, and the requested
   publish mode.

`update_page_data` locks the page, rereads the published template, rejects
source regression/future coverage, creates an immutable version through the
normal approval state machine, and deduplicates exact retries. On
`stale_version` or an ambiguous transport result, reread once, compare hashes,
and retry at most once only when the intended coverage is not already present.

Use `publish:false` for a canary. Approval-gated updates remain pending for a
human; a disabled page cannot be published through the takedown.

### Reconciling what you actually wrote

Step 3 above says "stop without writing if anything is missing" — but for a long
time nothing helped a caller know it was in that state. The schema validates
**shape**: a payload missing three weeks of days, or a whole deal, satisfies it
exactly as well as a correct one. A Vandelay campaign dashboard served understated SSP
totals and a DSP series that silently began nine days late, and both payloads
were schema-perfect (issue #102).

So both `update_page_data` and `get_page_data` return **`data_profile`** — what
the payload contains, at paths you can name:

```json
{ "arrays": { "rows": { "count": 101, "fields": {
      "date":   { "kind": "date",   "min": "2026-07-06", "max": "2026-08-05", "distinct": 31 },
      "dealId": { "kind": "key",    "distinct": 4, "values": { "allergy": 30, "1442375": 11 } },
      "spend":  { "kind": "number", "sum": 25743.281, "min": 118.542, "max": 502.61 } } } },
  "scalars": { "dataThrough": "2026-08-05" } }
```

Field kinds come from the **values**, not the schema — a column the schema calls
a string is reported as a date when it holds dates, because that is what a human
reconciles against a source export. String columns are enumerated as dimensions
until they hold more than 50 distinct values, at which point the field is `text`
with `distinct_overflow` and no `values`.

`values` is a **bounded summary**: the most frequent few, with `values_omitted`
counting the rest (a value longer than 120 characters is counted there rather
than echoed). The membership comparison behind `data_warnings` is *not* limited
to that summary — it runs over every value the field holds, so a deal that
merely became less frequent is not reported as deleted and a low-volume deal
that really disappeared is still named. Only **string** values are enumerated:
a dimension sent as JSON numbers is profiled as a number column and is not
diffed for membership.

`data_warnings` is itself capped, and says so with a `warnings_truncated` entry
naming how many it dropped rather than shortening the list silently.

Your own numbers come back unchanged. Pages renders its own BIGINT identifiers
as decimal strings (`live_version_id: "42"`), but never inside `data`, `config`,
`reference_config`, `schema`, `config_schema` or `data_profile` — those are your
document, echoed verbatim, so `get_page_data` → edit → `update_page_data`
round-trips through an `{"type":"integer"}` schema without Pages having changed
a value underneath you.

`update_page_data` also returns **`data_warnings`**, computed against the payload
it is replacing (already in hand, so this costs nothing):

| Code | What it means |
|------|---------------|
| `coverage_start_regressed` | The new date window starts **later** than the published one. A trailing-window source export reads exactly like this. |
| `coverage_end_regressed` | The new window ends **earlier** than the published one. |
| `row_count_dropped` | Fewer rows than before. On a cumulative source, that is missing data. |
| `dimension_values_missing` | A key value that had data is gone. Rows filtered to a configured subset read exactly like this. |
| `coverage_unverified` | There was **no baseline** to compare against and no `expect` naming this array, so its window and totals are unchecked by anything. |
| `data_unchanged` | The payload is **byte-identical** to the one already published. No metric moved. |
| `coverage_did_not_advance` | Same first day, same last day, same row count, different figures: the source restated days already covered and added none. |

These are **warnings, not errors** — narrowing is sometimes correct, e.g. a new
flight — so the write lands and the reason appears in `next_step` as well.

`coverage_unverified` exists because comparison has a blind spot, and the Vandelay
incident landed squarely in it. Reconstructed from its transcript, the DSP window
was wrong in the **first** payload:

```
create_page_from_template  rows= 31  2026-07-30 → 2026-08-05
create_page_from_template  rows= 31  2026-07-30 → 2026-08-05
create_page_from_template  rows= 31  2026-07-30 → 2026-08-05
update_page_data           rows= 21  2026-07-30 → 2026-08-05   ← shipped
```

Against a request for "7/6 and on". There was never a wider payload to regress
from, so a diff finds nothing — the one write that put wrong numbers in front of a
client is precisely the write a diff cannot see. So a first real payload over an
empty one is reported as unverified instead of silently accepted, and naming that
array in `expect` withdraws the warning because `expect` enforces it outright.

### "It said it updated, but there's no new data"

The other direction, and a separate incident. A dashboard refresh reported
**"Version 205 published and live"** with **"Data Warnings: None"** and the same
Aug 3 – Aug 5 coverage as the version before it. The reply came back: *"This just
ran but didn't update yesterday Aug 6th data?"*

Nothing was broken. The managed-data dedupe key is `(data_sha256,
data_template_sha256, source_as_of, render_mode)`, so **a newer `source_as_of`
over byte-identical data is deliberately not a dedupe** — that is how a
re-verified source gets recorded. But `deduped:false`, `version_is_live:true` and
a fresh version id all read like new numbers landed, and the warnings only
answered *"what got smaller"*.

`data_unchanged` and `coverage_did_not_advance` answer *"did this add anything"*.
They are deliberately separate, because they mean different things to whoever
asks: the first is "the same source file", the second is "the source was updated
and only restated days you already had".

Three rules keep them honest:

- **Anything that grew is not "added nothing"** — an earlier start, a later end, or
  more rows. A refresh that restores three weeks of history at the front has
  plainly added data even though its last day is unchanged.
- **Anything that shrank is a loss finding instead**, which is louder and more
  specific. `coverage_did_not_advance` is suppressed for a field that also
  regressed, and `next_step` only leads with "added nothing" when there is
  nothing worse to report.
- **A dedupe suppresses both.** It already says "the exact data and source
  coverage were already live" in its own fields, and `data_unchanged`'s "a version
  was still created" would be untrue there.

When one of these fires, `next_step` says **"THIS REFRESH ADDED NO NEW DATA"** and
tells the caller not to describe it as a data update — the mistake in that
incident was not a wrong number, it was announcing a refresh that had not
happened.

### `expect`: make it a contract instead of a promise

Stronger, and preferred when the source is in hand. Compute the same numbers from
the source in the pass that builds the payload, and let the server enforce them:

```
update_page_data  slug="vandelay-acct00142"  data={…}  source_as_of="2026-08-05"
                  expected_version=<live_version_id>
                  expect={ "row_count":  {"rows": 101},
                           "totals":     {"rows.spend": 24541.60},
                           "date_range": {"rows.date": ["2026-07-06", "2026-08-05"]} }
```

Verified against the server's profile **before anything is written**: a mismatch
fails with `data_reconciliation_failed` listing expected vs actual for every
check at once, and the live page does not move. `totals` allows 0.01% relative
slack by default (float summation order), overridable via `tolerance`; row counts
and date ranges are exact. A path that matches nothing is a mismatch, not a
silent pass, so a typo in `expect` cannot read as "reconciled".

## Optional least-privilege identity

A scheduler may use a manually provisioned `data_update` token instead of a
broad authoring token:

```bash
pages token add client-daily data_update acme/daily-performance
pages token list
pages token revoke <id>
```

This is optional and operator-managed. Pages never creates a token or grant as a
side effect of prompt preparation. A `data_update` token sees only
`get_page_data` and `update_page_data`, may access only its explicit slugs, is
denied from REST and unrelated MCP tools, and is recorded by token ID on writes.
The broad MCP token already used by Chat can prepare and execute user-directed
updates without additional Pages server configuration.

Grants bind to the **page that holds the slug**, not the slug text alone: a
grant for a not-yet-existing slug binds to the first live page that takes it,
and once bound it never re-binds. If a granted page is deleted and the slug is
later reused by a *new* page, the old grant stays inert (`slug_not_allowed`)
until an operator re-grants; a delete→restore of the *same* page keeps working.

## Legacy schedule retirement

Migration `010_retire_moc_refresh_dispatch.sql` pauses all old Pages-owned
refresh definitions, cancels every undispatched run, and removes the old
runner’s exact-slug grants. Historical definitions and runs remain in the
database for auditability; no live code reads or dispatches them.

## Bounded error codes

| Code | Meaning / action |
|------|------------------|
| `page_not_found` | The exact slug does not exist; correct it instead of creating an alternative. |
| `update_page_not_published` | The dashboard has no published base version to pin. |
| `recurring_layout_forbidden` | Recurring prompts must be managed-data updates, not unattended layout rewrites. |
| `update_credentials_forbidden` | Instructions contained a credential-shaped value; describe its configured location instead. |
| `page_not_data_managed` | The published page lacks a valid managed-data contract; use the returned migration prompt. |
| `data_contract_invalid` | Existing blocks/schema/envelope are malformed, duplicated, unsafe, or inconsistent. |
| `data_validation_failed` | Proposed `data` fails schema or size validation. |
| `source_as_of_invalid` / `source_in_future` / `source_regression` | Correct source coverage before writing. |
| `expected_version_required` / `stale_version` | Supply the read live version or reconcile once before a bounded retry. |
| `data_reconciliation_failed` | The payload disagrees with the `expect` you supplied; `details.mismatches` lists expected vs actual per check. Nothing was written — fix the aggregation, do not widen the tolerance to make it pass. |
| `slug_not_allowed` / `tool_not_allowed` | The scoped token lacks this exact slug/tool; do not broaden authority in a prompt. |
| `page_not_template_managed` | The page has no `pages-config` block, so it was not built from a template. See [TEMPLATES.md](TEMPLATES.md) for the template error codes. |
