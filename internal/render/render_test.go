// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package render

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

type navPortal struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type navPage struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Current bool   `json:"current"`
	Home    bool   `json:"home,omitempty"`
}

type navPayload struct {
	Portal    navPortal `json:"portal"`
	Pages     []navPage `json:"pages"`
	Truncated bool      `json:"truncated"`
}

var navFixture = &navPayload{
	Portal:    navPortal{Slug: "nwm", Name: "Northwind Media Group"},
	Pages:     []navPage{{Slug: "nwm-contoso", Title: "Contoso — Fabrikam", URL: "https://elcano-pages.com/nwm-contoso", Current: true}},
	Truncated: false,
}

var (
	builtInSheetRE = regexp.MustCompile(`:host\{all:initial;position:fixed`)
	themingRE      = regexp.MustCompile(`design-tokens\.css|fonts/fonts\.css|theme-controller`)
)

func hasBuiltIn(html string) bool {
	return builtInSheetRE.MatchString(html) && strings.Contains(html, `className="pgnav-host"`)
}

func mustContain(t *testing.T, s, sub, msg string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s: %q not found", msg, sub)
	}
}

func TestRawIsVerbatim(t *testing.T) {
	html := "<!doctype html><html><head></head><body><script>chart()</script></body></html>"
	if got := RenderVersion(Renderable{RenderMode: "raw", HTML: html}); got != html {
		t.Fatalf("raw changed: %q", got)
	}
}

func TestRawInPortalGainsSwitcherOnly(t *testing.T) {
	html := "<!doctype html><html><head><title>Bespoke</title></head><body><h1>Hi</h1><script>chart()</script></body></html>"
	out := RenderVersion(Renderable{RenderMode: "raw", HTML: html, Nav: navFixture})
	if themingRE.MatchString(out) {
		t.Error("raw is never themed, portal or not")
	}
	mustContain(t, out, `id="pages-nav"`, "payload")
	if strings.Index(out, `id="pages-nav"`) > strings.Index(out, "<body") {
		t.Error("payload as early as the document allows")
	}
	if !hasBuiltIn(out) {
		t.Error("built-in control missing")
	}
	if strings.Index(out, "pgnav") < strings.Index(out, "<h1>Hi</h1>") {
		t.Error("control after the author's content")
	}
	if !regexp.MustCompile(`pgnav[\s\S]*</body>`).MatchString(out) {
		t.Error("control inside the body")
	}
	if strings.Index(out, "<h1>Hi</h1>") > strings.Index(out, "<script>chart()</script>") {
		t.Error("author's bytes out of order")
	}
	reader := strings.Replace(html, "<h1>", `<script>document.getElementById("pages-nav");</script><h1>`, 1)
	owned := RenderVersion(Renderable{RenderMode: "raw", HTML: reader, Nav: navFixture})
	mustContain(t, owned, `id="pages-nav"`, "reader still gets payload")
	if builtInSheetRE.MatchString(owned) {
		t.Error("no second menu")
	}
	if RenderVersion(Renderable{RenderMode: "raw", HTML: html}) != html {
		t.Error("no portal → byte-for-byte")
	}
}

func TestRawInjectionMalformed(t *testing.T) {
	cases := map[string]string{
		"no closing body":    "<!doctype html><html><head></head><body><p>x</p></html>",
		"no head at all":     "<!doctype html><html><body><p>x</p></body></html>",
		"fragment only":      "<p>just a fragment</p>",
		"header but no head": "<!doctype html><html><body><header class=top>Nav</header><p>x</p></body></html>",
	}
	for name, html := range cases {
		out := RenderVersion(Renderable{RenderMode: "raw", HTML: html, Nav: navFixture})
		mustContain(t, out, `id="pages-nav"`, name)
		if !strings.HasPrefix(out, html[:15]) {
			t.Errorf("%s: something prepended", name)
		}
		if name == "header but no head" && strings.Index(out, "pages-nav") < strings.Index(out, "</header>") {
			t.Error("spliced into a <header>")
		}
	}
	themed := RenderVersion(Renderable{RenderMode: "themed", HTML: "<!doctype html><html><body><header class=top>Nav</header><p>x</p></body></html>"})
	if strings.Index(themed, "design-tokens.css") > strings.Index(themed, "<header") {
		t.Error("Flag goes in a synthesized head, not the banner")
	}
}

