// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package server

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/shell"
	"github.com/ElcanoTek/pages/internal/tokens"
)

const pageHTML = `<!doctype html><html><head><title>Northwind</title></head><body><h1>Q2 northwind-secret</h1></body></html>`

func TestNewRequiresDB(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without a DB must fail")
	}
}

// The two zones never serve each other's routes, and the content host never
// reads the SSO cookie.
func TestHostSplit(t *testing.T) {
	e := setup(t, nil)
	e.publish("northwind", pageHTML)

	// Dashboard routes do not exist on the content host.
	for _, p := range []string{"/admin", "/admin/northwind", "/api/v1/pages", "/api/me", "/view/northwind", "/shell-assets/admin.js", "/mcp", "/upload/x", "/export/a/b"} {
		r := e.get(p, opt{host: contentHost, cookie: e.adminCk})
		if r.status == http.StatusOK || strings.Contains(r.body, "pages-bootstrap") || r.header.Get("Content-Type") == "application/json; charset=utf-8" {
			t.Errorf("content host %s = %d %q (dashboard route leaked)", p, r.status, r.header.Get("Content-Type"))
		}
		if strings.Contains(r.header.Get("Content-Security-Policy"), "upgrade-insecure-requests") {
			t.Errorf("content host %s carries the dashboard shell CSP", p)
		}
	}
	// Content routes do not exist on the dashboard host.
	for _, p := range []string{"/raw/northwind", "/northwind", "/assets/flag/tokens/design-tokens.css", "/raw-template/1", "/favicon.ico"} {
		r := e.get(p, opt{cookie: e.adminCk})
		if r.status != http.StatusNotFound || r.body != shell.NotFound() {
			t.Errorf("dashboard %s = %d, want the shell 404", p, r.status)
		}
	}
	// The content host renders an Elcano-only page to nobody, admin SSO
	// cookie or not: it never looks at that cookie.
	for _, ck := range []string{"", e.adminCk} {
		r := e.get("/northwind", opt{host: contentHost, cookie: ck})
		if r.status != http.StatusForbidden || strings.Contains(r.body, "northwind-secret") {
			t.Fatalf("content /northwind with cookie %v = %d", ck != "", r.status)
		}
	}
	// Host decides, never X-Forwarded-Host; the alias and a port still route.
	t.Setenv("CONTENT_HOST_ALSO", "content-alias.test")
	if r := e.get("/raw/northwind", opt{headers: map[string]string{"X-Forwarded-Host": contentHost}}); r.status != 404 {
		t.Fatalf("dashboard Host with X-Forwarded-Host content = %d", r.status)
	}
	if r := e.get("/raw/northwind", opt{host: "CONTENT-ALIAS.TEST:443", headers: map[string]string{"X-Forwarded-Host": dashHost}}); r.status != 403 {
		t.Fatalf("content alias = %d", r.status)
	}
	if r := e.get("/healthz", opt{host: contentHost}); r.status != 200 || r.body != "ok" || r.header.Get("X-Robots-Tag") == "" {
		t.Fatalf("content /healthz = %d %q", r.status, r.body)
	}
	if r := e.get("/readyz", opt{host: contentHost}); r.status != 200 {
		t.Fatalf("content /readyz = %d", r.status)
	}
}

// Every dashboard response carries the shell headers — 404s, errors, the
// early mounts and redirects included — and none carries X-Powered-By.
func TestShellHeadersEverywhere(t *testing.T) {
	e := setup(t, nil)
	cases := []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/healthz", "", 200},
		{"GET", "/readyz", "", 200},
		{"GET", "/nope", "", 404},
		{"GET", "/", "", 302},
		{"GET", "/admin", "", 302},
		{"GET", "/api/v1/pages", "", 401},
		{"POST", "/api/v1/pages", "{bad", 400},
		{"POST", "/admin", "{bad", 400},
		{"GET", "/api/v1/nope", "", 401},
		{"GET", "/shell-assets/admin.js", "", 200},
		{"GET", "/shell-assets/flag/tokens/design-tokens.css", "", 200},
		{"GET", "/shell-assets/nope.js", "", 404},
		{"PUT", "/upload/nope", "x", 0},
		{"GET", "/export/a/b", "", 0},
		{"POST", "/mcp", "{}", 503},
		{"GET", "/logout", "", 303},
		{"DELETE", "/admin", "", 404},
	}
	want, err := shellHeaderNames()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		r := e.do(c.method, c.path, opt{body: c.body})
		if c.status != 0 && r.status != c.status {
			t.Errorf("%s %s = %d, want %d (%s)", c.method, c.path, r.status, c.status, r.body)
		}
		for _, h := range want {
			if r.header.Get(h) == "" {
				t.Errorf("%s %s: missing %s", c.method, c.path, h)
			}
		}
		if r.header.Get("X-Powered-By") != "" {
			t.Errorf("%s %s: X-Powered-By sent", c.method, c.path)
		}
	}
	if r := e.get("/healthz", opt{host: contentHost}); r.header.Get("X-Powered-By") != "" {
		t.Error("content host sent X-Powered-By")
	}
}

