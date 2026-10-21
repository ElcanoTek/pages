# Dashboard updates through Pages MCP

> Any compatible MCP client can prepare and execute these workflows. Pages
> depends on no particular agent runtime, scheduler or host; where this page
> mentions host features (audit declarations, tool aliases, file references),
> they apply only when your host provides them.

Pages owns the safe update contract, versioning, validation, publishing, and
audit trail. It does **not** own an agent runtime or recurring scheduler. Pages
does not dispatch work to any agent runtime and holds no model or source
credentials; those belong to the host that executes the workflow.

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
before accepting the task instead of discovering it at dispatch: a task
accepted without them can fail before any tool runs.

For `managed_data` it also states three things a scheduler can enforce without
asking a model:

- `roster: "required_tools_only"`: offer the run only `required_tools`. That
  list holds every tool a branch may call and nothing else. A refresh needs
  about eight Pages tools, and each step used to re-send the other thirty-odd
  tool schemas. Only a **recurring** prompt whose every source binding names
  its `required_tools` carries it: a scheduler honouring it registers nothing
  from a server none of whose tools is listed, so an unbound or server-only
  source would leave the run unable to read its data. One-time prompts, the
  managed half embedded in an adaptive prompt, and prompts prepared through the
  legacy `configure_page_refresh` workflow alias (whose lifted bindings may be a
  subset of the workflow it serializes) never carry it.
- `completion.any_succeeded`: the run is complete when one of these tools
  succeeded, which means a committed version or a recorded refresh check. Both
  are visible in tool records, so no model has to judge whether a correct
  `source_not_updated` or `blocked` run "finished". Recurring prompts only:
  a one-time prompt's blocked branch records no refresh check, so it carries
  no completion predicate.
- `completion.blocked_when`: `{"tool": "mcp_pages_record_refresh_check",
  "argument": "outcome", "in": ["blocked", "failed", "source_unreachable"],
  "detail_argument": "detail"}`.
  A run that completed through the predicate, whose last successful refresh
  check had one of those outcomes and which committed no version, finished
  **without publishing**. A scheduler that knows the key can show that run as
  blocked instead of as a plain success, with the check's `detail` as the
  reason, so a page whose refresh keeps blocking does not look refreshed.
- `serialization_key: "pages:<slug>"`: runs that share it must not overlap.
  Give every schedule for the page the same key. Two schedules refreshing one
  slug otherwise race to duplicate versions and `stale_version`.

Both commit transports are always in `required_tools` (and, on a recurring
prompt, in `completion.any_succeeded`): `mcp_pages_update_page_data` (inline) and
`mcp_pages_update_page_data_upload` with `start_page_upload` and
`append_page_upload`. Pages does not pin one at preparation. A recurring payload
grows as history accumulates, so a transport chosen from the size on the day the
prompt was prepared would, once a scheduler narrows the roster, lock a page that
later passes 20,000 bytes out of the upload tools. The run applies the 20,000-byte
rule to the file it built, on every run, and declares that one commit tool before
the gated mutation. A recurring prompt says so on a `ROSTER:` line, and tells the
run to verify from the commit response, `get_page_data` and `preflight_page` (the
check step 12 requires for a live publication) and to call no Pages tool outside
`required_tools`. A scheduler that can treat the two commit tools as one audited action (a tool
alias, where the host supports one) accepts either against one declaration.
`create_upload_ticket` is deliberately not listed: a client with direct file HTTP
may still use it, but a scheduler that does not expose it would refuse the task.

Each binding's `mcp_server` and `required_tools` are copied into the block
verbatim, so each must be one name matching `^[a-zA-Z0-9_.-]{1,200}$`; preparation
refuses anything else (for example `"file_store + file_helpers"`) with
`update_sources_invalid` rather than hand a scheduler a block it will reject. Bind
each server as its own source. The legacy `configure_page_refresh` alias never
fails on this: a workflow naming such a server lifts no bindings at all, and
every source is left to the serialized workflow. `serialization_key` is only a value to copy:
nothing enforces it until the installer sets it on the scheduled task.

For the largest data page:

