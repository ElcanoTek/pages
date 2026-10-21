// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Managed data and config tools.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/dataexport"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/pageurls"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/versions"
)

// publishedLiveState is for reads that return the PUBLISHED version: it is
// live exactly when the page is (published pointer set, page not disabled).
func publishedLiveState(page versions.Page) *jsonv.Object {
	pageIsLive := page.PublishedVersionID != 0 && !page.Disabled
	return jsonv.ObjectOf(
		"version_is_live", pageIsLive,
		"page_is_live", pageIsLive,
		"live_version_id", idOrNull(page.PublishedVersionID),
	)
}

// The two compact reads. Neither carries the schema or a row.
const (
	summaryNextStep = "Enough to pick a branch. For the schema or rows, call get_page_data once with detail='export' and download " +
		"the URLs into files (e.g. download_url), or detail='full' once if you cannot. live_version_id is expected_version."
	summaryTruncatedStep = "coverage_profile is partial (coverage_truncated): take complete row counts and date ranges from the exported data file."
	// A recorded check's detail may be 500 characters; two of them would be
	// most of a summary's budget. The complete lines stay on a full read.
	summaryDetailChars = 160
	// MCP and set_title cap a title at 200 characters, but POST
	// /api/v1/pages does not, so a legacy page can carry a far longer one.
	// The compact reads promise a small answer, so they clip it.
	briefTitleChars = 200
)

func clip(s string, n int) string {
	if jsstr.UTF16Len(s) > n {
		return jsstr.Slice(s, 0, n-1) + "…"
	}
	return s
}

func briefFreshness(f *versions.Freshness) (any, error) {
	if f == nil {
		return nil, nil
	}
	o, err := toObject(f)
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"latest_detail", "last_check_detail"} {
		if s, ok := o.Value(k).(string); ok {
			o.Set(k, clip(s, summaryDetailChars))
		}
	}
	return o, nil
}

func pageBrief(p versions.Page) *jsonv.Object {
	return jsonv.ObjectOf(
		"slug", p.Slug,
		"title", clip(p.Title, briefTitleChars),
		"disabled", p.Disabled,
		"require_approval", p.RequireApproval,
	)
}

func envelopeStamps(env *jsonv.Object) *jsonv.Object { return without(env, "data") }

func pageDataSummary(r *versions.PageDataView) (*jsonv.Object, error) {
	coverage, truncated := pagedata.BoundedCoverageProfile(r.DataProfile, pagedata.CoverageMaxBytes)
	fresh, err := briefFreshness(r.Freshness)
	if err != nil {
		return nil, err
	}
	out := jsonv.ObjectOf(
		"detail", "summary",
		"page", pageBrief(r.Page),
		"live_version_id", idOrNull(r.Page.PublishedVersionID),
		"data_sha256", r.DataSHA256,
		"schema_sha256", r.SchemaSHA256,
		"template_sha256", r.TemplateSHA256,
		"envelope", envelopeStamps(r.Envelope),
		"freshness", fresh,
		"coverage_profile", coverage,
	)
	next := summaryNextStep
	if truncated {
		out.Set("coverage_truncated", true)
		next += " " + summaryTruncatedStep
	}
	out.Set("next_step", next)
	return out, nil
}

func pageDataExport(r *versions.PageDataView, c *call) (*jsonv.Object, error) {
	minted, err := dataexport.Mint(dataexport.MintParams{PageID: r.Page.ID, VersionID: r.Version.ID, TokenID: c.actor.TokenID}, dataexport.ExportTTLSeconds)
	if err != nil {
		return nil, err
	}
	exports, err := spread(jsonv.ObjectOf("version_id", r.Version.ID.String()), dataexport.ExportURLs(minted.Token),
		jsonv.ObjectOf("expires_at", minted.ExpiresAt))
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf(
		"detail", "export",
		"page", pageBrief(r.Page),
		"live_version_id", idOrNull(r.Page.PublishedVersionID),
		"data_sha256", r.DataSHA256,
		"schema_sha256", r.SchemaSHA256,
		"template_sha256", r.TemplateSHA256,
		"envelope", envelopeStamps(r.Envelope),
		"exports", exports,
		"next_step", "Download schema_url and data_url once each into workspace files before "+minted.ExpiresAt+" "+
			"with a host-side URL download such as download_url, then read them from disk; do not print their contents. "+
			"Check sha256 of each downloaded file's exact bytes against schema_sha256 and data_sha256 (do not re-serialize to hash); "+
			"envelope_url is optional, since the stamps are above. These URLs are "+
			"credentials for this one version: never share or repeat them. Mint new ones only if these expired before download.",
	), nil
}

