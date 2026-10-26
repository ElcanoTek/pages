// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// The schemas the tool registry shares (lib/mcp-tools.js, top half). Each is
// a value, built once; builders copy, so per-use refinement is safe.

import (
	"math/big"
	"regexp"

	"github.com/ElcanoTek/pages/internal/agentguide"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/versions"
)

const decimalIDSource = `^[1-9][0-9]*$`

var decimalIDRE = regexp.MustCompile(decimalIDSource)

var maxBigint = big.NewInt(0).SetUint64(9223372036854775807)

// PostgreSQL BIGINT values are decimal strings in node-postgres. Advertise
// those strings in results, while accepting either a returned string or a
// safe positive integer from clients.
var (
	idOut        = zString().Regex(decimalIDSource)
	decimalIDArg = zString().Regex(decimalIDSource).Refine(func(v any) bool {
		s, _ := v.(string)
		if !decimalIDRE.MatchString(s) {
			return true
		}
		n, ok := new(big.Int).SetString(s, 10)
		return ok && n.Cmp(maxBigint) <= 0
	}, "ID exceeds PostgreSQL's BIGINT range")
	idArg = zPipe(zUnion(zInt().Positive().Int(), decimalIDArg), func(v any) any {
		return jsonv.ToString(v)
	})
	nullableIDArg    = zUnion(idArg, zNull())
	dateTime         = zString().DateTime()
	nullableDateTime = dateTime.Nullable()
	slugSchema       = zString().Min(1).Max(512).Describe("Page slug, for example 'northwind' or 'northwind/q2'.")
	sha256Schema     = zString().Regex(`^[0-9a-f]{64}$`)
	cursorSchema     = zString().Min(1).Max(2048)
	pageLimit        = zInt().Min(1).Max(100).Default(float64(50))
)

var urlsSchema = zObject(
	f("admin", zString().URL().Describe("Elcano-staff admin screen for this page (SSO, staff only).")),
	f("view", zString().URL().Describe(
		"Elcano-staff shortcut that opens the live page without its client password. Staff only — do NOT share this with a client; give them `live`.")),
	f("live", zString().URL().Describe("The client link. Share this one; a password-protected page prompts for its password here.")),
).Strict()

var pageSchema = zObject(
	f("id", idOut),
	f("slug", zString()),
	f("title", zString()),
	f("client_id", zString().Nullable()),
	f("workspace_id", idOut.Nullable()),
	f("workspace_name", zString().Nullable()),
	f("theme_id", idOut.Nullable()),
	f("theme_name", zString()),
	f("require_approval", zBool()),
	f("disabled", zBool()),
	f("published_version_id", idOut.Nullable()),
	f("created_at", dateTime),
	f("updated_at", dateTime),
	f("has_password", zBool()),
).Strict()

// freshnessSchema is the computed answer to "when was this last actually
// right?". Both stamps are already columns on page_versions; the days_*
// fields are derived at read time so a consumer can rank an estate by
// staleness in one call instead of noticing a frozen dashboard when a client
// asks. These facts stay verdict-free: Pages runs no scheduler, and its
// opinion on whether a number is too high lives beside them in
// refresh_health, only for pages someone gave an expected cadence.
var freshnessSchema = zObject(
	f("source_as_of", nullableDateTime.Describe("Latest source coverage the published data represents.")),
	f("refreshed_at", nullableDateTime.Describe("When that data was written to Pages.")),
	f("checked_at", nullableDateTime.Describe(
		"Last time anyone LOOKED, whether or not it produced a version — max(refreshed_at, recorded check).")),
	f("last_check_at", nullableDateTime.Describe("Timestamp of the explicit recorded check; its outcome may predate a successful refresh.")),
	f("latest_outcome", zString().Nullable().Describe("Outcome at checked_at: updated when the live refresh is newer, otherwise the recorded check outcome.")),
	f("latest_detail", zString().Nullable()),
	f("latest_source_as_of", nullableDateTime),
	f("last_check_outcome", zString().Nullable().Describe("Historical recorded check outcome at last_check_at, not necessarily the latest activity.")),
	f("last_check_detail", zString().Nullable()),
	f("last_check_source_as_of", nullableDateTime),
	f("days_since_source", zInt().NonNegative().Nullable()),
	f("days_since_refresh", zInt().NonNegative().Nullable()),
	f("days_since_check", zInt().NonNegative().Nullable()),
).Strict().Nullable()

