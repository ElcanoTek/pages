// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package preflight is the static "will this page actually work once we serve
// it?" check.
//
// WHY THIS EXISTS. Agents author these dashboards in a sandbox with no browser,
// no JS engine, and no read access to the live page — so their only way to
// judge a document was counting braces. A live campaign dashboard shipped with
// a dead date-range picker through five deploy attempts and two user complaints
// because nothing in the loop could execute the page. Pages can: it holds the
// bytes AND it owns the CSP they will be served under, so the check belongs
// here.
//
// Deliberately STATIC (an HTML parser and a JS syntax parser; authored code is
// never run). No browser is launched on the request path. Every failure mode
// observed in the wild is detectable without running the page, and a static
// pass is fast enough to run inline on every deploy.
//
// Findings are advisory. Pages never blocks a deploy on them — humans own
// publish (PLAN.md §5) and a false positive must never wedge an agent.
//
// The shadowed-name list is generated: see internal/genshadowed.
package preflight

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/render"
)

// MaxPerRule caps each findings list. Reports are read by a model inside a tool
// result, so a pathological document must not blow out an agent's context; the
// counts still tell the truth.
const MaxPerRule = 8

// maxScriptBytes bounds the syntax check; it runs on the deploy path (cf. the
// managed-data CPU bound in page-data).
const maxScriptBytes = 1024 * 1024

// Finding is one advisory result. Fields are declared in the order the JS
// emitted them (code, message, then the rule's own keys, then fix); each rule
// sets only its own.
type Finding struct {
	Code          string   `json:"code"`
	Message       string   `json:"message"`
	Element       string   `json:"element,omitempty"`
	Attribute     string   `json:"attribute,omitempty"`
	Identifier    string   `json:"identifier,omitempty"`
	Snippet       string   `json:"snippet,omitempty"`
	Host          string   `json:"host,omitempty"`
	Directive     string   `json:"directive,omitempty"`
	API           string   `json:"api,omitempty"`
	APIs          []string `json:"apis,omitempty"`
	RequiredToken string   `json:"required_token,omitempty"`
	ContractCode  string   `json:"contract_code,omitempty"`
	ScriptIndex   int      `json:"script_index,omitempty"`
	Fix           string   `json:"fix,omitempty"`
}

// Report is analyze()'s result, in the JS key order.
type Report struct {
	OK              bool      `json:"ok"`
	RenderMode      string    `json:"render_mode"`
	Errors          []Finding `json:"errors"`
	Warnings        []Finding `json:"warnings"`
	ErrorsOmitted   int       `json:"errors_omitted"`
	WarningsOmitted int       `json:"warnings_omitted"`
	Checks          []string  `json:"checks"`
	Summary         string    `json:"summary"`
}

// Options configure Analyze. The zero value checks a raw page against
// DefaultContentPolicy on env.ContentOrigin(), without the managed-block rule.
type Options struct {
	// RenderMode is the mode the page will be stored with; "" means "raw".
	RenderMode string
	// ManagedCheck validates the Pages-managed blocks of the document as it
	// will be STORED. The server wires it to
	//
	//	func(h string) error { _, err := pagedata.ParseManagedHTML(pagedata.EnsureConfigSchema(h).HTML); return err }
	//
	// (preflight does not import pagedata, which depends on much more than a
	// static check should). An error carrying an apierror code is a finding
	// unless the code is page_not_data_managed or page_not_template_managed;
	// an error without a code is ignored, as the JS did. nil skips the rule.
	//
	// Judging the stored bytes rather than the submitted ones matters: shipping
	// #pages-config with no #pages-config-schema is a supported path —
	// prepareDeploy derives the missing block on every deploy — so checking the
	// submitted bytes would report a contract failure the served page does not
	// have, and this rule and the deploy-side refusal would disagree.
	ManagedCheck func(html string) error
	// ContentPolicy is the Content-Security-Policy value of a /raw response on
	// the content host (the csp package's raw headers). "" means
	// DefaultContentPolicy. Subresource and sandbox rules read it directly, so
	// guidance can never drift from what is served.
	ContentPolicy string
	// ContentOrigin is the content host's origin that 'self' and relative URLs
	// resolve against. "" means env.ContentOrigin().
	ContentOrigin string
}