func TestThemedInjectsFlag(t *testing.T) {
	html := "<!doctype html><html><head><title>x</title></head><body><canvas id=c></canvas><script>chart()</script></body></html>"
	out := RenderVersion(Renderable{RenderMode: "themed", HTML: html})
	mustContain(t, out, `design-tokens.css" data-flag-injected`, "tokens")
	mustContain(t, out, `theme-controller.js" data-flag-injected`, "controller")
	mustContain(t, out, "<canvas id=c></canvas><script>chart()</script>", "body verbatim")

	out = RenderVersion(Renderable{RenderMode: "themed", HTML: "<html><head></head><body>hi</body></html>", OverrideCSS: ":root{--color-primary:#c8102e}"})
	mustContain(t, out, "--color-primary:#c8102e", "override")
	mustContain(t, out, "<style data-flag-injected>", "override tag")

	out = RenderVersion(Renderable{RenderMode: "themed", HTML: "<p>bare fragment</p>"})
	mustContain(t, out, "<head>", "synthesized head")
	mustContain(t, out, "design-tokens.css", "tokens in fragment")
	mustContain(t, out, "<p>bare fragment</p>", "fragment kept")
}

func TestThemeCannotCloseItsElement(t *testing.T) {
	payload := ":root{--x:1}\n.a::after{content:\"</style><script>window.__pwned=1</script>\"}"
	out := RenderVersion(Renderable{RenderMode: "themed", HTML: "<html><head></head><body>hi</body></html>", OverrideCSS: payload})
	var sheet string
	for _, tag := range regexp.MustCompile(`<style data-flag-injected>[\s\S]*?</style>`).FindAllString(out, -1) {
		if strings.Contains(tag, "--x:1") {
			sheet = tag
		}
	}
	if sheet == "" {
		t.Fatal("the theme still renders")
	}
	if strings.Contains(sheet, "</script>") {
		t.Error("the theme's script tag became a tag")
	}
	mustContain(t, sheet, `content:"<\/style><script>window.__pwned=1<\/script>"`, "escaped solidus")
	if n := len(regexp.MustCompile(`(?i)</style`).FindAllString(sheet, -1)); n != 1 || !strings.HasSuffix(sheet, "</style>") {
		t.Errorf("sheet closes early (%d closers)", n)
	}
	if strings.Contains(strings.Replace(out, sheet, "", 1), "__pwned") {
		t.Error("payload escaped the sheet")
	}
	plain := RenderVersion(Renderable{RenderMode: "themed", HTML: "<html><head></head><body>hi</body></html>", OverrideCSS: ":root{--color-primary:#c8102e}"})
	mustContain(t, plain, ":root{--color-primary:#c8102e}", "ordinary theme")
	if strings.Contains(plain, `\/`) {
		t.Error("an ordinary theme gains no escapes")
	}
}

// The JS spliced the head into `<html>` with a String#replace pattern string,
// so `$&` in a theme or a title was expanded into the matched tag.
func TestThemedInsertionIsLiteral(t *testing.T) {
	out := RenderVersion(Renderable{RenderMode: "themed", HTML: "<html lang=en><body>x</body></html>", OverrideCSS: "a::after{content:\"$& $1 $$ $'\"}"})
	mustContain(t, out, "a::after{content:\"$& $1 $$ $'\"}", "literal theme")
	if strings.Count(out, "<html lang=en>") != 1 {
		t.Error("$& expanded")
	}
}

