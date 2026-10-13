# Pages — agent workflow

How an agent should carry a Pages request from start to finish: what to find
out first, which route a new page takes, how to prove the data, how to publish
and confirm, how to change a live page, how to hand off a recurring refresh, and
what the final report says. The other guides hold the mechanics this one links
to: [AUTHORING.md](AUTHORING.md) (what works inside a page),
[TEMPLATES.md](TEMPLATES.md) (stored designs), [DATA_UPDATES.md](DATA_UPDATES.md)
(managed data and refreshes) and [MOTION.md](MOTION.md) (charts and explainers).

An agent reads this through the `get_guide` tool: call it with no arguments for
the index, then `topic="workflow"` and the `section` you need. Read the section
for the step you are on, not the whole guide up front.

## Who owns what

- **Pages** owns how to use Pages: the tools, the page contract, versions,
  publication state, and this guide. Tool names here are the server's own;
  your host may prefix them (for example `mcp_pages_list_templates`).
- **Your host** (the agent platform calling Pages) owns execution: approvals,
  audit declarations, schedulers, file transfer and which tools a run is offered.
  Follow its rules where they are stricter than anything here.
- **Your deployment** owns whose data, which connectors serve it, what its
  metrics mean, which designs it has, and who may see a page. None of that is in
  this guide. If your deployment gives you a source catalog or house rules, they
  decide those questions.

## Before you start: what to find out

A request names a route (new from a template, like an existing page, overview,
new design, content), a destination (slug, title, workspace), the data
(sources, scope, metrics) and who will see it.

- A missing line, a literal `{token}` or a `[PLACEHOLDER]` means **not
  provided**. Never fill it with a guess that looks like a value.
- **Interactive:** ask once, in one message, for every missing required input.
  Take sensible defaults for the rest and say which you took. Do not ask for
  something the request, the source or the page itself already answers.
- **Unattended** (a scheduled run): nobody can answer. A missing required
  input stops the run as blocked, naming it.
- Check the slug before building (`get_page`). Taken means ask (interactive) or
  block (unattended). Never invent a `-v2` or any other suffix.

## Choosing the route for a new page

Check the template library first (`list_templates`). A new page that belongs to
an existing design costs a few kilobytes of config, not a re-authored document.

| Route | When | How |
|---|---|---|
| **From a template** | a stored design covers the request | `get_template` → `create_page_from_template` |
| **Like an existing page** | "the same as /x, but for …" | reuse that page's design; never copy its HTML |
| **Overview** | one page across many projects | an overview design whose rows keep their native grain |
| **New design** | nothing stored fits | author it with the four managed blocks |
| **Content** | no data at all (guides, onboarding) | author HTML only; no data, no refresh |

- A request that only **narrows scope** ("like /x but only these items") is
  "like an existing page": scope is config, not a redesign.
- A request that **adds sections** the reference does not have is a new design
  that uses the reference as its style guide.
- If a stored design's data schema **requires a field the sources never
  report**, the design cannot hold this data honestly. Never invent the value
  and never drop the rows: build a new design, styled like it, whose schema
  makes the field optional and renders it as not available. Say so plainly.

## Sources: prove the data before you build

1. **Bind each input to an exact connector and tool** you actually have. A
   source named only in prose ("the latest delivery report") is not a binding.
   If a required connector is unavailable, say so; never substitute another
   source or reuse an old artifact.
2. **Retrieve fresh** and prove the subject is in it before building: the rows
   that match the requested scope, their date range, the distinct identifiers.
   Note those numbers; they become the write's `expect`.
3. **Read the dates inside the file,** not the folder or file name. A "daily"
   export is often a trailing window. When a source is partitioned (one file
   per day), enumerate every partition in range and aggregate them; never take
   only the newest.
4. **Check identity on every file you ingest.** A folder or brand name is not a
   campaign's identity. A file whose identifier does not match the request is a
   stop, not something to filter out after aggregating.
5. **Never filter source rows down to what the page's config lists.** Real
   delivery includes items the config does not model. Aggregate everything in
   scope, then report anything unmapped with its row count and totals.
6. **Required versus optional sources.** Decide which sources the page cannot
   exist without. A required source that cannot be reached or has no rows for
   the subject stops the build (interactive: say what was searched and ask
   whether to start without it; unattended: block). An optional companion
   source that is missing is a flag, not a stop. Never downgrade a required
   source on your own initiative.
7. **Never manufacture rows or dimensions,** not even to make totals reconcile:
   no balancing, plug or adjustment entries. When a derived value disagrees with
   a source-reported total, keep the raw numbers faithful and report the gap.
   Ratios come from summed numerators and denominators, not averaged ratios.

## Building the page

- **From a template:** `get_template` returns the config and data schemas and a
  reference config, not the HTML. Build a **complete** config; it is validated
  and never merged with the reference config, so nothing from another page's
  identity leaks in. Omit `data` to publish the awaiting-first-ingest state, then
  write the data with `update_page_data`. Details: TEMPLATES.md, "Building a
  page".