func getPageData(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	r, err := versions.GetPageData(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	var out *jsonv.Object
	detail, _ := argStr(a, "detail")
	switch detail {
	case "summary":
		out, err = pageDataSummary(r)
	case "export":
		out, err = pageDataExport(r, c)
	default:
		out, err = pageDataFull(r, !argBoolOr(a, "include_data", true))
	}
	if err != nil {
		return nil, err
	}
	// Issue #109: report the payload against the managed-data limit on every
	// read, so a refresh run sees its headroom before it writes.
	if r.Managed != nil {
		out.Set("data_usage", r.Managed.DataUsage())
	}
	return out, nil
}

func pageDataFull(r *versions.PageDataView, omitData bool) (*jsonv.Object, error) {
	out, err := spread(
		jsonv.ObjectOf(
			"page", r.Page,
			"version", r.Version,
			"schema", r.Schema,
			"data_sha256", r.DataSHA256,
			"schema_sha256", r.SchemaSHA256,
			"template_sha256", r.TemplateSHA256,
			"data_profile", r.DataProfile,
			"freshness", r.Freshness,
		),
		publishedLiveState(r.Page),
		jsonv.ObjectOf("urls", pageurls.For(r.Page.Slug)),
	)
	if err != nil {
		return nil, err
	}
	envelope := envelopeStamps(r.Envelope)
	if omitData {
		out.Set("data_omitted", true)
	} else {
		envelope.Set("data", r.Envelope.Value("data"))
	}
	out.Set("envelope", envelope)
	return out, nil
}

// warningLite is the part of a data warning next_step reads; a replayed
// receipt carries warnings as plain JSON.
type warningLite struct{ code, message string }

func warningsOf(ws []pagedata.Warning) []warningLite {
	out := make([]warningLite, len(ws))
	for i, w := range ws {
		out[i] = warningLite{w.Code, w.Message}
	}
	return out
}

func warningsFromJSON(v any) []warningLite {
	arr, _ := v.([]any)
	out := make([]warningLite, 0, len(arr))
	for _, e := range arr {
		o, _ := e.(*jsonv.Object)
		if o == nil {
			continue
		}
		code, _ := o.Value("code").(string)
		msg, _ := o.Value("message").(string)
		out = append(out, warningLite{code, msg})
	}
	return out
}

// dataUpdateNextStep: a stale refresh and a narrowed payload need DIFFERENT
// sentences. "Reconcile against the source" is right when figures may be
// wrong; it is the wrong advice when the figures are simply the ones already
// published, because the mistake there is not a bad number, it is
// ANNOUNCING an update that did not happen. Lead with whichever applies.
func dataUpdateNextStep(next string, warnings []warningLite) string {
	if len(warnings) == 0 {
		return next
	}
	isStale := func(w warningLite) bool { return w.code == "data_unchanged" || w.code == "coverage_did_not_advance" }
	var stale *warningLite
	allStale := true
	codes := make([]string, len(warnings))
	for i := range warnings {
		codes[i] = warnings[i].code
		if isStale(warnings[i]) {
			if stale == nil {
				stale = &warnings[i]
			}
		} else {
			allStale = false
		}
	}
	joined := strings.Join(codes, ", ")
	// "You added nothing" is only the headline when there is nothing WORSE
	// to say: a payload that also lost rows, coverage or a dimension value
	// has a real correctness problem.
	if stale != nil && allStale {
		return fmt.Sprintf("%s THIS REFRESH ADDED NO NEW DATA (%s). %s ", next, joined, stale.message) +
			"Do not describe this as a data update: say which coverage is live, which day was expected, and that the " +
			"source did not contain it."
	}
	return fmt.Sprintf("%s RECONCILE BEFORE SHARING: %d warning(s) about what this payload contains ", next, len(warnings)) +
		fmt.Sprintf("(%s). Read data_warnings and data_profile and check them against the source before telling anyone ", joined) +
		"this page is correct."
}

type inlineDataDetails struct {
	Bytes                    int               `json:"bytes"`
	MaxBytes                 int               `json:"max_bytes"`
	ChangingTransportCanHelp bool              `json:"changing_transport_can_help"`
	Limits                   datalimits.Limits `json:"limits"`
}

const ordinaryTimestamp = "2000-01-01T00:00:00.000Z"

// AssertInlineData enforces the stated managed-data ceilings on an inline
// payload (lib/mcp-tools.js assertInlineData). A payload under them is sent
// whole: splitting, sampling or summarizing to "be safe" produces a dashboard
// that is wrong in a way the schema cannot catch, which is strictly worse
// than a refusal. sourceAsOf nil means omitted. It returns the compact
// payload size.
func AssertInlineData(data any, sourceAsOf any) (int, error) {
	if _, ok := data.(*jsonv.Object); !ok {
		return 0, apierror.BadRequest("data must be a JSON object", "data_invalid", nil)
	}
	measured := datalimits.MeasureData(data)
	n := measured.PayloadBytes
	if err := datalimits.AssertDomainSize(n, "payload", ""); err != nil {
		return 0, err
	}
	source := ordinaryTimestamp
	if sourceAsOf != nil {
		normalized, err := pagedata.NormalizeSourceAsOf(sourceAsOf, time.Now())
		if err != nil {
			return 0, err
		}
		source = normalized
	}
	if err := datalimits.AssertDomainSize(measured.EscapedPayloadBytes+datalimits.EnvelopeOverhead(source, ordinaryTimestamp), "envelope", ""); err != nil {
		return 0, err
	}
	if limit := datalimits.MaxInlineDataBytes(); n > limit {
		canStage := n <= datalimits.MaxUploadBytes()
		advice := "The compact file also exceeds the staged upload limit. Keep the complete source and ask the operator to review capacity. "
		if canStage {
			advice = "Stage the complete compact JSON file: create_upload_ticket with kind 'data', PUT it from your shell, then update_page_data_upload. "
		}
		return 0, apierror.BadRequest(
			fmt.Sprintf("data is %d bytes; inline managed-data transport is capped at %d. ", n, limit)+advice+
				"Do NOT split, sample or summarize the payload to fit. "+datalimits.DataLimitGuidance(),
			"data_too_large_for_inline",
			inlineDataDetails{Bytes: n, MaxBytes: limit, ChangingTransportCanHelp: canStage, Limits: datalimits.Current()})
	}
	return n, nil
}

// dataUpdateResult is the response shape both managed-data writes return:
// one function, so a caller cannot tell the staged route from the inline one
// by what comes back.
func dataUpdateResult(r *versions.DataUpdateResult, page pageState, slug string, publishArg any) (*jsonv.Object, error) {
	versionIsLive := !page.Disabled && sameID(r.Version.ID, page.PublishedVersionID)
	pageIsLive := page.PublishedVersionID != 0 && !page.Disabled
	var next string
	switch {
	case versionIsLive && r.Deduped:
		next = "The exact data and source coverage were already live; no new immutable version was created."
	case versionIsLive:
		next = "The structured-data version is live now. Layout and schema bytes were preserved."
	case page.Disabled:
		next = "The data version was saved, but the page is disabled and is not serving. A human admin must re-enable it."
	case r.Gated && pageIsLive:
		next = fmt.Sprintf("Approval is required. Version %s is pending; the previous live version %s remains live.", r.Version.ID, page.PublishedVersionID)
	case r.Gated:
		next = fmt.Sprintf("Approval is required. Version %s is pending and no page version is currently live.", r.Version.ID)
	case publishArg == false:
		next = fmt.Sprintf("Draft canary %s was saved without moving the live pointer. Inspect it in urls.admin before a publishing call.", r.Version.ID)
	default:
		next = "The pointer changed concurrently after this update; call get_page_data before retrying."
	}
	warnings := r.DataWarnings
	if warnings == nil {
		warnings = []pagedata.Warning{}
	}
	out := jsonv.ObjectOf(
		"version", r.Version,
		"envelope", r.Envelope,
		"data_sha256", r.DataSHA256,
		"schema_sha256", r.SchemaSHA256,
		"template_sha256", r.TemplateSHA256,
		"deduped", r.Deduped,
		"published", r.Published,
		"gated", r.Gated,
		"version_is_live", versionIsLive,
		"page_is_live", pageIsLive,
		"live_version_id", idOrNull(page.PublishedVersionID),
		// A warning nobody reads is not a safeguard. next_step is the one
		// field an agent reliably acts on, so the count and the codes go
		// there rather than hoping data_warnings gets inspected.
		"next_step", dataUpdateNextStep(next, warningsOf(warnings)),
		"urls", pageurls.For(slug),
		"data_profile", r.DataProfile,
		"data_warnings", warnings,
	)
	// Issue #109: the written envelope against the managed-data limit.
	if r.Envelope != nil {
		payload := len(jsonv.Stringify(r.Envelope.Value("data")))
		out.Set("data_usage", datalimits.UsageOf(payload, len(datalimits.EscapedJSON(r.Envelope))))
	}
	return out, nil
}

func dataUpdateInput(a *jsonv.Object, slug string, data any) versions.DataUpdateInput {
	return versions.DataUpdateInput{
		Slug:            slug,
		Data:            data,
		SourceAsOf:      argAny(a, "source_as_of"),
		ExpectedVersion: argAny(a, "expected_version"),
		Publish:         argBoolOr(a, "publish", true),
		Note:            argAny(a, "note"),
		Expect:          a.Value("expect"),
	}
}

func publishArg(a *jsonv.Object) any {
	if v, ok := a.Get("publish"); ok {
		return v
	}
	return nil
}

func updatePageData(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	data := a.Value("data")
	if _, err := AssertInlineData(data, argAny(a, "source_as_of")); err != nil {
		return nil, err
	}
	slug, _ := argStr(a, "slug")
	r, err := versions.UpdatePageData(ctx, c.d, dataUpdateInput(a, slug, data), c.actor)
	if err != nil {
		return nil, err
	}
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	return dataUpdateResult(r, pageState{Disabled: pv.Page.Disabled, PublishedVersionID: pv.Page.PublishedVersionID}, pv.Page.Slug, publishArg(a))
}

func getPageConfig(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	r, err := versions.GetPageConfig(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	binding, err := templates.PageTemplateBinding(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	var template any
	if binding != nil {
		template = jsonv.ObjectOf(
			"name", binding.Template,
			"revision", binding.Revision,
			"is_current_revision", binding.IsCurrentRevision,
			"current_revision", binding.CurrentRevision,
		)
	}
	return spread(r, jsonv.ObjectOf("template", template), publishedLiveState(r.Page), jsonv.ObjectOf("urls", pageurls.For(r.Page.Slug)))
}

func updatePageConfig(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	r, err := versions.UpdatePageConfig(ctx, c.d, versions.ConfigUpdateInput{
		Slug:            slug,
		Config:          a.Value("config"),
		ExpectedVersion: argAny(a, "expected_version"),
		Publish:         argBoolOr(a, "publish", true),
		Note:            argAny(a, "note"),
	}, c.actor)
	if err != nil {
		return nil, err
	}
	normalized, err := versions.NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	page, err := currentPageState(ctx, c, slug)
	if err != nil {
		return nil, err
	}
	return spread(r, deployGuidance(&r.DeployResult, page, normalized))
}
