// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package contentview serves the live client page DIRECTLY on the content host
// (PLAN §6b, direct-serve decision): <content-host>/<slug> is the client URL.
//
// Gate (the content host owns it; the SSO cookie never reaches here):
//   - disabled / unknown / nothing-published → 404
//   - ?t=<broker token> (minted by the dashboard /view after SSO, for staff-only
//     pages) → set a short page-session cookie, redirect to the clean URL
//   - valid page-session cookie (or portal session for a member) → render the
//     published version (sandbox CSP)
//   - password page, no session → the password form (trusted, NOT sandboxed)
//   - staff-only page (no password), no session → 403 "open it from the dashboard"
//
// Two response flavours: the rendered agent page gets the `sandbox` CSP
// (csp.RawHeaders), the gate/form gets the trusted csp.GateHeaders (so it can
// submit). The package also owns the rest of the content host: /raw, the
// /assets mount and its error pages — see ContentHandler.
package contentview

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/assetver"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/i18n"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagecookie"
	"github.com/ElcanoTek/pages/internal/shell"
)

// FlagBase is the stable, unversioned path where the content host serves the
// vendored Flag files (same origin, so a gate page can use them under its
// CSP). Our own chrome links assetver.ContentFlag(), the versioned and
// immutable-cached form of the same files.
const FlagBase = "/assets/flag"

const defaultPasswordWindowMinutes = 15

// passwordWindowMinutes is the same knob internal/ratelimit reads, so the page
// cannot promise a wait the limiter does not enforce. It said "a few minutes"
// for a fifteen-minute window.
func passwordWindowMinutes() int {
	if m := env.Int("RL_PASSWORD_WINDOW_MIN", defaultPasswordWindowMinutes); m > 0 {
		return m
	}
	return defaultPasswordWindowMinutes
}

func esc(s string) string { return shell.Esc(s) }

// chrome — the trusted, Flag-themed shell for every content-host gate page
// (password prompt, staff-only notice, 404). Scriptless by design: it runs
// under the gate/raw CSP (default-src 'none', NO script-src). The agent's page
// content is NEVER rendered here — this is our chrome only (invariant #2).
func chrome(title, kicker, heading, body string, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return shell.Standalone(shell.StandaloneOptions{
		Title: title, Kicker: kicker, Heading: heading, BodyHTML: body, AssetsBase: assetver.ContentFlag(),
	}, locale)
}

// GatePageOptions describe a password prompt or staff-only notice.
type GatePageOptions struct {
	Slug     string
	Message  string // "" for none
	ShowForm bool
	// Credential says which secret opens this page, which changes only the
	// wording: "page" (its own client password; the default when ""),
	// "portal" (no password of its own, but in a portal) or "either" (both).
	// The copy never names a portal: that a page belongs to *some* portal is
	// already implied by offering the prompt at all; which one is not.
	Credential string
}

var (
	gatePrompt = map[string]string{
		"page":   i18n.Source("Enter the client password to continue to this page."),
		"portal": i18n.Source("Enter your portal password to continue to this dashboard."),
		"either": i18n.Source("Enter the password for this page, or your portal password, to continue."),
	}
	gateHelp = map[string]string{
		"page":   i18n.Source("Use the password supplied with this page link."),
		"portal": i18n.Source("Use the password from your portal link — the same one opens every dashboard in your portal."),
		"either": i18n.Source("Use the password supplied with this page link, or the one from your portal link."),
	}
	gateLabel = map[string]string{"portal": i18n.Source("Portal password"), "page": i18n.Source("Page password"), "either": i18n.Source("Password")}
)

