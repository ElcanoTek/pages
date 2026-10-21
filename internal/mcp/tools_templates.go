// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Template tools: one stored design, many pages.

import (
	"context"
	"fmt"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/uploads"
	"github.com/ElcanoTek/pages/internal/versions"
)

// TemplatePreviewTTLSeconds matches the admin library's preview-token TTL:
// long enough to open a link, short enough that a leaked URL is not a
// standing capability.
const TemplatePreviewTTLSeconds = 300

func listTemplates(ctx context.Context, c *call, _ *jsonv.Object) (any, error) {
	return templates.List(ctx, c.d)
}

func getTemplate(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	name, _ := argStr(a, "template")
	return templates.Get(ctx, c.d, name, argIntPtr(a, "revision"), argBoolOr(a, "include_html", false))
}

func listTemplateRevisions(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	name, _ := argStr(a, "template")
	return templates.Revisions(ctx, c.d, name)
}

func listTemplatePages(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	name, _ := argStr(a, "template")
	return templates.ListTemplatePages(ctx, c.d, name)
}

func createTemplateFromPage(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	name, _ := argStr(a, "template")
	result, err := templates.CreateFromPage(ctx, c.d, templates.PromoteInput{
		Slug:                   slug,
		Name:                   name,
		EmptyData:              a.Value("empty_data"),
		ExampleFromCurrentData: argBoolOr(a, "example_from_current_data", false),
		Title:                  argStrPtr(a, "title"),
		Description:            argStrPtr(a, "description"),
		Note:                   argStrPtr(a, "note"),
	}, c.actor)
	if err != nil {
		return nil, err
	}
	out, err := toObject(result)
	if err != nil {
		return nil, err
	}
	out.Set("next_step", templateNextStep(out))
	return out, nil
}

func validateTemplate(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	uploadID, hasUpload := argStr(a, "upload_id")
	html, hasHTML := argStr(a, "html")
	if hasUpload == hasHTML {
		return nil, apierror.BadRequest("pass exactly one of upload_id or html", "template_source_ambiguous", nil)
	}
	var name any
	if s, ok := argStr(a, "name"); ok {
		name = s
	}
	if hasUpload {
		// Peek verifies the staged bytes exactly as registration will —
		// target kind, completeness, SHA-256, UTF-8 — and leaves the upload
		// intact.
		staged, err := uploads.Peek(ctx, c.d, uploadID, c.actor, uploads.PeekOptions{ExpectKind: uploads.KindTemplate})
		if err != nil {
			return nil, err
		}
		html = staged.HTML
		// The upload already names the template, so the caller need not
		// repeat it.
		if name == nil && staged.Template != nil {
			name = *staged.Template
		}
	}
	report := templates.ValidateHTML(html, name)
	preflightFailed := report.Preflight != nil && !report.Preflight.OK
	var next string
	switch {
	case !report.ContractOK:
		next = "Fix the blocks named in contract_error, then validate again. Nothing was written."
	case report.NameError != nil:
		next = "The design is valid but the name is not; pick a url-safe name and validate again."
	case preflightFailed:
		next = "Valid, but preflight found errors that every page built from this would inherit. Fix the design, or register deliberately with allow_preflight_errors."
	case hasUpload:
		next = "Valid. Call register_template_upload with this same upload_id."
	default:
		next = "Valid. Stage the file with create_upload_ticket, then register_template_upload."
	}
	var upload any
	if hasUpload {
		upload = uploadID
	}
	return spread(jsonv.ObjectOf("upload_id", upload), report, jsonv.ObjectOf("next_step", next))
}

func deleteTemplate(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	name, _ := argStr(a, "template")
	result, err := templates.Remove(ctx, c.d, name, argBoolOr(a, "force", false), c.actor)
	if err != nil {
		return nil, err
	}
	note := "The name is free to re-register."
	if result.PagesBuilt > 0 {
		note = fmt.Sprintf("%d page(s) built from this keep serving, but can no longer be re-rendered from it. The name is free to re-register.", result.PagesBuilt)
	}
	return spread(result, jsonv.ObjectOf("note", note))
}