// refreshCheckOutcome mirrors the CHECK constraint in
// migrations/020_page_refresh_checks.sql. Closed on purpose: these values are
// scanned by humans and branched on by consumers, and free text would serve
// neither.
var refreshCheckOutcome = zEnum(versions.RefreshCheckOutcomes...).Describe(
	"updated: a version was published. source_not_updated: the source held no coverage this page lacks. " +
		"source_unreachable: the bound source could not be reached or authenticated. blocked: a gate stopped the run " +
		"(schema drift, approval required, missing tool). failed: the run errored.")

// refreshHealthSchema is freshness's verdict-bearing sibling: Pages judges a
// page only against a refresh schedule someone set on it
// (set_page_refresh_schedule), and states which rule it applied. The rules
// are versions.HealthOf's.
var refreshHealthSchema = zObject(
	f("refresh_schedule", zString().Nullable().Describe("Five-field cron, in UTC, that this page's refresh is scheduled at; null when none is set.")),
	f("refresh_owner", zString().Nullable().Describe("Email of the person a missed refresh should reach.")),
	f("health", zEnum(versions.Healths...).Describe(
		"unscheduled: no schedule, so no judgement. blocked: the latest outcome is a recorded failed check (blocked, source_unreachable, failed). "+
			"overdue: a scheduled refresh passed three hours ago with no refresh or check since. source_stale: runs are happening but source_as_of "+
			"is older than the schedule's longest gap plus two days. ok: none of those.")),
	f("health_reason", zString().Describe("One sentence a human can act on; for blocked, the run's own detail.")),
	f("health_since", nullableDateTime.Describe("When the problem started, where known: the failing check, the first missed refresh, or the stale source_as_of.")),
	f("last_due_at", nullableDateTime.Describe("The schedule's most recent fire at or before now.")),
	f("next_due_at", nullableDateTime),
	f("last_activity_at", nullableDateTime.Describe("Latest refresh (on any version, published or not) or recorded check.")),
	f("unscheduled_stale", unscheduledStaleSchema),
	f("days_since_source", zInt().NonNegative().Nullable().Describe("Whole days since the published data's source_as_of; null without managed data.")),
).Strict()

// unscheduledStaleSchema is the hint refresh_health carries beside its
// verdict (versions.UnscheduledStaleAfter): it never changes health and is
// never counted as needing attention.
var unscheduledStaleSchema = zBool().Describe(
	"A hint, not a health: true for a live page with no refresh schedule whose source_as_of is more than 7 days old — a campaign still " +
		"delivering with no refresh task installed, or one that has ended. health stays unscheduled and the page does not need attention.")

var pageSummarySchema = pageSchema.Extend(
	f("is_live", zBool()),
	f("urls", urlsSchema),
	f("freshness", freshnessSchema),
	f("refresh_health", refreshHealthSchema),
).Strict()

var healthCountsSchema = zObject(
	f("blocked", zInt().NonNegative()),
	f("overdue", zInt().NonNegative()),
	f("source_stale", zInt().NonNegative()),
	f("ok", zInt().NonNegative()),
	f("unscheduled", zInt().NonNegative()),
	f("unscheduled_stale", zInt().NonNegative().Describe("How many of the unscheduled pages carry unscheduled_stale (a subset of unscheduled).")),
).Strict()

var refreshHealthRowSchema = zObject(
	f("slug", zString()),
	f("title", zString()),
	f("workspace_id", idOut.Nullable()),
	f("workspace_name", zString().Nullable()),
	f("is_live", zBool()),
	f("refresh_owner", zString().Nullable()),
	f("refresh_schedule", zString().Nullable()),
	f("health", zEnum(versions.Healths...)),
	f("health_reason", zString()),
	f("health_since", nullableDateTime),
	f("last_due_at", nullableDateTime),
	f("source_as_of", nullableDateTime),
	f("last_activity_at", nullableDateTime),
	f("unscheduled_stale", unscheduledStaleSchema),
	f("days_since_source", zInt().NonNegative().Nullable()),
	f("urls", urlsSchema),
).Strict()

var versionSchema = zObject(
	f("id", idOut),
	f("page_id", idOut),
	f("content_sha256", zString()),
	f("status", zEnum("draft", "pending", "approved", "rejected")),
	f("render_mode", zEnum("themed", "raw")),
	f("author", zString().Nullable()),
	f("source", zEnum("api", "mcp", "admin")),
	f("note", zString().Nullable()),
	f("reviewed_by", zString().Nullable()),
	f("reviewed_at", nullableDateTime),
	f("created_at", dateTime),
).Strict()

