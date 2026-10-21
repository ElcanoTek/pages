// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/db"
)

// Renderable is everything /raw needs to render one version: the source, its
// mode, the owning page's slug and takedown flag, and the resolved theme.
type Renderable struct {
	ID         int64
	HTML       string
	RenderMode string
	PageID     int64
	// Status is set by GetLatestRenderable only.
	Status      string
	Slug        string
	Disabled    bool
	OverrideCSS string // "" when the page has no theme row
	DefaultMode string // "" when the page has no theme row
}

// PublicPage is everything the content host needs to serve a slug directly:
// the page's gate state, its published pointer, and the published version's
// renderable fields. HTML and RenderMode are "" when nothing is published.
type PublicPage struct {
	ID                 int64
	Slug               string
	Disabled           bool
	PasswordHash       string // "" = staff-only (NULL)
	PublishedVersionID int64  // 0 = nothing published (NULL)
	HTML               string
	RenderMode         string
	OverrideCSS        string
}

// Portal is a live partner portal as the gate sees it. HomePageID is only
// read by GetPublicPortal (0 = none).
type Portal struct {
	ID           int64
	Slug         string
	Name         string
	PasswordHash string
	HomePageID   int64
}

// PortalPage is one dashboard a portal may actually open, in curated order.
// The zero time means the timestamp is NULL.
type PortalPage struct {
	ID          int64
	Slug        string
	Title       string
	SortOrder   int
	IsHome      bool
	PublishedAt time.Time
	SourceAsOf  time.Time
}

// Store is the content host's read path. The production implementation is
// NewStore; a test substitutes one to force a lookup to fail, which is how the
// fail-closed and degrade-don't-break rules are pinned.
type Store interface {
	GetRenderable(ctx context.Context, versionID int64) (*Renderable, error)
	GetLatestRenderable(ctx context.Context, pageID int64) (*Renderable, error)
	GetPublicPage(ctx context.Context, slug string) (*PublicPage, error)
	GetPublicPortal(ctx context.Context, slug string) (*Portal, error)
	GetPortalPages(ctx context.Context, portalID int64) ([]PortalPage, error)
	GetPortalsForPage(ctx context.Context, pageID int64) ([]Portal, error)
}

// NewStore is the Store over q (a pool or a transaction).
func NewStore(q db.Querier) Store { return queries{q} }

type queries struct{ q db.Querier }

func (s queries) GetRenderable(ctx context.Context, versionID int64) (*Renderable, error) {
	return GetRenderable(ctx, s.q, versionID)
}
func (s queries) GetLatestRenderable(ctx context.Context, pageID int64) (*Renderable, error) {
	return GetLatestRenderable(ctx, s.q, pageID)
}
func (s queries) GetPublicPage(ctx context.Context, slug string) (*PublicPage, error) {
	return GetPublicPage(ctx, s.q, slug)
}
func (s queries) GetPublicPortal(ctx context.Context, slug string) (*Portal, error) {
	return GetPublicPortal(ctx, s.q, slug)
}
func (s queries) GetPortalPages(ctx context.Context, portalID int64) ([]PortalPage, error) {
	return GetPortalPages(ctx, s.q, portalID)
}
func (s queries) GetPortalsForPage(ctx context.Context, pageID int64) ([]Portal, error) {
	return GetPortalsForPage(ctx, s.q, pageID)
}

