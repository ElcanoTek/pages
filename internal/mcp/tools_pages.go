// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Page, version and serving tools.

import (
	"context"
	"fmt"
	"regexp"

	"github.com/ElcanoTek/pages/internal/agentguide"
	"github.com/ElcanoTek/pages/internal/api"
	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagepatch"
	"github.com/ElcanoTek/pages/internal/pageurls"
	"github.com/ElcanoTek/pages/internal/preflight"
	"github.com/ElcanoTek/pages/internal/uploads"
	"github.com/ElcanoTek/pages/internal/versions"
)

func analyze(html, renderMode string) preflight.Report {
	if renderMode == "" {
		renderMode = "themed"
	}
	return preflight.Analyze(html, api.PreflightOptions(renderMode))
}

func normalizedSlug(a *jsonv.Object) (string, error) {
	s, _ := argStr(a, "slug")
	return versions.NormalizeSlug(s)
}

// ── list_pages / list_versions ──────────────────────────────────────────────

func hasOr(a *jsonv.Object, key string) any {
	if v, ok := a.Get(key); ok {
		return v
	}
	return "*"
}

func pageFilters(a *jsonv.Object) *jsonv.Object {
	var query any
	if s, ok := argStr(a, "query"); ok && s != "" {
		query = s
	}
	return jsonv.ObjectOf(
		"query", query,
		"workspace_id", hasOr(a, "workspace_id"),
		"client_id", hasOr(a, "client_id"),
		"is_live", hasOr(a, "is_live"),
		"require_approval", hasOr(a, "require_approval"),
		"disabled", hasOr(a, "disabled"),
	)
}

func validKeysetAfter(after *jsonv.Object) bool {
	_, isStr := after.Value("created_at").(string)
	return isStr && decimalIDRE.MatchString(jsToString(after.Value("id"), after.Has("id")))
}

// jsToString is String(v), with undefined for an absent value.
func jsToString(v any, present bool) string {
	if !present {
		return "undefined"
	}
	return jsonv.ToString(v)
}

func boolPtrArg(a *jsonv.Object, key string) *bool {
	if b, ok := argBool(a, key); ok {
		return &b
	}
	return nil
}

func listPages(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	filters := pageFilters(a)
	cursor, _ := argStr(a, "cursor")
	after, err := decodeCursor(cursor, "pages", filters)
	if err != nil {
		return nil, err
	}
	if after != nil && !validKeysetAfter(after) {
		return nil, apierror.BadRequest("invalid pagination cursor", "invalid_cursor", nil)
	}
	limit, _ := argInt(a, "limit")
	fetch := limit + 1
	opts := versions.ListPagesOptions{Limit: &fetch}
	if after != nil {
		opts.After = &versions.After{CreatedAt: after.Value("created_at"), ID: after.Value("id")}
	}
	opts.Query = argStrPtr(a, "query")
	if v, ok := a.Get("workspace_id"); ok {
		opts.WorkspaceFilterSet = true
		opts.WorkspaceID = versions.Arg(v, true)
	}
	if v, ok := a.Get("client_id"); ok {
		opts.ClientIDSet = true
		if s, isStr := v.(string); isStr {
			opts.ClientID = &s
		}
	}
	opts.IsLive = boolPtrArg(a, "is_live")
	opts.RequireApproval = boolPtrArg(a, "require_approval")
	opts.Disabled = boolPtrArg(a, "disabled")
	rows, err := versions.ListPages(ctx, c.d, opts)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	pages := make([]any, len(rows))
	for i := range rows {
		o, err := spread(rows[i], jsonv.ObjectOf("urls", pageurls.For(rows[i].Slug)))
		if err != nil {
			return nil, err
		}
		pages[i] = o
	}
	var next any
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = encodeCursor("pages", jsonv.ObjectOf("created_at", jsx.FormatTime(last.CreatedAt.Time), "id", last.ID.String()), filters)
	}
	return jsonv.ObjectOf("pages", pages, "next_cursor", next), nil
}

func listVersions(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	var status any
	if s, ok := argStr(a, "status"); ok && s != "" {
		status = s
	}
	filters := jsonv.ObjectOf("slug", slug, "status", status)
	cursor, _ := argStr(a, "cursor")
	after, err := decodeCursor(cursor, "versions", filters)
	if err != nil {
		return nil, err
	}
	if after != nil && !validKeysetAfter(after) {
		return nil, apierror.BadRequest("invalid pagination cursor", "invalid_cursor", nil)
	}
	limit, _ := argInt(a, "limit")
	fetch := limit + 1
	opts := versions.ListVersionsOptions{Limit: &fetch, Status: argStrPtr(a, "status")}
	if after != nil {
		opts.After = &versions.After{CreatedAt: after.Value("created_at"), ID: after.Value("id")}
	}
	rows, err := versions.ListVersions(ctx, c.d, slug, opts)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	var next any
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = encodeCursor("versions", jsonv.ObjectOf("created_at", jsx.FormatTime(last.CreatedAt.Time), "id", last.ID.String()), filters)
	}
	if rows == nil {
		rows = []versions.ListedVersion{}
	}
	return jsonv.ObjectOf("versions", rows, "next_cursor", next), nil
}