var versionListSchema = versionSchema.Extend(f("is_published", zBool()), f("is_live", zBool())).Strict()

var versionWithHTMLSchema = versionSchema.Extend(f("html", zString())).Strict()

var workspaceSchema = zObject(
	f("id", idOut),
	f("name", zString().Min(1).Max(100)),
	f("page_count", zInt().Min(0)),
	f("created_at", dateTime),
	f("updated_at", dateTime),
).Strict()

var themeSchema = zObject(f("id", idOut), f("name", zString()), f("default_mode", zString())).Strict()

// jsonObjectSchema: JSON-RPC has already constrained these values to JSON.
// Keep the MCP schema object-rooted while allowing each page's embedded JSON
// Schema to own the actual property contract.
var jsonObjectSchema = zRecord(zUnknown())

var dataEnvelopeSchema = zObject(
	f("contract_version", zLiteral(float64(1))),
	f("refreshed_at", dateTime),
	f("source_as_of", dateTime),
	f("data", jsonObjectSchema),
).Strict()

var templateName = zString().Min(1).Max(templates.MaxNameChars).Describe("Template name, for example 'nwm-campaign-dashboard'. No slashes.")

var templateSchema = zObject(
	f("id", idOut),
	f("name", zString()),
	f("title", zString()),
	f("description", zString()),
	f("current_revision", zInt().Positive().Nullable()),
	f("current_version_id", idOut.Nullable()),
	f("created_at", dateTime),
	f("updated_at", dateTime),
).Strict()

var templateRevisionSchema = zObject(
	f("version_id", idOut),
	f("revision", zInt().Positive()),
	f("content_sha256", sha256Schema),
	f("config_schema_sha256", sha256Schema),
	f("data_schema_sha256", sha256Schema),
	f("author", zString()),
	// MUST stay in step with templates.Sources. "cli" is a file-backed
	// registration by an operator; it was once added to the domain enum
	// without being added here, so every template registered by `pages
	// template sync` made get_template and list_template_revisions fail
	// output validation with -32602. A test asserts the two sets are equal.
	f("source", zEnum(templates.Sources...)),
	f("note", zString().Nullable()),
	// Whether the revision carries a preview-only example dataset. The rows
	// are never returned. This schema is strict, so a field added to the
	// revision row and not here fails output validation on three tools; a
	// test pins the two together.
	f("has_sample_data", zBool()),
	f("created_at", dateTime),
).Strict()

// dataProfileFieldSchema is what the payload CONTAINS, alongside the hashes
// that say what it IS. A schema validates shape; #102 shipped two
// schema-perfect payloads to a client (a 24-day coverage gap and a dropped
// deal worth ~7% of spend) because nothing reported the contents.
var dataProfileFieldSchema = zObject(
	f("kind", zEnum("number", "date", "key", "text")),
	f("sum", zNumber().Nullable().Optional()),
	f("min", zUnion(zNumber(), zString()).Nullable().Optional()),
	f("max", zUnion(zNumber(), zString()).Nullable().Optional()),
	f("distinct", zInt().NonNegative().Optional()),
	f("distinct_overflow", zBool().Optional()),
	f("values", zRecord(zInt().NonNegative()).Optional()),
	f("values_omitted", zInt().NonNegative().Optional()),
	f("nulls", zInt().NonNegative()),
).Strict()

var dataProfileSchema = zObject(
	f("arrays", zRecord(zObject(f("count", zInt().NonNegative()), f("fields", zRecord(dataProfileFieldSchema))).Strict())),
	f("scalars", zRecord(zUnion(zString(), zNumber(), zBool(), zNull()))),
).Strict()

var pageDataPageBriefSchema = zObject(
	f("slug", zString()), f("title", zString()), f("disabled", zBool()), f("require_approval", zBool()),
).Strict()

var pageDataExportsSchema = zObject(
	f("version_id", idOut.Describe("The immutable version these URLs serve: the live version when they were minted.")),
	f("schema_url", zString().URL().Describe("Canonical JSON of the data schema; its SHA-256 is schema_sha256.")),
	f("data_url", zString().URL().Describe("Canonical JSON of envelope.data; its SHA-256 is data_sha256.")),
	f("envelope_url", zString().URL().Describe("Canonical JSON of the complete envelope, including data.")),
	f("expires_at", dateTime),
).Strict()

