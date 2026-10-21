// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/auth/central"
)

const clientSecret = "ssssssssssssssssssssssssssssssssssssssss"

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// stand-in Auth: /jwks.json and /token, the way Auth serves them.
type fakeAuth struct {
	priv  ed25519.PrivateKey
	pub   ed25519.PublicKey
	mu    sync.Mutex
	codes map[string]fakeCode
	srv   *httptest.Server
}

type fakeCode struct {
	challenge string
	claims    map[string]any
}

func (f *fakeAuth) sign(typ string, claims map[string]any) string {
	h := b64u(mustJSON(map[string]any{"alg": "EdDSA", "typ": typ, "kid": central.KidForKey(f.pub)}))
	body := b64u(mustJSON(claims))
	return h + "." + body + "." + b64u(ed25519.Sign(f.priv, []byte(h+"."+body)))
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/jwks.json":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustJSON(map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64u(f.pub), "kid": central.KidForKey(f.pub)}}}))
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		f.mu.Lock()
		c, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if user, pass, _ := r.BasicAuth(); !ok || user != "pages" || pass != clientSecret || b64u(sum[:]) != c.challenge {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims := map[string]any{"iat": time.Now().Unix()}
		for k, v := range c.claims {
			claims[k] = v
		}
		answer := map[string]any{"id_token": f.sign("JWT", claims)}
		for k, v := range claims {
			answer[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(mustJSON(answer))
	default:
		http.NotFound(w, r)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cookieFrom(r resp, name string) string {
	for _, c := range r.header.Values("Set-Cookie") {
		first, _, _ := strings.Cut(c, ";")
		if strings.HasPrefix(first, name+"=") && first != name+"=" {
			return first
		}
	}
	return ""
}

type centralEnv struct {
	*testEnv
	fa *fakeAuth
}

func setupCentral(t *testing.T, limits map[string]string) *centralEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAuth{priv: priv, pub: pub, codes: map[string]fakeCode{}}
	fa.srv = httptest.NewServer(fa)
	t.Cleanup(fa.srv.Close)
	e := setup(t, func() {
		for k, v := range map[string]string{
			"PAGES_AUTH_MODE":          "central",
			"AUTH_ISSUER_URL":          fa.srv.URL,
			"AUTH_ALLOW_INSECURE_HTTP": "1",
			"PAGES_AUTH_COOKIE_SECURE": "0",
			"PAGES_PUBLIC_URL":         "http://localhost",
			"AUTH_CLIENT_ID":           "pages",
			"AUTH_CLIENT_SECRET":       clientSecret,
			"AUTH_SIGNING_PUBKEY":      base64.StdEncoding.EncodeToString(pub),
		} {
			t.Setenv(k, v)
		}
		for k, v := range limits {
			t.Setenv(k, v)
		}
	})
	return &centralEnv{e, fa}
}

// pendingLogin starts a handoff and registers the code Auth would issue.
func (c *centralEnv) pendingLogin(sub, email string) (loginCookie, callbackPath string) {
	c.t.Helper()
	start := c.get("/auth/login", opt{})
	if start.status != 303 {
		c.t.Fatalf("login start = %d", start.status)
	}
	az, _ := url.Parse(start.header.Get("Location"))
	code := randHex(16)
	c.fa.mu.Lock()
	c.fa.codes[code] = fakeCode{challenge: az.Query().Get("code_challenge"), claims: map[string]any{
		"iss": c.fa.srv.URL, "aud": "pages", "sub": sub, "email": email, "nonce": az.Query().Get("nonce"), "exp": time.Now().Unix() + 300,
	}}
	c.fa.mu.Unlock()
	return cookieFrom(start, "pages_login"),
		"/auth/callback?code=" + code + "&state=" + url.QueryEscape(az.Query().Get("state")) + "&iss=" + url.QueryEscape(c.fa.srv.URL)
}

func (c *centralEnv) signIn(sub, email string) string {
	c.t.Helper()
	login, cb := c.pendingLogin(sub, email)
	r := c.get(cb, opt{cookie: login})
	if r.status != 303 {
		c.t.Fatalf("callback = %d %s", r.status, r.body)
	}
	return cookieFrom(r, "pages_session")
}

func (c *centralEnv) logoutToken(sub string) string {
	now := time.Now().Unix()
	return c.fa.sign("logout+jwt", map[string]any{
		"iss": c.fa.srv.URL, "aud": "pages", "sub": sub, "jti": randHex(16), "iat": now, "exp": now + 120,
		"events": map[string]any{central.BackchannelLogoutEvent: map[string]any{}},
	})
}

// Central routes exist on the dashboard host only; the shells redirect to
// local sign-in; /logout is the central one.
func TestCentralHostSplit(t *testing.T) {
	c := setupCentral(t, nil)
	if _, err := c.s.auth.Central().Store.GrantAccess(context.Background(), "ana@northwind.example"); err != nil {
		t.Fatal(err)
	}
	if r := c.get("/admin/templates", opt{}); r.status != 302 || r.header.Get("Location") != "/auth/login?next=%2Fadmin%2Ftemplates" {
		t.Fatalf("anon shell = %d %s", r.status, r.header.Get("Location"))
	}
	if r := c.get("/api/v1/admin/pages", opt{}); r.status != 401 || r.body != `{"error":"not signed in","code":"no_session"}` {
		t.Fatalf("anon admin API = %d %s", r.status, r.body)
	}
	session := c.signIn("sub-ana", "ana@northwind.example")
	if r := c.get("/api/me", opt{cookie: session}); r.body != `{"email":"ana@northwind.example","tenant":"","admin":true}` {
		t.Fatalf("/api/me = %s", r.body)
	}
	if r := c.get("/admin", opt{cookie: session}); r.status != 200 {
		t.Fatalf("/admin = %d", r.status)
	}
	for _, p := range []string{"/auth/login", "/auth/callback", "/signed-out", "/logout"} {
		r := c.get(p, opt{host: contentHost, cookie: session})
		if r.status == 303 || strings.Contains(r.header.Get("Location"), "/logout?client_id") || strings.Contains(r.body, "signed out of Pages") {
			t.Errorf("content host %s = %d (central route leaked)", p, r.status)
		}
	}
	if r := c.do("POST", "/auth/backchannel-logout", opt{host: contentHost, body: "logout_token=x", contentType: "application/x-www-form-urlencoded"}); r.status == 204 || r.status == 400 {
		t.Errorf("content host backchannel = %d", r.status)
	}
	if r := c.get("/signed-out", opt{}); r.status != 200 || !strings.Contains(r.body, "signed out of Pages") || r.header.Get("Content-Security-Policy") == "" {
		t.Fatalf("/signed-out = %d", r.status)
	}
	// A method a central route does not take falls through to the 404 page.
	if r := c.do("POST", "/auth/login", opt{}); r.status != 404 {
		t.Fatalf("POST /auth/login = %d", r.status)
	}
	out := c.do("POST", "/logout", opt{cookie: session, headers: map[string]string{"Origin": "http://localhost"}})
	if out.status != 303 || out.header.Get("Location") != c.fa.srv.URL+"/logout?client_id=pages" {
		t.Fatalf("logout = %d %s", out.status, out.header.Get("Location"))
	}
}

// central-auth.integration.js steps 19-20: each sign-in stage has its own
// budget, and the back-channel's is apart from the API's.
func TestCentralRateLimitBudgets(t *testing.T) {
	c := setupCentral(t, map[string]string{"RL_SIGNIN_PER_MIN": "5", "RL_API_PER_MIN": "20", "RL_SIGNOUT_PER_MIN": "30"})
	ctx := context.Background()
	if _, err := c.s.auth.Central().Store.GrantAccess(ctx, "dee@northwind.example"); err != nil {
		t.Fatal(err)
	}
	dee := c.signIn("sub-dee", "dee@northwind.example")
	pendingCookie, pendingCallback := c.pendingLogin("sub-dee", "dee@northwind.example")

	var limited *resp
	for i := 0; i < 50 && limited == nil; i++ {
		if r := c.get("/auth/login", opt{}); r.status == 429 {
			limited = &r
		}
	}
	if limited == nil {
		t.Fatal("sign-in limiter never engaged")
	}
	if !strings.HasPrefix(limited.header.Get("Content-Type"), "text/html") || limited.header.Get("Retry-After") != "60" ||
		!strings.Contains(limited.body, "Too many sign-in attempts") || limited.header.Get("Cache-Control") != "no-store" ||
		limited.header.Get("Content-Security-Policy") == "" {
		t.Fatalf("sign-in 429 = %v %s", limited.header, limited.body)
	}
	finished := c.get(pendingCallback, opt{cookie: pendingCookie})
	if finished.status != 303 || cookieFrom(finished, "pages_session") == "" {
		t.Fatalf("handoff in flight = %d", finished.status)
	}
	anonLimited := false
	for i := 0; i < 100 && !anonLimited; i++ {
		anonLimited = c.get("/logout", opt{}).status == 429
	}
	if !anonLimited {
		t.Fatal("anonymous /logout never limited")
	}
	out := c.get("/logout", opt{cookie: dee})
	if out.status != 303 || out.header.Get("Location") != c.fa.srv.URL+"/logout?client_id=pages" {
		t.Fatalf("signed-in logout = %d %s", out.status, out.header.Get("Location"))
	}
	if r := c.get("/admin", opt{cookie: dee}); r.status != 302 {
		t.Fatalf("session after logout = %d", r.status)
	}

	dee2 := cookieFrom(finished, "pages_session")
	apiLimited := false
	for i := 0; i < 100 && !apiLimited; i++ {
		apiLimited = c.get("/api/v1/admin/pages", opt{}).status == 429
	}
	if !apiLimited {
		t.Fatal("API budget never exhausted")
	}
	form := url.Values{"logout_token": {c.logoutToken("sub-dee")}}.Encode()
	if r := c.do("POST", "/auth/backchannel-logout", opt{body: form, contentType: "application/x-www-form-urlencoded"}); r.status != 204 {
		t.Fatalf("backchannel under API pressure = %d %s", r.status, r.body)
	}
	if r := c.get("/admin", opt{cookie: dee2}); r.status != 302 {
		t.Fatalf("revocation not applied: %d", r.status)
	}
}