// ── reads ───────────────────────────────────────────────────────────────────

func getPage(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	var published any
	if pv.Published != nil {
		if argBoolOr(a, "include_html", false) {
			published = pv.Published
		} else {
			published = pv.Published.Summary()
		}
	}
	return jsonv.ObjectOf(
		"page", pv.Page,
		"published", published,
		"is_live", pv.Page.PublishedVersionID != 0 && !pv.Page.Disabled,
		"urls", pageurls.For(pv.Page.Slug),
	), nil
}

func getPageRefresh(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf(
		"page", pv.Page,
		"scheduling", "user_owned",
		"refresh", nil,
		"runs", []any{},
		"next_step", "Ask for the complete update/source instructions, then call prepare_dashboard_update. If that tool is hidden by an older static client allowlist, call configure_page_refresh with the same new arguments. Show a recurring prompt to the user; Pages has no schedule to inspect.",
	), nil
}

func recordRefreshCheck(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	outcome, _ := argStr(a, "outcome")
	in := versions.RefreshCheckInput{Slug: slug, Outcome: outcome, Detail: versions.Null, SourceAsOfSeen: versions.Null}
	if s, ok := argStr(a, "detail"); ok {
		in.Detail = s
	}
	if s, ok := argStr(a, "source_as_of_seen"); ok {
		in.SourceAsOfSeen = s
	}
	return versions.RecordRefreshCheck(ctx, c.d, in, c.actor)
}

func getVersion(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	id, _ := argID(a, "version_id")
	v, err := versions.GetVersion(ctx, c.d, slug, id)
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf("version", v), nil
}

func listThemes(ctx context.Context, c *call, _ *jsonv.Object) (any, error) {
	themes, err := versions.ListThemes(ctx, c.d)
	if err != nil {
		return nil, err
	}
	if themes == nil {
		themes = []versions.Theme{}
	}
	return jsonv.ObjectOf("themes", themes), nil
}

func pageURLsTool(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	return pageurls.For(pv.Page.Slug), nil
}

func getGuide(_ context.Context, _ *call, a *jsonv.Object) (any, error) {
	return agentguide.Get(argStrPtr(a, "topic"), argStrPtr(a, "section"))
}

func renderGuide(result any) string {
	if r, ok := result.(*agentguide.Result); ok {
		return agentguide.Render(r)
	}
	return jsonv.Stringify(result)
}

// ── version lookup helpers ──────────────────────────────────────────────────

// publishedOrGiven resolves version_id, defaulting to the published version.
func publishedOrGiven(ctx context.Context, c *call, a *jsonv.Object, slug, missing string) (jsx.ID, error) {
	if id, ok := argID(a, "version_id"); ok {
		return id, nil
	}
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return 0, err
	}
	if pv.Page.PublishedVersionID == 0 {
		return 0, apierror.BadRequest(missing, "no_published_version", nil)
	}
	return pv.Page.PublishedVersionID, nil
}

func findInVersion(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, err := normalizedSlug(a)
	if err != nil {
		return nil, err
	}
	id, err := publishedOrGiven(ctx, c, a, slug, fmt.Sprintf("page %s has no published version; pass version_id", slug))
	if err != nil {
		return nil, err
	}
	v, err := versions.GetVersion(ctx, c.d, slug, id)
	if err != nil {
		return nil, err
	}
	query, _ := argStr(a, "query")
	opts := pagepatch.FindOptions{IgnoreCase: argBoolOr(a, "ignore_case", false)}
	if n, ok := argInt(a, "max_matches"); ok {
		opts.MaxMatches = n
	}
	found, err := pagepatch.FindMatches(v.HTML, query, opts)
	if err != nil {
		return nil, err
	}
	return spread(jsonv.ObjectOf("version_id", v.ID.String()), found)
}