// get_page_data answers three questions, and a refresh run asks the cheap one
// on every invocation. The shapes differ, but MCP requires an object-rooted
// output schema, so fields specific to one read mode are optional in the
// advertised schema and required or forbidden by refinePageDataMode. A full
// read (no detail) validates exactly as it did before detail existed.
var pageDataModeFields = map[string]struct{ required, forbidden []string }{
	"full": {
		required:  []string{"schema", "version", "version_is_live", "page_is_live", "urls", "data_profile", "freshness"},
		forbidden: []string{"coverage_profile", "coverage_truncated", "exports", "next_step", "refresh_health"},
	},
	"summary": {
		required:  []string{"coverage_profile", "freshness", "next_step"},
		forbidden: []string{"schema", "version", "version_is_live", "page_is_live", "urls", "data_profile", "data_omitted", "exports"},
	},
	"export": {
		required:  []string{"exports", "next_step"},
		forbidden: []string{"schema", "version", "version_is_live", "page_is_live", "urls", "data_profile", "data_omitted", "coverage_profile", "coverage_truncated", "freshness", "refresh_health"},
	},
}

func refinePageDataMode(v *jsonv.Object, add func(path []any, message string)) {
	mode, _ := v.Value("detail").(string)
	if mode == "" {
		mode = "full"
	}
	fields := pageDataModeFields[mode]
	for _, key := range fields.required {
		if !v.Has(key) {
			add([]any{key}, "a "+mode+" read includes "+key)
		}
	}
	for _, key := range fields.forbidden {
		if v.Has(key) {
			add([]any{key}, "a "+mode+" read omits "+key)
		}
	}
	if env, ok := v.Value("envelope").(*jsonv.Object); ok && mode != "full" && env.Has("data") {
		add([]any{"envelope", "data"}, "a "+mode+" read never carries envelope.data")
	}
	page, _ := v.Value("page").(*jsonv.Object)
	if (mode == "full") != (page != nil && page.Has("id")) {
		which := "brief"
		if mode == "full" {
			which = "complete"
		}
		add([]any{"page"}, "a "+mode+" read returns the "+which+" page")
	}
}

var dataWarningSchema = zObject(
	f("code", zEnum(
		"row_count_dropped",
		"coverage_start_regressed",
		"coverage_end_regressed",
		"dimension_values_missing",
		"coverage_unverified",
		// "This refresh added nothing": a real new version whose numbers
		// did not move. Suppressed on a dedupe.
		"data_unchanged",
		"coverage_did_not_advance",
		// Emitted only when the list itself was capped.
		"warnings_truncated",
	)),
	f("path", zString()),
	f("message", zString()),
	f("previous", zUnion(zNumber(), zString(), zArray(zString()))),
	f("current", zUnion(zNumber(), zString(), zArray(zString()))),
).Strict()

// dataUsageSchema reports how close a payload is to the managed-data limit
// (issue #109), so a refresh run can see its headroom instead of discovering
// the wall.
var dataUsageSchema = zObject(
	f("payload_bytes", zInt().NonNegative().Describe("UTF-8 bytes of the compact payload (JSON.stringify(data)).")),
	f("envelope_bytes", zInt().NonNegative().Describe("UTF-8 bytes of the stored HTML-escaped envelope (estimated with minimal timestamps where no envelope was written).")),
	f("limit_bytes", zInt().Positive().Describe("The managed-data limit both of those must fit (PAGES_DATA_MAX_BYTES).")),
	f("headroom_bytes", zInt().Describe("limit_bytes minus the larger of payload_bytes and envelope_bytes.")),
).Strict().Describe("Payload size against the managed-data limit (issue #109). Plan the next refresh against headroom_bytes; do NOT split, sample or summarize a payload to fit.")

func pageDataStateShape() []field {
	return []field{
		f("version", versionSchema),
		f("envelope", dataEnvelopeSchema),
		f("data_sha256", zString().Regex(`^[0-9a-f]{64}$`)),
		f("schema_sha256", zString().Regex(`^[0-9a-f]{64}$`)),
		f("template_sha256", zString().Regex(`^[0-9a-f]{64}$`)),
		f("version_is_live", zBool()),
		f("page_is_live", zBool()),
		f("live_version_id", idOut.Nullable()),
		f("urls", urlsSchema),
	}
}

