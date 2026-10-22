// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/render"
)

// Regression cover for the failure that shipped a dead date-range picker to a
// live client dashboard through five deploy attempts: an inline handler whose
// bare identifier resolved to a built-in DOM member instead of the page's own
// global.

func wrapPage(body, script string) string {
	return "<!doctype html><html><head><title>t</title></head><body>" + body + "<script>" + script + "</script></body></html>"
}

func raw(html string) Report { return Analyze(html, Options{RenderMode: "raw"}) }

func codes(fs []Finding) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Code
	}
	return out
}

func find(fs []Finding, code string) *Finding {
	for i := range fs {
		if fs[i].Code == code {
			return &fs[i]
		}
	}
	return nil
}

func count(fs []Finding, code string) int {
	n := 0
	for _, f := range fs {
		if f.Code == code {
			n++
		}
	}
	return n
}

func TestShadowedHandlerWithGlobalIsError(t *testing.T) {
	r := raw(wrapPage(`<button onclick="togglePopover()">x</button>`, "function togglePopover(){}"))
	hit := find(r.Errors, "inline_handler_shadowed")
	if r.OK || hit == nil || hit.Identifier != "togglePopover" || !strings.Contains(hit.Fix, "addEventListener") {
		t.Fatalf("%+v", r)
	}
}

func TestHandlerVariants(t *testing.T) {
	if r := raw(wrapPage(`<button id="t">x</button>`, "function togglePopover(){}\ndocument.getElementById('t').addEventListener('click', togglePopover);")); !r.OK {
		t.Errorf("addEventListener is clean: %+v", r.Errors)
	}
	if r := raw(wrapPage(`<button onclick="window.togglePopover()">x</button>`, "function togglePopover(){}")); count(r.Errors, "inline_handler_shadowed") != 0 {
		t.Error("member calls do not shadow")
	}
	if r := raw(wrapPage(`<button onclick="drTogglePopover()">x</button>`, "function drTogglePopover(){}")); !r.OK {
		t.Error("non-colliding name flagged")
	}
	r := raw(wrapPage(`<button onclick="close()">x</button>`, "var x=1;"))
	if count(r.Errors, "inline_handler_shadowed") != 0 || find(r.Warnings, "inline_handler_shadowed_maybe") == nil {
		t.Errorf("shadowed name without a global is a warning: %+v", r)
	}
}

func TestBlockedSubresources(t *testing.T) {
	r := raw(wrapPage(`<img src="https://www.weathercompany.com/logo.svg"><script src="https://cdn.example.com/chart.js"></script>`, ""))
	var hosts []string
	for _, e := range r.Errors {
		if e.Code == "remote_subresource_blocked" {
			hosts = append(hosts, e.Host)
		}
	}
	sort.Strings(hosts)
	if !slices.Equal(hosts, []string{"cdn.example.com", "www.weathercompany.com"}) {
		t.Errorf("hosts %v", hosts)
	}
	r = raw(wrapPage(`<img src="data:image/png;base64,iVBORw0KGgo="><img src="/assets/x.png"><img src="blob:abc">`, ""))
	if count(r.Errors, "remote_subresource_blocked") != 0 {
		t.Error("data:, blob: and relative URLs are same-origin-safe")
	}
}

func TestURLClassificationFollowsEachDirective(t *testing.T) {
	for _, origin := range []string{"", "http://localhost:8080"} {
		t.Run(origin, func(t *testing.T) {
			t.Setenv("CONTENT_ORIGIN", origin)
			own, _ := resolveURL(env.ContentOrigin(), resolvedURL{origin: "null"})
			cases := []struct {
				html                  func(string) string
				directive             string
				relative, data, blobs bool
			}{
				{func(u string) string { return `<script src="` + u + `"></script>` }, "script-src", true, false, true},
				{func(u string) string { return `<link rel="stylesheet" href="` + u + `">` }, "style-src", true, false, false},
				{func(u string) string { return `<img src="` + u + `">` }, "img-src", true, true, true},
				{func(u string) string { return `<link rel="preload" as="font" href="` + u + `">` }, "font-src", true, true, false},
				{func(u string) string { return `<video src="` + u + `"></video>` }, "media-src", false, true, true},
				{func(u string) string { return `<iframe src="` + u + `"></iframe>` }, "frame-src", false, false, false},
				{func(u string) string { return `<object data="` + u + `"></object>` }, "object-src", false, false, false},
			}
			for _, c := range cases {
				for _, u := range []struct {
					url     string
					allowed bool
				}{
					{"/assets/example", c.relative}, {"../assets/example", c.relative},
					{own.origin + "/assets/example", c.relative}, {"//" + own.host + "/assets/example", c.relative},
					{"//northwind.invalid/asset", false}, {"https://northwind.invalid/asset", false},
					{"data:application/octet-stream;base64,AA==", c.data}, {"blob:null/fixture", c.blobs},
				} {
					r := Analyze(wrapPage(c.html(u.url), ""), Options{})
					var hits []Finding
					for _, e := range r.Errors {
						if e.Code == "remote_subresource_blocked" {
							hits = append(hits, e)
						}
					}
					want := 1
					if u.allowed {
						want = 0
					}
					if len(hits) != want || want == 1 && hits[0].Directive != c.directive {
						t.Errorf("%s %s: %+v", c.directive, u.url, hits)
					}
				}
			}
		})
	}
}

