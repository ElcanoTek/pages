// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"fmt"
	"math"
	"net/http"

	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/preflight"
)

func sendBytes(w http.ResponseWriter, r *http.Request, status int, payload []byte) {
	httpx.Send(w, r, status, "application/json; charset=utf-8", payload)
}

// buildRoutes registers every route in the order the Node release's Express
// fixture did, which
// is the order Express matched them in.
func (a *App) buildRoutes() []route {
	var routes []route
	routes = append(routes, a.controlRoutes()...)
	routes = append(routes, staticRoutes()...)
	routes = append(routes, a.shellRoutes()...)
	routes = append(routes, a.portalRoutes()...)
	routes = append(routes, a.contentRoutes()...)
	routes = append(routes, route{pattern: "/api/v1/admin", mount: true, mw: a.adminAPI})
	routes = append(routes, a.bentoRoutes()...)
	routes = append(routes, a.realBentoRoutes()...)
	return routes
}

func (a *App) ok(w http.ResponseWriter, r *req) { sendJSON(w, r.Request, http.StatusOK, o("ok", true)) }

// locked runs mutate under the state lock and answers {"ok":true}.
func (a *App) locked(mutate func(r *req)) handler {
	return func(w http.ResponseWriter, r *req) {
		a.mu.Lock()
		mutate(r)
		a.mu.Unlock()
		a.ok(w, r)
	}
}

// controlRoutes are the /__fixture/* hooks the specs drive the state with.
func (a *App) controlRoutes() []route {
	return []route{
		{method: "GET", pattern: "/healthz", h: func(w http.ResponseWriter, r *req) { sendText(w, r.Request, http.StatusOK, "ok") }},
		{method: "POST", pattern: "/__fixture/reset", h: a.locked(func(*req) { a.st = initialState() })},
		{method: "GET", pattern: "/__fixture/events", h: func(w http.ResponseWriter, r *req) {
			a.mu.Lock()
			payload := marshal(o("events", a.st.events))
			a.mu.Unlock()
			sendBytes(w, r.Request, http.StatusOK, payload)
		}},
		{method: "POST", pattern: "/__fixture/empty", h: a.locked(func(*req) {
			a.st.pages = newList()
			a.st.details = map[string]*list{}
		})},
		{method: "POST", pattern: "/__fixture/empty-workspaces", h: a.locked(func(*req) {
			a.st.workspaces = newList()
			for _, p := range a.st.pages.items {
				set(asObj(p), "workspace_id", nil)
				set(asObj(p), "workspace_name", nil)
			}
		})},
		{method: "POST", pattern: "/__fixture/empty-templates", h: a.locked(func(*req) { a.st.templates = newList() })},
		// A page whose newest version is a Bento deck, so the detail screen has
		// an "Edit in Bento" to offer. On request, like pad-pages: the index
		// specs pin the default page count.
		{method: "POST", pattern: "/__fixture/deck-page", h: a.locked(func(*req) {
			const slug = "team/bento-guide"
			a.st.pages = a.st.pages.filter(func(p obj) bool { return get(p, "slug") != any(slug) })
			a.st.pages.unshift(o(
				"id", 48, "slug", slug, "title", "Team Bento guide", "client_id", nil, "workspace_id", nil, "workspace_name", nil,
				"theme_id", nil, "theme_name", "flag", "require_approval", false, "disabled", false, "published_version_id", 401,
				"has_password", false, "is_live", true, "created_at", iso(500), "updated_at", iso(5), "freshness", nil,
			))
			a.st.details[slug] = newList(
				version(401, 48, "approved", "raw", "qa-admin@elcanotek.com", "admin", "Deck, first cut", nil, nil, iso(5), bentoDeck),
			)
		})},
		// 37 pages against a 25-row window means "Show more" is pressed exactly
		// once and always removes itself, so the branch where the button
		// SURVIVES a press is unreachable with the default fixture — and that
		// is the branch a real account will live in once it passes fifty
		// pages. This pads the list so both can be pinned.
		{method: "POST", pattern: "/__fixture/pad-pages", h: func(w http.ResponseWriter, r *req) {
			count := math.Max(0, math.Min(200, num(or(get(r.body, "count"), 40))))
			a.mu.Lock()
			base := a.st.pages.len()
			for index := 1; float64(index) <= count; index++ {
				a.st.pages.push(o(
					"id", 1000+base+index,
					"slug", fmt.Sprintf("padded-%03d", index),
					"title", fmt.Sprintf("Padded page %d", index),
					"client_id", nil,
					"workspace_id", nil,
					"workspace_name", nil,
					"theme_id", nil,
					"theme_name", "flag",
					"require_approval", false,
					"disabled", false,
					"published_version_id", 9000+index,
					"has_password", false,
					"is_live", true,
					"created_at", iso(9000),
					"updated_at", iso(float64(index)),
					"freshness", nil,
				))
			}
			total := a.st.pages.len()
			a.mu.Unlock()
			sendJSON(w, r.Request, http.StatusOK, o("ok", true, "pages", total))
		}},
		{method: "POST", pattern: "/__fixture/fail-next-pages", h: a.locked(func(*req) { a.st.failNextPages = true })},
		{method: "POST", pattern: "/__fixture/delay-next-pages", h: a.locked(func(r *req) {
			a.st.delayNextPages = num(or(get(r.body, "milliseconds"), 500))
		})},
		// One-shot per-portal delay, so a spec can make an EARLIER detail
		// request land after a later one — the stale-response race,
		// deterministically.
		{method: "POST", pattern: "/__fixture/delay-portal-detail", h: a.locked(func(r *req) {
			id := get(r.body, "id")
			key := ""
			if _, missing := id.(undefined); !missing && id != nil {
				key = str(id)
			}
			a.portalDetailDelay[key] = num(or(get(r.body, "milliseconds"), 500))
		})},
		// The content host's policy and the preflight verdict, for
		// preflight-resources.spec.js: it asserts that what preflight blocks is
		// exactly what the served CSP blocks, so both answers must come from
		// the server that ships, not from a second implementation in the
		// spec's own process.
		{method: "GET", pattern: "/__fixture/content-policy", h: func(w http.ResponseWriter, r *req) {
			headers := o()
			for _, h := range csp.RawHeaders() {
				headers.Set(h.Name, h.Value)
			}
			sendJSON(w, r.Request, http.StatusOK, o("content_origin", env.ContentOrigin(), "raw_headers", headers))
		}},
		{method: "POST", pattern: "/__fixture/preflight", h: func(w http.ResponseWriter, r *req) {
			src, _ := get(r.body, "html").(string)
			mode, _ := get(r.body, "render_mode").(string)
			httpx.SendJSON(w, r.Request, http.StatusOK, preflight.Analyze(src, preflight.Options{RenderMode: mode}))
		}},
	}
}