func templateURLs(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	name, _ := argStr(a, "template")
	target, err := templates.GetRevisionHTML(ctx, c.d, name, argIntPtr(a, "revision"))
	if err != nil {
		return nil, err
	}
	token, err := rawtoken.Mint(rawtoken.MintParams{
		PageID:     0,
		VersionID:  int64(target.TemplateVersionID),
		Purpose:    rawtoken.Template,
		RenderMode: "themed",
	}, TemplatePreviewTTLSeconds)
	if err != nil {
		return nil, err
	}
	return jsonv.ObjectOf(
		"template", target.Template,
		"revision", target.Revision,
		"library", env.DashboardOrigin()+"/admin/templates",
		"preview", fmt.Sprintf("%s/raw-template/%s?t=%s", env.ContentOrigin(), target.TemplateVersionID, httpx.EncodeURIComponent(token)),
		"expires_in_seconds", TemplatePreviewTTLSeconds,
		"has_sample_data", target.HasSampleData,
	), nil
}

func rerenderPageFromTemplate(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	slug, _ := argStr(a, "slug")
	in := templates.RerenderInput{
		Slug:            slug,
		Revision:        argIntPtr(a, "revision"),
		ExpectedVersion: argAny(a, "expected_version"),
		Publish:         argBoolOr(a, "publish", false),
		Note:            argAny(a, "note"),
		Config:          argAny(a, "config"),
		Data:            argAny(a, "data"),
		SourceAsOf:      argAny(a, "source_as_of"),
	}
	if s, ok := argStr(a, "template"); ok {
		in.Template = s
	}
	result, err := templates.RerenderPage(ctx, c.d, in, c.actor)
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
	rest, err := toObject(result)
	if err != nil {
		return nil, err
	}
	out, err := spread(without(rest, "html"), deployGuidance(&result.DeployResult, page, normalized))
	if err != nil {
		return nil, err
	}
	out.Set("preflight", analyze(result.HTML, result.Version.RenderMode))
	return out, nil
}

func createPageFromTemplate(ctx context.Context, c *call, a *jsonv.Object) (any, error) {
	if a.Has("data") && !a.Has("source_as_of") {
		return nil, apierror.BadRequest(
			"source_as_of is required when data is supplied; it is the coverage that data represents",
			"source_as_of_invalid", nil)
	}
	name, _ := argStr(a, "template")
	slug, _ := argStr(a, "slug")
	renderMode, ok := argStr(a, "render_mode")
	if !ok || renderMode == "" {
		renderMode = "themed"
	}
	title, _ := argStr(a, "title")
	clientID := versions.Null
	if s, ok := argStr(a, "client_id"); ok && s != "" {
		clientID = s
	}
	in := templates.CreatePageInput{
		Template:        name,
		Revision:        argIntPtr(a, "revision"),
		Slug:            slug,
		Config:          a.Value("config"),
		Data:            versions.Null,
		SourceAsOf:      versions.Null,
		Title:           title,
		ClientID:        clientID,
		RequireApproval: argBoolOr(a, "require_approval", false),
		RenderMode:      renderMode,
		Publish:         argBoolOr(a, "publish", true),
		Note:            argAny(a, "note"),
	}
	if v, ok := a.Get("data"); ok {
		in.Data = v
	}
	if v, ok := a.Get("source_as_of"); ok {
		in.SourceAsOf = v
	}
	result, err := templates.CreatePage(ctx, c.d, in, c.actor)
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
	rest, err := toObject(result)
	if err != nil {
		return nil, err
	}
	out, err := spread(without(rest, "html"), deployGuidance(&result.DeployResult, page, normalized),
		jsonv.ObjectOf("created", result.Created))
	if err != nil {
		return nil, err
	}
	out.Set("preflight", analyze(result.HTML, renderMode))
	return out, nil
}