func shellHeaderNames() ([]string, error) {
	return []string{"Content-Security-Policy", "X-Frame-Options", "Referrer-Policy", "X-Content-Type-Options", "Cross-Origin-Opener-Policy", "Strict-Transport-Security"}, nil
}

// The HTML shells and their assets (test/admin.integration.js steps 9-9c).
func TestShells(t *testing.T) {
	e := setup(t, nil)
	e.publish("adm", pageHTML)
	admin := opt{cookie: e.adminCk}

	sh := e.get("/admin/adm", admin)
	if sh.status != 200 || sh.header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("/admin/adm = %d", sh.status)
	}
	for _, re := range []string{
		`id="pages-bootstrap"`, `id="page-switcher"[^>]+aria-busy="true"`,
		`id="page-switcher-select"[^>]+aria-describedby="page-switcher-count"[^>]+disabled`,
		`/shell-assets/page-switcher\.js`, `/shell-assets/primitives\.js`, `/shell-assets/admin\.js`,
		`shell-assets/flag/tokens/design-tokens\.css`, `shell-assets/flag/logos/elcano-mark-primary\.svg`,
	} {
		if !regexp.MustCompile(re).MatchString(sh.body) {
			t.Errorf("admin shell lacks %s", re)
		}
	}
	if strings.Contains(sh.body, "refresh-ui.js") {
		t.Error("admin shell loads the retired scheduler UI")
	}
	for _, p := range []string{"/admin", "/admin/welcome"} {
		l := e.get(p, admin)
		if l.status != 200 || !strings.Contains(l.body, "/shell-assets/welcome.js") || !regexp.MustCompile(`"csrf":"[^"]+"`).MatchString(l.body) {
			t.Errorf("%s = %d (welcome shell)", p, l.status)
		}
	}
	if w := e.get("/shell-assets/welcome.js", opt{}); w.status != 200 || !strings.Contains(w.body, "function filterWorkspacePages") ||
		regexp.MustCompile(`window\.(?:prompt|confirm|alert)`).MatchString(w.body) {
		t.Errorf("welcome.js = %d", w.status)
	}
	if r := e.get("/shell-assets/refresh-ui.js", opt{}); r.status != 404 {
		t.Errorf("retired refresh-ui.js = %d", r.status)
	}
	if a := e.get("/shell-assets/admin.js", opt{}); a.status != 200 || regexp.MustCompile(`Run update now|/refresh/run|data-refresh-confirm`).MatchString(a.body) {
		t.Errorf("admin.js = %d", a.status)
	}
	lib := e.get("/admin/templates", admin)
	if lib.status != 200 || !strings.Contains(lib.body, "/shell-assets/templates.js") || !strings.Contains(lib.body, `"csrf":"`) {
		t.Errorf("/admin/templates = %d", lib.status)
	}
	por := e.get("/admin/portals", admin)
	if por.status != 200 || !strings.Contains(por.body, "/shell-assets/portals.js") ||
		!regexp.MustCompile(`context-tab[^>]*href="/admin/portals"[^>]*aria-current="page"`).MatchString(por.body) {
		t.Errorf("/admin/portals = %d", por.status)
	}
	if p := e.get("/shell-assets/portals.js", opt{}); p.status != 200 || !strings.Contains(p.body, "function describeMember") {
		t.Errorf("portals.js = %d", p.status)
	}
	// Nested slugs resolve in the shell.
	if n := e.get("/admin/adm/nest", admin); n.status != 200 || !strings.Contains(n.body, `adm/nest`) {
		t.Errorf("/admin/adm/nest = %d", n.status)
	}
	// Anonymous → sign-in redirect; a signed-in outsider → 403.
	for _, p := range []string{"/admin", "/admin/welcome", "/admin/adm", "/admin/templates", "/admin/portals"} {
		if r := e.get(p, opt{}); r.status != 302 || !strings.HasPrefix(r.header.Get("Location"), "https://auth.elcanotek.com") {
			t.Errorf("anon %s = %d %s", p, r.status, r.header.Get("Location"))
		}
		if r := e.get(p, opt{cookie: e.outCk}); r.status != 403 {
			t.Errorf("outsider %s = %d", p, r.status)
		}
	}
	// A malformed escape in a splat is Express's 400, as the branded page.
	if r := e.get("/admin/%E0%A4", admin); r.status != 400 || r.body != shell.RequestError(400) {
		t.Errorf("bad escape = %d", r.status)
	}
}

