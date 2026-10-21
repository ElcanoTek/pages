// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package csp

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The dashboard host's security headers. lib/csp.js used to hand these options
// to helmet 8.3.0; it now writes the same set itself. The expectations below
// were recorded from helmet's middleware for Pages' options, so any drift in a
// directive, its order, or a companion header fails here.

const auth = "https://auth.northwind.example"

var common = map[string]string{
	"Cross-Origin-Opener-Policy":        "same-origin",
	"Cross-Origin-Resource-Policy":      "same-origin",
	"Origin-Agent-Cluster":              "?1",
	"Referrer-Policy":                   "no-referrer",
	"X-Content-Type-Options":            "nosniff",
	"X-DNS-Prefetch-Control":            "off",
	"X-Download-Options":                "noopen",
	"X-Frame-Options":                   "SAMEORIGIN",
	"X-Permitted-Cross-Domain-Policies": "none",
	"X-XSS-Protection":                  "0",
}

func setOrigins(t *testing.T, dash, content string) {
	t.Helper()
	t.Setenv("DASHBOARD_HOST", "")
	t.Setenv("CONTENT_HOST", "")
	t.Setenv("DASHBOARD_ORIGIN", dash)
	t.Setenv("CONTENT_ORIGIN", content)
}

// middlewareHeaders runs the middleware like the JS test drove helmet's: it
// records what was set and that X-Powered-By is removed.
func middlewareHeaders(t *testing.T) http.Header {
	t.Helper()
	mw, err := ShellHeadersMiddleware(auth)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Powered-By", "Express")
	mw(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Header().Get("X-Powered-By") != "" {
		t.Fatal("X-Powered-By must be removed")
	}
	return rec.Header()
}

func want(extra map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range common {
		m[k] = v
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func checkSet(t *testing.T, got http.Header, hs Headers, expect map[string]string) {
	t.Helper()
	if !reflect.DeepEqual(hs.Map(), expect) {
		t.Fatalf("headers:\n got %#v\nwant %#v", hs.Map(), expect)
	}
	if len(got) != len(expect) {
		t.Fatalf("middleware set %d headers, want %d: %v", len(got), len(expect), got)
	}
	for k, v := range expect {
		if got.Get(k) != v {
			t.Fatalf("middleware %s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestShellHeadersHTTPSMatchesHelmet(t *testing.T) {
	setOrigins(t, "https://pages.northwind.example", "https://northwind-pages.example")
	hs, err := ShellHeaders(auth)
	if err != nil {
		t.Fatal(err)
	}
	checkSet(t, middlewareHeaders(t), hs, want(map[string]string{
		"Content-Security-Policy": "default-src 'self';base-uri 'self';font-src 'self';form-action 'self' https://auth.northwind.example;" +
			"frame-ancestors 'none';img-src 'self' data:;object-src 'none';script-src 'self';script-src-attr 'none';" +
			"style-src 'self' 'unsafe-inline';upgrade-insecure-requests;frame-src https://northwind-pages.example;" +
			"connect-src 'self'",
		"Strict-Transport-Security": "max-age=31536000; includeSubDomains",
	}))
}

func TestShellHeadersPlainHTTPDropsHSTSAndUpgrade(t *testing.T) {
	setOrigins(t, "http://localhost:3099", "http://content.localhost:3099")
	hs, err := ShellHeaders(auth)
	if err != nil {
		t.Fatal(err)
	}
	checkSet(t, middlewareHeaders(t), hs, want(map[string]string{
		"Content-Security-Policy": "default-src 'self';base-uri 'self';font-src 'self';form-action 'self' https://auth.northwind.example;" +
			"frame-ancestors 'none';img-src 'self' data:;object-src 'none';script-src 'self';script-src-attr 'none';" +
			"style-src 'self' 'unsafe-inline';frame-src http://content.localhost:3099;connect-src 'self'",
	}))
}

func TestShellHeadersRefuseSmuggledDirective(t *testing.T) {
	setOrigins(t, "", "")
	if _, err := ShellHeaders("https://a.example; script-src *"); err == nil ||
		!strings.Contains(err.Error(), `invalid directive value for "form-action"`) {
		t.Fatalf("want form-action refusal, got %v", err)
	}
	if _, err := ShellHeadersMiddleware("https://a.example; script-src *"); err == nil {
		t.Fatal("middleware must refuse too")
	}
	t.Setenv("CONTENT_ORIGIN", "https://a.example,https://b.example")
	if _, err := ShellHeaders("https://auth.example"); err == nil ||
		!strings.Contains(err.Error(), `invalid directive value for "frame-src"`) {
		t.Fatalf("want frame-src refusal, got %v", err)
	}
}

func TestSandboxKeepsOpaqueOrigin(t *testing.T) {
	setOrigins(t, "", "")
	tokens := SandboxTokens()
	if !reflect.DeepEqual(tokens, []string{"allow-scripts", "allow-downloads", "allow-modals"}) {
		t.Fatalf("sandbox tokens %v", tokens)
	}
	for _, tok := range tokens {
		if tok == "allow-same-origin" {
			t.Fatal("allow-scripts + allow-same-origin lets the page escape the sandbox")
		}
	}
	csp := RawHeaders().Get("Content-Security-Policy")
	for _, re := range []string{`connect-src 'none'`, `img-src 'self' data: blob:`, `font-src 'self' data:`,
		`script-src 'self' 'unsafe-inline' blob:`, `media-src data: blob:`} {
		if !strings.Contains(csp, re) {
			t.Errorf("raw CSP lacks %q: %s", re, csp)
		}
	}
}

func TestRawHeadersExact(t *testing.T) {
	setOrigins(t, "", "")
	t.Setenv("DASHBOARD_HOST", "pages.elcanotek.com")
	t.Setenv("CONTENT_HOST", "elcano-pages.com")
	got := RawHeaders()
	expect := Headers{
		{"Content-Security-Policy", "sandbox allow-scripts allow-downloads allow-modals; default-src 'none'; " +
			"script-src 'self' 'unsafe-inline' blob:; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
			"media-src data: blob:; font-src 'self' data:; connect-src 'none'; form-action 'none'; base-uri 'none'; " +
			"object-src 'none'; frame-ancestors https://pages.elcanotek.com"},
		{"Referrer-Policy", "no-referrer"},
		{"X-Content-Type-Options", "nosniff"},
		{"X-Robots-Tag", "noindex, noarchive"},
		{"Cache-Control", "no-store"},
	}
	if !reflect.DeepEqual(got, expect) {
		t.Fatalf("raw headers\n got %#v\nwant %#v", got, expect)
	}
}

func TestRawEditHeadersOpenOnlyConnectSrc(t *testing.T) {
	setOrigins(t, "", "")
	t.Setenv("CONTENT_HOST", "elcano-pages.com")
	view := RawHeaders().Get("Content-Security-Policy")
	edit := RawEditHeaders().Get("Content-Security-Policy")
	origin := "https://elcano-pages.com"
	if !regexp.MustCompile(`connect-src ` + regexp.QuoteMeta(origin) + `(;|$)`).MatchString(edit) {
		t.Fatalf("an editor's deck can reach Pages: %s", edit)
	}
	if strings.Contains(edit, "connect-src 'none'") {
		t.Fatal("edit must not keep connect-src 'none'")
	}
	if strings.Replace(view, "connect-src 'none'", "connect-src "+origin, 1) != edit {
		t.Fatal("everything else must be the same policy")
	}
	if RawHeaders().Get("Content-Security-Policy") != view {
		t.Fatal("RawEditHeaders must not mutate RawHeaders")
	}
}

func TestGateAndBaseHeaders(t *testing.T) {
	setOrigins(t, "http://localhost:3099", "http://content.localhost:3099")
	if got := GateHeaders(GateOptions{}).Get("Content-Security-Policy"); got !=
		"default-src 'none'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'" {
		t.Fatalf("gate csp %q", got)
	}
	if got := GateHeaders(GateOptions{Framable: true}).Get("Content-Security-Policy"); !strings.HasSuffix(got,
		"frame-ancestors http://localhost:3099 http://127.0.0.1:3099") {
		t.Fatalf("framable gate csp %q", got)
	}
	base := ContentBaseHeaders()
	if base.Get("Content-Security-Policy") != "default-src 'none'" || base.Get("Cache-Control") != "no-store" ||
		base.Get("X-Robots-Tag") != "noindex, noarchive" || len(base) != 5 {
		t.Fatalf("base headers %v", base)
	}
}

func TestFrameAncestors(t *testing.T) {
	for _, tc := range []struct{ dash, want string }{
		{"https://pages.northwind.example", "https://pages.northwind.example"},
		{"http://localhost:3099", "http://localhost:3099 http://127.0.0.1:3099"},
		{"http://127.0.0.1:3099", "http://127.0.0.1:3099 http://localhost:3099"},
		{"http://localhost", "http://localhost http://127.0.0.1"},
		{"http://localhost:80", "http://localhost:80 http://127.0.0.1"},
		{"http://contoso.test:8080", "http://contoso.test:8080"},
		{"http://", "http://"},
	} {
		t.Setenv("DASHBOARD_ORIGIN", tc.dash)
		if got := FrameAncestors(); got != tc.want {
			t.Errorf("FrameAncestors(%q) = %q, want %q", tc.dash, got, tc.want)
		}
	}
}
