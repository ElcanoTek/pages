// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Presentation only: API payloads and published/customer content are untouched.
// The allowlist comes from owned API, preflight and data validation messages.
// Keep their English source spelling in sync when a server diagnostic changes.
(function (root, factory) {
  const api = factory(root && root.PagesI18n ? root.PagesI18n : require("./i18n.js"));
  if (typeof module === "object" && module.exports) module.exports = api;
  if (root) root.PagesMessages = api;
})(typeof window !== "undefined" ? window : null, function (i18n) {
  const sources = [
    "#pages-data contract_version must be 1",
    "#pages-data data must be an object",
    "#pages-data must contain an object envelope",
    "#pages-data must contain exactly contract_version, refreshed_at, source_as_of, and data",
    "#{0} exceeds its {1}-byte limit",
    "#{0} must contain a JSON object so its schema can be derived",
    "#{0} must contain valid JSON",
    "#{0} must use type=\"{1}\"",
    "Bind with addEventListener instead of {0}=, or rename the global (e.g. \"{1}\" -> \"dr_{2}\").",
    "Cannot use 'import.meta' outside a module",
    "Embed the data in the document at deploy time, or move the page to the managed-data path (update_page_data) so Pages refreshes the numbers for you.",
    "Inline <script> #{0} does not parse: {1}. Nothing in that block runs, so every function it declares is undefined and every control wired to one is dead.",
    "Inline <script> #{0} exceeds the {1} parser's nesting limit; its syntax could not be verified.",
    "Invalid input",
    "Invalid input: expected int, received number",
    "Invalid input: expected {0}",
    "Invalid input: expected {0}, received {1}",
    "Invalid option: expected one of {0}",
    "Keep render_mode raw. Remove the custom switcher reader, or guard a missing block and render nothing; the deck keeps its own toolbar. Return to the portal index to open another page.",
    "Keep the state in a module-level variable, or wrap every access in try/catch so an unhandled throw cannot take the rest of the script down with it.",
    "Managed data {0} is {1} bytes; its limit is {2} bytes. Changing upload transport cannot help. Keep the complete source file and correct the data contract or ask the operator to review capacity. Do NOT split, sample or summarize the payload to fit.",
    "Nothing to fix. To change the deck, edit it with fleet's bento_doc.py and deploy the new file as a new version.",
    "Omit render_mode — a deck defaults to raw — or pass \"raw\".",
    "Pages could not store the version",
    "Pages is updating — retry shortly",
    "Preflight found no problems: scripts parse, managed blocks satisfy their contract, no shadowed inline handlers, no blocked subresources or APIs.",
    "Preflight found {0}. These are advisory — nothing was blocked.",
    "Re-check the document was uploaded whole — a truncated or hand-reassembled script is the usual cause.",
    "Reduce expression nesting and re-run preflight; this finding does not prove a browser syntax error.",
    "Remove the control, or ask for \"{0}\" to be added to the content-host sandbox in internal/csp.",
    "Round-trip the file through fleet's bento_doc.py (`get`, then `set`) before deploying: `set` drops the block. Or accept the residue knowingly.",
    "The Pages-managed blocks do not satisfy their own contract: {0}",
    "The render layer JSON.parses these blocks, so this throws before anything draws and the page serves blank. Fix the block contents and redeploy — or, if the numbers are what changed, use update_page_data instead of editing the block by hand.",
    "This Bento deck reads #pages-nav, but Pages deliberately injects neither the navigation payload nor the Page menu into a deck, even when a portal authorises the view.",
    "This deck's document carries a live-collaboration block (a room URL and a private key). Bento writes one into any file saved from its own UI. It is inert on this host — the deck's own CSP and the content host's both forbid connections — but it is key material in a stored, shareable document, and if the deck's guard is ever removed the room becomes joinable.",
    "This is a Bento deck and render_mode is \"themed\". A deck is its own application with a complete design; themed would splice Flag's head tags ahead of the deck's own CSP meta and draw a Page menu over its toolbar. Pages refuses to store a deck this way.",
    "This is a Bento deck. Its runtime is deflate-compressed, so the static checks here could not see the application and judged only its bootstrap. On the content host the deck opens in Bento's full editor. Nothing edited there is saved to Pages: Save downloads a new .bento.html (the sandbox refuses native file pickers, so Pages removes that API before the deck boots and Bento takes its download path — it says so on the button), Export PDF prints, and unsaved edits are gone on reload (the sandbox has no storage). Speaker view needs pop-ups and copy-to-clipboard needs a permission, neither of which the sandbox grants. Embedded video and audio must be data: or blob: URLs.",
    "This payload is byte-identical to the one already published, so no metric changed. A version was still created because source_as_of advanced, which is how a re-verified source is recorded — but if you expected new numbers, the source did not contain any. Do not report this as a data update.",
    "Unexpected reserved word",
    "Use an asset source permitted by {0}: {1}. Do not widen the content policy to hide this finding.",
    "a cron expression has exactly five fields (minute hour day-of-month month day-of-week), e.g. \"30 14 * * *\"; got {0}",
    "a cron expression is at most {0} characters",
    "a deck edit session requires a staff actor",
    "a deployed upload cannot be cancelled",
    "a live page already uses this slug: {0}",
    "a managed page allows at most one #{0} block",
    "a newer deck version was saved — reopen the editor from the admin and reconcile your changes before saving again",
    "a page slug or page_id is required",
    "a portal is already named: {0}",
    "a section needs its topic",
    "a template name is required",
    "after.createdAt must be a valid timestamp",
    "after.id must be a positive database id",
    "an upload ticket is required",
    "at most {0} edits per patch",
    "await is only valid in async functions and the top level bodies of modules",
    "bad render_mode: {0}",
    "bad source: {0}",
    "bad version status: {0}",
    "boolean schema is false",
    "can only approve a pending version (status={0})",
    "can only publish a draft version (status={0})",
    "can only reject a draft/pending version (status={0})",
    "can only roll back to an approved version (status={0})",
    "cannot publish a rejected version; deploy new content",
    "chunk exceeds the declared total_bytes by {0} bytes (declared {1}, already stored {2}). Do NOT trim the payload to fit: the declaration describes a DIFFERENT document from the one you are sending, so a trimmed one would fail content_sha256 anyway — and if it did not, you would publish a truncated page. Recompute total_bytes and content_sha256 from the exact bytes you intend to send and start a new upload. {3}",
    "chunk_base64 must be canonical base64 for one non-empty chunk. {0}",
    "complete upload does not match content_sha256. Sequence {0} was NOT accepted, so the upload still holds {1} of {2} bytes and this exact sequence is still the one expected — re-send it if that chunk was wrong. Only if the earlier chunks are wrong does this need a new upload. {3}",
    "config does not satisfy the page's JSON Schema",
    "config is required and must be a complete object",
    "content_sha256 must be a lowercase SHA-256 hex digest",
    "could not derive a #{0} for this config",
    "credential-like field is forbidden at {0}.{1}",
    "credential-shaped value is forbidden at {0}",
    "dashboard must have a published version before it can be updated",
    "data does not satisfy the page's JSON Schema",
    "data export URLs are unavailable: this server has no RAW_TOKEN_SECRET",
    "data is {0} bytes; inline managed-data transport is capped at {1}. {2}Do NOT split, sample or summarize the payload to fit. {3}",
    "data must be a JSON object",
    "data must contain only JSON values",
    "data must contain only finite JSON numbers",
    "data_size is only valid for kind 'data'",
    "data_size requires nonnegative safe integer payload_bytes and escaped_payload_bytes >= payload_bytes",
    "data_update tools require an exactly granted page slug",
    "database query timed out — retry shortly",
    "database unavailable — retry shortly",
    "decoded chunk must be 1-{0} bytes. {1}",
    "detail must be a string",
    "detail must be at most {0} characters",
    "disabled must be a boolean",
    "edit {0} would make the document {1} bytes; the limit is {2} bytes",
    "edit {0}: count must be an integer 1-{1}",
    "edit {0}: expected {1} occurrence(s) of the anchor, found {2}. {3}",
    "edit {0}: find and replace must both be strings",
    "edit {0}: find must be 1-{1} characters",
    "edit {0}: replace must be at most {1} characters",
    "edits must be a non-empty array",
    "empty_data must be the object a page built from this shows before its first refresh",
    "expect must be an object",
    "expect.date_range[\"{0}\"] must be [min, max]",
    "expect.tolerance must be a fraction between 0 and 1",
    "expected exactly {0} bytes, received {1}",
    "expected upload sequence {0}, received {1}: {2} of {3} bytes are stored. A FAILED append does not advance the sequence, so whatever went wrong at {4} still has to be sent. {5}",
    "expected_version cannot be used when creating a missing page",
    "expected_version is required for config updates",
    "expected_version is required for data updates",
    "expected_version is required when migrating template config or data",
    "external schema references are not allowed",
    "html is a literal file placeholder, not page content; use the staged page-upload tools for workspace files",
    "html is required",
    "html is {0} bytes; inline deploys are capped at {1}. Every inline byte is output you pay for once and prompt you pay for on every later step. If this is another instance of a design Pages already stores, call list_templates and then create_page_from_template with just its config (kilobytes). Otherwise write the file to disk and use create_upload_ticket, which returns a URL your shell PUTs the file to — the bytes never pass through your context at all.",
    "id must be a positive integer",
    "instructions are required",
    "instructions are required; older clients may instead supply their credential-free workflow",
    "instructions must be at most {0} UTF-8 bytes",
    "internal error",
    "invalid JSON request body",
    "invalid or revoked token",
    "invalid or stale pagination cursor",
    "invalid pagination cursor",
    "invalid request",
    "kind applies to a slug target; a template upload is always kind template",
    "kind must be one of: page, data",
    "limit must be an integer from 1 to {0}",
    "managed pages allow exactly one #{0} block",
    "managed pages require exactly one #{0} block and one #{1} block",
    "materialized config exceeds the {0}-byte limit",
    "materialized page exceeds the {0}-byte limit",
    "missing bearer token",
    "must NOT be valid",
    "must NOT have additional properties",
    "must NOT have duplicate items (items ## {0} and {1} are identical)",
    "must NOT have more than {0} items",
    "must NOT have unevaluated properties",
    "must be equal to constant",
    "must be equal to one of the allowed values",
    "must be multiple of {0}",
    "must be object",
    "must be plain text: this page's layout writes it as HTML, so < is refused; rebuild the page from the current template revision",
    "must be plain text: this page's layout writes it as HTML, so <, > and & are refused; rebuild the page from the current template revision",
    "must be {0}",
    "must be {0} {1}",
    "must contain at least {0} and no more than {1} valid item(s)",
    "must contain at least {0} valid item(s)",
    "must have required property '{0}'",
    "must match \"{0}\" schema",
    "must match a schema in anyOf",
    "must match exactly one schema in oneOf",
    "must match format \"{0}\"",
    "must match pattern \"{0}\"",
    "needs_attention must be true or false",
    "no HTML was supplied",
    "no deleted page to restore: {0}",
    "no earlier approved version to roll back to",
    "no published version to promote for page: {0}",
    "no section {0} in topic {1}",
    "note must be 500 characters or fewer",
    "note must be a string or null",
    "only a Bento deck can be saved through this channel",
    "only a Bento deck opens in its own editor; every other page has Edit source",
    "outcome must be one of: {0}",
    "page data not found",
    "page has no #{0} block to update; it was not built from a template",
    "page has no #{0} block; it was not built from a template",
    "page has no published template to manage",
    "page has no published version",
    "page has no published version to rerender",
    "page has no published version to update",
    "page is missing its #{0} block",
    "page is not {0}-managed; add exactly one #{1} block and one #{2} block",
    "page not found: {0}",
    "page upload not found, expired, or owned by another token",
    "page {0} already exists; use update_page_config to change its settings, update_page_data to refresh its numbers, rerender_page_from_template to move it to a new design, or rollback_page to deliberately return it to an earlier version",
    "page {0} drifted off its template and its published HTML no longer carries the #pages-config and #pages-data blocks a rerender reads, so its config and data cannot be recovered from what is serving. Roll back to the last template-built version (list_versions), or rebuild it with create_page_from_template. ({1})",
    "page {0} has no published version to patch; deploy one first, or pass base_version_id",
    "page {0} has no published version; pass version_id",
    "page {0} has no published version; pass version_id to preflight a draft",
    "page {0} is already on {1} revision {2}",
    "page's published version is not bound to any template, so there is no design to move it to. If it drifted off one — list_template_pages reports drifted:true and the last_revision it was on — name that template explicitly to re-attach it.",
    "partner portals are admin-only: who may see which dashboards is a human decision",
    "pass a label or a sort_order to change",
    "pass either slug or page_id, not both",
    "pass exactly one of upload_id or html",
    "pass schedule, owner or both (null clears one)",
    "password must be 512 characters or fewer",
    "password must be a non-empty string",
    "patched document would be {0} bytes, over the {1} limit",
    "portal already exists: {0}",
    "portal label contains unsupported control characters",
    "portal label must be a string or null",
    "portal label must be {0} characters or fewer",
    "portal name contains unsupported control characters",
    "portal name is required",
    "portal name must be {0} characters or fewer",
    "portal not found",
    "portal password contains unsupported control characters",
    "portal password must be a string",
    "portal password must be at least {0} characters — omit it and Pages generates a strong one",
    "portal password must be {0} characters or fewer",
    "portal password must not begin or end with whitespace",
    "portal slug is required",
    "portal slug must be one url-safe segment (a-z 0-9 - _)",
    "portal slug must be {0} characters or fewer",
    "preflight found {0} error(s); every page built from this template inherits them",
    "prepared dashboard update prompt is too large",
    "property name must be valid",
    "provide data or a preserved envelope, not both",
    "published HTML exceeds the {0}-byte managed-data limit",
    "published HTML is empty",
    "published template is unavailable",
    "published version is unavailable",
    "query must be 1-{0} characters",
    "query must be 200 characters or fewer",
    "recurring updates must be data-only",
    "recurring updates require `sources`: bind each input to its MCP server (source_id + mcp_server, plus path/partition when the data is a date-partitioned folder). An unattended run cannot safely re-derive bindings from prose.",
    "refresh owner must be an email address",
    "refresh schedule: {0}",
    "refusing to publish: this document's Pages-managed blocks are not valid, so the page would render blank and every managed-data tool would fail on it. {0}",
    "removing a workspace is admin-only",
    "reopen the deck from the admin before saving: a valid X-Pages-Base-Version is required",
    "request body could not be decoded",
    "request body must be the raw page bytes",
    "request body too large",
    "require_approval must be a boolean",
    "schema recursion is too deep",
    "sequence must be a non-negative integer",
    "sequence {0} was already accepted with DIFFERENT bytes; {1} of {2} bytes are stored and the next chunk expected is {3}. You are re-sending a chunk you already sent, with a different encoding of it. {4}",
    "should be {0} {1}",
    "slug already exists: {0}",
    "slug changed concurrently: {0}",
    "slug is required",
    "slug is required (use null to clear the home page)",
    "slug must be url-safe (a-z 0-9 - _ /)",
    "slug segment '{0}' is reserved (collides with a route)",
    "sort_order must be an integer between 0 and {0}",
    "source_as_of cannot be more than {0}ms in the future",
    "source_as_of cannot move backward (current coverage is {0})",
    "source_as_of cannot move behind existing source coverage",
    "source_as_of is required when data is supplied; it is the coverage that data represents",
    "source_as_of is required with replacement migration data",
    "source_as_of requires replacement migration data",
    "source_as_of_seen must be an RFC3339 timestamp",
    "sources must be a non-empty array when provided",
    "sources must contain at most {0} entries",
    "sources[{0}] must be an object",
    "sources[{0}].partition must be an object",
    "sources[{0}].partition.by must be \"date\"",
    "sources[{0}].required_tools must be a non-empty array when provided",
    "sources[{0}].scope must be an object",
    "sources[{0}].scope.values must be a non-empty array",
    "sources[{0}].scope.values must contain at most {1} entries",
    "sources[{0}].scope.values[{1}] is duplicated",
    "sources[{0}].source_id is duplicated",
    "staged managed data must be a JSON object",
    "staged upload is not valid JSON: {0}. Stage the exact file your generator wrote; do not hand-edit it.",
    "stored upload failed byte-count or SHA-256 verification",
    "supply exactly one of slug or template",
    "template is required",
    "template must be url-safe with no slashes (a-z 0-9 - _), at most {0} characters",
    "template name changed concurrently: {0}",
    "template not found: {0}",
    "template revision not found: {0}",
    "template {0} has no revision {1}",
    "the assembled template exceeds the {0}-byte limit",
    "the derived #{0} exceeds its {1}-byte limit",
    "the edits produced no change; check that find and replace actually differ",
    "the page is busy with another update — retry shortly",
    "the payload does not match what you said it would contain, so nothing was written: {0}",
    "the {0} block must have an explicit closing </script> tag",
    "the {0} field {1} has an empty list item",
    "the {0} field {1} has an invalid step (use a number from 1 to {2})",
    "the {0} field {1} is not a number, range, list or step (names such as MON or JAN are not accepted)",
    "the {0} field {1} is outside {2}-{3}",
    "the {0} field {1} steps from a single number; write a range ({2}-{3}/{4}) or */{5}",
    "the {0} schema is not a valid self-contained JSON Schema 2020-12 document",
    "the {0} schema is too complex",
    "the {0} schema is too deeply nested",
    "the {0} schema must declare JSON Schema draft 2020-12 in $schema",
    "the {0} schema root must be an object",
    "the {0} schema root must declare type \"object\"",
    "the {0} schema uses a pattern that could backtrack catastrophically; simplify the regex",
    "this action is admin-only",
    "this bearer token may only use its scoped Pages MCP data tools",
    "this document is a Bento deck (it carries a <script type=\"application/bento+json\"> block), which Pages serves raw: it is its own application with a complete design, so render_mode \"themed\" would splice Flag's head tags ahead of the deck's own CSP and draw a Page menu over its toolbar. Omit render_mode or pass \"raw\".",
    "this edit session has expired — reopen the deck from the admin",
    "this edit session is no longer available for this page",
    "this page has been disabled (taken down) by an admin — agents cannot publish, roll back, or delete it",
    "this page has no #{0} block, so there is nothing to vary per instance; separate its per-instance values into #{1} first and redeploy",
    "this page is no longer a deck — reopen it from the admin",
    "this page requires approval — agents may only deploy a pending version; a human must approve",
    "this token already has {0} uploads in flight and none are stale. Call cancel_page_upload on the ones you are no longer sending, or wait for them to go idle.",
    "this token is not authorized for page slug {0}",
    "this upload has already been deployed",
    "this upload ticket has expired; create a new one",
    "this upload ticket was already used; create a new one",
    "this upload was already committed; its staged bytes are gone",
    "this upload was staged for a {0}, not a {1}; use {2} instead",
    "this upload was staged for {0}, not {1}; the target is fixed when the upload starts",
    "this upload_id was already deployed with different options; use the original options or start a new upload",
    "title is required",
    "token scope {0} cannot call {1}",
    "total_bytes must be 1-{0}",
    "unknown export file",
    "unknown guide topic",
    "unknown or expired data export URL",
    "unknown or expired upload ticket",
    "unknown theme: {0}",
    "unsupported content encoding {0}",
    "update instructions are nested too deeply",
    "update_type must be auto, data, or layout",
    "upload chunks are incomplete",
    "upload is incomplete: {0} of {1} bytes received",
    "uploaded bytes do not match content_sha256. The ticket is pinned to the hash you declared, so the file changed between hashing and sending, or the transfer altered it — re-hash the exact file you are sending (`sha256sum <file>`) and PUT it again with --data-binary; the ticket is still valid.",
    "uploaded page content must be valid UTF-8",
    "validation failed",
    "version {0} not found on page {1}",
    "version {0} not found on this page",
    "version_id is required",
    "version_id must be a positive integer",
    "workspace already exists: {0}",
    "workspace name contains unsupported control characters",
    "workspace name is required",
    "workspace name must be 100 characters or fewer",
    "workspace not found",
    "workspace organization requires an authenticated user or agent",
    "workspace_id is required (use null for Ungrouped)",
    "workspace_id must be a positive database id",
    "workspace_id must be a positive database id or null",
    "{0} Upload {1} is untouched and still deployable — retry deploy_page_upload with the same upload_id; do not re-upload the file.",
    "{0} cannot reach anything: the content host sets \"connect-src 'none'\". Every request fails.",
    "{0} carries an integer at {1} beyond IEEE-754 exact range (|value| > 9007199254740991), so it cannot round-trip without silently changing. If this is a count, it is almost certainly a summing bug — check for string concatenation. If it is an identifier, send it as a string.",
    "{0} does not satisfy the page's JSON Schema",
    "{0} does not work on the content host: its sandbox does not include \"{1}\". {2}",
    "{0} exceeds the {1}-byte limit",
    "{0} has {1} rows and the payload it replaces had none, so there is nothing to compare this against and you supplied no expect for it. Its coverage{2} and totals are UNVERIFIED. A first payload that is already truncated looks exactly like a correct one here. Reconcile data_profile against the source, or state the window and totals in expect so Pages can refuse a payload that disagrees.",
    "{0} is already in this portal",
    "{0} is already in {1} portals (limit {2})",
    "{0} is not in this portal",
    "{0} is not in this portal — add it first",
    "{0} is required",
    "{0} must be a boolean",
    "{0} must be a positive database id",
    "{0} must be a string or null",
    "{0} must be a valid RFC3339 timestamp",
    "{0} must be an RFC3339 timestamp with a timezone",
    "{0} must be an object",
    "{0} must be at most {1} characters",
    "{0} must be one MCP server or tool name (letters, digits, '_', '.', '-'); bind each server as its own source instead of combining names",
    "{0} must not contain line breaks or control characters",
    "{0} never fires: no month it names has that day",
    "{0} now ends at {1}, earlier than the {2} already published.",
    "{0} now starts at {1}, later than the {2} already published. A trailing-window source export reads exactly like this — confirm the earlier days are genuinely absent and not merely unread.",
    "{0} references {1}, which the content host's \"{2}\" blocks. It will not load.",
    "{0} throws SecurityError on the content host — pages are served from a sandboxed, cookieless opaque origin with no storage access.",
    "{0} went from {1} rows to {2}. If the source is cumulative, this refresh is missing data.",
    "{0}.{1} still ends at {2}, the same day the published payload ended on. Figures changed but the window did not, so this refresh restated days that were already covered and added no new one. If a newer day was expected, the source did not have it — say that rather than reporting a refresh.",
    "{0}=\"{1}(...)\" calls the built-in DOM member {2}(), not your global function of the same name. Inline handlers resolve identifiers against the element, its form and the document before window, so this throws at click time and the control silently does nothing.",
    "{0}=\"{1}(...)\" resolves to the built-in DOM member {2}() inside an inline handler, not to a global. If you meant a function you define elsewhere, this control is dead.",
    "clearing a page password is admin-only",
    "showing a page password is admin-only",
    "the current #{0} payload does not satisfy its schema",
    "the #{0} payload does not satisfy the #{1} schema",
    "must NOT have fewer than {0} characters",
    "must NOT have more than {0} characters",
    "must NOT have fewer than {0} items",
    "must NOT have fewer than {0} properties",
    "must NOT have more than {0} properties",
    "must have property {0} when property {1} is present",
    "must have properties {0} when property {1} is present",
    "Chromium logs \"Ignored call to print()\" and nothing happens — a \"Download PDF\" button does nothing at all.",
    "The call is ignored; confirm() returns false, so any flow gated on it silently takes the negative branch.",
    "The click is swallowed — an Export CSV/Excel button appears to work and produces no file.",
    "Returns null and no window opens.",
    "a[download] / programmatic download",
    "an invalid URL",
    "Video poster",
    "Media source",
    "<img> image",
    "<image> image",
    "<video> media",
    "<audio> media",
    "<object> object",
    "Do not hand-emit base64. If your client advertises workspace_file references on append_page_upload, use a whole-file SHA-256 and exact byte ranges, as few appends as max_chunk_bytes allows; the client encodes the file. Otherwise use a permitted programmatic transport (create_upload_ticket for direct file HTTP). Keep accepted bytes and resume this upload.",
    "Do NOT cancel: a failed append changes nothing, and every chunk already accepted is still here. Re-send from the expected sequence and continue.",
    "Widen the anchor until it is unique, or set count:{0} if every occurrence should change.",
    "The anchor is not present. Locate it with find_in_version and copy the exact bytes, including whitespace.",
    "The compact file also exceeds the staged upload limit. Keep the complete source and ask the operator to review capacity. ",
    "Stage the complete compact JSON file: create_upload_ticket with kind 'data', PUT it from your shell, then update_page_data_upload. ",
    "Managed-data limits in UTF-8 bytes: compact payload {0}; stored HTML-escaped envelope {1} including timestamps and at least {2} metadata bytes; inline payload transport {3}; staged raw file {4}; complete JSON HTTP request {5} including RPC fields and expect. All applicable limits must fit. Direct file staging removes the file from inline/JSON-RPC transport budgets, never the data/envelope limits. Before staging kind='data', optionally supply advisory data_size: payload_bytes is UTF-8 byteLength(JSON.stringify(data)); escaped_payload_bytes counts the same JSON after replacing <, >, &, U+2028 and U+2029 with six-character Unicode escapes. Measure the parsed object, not pretty-printed file bytes; total_bytes and SHA-256 still describe the exact file. The server validates received content independently; measurements do not guarantee acceptance. ",
    "{0} at {1} expected {2}, payload has {3}",
    "no array at that path",
    "no numeric field at that path",
    "no date field at that path",
    "{0} no longer contains {1}, which the published payload had. Rows filtered to a configured subset look exactly like this.",
    "{0} no longer contains {1} and {2} more, which the published payload had. Rows filtered to a configured subset look exactly like this.",
    "1 further warning were omitted to keep this response bounded. A payload that narrows this many fields at once should be reconciled against its source before publishing, not triaged warning by warning.",
    "1 page was built from {0}; pass force to retire it anyway. Those pages keep serving, but they lose their design provenance and can no longer be re-rendered from it. Call list_template_pages first to see which.",
    "{0} further warnings were omitted to keep this response bounded. A payload that narrows this many fields at once should be reconciled against its source before publishing, not triaged warning by warning.",
    "{0} pages were built from {1}; pass force to retire it anyway. Those pages keep serving, but they lose their design provenance and can no longer be re-rendered from it. Call list_template_pages first to see which.",
    "1 warning",
    "{0} warnings",
    " ({0} to {1})"
  ];
  const nestedSlots = new Map([
    [
      "The Pages-managed blocks do not satisfy their own contract: {0}",
      [
        0
      ]
    ],
    [
      "refresh schedule: {0}",
      [
        0
      ]
    ],
    [
      "refusing to publish: this document's Pages-managed blocks are not valid, so the page would render blank and every managed-data tool would fail on it. {0}",
      [
        0
      ]
    ],
    [
      "{0} Upload {1} is untouched and still deployable — retry deploy_page_upload with the same upload_id; do not re-upload the file.",
      [
        0
      ]
    ],
    [
      "chunk exceeds the declared total_bytes by {0} bytes (declared {1}, already stored {2}). Do NOT trim the payload to fit: the declaration describes a DIFFERENT document from the one you are sending, so a trimmed one would fail content_sha256 anyway — and if it did not, you would publish a truncated page. Recompute total_bytes and content_sha256 from the exact bytes you intend to send and start a new upload. {3}",
      [
        3
      ]
    ],
    [
      "chunk_base64 must be canonical base64 for one non-empty chunk. {0}",
      [
        0
      ]
    ],
    [
      "complete upload does not match content_sha256. Sequence {0} was NOT accepted, so the upload still holds {1} of {2} bytes and this exact sequence is still the one expected — re-send it if that chunk was wrong. Only if the earlier chunks are wrong does this need a new upload. {3}",
      [
        3
      ]
    ],
    [
      "data is {0} bytes; inline managed-data transport is capped at {1}. {2}Do NOT split, sample or summarize the payload to fit. {3}",
      [
        2,
        3
      ]
    ],
    [
      "decoded chunk must be 1-{0} bytes. {1}",
      [
        1
      ]
    ],
    [
      "edit {0}: expected {1} occurrence(s) of the anchor, found {2}. {3}",
      [
        3
      ]
    ],
    [
      "expected upload sequence {0}, received {1}: {2} of {3} bytes are stored. A FAILED append does not advance the sequence, so whatever went wrong at {4} still has to be sent. {5}",
      [
        5
      ]
    ],
    [
      "sequence {0} was already accepted with DIFFERENT bytes; {1} of {2} bytes are stored and the next chunk expected is {3}. You are re-sending a chunk you already sent, with a different encoding of it. {4}",
      [
        4
      ]
    ],
    [
      "{0} does not work on the content host: its sandbox does not include \"{1}\". {2}",
      [
        0,
        2
      ]
    ],
    [
      "{0} has {1} rows and the payload it replaces had none, so there is nothing to compare this against and you supplied no expect for it. Its coverage{2} and totals are UNVERIFIED. A first payload that is already truncated looks exactly like a correct one here. Reconcile data_profile against the source, or state the window and totals in expect so Pages can refuse a payload that disagrees.",
      [
        2
      ]
    ],
    [
      "{0} references {1}, which the content host's \"{2}\" blocks. It will not load.",
      [
        0,
        1
      ]
    ]
  ]);

  sources.push(...[
    "No refresh schedule is set, so Pages does not judge this page.",
    "The stored refresh schedule is not a valid cron expression, so Pages does not judge this page; set it again.",
    "The latest refresh run recorded {0} on {1}, with no detail.",
    "Expected a refresh at {0}; last activity {1}.",
    "Expected a refresh at {0}; no refresh or check has been recorded yet.",
    "Source data hasn't advanced since {0}.",
    "Source data hasn't advanced since {0}; refreshes are still running (last {1}).",
    "No source data yet since {0}.",
    "No source data yet since {0}; refreshes are still running (last {1}).",
    "Scheduled; the first refresh is due {0}.",
    "Refreshing on schedule; last activity {0}.",
    "Source not updated",
    "Source unreachable",
    "Failed",
    "In {0}, values for {1} end on {2} for {3} ({4}). Their rows continue to {5}.",
    "1 entity with key {0}",
    "{0} entities with key {1}",
    "{0} additional values",
    "1 additional value",
    "{0} (+1 more gap)",
    "{0} (+{1} more gaps)",
    "the payload"
  ]);

  const exact = new Set(sources);
  const patterns = sources.filter((source) => /\{\d+\}/.test(source)).map((source) => {
    const parts = source.split(/\{\d+\}/);
    const slots = [...source.matchAll(/\{(\d+)\}/g)].map((match) => Number(match[1]));
    return {source, parts, slots, specificity: parts.join("").length};
  }).sort((a, b) => b.specificity - a.specificity);

  // Literal delimiters avoid a chain of wildcard regexes over untrusted error
  // text. Matching is anchored at both ends and never backtracks over a value.
  function captures(pattern, value) {
    const {parts, slots} = pattern;
    if (!value.startsWith(parts[0])) return null;
    let position = parts[0].length;
    const values = [];
    for (let i = 0; i < slots.length; i++) {
      const delimiter = parts[i + 1];
      const final = i === slots.length - 1;
      if (!delimiter && !final) return null;
      const end = final
        ? (value.endsWith(delimiter) ? value.length - delimiter.length : -1)
        : value.indexOf(delimiter, position);
      if (end < position) return null;
      values[slots[i]] = value.slice(position, end);
      position = end + delimiter.length;
    }
    return position === value.length ? values : null;
  }

  const healthDates = new Map([["The latest refresh run recorded {0} on {1}, with no detail.", [1]], ["Expected a refresh at {0}; last activity {1}.", [0, 1]], ["Expected a refresh at {0}; no refresh or check has been recorded yet.", [0]], ["Source data hasn't advanced since {0}.", [0]], ["Source data hasn't advanced since {0}; refreshes are still running (last {1}).", [0, 1]], ["No source data yet since {0}.", [0]], ["No source data yet since {0}; refreshes are still running (last {1}).", [0, 1]], ["Scheduled; the first refresh is due {0}.", [0]], ["Refreshing on schedule; last activity {0}.", [0]]]);

  function create(locale) {
    const t = locale.t;
    const language = locale.language === "pt" ? "pt-PT" : locale.language;
    const outcomes = {updated:"Updated", source_not_updated:"Source not updated", source_unreachable:"Source unreachable", blocked:"Blocked", failed:"Failed", unknown:"Unknown"};
    function outcome(value) { return Object.hasOwn(outcomes, value) && locale.language !== "en" ? t(outcomes[value]) : value; }
    function date(value) {
      let stamp, hasYear = true, hasTime = false;
      if (/^\d{4}-\d{2}-\d{2}$/.test(value)) stamp = new Date(value + "T00:00:00Z");
      else {
        const found = /^(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) (\d{1,2})(?:, (\d{4}))?(?: (\d{2}):(\d{2}) UTC)?$/.exec(value);
        if (!found) return value;
        hasYear = Boolean(found[3]); hasTime = Boolean(found[4]);
        const month = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"].indexOf(found[1]);
        stamp = new Date(Date.UTC(Number(found[3] || 2000),month,Number(found[2]),Number(found[4] || 0),Number(found[5] || 0)));
      }
      if (!Number.isFinite(stamp.getTime())) return value;
      const options = {timeZone:"UTC", day:"numeric", month:"short", ...(hasYear ? {year:"numeric"} : {}), ...(hasTime ? {hour:"2-digit",minute:"2-digit",hourCycle:"h23"} : {})};
      return new Intl.DateTimeFormat(language,options).format(stamp) + (hasTime ? " UTC" : "");
    }
    function coverageGap(gap) {
      if (!gap || locale.language === "en") return gap && gap.message || "";
      if (!Array.isArray(gap.fields) || !Array.isArray(gap.entities)) return gap.message || "";
      const list = (values, limit, omitted) => {
        const shown = values.slice(0, limit);
        const extra = Math.max(0, omitted + values.length - shown.length);
        if (extra) shown.push(extra === 1 ? t("1 additional value") : t("{0} additional values",new Intl.NumberFormat(language).format(extra)));
        return new Intl.ListFormat(language,{type:"conjunction"}).format(shown);
      };
      const who = gap.entity_count === 1 ? t("1 entity with key {0}", gap.key_field) : t("{0} entities with key {1}",new Intl.NumberFormat(language).format(gap.entity_count),gap.key_field);
      return t("In {0}, values for {1} end on {2} for {3} ({4}). Their rows continue to {5}.",
        gap.path || t("the payload"), list(gap.fields,6,Number(gap.fields_omitted) || 0),date(gap.stopped_on),who,
        list(gap.entities.map(value => JSON.stringify(value)),3,Math.max(0,gap.entity_count-gap.entities.length)),date(gap.rows_through));
    }
    function healthReason(health, freshness = {}) {
      const original = health && health.health_reason || "";
      if (!health || locale.language === "en") return original;
      // Blocked reasons quote the caller-authored last-check detail. Never
      // reinterpret that detail, even when it resembles an owned diagnostic.
      if (health.health === "blocked" && (!Object.hasOwn(freshness,"last_check_detail") || freshness.last_check_detail)) return original;
      const gaps = health.coverage_gaps;
      if (health.health === "coverage_gap" && Array.isArray(gaps) && gaps.length) {
        const first = coverageGap(gaps[0]);
        return gaps.length === 1 ? first : gaps.length === 2 ? t("{0} (+1 more gap)",first) : t("{0} (+{1} more gaps)",first,new Intl.NumberFormat(language).format(gaps.length-1));
      }
      return message(original);
    }
    function summary(value) {
      const match = /^(?:(\d+) errors?)?(?: and )?(?:(\d+) warnings?)?$/.exec(value);
      if (!match || (!match[1] && !match[2])) return value;
      const parts = [];
      if (match[1]) parts.push(match[1] === "1" ? t("1 error") : t("{0} errors", match[1]));
      if (match[2]) parts.push(match[2] === "1" ? t("1 warning") : t("{0} warnings", match[2]));
      return parts.join(locale.language === "es" ? " y " : " e ");
    }
    function message(value, depth = 0) {
      if (typeof value !== "string" || locale.language === "en" || !value || value.length > 65536 || depth > 4) return value;
      if (exact.has(value)) return t(value);
      for (const pattern of patterns) {
        const values = captures(pattern, value);
        if (!values) continue;
        if (pattern.source === "Preflight found {0}. These are advisory — nothing was blocked.") {
          values[0] = summary(values[0]);
        }
        for (const index of healthDates.get(pattern.source) || []) values[index] = date(values[index]);
        if (pattern.source === "The latest refresh run recorded {0} on {1}, with no detail.") values[0] = outcome(values[0].replaceAll(" ", "_"));
        // These slots are constructed from other owned diagnostics. Every other
        // capture (names, source, URLs, JSON values and identifiers) stays raw.
        for (const index of nestedSlots.get(pattern.source) || []) values[index] = message(values[index], depth + 1);
        return t(pattern.source, ...values);
      }
      return value;
    }
    return {message, outcome, healthReason, coverageGap};
  }
  return {...create(i18n), create, sources: Object.freeze(sources)};
});