- **Like an existing page:** `get_page_config` answers whether the reference is
  template-built (`template` names it, or is `null`). Template-built: use that
  template with a complete config of your own. Custom: promote it with
  `create_template_from_page`; if the result lists `hardcoded_config_values`,
  the reference hardcodes its identity. A reference with no managed blocks at
  all (`page_not_template_managed`) cannot be promoted as it is. In either case
  do not edit the reference page unless the user asked you to: build a new
  design styled like it, or ask before migrating the reference to managed
  blocks. Never copy its HTML into the new slug.
- **Overview:** keep each source's native grain in the rows; derive totals in
  the page, not by pre-aggregating away the detail the page filters on.
- **New design:** use the four managed blocks so the page can be reused later
  (AUTHORING.md, "Structure every page so it can be reused"). Read every
  per-instance value, colours included, from `CONFIG`. A page you author is the
  one someone will ask for again; offer to register it as a template once it is
  live.
- **Content:** headings, sections and the material the user supplied. No data
  blocks, no refresh.
- **Large documents** go by reference, never pasted: over 20,000 UTF-8 bytes use
  a staged upload (AUTHORING.md, "Getting a large document in"). Never shrink a
  page to fit a transport.

## Motion: adding, recapping, editing

Flag Motion (MOTION.md) gives live charts, count-up numbers with provenance
cards, freshness and animated explainers. Use it when the request asks for
charts, animation or a recap, or when you author a new data design; it is not a
reason to rewrite charts that already work.

