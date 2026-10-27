// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/auth"
)

// A cookie minted by the Node release's dev-auth script with its throwaway key: the
// Go verifier must accept what the JS signer produced.
const (
	nodePubkey = "H6kMgF5PY/+ioK0Ljmqh/7usjVVvCYg/m0KMP2uVRgE="
	nodeCookie = "eyJlbWFpbCI6ImRldkBlbGNhbm90ZWsuY29tIiwidGVuYW50IjoiIiwiaWF0IjoxNzkyNjAzMzMyLCJleHAiOjE3OTUxOTUzMzJ9.3iMNxkwilN5EuAvCrVf81xIX4KepZEFWRc_H5GI0j4DnxuVIBz6EK93UkNCPBuXuJhX6zSiZ6_v7IbP45xNHBQ"
	nodeIAT    = 1792603332
)

func elcanoEnv(t *testing.T, pubkey string) {
	t.Helper()
	for k, v := range map[string]string{
		"PAGES_AUTH_MODE":     "",
		"AUTH_SIGNING_PUBKEY": pubkey,
		"AUTH_COOKIE_NAME":    "",
		"ADMIN_EMAIL_DOMAIN":  "",
		"AUTH_LOGIN_URL":      "https://auth.northwind.example/",
		"PAGES_DEV_LOGIN":     "",
		"DEV_ADMIN_COOKIE":    "",
		"PAGES_ENV":           "",
		"NODE_ENV":            "",
	} {
		t.Setenv(k, v)
	}
}

