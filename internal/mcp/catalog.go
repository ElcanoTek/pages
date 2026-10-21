// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// The typed Pages tool registry (lib/mcp-tools.js TOOLS). Every tool is a
// thin, validated adapter over the same versions/workspaces/templates state
// machines REST and the admin UI use; the handlers live in tools_*.go.
//
// Some advertised numbers are limits the operator can set (upload sizes,
// chunk size, the data budgets). They are read when the catalog is built, and
// the catalog is rebuilt whenever they change (see catalogFor).

import (
	"context"
	"strconv"
	"sync"

	"github.com/ElcanoTek/pages/internal/agentguide"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/pagepatch"
	"github.com/ElcanoTek/pages/internal/updateprompts"
	"github.com/ElcanoTek/pages/internal/uploads"
)

// handlerFunc runs one tool with parsed arguments.
type handlerFunc func(ctx context.Context, c *call, args *jsonv.Object) (any, error)

// toolDef is one registry entry.
type toolDef struct {
	name        string
	title       string
	description string
	input       *Schema
	output      *Schema
	annotations *jsonv.Object
	handler     handlerFunc
	// renderText renders the structured result as prose for the text
	// content block (get_guide only); nil keeps the serialized JSON.
	renderText func(structured any) string
}

// limits is every limit-dependent number the catalog advertises.
type limits struct {
	maxUploadBytes      int
	maxChunkBase64Chars int
	maxInlineHTMLBytes  int
	dataLimitGuidance   string
	pageUploadGuidance  string
	dataUploadGuidance  string
	patchMaxEdits       int
	patchMaxFindChars   int
	patchMaxReplace     int
	patchMaxCount       int
	findMaxMatches      int
}

// DefaultMaxInlineHTMLBytes is PAGES_MCP_MAX_INLINE_HTML_BYTES' default.
const DefaultMaxInlineHTMLBytes = 20000

// MaxInlineHTMLBytes is the ceiling on html passed inline to deploy_page and
// update_page (PAGES_MCP_MAX_INLINE_HTML_BYTES, default 20000). Four tool
// descriptions have always said "for content over 20,000 UTF-8 bytes, use
// the staged page-upload tools instead" — and nothing enforced it, so a
// caller that ignored the advice sent 43 KB of HTML inline and Pages took
// it. That is the single most expensive thing an agent can do here, and not
// because of the one call: those bytes then sit in the conversation and are
// re-sent on every subsequent step. So the ceiling is real: refusing is
// kinder than accepting, because the error names the two cheap paths.
func MaxInlineHTMLBytes() int {
	n := env.Int("PAGES_MCP_MAX_INLINE_HTML_BYTES", DefaultMaxInlineHTMLBytes)
	if n <= 0 {
		return DefaultMaxInlineHTMLBytes
	}
	return n
}

func currentLimits() limits {
	return limits{
		maxUploadBytes:      uploads.MaxUploadBytes(),
		maxChunkBase64Chars: uploads.MaxChunkBase64Chars(),
		maxInlineHTMLBytes:  MaxInlineHTMLBytes(),
		dataLimitGuidance:   datalimits.DataLimitGuidance(),
		pageUploadGuidance:  datalimits.FileUploadGuidance("page", ""),
		dataUploadGuidance:  datalimits.FileUploadGuidance("data", ""),
		patchMaxEdits:       pagepatch.MaxEdits,
		patchMaxFindChars:   pagepatch.MaxFindChars,
		patchMaxReplace:     pagepatch.MaxReplaceChars,
		patchMaxCount:       pagepatch.MaxExpectedCount,
		findMaxMatches:      pagepatch.MaxMatches,
	}
}

// catalog is a built registry plus its serialized forms.
type catalog struct {
	limits limits
	tools  []*toolDef
	byName map[string]*toolDef
	// instructions for broad and data_update tokens.
	instructions, dataUpdateInstructions string
}

var (
	catalogMu    sync.Mutex
	catalogCache *catalog
)

// catalogFor returns the registry for the limits in force now, building it
// only when they changed.
func catalogFor(l limits) *catalog {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if catalogCache != nil && catalogCache.limits == l {
		return catalogCache
	}
	c := buildCatalog(l)
	catalogCache = c
	return c
}

func currentCatalog() *catalog { return catalogFor(currentLimits()) }