// Body parser failures: the JSON envelope on /api/v1, the branded page
// elsewhere; both before authentication.
func TestBodyParserErrorContract(t *testing.T) {
	e := setup(t, func() { t.Setenv("MAX_HTML_BYTES", "1kb") })
	big := `{"html":"` + strings.Repeat("x", 2048) + `"}`
	for _, c := range []struct {
		path, body string
		status     int
		code       string
	}{
		{"/api/v1/pages", "{not json", 400, "bad_json"},
		{"/API/V1/pages", "{not json", 400, "bad_json"},
		{"/api/v1/pages", big, 413, "body_too_large"},
		{"/api/v1/admin/pages", "{not json", 400, "bad_json"},
	} {
		r := e.do("POST", c.path, opt{body: c.body})
		if r.status != c.status || r.header.Get("Content-Type") != "application/json; charset=utf-8" {
			t.Fatalf("%s = %d %s", c.path, r.status, r.body)
		}
		if m := r.json(t); m["code"] != c.code || len(m) != 2 {
			t.Fatalf("%s body = %s", c.path, r.body)
		}
	}
	for _, c := range []struct {
		path, body string
		status     int
	}{
		{"/admin", "{not json", 400},
		{"/view/x", big, 413},
		{"/nope", "{not json", 400},
	} {
		r := e.do("POST", c.path, opt{body: c.body})
		if r.status != c.status || r.body != shell.RequestError(c.status) {
			t.Fatalf("%s = %d %s", c.path, r.status, r.body)
		}
	}
	// A form over the 64 KiB urlencoded cap is a 413 too.
	if r := e.do("POST", "/admin", opt{body: "a=" + strings.Repeat("b", 70*1024), contentType: "application/x-www-form-urlencoded"}); r.status != 413 {
		t.Fatalf("big form = %d", r.status)
	}
}

func TestViewBroker(t *testing.T) {
	e := setup(t, nil)
	e.publish("northwind/q2", pageHTML)
	r := e.get("/view/northwind/q2", opt{cookie: e.adminCk})
	if r.status != 302 {
		t.Fatalf("broker = %d %s", r.status, r.body)
	}
	loc, err := url.Parse(r.header.Get("Location"))
	if err != nil || loc.Scheme+"://"+loc.Host+loc.Path != "https://content.localhost/northwind/q2" {
		t.Fatalf("broker Location = %s", r.header.Get("Location"))
	}
	c := rawtoken.Verify(loc.Query().Get("t"))
	if c == nil || c.Purpose != rawtoken.Session {
		t.Fatalf("broker token = %+v", c)
	}
	// The minted token is redeemable on the content host.
	if x := e.get("/northwind/q2?t="+url.QueryEscape(loc.Query().Get("t")), opt{host: contentHost}); x.status != 302 || !strings.Contains(strings.Join(x.header.Values("Set-Cookie"), "\n"), "pgs") {
		t.Fatalf("content exchange = %d %v", x.status, x.header.Values("Set-Cookie"))
	}
	if r := e.get("/view/northwind/q2", opt{}); r.status != 302 || !strings.HasPrefix(r.header.Get("Location"), "https://auth.elcanotek.com") {
		t.Fatalf("anon broker = %d", r.status)
	}
	if r := e.get("/view/northwind/q2", opt{cookie: e.outCk}); r.status != 403 {
		t.Fatalf("outsider broker = %d", r.status)
	}
	if r := e.get("/view/missing", opt{cookie: e.adminCk}); r.status != 404 || r.body != shell.RequestError(404) {
		t.Fatalf("missing page = %d %s", r.status, r.body)
	}
	if _, err := e.s.cfg.DB.Exec(context.Background(), `INSERT INTO pages(slug,title) VALUES('empty','Empty')`); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/view/empty", opt{cookie: e.adminCk}); r.status != 404 || r.body != "nothing published yet" || r.header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Fatalf("unpublished = %d %q", r.status, r.body)
	}
}