```json
{
  "mcp_servers": ["file_store", "pages"],
  "required_tools": [
    "download_url", "mcp_file_store_download", "mcp_file_store_resolve_path",
    "mcp_pages_append_page_upload", "mcp_pages_get_page_config", "mcp_pages_get_page_data",
    "mcp_pages_preflight_page", "mcp_pages_record_refresh_check",
    "mcp_pages_start_page_upload", "mcp_pages_update_page_data",
    "mcp_pages_update_page_data_upload"
  ],
  "roster": "required_tools_only",
  "completion": {
    "any_succeeded": ["mcp_pages_record_refresh_check", "mcp_pages_update_page_data", "mcp_pages_update_page_data_upload"],
    "blocked_when": { "tool": "mcp_pages_record_refresh_check", "argument": "outcome", "in": ["blocked", "failed", "source_unreachable"], "detail_argument": "detail" }
  },
  "serialization_key": "pages:northwind/overview",
  "network": false,
  "model_required": true,
  "mode": "managed_data"
}
```

These keys are advisory and forward compatible. A scheduler that does not know
one ignores it; Pages still enforces every write itself.

Recurring managed-data prompts also embed that same JSON after an
`EXECUTION REQUIREMENTS (JSON):` line, so copying the prompt retains the handoff.
An executor can reject missing network/tools before running the model. This is
an opt-in consumer contract, not a Pages scheduling API or permission grant.
Where a run uploads files over HTTPS, the executing sandbox must allow that
egress; working MCP downloads alone do not establish shell connectivity. A scheduler
must select the bound servers, helper servers supplying required tools, and the
right connected accounts. Source authentication and completeness are verified
again during execution.

Each recurring run reads the current published schema, hashes, version and
configured registries. The schema hash returned by prompt preparation is
informational; it is no longer an expiry condition embedded in recurring text.
One-time managed updates still pin the generated schema hash. A scheduled run
may follow a later compatible chat edit, but must block on changed grain,
missing fields or ambiguous source/KPI/deal mappings. It never migrates layout,
schema or configuration. Hashes must remain unchanged **during the run**, and
`expected_version` protects the write against concurrent edits.

Before selecting a branch, compare the full source identifier set with the
declared scope and any configured mapping registries. Bindings must state the intended source scope explicitly:
CONFIG supplies mappings, not permission to discard unconfigured source rows.
Report excluded identifiers, row counts and the scope rule. Keep every in-scope
zero-delivery row; block ambiguous scope instead of copying prior exclusions.