func TestCSSAndMediaUseTheirOwnPolicy(t *testing.T) {
	r := Analyze(wrapPage(`
    <style>
      @import url('//northwind.invalid/style.css');
      @font-face { font-family: Northwind; src: url('blob:null/font'); }
      .logo { background: url('data:image/svg+xml;base64,AA=='); }
    </style>
    <video poster="data:image/png;base64,AA=="><source src="/movie.mp4"></video>
    <link rel="modulepreload" href="//northwind.invalid/code.js">
    <link rel="preload" as="fetch" href="/data.json">
  `, ""), Options{})
	var dirs []string
	for _, e := range r.Errors {
		if e.Code == "remote_subresource_blocked" {
			dirs = append(dirs, e.Directive)
		}
	}
	sort.Strings(dirs)
	if !slices.Equal(dirs, []string{"connect-src", "font-src", "media-src", "script-src", "style-src"}) {
		t.Errorf("directives %v", dirs)
	}
}

func TestSyntax(t *testing.T) {
	r := raw(wrapPage("<div></div>", "function broken({ oops"))
	if hit := find(r.Errors, "script_syntax_error"); hit == nil || hit.ScriptIndex != 1 {
		t.Fatalf("%+v", r.Errors)
	}
	for _, src := range []string{
		"export const value = 1;",
		"import { value } from './values.js'; export { value };",
		"const value = await Promise.resolve(1); export default value;",
		"export const url = import.meta.url;",
	} {
		if n := count(Analyze(`<script type="module">`+src+`</script>`, Options{}).Errors, "script_syntax_error"); n != 0 {
			t.Errorf("module grammar rejects %q", src)
		}
		if n := count(Analyze(`<script>`+src+`</script>`, Options{}).Errors, "script_syntax_error"); n != 1 {
			t.Errorf("classic grammar accepts %q", src)
		}
	}
	r = Analyze(strings.Repeat(`<script type="module">export const = 1;</script>`, 20), Options{})
	if len(r.Errors) != MaxPerRule || r.ErrorsOmitted == 0 || r.Errors[0].Code != "script_syntax_error" || r.Errors[0].ScriptIndex != 1 {
		t.Errorf("bounded: %+v", r)
	}
	nested := strings.Repeat("(", 3000) + "1" + strings.Repeat(")", 3000)
	r = Analyze(`<script type="module">`+nested+`</script>`, Options{})
	if count(r.Errors, "script_syntax_error") != 0 || r.Errors[0].Code != "script_analysis_limit" {
		t.Errorf("parser limit: %+v", r.Errors)
	}
	r = Analyze(`<script type="module">
    import './does-not-exist.js';
    globalThis.__pagesPreflightExecuted = true;
    throw new Error('must never run');
  </script>`, Options{})
	if count(r.Errors, "script_syntax_error") != 0 {
		t.Error("valid module rejected")
	}
	r = raw(`<!doctype html><html><body><script type="application/json">{"a": 1,}</script></body></html>`)
	if count(r.Errors, "script_syntax_error") != 0 {
		t.Error("JSON data blocks are not script")
	}
}