// Preflight a stored version. Note what this deliberately does NOT do:
// return the HTML. Reading a 65 KB dashboard back just to look for a broken
// button was costing agents more context than authoring it did, so the bytes
// stay here and only the findings travel.
func preflightPage(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, err := normalizedSlug(a)
	if err != nil {
		return nil, err
	}
	id, err := publishedOrGiven(ctx, c, a, slug, fmt.Sprintf("page %s has no published version; pass version_id to preflight a draft", slug))
	if err != nil {
		return nil, err
	}
	v, err := versions.GetVersion(ctx, c.d, slug, id)
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf("version_id", v.ID.String(), "preflight", analyze(v.HTML, v.RenderMode)), nil
}

// ── deploys ─────────────────────────────────────────────────────────────────

// pageState is the serving state deployGuidance reads.
type pageState struct {
	Disabled           bool
	PublishedVersionID jsx.ID
}

// deployGuidance is the serving truth and next step every deploy-shaped
// result carries.
func deployGuidance(r *versions.DeployResult, page pageState, slug string) *jsonv.Object {
	versionIsLive := !page.Disabled && sameID(r.Version.ID, page.PublishedVersionID)
	pageIsLive := page.PublishedVersionID != 0 && !page.Disabled
	var next string
	switch {
	case versionIsLive:
		next = "This version is live now — share urls.live with the user or client."
	case page.Disabled:
		next = "The version was saved, but the page is disabled by an admin and is not serving. Ask a human admin to re-enable it; do not share urls.live yet."
	case r.Gated:
		prior := " Nothing is published yet, so urls.live does not serve a page."
		if pageIsLive {
			prior = fmt.Sprintf(" The previously published version %s remains live at urls.live until review completes.", page.PublishedVersionID)
		}
		if r.Version.Status == "approved" {
			next = fmt.Sprintf("This exact content already exists as approved version %s, but it is not the published version. A human must publish it from urls.admin.%s", r.Version.ID, prior)
		} else {
			next = "Approval-gated page: this version is pending human review. Send the user urls.admin; agents cannot approve it." + prior
		}
	case r.Version.Status == "approved":
		next = fmt.Sprintf("This exact content already exists as approved version %s. Call rollback_page with that version_id to publish it.", r.Version.ID)
	default:
		next = fmt.Sprintf("Draft saved. Call publish_page with version_id %s to publish it.", r.Version.ID)
	}
	return jsonv.ObjectOf(
		"version", r.Version,
		"deduped", r.Deduped,
		"published", r.Published,
		"gated", r.Gated,
		"live", versionIsLive,
		"version_is_live", versionIsLive,
		"page_is_live", pageIsLive,
		"live_version_id", idOrNull(page.PublishedVersionID),
		"next_step", next,
		"urls", pageurls.For(slug),
	)
}

func currentPageState(ctx context.Context, c *call, slug string) (pageState, error) {
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return pageState{}, err
	}
	return pageState{Disabled: pv.Page.Disabled, PublishedVersionID: pv.Page.PublishedVersionID}, nil
}

// jsSpace is JavaScript's \s.
const jsSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

var filePlaceholderRE = regexp.MustCompile(`(?i)^(?:\$\(` + jsSpace + `*cat(?:` + jsSpace + `+[^)]*)?` + jsSpace + `*\)|PLACEHOLDER|REPLACE_ME|FILE_CONTENT_PLACEHOLDER)$`)

// AssertMcpHTML is the check that applies to page HTML however it arrived
// (lib/mcp-tools.js assertMcpHtml). Staged bytes stop here: a 140 KB file is
// not a mistake on that path, it is the entire point of it. It returns the
// trimmed HTML.
func AssertMcpHTML(html string) (string, error) {
	trimmed := jsstr.Trim(html)
	if trimmed == "" {
		return "", apierror.BadRequest("html is required", "html_required", nil)
	}
	if filePlaceholderRE.MatchString(trimmed) {
		return "", apierror.BadRequest(
			"html is a literal file placeholder, not page content; use the staged page-upload tools for workspace files",
			"html_placeholder", nil)
	}
	return trimmed, nil
}

type inlineHTMLDetails struct {
	Bytes    int `json:"bytes"`
	MaxBytes int `json:"max_bytes"`
}

// AssertInlineHTML is the extra check for html passed INLINE as a tool
// argument, where every byte is output once and prompt on every later step.
func AssertInlineHTML(html string) (string, error) {
	trimmed, err := AssertMcpHTML(html)
	if err != nil {
		return "", err
	}
	limit := MaxInlineHTMLBytes()
	if n := len(trimmed); n > limit {
		return "", apierror.BadRequest(
			fmt.Sprintf("html is %d bytes; inline deploys are capped at %d. ", n, limit)+
				"Every inline byte is output you pay for once and prompt you pay for on every later step. "+
				"If this is another instance of a design Pages already stores, call list_templates and then "+
				"create_page_from_template with just its config (kilobytes). Otherwise write the file to disk and "+
				"use create_upload_ticket, which returns a URL your shell PUTs the file to — the bytes never pass "+
				"through your context at all.",
			"html_too_large_for_inline", inlineHTMLDetails{Bytes: n, MaxBytes: limit})
	}
	return trimmed, nil
}

