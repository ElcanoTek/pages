// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"fmt"
	"sort"
	"strings"
)

const (
	detailSlug = "long/client/q2-report"
	// gatedSlug is an approval-gated page whose only versions are still in the
	// queue: it has source, it just has nothing LIVE. The admin must not tell
	// that page's reviewer there is no source at all. (client-20 already
	// carries published_version_id null + require_approval true from the
	// generated run below.)
	gatedSlug   = "client-20"
	longTitle   = "North America Programmatic Revenue and Performance Review — Quarter Two Executive Detail"
	versionHTML = "<!doctype html><html><head><title>Fixture page</title></head><body><main><h1>Published fixture source</h1><p>Charts remain intact.</p></main></body></html>"
)

// sourceFor gives every version its OWN source, so a test can tell which one
// the editor seeded itself from. Only `published` ships its html in the detail
// payload; anything else is read through GET /versions/:id, exactly as
// production does.
func sourceFor(label string) string {
	return replaceFirst(versionHTML, "Published fixture source", label+" fixture source")
}

// state is everything /__fixture/reset restores.
type state struct {
	pages      *list
	workspaces *list
	// details maps a slug to its versions, newest first.
	details        map[string]*list
	events         *list
	failNextPages  bool
	delayNextPages float64
	nextWorkspace  float64
	nextPage       float64
	nextVersion    float64
	templates      *list
}

func version(id, pageID int, status, mode, author, source string, note any, reviewedBy any, reviewedAt any, created string, html string) obj {
	return o("id", id, "page_id", pageID, "status", status, "render_mode", mode, "author", author,
		"source", source, "note", note, "reviewed_by", reviewedBy, "reviewed_at", reviewedAt,
		"created_at", created, "html", html)
}

func initialState() *state {
	pages := newList(o(
		"id", 1,
		"slug", detailSlug,
		"title", longTitle,
		"client_id", nil,
		"workspace_id", 1,
		"workspace_name", "Campaign operations",
		"theme_id", nil,
		"theme_name", "flag",
		"require_approval", true,
		"disabled", false,
		"published_version_id", 102,
		"has_password", true,
		"is_live", true,
		"created_at", iso(5000),
		"updated_at", iso(5),
		// Frozen data behind a recently-touched page row: the exact shape the
		// "Last update" column could never show, since updated_at moves when
		// someone renames a page and stays put when a daily refresh stops.
		"freshness", o(
			"source_as_of", iso(46*1440),
			"refreshed_at", iso(27*1440),
			"checked_at", iso(2),
			"last_check_at", iso(2),
			"latest_outcome", "source_not_updated",
			"latest_detail", "upstream max date still 2026-07-02",
			"last_check_outcome", "source_not_updated",
			"last_check_detail", "upstream max date still 2026-07-02",
			"last_check_source_as_of", iso(46*1440),
			"days_since_source", 46,
			"days_since_refresh", 27,
			"days_since_check", 0,
		),
	))
	for index := 1; index <= 36; index++ {
		number := fmt.Sprintf("%02d", index)
		var published any
		if index%4 != 0 {
			published = float64(200 + index)
		}
		slug, title := "client-"+number, "Client "+number+" operations dashboard"
		if index == 36 {
			slug = "nested/team/annual-performance-with-a-very-long-slug"
			title = "A very long client title that must wrap without widening the page at any viewport"
		}
		var workspaceID, workspaceName any
		if index%3 != 0 {
			if index%2 == 1 {
				workspaceID, workspaceName = float64(1), "Campaign operations"
			} else {
				workspaceID, workspaceName = float64(2), "Executive reporting"
			}
		}
		// Most rows are managed and current; every fourth is a plain page with
		// no managed data at all, which must render no freshness line rather
		// than a row of zeroes.
		var freshness any
		if published != nil {
			day := float64(index * 1440)
			freshness = o(
				"source_as_of", iso(day),
				"refreshed_at", iso(day),
				"checked_at", iso(day),
				"last_check_at", iso(day+60),
				"latest_outcome", "updated",
				"latest_detail", nil,
				"last_check_outcome", "blocked",
				"last_check_detail", nil,
				"last_check_source_as_of", nil,
				"days_since_source", index,
				"days_since_refresh", index,
				"days_since_check", index,
			)
		}
		pages.push(o(
			"id", index+1,
			"slug", slug,
			"title", title,
			"client_id", nil,
			"workspace_id", workspaceID,
			"workspace_name", workspaceName,
			"theme_id", nil,
			"theme_name", "flag",
			"require_approval", index%5 == 0,
			"disabled", index%11 == 0,
			"published_version_id", published,
			"has_password", index%2 == 0,
			"is_live", published != nil && index%11 != 0,
			"created_at", iso(float64(4000-index)),
			"updated_at", iso(float64(index*13)),
			"freshness", freshness,
		))
	}
	return &state{
		pages: pages,
		workspaces: newList(
			o("id", 1, "name", "Campaign operations", "created_at", iso(6000), "updated_at", iso(6000)),
			o("id", 2, "name", "Executive reporting", "created_at", iso(5900), "updated_at", iso(5900)),
		),
		details: map[string]*list{
			gatedSlug: newList(
				version(300, 21, "pending", "themed", "agent@elcanotek.com", "mcp", "First cut, waiting on review", nil, nil, iso(15), sourceFor("Gated")),
			),
			detailSlug: newList(
				version(106, 1, "pending", "themed", "review-agent-with-a-long-address@elcanotek.com", "mcp", "Refresh the revenue totals and preserve the attribution chart. This intentionally long note must wrap cleanly on narrow screens.", nil, nil, iso(2), sourceFor("Pending")),
				version(105, 1, "pending", "themed", "analyst@elcanotek.com", "api", "Client corrections", nil, nil, iso(10), sourceFor("Corrections")),
				version(104, 1, "draft", "raw", "admin@elcanotek.com", "admin", "Alternative draft", nil, nil, iso(20), sourceFor("Draft")),
				version(103, 1, "rejected", "themed", "agent@elcanotek.com", "mcp", "Rejected copy", "reviewer@elcanotek.com", iso(25), iso(30), sourceFor("Rejected")),
				version(102, 1, "approved", "themed", "publisher@elcanotek.com", "api", "Current client-approved version", "reviewer@elcanotek.com", iso(35), iso(40), versionHTML),
				version(101, 1, "approved", "themed", "publisher@elcanotek.com", "api", "Prior live version", "reviewer@elcanotek.com", iso(50), iso(60), sourceFor("Prior")),
			),
		},
		events:        newList(),
		nextWorkspace: 3,
		nextPage:      50,
		nextVersion:   107,
		templates:     initialTemplates(),
	}
}