func scanRenderable(row pgx.Row, withStatus bool) (*Renderable, error) {
	var (
		r       Renderable
		css, dm *string
		status  *string
	)
	dest := []any{&r.ID, &r.HTML, &r.RenderMode, &r.PageID}
	if withStatus {
		dest = append(dest, &status)
	}
	dest = append(dest, &r.Slug, &r.Disabled, &css, &dm)
	if err := row.Scan(dest...); err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	r.OverrideCSS, r.DefaultMode = deref(css), deref(dm)
	r.Status = deref(status)
	return &r, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// GetRenderable loads one version for /raw; nil when there is no such version
// on a live (not soft-deleted) page.
func GetRenderable(ctx context.Context, q db.Querier, versionID int64) (*Renderable, error) {
	return scanRenderable(q.QueryRow(ctx,
		`SELECT v.id, v.html, v.render_mode::text, v.page_id,
		        p.slug, p.disabled,
		        t.override_css, t.default_mode
		   FROM page_versions v
		   JOIN pages p  ON p.id = v.page_id
		   LEFT JOIN themes t ON t.id = p.theme_id
		  WHERE v.id = $1 AND p.deleted_at IS NULL`, versionID), false)
}

// GetLatestRenderable is the NEWEST version of a page, whatever its status. A
// deck edit session continues from the last save, not from what clients see:
// saves land as drafts, and an editor that reopened on the live version would
// show the author their own work undone.
func GetLatestRenderable(ctx context.Context, q db.Querier, pageID int64) (*Renderable, error) {
	return scanRenderable(q.QueryRow(ctx,
		`SELECT v.id, v.html, v.render_mode::text, v.page_id, v.status::text,
		        p.slug, p.disabled,
		        t.override_css, t.default_mode
		   FROM page_versions v
		   JOIN pages p  ON p.id = v.page_id
		   LEFT JOIN themes t ON t.id = p.theme_id
		  WHERE v.page_id = $1 AND p.deleted_at IS NULL
		  ORDER BY v.created_at DESC, v.id DESC
		  LIMIT 1`, pageID), true)
}

// GetPublicPage returns the row even when nothing is published (HTML "") so
// the caller can tell "no such page" (nil) from "published nothing yet".
func GetPublicPage(ctx context.Context, q db.Querier, slug string) (*PublicPage, error) {
	var (
		p                    PublicPage
		hash, html, mode, cs *string
		pub                  *int64
	)
	err := q.QueryRow(ctx,
		`SELECT p.id, p.slug, p.disabled, p.password_hash, p.published_version_id,
		        v.html, v.render_mode::text, t.override_css
		   FROM pages p
		   LEFT JOIN page_versions v ON v.id = p.published_version_id
		   LEFT JOIN themes t ON t.id = p.theme_id
		  WHERE p.slug = $1 AND p.deleted_at IS NULL`, slug).
		Scan(&p.ID, &p.Slug, &p.Disabled, &hash, &pub, &html, &mode, &cs)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	p.PasswordHash, p.HTML, p.RenderMode, p.OverrideCSS = deref(hash), deref(html), deref(mode), deref(cs)
	if pub != nil {
		p.PublishedVersionID = *pub
	}
	return &p, nil
}

// GetPublicPortal is the portal's identity and the credential that opens it.
// Retired portals are invisible here, which is what makes retiring a portal
// the kill switch for its credential.
func GetPublicPortal(ctx context.Context, q db.Querier, slug string) (*Portal, error) {
	var (
		p    Portal
		home *int64
	)
	err := q.QueryRow(ctx,
		`SELECT id, slug, name, password_hash, home_page_id
		   FROM page_portals
		  WHERE slug = $1 AND deleted_at IS NULL`, slug).
		Scan(&p.ID, &p.Slug, &p.Name, &p.PasswordHash, &home)
	if err != nil {
		if db.IsNoRows(err) {
			return nil, nil
		}
		return nil, err
	}
	if home != nil {
		p.HomePageID = *home
	}
	return &p, nil
}

// GetPortalPages — the dashboards a portal may actually open, in curated order.
//
// The filter matches serve's 404 conditions exactly (soft-deleted, taken down,
// or nothing published), because a portal index that lists a page which then
// answers 404 is worse than one that omits it: the partner cannot tell a
// permissions problem from a broken link. `label` wins over `pages.title`
// because a title is agent-settable. The home page sorts first — that is what
// home_page_id is FOR — and sort_order decides the rest.
//
// The live version is joined for its timestamps, the partner's only freshness
// signal (#176): source_as_of says when the DATA is from, created_at when the
// live version was made. LEFT, not INNER: an inner join would let a dangling
// pointer silently drop a page the partner can still open.
func GetPortalPages(ctx context.Context, q db.Querier, portalID int64) ([]PortalPage, error) {
	rows, err := q.Query(ctx,
		`SELECT p.id, p.slug,
		        COALESCE(m.label, NULLIF(p.title, ''), p.slug) AS title,
		        m.sort_order,
		        (pp.home_page_id = p.id) AS is_home,
		        v.created_at AS published_at,
		        v.source_as_of AS source_as_of
		   FROM page_portal_members m
		   JOIN page_portals pp ON pp.id = m.portal_id
		   JOIN pages p ON p.id = m.page_id
		   LEFT JOIN page_versions v ON v.id = p.published_version_id
		  WHERE m.portal_id = $1
		    AND p.deleted_at IS NULL
		    AND NOT p.disabled
		    AND p.published_version_id IS NOT NULL
		  ORDER BY (pp.home_page_id = p.id) DESC,
		           m.sort_order,
		           lower(COALESCE(m.label, NULLIF(p.title, ''), p.slug)),
		           p.slug`, portalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PortalPage{}
	for rows.Next() {
		var (
			pg        PortalPage
			home      *bool
			pub, asOf *time.Time
		)
		if err := rows.Scan(&pg.ID, &pg.Slug, &pg.Title, &pg.SortOrder, &home, &pub, &asOf); err != nil {
			return nil, err
		}
		pg.IsHome = home != nil && *home
		if pub != nil {
			pg.PublishedAt = *pub
		}
		if asOf != nil {
			pg.SourceAsOf = *asOf
		}
		out = append(out, pg)
	}
	return out, rows.Err()
}

// GetPortalsForPage — every live portal that currently contains this page,
// ordered by id. Read per request rather than baked into a session: removing a
// page from a portal has to be effective on the very next request. The order
// is the tie-break AuthorizingPortal relies on: lowest portal id, always.
func GetPortalsForPage(ctx context.Context, q db.Querier, pageID int64) ([]Portal, error) {
	rows, err := q.Query(ctx,
		`SELECT pp.id, pp.slug, pp.name, pp.password_hash
		   FROM page_portal_members m
		   JOIN page_portals pp ON pp.id = m.portal_id
		  WHERE m.page_id = $1 AND pp.deleted_at IS NULL
		  ORDER BY pp.id`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Portal{}
	for rows.Next() {
		var p Portal
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.PasswordHash); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
