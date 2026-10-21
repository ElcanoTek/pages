// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package portals

import (
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
)

func portalNotFound() error { return apierror.NotFound("portal not found", "portal_not_found", nil) }

// List is every live portal with its live member count and home page slug,
// ordered by name.
func List(ctx context.Context, q db.Querier) ([]Summary, error) {
	rows, err := q.Query(ctx,
		`SELECT pp.id, pp.slug, pp.name, pp.home_page_id, hp.slug AS home_page_slug,
		        pp.created_at, pp.updated_at,
		        COUNT(p.id) FILTER (WHERE p.deleted_at IS NULL)::int AS page_count
		   FROM page_portals pp
		   LEFT JOIN page_portal_members m ON m.portal_id = pp.id
		   LEFT JOIN pages p ON p.id = m.page_id
		   LEFT JOIN pages hp ON hp.id = pp.home_page_id AND hp.deleted_at IS NULL
		  WHERE pp.deleted_at IS NULL
		  GROUP BY pp.id, hp.slug
		  ORDER BY lower(pp.name), pp.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var s Summary
		if err := rows.Scan(&s.ID, &s.Slug, &s.Name, &s.HomePageID, &s.HomePageSlug,
			&s.CreatedAt, &s.UpdatedAt, &s.PageCount); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// The Page-menu ownership test, run in the database so the multi-megabyte document
// stays there. Since every portal-authorised render gets a Page menu (themed
// via the template block or Pages' built-in control, raw via the injected
// fixed-position control), any published member shows one; what is still worth
// surfacing is WHOSE menu it is — matched on actually CONSUMING the block (a
// DOM read or an element carrying the id), mirroring render.readsNavBlock:
// authoring boilerplate mentions #pages-nav in a CSS comment without ever
// building the control, and a bare-mention match called that "owns its menu"
// while the page showed none.
const switcherIsOwnSQL = `(v.html ~ 'getElementById\s*\(\s*["'']pages-nav["'']\s*\)|querySelector(All)?\s*\(\s*["'']#?pages-nav["'']\s*\)|\yid\s*=\s*["'']pages-nav["'']')`

// Get is one portal and its membership, for the admin screen (404
// portal_not_found). Members whose page has been soft-deleted are RETURNED,
// flagged page_deleted, so a stale row is visible and removable instead of
// invisible and permanent. id is a raw id (see NormalizeID).
func Get(ctx context.Context, q db.Querier, id any) (*Detail, error) {
	pid, err := NormalizeID(id, "")
	if err != nil {
		return nil, err
	}
	d := &Detail{}
	p := &d.Portal
	err = q.QueryRow(ctx,
		`SELECT pp.id, pp.slug, pp.name, pp.home_page_id, hp.slug AS home_page_slug,
		        pp.created_at, pp.updated_at
		   FROM page_portals pp
		   LEFT JOIN pages hp ON hp.id = pp.home_page_id AND hp.deleted_at IS NULL
		  WHERE pp.id = $1 AND pp.deleted_at IS NULL`, pid).
		Scan(&p.ID, &p.Slug, &p.Name, &p.HomePageID, &p.HomePageSlug, &p.CreatedAt, &p.UpdatedAt)
	if db.IsNoRows(err) {
		return nil, portalNotFound()
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx,
		`SELECT m.page_id, p.slug, p.title, m.label,
		        COALESCE(m.label, NULLIF(p.title, ''), p.slug) AS display_title,
		        m.sort_order, m.added_at,
		        (p.password_hash IS NOT NULL) AS has_password,
		        p.disabled,
		        (p.published_version_id IS NOT NULL) AS published,
		        (p.deleted_at IS NOT NULL) AS page_deleted,
		        (v.id IS NOT NULL) AS shows_switcher,
		        `+switcherIsOwnSQL+` AS switcher_is_own,
		        v.render_mode
		   FROM page_portal_members m
		   JOIN pages p ON p.id = m.page_id
		   LEFT JOIN page_versions v ON v.id = p.published_version_id
		  WHERE m.portal_id = $1
		  ORDER BY m.sort_order, lower(COALESCE(m.label, NULLIF(p.title, ''), p.slug)), p.slug`, pid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	d.Members = []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.PageID, &m.Slug, &m.Title, &m.Label, &m.DisplayTitle, &m.SortOrder, &m.AddedAt,
			&m.HasPassword, &m.Disabled, &m.Published, &m.PageDeleted, &m.ShowsSwitcher, &m.SwitcherIsOwn,
			&m.RenderMode); err != nil {
			return nil, err
		}
		if !m.PageDeleted {
			p.PageCount++
		}
		d.Members = append(d.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d.LinkAudit = linkAudit(ctx, q, p.HomePageID, d.Members)
	return d, nil
}

// ListForPage is the other direction: which live portals expose this page.
// The admin page detail needs it, because a page missing from one audience's
// portal is invisible when you only ever look portal-first.
func ListForPage(ctx context.Context, q db.Querier, pageID jsx.ID) ([]PageMembership, error) {
	rows, err := q.Query(ctx,
		`SELECT pp.id, pp.slug, pp.name, m.label, m.sort_order,
		        (pp.home_page_id = m.page_id) AS is_home
		   FROM page_portal_members m
		   JOIN page_portals pp ON pp.id = m.portal_id
		  WHERE m.page_id = $1 AND pp.deleted_at IS NULL
		  ORDER BY lower(pp.name), pp.id`, pageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PageMembership{}
	for rows.Next() {
		var m PageMembership
		if err := rows.Scan(&m.ID, &m.Slug, &m.Name, &m.Label, &m.SortOrder, &m.IsHome); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ── link audit (read-only) ──────────────────────────────────────────────────
// The observed shape of portal drift (Fabrikam, 2026-08-19): the home page is
// a hand-authored hub whose links agents keep current, while membership — the
// thing that actually authorises a partner and carries the Page menu — is
// curated by a human and silently falls behind. A delete→recreate even drops a
// member with nobody touching the list, because membership binds to page_id.
// The partner then clicks a link the hub proudly shows and the nav vanishes —
// or a password wall appears. This audit makes that drift visible exactly
// where the human curates: same-host links in the HOME page's published HTML
// that resolve to live pages not in the member list. Read-only by design —
// membership stays a human decision; the screen offers the click.

const linkAuditMaxCandidates = 200

// JavaScript's \s (jsstr.IsSpace), spelled out: Go's \s is ASCII only.
const jsSpace = `[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]`

var (
	hrefRE = regexp.MustCompile(`(?i:href)` + jsSpace + `*=` + jsSpace + `*(?:"([^"]*)"|'([^']*)')`)
	// Another scheme (https:, mailto:, …). ASCII letters only, as the
	// JavaScript /i (non-unicode) class matched.
	schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	// One nested slug: segments of [a-z0-9] with inner -/_, joined by "/". The
	// same shape pages.slug enforces, so anything else cannot be a page and is
	// skipped without a database round-trip.
	linkableSlugRE = regexp.MustCompile(`^[a-z0-9]+([-_][a-z0-9]+)*(/[a-z0-9]+([-_][a-z0-9]+)*)*$`)
)