func deployInput(a *jsonv.Object, slug, html string) versions.DeployInput {
	return versions.DeployInput{
		Slug:            slug,
		HTML:            html,
		RenderMode:      argAny(a, "render_mode"),
		Note:            argAny(a, "note"),
		Source:          versions.SourceMCP,
		Publish:         argBoolOr(a, "publish", true),
		ExpectedVersion: argAny(a, "expected_version"),
	}
}

func createdOnlyInput(a *jsonv.Object, in versions.DeployInput) versions.CreateAndDeployInput {
	title, _ := argStr(a, "title")
	clientID := versions.Null
	if s, ok := argStr(a, "client_id"); ok && s != "" {
		clientID = s
	}
	return versions.CreateAndDeployInput{
		DeployInput:     in,
		Title:           title,
		ClientID:        clientID,
		RequireApproval: argBoolOr(a, "require_approval", false),
	}
}

func deployPage(ctx context.Context, c *call, a *jsonv.Object, requireExisting bool) (*jsonv.Object, error) {
	html, _ := argStr(a, "html")
	if _, err := AssertInlineHTML(html); err != nil {
		return nil, err
	}
	slug, _ := argStr(a, "slug")
	in := deployInput(a, slug, html)
	var created bool
	var result *versions.DeployResult
	if requireExisting {
		r, err := versions.Deploy(ctx, c.d, in, c.actor)
		if err != nil {
			return nil, err
		}
		result = r
	} else {
		r, err := versions.CreateAndDeploy(ctx, c.d, createdOnlyInput(a, in), c.actor)
		if err != nil {
			return nil, err
		}
		created, result = r.Created, &r.DeployResult
	}
	normalized, err := versions.NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	page, err := currentPageState(ctx, c, slug)
	if err != nil {
		return nil, err
	}
	out, err := spread(jsonv.ObjectOf("created", created), deployGuidance(result, page, normalized))
	if err != nil {
		return nil, err
	}
	// Passed through explicitly: this function shapes its output rather
	// than spreading the deploy result, so a new field is dropped unless
	// named here.
	if result.ConfigSchemaGenerated {
		out.Set("config_schema_generated", true)
	}
	mode, _ := argStr(a, "render_mode")
	out.Set("preflight", analyze(html, mode))
	return out, nil
}

func deployPageTool(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	return deployPage(ctx, c, a, false)
}

func updatePageTool(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	out, err := deployPage(ctx, c, a, true)
	if err != nil {
		return nil, err
	}
	return without(out, "created"), nil
}

func publishedResult(ctx context.Context, c *call, a *jsonv.Object, operation string) (any, error) {
	slug, err := normalizedSlug(a)
	if err != nil {
		return nil, err
	}
	versionID, _ := argID(a, "version_id")
	var v *versions.Version
	if operation == "publish" {
		v, err = versions.Publish(ctx, c.d, versions.PublishInput{Slug: slug, VersionID: versionID, ExpectedVersion: argAny(a, "expected_version")}, c.actor)
	} else {
		note := argAny(a, "note")
		if note == nil {
			note = versions.Null
		}
		v, err = versions.Rollback(ctx, c.d, versions.RollbackInput{Slug: slug, VersionID: versionID, ExpectedVersion: argAny(a, "expected_version"), Note: note}, c.actor)
	}
	if err != nil {
		return nil, err
	}
	page, err := currentPageState(ctx, c, slug)
	if err != nil {
		return nil, err
	}
	versionIsLive := !page.Disabled && sameID(v.ID, page.PublishedVersionID)
	next := "The live pointer changed again; reload get_page before sharing a URL."
	if versionIsLive {
		next = "Live now — share urls.live with the user or client."
	}
	return jsonv.ObjectOf(
		"version", v,
		"live", versionIsLive,
		"version_is_live", versionIsLive,
		"page_is_live", page.PublishedVersionID != 0 && !page.Disabled,
		"live_version_id", idOrNull(page.PublishedVersionID),
		"next_step", next,
		"urls", pageurls.For(slug),
	), nil
}

func publishPage(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	return publishedResult(ctx, c, a, "publish")
}

func rollbackPage(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	return publishedResult(ctx, c, a, "rollback")
}

