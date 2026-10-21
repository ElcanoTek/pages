// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package shell renders the server-side HTML fragments and documents for the
// Pages-owned screens: the admin shells (/admin, /admin/<slug>,
// /admin/templates, /admin/portals), the dashboard host's error pages, and the
// standalone chrome every gate and error page on either host is drawn in.
//
// The browser behaviour stays in public/shell-assets; these helpers keep the
// Flag header, approved branding, asset order and loading state identical
// across screens. The output is byte-for-byte what lib/*shell.js produced —
// the front end and the browser suite depend on the exact ids, classes and
// data attributes — and golden tests pin it.
package shell

import (
	"html"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	sprite  = "/shell-assets/flag/icons/core-icons.svg"
	mark    = "/shell-assets/flag/logos/elcano-mark-primary.svg"
	favicon = "/shell-assets/flag/logos/elcano-mark-favicon.svg"
)

// Esc escapes the five HTML-significant characters (& < > " ') for text and
// double- or single-quoted attribute values, byte for byte as the
// escape-html package did: html.EscapeString does the escaping (and is the
// sanitizer static analysis recognizes), then its &#34; is spelled &quot;.
func Esc(value string) string {
	out := html.EscapeString(value)
	if strings.Contains(out, "&#34;") {
		out = strings.ReplaceAll(out, "&#34;", "&quot;")
	}
	return out
}

// Field is one key of the bootstrap JSON island. Keys are emitted in slice
// order, which is the order the JS object literal declared them in.
type Field struct {
	Key string
	// Value is a string, bool, int or nil.
	Value any
}

// Bootstrap is the ordered object handed to the browser in
// <script type="application/json" id="pages-bootstrap">.
type Bootstrap []Field

// jsonIsland is JSON.stringify(value) with every "<" escaped, so no string in
// the island can close the <script> element it sits in. It is written by hand
// rather than through encoding/json because the bytes must match V8's
// JSON.stringify exactly: Go escapes U+2028/U+2029 (and, by default, > and &),
// which JS does not.
func jsonIsland(b Bootstrap) string {
	var sb strings.Builder
	sb.WriteByte('{')
	for i, f := range b {
		if i > 0 {
			sb.WriteByte(',')
		}
		writeJSONString(&sb, f.Key)
		sb.WriteByte(':')
		switch v := f.Value.(type) {
		case string:
			writeJSONString(&sb, v)
		case bool:
			sb.WriteString(strconv.FormatBool(v))
		case int:
			sb.WriteString(strconv.Itoa(v))
		default:
			sb.WriteString("null")
		}
	}
	sb.WriteByte('}')
	return strings.ReplaceAll(sb.String(), "<", "\\u003c")
}

// writeJSONString quotes s the way JSON.stringify does: \" \\ \b \f \n \r \t,
// other C0 controls as \u00xx (lowercase hex), everything else verbatim.
// Invalid UTF-8 cannot come from a JS string; it is replaced with U+FFFD.
func writeJSONString(sb *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	sb.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size == 1 {
				sb.WriteString("\uFFFD")
			} else {
				sb.WriteString(s[i : i+size])
			}
			i += size
			continue
		}
		switch c {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\b':
			sb.WriteString(`\b`)
		case '\f':
			sb.WriteString(`\f`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if c < 0x20 {
				sb.WriteString(`\u00`)
				sb.WriteByte(hex[c>>4])
				sb.WriteByte(hex[c&0xf])
			} else {
				sb.WriteByte(c)
			}
		}
		i++
	}
	sb.WriteByte('"')
}

// Head is the shared <head> body: charset, viewport, favicon, title, the Flag
// fonts/tokens and shell.css, the theme controller, the bootstrap island,
// primitives.js, then each of scripts (deferred, in order).
func Head(title string, bootstrap Bootstrap, scripts []string) string {
	tags := make([]string, len(scripts))
	for i, src := range scripts {
		tags[i] = `<script src="` + Esc(src) + `" defer></script>`
	}
	return `<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="` + favicon + `" type="image/svg+xml">
<title>` + Esc(title) + `</title>
<link rel="stylesheet" href="/shell-assets/flag/fonts/fonts.css">
<link rel="stylesheet" href="/shell-assets/flag/tokens/design-tokens.css">
<link rel="stylesheet" href="/shell-assets/shell.css">
<script src="/shell-assets/flag/theme/theme-controller.js"></script>
<script type="application/json" id="pages-bootstrap">` + jsonIsland(bootstrap) + `</script>
<script src="/shell-assets/primitives.js" defer></script>
` + strings.Join(tags, "\n")
}