// Paths under the content host that are routes, not page slugs.
var nonPagePrefixes = []string{"portal/", "raw/", "raw-template/", "shell-assets/"}

// ExtractLinkedSlugs pulls candidate page slugs (deduplicated, first-seen
// order, at most 200) out of href attributes: absolute links on contentOrigin
// and root-relative links. Bare-relative hrefs are skipped on purpose — slugs
// nest, so on /a/b/c a relative "d" resolves to /a/b/d and naming the author's
// intent from here is a guess. contentOrigin "" means env.ContentOrigin().
func ExtractLinkedSlugs(html, contentOrigin string) []string {
	if contentOrigin == "" {
		contentOrigin = env.ContentOrigin()
	}
	seen := map[string]bool{}
	out := []string{}
	for _, m := range hrefRE.FindAllStringSubmatchIndex(html, -1) {
		if len(out) >= linkAuditMaxCandidates {
			break
		}
		var target string
		if m[2] >= 0 {
			target = html[m[2]:m[3]]
		} else {
			target = html[m[4]:m[5]]
		}
		target = jsstr.Trim(target)
		if strings.HasPrefix(target, contentOrigin+"/") {
			target = target[len(contentOrigin):]
		} else if strings.HasPrefix(target, "//") || schemeRE.MatchString(target) {
			continue // another host or scheme
		}
		if !strings.HasPrefix(target, "/") {
			continue
		}
		slug := target[1:]
		if i := strings.IndexAny(slug, "?#"); i >= 0 {
			slug = slug[:i]
		}
		slug = strings.TrimRight(slug, "/")
		if slug == "" || !linkableSlugRE.MatchString(slug) {
			continue
		}
		routed := false
		for _, prefix := range nonPagePrefixes {
			if slug == prefix[:len(prefix)-1] || strings.HasPrefix(slug, prefix) {
				routed = true
				break
			}
		}
		if routed || seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}

// linkAudit is the detail-read companion of ExtractLinkedSlugs. It never
// fails: an audit that cannot run must not take the admin screen down with
// it, so any error degrades to {scanned: false} and a log line.
func linkAudit(ctx context.Context, q db.Querier, homePageID jsx.ID, members []Member) LinkAudit {
	unscanned := LinkAudit{Scanned: false, Missing: []LinkedPage{}}
	if homePageID == 0 {
		return unscanned
	}
	var html string
	err := q.QueryRow(ctx,
		`SELECT v.html
		   FROM pages p
		   JOIN page_versions v ON v.id = p.published_version_id
		  WHERE p.id = $1 AND p.deleted_at IS NULL`, homePageID).Scan(&html)
	if db.IsNoRows(err) {
		return unscanned
	}
	if err != nil {
		slog.Error("portals: link audit unavailable", "err", err)
		return unscanned
	}
	slugs := ExtractLinkedSlugs(html, "")
	if len(slugs) == 0 {
		return LinkAudit{Scanned: true, Missing: []LinkedPage{}}
	}
	memberIDs := map[jsx.ID]bool{}
	for _, m := range members {
		memberIDs[m.PageID] = true
	}
	rows, err := q.Query(ctx,
		`SELECT id, slug, title
		   FROM pages
		  WHERE slug = ANY($1)
		    AND deleted_at IS NULL
		    AND NOT disabled
		    AND published_version_id IS NOT NULL
		  ORDER BY slug`, slugs)
	if err != nil {
		slog.Error("portals: link audit unavailable", "err", err)
		return unscanned
	}
	defer rows.Close()
	missing := []LinkedPage{}
	for rows.Next() {
		var p LinkedPage
		if err := rows.Scan(&p.PageID, &p.Slug, &p.Title); err != nil {
			slog.Error("portals: link audit unavailable", "err", err)
			return unscanned
		}
		if !memberIDs[p.PageID] && p.PageID != homePageID {
			missing = append(missing, p)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Error("portals: link audit unavailable", "err", err)
		return unscanned
	}
	return LinkAudit{Scanned: true, Missing: missing}
}
