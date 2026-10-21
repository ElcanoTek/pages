// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/contentview"
	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/render"
	"github.com/ElcanoTek/pages/internal/shell"
)

const (
	adminEmail = "qa-admin@elcanotek.com"
	adminCSRF  = "fixture-csrf"
)

func notFoundShell() string { return shell.NotFound() }

func html(status int, body func(r *req) string) handler {
	return func(w http.ResponseWriter, r *req) { sendHTML(w, r.Request, status, body(r)) }
}

func static(status int, body string) handler {
	return html(status, func(*req) string { return body })
}

// shippedTemplate is the shipped campaign template. The dashboard routes
// render it through the REAL render path, with and without a switcher
// payload: an injected #pages-nav block becoming a Page menu a partner can
// click, so a design change that breaks the control fails here rather than in
// front of a partner.
func shippedTemplate() string {
	b, err := fs.ReadFile(pages.Templates(), "nwm-campaign-dashboard/template.html")
	if err != nil {
		panic(err) // embedded at build time
	}
	return string(b)
}

// fixtureNav is the switcher payload. url and home are what let the menu reach
// the portal index and say which page the partner's own index puts first
// (#162). Only the home entry carries a `home` key, exactly as the JavaScript
// literal did.
func fixtureNav(pages *list, truncated bool) obj {
	return o(
		"portal", o("slug", "nwm", "name", "Northwind Media Group", "url", "/live/portal/nwm"),
		"pages", pages,
		"truncated", truncated,
	)
}

func navPages() *list {
	return newList(
		o("slug", "nwm-client-overview", "title", "Portfolio overview", "url", "/live/nwm-client-overview", "current", false, "home", true),
		o("slug", "nwm/contoso-fabrikam", "title", "Contoso — Fabrikam always-on", "url", "/live/nwm/contoso-fabrikam", "current", true),
		o("slug", "nwm-zeta-q3", "title", "Zeta Q3", "url", "/live/nwm-zeta-q3", "current", false),
		// A sibling's title is set by whoever owns THAT page, so one of these
		// is markup. escapedJSON keeps it inside the block; textContent is what
		// keeps it out of the DOM.
		o("slug", "nwm-hostile", "title", "Zeta <img src=x onerror=alert(1)> Q4", "url", "/live/nwm-hostile", "current", false),
	)
}

// navJSON is the payload as the renderer embeds it, in the literal's key
// order (encoding/json passes a RawMessage through as it is).
func navJSON(nav obj) json.RawMessage { return json.RawMessage(marshal(nav)) }

// An OLD dashboard: no switcher code of its own, exactly like every page that
// existed before the feature shipped. Pages must supply the control itself, or
// a partner reaching this page has no way onward but the portal index.
const legacyDashboard = `<!doctype html><html><head><title>Legacy dashboard</title>
<style>body{margin:0;font-family:system-ui}main{padding:28px}table{border-collapse:collapse}td,th{border:1px solid #ccd;padding:6px 10px}</style>
</head><body><main><h1>Q3 delivery</h1><table><tr><th>Deal</th><th>Revenue</th></tr>
<tr><td>Awareness · Display</td><td>$412,300</td></tr><tr><td>Consideration · OLV</td><td>$338,150</td></tr></table></main></body></html>`

// A theme that tries to close the element it is injected into (#189). Themes
// are staff-curated, so this shape only ever arrives here — but "only staff
// write it" is the assumption that stops being true, and a real browser is the
// only thing that can prove the escaping holds.
const hostileTheme = ":root{--pwn:1}\n.a::after{content:\"</style><script>window.__pwned=1</script>\"}"

// A RAW dashboard — its own fonts and colours, no Flag. Most live pages are
// like this, so "raw gets no Page menu" meant most of a partner's set was a
// dead end. It must gain the menu and lose none of its own styling.
const rawDashboard = `<!doctype html><html><head><title>Bespoke raw</title>
<style>:root{--brand:#7a1f3d}body{margin:0;font-family:Georgia,serif;background:#fffaf5;color:#2b1b22}
h1{font-family:Georgia,serif;color:var(--brand);margin:0 0 18px}main{padding:30px}
.kpi{font-size:40px;font-weight:700;color:var(--brand)}</style>
</head><body><main><h1>Harbor Sun · flight to date</h1><div class="kpi">$918,400</div>
<p>Bespoke layout, deliberately not Flag.</p></main></body></html>`