// A Go stack overflow is a fatal crash, not an error: no document may be able
// to recurse a parser without bound. Each hostile input must be stopped by the
// nesting limit (script_analysis_limit, not a crash and not a syntax verdict
// reached by recursing all the way down), and stopping it must cost no more
// than linear work: doubling the input may at most about double the cost,
// counted in allocations, never timed.
func TestHostileNestingNeverCrashes(t *testing.T) {
	hostile := func(n int) map[string]string {
		return map[string]string{
			"arrays":      "var " + strings.Repeat("[", n/2) + strings.Repeat("]", n/2) + "=x",
			"objects":     "var " + strings.Repeat("{a:", n/4) + "b" + strings.Repeat("}", n/4) + "=x",
			"parens":      strings.Repeat("(", n/2) + strings.Repeat(")", n/2),
			"unary":       strings.Repeat("!", n) + "1",
			"assignments": strings.Repeat("a=", n/2) + "1",
			"arrows":      strings.Repeat("x=>", n/3) + "1",
			"templates":   strings.Repeat("`${", n/6) + strings.Repeat("}`", n/6),
		}
	}
	n := 1024*1024 - 16
	half := hostile(n / 2)
	for name, src := range hostile(n) {
		t.Run(name, func(t *testing.T) {
			r := Analyze("<script>"+src+"</script>", Options{})
			if len(r.Errors) == 0 || r.Errors[0].Code != "script_analysis_limit" {
				t.Fatalf("%.12q…: %v", src, codes(r.Errors))
			}
			assertNoBlowup(t, name,
				func() { Analyze("<script>"+half[name]+"</script>", Options{}) },
				func() { Analyze("<script>"+src+"</script>", Options{}) })
		})
	}
}

// A large but ordinary inline script (a bundled chart library) is still
// syntax-checked, all of it, and the check's cost grows linearly with the
// script. Both are asserted deterministically: the broken copy must be
// reported at its last byte (so the whole 6 MiB was parsed, not skipped or
// cut short), and the check's allocations — counted, not timed — must double,
// not quadruple, when the script doubles. A wall-clock bound here failed
// under -race on a loaded machine while proving nothing more; the absolute
// speed of a 6 MiB analysis is tracked by BenchmarkAnalyzeLargeScript.
func TestLargeScriptIsChecked(t *testing.T) {
	line := "var a" + strings.Repeat("b", 10) + " = function (x) { return x * 2 + 1; };\n"
	lines := (6 * 1024 * 1024) / len(line)
	src := strings.Repeat(line, lines)
	if len(src) > maxScriptBytes {
		t.Fatalf("the %d-byte fixture is over maxScriptBytes (%d), so it would be skipped, not checked", len(src), maxScriptBytes)
	}
	if r := Analyze("<script>"+src+"</script>", Options{}); find(r.Errors, "script_syntax_error") != nil {
		t.Fatalf("valid script reported: %v", codes(r.Errors))
	}
	broken := Analyze("<script>"+src+"function (</script>", Options{})
	hit := find(broken.Errors, "script_syntax_error")
	if hit == nil {
		t.Fatalf("a broken 6 MiB script is not reported: %v", codes(broken.Errors))
	}
	// The error sits on the line after the last ordinary one.
	if at := fmt.Sprintf("(%d:", lines+1); !strings.Contains(hit.Message, at) {
		t.Fatalf("the error is not reported at the end of the script (want %s…): %s", at, hit.Message)
	}

	// Linear cost: the same script at 1 MiB and 2 MiB. The counts are
	// deterministic (about 0.22 allocations per byte), so the band is loose
	// only to absorb small runtime differences between Go releases; a
	// quadratic check would come out near 4x.
	cost := func(n int) (mallocs, bytes uint64) {
		doc := "<script>" + strings.Repeat(line, n/len(line)) + "</script>"
		return allocCost(func() { Analyze(doc, Options{}) })
	}
	m1, b1 := cost(1 << 20)
	m2, b2 := cost(2 << 20)
	for _, c := range []struct {
		what   string
		small  uint64
		double uint64
	}{{"allocations", m1, m2}, {"bytes allocated", b1, b2}} {
		ratio := float64(c.double) / float64(c.small)
		t.Logf("%s: 1 MiB %d, 2 MiB %d (x%.2f)", c.what, c.small, c.double, ratio)
		if ratio < 1.5 || ratio > 2.5 {
			t.Errorf("doubling the script multiplied the check's %s by %.2f, want about 2 (linear)", c.what, ratio)
		}
	}
}