- **Motion on an existing page:** read the page once (`get_page`,
  `get_page_config`, `get_page_data`); locate edit points with
  `find_in_version` instead of re-reading HTML. Never change a number.
  - A **custom** page is edited in place. Build chart and explainer specs in the
    page's own script from `DATA`; never add keys to its `#pages-data`, because
    its refresh replaces the whole payload. Leave alone everything the user said
    to leave unchanged.
  - A **template-built** page is never hand-edited. For this page only, a motion
    version is a separate new page; create one only when the user asks for it.
    For every page on the design, it is a template revision (see "Changing a live
    page").
- **Recap of a page:** the recap's numbers are the page's published numbers
  (`get_page_data`), never a fresh pull, so they match to the cent. Carry the
  page's quality flags into the recap. Agree whether it is a snapshot or a live
  page with its own refresh, and whether it sits on the page or beside it.
- **Editing an explainer:** change only the scenes named; everything else stays
  byte-for-byte. Where the spec lives decides the edit: an inline spec is
  patched; a spec built by the page's script is changed in the script; a spec
  stored in `DATA` is rebuilt by the next refresh, so change the refresh
  instructions too and say both changed.
- The player's **Download video** button records the video in the viewer's
  browser. You cannot produce the file yourself; tell the user to press it.

## Publishing and confirming it is live

- **Say what the write will do before you make it:** publish to the live URL,
  save a draft, or wait for a human approver.
- **Follow the actual response, not an assumption.** Your host may run a write
  immediately, ask a human to approve it first, or only notify. Pages itself may
  publish, keep a draft (`publish=false`), or hold the version as pending on an
  approval-gated page. `page_is_live`, `version_is_live`, `live_version_id` and
  `next_step` say what happened. Never say nothing changed when the write ran.
- **Confirm before you say "live".** `get_page`: the published version must be
  the one your write returned. Draft: `publish_page` (only on open pages).
  Pending: say "not live yet" and name the approval that is waiting.
  "Approved" means the action ran, not that the page changed.
- **Read `preflight`** on the write result. It is a static check (CSP, blocked
  assets, script syntax), not proof that the page renders correctly. `ok:false`
  means fix and redeploy before sharing. If your host offers a browser, a visual
  check is still worth doing.
- **Reviewed workflows keep their review.** A design rerender defaults to a
  draft and a prepared full-page update deploys, checks, then publishes. Do not
  collapse those into one step.

## Changing a live page

Pick the tool by what is changing. The wrong tool silently detaches a page from
its design.

| What is changing | Tool | Cannot touch |
|---|---|---|
| the numbers | `get_page_data` → `update_page_data` (or the staged upload twin) | config, layout, schema |
| this page's settings | `get_page_config` → `update_page_config` | the data block |
| the shared design | new template revision → `rerender_page_from_template`, one page at a time | any page you do not name |
| a line in a custom page | `find_in_version` → `patch_page` | — |

- **Never deploy or patch HTML into a template-built page.** `get_page_config`
  tells you whether it is one. If it already happened, `list_template_pages`
  shows the page as `drifted`, and `rerender_page_from_template` with the
  template named explicitly pulls it back (TEMPLATES.md, "Propagating a design
  fix").
- **Unclear request:** `prepare_dashboard_update` classifies it and returns the
  safe workflow for that exact slug. Never create a replacement or companion
  page to avoid an update.
- **Several changes to one page** land in order: design, then config, then data,
  rereading the live version before each dependent write. A mixed sequence stops
  while an earlier step is a draft or pending.
- **Several pages:** list every requested slug and finish each one (read,
  reconcile, write, verify) before the next. Report every slug as updated,
  unchanged, blocked, failed or not attempted; a subset is not a finished batch.
  Never replay a completed write.
- **`stale_version`:** someone else's version landed first. Reread it. If it is
  already your change, stop. Otherwise reconcile against it before retrying:
  refresh the contract and any config your payload depends on, and rebuild the
  payload if they changed, so you never overwrite another edit with stale
  bytes. Then retry once with `details.retry_with_expected_version`. Reuse a
  still-staged upload (`details.upload_still_staged`) only when its exact bytes
  are still the right complete result; otherwise stage the rebuilt bytes
  (DATA_UPDATES.md, "Safe managed update execution").
- **Waiting on approval:** stop, say what is waiting and what happens after it
  lands. Never route around an approval by making the same change with another
  tool.
- **A failed upload chunk** is usually resumable: re-send from the sequence the
  error expects; replaying an exact chunk is harmless. Follow the error when it
  says the upload cannot continue (expired, or the declared size or hash cannot
  be met), and cancel an upload only when you no longer want to deploy it.

## Recurring refreshes

Pages prepares refresh prompts; it does not schedule anything.

1. `prepare_dashboard_update` with the exact slug, `recurring=true` and one
   source binding per input (DATA_UPDATES.md, "`prepare_dashboard_update`").
   `instructions` describe the data work of **one run**: scope, mapping, metric
   definitions. Leave out cadence and any publication gate the user did not ask
   for; the prompt's gap rules already decide what blocks.
2. The returned prompt is used **verbatim**, including its execution
   requirements. Never paraphrase it, add a schedule line or execute it now.
3. **Installing it:** if your host has a scheduler you are authorized to use,
   first look for an existing task for this slug and replace that task's prompt;
   otherwise create one task at the cadence the user asked for. Never leave two
   recurring tasks for one page. If you cannot install it, hand the prompt to the
   user verbatim and say it still needs installing.
4. Every tool a run may call must be in the binding's `required_tools`; a run
   offered only its required tools never sees one that is not listed.
5. Saved prompts do not update themselves. After a Pages upgrade or a change to
   the page's sources, regenerate the prompt and replace the task's copy.
6. A one-time request ("refresh it now") is not a recurring task: follow the
   returned prompt in this conversation.

A refresh run ends in exactly one outcome: updated, unchanged
(`source_not_updated`, recorded with `record_refresh_check`), or blocked. Only a
hard stop blocks; other gaps publish what the sources verifiably report and flag
each gap (DATA_UPDATES.md, "What a teammate does").

## Before you say it is done

For a write that committed a version:

1. **Reconcile `data_profile`** (row counts, date ranges, totals, distinct
   identifiers) against the numbers you computed from the source. Better, pass
   `expect` with the write so Pages refuses a payload that disagrees
   (`data_reconciliation_failed` means the aggregation is wrong; never widen
   `tolerance` to pass).
2. **Read every `data_warnings` entry** and check it against the source and the
   gap rules. A warning does not stop the write, so a real discrepancy reaches
   whatever version you wrote until you fix it; an expected, documented gap is
   reported, not "fixed". Whether that version is live comes from the returned
   state fields, not from the warning.
3. **Read `preflight`** and fix errors before sharing.
4. **Share `urls.live` verbatim.** Never rebuild a URL from a host and a slug.
5. **Access:** a page with no password and no partner portal is visible only to
   people signed in to this Pages deployment as admins. A portal grants its
   partner access on its own, and you cannot list portals, so never report a
   page as admin-only from a missing password alone. To share with someone
   outside the deployment, set a password with `set_password` and send the URL
   and the password, or ask an admin to add the page to a portal.

A data page is not done until it shows data. An empty page is a failure unless
every source genuinely has no rows in scope; then say exactly that.

## The final report

One message, in plain words:

- the live URL (`urls.live`), or "not live yet" and why;
- the design used (a stored template by name, or a custom design);
- per source: what was retrieved, its coverage and row counts;
- the totals you reconciled, and every quality flag or gap;
- access: password set, admin-only, or portal;
- the refresh: installed, handed off for installation, not applicable, or not
  possible and why.

Keep internal vocabulary ("route", "schema", "four blocks", stage names) out of
the user's message unless they used it first.

## Never

- Present a local file, a sandbox path or a draft as a published page.
- Copy another page's HTML into a new slug, or edit a page you were only asked
  to use as a reference.
- Invent a template name, a config field, a slug suffix, a row, a zero or a
  mapping. Read the schema; report the gap.
- Infer which template built a page from its metadata: `get_page_config` is the
  answer for one page, `list_template_pages` for one template.
- Send a path, `$(cat …)` or a placeholder as `html`: tool arguments are data,
  not a shell, and Pages cannot read your files.
- Shrink a working page, or drop rows, columns or date granularity, to fit a
  tool call.
- Invent a tool rejection. Report an error only when a tool returned it; the
  20,000-byte figure picks a transport, it is not the server's limit.
- Put credentials, tokens or secrets in a page, a prompt, an instruction or a
  source binding.
