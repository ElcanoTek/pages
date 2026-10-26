// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package templates

import (
	"context"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/versions"
)

// CreatePageInput builds a page from a template. The `any` fields follow the
// versions input convention (nil = omitted, versions.Null = explicit null).
// Config is required and must be a complete JSON object; Data nil/null keeps
// the template's empty state. RenderMode nil means "themed"; Publish nil
// means true. Now zero means time.Now().
type CreatePageInput struct {
	Template        any
	Revision        *int
	Slug            any
	Config          any
	Data            any
	SourceAsOf      any
	Title           any
	ClientID        any
	WorkspaceID     any
	RequireApproval any
	RenderMode      any
	Publish         any
	Note            any
	Now             time.Time
}

// CreatePage is the cost win, in one call. The caller sends the config that
// actually differs plus (optionally) data; the server renders the design it
// already stores. Config is ALWAYS required in full and never inherited from
// the template's reference config: a partially-specified campaign that
// silently kept another client's identity is exactly the failure a template
// makes easy, so it is refused by construction.
func CreatePage(ctx context.Context, d *db.DB, in CreatePageInput, a versions.Actor) (*CreatePageResult, error) {
	slug, err := versions.NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	config := asObject(in.Config)
	if config == nil {
		return nil, apierror.BadRequest("config is required and must be a complete object", "config_required", nil)
	}
	renderMode := in.RenderMode
	if renderMode == nil {
		renderMode = "themed"
	}
	publish := in.Publish
	if publish == nil {
		publish = true
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	return inTx(ctx, d, func(q db.Querier) (*CreatePageResult, error) {
		tmpl, err := LoadCurrent(ctx, q, in.Template, in.Revision)
		if err != nil {
			return nil, err
		}
		parsed, err := ParseTemplateHTML(tmpl.HTML)
		if err != nil {
			return nil, err
		}
		// Data is optional: a campaign is normally deployed empty and filled by
		// the first update_page_data. When omitted, the template's own
		// empty-state data is kept — that is what makes its
		// awaiting-first-ingest states render instead of fabricated zeros — but
		// its source coverage is reset to the epoch rather than inherited.
		// Source coverage is monotonic per page, so inheriting whatever
		// timestamp the template happened to ship would put a floor under the
		// page and make the first ingest of older data fail with
		// source_regression.
		update := pagedata.BlockUpdate{Config: config, SetConfig: true, SetData: true}
		opts := pagedata.MaterializeOptions{Now: now}
		if isNullish(in.Data) {
			update.Data = parsed.Data()
			opts.SourceAsOf = noSourceCoverage
		} else {
			update.Data = in.Data
			opts.SourceAsOf = unNull(in.SourceAsOf)
		}
		m, err := pagedata.MaterializeBlocks(parsed, update, opts)
		if err != nil {
			return nil, err
		}

		var renderModeParam any
		if s, ok := renderMode.(string); ok {
			renderModeParam = s
		}
		assertExistingPage := func(ctx context.Context, existing *versions.LockedPage) error {
			// The create-or-deploy helper invokes this only after locking the
			// final existing page, including when another transaction won the
			// INSERT. An earlier SELECT cannot protect an absent slug from a
			// concurrent create. Is this the SAME build arriving twice? Compare
			// the semantic identity the data path already dedupes on — not the
			// rendered bytes, which differ every call because Pages stamps
			// refreshed_at. template_sha256 covers the design and config;
			// render mode and template revision must also match so a different
			// build cannot qualify as a retry on identical payloads.
			//
			// Scoped to what the page is CURRENTLY SERVING, and that scope is
			// the whole guard. Matching any historical version made the retry
			// allowance mean something it was never meant to: after the page
			// took a refresh and a config edit, replaying the original create
			// matched the FIRST version, the deploy deduped onto it, and —
			// because that row is not the pointer — publish moved the live
			// pointer BACKWARD. A real client dashboard reverted to its
			// awaiting-first-ingest state, discarding every refresh since, and
			// the result said deduped:true, version_is_live:true and "share
			// urls.live".
			//
			// When nothing is published (pointer NULL) the match must instead be
			// the page's NEWEST version. Three things reach a NULL pointer and
			// only two of them are a died-mid-turn retry: a brand-new gated page
			// whose version is still pending, and a create that died before
			// publishing — in both the build IS the newest row. The third is an
			// admin restore_page, which clears deleted_at but leaves the pointer
			// NULL, so a restored page has a NULL pointer AND a full history.
			// Allowing any match there would republish the original empty state
			// over a page with refreshes behind it — the same revert by a
			// different route.
			var one int
			err := q.QueryRow(ctx,
				`SELECT 1 FROM page_versions
				  WHERE page_id = $1 AND data_sha256 = $2 AND data_template_sha256 = $3
				    AND source_as_of = $4::timestamptz AND status <> 'rejected'
				    AND render_mode = $6 AND template_version_id = $7
				    AND id = COALESCE(
				      $5::bigint,
				      (SELECT MAX(id) FROM page_versions WHERE page_id = $1 AND status <> 'rejected')
				    )
				  LIMIT 1`,
				existing.ID, m.DataSHA256, m.TemplateSHA256, m.Envelope.Value("source_as_of"),
				existing.PublishedVersionID, renderModeParam, tmpl.ID).Scan(&one)
			if db.IsNoRows(err) {
				// A turn that died after the commit may safely repeat itself.
				// Anything else is refused: this tool builds a page, and quietly
				// replacing BOTH halves of a live client dashboard is not that.
				// The dedicated tools say what they do.
				return apierror.Conflict(
					"page "+slug+" already exists; use update_page_config to change its settings, "+
						"update_page_data to refresh its numbers, rerender_page_from_template to move it to a new design, "+
						"or rollback_page to deliberately return it to an earlier version",
					"page_exists", nil)
			}
			return err
		}

		binding := &versions.TemplateBinding{TemplateName: tmpl.Name, TemplateVersionID: tmpl.ID,
			Revision: intPtr(tmpl.Revision), ConfigSHA: m.ConfigSHA256}
		result, err := versions.CreateAndDeployWithTx(ctx, q, versions.CreateAndDeployInput{
			DeployInput: versions.DeployInput{
				Slug: slug, HTML: m.HTML, RenderMode: renderMode, Note: in.Note, Source: versions.SourceMCP, Publish: publish,
				DataMetadata: &versions.DataMetadata{Action: "template_build", DataSHA: m.DataSHA256, SchemaSHA: m.SchemaSHA256,
					TemplateSHA: m.TemplateSHA256, SourceAsOf: jsonv.ToString(m.Envelope.Value("source_as_of")),
					RefreshedAt: jsonv.ToString(m.Envelope.Value("refreshed_at"))},
				TemplateBinding: binding,
			},
			Title: in.Title, ClientID: in.ClientID, WorkspaceID: in.WorkspaceID, RequireApproval: in.RequireApproval,
		}, a, versions.CreateAndDeployOptions{AssertExistingPage: assertExistingPage})
		if err != nil {
			return nil, err
		}
		return &CreatePageResult{
			CreateDeployResult: *result,
			Template:           tmpl.Name,
			TemplateRevision:   tmpl.Revision,
			TemplateVersionID:  tmpl.ID,
			Config:             m.Config,
			ConfigSHA256:       m.ConfigSHA256,
			ConfigSchemaSHA256: parsed.ConfigSchemaSHA256,
			Envelope:           m.Envelope,
			DataSHA256:         m.DataSHA256,
			SchemaSHA256:       m.SchemaSHA256,
			TemplateSHA256:     m.TemplateSHA256,
			HTML:               m.HTML,
		}, nil
	})
}

// RerenderInput moves one page onto a template revision. Template nil (or
// "") means the template the page is already on; Revision nil means its
// current revision. Config and Data nil keep the page's own; versions.Null is
// an explicit null (and fails validation). Publish nil means false.
type RerenderInput struct {
	Slug            any
	Template        any
	Revision        *int
	ExpectedVersion any
	Publish         any
	Note            any
	Config          any
	Data            any
	SourceAsOf      any
}

// RerenderPage moves ONE page onto a template revision, keeping its own config
// and data unless complete target replacements are supplied. Published HTML
// supplies the omitted blocks: nothing is re-derived from a stored copy that
// could have drifted from what is actually serving. Publish defaults to false
// so a human previews the new design before a client sees it.
func RerenderPage(ctx context.Context, d *db.DB, in RerenderInput, a versions.Actor) (*RerenderResult, error) {
	slug, err := versions.NormalizeSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	if (in.Config != nil || in.Data != nil) && isNullish(in.ExpectedVersion) {
		return nil, apierror.BadRequest("expected_version is required when migrating template config or data", "expected_version_required", nil)
	}
	if in.Data != nil && isNullish(in.SourceAsOf) {
		return nil, apierror.BadRequest("source_as_of is required with replacement migration data", "source_as_of_required", nil)
	}
	if in.Data == nil && !isNullish(in.SourceAsOf) {
		return nil, apierror.BadRequest("source_as_of requires replacement migration data", "data_required", nil)
	}
	return inTx(ctx, d, func(q db.Querier) (*RerenderResult, error) {
		page, err := versions.LockPage(ctx, q, slug)
		if err != nil {
			return nil, err
		}
		if !isNullish(in.ExpectedVersion) {
			if err := versions.AssertExpectedVersion(page, in.ExpectedVersion); err != nil {
				return nil, err
			}
		}
		notPublished := apierror.Conflict("page has no published version to rerender", "update_page_not_published", nil)
		if page.PublishedVersionID == 0 {
			return nil, notPublished
		}
		var currentHTML, currentMode string
		var currentBinding jsx.ID
		err = q.QueryRow(ctx,
			`SELECT html, render_mode, template_version_id
			   FROM page_versions
			  WHERE id = $1 AND page_id = $2`, page.PublishedVersionID, page.ID).Scan(&currentHTML, &currentMode, &currentBinding)
		if db.IsNoRows(err) {
			return nil, apierror.Conflict("published version is unavailable", "update_page_not_published", nil)
		}
		if err != nil {
			return nil, err
		}
		templateName, _ := in.Template.(string)
		explicit := in.Template != nil && in.Template != versions.Null && (!isString(in.Template) || templateName != "")
		// A DRIFTED page has no binding on its published version: a later raw
		// deploy_page or patch_page replaced the template-built version, and
		// PrepareDeploy writes no binding. Since list_template_pages reports
		// those pages, this is the call that has to be able to act on them —
		// otherwise the report is a dead end that names a problem with no
		// remedy. Re-attaching needs the caller to say WHICH design, because
		// there is no binding left to infer it from.
		if currentBinding == 0 && !explicit {
			return nil, apierror.Conflict(
				"page's published version is not bound to any template, so there is no design to move it to. "+
					"If it drifted off one — list_template_pages reports drifted:true and the last_revision it was "+
					"on — name that template explicitly to re-attach it.",
				"page_not_template_managed", nil)
		}
		// Which template? Default to the one this page is already on, so a
		// caller only has to name the revision they want.
		target := in.Template
		if !explicit {
			var owner string
			if err := q.QueryRow(ctx,
				`SELECT t.name FROM page_template_versions tv
				   JOIN page_templates t ON t.id = tv.template_id
				  WHERE tv.id = $1`, currentBinding).Scan(&owner); err != nil {
				return nil, err
			}
			target = owner
		}
		targetRow, err := LoadCurrent(ctx, q, target, in.Revision)
		if err != nil {
			return nil, err
		}
		// The page's own config and data come from what is SERVING, never from
		// a stored copy that could have drifted from it. That is also what
		// decides whether a drifted page is recoverable: patch_page edits the
		// design around these blocks and leaves them intact, so re-attaching
		// works. A page hand-deployed all the way down to plain HTML has no
		// config or data left to carry, and saying that plainly beats failing
		// on a block-shape error.
		live, err := ParseTemplateHTML(currentHTML)
		if err != nil {
			if e, ok := apierror.As(err); ok && currentBinding == 0 && e.Code != "" {
				return nil, apierror.Conflict(
					"page "+slug+" drifted off its template and its published HTML no longer carries the "+
						"#pages-config and #pages-data blocks a rerender reads, so its config and data cannot be "+
						"recovered from what is serving. Roll back to the last template-built version (list_versions), "+
						"or rebuild it with create_page_from_template. ("+e.Message+")",
					"page_not_template_managed", nil)
			}
			return nil, err
		}
		targetParsed, err := ParseTemplateHTML(targetRow.HTML)
		if err != nil {
			return nil, err
		}
		if targetRow.ID == currentBinding {
			return nil, apierror.Conflict(
				"page "+slug+" is already on "+targetRow.Name+" revision "+jsonv.FormatNumber(float64(targetRow.Revision)),
				"template_revision_unchanged", nil)
		}

		// The page's own config and data are re-validated against the TARGET
		// revision's schemas. A revision that tightened its contract fails
		// here, loudly and before anything is written, instead of producing a
		// page whose payload its own schema rejects.
		update := pagedata.BlockUpdate{SetConfig: true}
		if in.Config == nil {
			update.Config = live.Config
		} else {
			update.Config = unNull(in.Config)
		}
		if in.Data == nil {
			update.Envelope, update.SetEnvelope = live.Envelope, true
		} else {
			update.Data, update.SetData = unNull(in.Data), true
		}
		m, err := pagedata.MaterializeBlocks(targetParsed, update, pagedata.MaterializeOptions{SourceAsOf: unNull(in.SourceAsOf)})
		if err != nil {
			return nil, err
		}
		if in.Data != nil {
			// Replacement data follows the same coverage floor as
			// update_page_data, including unpublished canaries. The page lock
			// serializes both paths.
			floor, err := versions.SourceCoverageFloor(ctx, q, page.ID, live.SourceAsOf())
			if err != nil {
				return nil, err
			}
			src := versions.ParseTime(jsonv.ToString(m.Envelope.Value("source_as_of")))
			if floor != "" && !src.IsZero() && src.Before(versions.ParseTime(floor)) {
				return nil, apierror.Conflict("source_as_of cannot move behind existing source coverage", "source_regression", nil)
			}
		}
		expected := in.ExpectedVersion
		if expected == versions.Null {
			expected = nil
		}
		prepared, err := versions.PrepareDeploy(versions.DeployInput{
			Slug: slug, HTML: m.HTML, RenderMode: currentMode, Note: in.Note, Source: versions.SourceMCP,
			Publish: in.Publish, ExpectedVersion: expected,
			DataMetadata: &versions.DataMetadata{Action: "template_rerender", DataSHA: m.DataSHA256, SchemaSHA: m.SchemaSHA256,
				TemplateSHA: m.TemplateSHA256, SourceAsOf: jsonv.ToString(m.Envelope.Value("source_as_of")),
				RefreshedAt: jsonv.ToString(m.Envelope.Value("refreshed_at"))},
			TemplateBinding: &versions.TemplateBinding{TemplateName: targetRow.Name, TemplateVersionID: targetRow.ID,
				Revision: intPtr(targetRow.Revision), ConfigSHA: m.ConfigSHA256},
		}, a)
		if err != nil {
			return nil, err
		}
		result, err := versions.DeployLocked(ctx, q, page, prepared, a, versions.DeployLockedOptions{})
		if err != nil {
			return nil, err
		}
		var boundID jsx.ID
		var boundRevision int
		var boundName, storedHTML string
		if err := q.QueryRow(ctx,
			`SELECT tv.id, tv.revision, t.name, pv.html FROM page_versions pv
			   JOIN page_template_versions tv ON tv.id = pv.template_version_id
			   JOIN page_templates t ON t.id = tv.template_id WHERE pv.id = $1`, result.Version.ID).
			Scan(&boundID, &boundRevision, &boundName, &storedHTML); err != nil {
			return nil, err
		}
		// Exact retries reuse immutable bytes, including the refresh timestamp.
		stored, err := ParseTemplateHTML(storedHTML)
		if err != nil {
			return nil, err
		}
		var from *int
		if currentBinding != 0 {
			var rev int
			if err := q.QueryRow(ctx, `SELECT revision FROM page_template_versions WHERE id = $1`, currentBinding).Scan(&rev); err != nil {
				return nil, err
			}
			from = &rev
		}
		return &RerenderResult{
			DeployResult:      *result,
			Template:          boundName,
			FromRevision:      from,
			Reattached:        currentBinding == 0,
			TemplateRevision:  boundRevision,
			TemplateVersionID: boundID,
			Config:            stored.Config,
			ConfigSHA256:      stored.ConfigSHA256,
			Envelope:          stored.Envelope,
			DataSHA256:        stored.DataSHA256,
			TemplateSHA256:    stored.TemplateSHA256,
			HTML:              storedHTML,
		}, nil
	})
}

func isString(v any) bool { _, ok := v.(string); return ok }