A recurring prompt blocks only on a **hard stop**: a required source that cannot
be retrieved or authenticated, fails its hash or identity check, or cannot be
parsed; a genuinely ambiguous mapping (an identifier that could belong to more
than one configured record, or unclear account/campaign identity); or a schema or
grain the payload cannot satisfy without a schema change. Every other gap
follows the prompt's **GAP RULES**: publish what the sources verifiably report,
name each gap as a Quality Flag (in the final report and the update's `note`),
and never fill a gap with invented values:

- a source whose newest date trails the others publishes through its own cutoff;
- an unavailable companion report or cross-check is flagged, not a blocker, when
  every bound source file was retrieved;
- records missing on the newest date are absent, not zero, and are listed;
- a field blank on every in-scope row of a source follows the convention the live
  payload already uses for it (for example `0` on every existing row), or `null`
  where the schema allows it, and is flagged as not reported;
- an in-scope identifier CONFIG does not map is kept where the schema has an
  unmapped representation, otherwise reported with its row count and totals for a
  CONFIG decision — never dropped silently, never given an invented mapping;
- source values that differ from the live payload are corrections and publish.

A USER REQUEST block condition still applies when it names the specific report,
field or identifier. Separating hard stops from reportable gaps keeps a
refresh from blocking on a gap while its sources hold valid new data.

An unchanged reporting date or aggregate total does not prove unchanged history.
Compare complete in-scope records and represented metrics/dimensions with the live
payload, or verify unchanged immutable source revisions/hashes against provenance
tied to that version and the same scope/transformation. A prior refresh-check
claim is not baseline provenance. This catches historical corrections even when
the latest date and totals are unchanged.

A run selects exactly one branch:

- **source_not_updated:** freshly retrieved, verified sources add no missing
  coverage, historical correction or requested correction. Record the check and finish successfully;
  no upload, new version or post-publication preflight is required.
- **blocked:** source access, completeness, mapping or transport prevents a safe
  update. Record the exact blocker and preserve the live page. Never call this
  source_not_updated when a required source was not retrieved.
- **update:** rebuild and reconcile the complete payload, upload its exact bytes,
  commit with optimistic concurrency, then verify the returned state/profile
  and the live browser preflight.

The host's audit protocol applies when it has one. A completion audit with no
remaining mutations declares that in the host's documented form; it must not
promise publication for the other branches. Upload transport is
bounded: one transient retry, no policy bypass, no model-emitted large base64
payload, no repeated upload cancellation/restart. A missing transport is a
blocker, not permission to sample the data. Existing saved prompts are not
rewritten automatically: regenerate them after upgrading the producer and
executor. Customer data-grain migrations are separate work.

No per-dashboard server setup is required: preparing a prompt needs no runner
identity and no server-side scheduler configuration.

## `prepare_dashboard_update`

Arguments:

- `slug` — exact existing dashboard slug. Missing pages fail; Pages never makes
  a replacement or companion data page.
- `instructions` — the user’s complete source/data/design request in plain
  language. Name configured MCP accounts or credential locations, never secret
  values.
- `recurring` — default `false`. When true, only a reusable data workflow is
  returned: the verbatim task prompt of a recurring job. It describes one run
  and says nothing about who invokes it or when; the job carries the cadence.
  Sentences of `instructions` that are about scheduling or prompt creation
  ("this is prompt creation only; do not install a schedule", "run when invoked
  by the user's scheduler", a leading "create a reusable daily unattended …")
  are dropped before the request is quoted, and returned verbatim as
  `instructions_removed`. A source that is merely named "scheduled" (an "OpenX
  Scheduled Report") is kept.
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
    "mcp_server": "file_store",
    "path": "northwind/contoso/daily",
    "partition": { "by": "date", "format": "YYYY-MM-DD", "since": "source_as_of" },
    "required_tools": ["list_partitions"]
  }
  ```

  A declared `partition` changes the prompt's retrieval instruction from "find
  the file" to "enumerate EVERY partition in range and aggregate them; never take
  only the newest". That distinction is not cosmetic: a partitioned source holds
  many files matching the same name, one per day, and picking only the newest
  publishes a single day of a multi-week dataset.

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

The result reports the current live version and managed schema. One-time
workflows pin their baseline; recurring managed workflows read it at execution:

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
  it; the only signal is `drifted` in `list_template_pages`.
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
limits are 1 MiB for schema, 8 MiB for data, and 16 MiB for materialized HTML;
deployments can lower them with `PAGES_DATA_SCHEMA_MAX_BYTES`,
`PAGES_DATA_MAX_BYTES`, and `PAGES_DATA_TEMPLATE_MAX_BYTES`. Reads and writes of
managed data report `data_usage` — `payload_bytes`, `envelope_bytes`,
`limit_bytes` and `headroom_bytes` — so a run sees how close a page is to the
data limit before its next refresh.

## Safe managed update execution

`get_page_data` reads the contract in three sizes, chosen with `detail`:

| `detail` | Returns | Use |
| --- | --- | --- |
| `summary` | brief page flags, `live_version_id`, `data_sha256`/`schema_sha256`/`template_sha256`, envelope stamps, `freshness`, and `coverage_profile` (per-array `count`, date-field `min`/`max`/`distinct`, date-stamped scalars). About 1–2 KB for a typical payload; the coverage profile is capped at 3 KB and marked `coverage_truncated: true` when cut | the first read of every run; enough to stop early on a hard stop, but not proof that history is unchanged (compare complete records, or verify immutable source provenance, before choosing `source_not_updated`) |
| `export` | the same identity fields plus `schema_url`, `data_url` and `envelope_url` for the live version, valid for 10 minutes | fetch the schema and rows once into workspace files with a host-side URL download (a downloader tool your host provides), so they never enter the model's context |
| `full` (default) | the published schema/envelope, semantic hashes, full `data_profile`, URLs and truthful live-state fields, exactly as before `detail` existed | clients that cannot download URLs; one read per run |

The exported schema and data files are the canonical JSON the hashes are
computed over, so `sha256(schema.json)` equals `schema_sha256` and
`sha256(data.json)` equals `data_sha256`. An export URL serves only the version
that was live when it was minted, and it stops working when its minting token is
revoked or loses the page grant. See [SECURITY.md](SECURITY.md).

A full read with `include_data:false` still returns the complete contract,
hashes, profile, live state and coverage, with `envelope.data` omitted and
`data_omitted:true`. Never treat an omitted payload as an empty dataset.

A generated managed prompt requires the caller to:

1. read the exact slug with `detail:'summary'` and establish this run's
   schema/hash baseline (also verify the generated schema pin for a one-time
   update), then fetch the schema and live data **once** into workspace files,
   by `detail:'export'` (`schema_url` and `data_url`, each file's exact bytes
   hashed against `schema_sha256`/`data_sha256`) where the client can download
   URLs and by one `detail:'full'` read otherwise. Validation, historical overlap and
   reconciliation read those files. The run does not call `get_page_data` or
   `get_page_config` again, whether to re-verify or after a context
   compaction, unless a write returns `stale_version`;
2. retrieve every required source read-only and establish identity, coverage,
   freshness, completeness, row counts, and reconciliation evidence;
3. select the no-update or blocked branch when appropriate;
4. build one complete schema-valid data object, writing integral counts as JSON
   integers and keeping the source's decimal precision — never widen numbers
   through float conversion (`137`, not `137.0`); and
5. call `update_page_data_upload` for a built file over 20,000 bytes, or
   `update_page_data` inline otherwise (decided per run, from the file) with the read
   `live_version_id` as `expected_version`, the latest represented
   `source_as_of`, and the requested publish mode.

A data file staged with `start_page_upload kind='data'` and `append_page_upload`
costs one model round trip per append, so the file goes up in as few appends as
the returned `max_chunk_bytes` allows. At the 1 MiB default a ~1 MB payload
is one append. A client that reads `workspace_file` references host-side never
puts those bytes in model output; a deployment whose clients have the model emit
the base64 itself may lower `PAGE_UPLOAD_MAX_CHUNK_BYTES` (DEPLOYMENT.md §5),
at the cost of more appends. The ceiling changes transport only: the data and
escaped-envelope limits above apply unchanged.

`update_page_data` locks the page, rereads the published template, rejects
source regression/future coverage, creates an immutable version through the
normal approval state machine, and deduplicates exact retries. On
`stale_version` or an ambiguous transport result, reread the summary once, compare hashes,
and retry at most once only when the intended coverage is not already present.
Before that retry the run refreshes its contract files and, if it used CONFIG
registries, rereads them with `get_page_config`: a concurrent configuration
change is one cause of `stale_version`, and CONFIG is in neither export.

Use `publish:false` for a canary. Approval-gated updates remain pending for a
human; a disabled page cannot be published through the takedown.

### Reconciling what you actually wrote

Step 3 above says "stop without writing if anything is missing", and the schema
cannot tell you that you are in that state. The schema validates **shape**: a
payload missing three weeks of days, or a whole deal, satisfies it exactly as
well as a correct one, so totals can be understated and a series can start late
while the payload stays schema-valid.

So both `update_page_data` and `get_page_data` return **`data_profile`** — what
the payload contains, at paths you can name. The examples in this section share
one synthetic fixture (every name, identifier, date and amount is invented): two
deals over ten days, `example-deal-a` at 100 a day and `example-deal-b` at 50.

```json
{ "arrays": { "rows": { "count": 20, "fields": {
      "date":   { "kind": "date",   "min": "2026-01-01", "max": "2026-01-10", "distinct": 10 },
      "dealId": { "kind": "key",    "distinct": 2, "values": { "example-deal-a": 10, "example-deal-b": 10 } },
      "spend":  { "kind": "number", "sum": 1500, "min": 50, "max": 100 } } } },
  "scalars": { "dataThrough": "2026-01-10" } }
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

`coverage_unverified` exists because comparison has a blind spot: the **first**
payload. In the synthetic fixture, the request asks for January 1 onward, but the
source export is a trailing window, so the first payload built from it covers
only January 5–10:

```
create_page_from_template  (no data: awaiting first ingest)
update_page_data           rows= 12  2026-01-05 → 2026-01-10   ← wrong window
```

There is no wider payload to regress from, so a diff finds nothing: the first
write is precisely the write a diff cannot see. So a first real payload over an
empty one is reported as unverified instead of silently accepted, and naming that
array in `expect` withdraws the warning because `expect` enforces it outright.

### "It said it updated, but there's no new data"

The other direction. A refresh can publish a new version with no warnings and
exactly the same coverage as the version before it, for example January 1–10
again when the user expected January 11 to arrive.

Nothing is broken. The managed-data dedupe key is `(data_sha256,
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
tells the caller not to describe it as a data update. Report whether data or
coverage actually changed, not only that a version was published.

### `expect`: make it a contract instead of a promise

Stronger, and preferred when the source is in hand. Compute the same numbers from
the source in the pass that builds the payload, and let the server enforce them:

```
update_page_data  slug="example-campaign"  data={…}  source_as_of="2026-01-10"
                  expected_version=<live_version_id>
                  expect={ "row_count":  {"rows": 20},
                           "totals":     {"rows.spend": 1500.00},
                           "date_range": {"rows.date": ["2026-01-01", "2026-01-10"]} }
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
All three `get_page_data` read sizes are available to it. An export URL it mints
is re-authorized against the same grant on every fetch, so removing the grant
or revoking the token also ends its outstanding URLs.
An existing broad authoring token can prepare and execute user-directed updates
without additional Pages server configuration.

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