func annotations(readOnly, destructive, idempotent bool) *jsonv.Object {
	return jsonv.ObjectOf(
		"readOnlyHint", readOnly,
		"destructiveHint", destructive,
		"idempotentHint", idempotent,
		"openWorldHint", false,
	)
}

var (
	readAnnotations                  = annotations(true, false, true)
	writeAnnotations                 = annotations(false, true, false)
	idempotentWriteAnnotations       = annotations(false, true, true)
	additiveAnnotations              = annotations(false, false, false)
	idempotentAdditiveAnnotations    = annotations(false, false, true)
	idempotentNonDestructiveAnnotate = annotations(false, false, true)
)

// preflightFindingSchema: preflight findings are additive and optional on
// every deploy result — an agent that ignores the field behaves exactly as
// before, and one that reads it learns its dashboard is broken BEFORE the
// client does.
var preflightFindingSchema = zObject(
	f("code", zString()),
	f("message", zString()),
	f("fix", zString().Optional()),
	f("element", zString().Optional()),
	f("attribute", zString().Optional()),
	f("identifier", zString().Optional()),
	f("snippet", zString().Optional()),
	f("host", zString().Optional()),
	f("directive", zString().Optional()),
	f("api", zString().Optional()),
	f("apis", zArray(zString()).Optional()),
	f("required_token", zString().Optional()),
	f("script_index", zInt().Optional()),
	f("contract_code", zString().Optional().Describe("The managed-data contract error code, on managed_block_invalid.")),
).Strict()

var preflightSchema = zObject(
	f("ok", zBool().Describe("True when no errors were found. Warnings do not clear it.")),
	f("render_mode", zString()),
	f("errors", zArray(preflightFindingSchema)),
	f("warnings", zArray(preflightFindingSchema)),
	f("errors_omitted", zInt().NonNegative()),
	f("warnings_omitted", zInt().NonNegative()),
	f("checks", zArray(zString())),
	f("summary", zString()),
).Strict()

var deployOutputSchema = zObject(
	f("version", versionSchema),
	f("deduped", zBool()),
	f("published", zBool()),
	f("gated", zBool()),
	f("live", zBool().Describe("Compatibility alias for version_is_live.")),
	f("version_is_live", zBool()),
	f("page_is_live", zBool()),
	f("live_version_id", idOut.Nullable()),
	f("next_step", zString()),
	f("urls", urlsSchema),
	f("config_schema_generated", zBool().Optional().Describe(
		"Present only when this page shipped a #pages-config block with no #pages-config-schema, so Pages derived one from the values and wrote it into the stored document. It pins types and rejects unknown keys; it does not know your enums, formats or which keys are genuinely optional. Replace it by hand if this design becomes a family.")),
	f("preflight", preflightSchema.Optional().Describe(
		"Static check of the deployed document against the content host's CSP and sandbox. Advisory only — the deploy already happened. Errors mean parts of the page will not work in a browser.")),
).Strict()

var uploadID = zString().UUID().Describe("Opaque upload handle returned by start_page_upload.")

var uploadReplaySchema = zLiteral(true).Optional().Describe(
	"Present on an exact committed retry. Commit receipt fields describe the original operation; live-state fields and next_step are refreshed for this reply. The mutation was not repeated.")

// dataExpectSchema is what a caller believes its payload contains, computed
// from the SOURCE in the same pass that built it. Shared by both
// managed-data writes: the inline and staged routes must not be able to
// drift apart on what may be asserted.
var dataExpectSchema = zObject(
	f("row_count", zRecord(zInt().NonNegative()).Optional().Describe(`Keyed by array path, e.g. {"rows": 124}.`)),
	f("totals", zRecord(zNumber()).Optional().Describe(`Keyed by field path, e.g. {"rows.spend": 1500.25}.`)),
	// A length-2 array rather than a tuple: tools/list would emit a tuple as
	// draft-07 `items: [...]`, which Anthropic's draft 2020-12 validator
	// rejects, failing every Claude turn that has Pages connected. [min,
	// max] is checked at use.
	f("date_range", zRecord(zArray(zString()).Length(2)).Optional().Describe(
		`Keyed by field path, e.g. {"rows.date": ["2026-07-06", "2026-08-05"]}. Exact match.`)),
	f("tolerance", zNumber().Min(0).Max(1).Optional().Describe("Relative slack on totals only. Defaults to 0.0001 (0.01%), enough for float-order drift.")),
).Strict().Describe(
	"What you believe the payload contains, computed from the SOURCE in the same pass that built it. Verified against the server's own profile BEFORE anything is written; a mismatch fails with data_reconciliation_failed and changes nothing. Paths are the paths data_profile reports.")

