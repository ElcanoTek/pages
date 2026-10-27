// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
)

// MaxListLimit lets a public 100-row page fetch one look-ahead row.
const MaxListLimit = 101

// After is a keyset cursor position: rows strictly older than (CreatedAt, ID)
// in the (created_at ms, id) DESC order. CreatedAt is a time.Time or an ISO
// string; ID a positive id (string, number, jsx.ID).
type After struct {
	CreatedAt any
	ID        any
}

type cursor struct {
	createdAt string
	id        int64
}

func normalizeAfter(a *After) (*cursor, error) {
	if a == nil {
		return nil, nil
	}
	var t time.Time
	switch v := a.CreatedAt.(type) {
	case time.Time:
		t = v
	case jsx.Time:
		t = v.Time
	case string:
		t = ParseTime(v)
	}
	if t.IsZero() {
		return nil, apierror.BadRequest("after.createdAt must be a valid timestamp", "bad_cursor", nil)
	}
	var id int64
	ok := false
	switch v := a.ID.(type) {
	case string:
		if decimalIDRE.MatchString(v) {
			n, err := strconv.ParseInt(v, 10, 64)
			id, ok = n, err == nil
		}
	case float64:
		if v == math.Trunc(v) && math.Abs(v) <= maxSafeInteger {
			id, ok = int64(v), true
		}
	case int:
		id, ok = int64(v), true
	case int64:
		id, ok = v, true
	case jsx.ID:
		id, ok = int64(v), true
	}
	if !ok || id <= 0 {
		return nil, apierror.BadRequest("after.id must be a positive database id", "bad_cursor", nil)
	}
	return &cursor{createdAt: isoString(t), id: id}, nil
}

func normalizeListLimit(limit *int) (int, error) {
	if limit == nil {
		return 0, nil
	}
	if *limit < 1 || *limit > MaxListLimit {
		return 0, apierror.BadRequest(fmt.Sprintf("limit must be an integer from 1 to %d", MaxListLimit), "bad_limit", nil)
	}
	return *limit, nil
}