func TestNetworkStorageSandbox(t *testing.T) {
	if r := raw(wrapPage("<div></div>", "fetch('/api/data').then(r=>r.json());")); find(r.Errors, "network_blocked") == nil {
		t.Error("network calls are reported")
	}
	r := raw(wrapPage("<div></div>", "const seen = localStorage.getItem('k');"))
	hit := find(r.Warnings, "opaque_origin_api")
	if hit == nil || !regexp.MustCompile(`(?i)try/catch|in-memory|module-level`).MatchString(hit.Fix) {
		t.Errorf("storage warning: %+v", r.Warnings)
	}
	tokens := parsePolicy(DefaultContentPolicy).sandboxTokens()
	r = raw(wrapPage(`<button onclick="window.print()">pdf</button>`, ""))
	flagged := false
	for _, e := range r.Errors {
		if e.Code == "sandbox_capability_missing" && e.RequiredToken == "allow-modals" {
			flagged = true
		}
	}
	if flagged != !tokens["allow-modals"] {
		t.Error("print() tracks the served sandbox")
	}
	r = raw(wrapPage("<div></div>", "window.open('/x');"))
	if !slices.ContainsFunc(r.Errors, func(e Finding) bool {
		return e.Code == "sandbox_capability_missing" && e.RequiredToken == "allow-popups"
	}) {
		t.Error("window.open must be reported")
	}
	// The rules read the policy they are given, not a second list.
	withoutModals := strings.Replace(DefaultContentPolicy, " allow-modals", "", 1)
	r = Analyze(wrapPage(`<button onclick="window.print()">pdf</button>`, ""), Options{ContentPolicy: withoutModals})
	if find(r.Errors, "sandbox_capability_missing") == nil {
		t.Error("removing allow-modals from the served policy must re-enable the finding")
	}
}

func TestCapAndClean(t *testing.T) {
	var imgs strings.Builder
	for i := 0; i < 40; i++ {
		imgs.WriteString(`<img src="https://h` + string(rune('a'+i%26)) + `.example.com/x.png">`)
	}
	r := raw(wrapPage(imgs.String(), ""))
	if len(r.Errors) != MaxPerRule || r.ErrorsOmitted == 0 {
		t.Errorf("capped: %d, omitted %d", len(r.Errors), r.ErrorsOmitted)
	}
	r = raw(wrapPage(`<button id="b">x</button>`, "document.getElementById('b').addEventListener('click', () => {});"))
	if !r.OK || len(r.Errors) != 0 || len(r.Warnings) != 0 || !strings.Contains(r.Summary, "no problems") {
		t.Errorf("clean: %+v", r)
	}
}

func TestManagedCheckIsInjected(t *testing.T) {
	html := "<html><body></body></html>"
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{apierror.BadRequest("not managed", "page_not_data_managed", nil), false},
		{apierror.BadRequest("not managed", "page_not_template_managed", nil), false},
		{errors.New("no code"), false},
		{apierror.BadRequest("#pages-data is not valid JSON", "data_block_invalid", nil), true},
	}
	for _, c := range cases {
		var saw string
		r := Analyze(html, Options{ManagedCheck: func(h string) error { saw = h; return c.err }})
		if saw != html {
			t.Error("the check receives the source")
		}
		hit := find(r.Errors, "managed_block_invalid")
		if (hit != nil) != c.want {
			t.Errorf("%v: %+v", c.err, r.Errors)
		}
		if hit != nil && (hit.ContractCode != "data_block_invalid" || hit.Message != "The Pages-managed blocks do not satisfy their own contract: #pages-data is not valid JSON") {
			t.Errorf("finding: %+v", hit)
		}
	}
}

