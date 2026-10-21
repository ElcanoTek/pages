// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"net/http"
	"sort"
	"strings"
	"unicode"
)

// ── partner portals: the admin JSON surface, in memory ──────────────────────
// Enough state to drive the real screen: one portal whose membership covers
// every warning the UI can show, plus a staff-only page that is NOT yet a
// member — the reclassification notice is the thing most worth seeing in a
// browser, because it is the sentence that stands between an admin and
// publishing a confidential dashboard to a partner.

func (a *App) initialPortals() *list {
	return newList(
		o("id", 7, "slug", "nwm", "name", "Northwind Media Group", "home_page_id", 71, "home_page_slug", "nwm-client-overview",
			"page_count", 2, "created_at", iso(4000), "updated_at", iso(12), "url", a.origin+"/live/portal/nwm"),
		// A second portal, so the screen has something to switch BETWEEN: the
		// selected portal is route state and the detail request is racy, and
		// neither can be shown with a list of one.
		o("id", 9, "slug", "fabrikam", "name", "Fabrikam", "home_page_id", nil, "home_page_slug", nil,
			"page_count", 1, "created_at", iso(3000), "updated_at", iso(30), "url", a.origin+"/live/portal/fabrikam"),
	)
}

func member(pageID int, slug, title string, label any, display string, order int, added float64, password, disabled bool, own bool, mode string) obj {
	return o("page_id", pageID, "slug", slug, "title", title, "label", label, "display_title", display,
		"sort_order", order, "added_at", iso(added), "has_password", password, "disabled", disabled,
		"published", true, "page_deleted", false, "shows_switcher", true, "switcher_is_own", own, "render_mode", mode)
}

// Membership is mutable here: /pages/update really applies the sort_order it
// is sent, because the whole point of #173's controls is that the list comes
// back in a different order — a stub that echoed one member proved only that a
// request was made. resetPortalMembers puts the fixtures back so a spec that
// reorders does not decide what the next spec sees.
func (a *App) resetPortalMembers() {
	a.membersByPortal = map[string]*list{
		// One of each state the screen can report: a design that renders its
		// own menu, one that gets the built-in control (and was staff-only
		// before this portal), and a raw page — which since #125 gets the
		// injected menu like everything else, so the only warning its row earns
		// is the takedown.
		"7": newList(
			member(71, "nwm-client-overview", "NWM Client Overview", "Portfolio overview", "Portfolio overview", 0, 4000, true, false, true, "themed"),
			member(72, "nwm/contoso-fabrikam", "Contoso Fabrikam", nil, "Contoso Fabrikam", 1, 2000, false, false, false, "themed"),
			member(73, "nwm-taken-down", "Taken down", nil, "Taken down", 2, 900, true, true, false, "raw"),
		),
		"9": newList(
			member(91, "fabrikam-ssp-weekly", "Fabrikam SSP weekly", nil, "Fabrikam SSP weekly", 0, 3000, true, false, true, "themed"),
		),
	}
}

// localeCompare approximates String#localeCompare (ICU root collation) for
// the strings the fixture sorts: punctuation and spaces before digits before
// letters, letters compared case-insensitively first, lower case first on a
// tie.
func localeCompare(x, y string) int {
	key := func(r rune) (int, rune) {
		switch {
		case unicode.IsLetter(r):
			return 2, unicode.ToLower(r)
		case unicode.IsDigit(r):
			return 1, r
		}
		return 0, r
	}
	xr, yr := []rune(x), []rune(y)
	for i := 0; i < len(xr) && i < len(yr); i++ {
		cx, kx := key(xr[i])
		cy, ky := key(yr[i])
		if cx != cy {
			return cx - cy
		}
		if kx != ky {
			if kx < ky {
				return -1
			}
			return 1
		}
	}
	if len(xr) != len(yr) {
		return len(xr) - len(yr)
	}
	for i := range xr {
		if xr[i] != yr[i] {
			if unicode.IsLower(xr[i]) {
				return -1
			}
			return 1
		}
	}
	return 0
}