func normalizeSearchQuery(q *string) (string, error) {
	if q == nil {
		return "", nil
	}
	s := jsTrim(*q)
	if jsstr.UTF16Len(s) > 200 {
		return "", apierror.BadRequest("query must be 200 characters or fewer", "query_too_long", nil)
	}
	return s, nil
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ListPagesOptions are listPages' optional, bounded MCP filters. The zero
// value lists every live page (the REST/admin contract: the complete array).
type ListPagesOptions struct {
	// WorkspaceFilterSet filters by WorkspaceID: Null or "ungrouped" ⇒ pages
	// with no workspace, else a workspace id (nil ⇒ 400 bad_workspace_id, as
	// JavaScript's hasOwn(workspaceId) with an undefined value).
	WorkspaceFilterSet bool
	WorkspaceID        any
	// Query matches slug, title, client_id or workspace name (ILIKE, trimmed,
	// ≤ 200 characters; blank ⇒ no filter).
	Query *string
	// ClientIDSet filters by ClientID (nil ⇒ client_id IS NULL).
	ClientIDSet     bool
	ClientID        *string
	IsLive          *bool
	RequireApproval *bool
	Disabled        *bool
	After           *After
	Limit           *int // 1..MaxListLimit; nil ⇒ no limit
	// IncludeVersionNumbers adds the admin index's page-local ordinals.
	IncludeVersionNumbers bool
}

func buildListPagesQuery(o ListPagesOptions) (string, []any, error) {
	var params []any
	where := []string{"p.deleted_at IS NULL"}
	bind := func(v any) string {
		params = append(params, v)
		return "$" + strconv.Itoa(len(params))
	}
	if o.WorkspaceFilterSet {
		if o.WorkspaceID == Null || o.WorkspaceID == "ungrouped" {
			where = append(where, "p.workspace_id IS NULL")
		} else {
			id, err := NormalizeWorkspaceID(o.WorkspaceID)
			if err != nil {
				return "", nil, err
			}
			where = append(where, "p.workspace_id = "+bind(int64(id)))
		}
	}
	query, err := normalizeSearchQuery(o.Query)
	if err != nil {
		return "", nil, err
	}
	if query != "" {
		pattern := bind("%" + escapeLike(query) + "%")
		where = append(where,
			`(p.slug ILIKE `+pattern+` ESCAPE '\' OR p.title ILIKE `+pattern+` ESCAPE '\' `+
				`OR COALESCE(p.client_id, '') ILIKE `+pattern+` ESCAPE '\' `+
				`OR COALESCE(w.name, '') ILIKE `+pattern+` ESCAPE '\')`)
	}
	if o.ClientIDSet {
		if o.ClientID == nil {
			where = append(where, "p.client_id IS NULL")
		} else {
			where = append(where, "p.client_id = "+bind(*o.ClientID))
		}
	}
	if o.IsLive != nil {
		where = append(where, "(p.published_version_id IS NOT NULL AND NOT p.disabled) = "+bind(*o.IsLive))
	}
	if o.RequireApproval != nil {
		where = append(where, "p.require_approval = "+bind(*o.RequireApproval))
	}
	if o.Disabled != nil {
		where = append(where, "p.disabled = "+bind(*o.Disabled))
	}
	after, err := normalizeAfter(o.After)
	if err != nil {
		return "", nil, err
	}
	if after != nil {
		createdAt := bind(after.createdAt)
		id := bind(after.id)
		where = append(where,
			`(date_trunc('milliseconds', p.created_at), p.id) < `+
				`(date_trunc('milliseconds', `+createdAt+`::timestamptz), `+id+`::bigint)`)
	}
	limit, err := normalizeListLimit(o.Limit)
	if err != nil {
		return "", nil, err
	}
	limitSQL := ""
	if limit > 0 {
		limitSQL = " LIMIT " + bind(limit)
	}
	// The admin index presents page-local version ordinals, never the global
	// page_versions primary key. Keep these extra aggregates opt-in so the
	// REST/MCP page-summary contracts remain unchanged.
	versionNumberFields := ""
	if o.IncludeVersionNumbers {
		versionNumberFields = `,
            (SELECT COUNT(*)::integer
               FROM page_versions versions
              WHERE versions.page_id = p.id) AS version_count,
            CASE WHEN p.published_version_id IS NULL THEN NULL ELSE
              (SELECT COUNT(*)::integer
                 FROM page_versions versions
                WHERE versions.page_id = p.id
                  AND versions.id <= p.published_version_id)
            END AS published_version_number`
	}
	sql := `SELECT p.id, p.slug, p.title, p.client_id, p.workspace_id, w.name AS workspace_name,
            p.theme_id, COALESCE(t.name, 'flag') AS theme_name, p.require_approval, p.disabled,
            p.published_version_id, p.created_at, p.updated_at,
            (p.password_hash IS NOT NULL) AS has_password,
            (p.published_version_id IS NOT NULL AND NOT p.disabled) AS is_live,
            pv.source_as_of, pv.refreshed_at,
            CASE WHEN pv.data_sha256 IS NULL AND position('pages-data' IN pv.html) > 0
                 THEN pv.html END AS legacy_managed_html,
            p.last_check_at, p.last_check_outcome, p.last_check_detail,
            p.last_check_source_as_of,
            p.refresh_schedule, p.refresh_owner,
            ` + newestRefreshSQL + ` AS newest_refreshed_at,
            pv.coverage_gaps` + versionNumberFields + `
       FROM pages p
       LEFT JOIN workspaces w ON w.id = p.workspace_id
       LEFT JOIN themes t ON t.id = p.theme_id
       LEFT JOIN page_versions pv ON pv.id = p.published_version_id
      WHERE ` + strings.Join(where, " AND ") + `
      ORDER BY date_trunc('milliseconds', p.created_at) DESC, p.id DESC` + limitSQL
	return sql, params, nil
}

// ListPages lists live (non-deleted) pages newest first, each with its
// published version id, has_password / is_live (published AND not disabled →
// actually serving on the content host), freshness and refresh health. q may
// be a snapshot transaction (the admin index reads several things
// consistently). The raw coverage stamps stay off the row: every caller wants
// the derived answer. The published versions' coverage gaps feed a scheduled
// page's health; any not stored yet are computed here and saved through q,
// best effort (coveragegaps.go).
func ListPages(ctx context.Context, q db.Querier, o ListPagesOptions) ([]PageSummary, error) {
	sql, params, err := buildListPagesQuery(o)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, sql, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PageSummary{}
	type extra struct {
		source, refreshed jsx.Time
		legacy            *string
		lastCheck         LastCheck
		schedule, owner   *string
		newest            jsx.Time
		gaps              []byte
	}
	var extras []extra
	for rows.Next() {
		var p PageSummary
		var e extra
		dest := []any{&p.ID, &p.Slug, &p.Title, &p.ClientID, &p.WorkspaceID, &p.WorkspaceName,
			&p.ThemeID, &p.ThemeName, &p.RequireApproval, &p.Disabled,
			&p.PublishedVersionID, &p.CreatedAt, &p.UpdatedAt, &p.HasPassword, &p.IsLive,
			&e.source, &e.refreshed, &e.legacy,
			&e.lastCheck.LastCheckAt, &e.lastCheck.LastCheckOutcome, &e.lastCheck.LastCheckDetail, &e.lastCheck.LastCheckSourceAsOf,
			&e.schedule, &e.owner, &e.newest, &e.gaps}
		if o.IncludeVersionNumbers {
			p.VersionNumbers = &VersionNumbers{}
			dest = append(dest, &p.VersionCount, &p.PublishedVersionNumber)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, p)
		extras = append(extras, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// Only a scheduled page is judged, so only its gaps are needed.
	stored := make([]storedGaps, 0, len(out))
	for i := range out {
		if extras[i].schedule != nil {
			stored = append(stored, storedGaps{versionID: out[i].PublishedVersionID, raw: extras[i].gaps})
		}
	}
	gaps, err := resolveCoverageGaps(ctx, q, stored)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range out {
		e := extras[i]
		c, err := indexedOrLegacyCoverage(e.source, e.refreshed, e.legacy)
		if err != nil {
			return nil, err
		}
		out[i].Freshness = FreshnessOf(lastCheckInput(e.lastCheck, c), now)
		out[i].RefreshHealth = HealthOf(healthInput(e.schedule, e.owner, out[i].IsLive, out[i].CreatedAt, c, e.newest, e.lastCheck,
			gaps[out[i].PublishedVersionID]), now)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func lastCheckInput(lc LastCheck, c coverage) FreshnessInput {
	return FreshnessInput{
		SourceAsOf: c.sourceAsOf, RefreshedAt: c.refreshedAt,
		LastCheckAt: lc.LastCheckAt.Time, LastCheckOutcome: deref(lc.LastCheckOutcome),
		LastCheckDetail: deref(lc.LastCheckDetail), LastCheckSourceAsOf: lc.LastCheckSourceAsOf.Time,
	}
}

func pageNotFound(slug string) error {
	return apierror.NotFound("page not found: "+slug, "page_not_found", nil)
}

// GetPage reads page metadata plus the published version (nil when nothing
// is published). 404 page_not_found when absent or soft-deleted.
func GetPage(ctx context.Context, q db.Querier, slug string) (*PageView, error) {
	s, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	v := &PageView{}
	p := &v.Page
	err = q.QueryRow(ctx,
		`SELECT p.id, p.slug, p.title, p.client_id, p.workspace_id, w.name AS workspace_name,
		        p.theme_id, COALESCE(t.name, 'flag') AS theme_name, p.require_approval, p.disabled, p.published_version_id,
		        p.created_at, p.updated_at,
		        (p.password_hash IS NOT NULL) AS has_password,
		        p.last_check_at, p.last_check_outcome, p.last_check_detail, p.last_check_source_as_of
		   FROM pages p
		   LEFT JOIN workspaces w ON w.id = p.workspace_id
		   LEFT JOIN themes t ON t.id = p.theme_id
		  WHERE p.slug = $1 AND p.deleted_at IS NULL`, s).Scan(
		&p.ID, &p.Slug, &p.Title, &p.ClientID, &p.WorkspaceID, &p.WorkspaceName,
		&p.ThemeID, &p.ThemeName, &p.RequireApproval, &p.Disabled, &p.PublishedVersionID,
		&p.CreatedAt, &p.UpdatedAt, &p.HasPassword,
		&v.LastCheck.LastCheckAt, &v.LastCheck.LastCheckOutcome, &v.LastCheck.LastCheckDetail, &v.LastCheck.LastCheckSourceAsOf)
	if db.IsNoRows(err) {
		return nil, pageNotFound(s)
	}
	if err != nil {
		return nil, err
	}
	if p.PublishedVersionID != 0 {
		published, err := GetVersion(ctx, q, s, p.PublishedVersionID)
		if err != nil {
			// Only a genuinely missing version row may read as "nothing
			// published" (the pages_pubver_fk FK makes that impossible in
			// practice; keep it as defense). Anything else — a transient reset,
			// statement/lock timeout, pool exhaustion — must surface as an
			// error, never as a FALSE unpublished state that agents act on.
			if e, ok := apierror.As(err); !ok || e.Code != "version_not_found" {
				return nil, err
			}
		} else {
			v.Published = published
		}
	}
	return v, nil
}

// PageDataView is getPageData's result: the data-management contract read
// from the immutable version at the published pointer.
type PageDataView struct {
	Page           Page              `json:"page"`
	Version        Version           `json:"version"`
	Schema         *jsonv.Object     `json:"schema"`
	Envelope       *jsonv.Object     `json:"envelope"`
	DataSHA256     string            `json:"data_sha256"`
	SchemaSHA256   string            `json:"schema_sha256"`
	TemplateSHA256 string            `json:"template_sha256"`
	DataProfile    *pagedata.Profile `json:"data_profile"`
	Freshness      *Freshness        `json:"freshness"`
	// Managed is the parsed published document (not serialized).
	Managed *pagedata.Managed `json:"-"`
}

// GetPageData reads the managed-data contract of the published version.
// 409 page_not_data_managed when nothing is published; parse errors from
// pagedata otherwise. The page's serving flags distinguish pointer truth from
// actual live serving when an admin has disabled the page.
func GetPageData(ctx context.Context, q db.Querier, slug string) (*PageDataView, error) {
	pv, err := GetPage(ctx, q, slug)
	if err != nil {
		return nil, err
	}
	if pv.Published == nil {
		return nil, apierror.Conflict("page has no published template to manage", "page_not_data_managed", nil)
	}
	m, err := pagedata.ParseManagedHTML(pv.Published.HTML)
	if err != nil {
		return nil, err
	}
	in := lastCheckInput(pv.LastCheck, coverage{})
	in.SourceAsOf = ParseTime(m.SourceAsOf())
	in.RefreshedAt = ParseTime(m.RefreshedAt())
	return &PageDataView{
		Page:           pv.Page,
		Version:        pv.Published.Summary(),
		Schema:         m.Schema,
		Envelope:       m.Envelope,
		DataSHA256:     m.DataSHA256,
		SchemaSHA256:   m.SchemaSHA256,
		TemplateSHA256: m.TemplateSHA256,
		DataProfile:    pagedata.ProfileData(m.Data()),
		Freshness:      FreshnessOf(in, time.Time{}),
		Managed:        m,
	}, nil
}

// parseConfigManaged reads a page's config contract. A page with no managed
// blocks at all and a page with only the data pair are the same answer from a
// config caller's point of view: it was not built from a template.
// Reporting page_not_data_managed here would send them to fix the wrong thing.
func parseConfigManaged(html string) (*pagedata.Managed, error) {
	notTemplate := apierror.Conflict(
		fmt.Sprintf("page has no #%s block; it was not built from a template", pagedata.ConfigID),
		"page_not_template_managed", nil)
	m, err := pagedata.ParseManagedHTML(html)
	if err != nil {
		if e, ok := apierror.As(err); ok && e.Code == "page_not_data_managed" {
			return nil, notTemplate
		}
		return nil, err
	}
	if m.ConfigBlock == nil {
		return nil, notTemplate
	}
	return m, nil
}

// PageConfigView is getPageConfig's result.
type PageConfigView struct {
	Page               Page          `json:"page"`
	Version            Version       `json:"version"`
	ConfigSchema       *jsonv.Object `json:"config_schema"`
	Config             *jsonv.Object `json:"config"`
	ConfigSHA256       string        `json:"config_sha256"`
	ConfigSchemaSHA256 string        `json:"config_schema_sha256"`
	TemplateSHA256     string        `json:"template_sha256"`
}

// GetPageConfig reads the deploy-time settings half of a template-built
// page. Kept separate from GetPageData so the data path is untouched and an
// agent that only needs to change the campaign's identity never reads its
// rows. 409 update_page_not_published / page_not_template_managed.
func GetPageConfig(ctx context.Context, q db.Querier, slug string) (*PageConfigView, error) {
	pv, err := GetPage(ctx, q, slug)
	if err != nil {
		return nil, err
	}
	if pv.Published == nil {
		return nil, apierror.Conflict("page has no published version", "update_page_not_published", nil)
	}
	m, err := parseConfigManaged(pv.Published.HTML)
	if err != nil {
		return nil, err
	}
	return &PageConfigView{
		Page: pv.Page, Version: pv.Published.Summary(),
		ConfigSchema: m.ConfigSchema, Config: m.Config,
		ConfigSHA256: m.ConfigSHA256, ConfigSchemaSHA256: m.ConfigSchemaSHA256, TemplateSHA256: m.TemplateSHA256,
	}, nil
}

type loadedPage struct {
	id              jsx.ID
	slug            string
	requireApproval bool
	disabled        bool
	publishedID     jsx.ID
}

func loadPage(ctx context.Context, q db.Querier, slug string) (*loadedPage, error) {
	s, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	p := &loadedPage{}
	err = q.QueryRow(ctx,
		`SELECT id, slug, require_approval, disabled, published_version_id
		   FROM pages WHERE slug = $1 AND deleted_at IS NULL`, s).
		Scan(&p.id, &p.slug, &p.requireApproval, &p.disabled, &p.publishedID)
	if db.IsNoRows(err) {
		return nil, pageNotFound(s)
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// VersionStatuses are the review lifecycle states.
var VersionStatuses = []string{"draft", "pending", "approved", "rejected"}

// ListVersionsOptions bound listVersions for MCP; the zero value is the full
// history.
type ListVersionsOptions struct {
	Status *string // one of VersionStatuses (400 bad_version_status otherwise)
	After  *After
	Limit  *int
}

// ListVersions is a page's full history, newest first.
func ListVersions(ctx context.Context, q db.Querier, slug string, o ListVersionsOptions) ([]ListedVersion, error) {
	page, err := loadPage(ctx, q, slug)
	if err != nil {
		return nil, err
	}
	params := []any{page.id}
	where := []string{"v.page_id = $1"}
	if o.Status != nil {
		valid := false
		for _, s := range VersionStatuses {
			valid = valid || s == *o.Status
		}
		if !valid {
			return nil, apierror.BadRequest("bad version status: "+*o.Status, "bad_version_status", nil)
		}
		params = append(params, *o.Status)
		where = append(where, "v.status = $"+strconv.Itoa(len(params))+"::version_status")
	}
	after, err := normalizeAfter(o.After)
	if err != nil {
		return nil, err
	}
	if after != nil {
		params = append(params, after.createdAt, after.id)
		n := len(params)
		where = append(where, fmt.Sprintf(
			"(date_trunc('milliseconds', v.created_at), v.id) < (date_trunc('milliseconds', $%d::timestamptz), $%d::bigint)", n-1, n))
	}
	limit, err := normalizeListLimit(o.Limit)
	if err != nil {
		return nil, err
	}
	limitSQL := ""
	if limit > 0 {
		params = append(params, limit)
		limitSQL = " LIMIT $" + strconv.Itoa(len(params))
	}
	rows, err := q.Query(ctx,
		`SELECT v.id, v.page_id, v.content_sha256, v.status, v.render_mode, v.author, v.source,
		        v.note, v.reviewed_by, v.reviewed_at, v.created_at
		   FROM page_versions v
		  WHERE `+strings.Join(where, " AND ")+`
		  ORDER BY date_trunc('milliseconds', v.created_at) DESC, v.id DESC`+limitSQL, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ListedVersion{}
	for rows.Next() {
		var lv ListedVersion
		if err := rows.Scan(lv.scanArgs()...); err != nil {
			return nil, err
		}
		// Published means pointer equality even during an admin takedown. Live
		// is the stricter serving truth: pointer equality AND not disabled.
		lv.IsPublished = page.publishedID != 0 && lv.ID == page.publishedID
		lv.IsLive = lv.IsPublished && !page.disabled
		out = append(out, lv)
	}
	return out, rows.Err()
}

// GetVersion reads one version's full row (html included). 404
// version_not_found when it is not on this page (never another page's html).
func GetVersion(ctx context.Context, q db.Querier, slug string, versionID jsx.ID) (*VersionDetail, error) {
	page, err := loadPage(ctx, q, slug)
	if err != nil {
		return nil, err
	}
	v := &VersionDetail{}
	err = q.QueryRow(ctx,
		`SELECT id, page_id, html, content_sha256, status, render_mode, author,
		        source, note, reviewed_by, reviewed_at, created_at
		   FROM page_versions
		  WHERE id = $1 AND page_id = $2`, versionID, page.id).Scan(
		&v.ID, &v.PageID, &v.HTML, &v.ContentSHA256, &v.Status, &v.RenderMode, &v.Author,
		&v.Source, &v.Note, &v.ReviewedBy, &v.ReviewedAt, &v.CreatedAt)
	if db.IsNoRows(err) {
		return nil, apierror.NotFound(fmt.Sprintf("version %s not found on page %s", jsString(versionID), slug), "version_not_found", nil)
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// ListThemes lists the names available to the theme picker.
func ListThemes(ctx context.Context, q db.Querier) ([]Theme, error) {
	rows, err := q.Query(ctx, `SELECT id, name, default_mode FROM themes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Theme{}
	for rows.Next() {
		var t Theme
		if err := rows.Scan(&t.ID, &t.Name, &t.DefaultMode); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