func TestMeRootLogout(t *testing.T) {
	e := setup(t, func() { t.Setenv("AUTH_LOGIN_URL", "https://auth.northwind.example/") })
	me := e.get("/api/me", opt{cookie: e.adminCk})
	if me.status != 200 || me.body != `{"email":"admin@elcanotek.com","tenant":"","admin":true}` {
		t.Fatalf("/api/me = %d %s", me.status, me.body)
	}
	if r := e.get("/api/me", opt{cookie: e.outCk}); r.body != `{"email":"outsider@example.com","tenant":"","admin":false}` {
		t.Fatalf("/api/me outsider = %s", r.body)
	}
	if r := e.get("/api/me", opt{}); r.status != 302 {
		t.Fatalf("/api/me anon = %d", r.status)
	}
	if r := e.get("/", opt{}); r.status != 302 || r.header.Get("Location") != "/admin" {
		t.Fatalf("/ = %d %s", r.status, r.header.Get("Location"))
	}
	for _, m := range []string{"GET", "POST", "DELETE"} {
		if r := e.do(m, "/logout", opt{}); r.status != 303 || r.header.Get("Location") != "https://auth.northwind.example/logout" {
			t.Fatalf("%s /logout = %d %s", m, r.status, r.header.Get("Location"))
		}
	}
	if r := e.get("/logout", opt{host: contentHost}); r.status == 303 {
		t.Fatal("content host has a /logout handoff")
	}
}

func TestDevLoginGating(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		e := setup(t, func() { t.Setenv("DEV_ADMIN_COOKIE", "x") })
		if r := e.get("/__dev/login", opt{}); r.status != 404 {
			t.Fatalf("/__dev/login without PAGES_DEV_LOGIN = %d", r.status)
		}
	})
	t.Run("production", func(t *testing.T) {
		e := setup(t, func() {
			t.Setenv("PAGES_DEV_LOGIN", "1")
			t.Setenv("DEV_ADMIN_COOKIE", "x")
			t.Setenv("PAGES_ENV", "production")
		})
		if r := e.get("/__dev/login", opt{}); r.status != 404 {
			t.Fatalf("/__dev/login in production = %d", r.status)
		}
	})
	t.Run("on", func(t *testing.T) {
		e := setup(t, func() {
			t.Setenv("PAGES_DEV_LOGIN", "1")
			t.Setenv("DEV_ADMIN_COOKIE", "dev-cookie-value")
		})
		r := e.get("/__dev/login?next=%2Fadmin%2Fadm", opt{})
		if r.status != 302 || r.header.Get("Location") != "/admin/adm" ||
			r.header.Get("Set-Cookie") != "elcano_auth=dev-cookie-value; Path=/; HttpOnly; SameSite=Lax" {
			t.Fatalf("/__dev/login = %d %s %s", r.status, r.header.Get("Location"), r.header.Get("Set-Cookie"))
		}
		if r := e.get("/admin/adm?x=1", opt{}); r.status != 302 || r.header.Get("Location") != "/__dev/login?next=%2Fadmin%2Fadm%3Fx%3D1" {
			t.Fatalf("intercept = %d %s", r.status, r.header.Get("Location"))
		}
		if r := e.get("/admin/adm", opt{cookie: e.adminCk}); r.status != 200 {
			t.Fatalf("signed-in admin under dev login = %d", r.status)
		}
	})
}