func TestSwitcherPayloadInHead(t *testing.T) {
	src := "<!doctype html><html><head><title>T</title></head><body><h1>Live</h1></body></html>"
	withNav := RenderVersion(Renderable{HTML: src, RenderMode: "themed", Nav: navFixture})
	mustContain(t, withNav, `<script type="application/json" id="pages-nav" data-flag-injected>`, "head tag")
	if strings.Index(withNav, "pages-nav") > strings.Index(withNav, "<body") {
		t.Error("payload in <head>")
	}
	block := regexp.MustCompile(`id="pages-nav"[^>]*>([\s\S]*?)</script>`).FindStringSubmatch(withNav)[1]
	var parsed navPayload
	if err := json.Unmarshal([]byte(block), &parsed); err != nil || !reflect.DeepEqual(&parsed, navFixture) {
		t.Errorf("payload round trip: %v %#v", err, parsed)
	}
	withoutNav := RenderVersion(Renderable{HTML: src, RenderMode: "themed"})
	if strings.Contains(withoutNav, "pages-nav") {
		t.Error("no portal, no block")
	}
	bodyOf := func(h string) string { _, after, _ := strings.Cut(h, "<body"); return after }
	headOf := func(h string) string { before, _, _ := strings.Cut(h, "<body"); return before }
	if bodyOf(withNav) != bodyOf(withoutNav) {
		t.Error("author's body changed")
	}
	added := strings.Replace(headOf(withNav), headOf(withoutNav), "", 1)
	if added == "" {
		t.Fatal("switcher added nothing")
	}
	for _, tag := range regexp.MustCompile(`<(script|style|link)\b[^>]*>`).FindAllString(added, -1) {
		if !strings.Contains(tag, "data-flag-injected") {
			t.Errorf("unmarked injected tag %q", tag)
		}
	}
	if RenderVersion(Renderable{HTML: src, RenderMode: "raw"}) != src {
		t.Error("raw without portal")
	}
	if themingRE.MatchString(RenderVersion(Renderable{HTML: src, RenderMode: "raw", Nav: navFixture})) {
		t.Error("raw themed")
	}
}

func TestReadsNavBlockOwnership(t *testing.T) {
	plain := "<!doctype html><html><head><title>T</title></head><body><h1>Old dashboard</h1></body></html>"
	reader := strings.Replace(plain, "<h1>", `<script>var n=document.getElementById("pages-nav");</script><h1>`, 1)
	boilerplate := strings.Replace(plain, "<h1>", "<style>/* Page switcher (Pages injects #pages-nav on a portal-authorised render; absent\n   otherwise, and this whole control is then never built). */</style><h1>", 1)
	checks := []struct {
		html string
		want bool
	}{
		{plain, false},
		{reader, true},
		{boilerplate, false},
		{strings.Replace(plain, "<h1>", `<script>document.querySelector("#pages-nav");</script><h1>`, 1), true},
		{strings.Replace(plain, "<h1>", `<nav id="pages-nav"></nav><h1>`, 1), true},
	}
	for i, c := range checks {
		if got := ReadsNavBlock(c.html); got != c.want {
			t.Errorf("case %d: ReadsNavBlock = %v", i, got)
		}
	}
	builtIn := RenderVersion(Renderable{HTML: plain, RenderMode: "themed", Nav: navFixture})
	if !hasBuiltIn(builtIn) || !strings.Contains(builtIn, `id="pages-nav"`) {
		t.Error("plain design gets the built-in control and payload")
	}
	own := RenderVersion(Renderable{HTML: reader, RenderMode: "themed", Nav: navFixture})
	if hasBuiltIn(own) || !strings.Contains(own, `id="pages-nav"`) {
		t.Error("reader gets payload, no second menu")
	}
	if !hasBuiltIn(RenderVersion(Renderable{HTML: boilerplate, RenderMode: "themed", Nav: navFixture})) {
		t.Error("a comment mention must not cost the page its menu")
	}
	for _, h := range []string{plain, reader} {
		if hasBuiltIn(RenderVersion(Renderable{HTML: h, RenderMode: "themed"})) {
			t.Error("no portal, no control")
		}
	}
	// A typed nil pointer is "no portal" too.
	var none *navPayload
	if strings.Contains(RenderVersion(Renderable{HTML: plain, RenderMode: "themed", Nav: none}), "pages-nav") {
		t.Error("typed nil nav injected a block")
	}
}

