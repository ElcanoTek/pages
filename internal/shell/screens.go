// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package shell

// Admin is the per-page review shell (/admin/<slug>). contentOrigin (for
// example https://pages-content.example.com) lets admin.js show the live
// client URL in the password / client-access card.
func Admin(slug, csrfToken, email, contentOrigin string) string {
	s := Esc(slug)
	return `<!doctype html>
<html lang="en">
<head>
` + Head(PageTitle("/"+slug), Bootstrap{
		{"slug", slug}, {"csrf", csrfToken}, {"admin", email}, {"contentOrigin", contentOrigin},
	}, []string{"/shell-assets/page-switcher.js", "/shell-assets/admin.js"}) + `
</head>
<body>
` + Header(email) + `
<!-- Named because the sandboxed preview renders a whole document inside this one,
     and two unnamed <main> landmarks are indistinguishable in the accessibility
     tree (axe: landmark-unique). -->
<main class="shell detail-shell" aria-label="Page review">
  <div class="context-bar">
    ` + SectionNav(SectionPages) + `
  </div>
  <div class="detail-bar">
    ` + Breadcrumb(slug) + `
    <nav class="page-switcher" id="page-switcher" aria-label="Page navigation" aria-busy="true">
      <button class="icon-action page-switcher__step" id="page-switcher-prev" type="button" aria-label="Previous page" disabled><span aria-hidden="true">←</span></button>
      <label class="sr-only" for="page-switcher-select">Switch admin page</label>
      <select class="page-switcher__select" id="page-switcher-select" aria-describedby="page-switcher-count" disabled>
        <option>Loading pages…</option>
      </select>
      <span class="page-switcher__count" id="page-switcher-count" role="status" aria-live="polite">Loading…</span>
      <button class="icon-action page-switcher__step" id="page-switcher-next" type="button" aria-label="Next page" disabled><span aria-hidden="true">→</span></button>
    </nav>
  </div>
  <div id="app" class="screen-stack">
    ` + Loading("Loading /"+s) + `
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
func Templates(csrfToken, email, contentOrigin string) string {
	return `<!doctype html>
<html lang="en">
<head>
` + Head(PageTitle("Template library"), Bootstrap{
		{"csrf", csrfToken}, {"admin", email}, {"contentOrigin", contentOrigin},
	}, []string{"/shell-assets/templates.js"}) + `
</head>
<body>
` + Header(email) + `
<!-- Named for the same reason as the page detail's: the sandboxed template
     preview renders its own <main> inside this document. -->
<main class="shell detail-shell" aria-label="Template library">
  <div class="context-bar">
    ` + SectionNav(SectionTemplates) + `
  </div>
  <div id="app" class="screen-stack">
    ` + Loading("Loading templates") + `
  </div>
</main>
</body>
</html>`
}

// Portals is the partner portal screen (/admin/portals). This screen is the
// ONLY way a portal is ever changed: the portals package refuses any actor
// that is not a human admin and there is no MCP or bearer equivalent, so
// "who may see which dashboards" is a decision that exists nowhere but here.
func Portals(csrfToken, email, contentOrigin string) string {
	return `<!doctype html>
<html lang="en">
<head>
` + Head(PageTitle("Partner portals"), Bootstrap{
		{"csrf", csrfToken}, {"admin", email}, {"contentOrigin", contentOrigin},
	}, []string{"/shell-assets/portals.js"}) + `
</head>
<body>
` + Header(email) + `
<main class="shell detail-shell">
  <div class="context-bar">
    ` + SectionNav(SectionPortals) + `
  </div>
  <div id="app" class="screen-stack">
    ` + Loading("Loading partner portals") + `
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
func Welcome(email, contentOrigin string, opts WelcomeOptions) string {
	return `<!doctype html>
<html lang="en">
<head>
` + Head(PageTitle("Client pages"), Bootstrap{
		{"admin", email}, {"contentOrigin", contentOrigin}, {"csrf", opts.CSRF}, {"compose", opts.Compose},
	}, []string{"/shell-assets/welcome.js"}) + `
</head>
<body>
` + Header(email) + `
<main class="shell">
  <div class="context-bar">
    ` + SectionNav(SectionPages) + `
  </div>
  <div id="app" class="screen-stack">
    ` + Loading("Loading page operations") + `
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
func NotFound() string {
	return Standalone(StandaloneOptions{
		Title:      "Not found · Pages",
		Kicker:     "Not found",
		Heading:    "Page not found",
		BodyHTML:   "<p>This address is not a Pages route.</p>",
		AssetsBase: "/shell-assets/flag",
		CTA:        &CTA{Href: "/admin", Label: "Back to Pages"},
	})
}

// RequestError is the dashboard host's page for a failed request: a 4xx
// says the request could not be read, a 5xx asks to retry.
func RequestError(status int) string {
	heading, body := "Pages could not complete this request", "<p>Please try again shortly.</p>"
	if status < 500 {
		heading, body = "Request could not be read", "<p>Check the request and try again.</p>"
	}
	return Standalone(StandaloneOptions{
		Title:      "Request failed · Pages",
		Kicker:     "Request failed",
		Heading:    heading,
		BodyHTML:   body,
		AssetsBase: "/shell-assets/flag",
		CTA:        &CTA{Href: "/admin", Label: "Back to Pages"},
	})
}
