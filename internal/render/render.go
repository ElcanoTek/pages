// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package render turns a stored version into the bytes served at /raw.
//
//	render_mode "raw"    → the agent's HTML, verbatim (the "bespoke" path).
//	render_mode "themed" → inject the Flag design system (tokens + fonts +
//	                       theme controller) and the page's client theme
//	                       override into <head>, WITHOUT touching the rest of
//	                       the document. Injected nodes are tagged
//	                       [data-flag-injected] so the source editor can strip
//	                       them — we persist source, never the rendered frame.
//
// We inject by string-splicing after the opening <head> (or synthesizing one).
// This is deliberately NOT a full parse/serialize: we never rewrite the agent's
// markup, so chart/script structure is preserved byte-for-byte.
//
// The package also holds what Pages knows about a Bento deck's document (see
// bento.go).
package render

import (
	"fmt"
	"html"
	"reflect"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/assetver"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// flagAssetsBase is where the injected Flag tags point: by default the content
// host's versioned, immutable-cached base (internal/assetver), or
// FLAG_ASSETS_BASE when an operator serves a copy elsewhere on the content
// host. Read at call time so tests can set FLAG_ASSETS_BASE. An invalid value
// (ValidateAssetsBase refuses it at startup) falls back to the default rather
// than emitting tags the content CSP would block.
func flagAssetsBase() string {
	if base := env.String("FLAG_ASSETS_BASE", ""); base != "" && sameOriginPath(base) {
		return base
	}
	return assetver.ContentFlag()
}

// assetsBase is flagAssetsBase, swappable so a test can hand flagHead a base
// that skipped validation and prove the escaping alone keeps it inert.
var assetsBase = flagAssetsBase

// assetsBaseRE is the whole allowed shape of FLAG_ASSETS_BASE: an absolute
// path made of unreserved URL characters and slashes. Nothing that could
// close an attribute or start markup (quotes, angle brackets, spaces), no
// query or fragment, no percent-escapes, no backslash.
var assetsBaseRE = regexp.MustCompile(`^/[A-Za-z0-9._~/-]*$`)

// sameOriginPath reports whether base is a plain path on the serving origin:
// it matches assetsBaseRE, is not host-relative ("//host", which browsers read
// as another origin) and has no "." or ".." segment. The content host's CSP
// allows only 'self' for styles, fonts and scripts, so any other origin would
// be blocked; widening that CSP is not the answer.
func sameOriginPath(base string) bool {
	if !assetsBaseRE.MatchString(base) || strings.HasPrefix(base, "//") {
		return false
	}
	for _, seg := range strings.Split(base, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ValidateAssetsBase is the startup check for FLAG_ASSETS_BASE: unset, or a
// same-origin absolute path such as /assets/flag-copy.
func ValidateAssetsBase() error {
	base := env.String("FLAG_ASSETS_BASE", "")
	if base == "" || sameOriginPath(base) {
		return nil
	}
	return fmt.Errorf("FLAG_ASSETS_BASE must be a path on the content host such as /assets/flag-copy (got %q): the content CSP allows only 'self', so another origin would be blocked", base)
}

// NavBlockID is the id Pages owns. A page switcher needs the CURRENT membership
// of whichever portal authorised this request, which is not knowable at deploy
// time and must not require re-deploying every page when it changes — so the
// list is injected here, as one more head tag, rather than substituted into a
// block the author declares. The author declares nothing at all: a template
// that wants a switcher reads this id, and a template that does not gets the
// BUILT-IN one.
//
// That fallback is not a nicety. Shipping only the payload meant a design still
// had to opt in with code, so the switcher worked on pages authored after the
// feature and on nothing else — which, across a fleet of dashboards that
// already exist, is almost every page. Pages therefore renders the control
// itself unless the document already reads this id, in which case the design
// owns it and Pages adds nothing but the data.
//
// Injection rather than substitution is what keeps this cheap and safe: no
// parser on the serve path, no offset cache to invalidate, no new failure site,
// and no amendment to "raw is served verbatim".
//
// It sits in <head>, which precedes <body>, so getElementById returns OURS even
// if a document somehow carries its own. StripInjectedNav removes any such copy
// at deploy time anyway, so the id means one thing.
const NavBlockID = "pages-nav"

var (
	// The id attribute, tested against one opening tag at a time: the three
	// quoting forms spelled out, so `id=pages-nav-extra` is not a match. (The JS
	// used a lookahead for the unquoted form; the tag always ends in `>`, so
	// consuming the following byte is equivalent for a test.)
	navIDAttrRE    = regexp.MustCompile(sp + fold("id") + sp + `*=` + sp + `*(?:"` + fold("pages-nav") + `"|'` + fold("pages-nav") + `'|` + fold("pages-nav") + `[` + jsS + `>])`)
	scriptOpenRE   = regexp.MustCompile(`<` + fold("script") + `\b[^>]*>`)
	scriptCloseRE  = regexp.MustCompile(`</` + fold("script") + sp + `*>`)
	headOpenRE     = regexp.MustCompile(`<` + fold("head") + `(?:>|` + sp + `[^>]*>)`)
	htmlOpenRE     = regexp.MustCompile(`<` + fold("html") + `[^>]*>`)
	bodyCloseRE    = regexp.MustCompile(`</` + fold("body") + sp + `*>`)
	bentoTypeValue = fold("application/bento+json")
	// bentoDocRE: a <script> whose type is application/bento+json, in any of the
	// three quoting forms.
	bentoDocRE = regexp.MustCompile(`<` + fold("script") + `\b[^>]*\b` + fold("type") + sp + `*=` + sp + `*(?:(?:"` + bentoTypeValue + `"|'` + bentoTypeValue + `')[^>]*>|` + bentoTypeValue + `(?:>|` + sp + `[^>]*>))`)
	navReadRE  = regexp.MustCompile(strings.Join([]string{
		`getElementById` + sp + `*\(` + sp + `*["'](?:#` + sp + `*)?pages-nav["']` + sp + `*\)`,
		`querySelector(?:All)?` + sp + `*\(` + sp + `*["']#?pages-nav["']` + sp + `*\)`,
		`\bid` + sp + `*=` + sp + `*["']pages-nav["']`,
	}, "|"))
)

// StripInjectedNav is deploy-time normalisation. `#pages-nav` is Pages' id; a
// document that ships its own copy would be a second answer to "which
// dashboards can this viewer open", authored by whoever wrote the page.
// Removing it at deploy time (before the content hash, like the config-schema
// normaliser) means the served bytes can only ever contain the one Pages
// injects.
//
// It walks <script> ELEMENTS, not text: a script's raw text runs to the first
// `</script`, so a `<script … id="pages-nav">` written inside another script —
// the campaign templates' page-switcher comment documents the block that way —
// is text, not an element. A single regex over the document matched that
// comment and deleted everything up to the end of the dashboard's own script.
func StripInjectedNav(sourceHTML string) string {
	if !strings.Contains(sourceHTML, NavBlockID) {
		return sourceHTML
	}
	var out strings.Builder
	kept := 0
	for pos := 0; pos <= len(sourceHTML); {
		open := scriptOpenRE.FindStringIndex(sourceHTML[pos:])
		if open == nil {
			break
		}
		openStart, openEnd := pos+open[0], pos+open[1]
		closeLoc := scriptCloseRE.FindStringIndex(sourceHTML[openEnd:])
		if closeLoc == nil {
			break
		}
		closeEnd := openEnd + closeLoc[1]
		if navIDAttrRE.MatchString(sourceHTML[openStart:openEnd]) {
			out.WriteString(sourceHTML[kept:openStart])
			kept = closeEnd
		}
		pos = closeEnd
	}
	if kept == 0 {
		return sourceHTML
	}
	out.WriteString(sourceHTML[kept:])
	return out.String()
}

// ReadsNavBlock reports whether this document RENDERS its own switcher. Scan
// the SOURCE (before injection, or the block about to be added would answer
// for it).
//
// This used to be a bare substring scan for the id. Production falsified that:
// dashboard-authoring boilerplate carries a CSS comment that mentions
// #pages-nav while the control it describes was never built, so the comment
// suppressed the built-in menu and the page rendered nothing. A false "owns it"
// hides the nav and takes a debugging session to find; a false "doesn't own it"
// doubles a menu on screen where the design's author sees it immediately. So
// the scan matches the ways a document actually CONSUMES the block — a DOM read
// of the id, or shipping an element that carries it.
func ReadsNavBlock(sourceHTML string) bool {
	return strings.Contains(sourceHTML, NavBlockID) && navReadRE.MatchString(sourceHTML)
}

// IsBentoDeck reports whether the document is a Bento deck — bento.page's
// one-file presentation, the .bento.html that fleet's bento-slides skill
// produces. A deck is its own viewer AND editor; Pages recognises one by its
// document block, which upstream's own tooling anchors on.
//
// Recognition matters because a deck is an application, not a dashboard. It has
// to be served byte-for-byte: no Flag head (the deck carries a complete design
// of its own, and its CSP <meta> must stay the first thing in <head>), and no
// Page menu (the deck's own toolbar is exactly where the control would land).
func IsBentoDeck(sourceHTML string) bool {
	return strings.Contains(sourceHTML, "application/bento+json") && bentoDocRE.MatchString(sourceHTML)
}

// EscapeStyleText makes a theme's override_css safe inside a <style> element.
// The HTML tokenizer ends a <style> at the first `</style`, wherever it appears
// — inside a CSS string, inside a comment — and everything after it is then
// parsed as markup in the page's own document.
//
// Escaping at render time rather than rejecting at write time also covers every
// theme already in the database. `\/` is CSS's escape for a solidus and means
// exactly `/` in a string or an identifier; in a comment the backslash is inert;
// and outside those there is no legal CSS in which `</` appears at all. So the
// rendered sheet is the same stylesheet. Deliberately every `</`, not just
// `</style`: the tokenizer is case-insensitive and accepts whitespace or `/`
// after the name, and enumerating those is how one spelling gets missed.
func EscapeStyleText(css string) string { return strings.ReplaceAll(css, "</", `<\/`) }

// navBlockTag uses datalimits.EscapedJSON, not plain JSON: the first dashboard title
// containing `</script` would otherwise end the block and put the rest of the
// payload into the document as markup.
func navBlockTag(nav any) string {
	return `<script type="application/json" id="` + NavBlockID + `" data-flag-injected>` + datalimits.EscapedJSON(nav) + `</script>`
}

func flagHead(overrideCSS string, nav any, withDefaultSwitcher bool) string {
	// Escaped as an attribute value as well as validated (sameOriginPath), so
	// no base can ever close the attribute it is written into.
	base := html.EscapeString(assetsBase())
	tags := []string{
		`<link rel="stylesheet" href="` + base + `/fonts/fonts.css" data-flag-injected>`,
		`<link rel="stylesheet" href="` + base + `/tokens/design-tokens.css" data-flag-injected>`,
	}
	if overrideCSS != "" && jsstr.Trim(overrideCSS) != "" {
		// override_css is curated by Elcano (not agents); still sandboxed regardless.
		tags = append(tags, "<style data-flag-injected>\n"+EscapeStyleText(overrideCSS)+"\n</style>")
	}
	tags = append(tags, `<script src="`+base+`/theme/theme-controller.js" data-flag-injected defer></script>`)
	if !isNil(nav) {
		tags = append(tags, navBlockTag(nav))
		// …and the control itself, unless the design renders its own.
		if withDefaultSwitcher {
			tags = append(tags, defaultSwitcherTags()...)
		}
	}
	return strings.Join(tags, "\n")
}

// isNil is JS truthiness for the nav payload: nil, or a typed nil pointer,
// map or slice, means "no portal authorised this view".
func isNil(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// ThemeOptions are InjectThemed's inputs.
type ThemeOptions struct {
	// OverrideCSS is the page's client theme sheet ("" for none).
	OverrideCSS string
	// Nav is the page-switcher payload, or nil when no portal authorised the
	// view. It must encode through encoding/json in the order the wire expects
	// (a struct, not a map).
	Nav any
}

// InjectThemed splices the Flag head (and, with a Nav, the switcher payload and
// possibly the built-in control) into the document.
//
// `<head` followed by whitespace or `>`, and not `<head[^>]*>`: the looser form
// also matches `<header class="…">`, so a fragment with a header and no head
// would have had the whole Flag payload spliced into its banner.
func InjectThemed(sourceHTML string, opts ThemeOptions) string {
	// Read the SOURCE, before the block we are about to add would answer for it.
	withSwitcher := !isNil(opts.Nav) && !ReadsNavBlock(sourceHTML)
	head := flagHead(opts.OverrideCSS, opts.Nav, withSwitcher)
	if headOpenRE.MatchString(sourceHTML) {
		return replaceFirst(headOpenRE, sourceHTML, func(m string) string { return m + "\n" + head })
	}
	if htmlOpenRE.MatchString(sourceHTML) {
		// The JS passed this replacement as a string, so `$&`, `$1` or `$'` in an
		// override sheet or a nav title was expanded by String#replace. Inserting
		// literally is the intended behaviour.
		return replaceFirst(htmlOpenRE, sourceHTML, func(m string) string { return m + "\n<head>\n" + head + "\n</head>" })
	}
	return "<!doctype html>\n<html>\n<head>\n" + head + "\n</head>\n<body>\n" + sourceHTML + "\n</body>\n</html>"
}

// InjectRawNav adds the switcher, and ONLY the switcher, to a `raw` document.
//
// `raw` exists so an agent can push a fully self-contained design and have
// Pages not restyle it. That is what it protects, and this keeps protecting it:
// no Flag tokens, no fonts, no theme controller ever reach a raw page. What it
// stopped protecting is navigation — most of a partner's set was raw, so "raw
// gets no Page menu" meant most of it was a dead end.
//
// Placement matters. The payload goes as early as the document allows, because
// a design that reads it can run its script anywhere; the control goes last, so
// nothing of the author's is displaced.
func InjectRawNav(sourceHTML string, nav any) string {
	payload := navBlockTag(nav)
	var control []string
	if !ReadsNavBlock(sourceHTML) {
		control = defaultSwitcherTags()
	}
	if !headOpenRE.MatchString(sourceHTML) {
		// Nothing to hang the data on early. Everything goes at the end rather than
		// before the doctype, which would put the document into quirks mode.
		return appendToBody(sourceHTML, strings.Join(append([]string{payload}, control...), "\n"))
	}
	withPayload := replaceFirst(headOpenRE, sourceHTML, func(m string) string { return m + "\n" + payload })
	if len(control) > 0 {
		return appendToBody(withPayload, strings.Join(control, "\n"))
	}
	return withPayload
}

func appendToBody(html, extra string) string {
	if bodyCloseRE.MatchString(html) {
		return replaceFirst(bodyCloseRE, html, func(m string) string { return extra + "\n" + m })
	}
	return html + "\n" + extra
}

// DeckHostAdaptation is the one thing Pages adds to a deck. The content host's
// sandbox refuses every native file picker, and Bento chooses between "rewrite
// this file in place" and "download an updated copy" by whether
// window.showSaveFilePicker EXISTS. So on Chrome and Edge, Save reached for the
// picker, the browser refused, and nothing happened. Making the API absent
// before the runtime boots puts Bento on its download path. Idempotent, and
// stripped again at deploy time (StripDeckHostAdaptation), because a file saved
// from the hosted editor carries it and may be uploaded back.
//
// Two more rules, both so that a file which LEAVES Pages is pure Bento: (1) the
// script removes its own element once it has run, so a saved copy does not carry
// it; (2) it acts only in a sandboxed document (window.origin is "null") served
// over http(s), so a local file:// open leaves the pickers alone.
const DeckHostAdaptation = `<script data-pages-deck-host>(function(){var s=document.currentScript;` +
	`if(window.origin==="null"&&/^https?:$/.test(location.protocol)){` +
	`for(var k of ["showSaveFilePicker","showOpenFilePicker","showDirectoryPicker"]){` +
	`try{Object.defineProperty(window,k,{value:undefined,configurable:true,writable:true})}catch(e){}}}` +
	`if(s&&s.parentNode)s.parentNode.removeChild(s)})();</script>`

var (
	// Matches the tag as WRITTEN and as a browser re-serialises it: a file saved
	// from the hosted editor comes back with `data-pages-deck-host=""`.
	deckHostAdaptationRE = regexp.MustCompile(`<` + fold("script") + `\b[^>]*\b` + fold("data-pages-deck-host") + `\b[^>]*>[\s\S]*?</` + fold("script") + sp + `*>`)
	// guardMetaRE is a deck's own CSP <meta>.
	guardMetaRE = regexp.MustCompile(`<` + fold("meta") + `\b[^>]*` + fold("http-equiv") + sp + `*=` + sp + `*["']` + fold("Content-Security-Policy") + `["'][^>]*>`)
)

// AdaptDeckToHost serves a deck: the stored bytes plus DeckHostAdaptation right
// after <head>, ahead of the bootstrap at the end of <body>. No whitespace of
// our own around the tag: the strip removes exactly the tag, so the stored
// bytes of a re-uploaded saved copy are the original bytes.
func AdaptDeckToHost(html string) string {
	clean := StripDeckHostAdaptation(html)
	if headOpenRE.MatchString(clean) {
		return replaceFirst(headOpenRE, clean, func(m string) string { return m + DeckHostAdaptation })
	}
	return appendToBody(clean, DeckHostAdaptation)
}

// StripDeckHostAdaptation removes every data-pages-deck-host tag, and restores
// a guard meta an edit session widened (connect-src <content origin> back to
// connect-src 'none'), so the stored bytes keep fleet's guard exactly as fleet
// wrote it and a copy that later leaves Pages cannot reach anything.
func StripDeckHostAdaptation(html string) string {
	out := html
	if strings.Contains(html, "data-pages-deck-host") {
		out = deckHostAdaptationRE.ReplaceAllLiteralString(html, "")
	}
	widened := "connect-src " + env.ContentOrigin()
	if strings.Contains(out, widened) {
		out = replaceFirst(guardMetaRE, out, func(tag string) string {
			return strings.Replace(tag, widened, "connect-src 'none'", 1)
		})
	}
	return out
}

// Renderable is a row from getRenderable plus the optional switcher payload.
type Renderable struct {
	HTML        string
	RenderMode  string // "raw" or "themed"
	OverrideCSS string
	// Nav is the page-switcher payload when a portal authorised the view, else
	// nil. See ThemeOptions.Nav.
	Nav any
}

// RenderVersion produces the served bytes.
//
// A deck is served as stored — whatever its row says, no theme and no switcher
// — plus the one host adaptation. `raw` is returned byte-for-byte UNLESS a
// portal authorised the view, in which case it gains the switcher and nothing
// else. Anything else is themed.
func RenderVersion(r Renderable) string {
	if IsBentoDeck(r.HTML) {
		return AdaptDeckToHost(r.HTML)
	}
	if r.RenderMode == "raw" {
		if isNil(r.Nav) {
			return r.HTML
		}
		return InjectRawNav(r.HTML, r.Nav)
	}
	return InjectThemed(r.HTML, ThemeOptions{OverrideCSS: r.OverrideCSS, Nav: r.Nav})
}
