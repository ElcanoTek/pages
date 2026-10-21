// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package shell

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The goldens in testdata/ were captured from lib/*shell.js and
// lib/standalone-chrome.js under node for the same inputs. The front end and
// the browser suite key off exact ids, classes and data attributes, so the Go
// output must match byte for byte.
const (
	nasty = `a"<b>&'c</script>`
	weird = "line\u2028sep\u2029x\b\f\n\r\t\u0001\u001f\\\"é😀"
)

func goldenCases() map[string]string {
	return map[string]string{
		"admin_basic":            Admin("northwind/q2-report", "csrf.tok-1", "ops@example.com", "https://content.example.com"),
		"admin_nasty":            Admin(nasty, weird, `o'"<x>@example.com`, ""),
		"templates_basic":        Templates("csrf.tok-2", "ops@example.com", "https://content.example.com"),
		"templates_empty_origin": Templates("tok", "a&b@example.com", ""),
		"portals_basic":          Portals("csrf.tok-3", "ops@example.com", "https://content.example.com"),
		"welcome_compose":        Welcome("ops@example.com", "https://content.example.com", WelcomeOptions{CSRF: "csrf.tok-4", Compose: true}),
		"welcome_default":        Welcome("ops@example.com", "", WelcomeOptions{}),
		"not_found":              NotFound(),
		"request_error_400":      RequestError(400),
		"request_error_499":      RequestError(499),
		"request_error_500":      RequestError(500),
		"request_error_503":      RequestError(503),
		"standalone_minimal":     Standalone(StandaloneOptions{Title: "T <1>", Heading: "H & h"}),
		"standalone_full": Standalone(StandaloneOptions{
			Title: "Fabrikam", Kicker: "Kick'er", Heading: `Contoso "q"`, BodyHTML: "<p>raw <b>body</b></p>",
			AssetsBase: "/x/flag", CTA: &CTA{Href: `/a?b=1&c="2"`, Label: "<Go>"},
		}),
		"frag_header":          Header(nasty),
		"frag_icon_default":    Icon("sun", ""),
		"frag_icon_class":      Icon(nasty, "c1 c2"),
		"frag_loading_default": Loading(""),
		"frag_loading_label":   Loading(nasty),
		"frag_nav_pages":       SectionNav(SectionPages),
		"frag_nav_templates":   SectionNav(SectionTemplates),
		"frag_nav_portals":     SectionNav(SectionPortals),
		"frag_nav_none":        SectionNav("other"),
		"frag_breadcrumb":      Breadcrumb(nasty),
		"frag_title":           PageTitle("Client pages"),
		"frag_head_noscripts":  Head(nasty, Bootstrap{{"s", weird}, {"b", false}, {"n", nil}}, nil),
		"frag_head_scripts":    Head("x", Bootstrap{}, []string{"/a.js", nasty}),
	}
}

func TestGoldens(t *testing.T) {
	cases := goldenCases()
	files, err := filepath.Glob("testdata/*.html")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(cases) {
		t.Errorf("%d goldens on disk, %d cases", len(files), len(cases))
	}
	for name, got := range cases {
		t.Run(name, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("testdata", name+".html"))
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("output differs from the JS golden\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

func TestEsc(t *testing.T) {
	if got := Esc(`&<>"'x`); got != "&amp;&lt;&gt;&quot;&#39;x" {
		t.Fatalf("Esc = %q", got)
	}
}

// unit.test.js: "content host: nothing a client can reach calls this an internal tool".
func TestNothingCallsPagesInternal(t *testing.T) {
	html := NotFound()
	if strings.Contains(html, "Elcano Internal") {
		t.Error("the dashboard-host 404 still calls Pages an internal tool")
	}
	if !strings.Contains(html, "Elcano") {
		t.Error("the dashboard-host 404 lost its branding altogether")
	}
	header := Header("qa@example.com")
	if strings.Contains(header, "Elcano Internal") {
		t.Error("admin header still calls Pages an internal tool")
	}
	if !strings.Contains(header, `ds-app-header__title">Pages<`) {
		t.Error("admin header does not name the tool")
	}
}

// unit.test.js: "content host: one chrome, one favicon, one 404" (the
// dashboard-host half; the gate half lives with contentview).
func TestOneChrome(t *testing.T) {
	for _, tc := range []struct{ name, html, base string }{
		{"404", NotFound(), "/shell-assets/flag"},
		{"default base", Standalone(StandaloneOptions{Title: "t", Heading: "h"}), "/assets/flag"},
	} {
		if !strings.Contains(tc.html, `<link rel="icon" href="`+tc.base+`/logos/elcano-mark-favicon.svg"`) {
			t.Errorf("%s has no favicon", tc.name)
		}
		if !strings.Contains(tc.html, tc.base+"/tokens/design-tokens.css") {
			t.Errorf("%s points at the wrong asset base", tc.name)
		}
		if !strings.Contains(tc.html, "min-height:100dvh") || strings.Contains(tc.html, "min-height:100vh") {
			t.Errorf("%s must size on dvh, not vh", tc.name)
		}
		if !regexp.MustCompile(`h1\{[^}]*overflow-wrap:anywhere`).MatchString(tc.html) {
			t.Errorf("%s lets a long heading widen the card", tc.name)
		}
	}
	if !strings.Contains(NotFound(), `href="/admin"`) {
		t.Error("the dashboard 404 lost its CTA")
	}
	if strings.Contains(Standalone(StandaloneOptions{Title: "t", Heading: "h"}), `href="/admin"`) {
		t.Error("a CTA-less page links to /admin")
	}
	if StandaloneCSS == "" {
		t.Error("the sheet is exported so both hosts share one copy")
	}
}

// unit.test.js: "content host: no word on a partner-facing page is one only
// we would use" (the dashboard-host 404 entry).
func TestNoInternalWordsOnDashboard404(t *testing.T) {
	ourWords := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bunpublished\b`),
		regexp.MustCompile(`(?i)\bthis host\b`),
		regexp.MustCompile(`(?i)\bshort-lived\b`),
		regexp.MustCompile(`(?i)\brendered\b`),
		regexp.MustCompile(`(?i)\bElcano-only\b`),
		regexp.MustCompile(`(?i)\bsandbox`),
		regexp.MustCompile(`(?i)\bpayload\b`),
		regexp.MustCompile(`(?i)\blifecycle\b`),
		regexp.MustCompile(`(?i)\bversion \d`),
		regexp.MustCompile(`(?i)\bslug\b`),
		regexp.MustCompile(`(?i)\btemplate revision\b`),
		regexp.MustCompile(`\b404\b`),
	}
	for name, html := range map[string]string{"404": NotFound(), "400": RequestError(400), "500": RequestError(500)} {
		prose := regexp.MustCompile(`(?s)<style>.*?</style>`).ReplaceAllString(html, "")
		prose = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(prose, " ")
		prose = regexp.MustCompile(`\s+`).ReplaceAllString(prose, " ")
		for _, re := range ourWords {
			if m := re.FindString(prose); m != "" {
				t.Errorf("%s says %q", name, m)
			}
		}
	}
}

func TestBootstrapIslandCannotCloseScript(t *testing.T) {
	out := Head("t", Bootstrap{{"slug", "</script><script>alert(1)</script>"}}, nil)
	if strings.Count(out, "</script>") != 3 { // theme controller, island, primitives
		t.Fatalf("bootstrap value escaped its <script>: %s", out)
	}
}
