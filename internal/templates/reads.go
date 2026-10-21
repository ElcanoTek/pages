// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package templates

import (
	"context"
	"fmt"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Current is the row a create/rerender builds from: a template and one of
// its revisions, with the revision's stored payloads.
type Current struct {
	TemplateID        jsx.ID
	Name              string
	Title             string
	Description       string
	CurrentVersionID  jsx.ID
	CreatedAt         jsx.Time
	UpdatedAt         jsx.Time
	ID                jsx.ID // the revision's template_version_id
	Revision          int
	HTML              string
	ContentSHA256     string
	ConfigSchema      *jsonv.Object
	DataSchema        *jsonv.Object
	ConfigSchemaSHA   string
	DataSchemaSHA     string
	ReferenceConfig   *jsonv.Object
	SampleData        any // nil when the revision has no example dataset
	HasSampleData     bool
	Author            string
	Source            string
	Note              *string
	RevisionCreatedAt jsx.Time
}

func (c *Current) template() Template {
	return Template{ID: c.TemplateID, Name: c.Name, Title: c.Title, Description: c.Description,
		CurrentRevision: intPtr(c.Revision), CurrentVersionID: c.CurrentVersionID, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

func (c *Current) revision() Revision {
	return Revision{VersionID: c.ID, Revision: c.Revision, ContentSHA256: c.ContentSHA256, ConfigSchemaSHA256: c.ConfigSchemaSHA,
		DataSchemaSHA256: c.DataSchemaSHA, Author: c.Author, Source: c.Source, Note: c.Note, HasSampleData: c.HasSampleData,
		CreatedAt: c.RevisionCreatedAt}
}

// LoadCurrent reads a template revision: revision nil means the current
// pointer, otherwise that exact revision. 404 template_not_found /
// template_revision_not_found.
func LoadCurrent(ctx context.Context, q db.Querier, name any, revision *int) (*Current, error) {
	n, err := NormalizeTemplateName(name)
	if err != nil {
		return nil, err
	}
	var rev any
	if revision != nil {
		rev = *revision
	}
	c := &Current{}
	var configSchema, dataSchema, reference string
	var sample *string
	err = q.QueryRow(ctx,
		`SELECT t.id AS template_id, t.name, t.title, t.description,
		        t.current_version_id, t.created_at, t.updated_at,
		        v.id, v.revision, v.html, v.content_sha256, v.config_schema, v.data_schema,
		        v.config_schema_sha256, v.data_schema_sha256, v.reference_config,
		        v.sample_data, v.author, v.source, v.note, v.created_at AS revision_created_at
		   FROM page_templates t
		   JOIN page_template_versions v ON v.template_id = t.id
		  WHERE t.name = $1 AND t.deleted_at IS NULL
		    AND (
		      ($2::int IS NULL AND v.id = t.current_version_id)
		      OR ($2::int IS NOT NULL AND v.revision = $2::int)
		    )`, n, rev).Scan(
		&c.TemplateID, &c.Name, &c.Title, &c.Description, &c.CurrentVersionID, &c.CreatedAt, &c.UpdatedAt,
		&c.ID, &c.Revision, &c.HTML, &c.ContentSHA256, &configSchema, &dataSchema,
		&c.ConfigSchemaSHA, &c.DataSchemaSHA, &reference, &sample, &c.Author, &c.Source, &c.Note, &c.RevisionCreatedAt)
	if db.IsNoRows(err) {
		if revision != nil && *revision != 0 {
			return nil, apierror.NotFound(fmt.Sprintf("template %s has no revision %d", n, *revision), "template_revision_not_found", nil)
		}
		return nil, apierror.NotFound("template not found: "+n, "template_not_found", nil)
	}
	if err != nil {
		return nil, err
	}
	if c.ConfigSchema, err = parseObject(configSchema); err != nil {
		return nil, err
	}
	if c.DataSchema, err = parseObject(dataSchema); err != nil {
		return nil, err
	}
	if c.ReferenceConfig, err = parseObject(reference); err != nil {
		return nil, err
	}
	if sample != nil {
		c.HasSampleData = true
		if c.SampleData, err = jsonv.ParseString(*sample); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Get is get_template: a revision's identity and contract; HTML only when
// includeHTML.
func Get(ctx context.Context, q db.Querier, name any, revision *int, includeHTML bool) (*GetResult, error) {
	c, err := LoadCurrent(ctx, q, name, revision)
	if err != nil {
		return nil, err
	}
	r := &GetResult{Template: c.template(), Revision: c.revision(), ConfigSchema: c.ConfigSchema,
		DataSchema: c.DataSchema, ReferenceConfig: c.ReferenceConfig}
	if includeHTML {
		h := c.HTML
		r.HTML = &h
	}
	return r, nil
}

// GetRevisionHTML returns the bytes for a preview, with the revision's
// identity so the caller can bind a signed token to exactly these bytes.
func GetRevisionHTML(ctx context.Context, q db.Querier, name any, revision *int) (*RevisionHTML, error) {
	c, err := LoadCurrent(ctx, q, name, revision)
	if err != nil {
		return nil, err
	}
	return &RevisionHTML{Template: c.Name, TemplateVersionID: c.ID, Revision: c.Revision,
		ContentSHA256: c.ContentSHA256, HasSampleData: c.HasSampleData, HTML: c.HTML}, nil
}

// PreviewBytes is what a preview actually renders, by template_version_id
// (which is what a signed template token names).
//
// A preview is the template MATERIALIZED, not its stored bytes: the example
// block is deleted and, when the revision carries an example dataset, it is
// poured into #pages-data. So the previewed document is exactly the shape a
// page built from this design would have — with rows in it, rather than the
// empty state a template necessarily ships. now zero means time.Now().
func PreviewBytes(ctx context.Context, q db.Querier, templateVersionID jsx.ID, now time.Time) (*Preview, error) {
	var html, name string
	var sample *string
	var revision int
	err := q.QueryRow(ctx,
		`SELECT v.html, v.sample_data, v.revision, t.name
		   FROM page_template_versions v
		   JOIN page_templates t ON t.id = v.template_id
		  WHERE v.id = $1 AND t.deleted_at IS NULL`, templateVersionID).Scan(&html, &sample, &revision, &name)
	if db.IsNoRows(err) {
		return nil, apierror.NotFound(fmt.Sprintf("template revision not found: %d", int64(templateVersionID)), "template_revision_not_found", nil)
	}
	if err != nil {
		return nil, err
	}
	managed, err := ParseTemplateHTML(html)
	if err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	// No data when there is no example dataset: materialization then leaves
	// the empty envelope byte-identical and only deletes the example block.
	var update pagedata.BlockUpdate
	if sample != nil {
		data, err := jsonv.ParseString(*sample)
		if err != nil {
			return nil, err
		}
		update = pagedata.BlockUpdate{Data: data, SetData: true}
	}
	m, err := pagedata.MaterializeBlocks(managed, update, pagedata.MaterializeOptions{SourceAsOf: jsx.FormatTime(now), Now: now})
	if err != nil {
		return nil, err
	}
	return &Preview{Template: name, Revision: revision, HTML: m.HTML, UsedSampleData: sample != nil}, nil
}

// List is list_templates.
func List(ctx context.Context, q db.Querier) (*ListResult, error) {
	rows, err := q.Query(ctx,
		`SELECT t.id, t.name, t.title, t.description, t.current_version_id,
		        t.created_at, t.updated_at,
		        v.revision AS current_revision,
		        v.config_schema_sha256, v.data_schema_sha256,
		        -- Two different questions, and conflating them is what hid template
		        -- drift: page_count is every live page whose history touches this
		        -- design, serving_count only those whose PUBLISHED version still
		        -- comes from it. page_count - serving_count is the drift.
		        (SELECT count(DISTINCT pv.page_id)
		           FROM page_versions pv
		           JOIN page_template_versions tv ON tv.id = pv.template_version_id
		           JOIN pages p ON p.id = pv.page_id AND p.deleted_at IS NULL
		          WHERE tv.template_id = t.id) AS page_count,
		        (SELECT count(DISTINCT pv.page_id)
		           FROM page_versions pv
		           JOIN page_template_versions tv ON tv.id = pv.template_version_id
		           JOIN pages p ON p.id = pv.page_id AND p.deleted_at IS NULL
		          WHERE tv.template_id = t.id
		            AND pv.id = p.published_version_id) AS serving_count
		   FROM page_templates t
		   LEFT JOIN page_template_versions v ON v.id = t.current_version_id
		  WHERE t.deleted_at IS NULL
		  ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &ListResult{Templates: []ListedTemplate{}}
	for rows.Next() {
		var t ListedTemplate
		if err := rows.Scan(&t.ID, &t.Name, &t.Title, &t.Description, &t.CurrentVersionID, &t.CreatedAt, &t.UpdatedAt,
			&t.CurrentRevision, &t.ConfigSchemaSHA256, &t.DataSchemaSHA256, &t.PageCount, &t.ServingCount); err != nil {
			return nil, err
		}
		t.DriftedCount = t.PageCount - t.ServingCount
		out.Templates = append(out.Templates, t)
	}
	return out, rows.Err()
}

// Revisions is list_template_revisions, newest first. 404 template_not_found.
func Revisions(ctx context.Context, q db.Querier, name any) (*RevisionsResult, error) {
	n, err := NormalizeTemplateName(name)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx,
		`SELECT v.id, v.revision, v.content_sha256, v.config_schema_sha256, v.data_schema_sha256,
		        (v.sample_data IS NOT NULL) AS has_sample_data, v.author, v.source, v.note, v.created_at,
		        (v.id = t.current_version_id) AS is_current
		   FROM page_templates t
		   JOIN page_template_versions v ON v.template_id = t.id
		  WHERE t.name = $1 AND t.deleted_at IS NULL
		  ORDER BY v.revision DESC`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &RevisionsResult{Template: n, Revisions: []ListedRevision{}}
	for rows.Next() {
		var r ListedRevision
		var current *bool
		if err := rows.Scan(&r.VersionID, &r.Revision.Revision, &r.ContentSHA256, &r.ConfigSchemaSHA256, &r.DataSchemaSHA256,
			&r.HasSampleData, &r.Author, &r.Source, &r.Note, &r.CreatedAt, &current); err != nil {
			return nil, err
		}
		r.IsCurrent = current != nil && *current
		out.Revisions = append(out.Revisions, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out.Revisions) == 0 {
		return nil, apierror.NotFound("template not found: "+n, "template_not_found", nil)
	}
	return out, nil
}

// PageTemplateBinding is which template revision produced the page currently
// being served, or nil when it was authored directly. Cheap: reads the
// provenance columns instead of parsing the page's HTML.
func PageTemplateBinding(ctx context.Context, q db.Querier, slug any) (*Binding, error) {
	s, err := versions.NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	b := &Binding{}
	var isCurrent *bool
	err = q.QueryRow(ctx,
		`SELECT t.name AS template, tv.revision, tv.config_schema_sha256, tv.data_schema_sha256,
		        tv.id AS template_version_id, v.config_sha256,
		        (tv.id = t.current_version_id) AS is_current,
		        current.revision AS current_revision
		   FROM pages p
		   JOIN page_versions v ON v.id = p.published_version_id
		   JOIN page_template_versions tv ON tv.id = v.template_version_id
		   JOIN page_templates t ON t.id = tv.template_id
		   LEFT JOIN page_template_versions current ON current.id = t.current_version_id
		  WHERE p.slug = $1 AND p.deleted_at IS NULL`, s).Scan(
		&b.Template, &b.Revision, &b.ConfigSchemaSHA256, &b.DataSchemaSHA256, &b.TemplateVersionID, &b.ConfigSHA256,
		&isCurrent, &b.CurrentRevision)
	if db.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	b.IsCurrentRevision = isCurrent != nil && *isCurrent
	return b, nil
}

// ListTemplatePages answers "what does a design fix have to move?", which
// needs BOTH populations. Requiring the page's published version to come from
// the template made a page built from it and later overwritten by a raw
// deploy_page or patch_page silently vanish: it appeared in no tool at all,
// while list_templates still counted it — page_count 5 against 2 listed
// pages, with the 3 forked pages unreachable.
//
// So it enumerates every live page whose history touches the template, and
// says for each whether the version a VIEWER IS SERVED still comes from it. A
// drifted page keeps last_revision, which is what rerender_page_from_template
// needs to pull it back onto the shared design.
//
// Propagation is always user-initiated: a new revision never moves a deployed
// page by itself. This read and RerenderPage exist so a human can SEE who is
// behind and then move pages one at a time, previewing each. There is
// deliberately no fan-out primitive — no single call can rewrite every live
// client dashboard, because a bad design revision would then be a fleet-wide
// incident instead of one page to fix.
func ListTemplatePages(ctx context.Context, q db.Querier, name any) (*TemplatePages, error) {
	n, err := NormalizeTemplateName(name)
	if err != nil {
		return nil, err
	}
	tmpl, err := LoadCurrent(ctx, q, n, nil)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx,
		`WITH built AS (
		    SELECT DISTINCT pv.page_id
		      FROM page_versions pv
		      JOIN page_template_versions tv ON tv.id = pv.template_version_id
		     WHERE tv.template_id = $1
		 )
		 SELECT p.slug, p.title, p.disabled, p.published_version_id,
		        live.id AS live_version_id,
		        live.config_sha256,
		        livetv.revision AS live_revision,
		        livetv.template_id AS live_template_id,
		        (SELECT max(tv2.revision)
		           FROM page_versions pv2
		           JOIN page_template_versions tv2 ON tv2.id = pv2.template_version_id
		          WHERE pv2.page_id = p.id AND tv2.template_id = $1) AS last_revision
		   FROM built b
		   JOIN pages p ON p.id = b.page_id AND p.deleted_at IS NULL
		   LEFT JOIN page_versions live ON live.id = p.published_version_id
		   LEFT JOIN page_template_versions livetv ON livetv.id = live.template_version_id
		  ORDER BY p.slug`, tmpl.TemplateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := &TemplatePages{Template: n, CurrentRevision: tmpl.Revision, Pages: []TemplatePage{}}
	for rows.Next() {
		var p TemplatePage
		var disabled bool
		var published, liveTemplate jsx.ID
		var liveRevision *int
		var configSHA *string
		if err := rows.Scan(&p.Slug, &p.Title, &disabled, &published, &p.LiveVersionID, &configSHA,
			&liveRevision, &liveTemplate, &p.LastRevision); err != nil {
			return nil, err
		}
		// Serving this design means the PUBLISHED version is bound to a
		// revision of THIS template. A page published from another template's
		// revision, or from a hand-deployed version with no binding at all, or
		// with nothing published, is no longer being served the shared design.
		serving := liveTemplate != 0 && liveTemplate == tmpl.TemplateID
		if serving {
			p.Revision = liveRevision
			p.Behind = liveRevision != nil && *liveRevision < tmpl.Revision
			p.ConfigSHA256 = configSHA
		}
		p.Drifted = !serving
		p.PageIsLive = published != 0 && !disabled
		if !serving {
			out.DriftedCount++
		}
		out.Pages = append(out.Pages, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.ServingCount = len(out.Pages) - out.DriftedCount
	return out, nil
}
