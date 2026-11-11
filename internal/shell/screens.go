// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package shell

import (
	"github.com/ElcanoTek/pages/internal/assetver"
	"github.com/ElcanoTek/pages/internal/i18n"
)

// Admin is the per-page review shell (/admin/<slug>). contentOrigin (for
// example https://pages-content.example.com) lets admin.js show the live
// client URL in the password / client-access card.
func Admin(slug, csrfToken, email, contentOrigin string, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)

	return `<!doctype html>
<html lang="` + locale.Language + `">
<head>
` + Head(PageTitle("/"+slug), Bootstrap{
		{"slug", slug}, {"csrf", csrfToken}, {"admin", email}, {"contentOrigin", contentOrigin},
	}, []string{"/shell-assets/page-switcher.js", "/shell-assets/refresh-health.js", "/shell-assets/admin.js"}, locale) + `
</head>
<body>
` + Header(email, locale) + `
<!-- Named because the sandboxed preview renders a whole document inside this one,
     and two unnamed <main> landmarks are indistinguishable in the accessibility
     tree (axe: landmark-unique). -->
<main class="shell detail-shell" aria-label="` + Esc(locale.T("Page review")) + `">
  <div class="context-bar">
    ` + SectionNav(SectionPages, locale) + `
  </div>
  <div class="detail-bar">
    ` + Breadcrumb(slug, locale) + `
    <nav class="page-switcher" id="page-switcher" aria-label="` + Esc(locale.T("Page navigation")) + `" aria-busy="true">
      <button class="icon-action page-switcher__step" id="page-switcher-prev" type="button" aria-label="` + Esc(locale.T("Previous page")) + `" disabled><span aria-hidden="true">←</span></button>
      <label class="sr-only" for="page-switcher-select">` + Esc(locale.T("Switch admin page")) + `</label>
      <select class="page-switcher__select" id="page-switcher-select" aria-describedby="page-switcher-count" disabled>
        <option>` + Esc(locale.T("Loading pages…")) + `</option>
      </select>
      <span class="page-switcher__count" id="page-switcher-count" role="status" aria-live="polite">` + Esc(locale.T("Loading…")) + `</span>
      <button class="icon-action page-switcher__step" id="page-switcher-next" type="button" aria-label="` + Esc(locale.T("Next page")) + `" disabled><span aria-hidden="true">→</span></button>
    </nav>
  </div>
  <div id="app" class="screen-stack">
    ` + Loading(locale.T("Loading /{0}", slug), locale) + `
  </div>
</main>
</body>
</html>`
}

// Templates is the template library shell (/admin/templates): a
// server-rendered bootstrap that hands the browser a CSRF token and the
// content origin, then lets shell-assets/templates.js drive the
// CSRF-protected admin JSON API. No template bytes are rendered here — a
// design is untrusted HTML and previews only on the content host, under the
// sandbox+CSP, through a signed short-TTL URL.
func Templates(csrfToken, email, contentOrigin string, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return `<!doctype html>
<html lang="` + locale.Language + `">
<head>
` + Head(PageTitle(locale.T("Template library")), Bootstrap{
		{"csrf", csrfToken}, {"admin", email}, {"contentOrigin", contentOrigin},
	}, []string{"/shell-assets/templates.js"}, locale) + `
</head>
<body>
` + Header(email, locale) + `
<!-- Named for the same reason as the page detail's: the sandboxed template
     preview renders its own <main> inside this document. -->
<main class="shell detail-shell" aria-label="` + Esc(locale.T("Template library")) + `">
  <div class="context-bar">
    ` + SectionNav(SectionTemplates, locale) + `
  </div>
  <div id="app" class="screen-stack">
    ` + Loading(locale.T("Loading templates"), locale) + `
  </div>
</main>
</body>
</html>`
}

// Portals is the partner portal screen (/admin/portals). This screen is the
// ONLY way a portal is ever changed: the portals package refuses any actor
// that is not a human admin and there is no MCP or bearer equivalent, so
// "who may see which dashboards" is a decision that exists nowhere but here.
func Portals(csrfToken, email, contentOrigin string, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return `<!doctype html>
<html lang="` + locale.Language + `">
<head>
` + Head(PageTitle(locale.T("Partner portals")), Bootstrap{
		{"csrf", csrfToken}, {"admin", email}, {"contentOrigin", contentOrigin},
	}, []string{"/shell-assets/portals.js"}, locale) + `
</head>
<body>
` + Header(email, locale) + `
<main class="shell detail-shell">
  <div class="context-bar">
    ` + SectionNav(SectionPortals, locale) + `
  </div>
  <div id="app" class="screen-stack">
    ` + Loading(locale.T("Loading partner portals"), locale) + `
  </div>
</main>
</body>
</html>`
}

// WelcomeOptions configures the admin landing.
type WelcomeOptions struct {
	// CSRF is the admin CSRF token.
	CSRF string
	// Compose says whether the dev-only compose panel is mounted.
	Compose bool
}

// Welcome is the admin landing (/admin): the workspace index.
func Welcome(email, contentOrigin string, opts WelcomeOptions, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return `<!doctype html>
<html lang="` + locale.Language + `">
<head>
` + Head(PageTitle(locale.T("Client pages")), Bootstrap{
		{"admin", email}, {"contentOrigin", contentOrigin}, {"csrf", opts.CSRF}, {"compose", opts.Compose},
	}, []string{"/shell-assets/refresh-health.js", "/shell-assets/welcome.js"}, locale) + `
</head>
<body>
` + Header(email, locale) + `
<main class="shell">
  <div class="context-bar">
    ` + SectionNav(SectionPages, locale) + `
  </div>
  <div id="app" class="screen-stack">
    ` + Loading(locale.T("Loading page operations"), locale) + `
  </div>
</main>
</body>
</html>`
}

// The dashboard host's 404 and request-error pages are PUBLIC surfaces —
// anyone who mistypes the host reaches them without ever signing in — so they
// use the same standalone chrome as the content host's gates rather than a
// second hand-written copy of it. The only differences are where this host
// serves the Flag files from, and the CTA, which only makes sense here.

// NotFound is the dashboard host's 404 page.
func NotFound(locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	return Standalone(StandaloneOptions{
		Title:      locale.T("Not found · Pages"),
		Kicker:     locale.T("Not found"),
		Heading:    locale.T("Page not found"),
		BodyHTML:   "<p>" + Esc(locale.T("This address is not a Pages route.")) + "</p>",
		AssetsBase: assetver.ShellFlag(),
		CTA:        &CTA{Href: "/admin", Label: locale.T("Back to Pages")},
	}, locale)
}

// RequestError is the dashboard host's page for a failed request: a 4xx
// says the request could not be read, a 5xx asks to retry.
func RequestError(status int, locales ...i18n.Locale) string {
	locale := i18n.First(locales...)
	heading, body := locale.T("Pages could not complete this request"), "<p>"+Esc(locale.T("Please try again shortly."))+"</p>"
	if status < 500 {
		heading, body = locale.T("Request could not be read"), "<p>"+Esc(locale.T("Check the request and try again."))+"</p>"
	}
	return Standalone(StandaloneOptions{
		Title:      locale.T("Request failed · Pages"),
		Kicker:     locale.T("Request failed"),
		Heading:    heading,
		BodyHTML:   body,
		AssetsBase: assetver.ShellFlag(),
		CTA:        &CTA{Href: "/admin", Label: locale.T("Back to Pages")},
	}, locale)
}