// sortMembers is the ORDER BY the real portal read uses, so the browser sees
// the order the real screen would get back: curated position first, then a
// stable tiebreak.
func sortMembers(members *list) {
	sort.SliceStable(members.items, func(i, j int) bool {
		a, b := members.items[i], members.items[j]
		if d := num(get(a, "sort_order")) - num(get(b, "sort_order")); d != 0 {
			return d < 0
		}
		if c := localeCompare(strings.ToLower(str(get(a, "display_title"))), strings.ToLower(str(get(b, "display_title")))); c != 0 {
			return c < 0
		}
		return localeCompare(str(get(a, "slug")), str(get(b, "slug"))) < 0
	})
}

// The drift the link audit exists for: the home page links these live pages,
// but nobody added them as members — a partner following the link loses the
// nav. TWO of them, because "Add all N" only exists over more than one row,
// and one of the two has no password of its own, so adding it really does
// reclassify a staff-only page and the screen has something true to confirm
// afterwards.
var fixtureLinkedPages = []struct {
	id          float64
	slug, title string
	password    bool
}{
	{81, "nwm-woodgrove", "Woodgrove campaign", true},
	{82, "nwm-tailspin-pet", "Tailspin Pet Q3", false},
}

// linkAudit is what the real link audit sends: id, slug, title — and only the
// ones that are not already members. has_password is NOT in the payload;
// whether an add reclassified anything is reported by the ADD response, which
// is the whole reason the screen can only say it afterwards. Recomputed per
// request rather than frozen, so a spec can assert that adding a linked page
// clears its row.
func (a *App) linkAudit(portalID string) obj {
	members := a.membersByPortal[portalID]
	missing := newList()
	for _, p := range fixtureLinkedPages {
		found := members != nil && members.find(func(m obj) bool { return sameString(get(m, "page_id"), p.id) }) != nil
		if !found {
			missing.push(o("page_id", p.id, "slug", p.slug, "title", p.title))
		}
	}
	return o("scanned", true, "missing", missing)
}

// pageBySlug: the real add-a-member route takes any page by slug. The link
// audit's own pages exist but are deliberately NOT in state.pages, which the
// index specs own and count — so resolving only against state.pages made "Add
// to portal" answer 404, and a spec asserting only the outbound request body
// never noticed.
func (a *App) pageBySlug(slug any) obj {
	if page := a.st.pages.find(func(p obj) bool { return get(p, "slug") == slug }); page != nil {
		return page
	}
	for _, p := range fixtureLinkedPages {
		if slug == any(p.slug) {
			return o("id", p.id, "slug", p.slug, "title", p.title, "has_password", p.password)
		}
	}
	return nil
}

func (a *App) findPortal(id string) (int, obj) {
	i := a.portals.findIndex(func(p obj) bool { return str(get(p, "id")) == id })
	if i < 0 {
		return -1, nil
	}
	return i, a.portals.at(i)
}

func portalNotFound() obj { return o("error", "portal not found", "code", "portal_not_found") }