// Checks lists every rule, in the order the report names them.
var Checks = []string{
	"script_syntax",
	"inline_handler_shadowed",
	"remote_subresource",
	"sandbox_capability",
	"network",
	"opaque_origin_api",
	"managed_block_contract",
	"page_switcher_block",
	"bento_deck",
}

// ── document helpers ────────────────────────────────────────────────────────

// walk visits n and its descendants in document order. A <template>'s
// contents are inert until a script clones them, and the JS walk never entered
// them (parse5 keeps them in a separate fragment), so neither does this one.
func walk(n *html.Node, visit func(*html.Node)) {
	visit(n)
	if n.Type == html.ElementNode && n.Data == "template" && n.Namespace == "" {
		return
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, visit)
	}
}

// attrs are the element's attributes, first occurrence of each name winning
// (a parse5 tree never carries a duplicate).
func attrs(n *html.Node) []html.Attribute {
	if n.Type != html.ElementNode {
		return nil
	}
	out := make([]html.Attribute, 0, len(n.Attr))
	for _, a := range n.Attr {
		if !slices.ContainsFunc(out, func(b html.Attribute) bool { return b.Namespace == a.Namespace && b.Key == a.Key }) {
			out = append(out, a)
		}
	}
	return out
}

// attr is the first attribute with that local name (parse5 compared names
// only, so xlink:href answers for href).
func attr(n *html.Node, name string) (string, bool) {
	for _, a := range attrs(n) {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func nodeName(n *html.Node) string {
	if n == nil || n.Type != html.ElementNode {
		return ""
	}
	return n.Data
}

func textOf(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

func capList(list []Finding) ([]Finding, int) {
	if len(list) <= MaxPerRule {
		return list, 0
	}
	return list[:MaxPerRule], len(list) - MaxPerRule
}

type script struct {
	src    string
	module bool
}

var executableType = regexp.MustCompile(`^(text/)?(javascript|module|ecmascript)$`)

func collectScripts(doc *html.Node) []script {
	var scripts []script
	walk(doc, func(n *html.Node) {
		if nodeName(n) != "script" {
			return
		}
		typ, _ := attr(n, "type")
		typ = strings.ToLower(typ)
		// Data blocks (application/json, text/template) are not executed.
		if typ != "" && !executableType.MatchString(typ) {
			return
		}
		if _, ok := attr(n, "src"); ok {
			return
		}
		if src := textOf(n); jsTrim(src) != "" {
			scripts = append(scripts, script{src: src, module: typ == "module"})
		}
	})
	return scripts
}

// ── R1: inline on*= handler shadowed by a DOM member ─────────────────────────
// The bug class that killed the live picker. `onclick="togglePopover()"` calls
// HTMLElement.prototype.togglePopover, never the author's global.

var (
	eventAttr      = regexp.MustCompile(`^on[a-z]+$`)
	definedGlobals = regexp.MustCompile(`(?:^|[` + jsS + `;}])(?:function` + sp + `*\*?` + sp + `*|(?:const|let|var)` + sp + `+)([A-Za-z_$][A-Za-z0-9_$]*)`)
)

func checkInlineHandlers(doc *html.Node, scriptText string) (errs, warns []Finding) {
	defined := map[string]bool{}
	for _, m := range definedGlobals.FindAllStringSubmatch(scriptText, -1) {
		defined[m[1]] = true
	}
	walk(doc, func(n *html.Node) {
		for _, a := range attrs(n) {
			if a.Namespace != "" || !eventAttr.MatchString(a.Key) {
				continue
			}
			for _, name := range bareCalls(a.Val) {
				if _, shadowed := shadowedNames[name]; !shadowed {
					continue
				}
				f := Finding{
					Element:    n.Data,
					Attribute:  a.Key,
					Identifier: name,
					Snippet:    "<" + n.Data + " " + a.Key + `="` + jsSlice(a.Val, 60) + `">`,
					Fix:        "Bind with addEventListener instead of " + a.Key + `=, or rename the global (e.g. "` + name + `" -> "dr_` + name + `").`,
				}
				if defined[name] {
					f.Code = "inline_handler_shadowed"
					f.Message = a.Key + `="` + name + `(...)" calls the built-in DOM member ` + name + `(), not your global function of the same name. ` +
						"Inline handlers resolve identifiers against the element, its form and the document before window, so this throws at click time and the control silently does nothing."
					errs = append(errs, f)
				} else {
					f.Code = "inline_handler_shadowed_maybe"
					f.Message = a.Key + `="` + name + `(...)" resolves to the built-in DOM member ` + name + `() inside an inline handler, not to a global. ` +
						"If you meant a function you define elsewhere, this control is dead."
					warns = append(warns, f)
				}
			}
		}
	})
	return errs, warns
}

// ── R2: subresources the served CSP will block ───────────────────────────────

type subresourceRule struct{ tag, attr, directive, kind string }

var subresourceRules = []subresourceRule{
	{"script", "src", "script-src", "script"},
	{"img", "src", "img-src", "image"},
	{"image", "href", "img-src", "image"},
	{"iframe", "src", "frame-src", "iframe"},
	{"object", "data", "object-src", "object"},
	{"embed", "src", "object-src", "embed"},
	{"video", "src", "media-src", "media"},
	{"audio", "src", "media-src", "media"},
}

var preloadDirective = map[string]string{
	"script": "script-src", "style": "style-src", "image": "img-src", "font": "font-src",
	"audio": "media-src", "video": "media-src", "fetch": "connect-src",
}

var (
	cssURL      = regexp.MustCompile(fold("url") + `\(` + sp + `*['"]?([^'")]+)['"]?` + sp + `*\)`)
	cssFontFace = regexp.MustCompile(`@` + fold("font-face") + sp + `*\{[^}]*\}`)
	cssImport   = regexp.MustCompile(`@` + fold("import") + sp + `+(?:` + fold("url") + `\(` + sp + `*)?['"]?([^'"` + jsS + `);]+)['"]?` + sp + `*\)?[^;]*;`)
	schemeSrc   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*:$`)
)

type subresourceChecker struct {
	pol    policy
	base   resolvedURL
	errors []Finding
}

// classify is resourceUrl: nil when there is nothing to check.
func (c *subresourceChecker) classify(value, directive string) (allowed bool, host string, ok bool) {
	if jsTrim(value) == "" {
		return false, "", false
	}
	u, valid := resolveURL(jsTrim(value), c.base)
	if !valid {
		return false, "an invalid URL", true
	}
	for _, source := range c.pol.sources(directive) {
		if source == "'self'" {
			if u.origin == c.base.origin && (u.protocol == "http:" || u.protocol == "https:") {
				allowed = true
			}
		} else if schemeSrc.MatchString(source) && u.protocol == strings.ToLower(source) {
			allowed = true
		}
	}
	host = u.host
	if host == "" {
		host = u.protocol
	}
	return allowed, host, true
}

func (c *subresourceChecker) check(value string, present bool, directive, element, description string) {
	if !present {
		return
	}
	allowed, host, ok := c.classify(value, directive)
	if !ok || allowed {
		return
	}
	permitted := c.pol.sources(directive)
	c.errors = append(c.errors, Finding{
		Code:      "remote_subresource_blocked",
		Message:   description + " references " + host + `, which the content host's "` + directive + `" blocks. It will not load.`,
		Element:   element,
		Host:      host,
		Directive: directive,
		Fix:       "Use an asset source permitted by " + directive + ": " + strings.Join(permitted, " ") + ". Do not widen the content policy to hide this finding.",
	})
}

func (c *subresourceChecker) cssURLs(css, directive string) {
	for _, m := range cssURL.FindAllStringSubmatch(css, -1) {
		c.check(m[1], true, directive, "css", "CSS url()")
	}
}

func checkSubresources(doc *html.Node, cssText string, pol policy, base resolvedURL) []Finding {
	c := &subresourceChecker{pol: pol, base: base}
	walk(doc, func(n *html.Node) {
		name := nodeName(n)
		if name == "" {
			return
		}
		for _, rule := range subresourceRules {
			if name == rule.tag {
				v, ok := attr(n, rule.attr)
				c.check(v, ok, rule.directive, rule.tag, "<"+rule.tag+"> "+rule.kind)
			}
		}
		if name == "video" {
			v, ok := attr(n, "poster")
			c.check(v, ok, "img-src", "video", "Video poster")
		}
		if parent := nodeName(n.Parent); name == "source" && (parent == "audio" || parent == "video") {
			v, ok := attr(n, "src")
			c.check(v, ok, "media-src", "source", "Media source")
		}
		if name == "link" {
			relAttr, _ := attr(n, "rel")
			rel := splitJSWhitespace(strings.ToLower(relAttr))
			var directive string
			switch {
			case slices.Contains(rel, "stylesheet"):
				directive = "style-src"
			case slices.Contains(rel, "modulepreload"):
				directive = "script-src"
			case slices.Contains(rel, "icon"):
				directive = "img-src"
			case slices.Contains(rel, "preload"):
				as, _ := attr(n, "as")
				directive = preloadDirective[strings.ToLower(as)]
			}
			if directive != "" {
				v, ok := attr(n, "href")
				c.check(v, ok, directive, "link", `<link rel="`+strings.Join(rel, " ")+`">`)
			}
		}
	})

	// Font sources and imports have different scheme permissions from images.
	// Remove the recognized blocks before scanning the remaining image URLs.
	images := cssFontFace.ReplaceAllStringFunc(cssText, func(block string) string {
		c.cssURLs(block, "font-src")
		return ""
	})
	var rest strings.Builder
	last := 0
	for _, m := range cssImport.FindAllStringSubmatchIndex(images, -1) {
		c.check(images[m[2]:m[3]], true, "style-src", "css", "CSS @import")
		rest.WriteString(images[last:m[0]])
		last = m[1]
	}
	rest.WriteString(images[last:])
	c.cssURLs(rest.String(), "img-src")
	return c.errors
}

// ── R3: APIs the sandbox response header turns into silent no-ops ────────────

type sandboxAPI struct {
	token, label, consequence string
	re                        *regexp.Regexp
}

const notMember = `(?:^|[^.\w$])`

var sandboxAPIs = []sandboxAPI{
	{"allow-modals", "window.print()", `Chromium logs "Ignored call to print()" and nothing happens — a "Download PDF" button does nothing at all.`,
		regexp.MustCompile(notMember + `(?:window` + sp + `*\.` + sp + `*)?print` + sp + `*\(`)},
	{"allow-modals", "alert()/confirm()/prompt()", "The call is ignored; confirm() returns false, so any flow gated on it silently takes the negative branch.",
		regexp.MustCompile(notMember + `(?:window` + sp + `*\.` + sp + `*)?(?:alert|confirm|prompt)` + sp + `*\(`)},
	{"allow-downloads", "a[download] / programmatic download", "The click is swallowed — an Export CSV/Excel button appears to work and produces no file.",
		regexp.MustCompile(`\.` + sp + `*` + fold("download") + sp + `*=|<` + fold("a") + `[^>]+` + sp + fold("download") + `[` + jsS + `=>]`)},
	{"allow-popups", "window.open()", "Returns null and no window opens.",
		regexp.MustCompile(notMember + `window` + sp + `*\.` + sp + `*open` + sp + `*\(`)},
}

func checkSandboxCapabilities(text string, tokens map[string]bool) []Finding {
	var errs []Finding
	for _, api := range sandboxAPIs {
		if tokens[api.token] || !api.re.MatchString(text) {
			continue
		}
		errs = append(errs, Finding{
			Code:          "sandbox_capability_missing",
			Message:       api.label + ` does not work on the content host: its sandbox does not include "` + api.token + `". ` + api.consequence,
			API:           api.label,
			RequiredToken: api.token,
			Fix:           `Remove the control, or ask for "` + api.token + `" to be added to the content-host sandbox in lib/csp.js.`,
		})
	}
	return errs
}

// ── R4: opaque-origin APIs that throw rather than no-op ──────────────────────

var opaqueOriginAPIs = []struct {
	re    *regexp.Regexp
	label string
}{
	{regexp.MustCompile(notMember + `(?:window` + sp + `*\.` + sp + `*)?(localStorage|sessionStorage)\b`), "localStorage/sessionStorage"},
	{regexp.MustCompile(`document` + sp + `*\.` + sp + `*cookie`), "document.cookie"},
	{regexp.MustCompile(notMember + `indexedDB\b`), "indexedDB"},
}

func checkOpaqueOrigin(text string) []Finding {
	var warns []Finding
	for _, api := range opaqueOriginAPIs {
		if !api.re.MatchString(text) {
			continue
		}
		warns = append(warns, Finding{
			Code:    "opaque_origin_api",
			Message: api.label + " throws SecurityError on the content host — pages are served from a sandboxed, cookieless opaque origin with no storage access.",
			API:     api.label,
			Fix:     "Keep the state in a module-level variable, or wrap every access in try/catch so an unhandled throw cannot take the rest of the script down with it.",
		})
	}
	return warns
}

// ── R5: network calls the CSP forbids outright ───────────────────────────────

var (
	fetchCall  = regexp.MustCompile(notMember + `fetch` + sp + `*\(`)
	xhrNew     = regexp.MustCompile(`new` + sp + `+XMLHttpRequest\b`)
	socketNew  = regexp.MustCompile(`new` + sp + `+(?:WebSocket|EventSource)\b`)
	beaconCall = regexp.MustCompile(notMember + `navigator` + sp + `*\.` + sp + `*sendBeacon` + sp + `*\(`)
)

func checkNetwork(text string) []Finding {
	var hits []string
	if fetchCall.MatchString(text) {
		hits = append(hits, "fetch()")
	}
	if xhrNew.MatchString(text) {
		hits = append(hits, "XMLHttpRequest")
	}
	if socketNew.MatchString(text) {
		hits = append(hits, "WebSocket/EventSource")
	}
	if beaconCall.MatchString(text) {
		hits = append(hits, "navigator.sendBeacon")
	}
	if len(hits) == 0 {
		return nil
	}
	return []Finding{{
		Code:    "network_blocked",
		Message: strings.Join(hits, ", ") + ` cannot reach anything: the content host sets "connect-src 'none'". Every request fails.`,
		APIs:    hits,
		Fix:     "Embed the data in the document at deploy time, or move the page to the managed-data path (update_page_data) so Pages refreshes the numbers for you.",
	}}
}

// ── R7: the Pages-managed blocks do not parse ────────────────────────────────
// The most consequential thing preflight can catch. A managed page reads its
// own numbers with JSON.parse(document.getElementById('pages-data').textContent)
// — so a malformed #pages-data block is not a data problem, it is an uncaught
// exception on line one of the render layer and a blank page. Answering ok:true
// for a document that cannot render at all is worse than staying silent.
//
// page_not_data_managed is not a finding. Most pages are ordinary documents
// with no managed blocks and nothing to check.

func checkManagedBlocks(source string, check func(string) error) []Finding {
	if check == nil {
		return nil
	}
	err := check(source)
	if err == nil {
		return nil
	}
	e, ok := apierror.As(err)
	if !ok || e.Code == "" || e.Code == "page_not_data_managed" || e.Code == "page_not_template_managed" {
		return nil
	}
	return []Finding{{
		Code:         "managed_block_invalid",
		Message:      "The Pages-managed blocks do not satisfy their own contract: " + err.Error(),
		ContractCode: e.Code,
		Fix: "The render layer JSON.parses these blocks, so this throws before anything draws and the page serves blank. " +
			"Fix the block contents and redeploy — or, if the numbers are what changed, use update_page_data instead of " +
			"editing the block by hand.",
	}}
}

// ── page switcher ────────────────────────────────────────────────────────────
// Both raw and themed pages receive navigation when a portal authorises the
// request. Source-only preflight cannot know that future request's
// credentials, so a reader is not itself a defect. Decks deliberately receive
// neither the payload nor the menu; the renderer's predicates are used so
// guidance follows the same exception and does not mistake a prose/comment
// mention for a reader.

func checkNavBlock(source string) []Finding {
	if !render.IsBentoDeck(source) || !render.ReadsNavBlock(source) {
		return nil
	}
	return []Finding{{
		Code:    "nav_block_ignored",
		Message: "This Bento deck reads #pages-nav, but Pages deliberately injects neither the navigation payload nor the Page menu into a deck, even when a portal authorises the view.",
		Fix: "Keep render_mode raw. Remove the custom switcher reader, or guard a missing block and render nothing; " +
			"the deck keeps its own toolbar. Return to the portal index to open another page.",
	}}
}

// ── R8: a Bento deck ─────────────────────────────────────────────────────────
// Every rule above reads plaintext. A Bento deck keeps its 650KB runtime
// deflate-compressed in a `type="bento/deflate-b64"` block and boots it through
// a blob: module import, so on a deck the rules above see one tiny bootstrap
// and fleet's guard script and report a clean page. That is exactly what
// preflight did while the deck could not start at all. So: say what a deck is
// and what to expect of it, and do not pretend the plaintext rules judged the
// application.

var (
	deckDocText = regexp.MustCompile(`<` + fold("script") + `\b[^>]*` + fold("application/bento+json") + `[^>]*>([\s\S]*?)</` + fold("script"))
	collabKey   = regexp.MustCompile(`"collab"` + sp + `*:` + sp + `*\{`)
)

func checkBentoDeck(source, renderMode string) (deck bool, errs, warns []Finding) {
	if !render.IsBentoDeck(source) {
		return false, nil, nil
	}
	if renderMode != "raw" {
		errs = append(errs, Finding{
			Code:    "bento_deck_themed",
			Message: `This is a Bento deck and render_mode is "themed". A deck is its own application with a complete design; themed would splice Flag's head tags ahead of the deck's own CSP meta and draw a Page menu over its toolbar. Pages refuses to store a deck this way.`,
			Fix:     `Omit render_mode — a deck defaults to raw — or pass "raw".`,
		})
	}
	warns = append(warns, Finding{
		Code:    "bento_deck",
		Message: "This is a Bento deck. Its runtime is deflate-compressed, so the static checks here could not see the application and judged only its bootstrap. On the content host the deck opens in Bento's full editor. Nothing edited there is saved to Pages: Save downloads a new .bento.html (the sandbox refuses native file pickers, so Pages removes that API before the deck boots and Bento takes its download path — it says so on the button), Export PDF prints, and unsaved edits are gone on reload (the sandbox has no storage). Speaker view needs pop-ups and copy-to-clipboard needs a permission, neither of which the sandbox grants. Embedded video and audio must be data: or blob: URLs.",
		Fix:     "Nothing to fix. To change the deck, edit it with fleet's bento_doc.py and deploy the new file as a new version.",
	})
	// Bento mints a live-collaboration block — room URL and private key — into
	// any file saved from its own UI. Inert here (both CSPs forbid connections),
	// but it is key material in a stored, shareable document, and if anyone ever
	// strips the deck's guard the room is joinable. Pages will not edit the
	// document block on deploy (its escaping rules are the helper's job), so say so.
	docBlock := ""
	if m := deckDocText.FindStringSubmatch(source); m != nil {
		docBlock = m[1]
	}
	if collabKey.MatchString(docBlock) {
		warns = append(warns, Finding{
			Code:    "bento_deck_collab_keys",
			Message: "This deck's document carries a live-collaboration block (a room URL and a private key). Bento writes one into any file saved from its own UI. It is inert on this host — the deck's own CSP and the content host's both forbid connections — but it is key material in a stored, shareable document, and if the deck's guard is ever removed the room becomes joinable.",
			Fix:     "Round-trip the file through fleet's bento_doc.py (`get`, then `set`) before deploying: `set` drops the block. Or accept the residue knowingly.",
		})
	}
	return true, errs, warns
}

// ── R6: the script does not even parse ───────────────────────────────────────
// Parse-only. Catches corruption that a chunked upload or a hand-reassembled
// document can leave behind.

func checkSyntax(scripts []script) []Finding {
	var errs []Finding
	for i, s := range scripts {
		if len(s.src) > maxScriptBytes {
			continue
		}
		index := i + 1
		v := checkScriptSyntax(s.src, s.module)
		switch {
		case v.limit:
			parser := "module"
			if !s.module {
				parser = "script"
			}
			errs = append(errs, Finding{
				Code:        "script_analysis_limit",
				Message:     "Inline <script> #" + strconv.Itoa(index) + " exceeds the " + parser + " parser's nesting limit; its syntax could not be verified.",
				ScriptIndex: index,
				Fix:         "Reduce expression nesting and re-run preflight; this finding does not prove a browser syntax error.",
			})
		case v.err != "":
			errs = append(errs, Finding{
				Code:        "script_syntax_error",
				Message:     "Inline <script> #" + strconv.Itoa(index) + " does not parse: " + v.err + ". Nothing in that block runs, so every function it declares is undefined and every control wired to one is dead.",
				ScriptIndex: index,
				Fix:         "Re-check the document was uploaded whole — a truncated or hand-reassembled script is the usual cause.",
			})
		}
	}
	return errs
}

// Analyze checks a page document (the stored source, before theme injection)
// the way the content host will serve it.
func Analyze(source string, opts Options) Report {
	renderMode := opts.RenderMode
	if renderMode == "" {
		renderMode = "raw"
	}
	csp := opts.ContentPolicy
	if csp == "" {
		csp = DefaultContentPolicy
	}
	pol := parsePolicy(csp)
	origin := opts.ContentOrigin
	if origin == "" {
		origin = env.ContentOrigin()
	}
	base, ok := resolveURL(origin, resolvedURL{origin: "null"})
	if !ok {
		base = resolvedURL{origin: "null"}
	}

	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		// Reading from a string cannot fail; an empty document is the safe answer.
		doc = &html.Node{Type: html.DocumentNode}
	}

	scripts := collectScripts(doc)
	srcs := make([]string, len(scripts))
	for i, s := range scripts {
		srcs[i] = s.src
	}
	scriptText := strings.Join(srcs, "\n;\n")

	var css strings.Builder
	walk(doc, func(n *html.Node) {
		if nodeName(n) == "style" {
			css.WriteString(textOf(n) + "\n")
		}
		for _, a := range attrs(n) {
			if a.Key == "style" && a.Namespace == "" {
				css.WriteString(a.Val + "\n")
			}
		}
	})

	handlerErrs, handlerWarns := checkInlineHandlers(doc, scriptText)
	isDeck, deckErrs, deckWarns := checkBentoDeck(source, renderMode)
	errs := []Finding{}
	errs = append(errs, checkSyntax(scripts)...)
	errs = append(errs, handlerErrs...)
	errs = append(errs, checkSubresources(doc, css.String(), pol, base)...)
	errs = append(errs, checkSandboxCapabilities(scriptText+"\n"+source, pol.sandboxTokens())...)
	errs = append(errs, checkNetwork(scriptText)...)
	errs = append(errs, checkManagedBlocks(source, opts.ManagedCheck)...)
	errs = append(errs, deckErrs...)

	warns := []Finding{}
	warns = append(warns, handlerWarns...)
	warns = append(warns, checkOpaqueOrigin(scriptText)...)
	warns = append(warns, checkNavBlock(source)...)
	// On a deck the one plaintext storage hit is fleet's guard script, which is
	// already wrapped in try/catch; the deck finding says what missing storage
	// means to a reader, so the generic one would only repeat it.
	if isDeck {
		warns = slices.DeleteFunc(warns, func(f Finding) bool { return f.Code == "opaque_origin_api" })
	}
	warns = append(warns, deckWarns...)

	e, eOmitted := capList(errs)
	w, wOmitted := capList(warns)
	var parts []string
	if len(e) > 0 {
		parts = append(parts, plural(len(errs), "error"))
	}
	if len(w) > 0 {
		parts = append(parts, plural(len(warns), "warning"))
	}
	summary := "Preflight found no problems: scripts parse, managed blocks satisfy their contract, no shadowed inline handlers, no blocked subresources or APIs."
	if len(parts) > 0 {
		summary = "Preflight found " + strings.Join(parts, " and ") + ". These are advisory — nothing was blocked."
	}
	return Report{
		OK:              len(errs) == 0,
		RenderMode:      renderMode,
		Errors:          e,
		Warnings:        w,
		ErrorsOmitted:   eOmitted,
		WarningsOmitted: wOmitted,
		Checks:          slices.Clone(Checks),
		Summary:         summary,
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}