// Icon is an inline <svg> referencing name in the Flag core icon sprite.
// className "" means the default "icon-inline".
func Icon(name, className string) string {
	if className == "" {
		className = "icon-inline"
	}
	return `<svg class="` + Esc(className) + `" aria-hidden="true"><use href="` + sprite + `#` + Esc(name) + `"></use></svg>`
}

// Header is the Flag app header: brand, the signed-in email, the theme toggle
// and sign-out.
func Header(email string) string {
	return `<header class="page-header">
  <div class="ds-app-header shell-header">
    <a class="ds-app-header__brand brand-link" href="/admin" aria-label="Pages home">
      <img class="ds-app-header__mark" src="` + mark + `" alt="">
      <span class="ds-app-header__text">
        <span class="ds-app-header__title">Pages</span>
      </span>
    </a>
    <div class="ds-app-header__actions">
      <span class="session-user" title="` + Esc(email) + `">` + Esc(email) + `</span>
      <button class="icon-action theme-toggle" type="button" data-flag-theme-toggle aria-label="Switch theme" aria-pressed="false" title="Change color theme">
        ` + Icon("sun", "icon-inline theme-toggle__icon theme-toggle__icon--sun") + `
        ` + Icon("moon", "icon-inline theme-toggle__icon theme-toggle__icon--moon") + `
      </button>
      <!-- The label is hidden on narrow viewports, so the accessible name has to
           come from aria-label or the link becomes an unnamed icon. -->
      <a class="btn btn-ghost header-signout" href="/logout" aria-label="Sign out">` + Icon("logout", "") + `<span>Sign out</span></a>
    </div>
  </div>
</header>`
}

// PageTitle is every admin screen's document title, "<screen> · Pages", so a
// row of browser tabs is scannable by its first word and the product name
// never leads. The screen name is the same string its section tab and its
// <h1> use.
func PageTitle(screen string) string { return screen + " · Pages" }

// Section keys for SectionNav.
const (
	SectionPages     = "pages"
	SectionTemplates = "templates"
	SectionPortals   = "portals"
)

// Each tab names its destination the way that page's own <h1> names itself —
// a tab whose label is echoed by an overline above a different heading reads
// as a second header rather than as navigation.
var sections = []struct{ key, href, label string }{
	{SectionPages, "/admin", "Client pages"},
	{SectionTemplates, "/admin/templates", "Template library"},
	{SectionPortals, "/admin/portals", "Partner portals"},
}

// SectionNav is the top-level section navigation, shared so every section
// renders identical markup; current is marked aria-current="page".
func SectionNav(current string) string {
	tabs := make([]string, len(sections))
	for i, s := range sections {
		active := ""
		if s.key == current {
			active = ` aria-current="page"`
		}
		tabs[i] = `    <a class="context-tab" href="` + s.href + `"` + active + `>` + Esc(s.label) + `</a>`
	}
	return `<nav class="context-nav" aria-label="Pages sections">
` + strings.Join(tabs, "\n") + `
  </nav>`
}

// Breadcrumb is the detail screen's second row. The trail is server-rendered
// from the slug so there is something correct before the payload lands;
// admin.js then upgrades the workspace and the human title in place. Marking
// the last crumb aria-current makes the trail a location, not a set of links.
func Breadcrumb(slug string) string {
	return `<nav class="breadcrumb" aria-label="Breadcrumb">
    <ol class="breadcrumb__list">
      <li class="breadcrumb__item"><a href="/admin">Client pages</a></li>
      <li class="breadcrumb__item breadcrumb__item--workspace" id="breadcrumb-workspace" hidden></li>
      <li class="breadcrumb__item"><span id="breadcrumb-current" aria-current="page">/` + Esc(slug) + `</span></li>
    </ol>
  </nav>`
}

// Loading is the loading state panel; label "" means "Loading pages".
func Loading(label string) string {
	if label == "" {
		label = "Loading pages"
	}
	return `<div class="state-panel state-panel--loading" role="status" aria-live="polite">
  <span class="spinner spinner--panel" aria-hidden="true"></span>
  <p>` + Esc(label) + `…</p>
</div>`
}