func (a *App) portalRoutes() []route {
	return []route{
		// One endpoint restores BOTH halves of the portal fixtures — the list
		// and each portal's membership.
		{method: "POST", pattern: "/__fixture/reset-portals", h: func(w http.ResponseWriter, r *req) {
			a.mu.Lock()
			a.portals = clone(a.portalSeed).(*list)
			a.resetPortalMembers()
			a.mu.Unlock()
			sendJSON(w, r.Request, http.StatusOK, o("ok", true))
		}},
		// One-shot delay on the LIST, so a spec can hold a reload's list
		// response open and click a row while it is in flight — the race that
		// used to discard it.
		{method: "POST", pattern: "/__fixture/delay-portal-list", h: func(w http.ResponseWriter, r *req) {
			a.mu.Lock()
			a.portalListDelay = num(or(get(r.body, "milliseconds"), 500))
			a.mu.Unlock()
			sendJSON(w, r.Request, http.StatusOK, o("ok", true))
		}},
		{method: "GET", pattern: "/api/v1/admin/portals", h: func(w http.ResponseWriter, r *req) {
			a.mu.Lock()
			delay := a.portalListDelay
			a.portalListDelay = 0
			a.mu.Unlock()
			// The body is built when it is SENT, so a delayed list still
			// reports whatever the mutation that triggered the reload did.
			if truthy(delay) {
				sleep(delay)
			}
			a.mu.Lock()
			payload := marshal(o("portals", a.portals))
			a.mu.Unlock()
			sendRaw(w, r, http.StatusOK, payload)
		}},
		{method: "GET", pattern: "/api/v1/admin/portals/:id", h: func(w http.ResponseWriter, r *req) {
			id := r.params["id"]
			a.mu.Lock()
			_, portal := a.findPortal(id)
			// The real route 404s an id it does not hold. Answering
			// 200-with-the-first-portal instead hid exactly the bug class this
			// screen has — a stale id rendering somebody else's detail.
			if portal == nil {
				a.mu.Unlock()
				sendJSON(w, r.Request, http.StatusNotFound, portalNotFound())
				return
			}
			var members any = newList()
			if m, ok := a.membersByPortal[id]; ok {
				members = m
			}
			audit := o("scanned", true, "missing", newList())
			if id == "7" {
				audit = a.linkAudit(id)
			}
			body := o("portal", portal, "members", members, "link_audit", audit)
			delay, hasDelay := a.portalDetailDelay[id]
			if hasDelay && truthy(delay) {
				delete(a.portalDetailDelay, id)
			}
			a.mu.Unlock()
			// Serialised when sent, as res.json(body) inside setTimeout was:
			// the portal and its member list are live references.
			if hasDelay && truthy(delay) {
				sleep(delay)
			}
			a.mu.Lock()
			payload := marshal(body)
			a.mu.Unlock()
			sendRaw(w, r, http.StatusOK, payload)
		}},
		{method: "POST", pattern: "/api/v1/admin/portals", h: func(w http.ResponseWriter, r *req) {
			body := r.bodyOr()
			slug, name := get(body, "slug"), get(body, "name")
			if !truthy(slug) || !truthy(name) {
				sendJSON(w, r.Request, http.StatusBadRequest, o("error", "portal name is required", "code", "portal_name_required"))
				return
			}
			portal := o("id", 8, "slug", slug, "name", name, "home_page_id", nil, "home_page_slug", nil, "page_count", 0,
				"created_at", iso(0), "updated_at", iso(0), "url", a.origin+"/live/portal/"+str(slug))
			a.mu.Lock()
			a.portals.push(portal)
			payload := marshal(o("portal", portal, "password", "pr7k-9mfx-t2qd-w4hn", "password_generated", true))
			a.mu.Unlock()
			sendCredentialBytes(w, r.Request, http.StatusCreated, payload)
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/pages", h: func(w http.ResponseWriter, r *req) {
			id := r.params["id"]
			body := r.bodyOr()
			a.mu.Lock()
			defer a.mu.Unlock()
			page := a.pageBySlug(get(body, "slug"))
			if page == nil {
				sendJSON(w, r.Request, http.StatusNotFound, o("error", "page not found", "code", "page_not_found"))
				return
			}
			// The added member is KEPT, at the sort_order it was sent, and the
			// list is re-sorted. While this echoed a hard sort_order: 0 and
			// persisted nothing, a spec could only assert the outbound request
			// body.
			members, ok := a.membersByPortal[id]
			if !ok {
				members = newList()
				a.membersByPortal[id] = members
			}
			m := o(
				"page_id", get(page, "id"), "slug", get(page, "slug"), "title", get(page, "title"), "label", or(get(body, "label"), nil),
				"display_title", or(get(body, "label"), get(page, "title")), "sort_order", numOr0(get(body, "sort_order")),
				"added_at", iso(0), "has_password", truthy(get(page, "has_password")), "disabled", false, "published", true,
				"page_deleted", false, "shows_switcher", true, "switcher_is_own", false, "render_mode", "themed",
			)
			members.push(m)
			sortMembers(members)
			sendJSON(w, r.Request, http.StatusCreated, o("member", m, "portal_id", num(id), "reclassifies_staff_only", !truthy(get(page, "has_password"))))
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/pages/update", h: func(w http.ResponseWriter, r *req) {
			id := r.params["id"]
			body := r.bodyOr()
			a.mu.Lock()
			defer a.mu.Unlock()
			members := a.membersByPortal[id]
			var m obj
			if members != nil {
				m = members.find(func(c obj) bool { return sameString(get(c, "page_id"), get(body, "page_id")) })
			}
			if m == nil {
				sendJSON(w, r.Request, http.StatusNotFound, o("error", "not in this portal", "code", "portal_page_not_found"))
				return
			}
			// Only sort_order is applied. A label is echoed back but not kept:
			// the spec that presses Enter in the Edit dialog renames Contoso
			// Fabrikam, and a fixture that remembered it would rename the row
			// for every spec that follows.
			if has(body, "sort_order") {
				set(m, "sort_order", numOr0(get(body, "sort_order")))
			}
			sortMembers(members)
			sendJSON(w, r.Request, http.StatusOK, o("member", m, "portal_id", num(id)))
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/pages/remove", h: func(w http.ResponseWriter, r *req) {
			sendJSON(w, r.Request, http.StatusOK, o("portal_id", 7, "removed", true, "home_cleared", false))
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/home", h: func(w http.ResponseWriter, r *req) {
			a.mu.Lock()
			defer a.mu.Unlock()
			sendJSON(w, r.Request, http.StatusOK, o("portal", a.firstPortal()))
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/rename", h: func(w http.ResponseWriter, r *req) {
			a.mu.Lock()
			defer a.mu.Unlock()
			first := asObj(a.firstPortal())
			if first == nil {
				sendError(w, r.Request, http.StatusInternalServerError, "TypeError: Cannot read properties of undefined (reading 'name')")
				return
			}
			set(first, "name", or(get(r.bodyOr(), "name"), get(first, "name")))
			sendJSON(w, r.Request, http.StatusOK, o("portal", first))
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/password", h: func(w http.ResponseWriter, r *req) {
			a.mu.Lock()
			defer a.mu.Unlock()
			sendCredential(w, r.Request, http.StatusOK, o("portal", a.firstPortal(), "password", "zc4m-8kdq-r3vp-h7ns", "password_generated", true))
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/reveal-password", h: func(w http.ResponseWriter, r *req) {
			sendCredential(w, r.Request, http.StatusOK, o("id", num(r.params["id"]), "available", true, "password", "pr7k-9mfx-t2qd-w4hn"))
		}},
		{method: "POST", pattern: "/api/v1/admin/portals/:id/delete", h: func(w http.ResponseWriter, r *req) {
			// Retire the NAMED portal, not the whole table: with a second
			// portal in the fixture, wiping the array made one Retire look like
			// every portal vanishing.
			id := r.params["id"]
			a.mu.Lock()
			defer a.mu.Unlock()
			index, _ := a.findPortal(id)
			if index < 0 {
				sendJSON(w, r.Request, http.StatusNotFound, portalNotFound())
				return
			}
			removed := asObj(a.portals.splice(index))
			count := 0
			if m, ok := a.membersByPortal[id]; ok {
				count = m.len()
			}
			sendJSON(w, r.Request, http.StatusOK, o("portal", o("id", get(removed, "id"), "slug", get(removed, "slug"),
				"name", get(removed, "name"), "deleted", true, "member_count", count)))
		}},
	}
}

// firstPortal is fixturePortals[0] (undefined for an empty list).
func (a *App) firstPortal() any {
	if a.portals.len() == 0 {
		return undef
	}
	return a.portals.at(0)
}

func sendRaw(w http.ResponseWriter, r *req, status int, payload []byte) {
	sendBytes(w, r.Request, status, payload)
}