// A dark bespoke report. The light one above is what let a white pill ship:
// every token fallback in the old sheet fired at once on a raw page, and the
// fixture never had a page dark enough to show it (#163).
const rawDarkDashboard = `<!doctype html><html><head><title>Bespoke dark</title>
<style>body{margin:0;font-family:Georgia,serif;background:#101014;color:#f4f1ea}
h1{margin:0 0 18px;color:#e8c37a}main{padding:30px}</style>
</head><body><main><h1>Night desk · flight to date</h1><p>Deliberately dark.</p></main></body></html>`

func renderPage(src, mode string, nav obj, overrideCSS string) string {
	rr := render.Renderable{HTML: src, RenderMode: mode, OverrideCSS: overrideCSS}
	if nav != nil {
		rr.Nav = navJSON(nav)
	}
	return render.RenderVersion(rr)
}

func dashboard(src, mode string, nav func() obj) handler {
	return html(http.StatusOK, func(*req) string {
		var n obj
		if nav != nil {
			n = nav()
		}
		return renderPage(src, mode, n, "")
	})
}

// The partner portal index, in its real states. It is the surface a partner
// actually lands on, so it goes through the same scriptless / branded /
// axe-clean / no-overflow bar as every other public gate.
var (
	fixturePortal = contentview.Portal{Slug: "nwm", Name: "Northwind Media Group"}
	// portalNow is a FIXED "now" that is also handed to the renderer, so
	// "3 days ago" is the same string on every run and in every timezone.
	portalNow = time.UnixMilli(mustTime("2026-08-27T09:00:00Z").UnixMilli())
)

func daysBefore(days float64) time.Time {
	return portalNow.Add(-time.Duration(days * 86400000 * float64(time.Millisecond)))
}

func portalOpenPage() string {
	return contentview.PortalIndexPage(contentview.PortalIndexOptions{
		Portal: fixturePortal,
		Now:    portalNow,
		// Each row carries a different freshness shape, because they render
		// differently: a data envelope, a plain publish, and nothing at all.
		Pages: []contentview.PortalPage{
			{Slug: "nwm-client-overview", Title: "Portfolio overview", IsHome: true, SourceAsOf: daysBefore(0.25), PublishedAt: daysBefore(0.2)},
			{Slug: "nwm/contoso-fabrikam-always-on", Title: "Contoso — Fabrikam always-on performance, quarter to date", SourceAsOf: daysBefore(3), PublishedAt: daysBefore(3)},
			{Slug: "nwm-zeta-q3", Title: "Zeta Q3", PublishedAt: daysBefore(19)},
		},
	})
}

// withCSP returns hs with its Content-Security-Policy replaced by edit(csp).
func withCSP(hs csp.Headers, edit func(string) string) csp.Headers {
	out := make(csp.Headers, len(hs))
	copy(out, hs)
	for i := range out {
		if strings.EqualFold(out[i].Name, "Content-Security-Policy") {
			out[i].Value = edit(out[i].Value)
		}
	}
	return out
}

func withHeaders(hs func() csp.Headers, h handler) handler {
	return func(w http.ResponseWriter, r *req) {
		hs().Apply(w.Header())
		h(w, r)
	}
}

// ── sandbox navigation spike (test/browser/sandbox-nav.spec.js) ─────────────
// The partner page switcher rests on one browser behaviour: can a TOP-LEVEL
// document served with `Content-Security-Policy: sandbox allow-scripts …`
// navigate ITSELF, and does the target's SameSite=Lax cookie ride along when
// the initiator is an opaque origin? These routes use the REAL raw headers and
// the REAL page-cookie code so the answer is about Pages, not about a
// synthetic fixture.
const sandboxPageID = 4242

const sandboxSource = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sandbox source</title></head><body>
<h1>Sandbox source</h1>
<a id="anchor" href="/sandbox-nav/target?via=anchor">Anchor to sibling</a>
<a id="blank" href="/sandbox-nav/target?via=blank" target="_blank" rel="noopener">Anchor in a new tab</a>
<button id="assign" type="button">Assign to sibling</button>
<script>
  document.getElementById("assign").addEventListener("click", function () {
    location.assign("/sandbox-nav/target?via=assign");
  });