// patchPage patches the live document in place. The base HTML is read,
// edited and redeployed entirely server-side, so a one-line CSS fix costs
// anchors instead of a whole document in each direction.
func patchPage(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, err := normalizedSlug(a)
	if err != nil {
		return nil, err
	}
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	baseID, hasBase := argID(a, "base_version_id")
	if pv.Page.PublishedVersionID == 0 && !hasBase {
		return nil, apierror.BadRequest(
			fmt.Sprintf("page %s has no published version to patch; deploy one first, or pass base_version_id", slug),
			"no_published_version", nil)
	}
	if !hasBase {
		baseID = pv.Page.PublishedVersionID
	}
	base, err := versions.GetVersion(ctx, c.d, slug, baseID)
	if err != nil {
		return nil, err
	}
	edits, err := pagepatch.ParseEdits(a.Value("edits"))
	if err != nil {
		return nil, err
	}
	patched, err := pagepatch.ApplyEdits(base.HTML, edits)
	if err != nil {
		return nil, err
	}
	if patched.HTML == base.HTML {
		return nil, apierror.BadRequest("the edits produced no change; check that find and replace actually differ", "patch_noop", nil)
	}
	n := len(patched.HTML)
	if limit := uploads.MaxUploadBytes(); n > limit {
		return nil, apierror.BadRequest(fmt.Sprintf("patched document would be %d bytes, over the %d limit", n, limit), "upload_size_invalid", nil)
	}
	renderMode, ok := argStr(a, "render_mode")
	if !ok || renderMode == "" {
		renderMode = base.RenderMode
	}
	// Default the concurrency check to the POINTER we read, not the version
	// we patched: expected_version is compared against
	// published_version_id, so defaulting it to the base meant any explicit
	// base_version_id naming a version that is not the live one failed
	// stale_version every time, against a page nobody else had touched. On a
	// page with nothing published the pointer is null and the check does not
	// apply. The default can only catch a publish landing between the read
	// above and the row lock; pass an explicit expected_version from an
	// EARLIER turn for a real guarantee.
	expected := argAny(a, "expected_version")
	if expected == nil {
		if pv.Page.PublishedVersionID != 0 {
			expected = pv.Page.PublishedVersionID.String()
		} else {
			expected = versions.Null
		}
	}
	in := versions.DeployInput{
		Slug:            slug,
		HTML:            patched.HTML,
		RenderMode:      renderMode,
		Note:            argAny(a, "note"),
		Source:          versions.SourceMCP,
		Publish:         argBoolOr(a, "publish", true),
		ExpectedVersion: expected,
	}
	result, err := versions.Deploy(ctx, c.d, in, c.actor)
	if err != nil {
		return nil, err
	}
	after, err := currentPageState(ctx, c, slug)
	if err != nil {
		return nil, err
	}
	out := deployGuidance(result, after, slug)
	applied := patched.Applied
	if applied == nil {
		applied = []pagepatch.Applied{}
	}
	out.Set("patch", jsonv.ObjectOf(
		"base_version_id", baseID.String(),
		"edits_applied", applied,
		"bytes_before", float64(len(base.HTML)),
		"bytes_after", float64(n),
	))
	out.Set("preflight", analyze(patched.HTML, renderMode))
	return out, nil
}

// ── page settings ───────────────────────────────────────────────────────────

func setPassword(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	password, _ := argStr(a, "password")
	result, err := versions.SetPassword(ctx, c.d, slug, password, c.actor)
	if err != nil {
		return nil, err
	}
	pv, err := versions.GetPage(ctx, c.d, slug)
	if err != nil {
		return nil, err
	}
	page := pv.Page
	var next string
	switch {
	case page.Disabled:
		next = "Password saved, but the page is disabled and not serving. Ask a human admin to re-enable it before sharing urls.live."
	case page.PublishedVersionID == 0:
		next = "Password saved, but no version is published. Publish or complete human approval before sharing urls.live."
	default:
		next = "Client access is ready — share urls.live and the password with the client."
	}
	return spread(result, jsonv.ObjectOf(
		"page_is_live", page.PublishedVersionID != 0 && !page.Disabled,
		"urls", pageurls.For(page.Slug),
		"next_step", next,
	))
}

func setTitle(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	title, _ := argStr(a, "title")
	return versions.SetTitle(ctx, c.d, slug, title, c.actor)
}

func deletePage(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	result, err := versions.DeletePage(ctx, c.d, slug, c.actor)
	if err != nil {
		return nil, err
	}
	return spread(result, jsonv.ObjectOf(
		"live", false,
		"page_is_live", false,
		"note", "Soft-deleted; a human admin can restore it. The slug is free to reuse.",
	))
}