// versionsOf is `state.details[slug] || (state.details[slug] = { versions: [] })`.
func (s *state) versionsOf(slug string) *list {
	v, ok := s.details[slug]
	if !ok {
		v = newList()
		s.details[slug] = v
	}
	return v
}

func (s *state) workspaceRows() *list {
	out := newList()
	for _, item := range s.workspaces.items {
		workspace := asObj(item)
		count := 0
		for _, p := range s.pages.items {
			if str(or(get(p, "workspace_id"), "")) == str(get(workspace, "id")) {
				count++
			}
		}
		out.push(spread(workspace, "page_count", count))
	}
	return out
}

func (s *state) pageRows() *list {
	out := newList()
	for _, item := range s.pages.items {
		page := asObj(item)
		var versions []any
		if v, ok := s.details[str(get(page, "slug"))]; ok {
			versions = v.items
		}
		publishedID := get(page, "published_version_id")
		count := float64(len(versions))
		if count == 0 && truthy(publishedID) {
			count = 1
		}
		var number any
		if truthy(publishedID) {
			number = float64(1)
			// chronological: oldest first
			for i := len(versions) - 1; i >= 0; i-- {
				if sameString(get(versions[i], "id"), publishedID) {
					number = float64(len(versions) - i)
					break
				}
			}
		}
		out.push(spread(page, "version_count", count, "published_version_number", number))
	}
	return out
}

// findPageFromPath resolves the longest slug the raw path starts with.
func (s *state) findPageFromPath(rawPath string) (obj, string) {
	decoded, err := decodeURIComponent(rawPath)
	if err != nil {
		decoded = rawPath
	}
	sorted := append([]any(nil), s.pages.items...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return jsLen(str(get(sorted[i], "slug"))) > jsLen(str(get(sorted[j], "slug")))
	})
	for _, item := range sorted {
		slug := str(get(item, "slug"))
		if decoded == slug || strings.HasPrefix(decoded, slug+"/") {
			return asObj(item), decoded[len(slug):]
		}
	}
	return nil, ""
}

func (s *state) detailFor(page obj) obj {
	versions := s.versionsOf(str(get(page, "slug")))
	var published any
	if v := versions.find(func(v obj) bool { return sameString(get(v, "id"), get(page, "published_version_id")) }); v != nil {
		published = v
	}
	deck := versions.len() > 0 && isDeck(get(versions.at(0), "html"))
	metas := newList()
	pending := newList()
	for _, item := range versions.items {
		meta := without(asObj(item), "html")
		metas.push(meta)
		if str(get(item, "status")) == "pending" {
			pending.push(meta)
		}
	}
	return o(
		"page", page,
		"deck", deck,
		"versions", metas,
		"pending", pending,
		"published", published,
		"themes", newList(
			o("id", 1, "name", "flag", "default_mode", "system"),
			o("id", 2, "name", "client-brand", "default_mode", "light"),
		),
		"csrfOk", true,
	)
}

func (s *state) record(r *req, page obj) {
	var slug any
	if page != nil {
		slug = get(page, "slug")
	}
	s.events.push(o("method", r.Method, "path", r.rest, "slug", slug, "body", r.bodyOr()))
}