</script>
</body></html>`

var nonLowerAlpha = regexp.MustCompile(`[^a-z]`)

func sandboxTarget(r *req) string {
	token, present := httpx.RequestCookies(r.Request)[pagecookie.CookieName(sandboxPageID)]
	verified := present && pagecookie.VerifySession(token, sandboxPageID, "")
	via := ""
	if q := httpx.QueryParam(r.Request, "via"); q.Present {
		via = strings.Join(q.Values, ",")
	}
	yes := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sandbox target</title></head><body>
<h1 id="arrived">arrived</h1>
<p id="via">` + nonLowerAlpha.ReplaceAllString(via, "") + `</p>
<p id="cookie-present">` + yes(token != "") + `</p>
<p id="cookie-verified">` + yes(verified) + `</p>
</body></html>`
}

func (a *App) shellRoutes() []route {
	live := a.origin + "/live"
	return []route{
		{method: "GET", pattern: "/admin", h: static(http.StatusOK, shell.Welcome(adminEmail, live, shell.WelcomeOptions{CSRF: adminCSRF, Compose: true}))},
		// Registered before /admin/*slug, exactly as the server does it —
		// otherwise "templates" is read as a page slug and the library never
		// renders.
		{method: "GET", pattern: "/admin/templates", h: static(http.StatusOK, shell.Templates(adminCSRF, adminEmail, live))},
		{method: "GET", pattern: "/admin/portals", h: static(http.StatusOK, shell.Portals(adminCSRF, adminEmail, live))},
		{method: "GET", pattern: "/admin/*slug", h: html(http.StatusOK, func(r *req) string {
			return shell.Admin(r.params["slug"], adminCSRF, adminEmail, live)
		})},
	}
}