// test/ratelimit.integration.js: the API limiter runs before the body
// parser; the content limiter answers a person with the hardened HTML page.
func TestRateLimitOrdering(t *testing.T) {
	e := setup(t, func() {
		t.Setenv("RL_API_PER_MIN", "3")
		t.Setenv("RL_CONTENT_PER_MIN", "2")
	})
	if r := e.do("POST", "/api/v1/pages", opt{body: "{not json"}); r.status != 400 {
		t.Fatalf("under-limit malformed = %d", r.status)
	}
	for i := 0; i < 2; i++ {
		if r := e.get("/api/v1/pages", opt{}); r.status != 401 {
			t.Fatalf("under-limit = %d", r.status)
		}
	}
	lim := e.get("/api/v1/pages", opt{})
	if lim.status != 429 || lim.body != `{"error":"rate limit exceeded — slow down","code":"rate_limited"}` {
		t.Fatalf("over-limit = %d %s", lim.status, lim.body)
	}
	if r := e.do("POST", "/api/v1/pages", opt{body: "{not json"}); r.status != 429 {
		t.Fatalf("over-limit malformed = %d (parsed before the limiter?)", r.status)
	}
	// The same budget covers /upload and /export.
	for _, p := range []string{"/upload/x", "/export/a/b"} {
		if r := e.do("PUT", p, opt{body: "x"}); r.status != 429 {
			t.Fatalf("%s after the API budget = %d", p, r.status)
		}
	}
	for i := 0; i < 2; i++ {
		if r := e.get("/some-client-page", opt{host: contentHost}); r.status == 429 {
			t.Fatalf("content request %d limited", i+1)
		}
	}
	busy := e.get("/some-client-page", opt{host: contentHost})
	if busy.status != 429 || !strings.HasPrefix(busy.header.Get("Content-Type"), "text/html") ||
		!strings.Contains(busy.body, "This page is being loaded too often") || strings.Contains(strings.ToLower(busy.body), "<script") ||
		busy.header.Get("Content-Security-Policy") == "" || busy.header.Get("Cache-Control") != "no-store" ||
		busy.header.Get("X-Robots-Tag") == "" || busy.header.Get("Retry-After") != "60" {
		t.Fatalf("content 429 = %d %v", busy.status, busy.header)
	}
}

// test/proxy.integration.js, server-level parts: per-client budgets under
// the one-hop proxy policy, and audit attribution.
func TestProxyAttribution(t *testing.T) {
	e := setup(t, func() {
		for _, k := range []string{"RL_API_PER_MIN", "RL_CONTENT_PER_MIN", "RL_PASSWORD_TRIES"} {
			t.Setenv(k, "2")
		}
	})
	e.publish("northwind", pageHTML)
	surfaces := []struct {
		name, method, path, host string
		status                   int
		html                     bool
	}{
		{"content", "GET", "/raw/northwind", contentHost, 403, true},
		{"API", "GET", "/api/v1/pages", "", 401, false},
		{"password", "POST", "/northwind-missing", contentHost, 404, true},
	}
	for _, s := range surfaces {
		for _, client := range []string{"127.0.0.2", "127.0.0.3"} {
			xff := map[string]string{"X-Forwarded-For": client}
			o := opt{host: s.host, headers: xff}
			if s.method == "POST" {
				o.body, o.contentType = "password=x", "application/x-www-form-urlencoded"
			}
			for i := 0; i < 2; i++ {
				if r := e.do(s.method, s.path, o); r.status != s.status {
					t.Fatalf("%s %s allowance %d = %d", s.name, client, i+1, r.status)
				}
			}
			lim := e.do(s.method, s.path, o)
			if lim.status != 429 || lim.header.Get("RateLimit") == "" || strings.HasPrefix(lim.header.Get("Content-Type"), "text/html") != s.html {
				t.Fatalf("%s %s third = %d %s", s.name, client, lim.status, lim.header.Get("Content-Type"))
			}
			o.headers = map[string]string{"X-Forwarded-For": "192.0.2.10, 192.0.2.11, " + client}
			if r := e.do(s.method, s.path, o); r.status != 429 {
				t.Fatalf("%s: earlier forwarded values created an allowance (%d)", s.name, r.status)
			}
		}
	}
	ctx := context.Background()
	m, err := tokens.Mint(ctx, e.d, tokens.MintParams{Label: "northwind-proxy", Scope: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh address: the API budget of 2 is per client.
	created := e.do("POST", "/api/v1/pages", opt{
		body:    `{"slug":"northwind-proxy","title":"Northwind"}`,
		headers: map[string]string{"Authorization": "Bearer " + m.Token, "X-Forwarded-For": "127.0.0.5"},
	})
	if created.status != 201 {
		t.Fatalf("create = %d %s", created.status, created.body)
	}
	var ip string
	if err := e.d.QueryRow(ctx, `SELECT ip FROM audit_log a JOIN pages p ON p.id=a.page_id WHERE p.slug='northwind-proxy' ORDER BY a.id DESC LIMIT 1`).Scan(&ip); err != nil {
		t.Fatal(err)
	}
	if ip != "127.0.0.5" {
		t.Fatalf("audit ip = %q", ip)
	}
}

func TestProxyDirectMode(t *testing.T) {
	e := setup(t, func() {
		t.Setenv("PAGES_TRUST_PROXY", "false")
		t.Setenv("RL_CONTENT_PER_MIN", "2")
	})
	// With no proxy trusted, a forwarded header cannot change attribution.
	for i, want := range []int{403, 403, 429} {
		r := e.get("/raw/northwind", opt{host: contentHost, headers: map[string]string{"X-Forwarded-For": "192.0.2." + string(rune('1'+i))}})
		if r.status != want {
			t.Fatalf("direct request %d = %d, want %d", i+1, r.status, want)
		}
	}
}

func TestInvalidTrustProxyFailsClosed(t *testing.T) {
	pub, _, _ := ed25519GenerateKey()
	baseEnv(t, pub)
	for _, v := range []string{"true", "1", "loopback,2"} {
		t.Setenv("PAGES_TRUST_PROXY", v)
		if err := Validate(); err == nil || !strings.Contains(err.Error(), "PAGES_TRUST_PROXY") {
			t.Errorf("PAGES_TRUST_PROXY=%s: Validate = %v", v, err)
		}
	}
}

func TestValidateFailsClosed(t *testing.T) {
	pub, _, _ := ed25519GenerateKey()
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"raw secret", map[string]string{"PAGES_ENV": "production", "RAW_TOKEN_SECRET": ""}, "RAW_TOKEN_SECRET"},
		{"cookie secret", map[string]string{"PAGES_ENV": "production", "PAGE_COOKIE_SECRET": ""}, "PAGE_COOKIE_SECRET"},
		{"pepper", map[string]string{"PAGES_ENV": "production", "API_TOKEN_PEPPER": ""}, "API_TOKEN_PEPPER"},
		{"max html", map[string]string{"MAX_HTML_BYTES": "lots"}, "MAX_HTML_BYTES"},
		{"central", map[string]string{"PAGES_AUTH_MODE": "central"}, "AUTH_"},
		{"mode", map[string]string{"PAGES_AUTH_MODE": "bogus"}, "PAGES_AUTH_MODE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			baseEnv(t, pub)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if err := Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Validate = %v, want mention of %s", err, c.want)
			}
		})
	}
	baseEnv(t, pub)
	if err := Validate(); err != nil {
		t.Fatalf("valid configuration refused: %v", err)
	}
}