// GatePage is the password prompt or staff-only notice, in the shared chrome.
func GatePage(o GatePageOptions, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	cred := o.Credential
	if cred == "" {
		cred = "page"
	}
	msg := ""
	if o.Message != "" {
		msg = `<div class="alert" id="password-error" role="alert"><strong>` + esc(locale.T(o.Message)) +
			`</strong><span>` + esc(locale.T("Check the password and try again. Each wrong attempt adds a short delay before the next one.")) + `</span></div>`
	}
	var body string
	if o.ShowForm {
		described := "password-help"
		if o.Message != "" {
			described += " password-error"
		}
		body = `<p class="sub">` + esc(locale.T(gatePrompt[cred])) + `</p>` + msg + `
       <form method="post" action="` + esc(locale.URL("/"+o.Slug)) + `">
         <input type="text" name="account" value="` + esc(o.Slug) + `" autocomplete="username" readonly tabindex="-1" aria-hidden="true" class="credential-account">
         <div class="field">
           <label for="page-password">` + esc(locale.T(gateLabel[cred])) + `</label>
           <input id="page-password" type="password" name="password" autofocus autocomplete="current-password" required aria-describedby="` + described + `">
           <span class="help" id="password-help">` + esc(locale.T(gateHelp[cred])) + `</span>
         </div>
         <button type="submit">` + esc(locale.T("View page")) + `</button>
       </form>`
	} else {
		body = `<p class="sub">` + esc(locale.T("The person who sent you this link needs to turn on access before it will open. Ask them to share it again.")) + `</p>` +
			msg + `<p class="guidance">` + esc(locale.T("Elcano staff:")) + ` <a href="` + esc(env.DashboardOrigin()+locale.URL("/view/"+o.Slug)) +
			`">` + esc(locale.T("open it from the Pages dashboard")) + `</a>.</p>`
	}
	kicker, heading := i18n.Source("Restricted"), i18n.Source("This page hasn't been shared yet")
	if o.ShowForm {
		// The heading names what the READER can do about it. "Staff-only page"
		// is true and useless to the partner who followed a link shared before
		// the page was passworded — which is who actually lands here.
		kicker, heading = i18n.Source("Protected"), i18n.Source("This page is protected")
	}
	return chrome(locale.T("Protected page · Elcano Pages"), locale.T(kicker), locale.T(heading), body, locale)
}

// ── partner-facing timestamps ───────────────────────────────────────────────
// These pages carry no script, so the formatting happens here. The convention
// is the admin's formatWhen (public/shell-assets/primitives.js): relative while
// a person would still count it in days, absolute once they would not, and
// seconds never. Relative is computed from ELAPSED time, not calendar days,
// which is what makes it safe to render server-side for a reader whose
// timezone we do not know: "3 days ago" is true in every timezone, "today"
// would not be.

const relativeLimit = 7 * 24 * time.Hour

// en-GB short months as current ICU writes them ("Sept", not "Sep").
var gbMonths = [...]string{i18n.Source("Jan"), i18n.Source("Feb"), i18n.Source("Mar"), i18n.Source("Apr"), i18n.Source("May"), i18n.Source("Jun"), i18n.Source("Jul"), i18n.Source("Aug"), i18n.Source("Sept"), i18n.Source("Oct"), i18n.Source("Nov"), i18n.Source("Dec")}

func jsRound(x float64) int64 { return int64(math.Floor(x + 0.5)) }

func relative(n int64, unit string, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	if unit == "day" && n == 1 {
		return locale.T("yesterday")
	}
	switch unit {
	case "minute":
		if n == 1 {
			return locale.T("1 minute ago")
		}
		return locale.T("{0} minutes ago", n)
	case "hour":
		if n == 1 {
			return locale.T("1 hour ago")
		}
		return locale.T("{0} hours ago", n)
	default:
		return locale.T("{0} days ago", n)
	}
}

// WhenPhrase is the partner-facing phrase for t as seen at now: "just now",
// "5 minutes ago", "yesterday", "3 days ago", or — past a week, or in the
// future (clock skew, not a countdown) — a date like "18 Aug 2026" in the
// server's local zone. The zero time yields "".
func WhenPhrase(t, now time.Time, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	if t.IsZero() {
		return ""
	}
	t = t.Truncate(time.Millisecond)
	elapsed := now.Sub(t)
	if elapsed < 0 || elapsed >= relativeLimit {
		l := t.Local()
		return strconv.Itoa(l.Day()) + " " + locale.T(gbMonths[l.Month()-1]) + " " + strconv.Itoa(l.Year())
	}
	ms := float64(elapsed.Milliseconds())
	switch {
	case elapsed < time.Minute:
		return locale.T("just now")
	case elapsed < time.Hour:
		return relative(jsRound(ms/60000), "minute", locale)
	case elapsed < 24*time.Hour:
		return relative(jsRound(ms/3600000), "hour", locale)
	default:
		return relative(jsRound(ms/86400000), "day", locale)
	}
}

// freshnessHTML — the one line under a dashboard's title (#176). The two
// timestamps are NOT interchangeable and are not coalesced into one phrase:
// SourceAsOf is when the DATA is from, which is what a partner is actually
// asking; PublishedAt is only when we last published something. "Updated
// today" about a page republished today from a three-week-old extract is
// precisely the false reassurance this is meant to remove.
func freshnessHTML(e PortalPage, now time.Time, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	stamp, label := e.SourceAsOf, i18n.Source("Data as of {0}")
	if stamp.IsZero() {
		stamp, label = e.PublishedAt, i18n.Source("Updated {0}")
	}
	phrase := WhenPhrase(stamp, now, locale)
	if phrase == "" {
		return ""
	}
	iso := jsx.FormatTime(stamp)
	return `<span class="portal-item__when"><time datetime="` + esc(iso) + `" title="` + esc(iso) + `">` +
		esc(locale.T(label, phrase)) + `</time></span>`
}