func (a *App) contentRoutes() []route {
	gate := contentview.GatePage
	portalIndex := contentview.PortalIndexPage
	shipped := shippedTemplate()
	full := func() obj { return fixtureNav(navPages(), false) }
	onePage := func() obj { return fixtureNav(newList(navPages().items[1]), false) }
	relocated := replaceFirst(rawDashboard, ":root{--brand:#7a1f3d}", ":root{--brand:#7a1f3d;--pages-nav-top:64px;--pages-nav-right:40px}")

	return []route{
		{method: "GET", pattern: "/preview/:id", h: html(http.StatusOK, func(r *req) string {
			return `<!doctype html><html><head><title>Preview ` + r.params["id"] + `</title></head><body><main><h1>Fixture preview #` + r.params["id"] + `</h1><p>Sandboxed source preview.</p></main></body></html>`
		})},
		{method: "GET", pattern: "/view/*slug", h: html(http.StatusOK, func(r *req) string {
			return `<!doctype html><title>Live fixture</title><h1>Live ` + r.params["slug"] + `</h1>`
		})},
		{method: "GET", pattern: "/live/*slug", h: html(http.StatusOK, func(r *req) string {
			return `<!doctype html><title>Client fixture</title><h1>Client ` + r.params["slug"] + `</h1>`
		})},

		{method: "GET", pattern: "/gate/password", h: static(http.StatusUnauthorized, gate(contentview.GatePageOptions{Slug: "gate/password", ShowForm: true}))},
		{method: "GET", pattern: "/gate/incorrect", h: static(http.StatusUnauthorized, gate(contentview.GatePageOptions{Slug: "gate/incorrect", Message: "Incorrect password.", ShowForm: true}))},
		{method: "GET", pattern: "/gate/staff", h: static(http.StatusForbidden, gate(contentview.GatePageOptions{Slug: "gate/staff"}))},
		{method: "GET", pattern: "/gate/404", h: static(http.StatusNotFound, contentview.NotFoundPage())},
		{method: "GET", pattern: "/gate/rate-limit", h: static(http.StatusTooManyRequests, contentview.RateLimitPage("gate/rate-limit", 15))},
		{method: "GET", pattern: "/gate/busy", h: static(http.StatusTooManyRequests, contentview.BusyPage("gate/busy"))},
		{method: "GET", pattern: "/gate/error", h: static(http.StatusInternalServerError, contentview.ServerErrorPage())},
		{method: "GET", pattern: "/gate/expired", h: static(http.StatusForbidden, contentview.ExpiredLinkPage())},

		{method: "GET", pattern: "/dashboard/with-switcher", h: dashboard(shipped, "themed", full)},
		{method: "GET", pattern: "/dashboard/no-switcher", h: dashboard(shipped, "themed", nil)},
		{method: "GET", pattern: "/dashboard/hostile-theme", h: html(http.StatusOK, func(*req) string {
			return renderPage(legacyDashboard, "themed", nil, hostileTheme)
		})},
		{method: "GET", pattern: "/dashboard/legacy-with-portal", h: dashboard(legacyDashboard, "themed", full)},
		// The truncation row only renders when the payload says the list was
		// cut, which BuildNav does at NavMaxEntries or the byte ceiling —
		// neither reachable with four fixture pages.
		{method: "GET", pattern: "/dashboard/legacy-truncated", h: dashboard(legacyDashboard, "themed", func() obj { return fixtureNav(navPages(), true) })},
		{method: "GET", pattern: "/dashboard/legacy-no-portal", h: dashboard(legacyDashboard, "themed", nil)},
		{method: "GET", pattern: "/dashboard/raw-with-portal", h: dashboard(rawDashboard, "raw", full)},
		{method: "GET", pattern: "/dashboard/raw-no-portal", h: dashboard(rawDashboard, "raw", nil)},
		{method: "GET", pattern: "/dashboard/raw-dark", h: dashboard(rawDarkDashboard, "raw", full)},
		// A design that wants the control somewhere else says so with the two
		// custom properties, rather than by fighting it with CSS it cannot
		// reach.
		{method: "GET", pattern: "/dashboard/raw-relocated", h: dashboard(relocated, "raw", full)},
		// A legacy dashboard in a portal that holds only this one page: there
		// is nowhere to switch to, so the built-in control must not appear.
		{method: "GET", pattern: "/dashboard/legacy-one-page", h: dashboard(legacyDashboard, "themed", onePage)},
		{method: "GET", pattern: "/dashboard/one-page", h: dashboard(shipped, "themed", onePage)},

		{method: "GET", pattern: "/gate/portal", h: static(http.StatusUnauthorized, portalIndex(contentview.PortalIndexOptions{Portal: fixturePortal, ShowForm: true}))},
		{method: "GET", pattern: "/gate/portal-incorrect", h: static(http.StatusUnauthorized, portalIndex(contentview.PortalIndexOptions{Portal: fixturePortal, ShowForm: true, Message: "Incorrect password."}))},
		{method: "GET", pattern: "/gate/portal-open", h: static(http.StatusOK, portalOpenPage())},
		{method: "GET", pattern: "/gate/portal-empty", h: html(http.StatusOK, func(*req) string {
			return portalIndex(contentview.PortalIndexOptions{Portal: fixturePortal})
		})},
		{method: "GET", pattern: "/dashboard-missing", h: static(http.StatusNotFound, shell.NotFound())},

		// Hands out a real page-session cookie, then bounces to the sandboxed
		// source. Secure is off because the fixture is http://127.0.0.1.
		{method: "GET", pattern: "/sandbox-nav/unlock", h: func(w http.ResponseWriter, r *req) {
			w.Header().Set("Set-Cookie", pagecookie.SessionCookieHeader(sandboxPageID, pagecookie.CookieOptions{Insecure: true}))
			httpx.Redirect(w, r.Request, http.StatusFound, "/sandbox-nav/source")
		}},
		// The sandboxed document a partner is looking at. Anchor and button
		// cover the two ways a switcher could navigate: a plain link, and
		// location.assign from a handler.
		{method: "GET", pattern: "/sandbox-nav/source", h: withHeaders(csp.RawHeaders, static(http.StatusOK, sandboxSource))},
		// The sibling dashboard. Reports, in the DOM, whether the real
		// page-session cookie survived the navigation and still verifies.
		{method: "GET", pattern: "/sandbox-nav/target", h: withHeaders(csp.RawHeaders, html(http.StatusOK, sandboxTarget))},
	}
}