var dataLimitKeys = []string{"max_payload_bytes", "max_envelope_bytes", "max_inline_bytes", "max_staged_bytes", "max_request_bytes", "min_envelope_overhead_bytes"}

func dataLimitsSchema() *Schema {
	fields := make([]field, len(dataLimitKeys))
	for i, k := range dataLimitKeys {
		fields[i] = f(k, zInt().Positive())
	}
	return zObject(fields...).Strict()
}

var dataSizeSchema = zObject(
	f("payload_bytes", zInt().NonNegative().Max(maxSafeInteger)),
	f("escaped_payload_bytes", zInt().NonNegative().Max(maxSafeInteger)),
).Strict().Describe("Advisory UTF-8 sizes of compact JSON and HTML-escaped compact JSON; only for kind data. Actual content is always validated.")

var uploadStateSchema = zObject(
	f("upload_id", uploadID),
	// An upload targets a page's HTML, a template, or a page's managed
	// DATA. 'page' and 'data' both name a slug; 'template' names a
	// template. The field that does not apply is null.
	f("target_kind", zEnum("page", "template", "data")),
	f("slug", zString().Nullable()),
	f("template", zString().Nullable()),
	f("content_sha256", sha256Schema),
	f("total_bytes", zInt().Positive()),
	f("bytes_received", zInt().NonNegative()),
	f("next_sequence", zInt().NonNegative()),
	f("max_chunk_bytes", zInt().Positive()),
	f("data_limits", dataLimitsSchema().Optional()),
	f("complete", zBool()),
	f("expires_at", dateTime),
	f("next_step", zString()),
).Strict()

var publishOutputSchema = zObject(
	f("version", versionSchema),
	f("live", zBool().Describe("Compatibility alias for version_is_live.")),
	f("version_is_live", zBool()),
	f("page_is_live", zBool()),
	f("live_version_id", idOut.Nullable()),
	f("next_step", zString()),
	f("urls", urlsSchema),
).Strict()

const (
	renderModeDescription = "Defaults to themed. A Bento deck (.bento.html) defaults to raw and is refused as themed: it is its own application."
	titleIfCreated        = "Used only if the page is created. PLAIN TEXT — Pages escapes it wherever it renders, so pass \"Contoso & Fabrikam\", never \"Contoso &amp; Fabrikam\"; a pre-escaped title shows its own entities."
)

// sourceBindingSchema is one source binding: which connector serves an input
// the request names. The vocabulary matches the `workflow.sources` shape
// client bundles already author. Credential-free by construction (names
// only) and re-screened by updateprompts.AssertCredentialFree.
var sourceBindingSchema = zObject(
	f("source_id", zString().Min(1).Max(120).Describe("Stable identifier for this input, e.g. ix_ssp or amazon_dsp.")),
	f("mcp_server", zString().Min(1).Max(120).Describe("MCP server name that serves it, e.g. indexexchange_mcp.")),
	f("account", zString().Min(1).Max(120).Optional().Describe("Logical connector account/variant name; omit for the default seat.")),
	f("required_tools", zArray(zString().Min(1).Max(200)).Min(1).Max(40).Optional().Describe("Read-only tool names this source must be retrieved through.")),
	f("path", zString().Min(1).Max(500).Optional().Describe("Exact location in the source system, e.g. northwind/contoso/daily. Beats searching for a filename.")),
	f("partition", zObject(
		f("by", zLiteral("date").Describe("Only date partitioning is modelled today.")),
		f("format", zString().Min(1).Max(40).Optional().Describe("Partition folder/file format, e.g. YYYY-MM-DD.")),
		f("since", zString().Min(1).Max(40).Optional().Describe("Inclusive lower bound, or source_as_of to continue from the page.")),
		f("until", zString().Min(1).Max(40).Optional().Describe("Inclusive upper bound; omit for the newest available.")),
	).Strict().Optional().Describe(
		"Set when this source is a date-partitioned folder (one file per day). The prompt then requires enumerating EVERY partition in range — the default newest-wins pick silently publishes a single day of a multi-week dataset.")),
	f("retrieval_instructions", zString().Min(1).Max(2000).Optional().Describe("Deterministic, bounded retrieval detail for this source (window, filters, expected artifacts).")),
).Strict()

var guideTopic = zEnum(agentguide.TopicNames...)