// PortalIndexOptions describe the partner's portal landing page.
type PortalIndexOptions struct {
	Portal   Portal
	Pages    []PortalPage
	ShowForm bool
	Message  string
	Now      time.Time // zero = time.Now()
}

// PortalIndexPage — the partner's landing page: one credential, then the set
// of dashboards that credential opens. Deliberately OUR page and not agent
// HTML, so it is served under the gate headers: scriptless, Flag-themed, and
// NOT sandboxed — its links are ordinary links. It is also the surface that
// always works: the in-page switcher depends on a template rendering a block,
// this does not.
func PortalIndexPage(o PortalIndexOptions, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	now := o.Now
	if now.IsZero() {
		now = time.Now()
	}
	portal := o.Portal
	action := esc(locale.URL("/portal/" + portal.Slug))
	msg := ""
	if o.Message != "" {
		msg = `<div class="alert" id="portal-error" role="alert"><strong>` + esc(locale.T(o.Message)) +
			`</strong><span>` + esc(locale.T("Check the password and try again. Each wrong attempt adds a short delay before the next one.")) + `</span></div>`
	}
	var body string
	switch {
	case o.ShowForm:
		described := "portal-help"
		if o.Message != "" {
			described += " portal-error"
		}
		body = `<p class="sub">` + esc(locale.T("Enter your portal password to open your dashboards.")) + `</p>` + msg + `
       <form method="post" action="` + action + `">
         <input class="credential-account" type="text" name="account" value="` + esc(portal.Slug) + `" tabindex="-1" aria-hidden="true" autocomplete="username" readonly>
         <div class="field">
           <label for="portal-password">` + esc(locale.T("Portal password")) + `</label>
           <input id="portal-password" type="password" name="password" autofocus autocomplete="current-password" required aria-describedby="` + described + `">
           <span class="help" id="portal-help">` + esc(locale.T("Use the password supplied with this portal link. One password opens every dashboard listed here.")) + `</span>
         </div>
         <button type="submit">` + esc(locale.T("Open portal")) + `</button>
       </form>`
	case len(o.Pages) > 0:
		// Root-absolute hrefs, never relative: slugs nest, so on /a/b/c a
		// relative href resolves against /a/b/ — and `base-uri 'none'` means a
		// <base> tag cannot rescue it. The freshness line sits INSIDE the
		// anchor: "Portfolio overview, data as of 3 days ago" is the reason
		// someone picks one link over another, so it earns its place in the name.
		item := func(e PortalPage) string {
			return `<li class="portal-item"><a href="` + esc(locale.URL("/"+e.Slug)) + `">` +
				`<span class="portal-item__title">` + esc(e.Title) + `</span>` +
				freshnessHTML(e, now, locale) + `</a></li>`
		}
		join := func(list []PortalPage) string {
			var b strings.Builder
			for _, e := range list {
				b.WriteString(item(e))
			}
			return b.String()
		}
		var home, rest []PortalPage
		for _, e := range o.Pages {
			if e.IsHome {
				home = append(home, e)
			} else {
				rest = append(rest, e)
			}
		}
		// Two sections only when there is something to separate: headings over
		// a list of one are hierarchy for its own sake.
		var list string
		if len(home) > 0 && len(rest) > 0 {
			list = `<h2 class="portal-section">` + esc(locale.T("Start here")) + `</h2>` +
				`<ul class="portal-list portal-list--lead">` + join(home) + `</ul>` +
				`<h2 class="portal-section">` + esc(locale.T("All dashboards")) + `</h2>` +
				`<ul class="portal-list">` + join(rest) + `</ul>`
		} else {
			list = `<ul class="portal-list">` + join(o.Pages) + `</ul>`
		}
		summary := locale.T("{0} dashboards available to you.", len(o.Pages))
		if len(o.Pages) == 1 {
			summary = locale.T("1 dashboard available to you.")
		}
		body = `<p class="sub">` + esc(summary) + `</p>` + list + sessionFooter(portal, locale)
	default:
		// A real state, not an error: a portal can exist before anything is
		// added to it, and a member that is taken down or unpublished is
		// filtered out rather than listed as a dead link.
		body = `<p class="sub">` + esc(locale.T("No dashboards are available in this portal yet.")) + `</p>` +
			`<p class="guidance">` + esc(locale.T("Nothing is wrong with your password — ask your Elcano contact to add your dashboards to this portal.")) + `</p>` +
			sessionFooter(portal, locale)
	}
	return chrome(portal.Name+" · Elcano Pages", locale.T("Partner portal"), portal.Name, body, locale)
}