func TestHostileTitles(t *testing.T) {
	nav := &navPayload{
		Portal: navPortal{Slug: "p", Name: "</style><img src=x onerror=alert(1)>"},
		Pages: []navPage{
			{Slug: "a", Title: "</script><img src=y onerror=alert(2)>", URL: "https://x/a"},
			{Slug: "b", Title: "B", URL: "https://x/b", Current: true},
		},
	}
	out := RenderVersion(Renderable{HTML: "<!doctype html><html><head></head><body></body></html>", RenderMode: "themed", Nav: nav})
	for _, bad := range []string{"<img src=x", "<img src=y", "</style><img", "</script><img"} {
		if strings.Contains(out, bad) {
			t.Errorf("markup escaped: %q", bad)
		}
	}
	script := regexp.MustCompile(`<script data-flag-injected>([\s\S]*?)</script>`).FindStringSubmatch(out)[1]
	if regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write`).MatchString(script) {
		t.Error("markup sink in the control")
	}
	mustContain(t, script, "textContent", "titles as text")
	if _, err := js.Parse(parse.NewInputString(script), js.Options{}); err != nil {
		t.Errorf("built-in control does not parse: %v", err)
	}
	block := regexp.MustCompile(`id="pages-nav"[^>]*>([\s\S]*?)</script>`).FindStringSubmatch(out)[1]
	var parsed navPayload
	_ = json.Unmarshal([]byte(block), &parsed)
	if parsed.Pages[0].Title != "</script><img src=y onerror=alert(2)>" {
		t.Error("title reads back")
	}
}

func TestStripInjectedNav(t *testing.T) {
	body := `<script type="application/json" id="pages-nav">{"pages":[{"slug":"someone-elses"}]}</script>`
	for _, variant := range []string{body, strings.Replace(body, `"pages-nav"`, `'pages-nav'`, 1), strings.Replace(body, `"pages-nav"`, "pages-nav", 1)} {
		html := "<html><head></head><body>" + variant + "<p>real content</p></body></html>"
		stripped := StripInjectedNav(html)
		if strings.Contains(stripped, "someone-elses") || !strings.Contains(stripped, "<p>real content</p>") {
			t.Errorf("variant %q: %q", variant, stripped)
		}
	}
	keep := `<html><head></head><body><script id="pages-nav-extra">keepme</script><script>chart()</script></body></html>`
	if StripInjectedNav(keep) != keep {
		t.Error("lookalike id stripped")
	}
	app := "<script>const a = 1;\n/* Pages injects <script type=\"application/json\" id=\"pages-nav\"> into <head> */\nrenderAll();\n</script>"
	html := "<html><head></head><body>" + app + "<p>after</p></body></html>"
	if StripInjectedNav(html) != html {
		t.Error("text inside a script is not an element")
	}
	stored := body
	both := "<html><head>" + stored + "</head><body>" + app + strings.Replace(stored, "someone-elses", "another", 1) + "</body></html>"
	if got := StripInjectedNav(both); got != "<html><head></head><body>"+app+"</body></html>" {
		t.Errorf("both copies: %q", got)
	}
}

func TestLargeDocumentRenderIsFast(t *testing.T) {
	big := "<!doctype html><html><head><title>Big</title></head><body>" + strings.Repeat("<p>x</p>", 280_000) + "</body></html>"
	start := time.Now()
	out := RenderVersion(Renderable{HTML: big, RenderMode: "themed", Nav: navFixture})
	elapsed := time.Since(start)
	mustContain(t, out, `id="pages-nav"`, "payload")
	if elapsed > 100*time.Millisecond {
		t.Errorf("2 MiB themed render took %v", elapsed)
	}
}

func TestEscapedJSONMatchesDataLimits(t *testing.T) {
	got := datalimits.EscapedJSON(map[string]any{"a": "</script>&\u2028\u2029'"})
	want := `{"a":"\u003c/script\u003e\u0026\u2028\u2029'"}`
	if got != want {
		t.Errorf("got %s want %s", got, want)
	}
}