func TestMountSemantics(t *testing.T) {
	for _, c := range []struct {
		path, prefix string
		want         bool
	}{
		{"/upload", "/upload", true}, {"/upload/", "/upload", true}, {"/UPLOAD/x", "/upload", true},
		{"/uploadx", "/upload", false}, {"/uploa", "/upload", false}, {"/mcp", "/mcp", true}, {"/mcpx", "/mcp", false},
	} {
		if got := under(c.path, c.prefix); got != c.want {
			t.Errorf("under(%q,%q) = %v", c.path, c.prefix, got)
		}
	}
	e := setup(t, nil)
	// /uploadx is not the upload mount: the dashboard 404.
	if r := e.do("PUT", "/uploadx", opt{}); r.status != 404 || r.body != shell.NotFound() {
		t.Fatalf("/uploadx = %d", r.status)
	}
	// A path the upload router does not take falls through to the 404 page.
	if r := e.get("/upload", opt{}); r.status != 404 || r.body != shell.NotFound() {
		t.Fatalf("GET /upload = %d %s", r.status, r.body)
	}
	if r := e.get("/export", opt{}); r.status != 404 || r.body != shell.NotFound() {
		t.Fatalf("GET /export = %d %s", r.status, r.body)
	}
}

func TestMCPInjected(t *testing.T) {
	var seen string
	e := setup(t, nil, func(c *Config) {
		c.MCP = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r.URL.Path
			w.WriteHeader(204)
		})
	})
	if r := e.do("POST", "/mcp", opt{body: "{bad"}); r.status != 204 || seen != "/mcp" {
		t.Fatalf("/mcp = %d %q", r.status, seen)
	}
	if r := e.do("POST", "/MCP/", opt{body: "{bad"}); r.status != 204 || seen != "/MCP/" {
		t.Fatalf("/MCP/ = %d %q", r.status, seen)
	}
	if r := e.get("/mcp", opt{host: contentHost}); r.status == 204 {
		t.Fatal("MCP reachable on the content host")
	}
}

func TestBanner(t *testing.T) {
	t.Setenv("DASHBOARD_HOST", "Pages.Northwind.example")
	t.Setenv("CONTENT_HOST", "northwind-pages.example")
	if got := Banner("3002"); got != "Elcano Pages on :3002  (dashboard=pages.northwind.example  content=northwind-pages.example)" {
		t.Fatal(got)
	}
}