func newElcano(t *testing.T, pubkey string) *auth.Authenticator {
	t.Helper()
	elcanoEnv(t, pubkey)
	a, err := auth.NewFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// mint signs a token the way auth.elcanotek.com does: base64url(JSON) "."
// base64url(sig over the base64url body).
func mint(priv ed25519.PrivateKey, payload map[string]any) string {
	b, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
}

func TestParseCookies(t *testing.T) {
	cases := map[string]map[string]string{
		"":                            {},
		"a=1; b=two%21":               {"a": "1", "b": "two!"},
		"=nokey; c=3":                 {"c": "3"},
		"x=%":                         {"x": "%"},
		"x=%2":                        {"x": "%2"},
		"elcano_auth=%zz":             {"elcano_auth": "%zz"},
		"good=a%20b; bad=%; other=ok": {"good": "a b", "bad": "%", "other": "ok"},
	}
	for in, want := range cases {
		if got := auth.ParseCookies(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseCookies(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCurrentSessionMalformedCookieIsLoggedOut(t *testing.T) {
	a := newElcano(t, nodePubkey)
	r := httptest.NewRequest("GET", "/admin", nil)
	r.Header.Set("Cookie", "elcano_auth=%")
	if s := a.CurrentSession(r); s != nil {
		t.Fatalf("session = %+v", s)
	}
}

func TestVerifyElcanoSessionAcceptsNodeMintedCookie(t *testing.T) {
	a := newElcano(t, nodePubkey)
	a.Now = func() time.Time { return time.Unix(nodeIAT+60, 0) }
	s := a.VerifyElcanoSession(nodeCookie)
	if s == nil || s.Email != "dev@elcanotek.com" || s.Tenant != "" || s.Exp != nodeIAT+30*86400 {
		t.Fatalf("session = %+v", s)
	}
	if !a.IsAdmin(s) {
		t.Fatal("dev@elcanotek.com is an admin")
	}
	a.Now = func() time.Time { return time.Unix(nodeIAT+31*86400, 0) }
	if a.VerifyElcanoSession(nodeCookie) != nil {
		t.Fatal("expired cookie accepted")
	}
}

func TestVerifyElcanoSessionRefuses(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	a := newElcano(t, base64.StdEncoding.EncodeToString(pub))
	exp := time.Now().Unix() + 600
	good := mint(priv, map[string]any{"email": "ana@elcanotek.com", "tenant": "northwind", "exp": exp})
	if s := a.VerifyElcanoSession(good); s == nil || s.Tenant != "northwind" {
		t.Fatalf("good = %+v", s)
	}
	body, sig, _ := strings.Cut(good, ".")
	for name, tok := range map[string]string{
		"empty":         "",
		"no dot":        body,
		"leading dot":   "." + sig,
		"trailing dot":  body + ".",
		"other key":     mint(otherPriv, map[string]any{"email": "ana@elcanotek.com", "exp": exp}),
		"tampered":      base64.RawURLEncoding.EncodeToString([]byte(`{"email":"evil@elcanotek.com","exp":9999999999}`)) + "." + sig,
		"no email":      mint(priv, map[string]any{"exp": exp}),
		"empty email":   mint(priv, map[string]any{"email": "", "exp": exp}),
		"email number":  mint(priv, map[string]any{"email": 5, "exp": exp}),
		"string exp":    mint(priv, map[string]any{"email": "a@elcanotek.com", "exp": "9999999999"}),
		"expired":       mint(priv, map[string]any{"email": "a@elcanotek.com", "exp": time.Now().Unix() - 1}),
		"not an object": base64.RawURLEncoding.EncodeToString([]byte(`[1]`)) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(base64.RawURLEncoding.EncodeToString([]byte(`[1]`))))),
	} {
		if s := a.VerifyElcanoSession(tok); s != nil {
			t.Errorf("%s: accepted %+v", name, s)
		}
	}
	// Without a (valid) key every token fails closed.
	for _, pubkey := range []string{"", "AAAA"} {
		if s := newElcano(t, pubkey).VerifyElcanoSession(good); s != nil {
			t.Errorf("pubkey %q: accepted", pubkey)
		}
	}
}

func TestIsElcanoAdmin(t *testing.T) {
	a := newElcano(t, "")
	for email, want := range map[string]bool{
		"ana@elcanotek.com": true, "Ana@ElcanoTek.COM": true, "ana@northwind.example": false,
		"ana@elcanotek.com.evil": false, "elcanotek.com": false, "a@b@elcanotek.com": true,
	} {
		if got := a.IsElcanoAdmin(&auth.Session{Email: email}); got != want {
			t.Errorf("%s: %v", email, got)
		}
	}
	if a.IsAdmin(nil) || a.IsElcanoAdmin(nil) {
		t.Fatal("nil session is an admin")
	}
	t.Setenv("ADMIN_EMAIL_DOMAIN", "Northwind.Example")
	b, _ := auth.NewFromEnv(nil)
	if !b.IsAdmin(&auth.Session{Email: "ana@northwind.example"}) {
		t.Fatal("ADMIN_EMAIL_DOMAIN not honoured")
	}
}

func TestLoginRedirectURLElcano(t *testing.T) {
	a := newElcano(t, "")
	if a.LoginURL() != "https://auth.northwind.example" || a.Origin() != "https://auth.northwind.example" {
		t.Fatalf("login url %q origin %q", a.LoginURL(), a.Origin())
	}
	r := httptest.NewRequest("GET", "/admin/northwind?x=1", nil)
	r.Host = "pages.northwind.example"
	if got := a.LoginRedirectURL(r); got != "https://auth.northwind.example/?return_to=http%3A%2F%2Fpages.northwind.example%2Fadmin%2Fnorthwind%3Fx%3D1" {
		t.Fatalf("plain = %s", got)
	}
	r.Header.Set("X-Forwarded-Proto", "HTTPS, http")
	if got := a.LoginRedirectURL(r); !strings.Contains(got, "return_to=https%3A%2F%2F") {
		t.Fatalf("forwarded https = %s", got)
	}
	r.Header.Del("X-Forwarded-Proto")
	r.TLS = &tls.ConnectionState{}
	if got := a.LoginRedirectURL(r); !strings.Contains(got, "return_to=https%3A%2F%2F") {
		t.Fatalf("tls = %s", got)
	}
}

func TestRequireAdminAndRequireAuth(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	a := newElcano(t, base64.StdEncoding.EncodeToString(pub))
	exp := time.Now().Unix() + 600
	staff := "elcano_auth=" + mint(priv, map[string]any{"email": "ana@elcanotek.com", "exp": exp})
	outsider := "elcano_auth=" + mint(priv, map[string]any{"email": "ana@northwind.example", "tenant": "northwind", "exp": exp})
	var seen *auth.Session
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = auth.User(r); _, _ = w.Write([]byte("ok")) })
	admin, anyone := a.RequireAdmin(ok), a.RequireAuth(ok)

	do := func(h http.Handler, cookie string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/admin", nil)
		r.Host = "pages.northwind.example"
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := do(admin, "")
	if w.Code != 302 || w.Header().Get("Location") != "https://auth.northwind.example/?return_to=http%3A%2F%2Fpages.northwind.example%2Fadmin" {
		t.Fatalf("anonymous = %d %q", w.Code, w.Header().Get("Location"))
	}
	// Express's res.redirect body and headers.
	want := "Found. Redirecting to https://auth.northwind.example/?return_to=http%3A%2F%2Fpages.northwind.example%2Fadmin"
	if w.Body.String() != want || w.Header().Get("Content-Type") != "text/plain; charset=utf-8" || w.Header().Get("Vary") != "Accept" {
		t.Fatalf("redirect body %q headers %v", w.Body.String(), w.Header())
	}
	w = do(admin, outsider)
	// Byte-for-byte what Express answered (recorded from express 5).
	if w.Code != 403 || w.Body.String() != "Forbidden — Elcano staff only." ||
		w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("ETag") != `W/"20-Ta4d1GioLHOsrqHSRMh/U9OUaP8"` {
		t.Fatalf("outsider = %d %q %v", w.Code, w.Body.String(), w.Header())
	}
	w = do(admin, staff)
	if w.Code != 200 || seen == nil || seen.Email != "ana@elcanotek.com" {
		t.Fatalf("staff = %d %+v", w.Code, seen)
	}
	w = do(anyone, outsider)
	if w.Code != 200 || seen.Tenant != "northwind" {
		t.Fatalf("requireAuth outsider = %d %+v", w.Code, seen)
	}
}

func TestModeValidation(t *testing.T) {
	elcanoEnv(t, "")
	t.Setenv("PAGES_AUTH_MODE", "  Elcano ")
	if a, err := auth.NewFromEnv(nil); err != nil || a.Mode() != auth.ModeElcano || a.Central() != nil {
		t.Fatalf("mode: %v", err)
	}
	t.Setenv("PAGES_AUTH_MODE", "ldap")
	if _, err := auth.NewFromEnv(nil); err == nil || err.Error() != `PAGES_AUTH_MODE must be "elcano" or "central"` {
		t.Fatalf("bad mode: %v", err)
	}
	t.Setenv("PAGES_AUTH_MODE", "central")
	t.Setenv("DASHBOARD_HOST", "")
	if _, err := auth.NewFromEnv(nil); err == nil {
		t.Fatal("central mode without config started")
	}
}

func TestDevLogin(t *testing.T) {
	a := newElcano(t, nodePubkey)
	if _, ok := a.DevLoginFromEnv(); ok {
		t.Fatal("dev login without the flag")
	}
	t.Setenv("PAGES_DEV_LOGIN", "1")
	if _, ok := a.DevLoginFromEnv(); ok {
		t.Fatal("dev login without a cookie")
	}
	t.Setenv("DEV_ADMIN_COOKIE", nodeCookie)
	t.Setenv("PAGES_ENV", "production")
	if _, ok := a.DevLoginFromEnv(); ok {
		t.Fatal("dev login in production")
	}
	t.Setenv("PAGES_ENV", "")
	dl, ok := a.DevLoginFromEnv()
	if !ok {
		t.Fatal("dev login not mounted")
	}
	get := func(h http.Handler, target string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", target, nil))
		return w
	}
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	if w := get(dl.Intercept(inner), "/admin/northwind?x=1"); w.Code != 302 || w.Header().Get("Location") != "/__dev/login?next=%2Fadmin%2Fnorthwind%3Fx%3D1" {
		t.Fatalf("intercept = %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(dl.Intercept(inner), "/admin"); w.Code != 204 {
		t.Fatalf("/admin itself is not intercepted: %d", w.Code)
	}
	w := get(dl.Login, "/__dev/login?next=%2Fadmin%2Fx")
	if w.Code != 302 || w.Header().Get("Location") != "/admin/x" || w.Header().Get("Set-Cookie") != "elcano_auth="+nodeCookie+"; Path=/; HttpOnly; SameSite=Lax" {
		t.Fatalf("login = %d %v", w.Code, w.Header())
	}
	for target, want := range map[string]string{
		"/__dev/login": "/",
		"/__dev/login?return_to=http%3A%2F%2Fpages.example%2Fadmin%3Fa%3D1": "/admin?a=1",
		"/__dev/login?next=%2F%2Fevil.example%2Fx":                          "/x",
		"/__dev/login?next=relative":                                        "/",
		"/__dev/login?next=%2F%5Cevil.example%2Fx":                          "/",
		"/__dev/login?next=%2F%09%2Fevil.example":                           "/",
		"/__dev/login?next=%2Fadmin%0A%2F%2Fevil.example":                   "/",
	} {
		if got := get(dl.Login, target).Header().Get("Location"); got != want {
			t.Errorf("%s → %q, want %q", target, got, want)
		}
	}
}

// A failed session lookup carried on the request (the dashboard resolves
// before choosing body limits) is what Resolve answers afterwards: the admin
// gate does not query the session store a second time during an outage.
func TestResolveReusesACarriedLookupError(t *testing.T) {
	a := &auth.Authenticator{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/pages", nil)
	storeDown := errors.New("northwind session store unavailable")
	s, err := a.Resolve(auth.WithResolveError(r, storeDown))
	if s != nil || !errors.Is(err, storeDown) {
		t.Fatalf("Resolve = %v, %v; want the carried error", s, err)
	}
	want := &auth.Session{Email: "staff@example.com"}
	if s, err := a.Resolve(auth.WithSession(r, want)); s != want || err != nil {
		t.Fatalf("a carried session: %v, %v", s, err)
	}
}