func bentoDeckHTML() string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><title>Deck</title>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline' blob:; connect-src 'none'">
<script id="fleet-offline-deck">(function(){try{localStorage.setItem("bento-offline","on")}catch(e){}})();</script>
<script type="application/bento+json" id="bento-doc">{"format":"bento/slides","version":1,"slides":[]}</script>
<script id="bento-rt" type="bento/deflate-b64">c8vPz1EoLSjOSczJyU9V0FIoLSktKSpJUUhJVEgQMAAA</script>
</head><body><div id="bento-splash"></div><script>(async()=>{var js="";var url=URL.createObjectURL(new Blob([js],{type:"text/javascript"}));await import(url)})()</script></body></html>`
}

func TestSwitcherReaders(t *testing.T) {
	html := `<!doctype html><html><head></head><body><script>
    const nav = document.getElementById("pages-nav");
    if (nav) render(JSON.parse(nav.textContent));
  </script></body></html>`
	for _, mode := range []string{"raw", "themed"} {
		r := Analyze(html, Options{RenderMode: mode})
		if !r.OK || r.RenderMode != mode {
			t.Errorf("%s: %+v", mode, r)
		}
		for _, w := range r.Warnings {
			if strings.HasPrefix(w.Code, "nav_block_") {
				t.Errorf("%s: a supported reader is not a defect", mode)
			}
		}
	}
	deck := strings.Replace(bentoDeckHTML(), "</body>", `<script>document.querySelector("#pages-nav");</script></body>`, 1)
	r := Analyze(deck, Options{})
	g := find(r.Warnings, "nav_block_ignored")
	if g == nil || !strings.Contains(g.Message, "Bento deck") || !regexp.MustCompile(`even (?:in a portal|when a portal)`).MatchString(g.Message) ||
		!strings.Contains(g.Fix, "raw") || strings.Contains(g.Fix, "themed") || !r.OK {
		t.Errorf("deck reader guidance: %+v", r)
	}
	if strings.Contains(render.RenderVersion(render.Renderable{HTML: deck, RenderMode: "raw", Nav: struct{}{}}), `id="pages-nav"`) {
		t.Error("deck received a payload")
	}
	for _, h := range []string{
		"<html><body><p>no switcher here</p></body></html>",
		"<html><head><style>/* Pages injects #pages-nav in a portal. */</style></head><body></body></html>",
		"<html><body><p>Our handbook explains pages-nav.</p></body></html>",
	} {
		for _, mode := range []string{"raw", "themed"} {
			for _, w := range Analyze(h, Options{RenderMode: mode}).Warnings {
				if strings.HasPrefix(w.Code, "nav_block_") {
					t.Errorf("prose mention warned: %q", h)
				}
			}
		}
	}
}

func TestBentoDeck(t *testing.T) {
	r := Analyze(bentoDeckHTML(), Options{RenderMode: "raw"})
	if !r.OK || !slices.Equal(codes(r.Warnings), []string{"bento_deck"}) {
		t.Fatalf("%+v", r)
	}
	for _, want := range []string{"compressed", "editor", "Nothing edited there is saved to Pages"} {
		if !strings.Contains(r.Warnings[0].Message, want) {
			t.Errorf("deck message lacks %q", want)
		}
	}
	if !slices.Contains(r.Checks, "bento_deck") {
		t.Error("checks")
	}
	themed := Analyze(bentoDeckHTML(), Options{RenderMode: "themed"})
	if themed.OK || find(themed.Errors, "bento_deck_themed") == nil {
		t.Error("themed deck is an error")
	}
	withKeys := strings.Replace(bentoDeckHTML(), `{"format":"bento/slides","version":1,"slides":[]}`,
		`{"format":"bento/slides","version":1,"slides":[],"collab":{"room":"wss://sync.bento.page/d/abc","key":"k"}}`, 1)
	keyed := Analyze(withKeys, Options{})
	if !slices.Equal(codes(keyed.Warnings), []string{"bento_deck", "bento_deck_collab_keys"}) || !strings.Contains(keyed.Warnings[1].Fix, "bento_doc.py") {
		t.Errorf("collab: %+v", keyed.Warnings)
	}
	plain := Analyze("<html><head></head><body><script>localStorage.getItem('x')</script></body></html>", Options{})
	if find(plain.Warnings, "opaque_origin_api") == nil || find(plain.Warnings, "bento_deck") != nil {
		t.Error("ordinary page untouched by the deck rule")
	}
}

func TestDefaultPolicyMatchesServedShape(t *testing.T) {
	// Pinned against lib/csp.js's rawHeaders(): every directive a rule reads.
	p := parsePolicy(DefaultContentPolicy)
	for directive, want := range map[string]string{
		"script-src": "'self' 'unsafe-inline' blob:", "img-src": "'self' data: blob:", "font-src": "'self' data:",
		"media-src": "data: blob:", "connect-src": "'none'", "default-src": "'none'",
		"sandbox": "allow-scripts allow-downloads allow-modals",
	} {
		if got := strings.Join(p[directive], " "); got != want {
			t.Errorf("%s = %q, want %q", directive, got, want)
		}
	}
	if p["sandbox"] == nil || slices.Contains(p["sandbox"], "allow-same-origin") {
		t.Error("allow-same-origin is a sandbox escape")
	}
}

func TestShadowedNamesGenerated(t *testing.T) {
	if len(shadowedNames) != 506 {
		t.Errorf("%d names; regenerate with internal/genshadowed", len(shadowedNames))
	}
	for _, n := range []string{"togglePopover", "close", "submit", "reset", "remove"} {
		if _, ok := shadowedNames[n]; !ok {
			t.Errorf("%s missing", n)
		}
	}
	for n := range shadowedNames {
		if strings.HasPrefix(n, "on") && len(n) > 2 && n[2] >= 'a' && n[2] <= 'z' || n == "constructor" {
			t.Errorf("%s should be filtered", n)
		}
	}
}