// groupedInt formats n with thousands separators ("20,000").
func groupedInt(n int) string {
	s := strconv.Itoa(n)
	neg := false
	if n < 0 {
		neg, s = true, s[1:]
	}
	var out []byte
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// usageDescription is the #109 sentence the managed-data tools carry.
const usageDescription = " The response also reports data_usage: payload_bytes, envelope_bytes, limit_bytes and headroom_bytes against the managed-data limit, so a run can see its headroom before the next refresh."

func buildCatalog(l limits) *catalog {
	inlineBytes := groupedInt(l.maxInlineHTMLBytes)
	deployInputShape := []field{
		f("slug", slugSchema),
		// The HTTP boundary enforces MAX_HTML_BYTES on the complete JSON
		// body. Do not advertise a character limit here: UTF-8 width and
		// JSON overhead make a character count a different, misleading
		// contract.
		f("html", zString().Min(1).Describe(
			"Inline page HTML or fragment. For a workspace file or content over "+inlineBytes+" UTF-8 bytes, do not inline it. "+l.pageUploadGuidance)),
		f("render_mode", zEnum("themed", "raw").Optional().Describe(renderModeDescription)),
		f("note", zString().Max(500).Optional()),
		f("publish", zBool().Optional().Describe("Publish immediately on open pages; defaults to true.")),
		f("expected_version", idArg.Optional().Describe("Optimistic-concurrency check against the current published version.")),
	}
	createdOnly := []field{
		f("title", zString().Max(200).Optional().Describe(titleIfCreated)),
		f("require_approval", zBool().Optional().Describe("Used only if the page is created.")),
		f("client_id", zString().Max(200).Optional().Describe("Used only if the page is created.")),
	}
	uploadDeployInputShape := append([]field{
		f("upload_id", uploadID),
		f("render_mode", zEnum("themed", "raw").Optional().Describe(renderModeDescription)),
		f("note", zString().Max(500).Optional()),
		f("publish", zBool().Optional().Describe("Publish immediately on open pages; defaults to true.")),
		f("expected_version", idArg.Optional().Describe("Optimistic-concurrency check against the current published version.")),
	}, createdOnly...)

	prepareInput := zObject(
		f("slug", slugSchema),
		f("instructions", zString().Min(1).Max(updateprompts.MaxInstructions).Describe(
			"The user's exact data/source/design request in plain language; describe credential locations, never secret values. For recurring=true describe the data work of one run (sources, scope, mappings, metrics), not when or how it is scheduled: scheduling and prompt-creation sentences are removed and echoed back as instructions_removed.")),
		f("recurring", zBool().Optional().Describe(
			"False/default: execute once now. True: return the task prompt for a recurring job; whoever schedules the job sets its cadence, and Pages does not schedule it.")),
		f("update_type", zEnum("auto", "data", "layout").Optional().Describe(
			"Choose data for values only, layout for design/schema/JavaScript, or auto when the request genuinely needs classification.")),
		f("publish", zBool().Optional().Describe("Whether the eventual update should publish; defaults true.")),
		f("sources", zArray(sourceBindingSchema).Min(1).Max(updateprompts.MaxSources).Optional().Describe(
			"Exact source bindings: which connector serves each input, where the data sits, and whether it is date-partitioned. REQUIRED when recurring is true — an unattended run executed weeks later cannot safely re-derive bindings from prose. Optional for a one-time update, but still the difference between a pinned source and a guess. Names only, never secret values.")),
	).Strict()

	prepareOutput := zObject(
		f("page", pageSchema),
		f("mode", zEnum("adaptive", "managed_data", "managed_template", "full_page", "migration_required")),
		f("recurring", zBool()),
		f("prompt", zString()),
		f("prompt_sha256", sha256Schema),
		f("schema_sha256", sha256Schema.Nullable()),
		f("sources", zArray(sourceBindingSchema).Nullable()),
		f("instructions_removed", zArray(zString()).Describe(
			"Recurring only: scheduling and prompt-creation prose removed from instructions before quoting them as USER REQUEST.")),
		// What a run of this prompt needs, in a shape a scheduler can
		// validate before accepting the task rather than discovering at
		// dispatch.
		f("execution_requirements", zObject(
			f("mcp_servers", zArray(zString())),
			f("required_tools", zArray(zString())),
			f("roster", zLiteral("required_tools_only").Optional().Describe(
				"Recurring managed data with fully bound sources only: offer the run only required_tools; every tool a branch may call is listed.")),
			f("completion", zObject(
				f("any_succeeded", zArray(zString()).Min(1)),
				f("blocked_when", zObject(
					f("tool", zString()), f("argument", zString()), f("in", zArray(zString()).Min(1)), f("detail_argument", zString().Optional()),
				).Strict().Optional().Describe(
					"The run finished without publishing when its last successful call of `tool` had `argument` set to one of `in` and no other completion tool succeeded; `detail_argument` names that call's argument holding the reason.")),
			).Strict().Optional().Describe(
				"Recurring managed data only: the run is complete when any one of these tools succeeded: a committed version or a recorded check.")),
			f("serialization_key", zString().Optional().Describe(
				"pages:<slug>. Runs sharing it should not overlap; give every schedule for this page the same key.")),
			f("network", zBool()),
			f("model_required", zBool()),
			f("mode", zString()),
		).Strict()),
		f("live_version_id", idOut),
		f("page_is_live", zBool()),
		f("next_step", zString()),
	).Strict()

	configureInput := zObject(
		f("slug", slugSchema),
		f("instructions", zString().Min(1).Max(updateprompts.MaxInstructions).Optional().Describe("Preferred: the user's exact plain-language request.")),
		f("recurring", zBool().Optional()),
		f("update_type", zEnum("auto", "data", "layout").Optional()),
		f("publish", zBool().Optional()),
		f("daily_at_utc", zString().Regex(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`).Optional().Describe(
			"Legacy compatibility only; Pages does not install this cadence.")),
		f("workflow", jsonObjectSchema.Optional().Describe("Legacy credential-free source workflow converted into user-owned prompt text.")),
		f("run_now", zBool().Optional().Describe("Legacy compatibility only; ignored because prompt preparation never executes work.")),
	).Strict()

	uploadTargetFields := func(kindDescription string) []field {
		return []field{
			f("slug", slugSchema.Optional().Describe("The page this upload targets. Supply exactly one of slug or template.")),
			f("template", templateName.Optional().Describe(
				"Register a reusable template instead of deploying a page. Consume with register_template_upload.")),
			f("kind", zEnum("page", "data").Optional().Describe(kindDescription)),
			f("data_size", dataSizeSchema.Optional()),
			f("total_bytes", zInt().Min(1).Max(float64(l.maxUploadBytes))),
			f("content_sha256", sha256Schema.Describe("Lowercase SHA-256 of the exact original UTF-8 file bytes.")),
		}
	}

	hardcodedValues := zArray(zObject(f("path", zString()), f("value", zString()), f("occurrences", zInt().Positive())).Strict()).Nullable().Describe(
		"Config values that are ALSO written into the design outside the config block — a <title>, a heading, a legend. Moving a value into #pages-config only makes it vary per instance if the design reads it from there, so every page built from this would show the hardcoded copy. Advisory: fix them with patch_page, or ignore any that genuinely belong to the design.")

	dataUsage := dataUsageSchema.Optional()

	tools := []*toolDef{
		{
			name:  "list_pages",
			title: "List Pages",
			description: "List active Pages with workspace, theme, client-access, serving state, and routing URLs. Supports bounded keyset pagination and filters; workspace_id:null selects Ungrouped.\n\n" +
				"Each row carries freshness: source_as_of, refreshed_at, checked_at, and days_since_* for each. That is how you find dashboards that have quietly stopped updating — Pages states no opinion on what counts as overdue, because it does not know any page's expected cadence.",
			input: zObject(
				f("query", zString().Min(1).Max(200).Optional()),
				f("workspace_id", nullableIDArg.Optional()),
				f("client_id", zString().Nullable().Optional()),
				f("is_live", zBool().Optional()),
				f("require_approval", zBool().Optional()),
				f("disabled", zBool().Optional()),
				f("limit", pageLimit),
				f("cursor", cursorSchema.Optional()),
			).Strict(),
			output:      zObject(f("pages", zArray(pageSummarySchema)), f("next_cursor", zString().Nullable())).Strict(),
			annotations: readAnnotations,
			handler:     listPages,
		},
		{
			name:        "get_page",
			title:       "Get Page",
			description: "Get page metadata, workspace/theme, routing URLs, serving state, and published-version metadata. HTML is omitted unless include_html is true.",
			input:       zObject(f("slug", slugSchema), f("include_html", zBool().Optional())).Strict(),
			output: zObject(
				f("page", pageSchema),
				f("published", zUnion(versionSchema, versionWithHTMLSchema).Nullable()),
				f("is_live", zBool()),
				f("urls", urlsSchema),
			).Strict(),
			annotations: readAnnotations,
			handler:     getPage,
		},
		{
			name:  "get_page_data",
			title: "Get Managed Page Data",
			description: "Read the currently published managed-data contract. Use its live_version_id as update_page_data.expected_version. Fails with page_not_data_managed unless the published HTML has exactly one pages-data-schema block and one pages-data block.\n\n" +
				"detail picks how much comes back. 'summary' (~1–2 KB): brief page flags, live_version_id, the three hashes, envelope stamps, freshness and a coverage profile (per-array row counts, date field min/max/distinct, date-stamped scalars). Read it first; it is enough to decide whether a refresh is due. 'export': short-lived URLs for the schema, data and envelope of the live version, for a client that downloads URLs into workspace files host-side; the bytes then never enter your context, and sha256 of the schema/data files equals schema_sha256/data_sha256. 'full' (default): today's complete answer — schema, envelope with data, full data_profile, URLs and live state. Read the schema or rows once per run, by export where you can; do not re-read after a compaction.\n\n" +
				"freshness.source_as_of is the coverage watermark to compare your source against before refreshing: refresh when the source holds a period this page does not already represent, not because a file's timestamp moved." +
				usageDescription,
			input: zObject(
				f("slug", slugSchema),
				f("detail", zEnum("summary", "full", "export").Optional().Describe(
					"Default 'full'. 'summary' for the compact contract read; 'export' for download URLs instead of inline schema/data.")),
				f("include_data", zBool().Optional().Describe(
					"Full reads only. Default true. False returns the schema, hashes, profile, live state and coverage without envelope.data.")),
			).Strict(),
			output: zObject(
				f("detail", zEnum("summary", "export").Optional().Describe("Absent on a full read.")),
				f("page", zUnion(pageSchema, pageDataPageBriefSchema)),
				f("version", versionSchema.Optional()),
				f("schema", jsonObjectSchema.Optional()),
				f("envelope", dataEnvelopeSchema.Partial("data")),
				f("data_sha256", sha256Schema),
				f("schema_sha256", sha256Schema),
				f("template_sha256", sha256Schema),
				f("version_is_live", zBool().Optional()),
				f("page_is_live", zBool().Optional()),
				f("live_version_id", idOut.Nullable()),
				f("urls", urlsSchema.Optional()),
				f("data_omitted", zLiteral(true).Optional()),
				f("data_profile", dataProfileSchema.Optional()),
				f("coverage_profile", dataProfileSchema.Optional().Describe(
					"Summary reads: data_profile reduced to row counts, date fields and date-stamped scalars.")),
				f("coverage_truncated", zLiteral(true).Optional().Describe(
					"Summary reads: coverage_profile is incomplete (size bound, a clipped extent, or the profile's array/field/scalar/depth limits); the exported data file is complete.")),
				f("freshness", freshnessSchema.Optional()),
				f("exports", pageDataExportsSchema.Optional()),
				f("next_step", zString().Optional()),
				f("data_usage", dataUsage),
			).Strict().SuperRefine(refinePageDataMode),
			annotations: readAnnotations,
			handler:     getPageData,
		},
		// Compatibility guidance for the static Pages allowlists already
		// deployed in Chat and Cutlass. It intentionally exposes no
		// historical schedule or dispatch state.
		{
			name:        "get_page_refresh",
			title:       "Get Dashboard Scheduling Guidance",
			description: "Compatibility read for clients that still ask about a Pages refresh. Pages no longer stores or dispatches recurring schedules. Read the exact existing page, then call prepare_dashboard_update—or configure_page_refresh on an older static allowlist—with the user's instructions and recurring=true. Show that returned prompt to the user for their own scheduler.",
			input:       zObject(f("slug", slugSchema)).Strict(),
			output: zObject(
				f("page", pageSchema),
				f("scheduling", zLiteral("user_owned")),
				f("refresh", zNull()),
				f("runs", zArray(zNever()).Max(0)),
				f("next_step", zString()),
			).Strict(),
			annotations: readAnnotations,
			handler:     getPageRefresh,
		},
		// The counterpart to freshness: a refresh that ran correctly and
		// published nothing writes nothing anywhere, so "we check daily and
		// the upstream is dead" reads exactly like "nobody has run this in
		// three weeks".
		{
			name:  "record_refresh_check",
			title: "Record Refresh Check",
			description: "Record that a refresh looked at this page and what it concluded, WITHOUT creating a version. Call it when a run ends without publishing — the source had no new coverage, a required tool was unavailable, a gate stopped it. It moves freshness.checked_at only: the published pointer, the data, and every hash are untouched, so the page keeps serving exactly what it was serving.\n\n" +
				"This is what separates a dashboard whose upstream has stopped producing from one nobody is running any more. Those look identical otherwise and need different people to act. A run that DID publish does not need this — update_page_data already stamps the page.",
			input: zObject(
				f("slug", slugSchema),
				f("outcome", refreshCheckOutcome),
				f("detail", zString().Max(500).Optional().Describe(
					"One line a human can act on, e.g. 'upstream max date still 2026-07-02, unchanged for 6 days'.")),
				f("source_as_of_seen", dateTime.Optional().Describe(
					"The latest coverage the SOURCE offered, when you could determine it. Compared against the page's own source_as_of, this is what shows an upstream has frozen rather than the job.")),
			).Strict(),
			output: zObject(f("slug", zString()), f("freshness", freshnessSchema)).Strict(),
			// Idempotent: recording the same conclusion twice leaves the
			// same state.
			annotations: idempotentNonDestructiveAnnotate,
			handler:     recordRefreshCheck,
		},
		{
			name:        "prepare_dashboard_update",
			title:       "Prepare Dashboard Update",
			description: "Prepare a safe exact-slug update prompt from a user's plain-language request. Call this whenever a user says 'update <slug> dashboard with ...'. For a one-time request, follow the returned prompt now. With recurring=true, the returned prompt is the verbatim task prompt of a recurring job (its cadence belongs to the job, not the prompt); Pages never schedules or dispatches it. update_type=data uses the managed-data contract, layout uses an existing-page staged deployment, and auto supplies bounded routing guidance. This tool is read-only and never changes a page.",
			input:       prepareInput,
			output:      prepareOutput,
			annotations: readAnnotations,
			handler:     prepareDashboardUpdateTool,
		},
		{
			name:        "find_in_version",
			title:       "Find In Page Version",
			description: "Literal (non-regex) search inside a stored version, returning bounded line numbers and short excerpts instead of the document. Use this to locate the exact anchor text for patch_page without reading a whole dashboard back into context. Defaults to the published version. Read-only.",
			input: zObject(
				f("slug", slugSchema),
				f("query", zString().Min(1).Max(float64(l.patchMaxFindChars)).Describe("Exact substring to look for; not a regular expression.")),
				f("version_id", idArg.Optional().Describe("Defaults to the currently published version.")),
				f("max_matches", zInt().Min(1).Max(float64(l.findMaxMatches)).Optional()),
				f("ignore_case", zBool().Optional()),
			).Strict(),
			output: zObject(
				f("version_id", idOut),
				f("total_matches", zInt().NonNegative()),
				f("matches_omitted", zInt().NonNegative()),
				f("matches", zArray(zObject(f("offset", zInt().NonNegative()), f("line", zInt().Positive()), f("excerpt", zString())).Strict())),
			).Strict(),
			annotations: readAnnotations,
			handler:     findInVersion,
		},
		{
			name:        "patch_page",
			title:       "Patch Page",
			description: "Deploy a new version by applying small anchored find/replace edits to the CURRENT published HTML, server-side. Use this for targeted changes — a CSS rule, a handler name, a label — instead of re-uploading the whole document. `find` is a literal string, not a regex, and must match exactly `count` times (default 1) or the whole patch is rejected. Locate anchors with find_in_version. By default it patches the live version and is concurrency-checked against the live pointer, so a patch cannot clobber a deploy that landed between the read and the write. `base_version_id` patches a DIFFERENT version instead — and publishing that republishes its bytes, discarding whatever was deployed since, so pass `expected_version` (or `publish:false`) when you rebase off an older version deliberately.",
			input: zObject(
				f("slug", slugSchema),
				f("edits", zArray(zObject(
					f("find", zString().Min(1).Max(float64(l.patchMaxFindChars)).Describe("Exact literal text to replace, whitespace included.")),
					f("replace", zString().Max(float64(l.patchMaxReplace))),
					f("count", zInt().Min(1).Max(float64(l.patchMaxCount)).Optional().Describe("Required number of occurrences; defaults to 1.")),
				).Strict()).Min(1).Max(float64(l.patchMaxEdits)).Describe("Applied in order; each sees the result of the previous.")),
				f("base_version_id", idArg.Optional().Describe("Patch this version instead of the published one.")),
				f("render_mode", zEnum("themed", "raw").Optional().Describe("Defaults to the base version's mode.")),
				f("note", zString().Max(500).Optional()),
				f("publish", zBool().Optional().Describe("Publish immediately on open pages; defaults to true.")),
				f("expected_version", idArg.Optional().Describe("Defaults to the page's current live version, not to base_version_id.")),
			).Strict(),
			output: deployOutputSchema.Extend(f("patch", zObject(
				f("base_version_id", idOut),
				f("bytes_before", zInt().NonNegative()),
				f("bytes_after", zInt().NonNegative()),
				f("edits_applied", zArray(zObject(
					f("index", zInt().NonNegative()),
					f("count", zInt().Positive()),
					f("first_line", zInt().Positive()),
					f("bytes_delta", zInt()),
				).Strict())),
			).Strict())).Strict(),
			annotations: writeAnnotations,
			handler:     patchPage,
		},
		{
			name:        "preflight_page",
			title:       "Preflight Page",
			description: "Check a stored version against the exact CSP and sandbox the content host serves it under, WITHOUT pulling the HTML into context. Reports controls that will silently do nothing in a browser: inline on*= handlers shadowed by a built-in DOM member, subresources the CSP blocks, APIs the sandbox ignores, and scripts that do not parse. Defaults to the published version. Read-only.",
			input: zObject(
				f("slug", slugSchema),
				f("version_id", idArg.Optional().Describe("Defaults to the currently published version.")),
			).Strict(),
			output:      zObject(f("version_id", idOut), f("preflight", preflightSchema)).Strict(),
			annotations: readAnnotations,
			handler:     preflightPage,
		},
		{
			name:        "get_version",
			title:       "Get Page Version",
			description: "Get one version's full metadata and HTML source. Use this for drafts, pending reviews, rollback targets, and source edits.",
			input:       zObject(f("slug", slugSchema), f("version_id", idArg)).Strict(),
			output:      zObject(f("version", versionWithHTMLSchema)).Strict(),
			annotations: readAnnotations,
			handler:     getVersion,
		},
		{
			name:        "list_versions",
			title:       "List Page Versions",
			description: "List bounded version history newest-first. is_published marks pointer equality; is_live additionally requires the page to be enabled.",
			input: zObject(
				f("slug", slugSchema),
				f("status", zEnum("draft", "pending", "approved", "rejected").Optional()),
				f("limit", pageLimit),
				f("cursor", cursorSchema.Optional()),
			).Strict(),
			output:      zObject(f("versions", zArray(versionListSchema)), f("next_cursor", zString().Nullable())).Strict(),
			annotations: readAnnotations,
			handler:     listVersions,
		},
		{
			name:        "list_workspaces",
			title:       "List Workspaces",
			description: "List one-level workspaces with active-page counts. Use returned IDs with set_page_workspace or list_pages filtering.",
			input:       zObject(f("query", zString().Min(1).Max(100).Optional()), f("limit", pageLimit), f("cursor", cursorSchema.Optional())).Strict(),
			output:      zObject(f("workspaces", zArray(workspaceSchema)), f("next_cursor", zString().Nullable())).Strict(),
			annotations: readAnnotations,
			handler:     listWorkspaces,
		},
		{
			name:        "create_workspace",
			title:       "Create Workspace",
			description: "Create global, reversible organization metadata. Workspace names are case-insensitively unique; live content and URLs are unchanged.",
			input:       zObject(f("name", zString().Min(1).Max(100))).Strict(),
			output:      zObject(f("workspace", workspaceSchema)).Strict(),
			annotations: additiveAnnotations,
			handler:     createWorkspace,
		},
		{
			name:        "rename_workspace",
			title:       "Rename Workspace",
			description: "Rename a workspace globally without changing member pages, URLs, versions, or serving state.",
			input:       zObject(f("workspace_id", idArg), f("name", zString().Min(1).Max(100))).Strict(),
			output:      zObject(f("workspace", workspaceSchema)).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     renameWorkspace,
		},
		{
			name:        "set_page_workspace",
			title:       "Set Page Workspace",
			description: "Move an active page into a workspace, or pass workspace_id:null for Ungrouped. This is reversible and does not affect serving or URLs.",
			input:       zObject(f("slug", slugSchema), f("workspace_id", nullableIDArg)).Strict(),
			output: zObject(f("page", zObject(
				f("slug", zString()), f("workspace_id", idOut.Nullable()), f("workspace_name", zString().Nullable()),
			).Strict())).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     setPageWorkspace,
		},
		{
			name:        "list_themes",
			title:       "List Page Themes",
			description: "List the curated themes a human admin can assign. Theme mutation remains human-only.",
			input:       zObject().Strict(),
			output:      zObject(f("themes", zArray(themeSchema))).Strict(),
			annotations: readAnnotations,
			handler:     listThemes,
		},
		{
			name:        "get_guide",
			title:       "Get Pages Agent Guide",
			description: "Read Pages' own guide for agents: how to carry a request from the first question to the final report (topic workflow), and the reference it links to (authoring, templates, data_updates, motion). Call with no arguments for the index, with a topic for its sections, and with a topic and section for that section's text. Read the workflow section for the step you are on before creating, reusing or changing a page; it does not replace your deployment's own rules on sources, approvals and access. Read-only; the text matches this server's version.",
			input: zObject(
				f("topic", guideTopic.Optional().Describe("Omit for the index.")),
				f("section", zString().Max(80).Regex(agentguide.SectionRE.String()).Optional().Describe("A section slug from the topic's list. Requires topic.")),
			).Strict(),
			output: zObject(
				f("product_version", zString()),
				f("topic", guideTopic.Nullable()),
				f("section", zString().Nullable()),
				f("title", zString()),
				f("content", zString()),
				f("content_sha256", zString().Regex(`^[0-9a-f]{64}$`)),
				f("entries", zArray(zObject(
					f("topic", guideTopic),
					f("section", zString().Nullable()),
					f("title", zString()),
					f("bytes", zInt().NonNegative()),
				).Strict())),
			).Strict(),
			annotations: readAnnotations,
			handler:     getGuide,
			renderText:  renderGuide,
		},
		{
			name:        "create_upload_ticket",
			title:       "Create Page Upload Ticket",
			description: l.dataLimitGuidance + "For clients without workspace file references on append_page_upload, the preferred way to publish a file you already have on disk — page HTML, a template, or a managed-data JSON payload. Returns a one-shot URL your shell can PUT the file to directly, so the bytes never pass through your own output: `curl -fsS -X PUT --data-binary @<file> -H \"Authorization: Bearer <ticket>\" <upload_url>`. Supply the exact byte count and lowercase SHA-256 first — the ticket only ever accepts those exact bytes. Then call deploy_page_upload with the upload_id — register_template_upload if you staged a template, update_page_data_upload if you staged kind 'data'. Use append_page_upload instead only if your environment cannot make outbound HTTP requests.",
			input: zObject(uploadTargetFields(
				"With a slug: 'page' (default) stages HTML for deploy_page_upload; 'data' stages a managed-data JSON " +
					"payload for update_page_data_upload. Fixed here and enforced at deploy, so JSON can never be served " +
					"as a page and a document can never be parsed into a data envelope.")...).Strict(),
			output: uploadStateSchema.Extend(
				f("upload_url", zString().Describe("PUT the raw file bytes here. Absolute — do not rebuild it from a hostname.")),
				f("ticket", zString().Describe("Send as `Authorization: Bearer <ticket>`. Write-only, single upload, expires in minutes.")),
				f("ticket_expires_at", dateTime),
				f("curl", zString().Describe("A ready-to-run command; substitute your local file path.")),
			).Strict(),
			annotations: additiveAnnotations,
			handler:     createUploadTicket,
		},
		{
			name:        "start_page_upload",
			title:       "Start Page Content Upload",
			description: l.dataLimitGuidance + "Stage exact file bytes for ordered MCP appends. Prefer this path when your client advertises workspace_file references on append_page_upload.chunk_base64: the client reads and encodes the file, with no model-generated base64 and no shell credential. Supply the exact whole-file byte count and lowercase SHA-256, append the original bytes in sequence in as few appends as the returned max_chunk_bytes allows, then call the matching upload consumer. Staging does not publish. An append failure does not advance the sequence; retry the same exact bytes. Do not cancel and restart a usable upload. Clients without file-reference support need another permitted programmatic transport; create_upload_ticket supports direct file HTTP. Never print or transcribe a large base64 payload through model output.",
			input: zObject(uploadTargetFields(
				"With a slug: 'page' (default) stages HTML for deploy_page_upload; 'data' stages a managed-data JSON " +
					"payload for update_page_data_upload.")...).Strict(),
			output:      uploadStateSchema,
			annotations: additiveAnnotations,
			handler:     startPageUpload,
		},
		{
			name:  "append_page_upload",
			title: "Append Page Content Upload",
			description: "Append one ordered base64 chunk to a staged upload. If your client advertises a workspace_file reference for chunk_base64, use it to transmit exact file bytes without printing or generating base64. Decode size must not exceed max_chunk_bytes from start_page_upload; each append is a full round trip, so use as few appends as max_chunk_bytes allows. Send the exact next_sequence; replaying the same sequence and the same bytes is a safe no-op.\n\n" +
				"A FAILED append does not advance the sequence and does not lose anything already accepted, so every error here is resumable: re-send from the expected_sequence the error reports. Do not cancel to recover — that deletes every byte you have already emitted and makes you send the whole document again, which is how a single page can consume a whole turn.\n\n" +
				"Preserve original byte order; do not minify or alter content between chunks.",
			input: zObject(
				f("upload_id", uploadID),
				f("sequence", zInt().NonNegative()),
				f("chunk_base64", zString().Min(4).Max(float64(l.maxChunkBase64Chars)).
					Describe("Canonical base64 for the next raw file chunk; no data-URI prefix or whitespace.").
					Meta("contentEncoding", "base64")),
			).Strict(),
			output:      uploadStateSchema.Extend(f("deduped", zBool())).Strict(),
			annotations: idempotentAdditiveAnnotations,
			handler:     appendPageUpload,
		},
		{
			name:  "cancel_page_upload",
			title: "Cancel Page Content Upload",
			description: "Discard an uncommitted staged upload and free its active-upload slot. This never changes a page or deployed version. Repeating a cancellation is a safe no-op; committed uploads cannot be cancelled.\n\n" +
				"Do NOT use this to recover from an append error. Sequence and hash failures are resumable — the failed chunk was not accepted and everything before it is still stored — so cancelling only throws away bytes you already paid to emit and forces the whole document again. Cancel when you no longer want to deploy this content at all.",
			input:       zObject(f("upload_id", uploadID)).Strict(),
			output:      zObject(f("upload_id", uploadID), f("cancelled", zBool()), f("next_step", zString())).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     cancelPageUpload,
		},
		{
			name:        "list_templates",
			title:       "List Templates",
			description: "List reusable page templates: name, title, current revision, schema hashes, and how many pages were built from each. page_count is every live page whose history touches the design; serving_count is those whose PUBLISHED version still comes from it; drifted_count is the difference — pages a later raw deploy detached, which a design fix will no longer reach. Call list_template_pages to see which. A template is one stored design; a page built from it carries only its own config and data, so building the second dashboard of a family costs kilobytes of JSON instead of the whole document.",
			input:       zObject().Strict(),
			output: zObject(f("templates", zArray(templateSchema.Extend(
				f("config_schema_sha256", sha256Schema.Nullable()),
				f("data_schema_sha256", sha256Schema.Nullable()),
				f("page_count", zInt().NonNegative()),
				f("serving_count", zInt().NonNegative()),
				f("drifted_count", zInt().NonNegative()),
			).Strict()))).Strict(),
			annotations: readAnnotations,
			handler:     listTemplates,
		},
		{
			name:        "get_template",
			title:       "Get Template",
			description: "Read what a template needs in order to build a page: its config JSON Schema, its data JSON Schema, and the reference config it ships with. Call this INSTEAD of reading a dashboard's HTML — it answers 'what does this design require?' in a couple of KB. The HTML is omitted unless include_html is true, and you almost never need it. Pass revision to inspect an exact past revision.",
			input: zObject(
				f("template", templateName),
				f("revision", zInt().Positive().Optional().Describe("Defaults to the current revision.")),
				f("include_html", zBool().Optional().Describe("Defaults to false. Only set this if you must edit the design itself.")),
			).Strict(),
			output: zObject(
				f("template", templateSchema),
				f("revision", templateRevisionSchema),
				f("config_schema", jsonObjectSchema),
				f("data_schema", jsonObjectSchema),
				f("reference_config", jsonObjectSchema.Describe(
					"The config this template shipped with. A STARTING POINT to read, not a default to inherit: create_page_from_template requires a complete config so one client's identity can never leak into another's page.")),
				f("html", zString().Optional()),
			).Strict(),
			annotations: readAnnotations,
			handler:     getTemplate,
		},
		{
			name:        "list_template_revisions",
			title:       "List Template Revisions",
			description: "List a template's revisions, newest first, marking the current one. Pages pin the exact revision they were built from, so a new revision never changes a deployed page.",
			input:       zObject(f("template", templateName)).Strict(),
			output: zObject(
				f("template", zString()),
				f("revisions", zArray(templateRevisionSchema.Extend(f("is_current", zBool())).Strict())),
			).Strict(),
			annotations: readAnnotations,
			handler:     listTemplateRevisions,
		},
		{
			name:        "create_template_from_page",
			title:       "Make An Existing Page Reusable",
			description: "Promote a page you already like into a reusable design, WITHOUT moving its bytes — the design is already on the server, so nothing is re-authored and nothing passes through your output. Use this whenever someone says 'I want more pages like this one'. It requires only that the page already separates its per-instance values into a #pages-config block; if it does not, this fails with page_not_template_managed and you should separate them and redeploy first (a config schema is derived for you, so that costs no schema authoring). Then build the next client's page with create_page_from_template and a few KB of config.",
			input: zObject(
				f("slug", slugSchema.Describe("The page to promote. Its published version is what gets stored.")),
				f("template", templateName.Describe("The name to register the design under.")),
				f("empty_data", jsonObjectSchema.Describe(
					"What a page built from this shows BEFORE its first refresh — usually empty collections, e.g. {\"rows\":[]}. Validated against the page's own data schema. A template must ship empty so no page inherits this one's numbers.")),
				f("example_from_current_data", zBool().Optional().Describe(
					"Copy this page's CURRENT data into the design's preview-only example block, so the library shows the design populated. Off by default because it is a real client's numbers and a template is visible to everyone — prefer a fictional dataset.")),
				f("title", zString().Max(200).Optional()),
				f("description", zString().Max(2000).Optional()),
				f("note", zString().Max(500).Optional()),
			).Strict(),
			output: zObject(
				f("created", zBool()),
				f("deduped", zBool()),
				f("metadata_updated", zBool().Describe("The existing template title or description changed; the mutation was audited.")),
				f("template", templateSchema),
				f("revision", templateRevisionSchema),
				f("config_schema", jsonObjectSchema),
				f("data_schema", jsonObjectSchema),
				f("reference_config", jsonObjectSchema.Describe(
					"The promoted page's own config, now the design's reference config. It is shown to everyone who opens the library and handed to agents by get_template — if it names a real client, replace it.")),
				f("has_sample_data", zBool()),
				f("from_page", zString()),
				f("from_version_id", idOut),
				f("example_from_current_data", zBool()),
				f("hardcoded_config_values", hardcodedValues),
				f("preflight", preflightSchema),
				f("next_step", zString()),
			).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     createTemplateFromPage,
		},
		{
			name:        "validate_template",
			title:       "Validate A Template Before Registering It",
			description: "Dry-run the template contract and report what is wrong, WITHOUT writing anything. Pass upload_id to check bytes you already staged through a ticket — they are verified and read but NOT consumed, so the same upload_id still registers afterwards and you never re-send the file. Pass html instead only for something small. Checks all five blocks, that both schemas are self-contained JSON Schema 2020-12, that the shipped reference config and empty envelope satisfy their own schemas, that an optional #pages-data-example satisfies the data schema, and runs preflight. Call this before register_template_upload on any design you have not registered before.",
			input: zObject(
				f("upload_id", uploadID.Optional().Describe(
					"A staged upload started with `template`. Preferred: the bytes never pass through your output, and this does not consume the upload.")),
				f("html", zString().Min(1).Optional().Describe("Inline alternative to upload_id. Costs the whole document in output tokens.")),
				f("name", templateName.Optional().Describe(
					"The name you intend to register under. Checked too, and reported separately from the contract, so a bad name and a bad design are told apart.")),
			).Strict(),
			output: zObject(
				f("upload_id", uploadID.Nullable()),
				f("name", zString().Nullable()),
				f("name_error", zObject(f("code", zString()), f("message", zString())).Strict().Nullable()),
				f("contract_ok", zBool()),
				f("contract_error", zObject(f("code", zString()), f("message", zString()), f("details", zUnknown().Optional())).Loose().Nullable()),
				f("bytes", zInt().NonNegative().Nullable()),
				f("config_schema", jsonObjectSchema.Nullable()),
				f("data_schema", jsonObjectSchema.Nullable()),
				f("reference_config", jsonObjectSchema.Nullable()),
				f("data_keys", zArray(zString()).Nullable()),
				f("ships_empty", zBool().Nullable().Describe(
					"False means #pages-data carries rows, which every page built from this would inherit until its first refresh. Put example rows in #pages-data-example instead.")),
				f("has_sample_data", zBool().Nullable()),
				f("sample_data_keys", zArray(zString()).Nullable()),
				f("hardcoded_config_values", hardcodedValues),
				f("preflight", preflightSchema.Nullable().Describe("Errors here are inherited by every page built from this template.")),
				f("next_step", zString()),
			).Strict(),
			annotations: readAnnotations,
			handler:     validateTemplate,
		},
		{
			name:        "delete_template",
			title:       "Retire A Template",
			description: "Soft-delete a template so it leaves the library and its name becomes reusable — the fix for a mistyped name, and the way to retire a superseded design. Revisions are kept, and pages already built from it KEEP SERVING: a page carries its own materialized HTML and never reads the template at runtime. Refused when pages were built from it unless force is true; those pages then lose their design provenance and can no longer be re-rendered from it. Call list_template_pages first. Confirm with the user before calling.",
			input: zObject(
				f("template", templateName),
				f("force", zBool().Optional().Describe("Required to retire a template that pages were built from. Defaults to false.")),
			).Strict(),
			output: zObject(
				f("template", zString()),
				f("deleted", zBool()),
				f("pages_built", zInt().NonNegative()),
				f("note", zString()),
			).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     deleteTemplate,
		},
		{
			name:        "template_urls",
			title:       "Get Template URLs",
			description: "Return the library URL for a template plus a short-TTL signed preview URL that renders the design on the sandboxed content host — populated with the design's example data when it ships one, otherwise its empty state. Use it to show a human what a design looks like before building pages from it. The preview URL expires; mint a fresh one rather than storing it.",
			input: zObject(
				f("template", templateName),
				f("revision", zInt().Positive().Optional().Describe("Defaults to the current revision.")),
			).Strict(),
			output: zObject(
				f("template", zString()),
				f("revision", zInt().Positive()),
				f("library", zString().URL().Describe("The template library, on the dashboard host. Requires an admin session.")),
				f("preview", zString().URL().Describe("Signed, sandboxed, on the content host. Expires.")),
				f("expires_in_seconds", zInt().Positive()),
				f("has_sample_data", zBool()),
			).Strict(),
			annotations: readAnnotations,
			handler:     templateURLs,
		},
		{
			name:        "list_template_pages",
			title:       "List Pages Built From A Template",
			description: "Show every live page whose history touches this template: which revision each one currently serves, which are behind the current revision, and which have DRIFTED off the design entirely (drifted:true — a later raw deploy_page or patch_page replaced the template-built version, so a design fix no longer reaches them). A drifted page keeps last_revision, which is what rerender_page_from_template needs to pull it back. Read-only. Use it to decide what a design fix should move; Pages never re-renders a page on its own, because a bad revision would otherwise be a fleet-wide incident instead of one page to fix.",
			input:       zObject(f("template", templateName)).Strict(),
			output: zObject(
				f("template", zString()),
				f("current_revision", zInt().Positive()),
				f("serving_count", zInt().NonNegative()),
				f("drifted_count", zInt().NonNegative()),
				f("pages", zArray(zObject(
					f("slug", zString()),
					f("title", zString()),
					// Nullable on a drifted page: there is no published
					// version, or the one there is has no binding to this
					// template.
					f("live_version_id", idOut.Nullable()),
					f("revision", zInt().Positive().Nullable()),
					f("behind", zBool()),
					f("drifted", zBool()),
					f("last_revision", zInt().Positive().Nullable()),
					f("page_is_live", zBool()),
					f("config_sha256", sha256Schema.Nullable()),
				).Strict())),
			).Strict(),
			annotations: readAnnotations,
			handler:     listTemplatePages,
		},
		{
			name:        "rerender_page_from_template",
			title:       "Rerender One Page On A Template Revision",
			description: "Move ONE page onto a different revision of its template, or pull a DRIFTED page back onto it, keeping its own config and data by default. For a schema migration, supply complete target-valid config and/or data in this same call, with expected_version. Replacement data requires explicit source_as_of and records a fresh ingestion; omitted data preserves the entire live envelope. A page a raw deploy_page or patch_page detached (list_template_pages: drifted:true) has no binding left to infer the design from, so name the template explicitly and it is re-attached — which works as long as its published HTML still carries #pages-config and #pages-data, the usual case after patch_page. Any omitted config or data comes from the page's published HTML, and all values are validated against the target revision's schemas, so a revision that tightened its contract fails loudly instead of producing a page its own schema rejects. publish defaults to FALSE: the new design lands as an inspectable version so a human can preview it before a client sees it, then publish_page makes it live. One page per call — there is no bulk re-render.",
			input: zObject(
				f("slug", slugSchema),
				f("template", templateName.Optional().Describe("Defaults to the template the page is already built from.")),
				f("revision", zInt().Positive().Optional().Describe("Defaults to the template's current revision.")),
				f("config", jsonObjectSchema.Optional().Describe("Complete replacement config validated against the TARGET revision; never merged with reference config. Requires expected_version.")),
				f("data", jsonObjectSchema.Optional().Describe("Complete replacement data validated against the TARGET revision. Requires source_as_of and expected_version.")),
				f("source_as_of", dateTime.Optional().Describe("Required only with replacement data; source coverage cannot regress.")),
				f("publish", zBool().Optional().Describe("Defaults to FALSE. Set true only when a design change has already been reviewed.")),
				f("expected_version", idArg.Optional().Describe("Optimistic-concurrency check against the current published version.")),
				f("note", zString().Max(500).Optional()),
			).Strict(),
			output: deployOutputSchema.Extend(
				f("template", zString()),
				f("from_revision", zInt().Positive().Nullable().Describe("The revision the page was serving, or null when it had drifted off the design entirely.")),
				f("reattached", zBool().Describe("True when this call pulled a drifted page back onto the template rather than moving revisions.")),
				f("template_revision", zInt().Positive()),
				f("template_version_id", idOut),
				f("config", jsonObjectSchema),
				f("config_sha256", sha256Schema),
				f("envelope", dataEnvelopeSchema),
				f("data_sha256", sha256Schema),
				f("template_sha256", sha256Schema),
			).Strict(),
			annotations: writeAnnotations,
			handler:     rerenderPageFromTemplate,
		},
		{
			name:        "register_template_upload",
			title:       "Register Staged Template",
			description: "Register a complete staged upload as a template revision. The upload must have been started with `template` rather than `slug`. The HTML must be a full managed page carrying BOTH block pairs — pages-config-schema/pages-config and pages-data-schema/pages-data — with a reference config and an empty-state data envelope that each satisfy their own schema; that is what lets Pages validate the design instead of trusting it. Registering identical bytes again returns the same revision. This never touches a live page: existing pages stay pinned to the revision they were built from. Exact committed retries add replayed:true and refresh next_step for newer revisions or retirement without registering again.",
			input: zObject(
				f("upload_id", uploadID),
				f("title", zString().Max(200).Optional()),
				f("description", zString().Max(2000).Optional()),
				f("note", zString().Max(500).Optional()),
			).Strict(),
			output: zObject(
				f("upload_id", uploadID),
				f("replayed", uploadReplaySchema),
				f("created", zBool()),
				f("deduped", zBool()),
				f("metadata_updated", zBool().Describe("The existing template title or description changed; the mutation was audited.")),
				f("template", templateSchema),
				f("revision", templateRevisionSchema),
				f("config_schema", jsonObjectSchema),
				f("data_schema", jsonObjectSchema),
				f("reference_config", jsonObjectSchema),
				f("has_sample_data", zBool().Describe(
					"Whether the design shipped an optional #pages-data-example block. Preview-only: it is deleted from every page built from this template.")),
				f("preflight", preflightSchema.Describe(
					"Static check of the TEMPLATE. Errors here are inherited by every page built from it, so fix them before creating pages.")),
				f("next_step", zString()),
			).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     registerTemplateUpload,
		},
		{
			name:        "deploy_page_upload",
			title:       "Deploy Staged Page Content",
			description: "Verify and atomically deploy a complete staged upload, creating the page if needed and publishing by default on open pages. The upload slug is fixed by start_page_upload. Exact retries preserve the original commit receipt, add replayed:true, and refresh current live-state fields and next_step without repeating the mutation. Approval, optimistic concurrency, immutable versions, and audit behavior match deploy_page.",
			input:       zObject(uploadDeployInputShape...).Strict(),
			output:      deployOutputSchema.Extend(f("upload_id", uploadID), f("created", zBool()), f("replayed", uploadReplaySchema)).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     deployPageUpload,
		},
		// The managed-data half of the staged path. HTML has had one since
		// #14; data — generated on disk and routinely LARGER than the HTML —
		// could only ever travel inline as a tool argument.
		{
			name:  "update_page_data_upload",
			title: "Update Managed Page Data From Staged Upload",
			description: l.dataLimitGuidance + "Publish a managed-data payload you staged with create_upload_ticket (kind 'data') instead of sending it inline. The JSON never passes through your context. Payload, escaped-envelope and staged-file limits still apply. Everything else is update_page_data exactly: same schema validation, same monotonic source_as_of, same mandatory expected_version, same dedupe, same data_profile / data_warnings, same expect reconciliation — one write path, not two.\n\n" +
				"The staged bytes must parse as a JSON object. An upload staged as 'page' is refused here, and one staged as 'data' is refused by deploy_page_upload. Exact committed retries add replayed:true, preserve the original data receipt, and refresh current live-state fields and next_step without updating data again." +
				usageDescription,
			input: zObject(
				f("upload_id", uploadID),
				f("slug", slugSchema.Describe("Must match the slug the upload was staged for.")),
				f("source_as_of", dateTime.Describe("Latest source coverage represented by the payload; cannot regress or be materially future.")),
				f("expected_version", idArg.Describe("Required live_version_id from get_page_data.")),
				f("publish", zBool().Optional().Describe("Defaults to true. False creates an inspectable canary draft/pending version.")),
				f("note", zString().Max(500).Optional()),
				f("expect", dataExpectSchema.Optional()),
			).Strict(),
			output: zObject(append(pageDataStateShape(),
				f("upload_id", uploadID),
				f("replayed", uploadReplaySchema),
				f("deduped", zBool()),
				f("published", zBool()),
				f("gated", zBool()),
				f("next_step", zString()),
				f("data_profile", dataProfileSchema),
				f("data_warnings", zArray(dataWarningSchema)),
				f("data_usage", dataUsage),
			)...).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     updatePageDataUpload,
		},
		{
			name:        "create_page_from_template",
			title:       "Create Page From Template",
			description: "Build a page from a stored template by sending ONLY its config — the design is already on the server, so this costs kilobytes of JSON instead of the whole document. Call get_template first to read the config and data schemas. config must be COMPLETE: it is validated against the template's config schema and is never merged with the template's reference config, so one client's identity cannot leak into another's page. Omit data to deploy the design's awaiting-first-ingest state and fill it later with update_page_data. Fails with page_exists if the slug is taken. An identical retry is a no-op only while that build is still the page's live version (or the page has published nothing yet) — once the page has taken a refresh or a config edit, replaying the create is page_exists too, because deploying it would move the live pointer back to the empty state. Use rollback_page if reverting is what you mean.",
			input: zObject(
				f("template", templateName),
				f("slug", slugSchema),
				f("config", jsonObjectSchema.Describe("Complete config satisfying the template's config schema.")),
				f("revision", zInt().Positive().Optional().Describe("Build from an exact template revision. Defaults to the current one.")),
				f("data", jsonObjectSchema.Optional().Describe(
					"Optional first payload. When omitted the template's empty-state data is kept and the page shows its awaiting-first-ingest states rather than fabricated zeros.")),
				f("source_as_of", dateTime.Optional().Describe("Required when data is supplied: the coverage that data represents.")),
				f("render_mode", zEnum("themed", "raw").Optional().Describe(renderModeDescription)),
				f("title", zString().Max(200).Optional()),
				f("require_approval", zBool().Optional()),
				f("client_id", zString().Max(200).Optional()),
				f("publish", zBool().Optional().Describe("Defaults to true on open pages.")),
				f("note", zString().Max(500).Optional()),
			).Strict(),
			output: deployOutputSchema.Extend(
				f("created", zBool()),
				f("template", zString()),
				f("template_revision", zInt().Positive()),
				f("template_version_id", idOut),
				f("config", jsonObjectSchema),
				f("config_sha256", sha256Schema),
				f("config_schema_sha256", sha256Schema),
				f("envelope", dataEnvelopeSchema),
				f("data_sha256", sha256Schema),
				f("schema_sha256", sha256Schema),
				f("template_sha256", sha256Schema),
			).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     createPageFromTemplate,
		},
		{
			name:        "get_page_config",
			title:       "Get Page Config",
			description: "Read the deploy-time config of a template-built page: its config JSON Schema, the current config, deterministic hashes, and the live version to pass as update_page_config's expected_version. Separate from get_page_data so changing a campaign's identity never requires reading its rows. Fails with page_not_template_managed on a page that has no pages-config block. `template` names the template revision the live version was built from, or is null for a page that only carries managed blocks (a copy of a template page, or a design never registered): null is the authoritative answer to \"is this page template-built?\"; there is no need to search list_template_pages.",
			input:       zObject(f("slug", slugSchema)).Strict(),
			output: zObject(
				f("page", pageSchema),
				f("version", versionSchema),
				f("config_schema", jsonObjectSchema),
				f("config", jsonObjectSchema),
				f("config_sha256", sha256Schema),
				f("config_schema_sha256", sha256Schema),
				f("template_sha256", sha256Schema),
				f("template", zObject(
					f("name", zString()),
					f("revision", zInt()),
					f("is_current_revision", zBool()),
					f("current_revision", zInt().Nullable()),
				).Strict().Nullable()),
				f("version_is_live", zBool()),
				f("page_is_live", zBool()),
				f("live_version_id", idOut.Nullable()),
				f("urls", urlsSchema),
			).Strict(),
			annotations: readAnnotations,
			handler:     getPageConfig,
		},
		{
			name:        "update_page_config",
			title:       "Update Page Config",
			description: "Replace the config of a template-built page, validated against its embedded config schema. The data block is left BYTE-FOR-BYTE unchanged, so renaming a campaign or retargeting a KPI cannot disturb, restate, or roll back its numbers, and source coverage is carried across untouched. config must be complete — this replaces, it does not merge. expected_version is mandatory optimistic concurrency; use the live_version_id from get_page_config.",
			input: zObject(
				f("slug", slugSchema),
				f("config", jsonObjectSchema.Describe("Complete replacement config satisfying the page's config schema.")),
				f("expected_version", idArg.Describe("Required live_version_id from get_page_config.")),
				f("publish", zBool().Optional().Describe("Defaults to true. False creates an inspectable canary.")),
				f("note", zString().Max(500).Optional()),
			).Strict(),
			output: zObject(
				f("version", versionSchema),
				f("config", jsonObjectSchema),
				f("config_schema", jsonObjectSchema),
				f("config_sha256", sha256Schema),
				f("config_schema_sha256", sha256Schema),
				f("envelope", dataEnvelopeSchema),
				f("data_sha256", sha256Schema),
				f("template_sha256", sha256Schema),
				f("deduped", zBool()),
				f("published", zBool()),
				f("gated", zBool()),
				f("live", zBool().Describe("Compatibility alias for version_is_live.")),
				f("version_is_live", zBool()),
				f("page_is_live", zBool()),
				f("live_version_id", idOut.Nullable()),
				f("urls", urlsSchema),
				f("next_step", zString()),
			).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     updatePageConfig,
		},
		{
			name:        "deploy_page",
			title:       "Deploy Page",
			description: "Create a page if needed from small inline HTML and publish by default on open pages. For a workspace file or content over " + inlineBytes + " UTF-8 bytes, use the staged page-upload tools instead. Approval-gated pages remain pending. Follow page_is_live, version_is_live, and next_step.",
			input:       zObject(append(append([]field{}, deployInputShape...), createdOnly...)...).Strict(),
			output:      deployOutputSchema.Extend(f("created", zBool())).Strict(),
			annotations: writeAnnotations,
			handler:     deployPageTool,
		},
		{
			name:        "update_page",
			title:       "Update Page",
			description: "Deploy small inline HTML to an existing page and publish by default on open pages. For a workspace file or content over " + inlineBytes + " UTF-8 bytes, use the staged page-upload tools instead. Fails if the page is missing. Supports optimistic concurrency.",
			input:       zObject(deployInputShape...).Strict(),
			output:      deployOutputSchema,
			annotations: writeAnnotations,
			handler:     updatePageTool,
		},
		{
			name:  "update_page_data",
			title: "Update Managed Page Data",
			description: "Validate structured data against the published page's embedded JSON Schema, then create an immutable version by replacing only the pages-data script contents. source_as_of is required, monotonic RFC3339 source coverage; refreshed_at is generated by Pages. expected_version is mandatory optimistic concurrency. Same template+data+source dedupes; newer source coverage versions even unchanged metrics. Defaults publish=true; use publish=false for a canary.\n\n" +
				"The schema validates SHAPE ONLY — a payload missing three weeks of days, or a whole deal, satisfies it exactly as well as a correct one. So the response returns data_profile (row counts, date extents, numeric totals, distinct values of low-cardinality keys) and data_warnings (coverage that starts later or ends earlier than what is already published, rows that dropped, dimension values that disappeared). RECONCILE data_profile against your source before you tell anyone the page is right. Better: pass expect with the totals you computed from the source and let Pages refuse the write if the payload disagrees." +
				usageDescription,
			input: zObject(
				f("slug", slugSchema),
				f("data", jsonObjectSchema.Describe(
					"Complete data object that satisfies the page's returned schema; never include credentials. "+
						"SEND IT WHOLE. Do not split it across calls, sample it, or decline to send because it looks "+
						"large. "+l.dataUploadGuidance+" A partial payload passes schema "+
						"validation while publishing wrong numbers. Over the cap you get an explicit "+
						"data_validation_failed for payload/envelope limits or data_too_large_for_inline for the inline transport limit; nothing is truncated.")),
				f("source_as_of", dateTime.Describe("Latest source coverage represented by data; cannot regress or be materially future.")),
				f("expected_version", idArg.Describe("Required live_version_id from get_page_data.")),
				f("publish", zBool().Optional().Describe("Defaults to true. False creates an inspectable canary draft/pending version.")),
				f("note", zString().Max(500).Optional()),
				f("expect", dataExpectSchema.Optional()),
			).Strict(),
			output: zObject(append(pageDataStateShape(),
				f("deduped", zBool()),
				f("published", zBool()),
				f("gated", zBool()),
				f("next_step", zString()),
				f("data_profile", dataProfileSchema),
				f("data_warnings", zArray(dataWarningSchema)),
				f("data_usage", dataUsage),
			)...).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     updatePageData,
		},
		// Transitional alias retained because the deployed Chat/Cutlass
		// allowlists already permit this name. Its old schedule-mutating
		// contract is gone; it is the same read-only prompt preparation as
		// the canonical tool.
		{
			name:        "configure_page_refresh",
			title:       "Prepare Dashboard Update (Compatibility)",
			description: "Compatibility alias for prepare_dashboard_update on older static MCP allowlists. On those clients call this whenever a user says 'update <slug> dashboard with ...', passing the exact request as instructions. It is read-only: Pages does not configure, persist, or dispatch a schedule. A legacy workflow argument is converted into recurring prompt text and daily_at_utc is accepted but not written into it, so already-deployed clients need no code change; run_now never executes work.",
			input:       configureInput,
			output:      prepareOutput,
			annotations: readAnnotations,
			handler:     configurePageRefresh,
		},
		{
			name:        "publish_page",
			title:       "Publish Page Version",
			description: "Publish a draft version on an open page. Agents cannot publish approval-gated or disabled pages.",
			input:       zObject(f("slug", slugSchema), f("version_id", idArg), f("expected_version", idArg.Optional())).Strict(),
			output:      publishOutputSchema,
			annotations: writeAnnotations,
			handler:     publishPage,
		},
		{
			name:        "rollback_page",
			title:       "Rollback Page",
			description: "Move an open page's published pointer to an approved version. Omit version_id to choose the previous approved version. note is recorded in the audit log — a rollback republishes bytes that already exist, so the reason is not inferable from any diff.",
			input: zObject(
				f("slug", slugSchema),
				f("version_id", idArg.Optional()),
				f("expected_version", idArg.Optional()),
				f("note", zString().Max(500).Optional().Describe("Why the pointer moved. Recorded in the audit log.")),
			).Strict(),
			output:      publishOutputSchema,
			annotations: writeAnnotations,
			handler:     rollbackPage,
		},
		{
			name:        "set_password",
			title:       "Set Client Password",
			description: "Set or change a page's client password. Clearing remains human-admin-only. The result says whether the page is actually serving.",
			input:       zObject(f("slug", slugSchema), f("password", zString().Min(1).Max(512))).Strict(),
			output: zObject(
				f("slug", zString()),
				f("has_password", zBool()),
				f("page_is_live", zBool()),
				f("urls", urlsSchema),
				f("next_step", zString()),
			).Strict(),
			annotations: writeAnnotations,
			handler:     setPassword,
		},
		{
			name:        "set_title",
			title:       "Set Page Title",
			description: "Change a page's display title without changing its URL, published version, or content.",
			input:       zObject(f("slug", slugSchema), f("title", zString().Min(1).Max(200))).Strict(),
			output:      zObject(f("slug", zString()), f("title", zString())).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     setTitle,
		},
		{
			name:        "delete_page",
			title:       "Soft-delete Page",
			description: "Soft-delete an open page so it stops serving and the slug can be reused. Reversible by a human admin. Confirm with the user first.",
			input:       zObject(f("slug", slugSchema)).Strict(),
			output: zObject(
				f("slug", zString()), f("deleted", zBool()), f("live", zBool()), f("page_is_live", zBool()), f("note", zString()),
			).Strict(),
			annotations: idempotentWriteAnnotations,
			handler:     deletePage,
		},
		{
			name:  "page_urls",
			title: "Get Page URLs",
			description: "Validate that a page exists and return its routing URLs: `live` is the client link to share, " +
				"`admin` and `view` are Elcano-staff-only.",
			input:       zObject(f("slug", slugSchema)).Strict(),
			output:      urlsSchema,
			annotations: readAnnotations,
			handler:     pageURLsTool,
		},
	}

	c := &catalog{limits: l, tools: tools, byName: map[string]*toolDef{}}
	for _, t := range tools {
		c.byName[t.name] = t
	}
	c.instructions = instructions(l)
	c.dataUpdateInstructions = dataUpdateInstructions
	return c
}

// instructions is the initialize text for broad tokens.
func instructions(l limits) string {
	return "Elcano Pages hosts versioned, Flag-themed client dashboards at stable live URLs. " +
		"Use list_workspaces and filtered list_pages to discover existing work before creating anything. " +
		"deploy_page creates a missing page and publishes by default on open pages; update_page requires an existing page. Pass expected_version when updating a page you previously read. These tools are for small inline HTML only. " +
		"For HTML saved in a workspace file or over " + groupedInt(l.maxInlineHTMLBytes) + " UTF-8 bytes, never paste the whole file into deploy_page/update_page and never pass a path, $(cat...), or placeholder. " + l.pageUploadGuidance + " " +
		"Always follow page_is_live, version_is_live, live_version_id, and next_step. On approval-gated pages a new version waits for a human in urls.admin while an older published version may remain live. " +
		"When an upload commit returns replayed:true, its version/revision, published, gated, and data fields are the original operation receipt; current live-state fields and next_step are refreshed without repeating that operation. Never infer current publication from published alone. " +
		"Use create_workspace, rename_workspace, and set_page_workspace for reversible organization. Workspace deletion, approval/rejection, takedown controls, theme changes, password clearing, and restoration remain human-admin-only. " +
		"Set a client password before sharing client-only work. delete_page is a reversible soft delete, but confirm with the user first. " +
		"When a user says 'update <slug> dashboard with ...' or otherwise asks to change a specific existing dashboard, call prepare_dashboard_update first with that exact slug and the user's complete plain-language request. If an older static client allowlist hides that canonical tool, call its read-only compatibility alias configure_page_refresh with the same arguments. Use update_type=data for values only, layout for design/schema/JavaScript, or auto when classification is genuinely unclear. Never create a replacement slug or companion data page. " +
		"For a one-time request, follow the returned prompt now in this conversation. If it returns migration_required, run that same-slug migration prompt, then call prepare_dashboard_update again as instructed. For recurring=true, show the returned prompt to the user verbatim so they can place it in their scheduler of choice; do not execute it now, claim that Pages scheduled it, or require Pages, MOC, Chat, Fleet, or Cutlass configuration changes. " +
		"When following a managed-data prompt, call get_page_data with detail='summary' first and fetch the schema and rows once (detail='export' URLs downloaded to files where your client can, otherwise one full read), gather complete and fresh source data, map only the schema-defined data object, then call update_page_data with that live_version_id as expected_version and the latest represented source_as_of. Pages generates refreshed_at, preserves all layout/schema bytes, rejects source regressions, and dedupes exact retries. Never call the update when source data is missing, stale, or incomplete. After an ambiguous write or stale_version, reread once and compare schema/data hashes before at most one safe retry."
}

// dataUpdateInstructions is the initialize text for data_update tokens.
const dataUpdateInstructions = "This token is restricted to Pages managed-data automation for exact granted slugs. " +
	"Call get_page_data with detail='summary' first and use its live_version_id as update_page_data.expected_version. Read the schema and rows once per run: detail='export' returns short-lived URLs for a host-side download into files; detail='full' (the default) returns them inline. " +
	"Gather every required source and enforce freshness/completeness before writing. Never guess, send credentials, or update partial/stale data. " +
	"Map a complete data object against the returned self-contained JSON Schema. source_as_of is the latest represented source coverage; Pages separately generates refreshed_at. " +
	"update_page_data preserves all layout and schema bytes, rejects regression/future coverage, serializes writers, and dedupes exact template+data+source retries. publish defaults true; publish:false creates a canary. " +
	"On stale_version or an ambiguous write, reread and compare schema/data hashes, then retry at most once only if the intended source coverage is not already represented."

// ToolNames lists the static catalog, in registration order.
func ToolNames() []string {
	c := currentCatalog()
	names := make([]string, len(c.tools))
	for i, t := range c.tools {
		names[i] = t.name
	}
	return names
}

// Instructions is the initialize instructions text for broad tokens.
func Instructions() string { return currentCatalog().instructions }

// DataUpdateInstructions is the initialize instructions text for
// data_update tokens.
func DataUpdateInstructions() string { return dataUpdateInstructions }