// sessionFooter — how long this browser stays open, and how to close it. The
// lifetime is read from pagecookie rather than restated, and the button is a
// plain form because this host runs no script.
func sessionFooter(p Portal, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return `<div class="portal-footer">` +
		`<p class="guidance">` + esc(locale.T("Each dashboard opens without another password for {0} days on this device.", pagecookie.DefaultTTLDays)) + `</p>` +
		`<form class="portal-signout" method="post" action="` + esc(locale.URL("/portal/"+p.Slug+"/lock")) + `">` +
		`<button type="submit">` + esc(locale.T("Sign out")) + `</button>` +
		`</form>` +
		`</div>`
}

// NotFoundPage is the content host's styled 404.
func NotFoundPage(locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return chrome(locale.T("Not found · Elcano Pages"), locale.T("Not found"), locale.T("Page not found"),
		`<p class="sub">`+esc(locale.T("This address may be wrong, or the page may have been taken down. Check the link with the person who shared it."))+`</p>`, locale)
}

// ExpiredLinkPage — a preview link is signed and short-lived, so this is the
// state a reviewer reaches most often; it gets a branded interstitial that
// says what to do next rather than a page that looks like a server fault.
func ExpiredLinkPage(locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return chrome(locale.T("Link expired · Elcano Pages"), locale.T("Link expired"), locale.T("This preview link is no longer valid"),
		`<p class="sub">`+esc(locale.T("Preview links expire after a short time, so they cannot be shared or bookmarked."))+`</p>`+
			`<p class="guidance">`+esc(locale.T("Ask for a fresh link — generating one takes a second and the design has not changed."))+`</p>`, locale)
}

// RateLimitPage is the password brute-force guard's 429 body. slug ("" for
// none) adds a way back to the page; minutes <= 0 means the configured
// RL_PASSWORD_WINDOW_MIN (default 15).
func RateLimitPage(slug string, minutes int, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	if minutes <= 0 {
		minutes = passwordWindowMinutes()
	}
	back := ""
	if slug != "" {
		back = `<p class="guidance"><a href="` + esc(locale.URL("/"+slug)) + `">` + esc(locale.T("Return to the page")) + `</a> ` + esc(locale.T("when the wait is over.")) + `</p>`
	}
	wait := locale.T("Too many passwords have been tried for this page. Wait about {0} minutes, then try again.", minutes)
	if minutes == 1 {
		wait = locale.T("Too many passwords have been tried for this page. Wait about 1 minute, then try again.")
	}
	return chrome(locale.T("Too many attempts · Elcano Pages"), locale.T("Access paused"), locale.T("Too many password attempts"),
		`<p class="sub">`+esc(wait)+`</p>`+
			`<p class="guidance">`+esc(locale.T("If access is urgent, ask the person who shared this page to confirm the password."))+`</p>`+back, locale)
}

// AttemptBusyPage is the 429 body for a password attempt refused because
// other attempts on the same page or portal are still being checked (the
// per-credential turn, passwordgate.Turns). slug "" omits the return link.
func AttemptBusyPage(slug string, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	back := ""
	if slug != "" {
		back = `<p class="guidance"><a href="` + esc(locale.URL("/"+slug)) + `">` + esc(locale.T("Return to the page")) + `</a> ` + esc(locale.T("and try again.")) + `</p>`
	}
	return chrome(locale.T("Too many attempts · Elcano Pages"), locale.T("Access paused"), locale.T("Too many password attempts"),
		`<p class="sub">`+esc(locale.T("Other password attempts for this page are being checked. Wait a few seconds, then try again."))+`</p>`+
			`<p class="guidance">`+esc(locale.T("If access is urgent, ask the person who shared this page to confirm the password."))+`</p>`+back, locale)
}

// BusyPage is the READ limiter's 429 body — a different event from the
// password guard: nothing is wrong with the viewer's credentials, the host is
// shedding load. slug "" omits the reload link.
func BusyPage(slug string, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	reload := ""
	if slug != "" {
		reload = `<p class="guidance"><a href="` + esc(locale.URL("/"+slug)) + `">` + esc(locale.T("Reload the page")) + `</a></p>`
	}
	return chrome(locale.T("Too many requests · Elcano Pages"), locale.T("Slow down"), locale.T("This page is being loaded too often"),
		`<p class="sub">`+esc(locale.T("The page is fine — requests from your network are arriving faster than our servers can answer them. Wait a moment and reload."))+`</p>`+
			`<p class="guidance">`+esc(locale.T("If this keeps happening, tell the person who shared the page; it usually means something is reloading it automatically."))+`</p>`+reload, locale)
}

// ServerErrorPage — a fault on our side, said plainly rather than implying the
// viewer's link is wrong (which a 404 would) or leaking a default error page.
func ServerErrorPage(locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return chrome(locale.T("Something went wrong · Elcano Pages"), locale.T("Error"), locale.T("This page couldn't be loaded"),
		`<p class="sub">`+esc(locale.T("Something went wrong on our side, not with your link. Reloading in a moment usually works."))+`</p>`+
			`<p class="guidance">`+esc(locale.T("If it keeps failing, tell the person who shared this page so an Elcano admin can look."))+`</p>`, locale)
}

// ── page switcher ───────────────────────────────────────────────────────────

// The switcher payload ships on every render of every member page, so it is
// bounded on all three axes an unbounded one would grow along: entries, text
// length, and total bytes. Truncated is in the payload rather than silent, so a
// template can say "and N more" instead of quietly showing a partial set.
const (
	NavMaxEntries = 50
	NavMaxText    = 200
	NavMaxBytes   = 16 * 1024
)

// Nav is the page-switcher payload, in the wire's key order.
type Nav struct {
	// Portal: the index is the one surface that always works for a partner,
	// so the menu carries its URL (a template must never build an href).
	Portal    NavPortal `json:"portal"`
	Pages     []NavPage `json:"pages"`
	Truncated bool      `json:"truncated"`
}

// NavPortal names the authorising portal.
type NavPortal struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

// NavPage is one sibling dashboard. URL is ready-made and absolute: slugs
// nest, so on /a/b/c a relative "d" resolves to /a/b/d, and `base-uri 'none'`
// means a <base> tag cannot correct it.
type NavPage struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Current bool   `json:"current"`
	// Home: the partner's index puts the home page first whatever the sort
	// order, so the menu has to be able to say which one it is.
	Home bool `json:"home"`
}

func clip(s string) string { return jsstr.Prefix(s, NavMaxText) }

// jsonLen is JSON.stringify(v).length: UTF-16 units, with U+2028/2029 raw
// (encoding/json escapes them; JavaScript does not).
func jsonLen(v any) int {
	b, err := httpx.Marshal(v)
	if err != nil {
		return 0
	}
	s := strings.NewReplacer(` `, " ", ` `, " ").Replace(string(b))
	return jsstr.UTF16Len(s)
}

// BuildNav is the switcher payload. Pure. The list is a function of the
// AUTHORISING portal, never of "every portal containing this page": a page can
// sit in two partners' portals, and showing the union would hand each of them
// the other's dashboard titles. Holding a valid cookie for that portal is what
// proves entitlement to its list.
func BuildNav(portal Portal, rows []PortalPage, currentSlug string) Nav {
	origin := env.ContentOrigin()
	nav := Nav{
		Portal: NavPortal{Slug: clip(portal.Slug), Name: clip(portal.Name), URL: origin + "/portal/" + portal.Slug},
		Pages:  []NavPage{},
	}
	for i, row := range rows {
		if i == NavMaxEntries {
			break
		}
		title := row.Title
		if title == "" {
			title = row.Slug
		}
		nav.Pages = append(nav.Pages, NavPage{
			Slug: clip(row.Slug), Title: clip(title), URL: origin + "/" + row.Slug,
			Current: row.Slug == currentSlug, Home: row.IsHome,
		})
	}
	nav.Truncated = len(rows) > NavMaxEntries
	// Byte ceiling last, since a title is bounded but a hundred of them are not.
	for len(nav.Pages) > 1 && jsonLen(nav) > NavMaxBytes {
		nav.Pages = nav.Pages[:len(nav.Pages)-1]
		nav.Truncated = true
	}
	return nav
}

// AuthorizingPortal — which portal, if any, this request has already
// unlocked. The portals arrive ordered by id (GetPortalsForPage) and the FIRST
// match wins, so a viewer holding cookies for two portals that both contain
// this page gets the same answer on every request: the sibling list a page
// shows is scoped to whichever portal authorised it, and must not flip between
// two of them from one load to the next. Pure; it trusts its input order.
func AuthorizingPortal(cookies map[string]string, portals []Portal) *Portal {
	for i := range portals {
		p := &portals[i]
		if tok, ok := cookies[pagecookie.PortalCookieName(p.ID)]; ok &&
			pagecookie.VerifyPortalSession(tok, p.ID, p.PasswordHash) {
			return p
		}
	}
	return nil
}
