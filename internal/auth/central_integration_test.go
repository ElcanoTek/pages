// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Integration check for PAGES_AUTH_MODE=central, ported from
// test/central-auth.integration.js: the application handoff against a
// stand-in Auth, the local access list that Auth's signed events maintain,
// the session conventions, and sign-out both ways. The stand-in serves
// /token and /jwks.json the way Auth does; /authorize is never fetched by
// Pages, so the test reads the redirect Pages builds and answers it itself.
//
// The host split (step 10) and the shared rate-limit budgets (steps 19-20)
// belong to the server and rate limiter; here a counting middleware proves
// each route is wrapped in its own limiter.
package auth_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/auth"
	"github.com/ElcanoTek/pages/internal/auth/central"
	"github.com/ElcanoTek/pages/internal/csrf"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/testdb"
)

const clientSecret = "ssssssssssssssssssssssssssssssssssssssss"

type kp struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func genKey(t *testing.T) kp {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return kp{priv, pub}
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jsonOf(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func signed(typ string, claims map[string]any, k kp) string {
	h := b64u(jsonOf(map[string]any{"alg": "EdDSA", "typ": typ, "kid": central.KidForKey(k.pub)}))
	body := b64u(jsonOf(claims))
	return h + "." + body + "." + b64u(ed25519.Sign(k.priv, []byte(h+"."+body)))
}

func nowS() int64 { return time.Now().Unix() }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

type codeEntry struct {
	challenge string
	claims    map[string]any
	idToken   string // "" signed, "forged", "omit"
}

type fakeAuth struct {
	t                *testing.T
	signing, rotated kp
	forger           kp
	mu               sync.Mutex
	codes            map[string]codeEntry
	tokenStatus      int
	srv              *httptest.Server
	callbackURL      string
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/jwks.json":
		var keys []map[string]any
		for _, k := range []ed25519.PublicKey{f.signing.pub, f.rotated.pub} {
			keys = append(keys, map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64u(k), "kid": central.KidForKey(k)})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonOf(map[string]any{"keys": keys}))
	case r.URL.Path == "/token" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		user, pass, _ := r.BasicAuth()
		f.mu.Lock()
		entry, ok := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		status := f.tokenStatus
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if status != 200 || user+":"+pass != "pages:"+clientSecret || !ok || b64u(sum[:]) != entry.challenge ||
			r.PostForm.Get("redirect_uri") != f.callbackURL {
			if status == 200 {
				status = 400
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		// Like Auth: the claims in the clear and as an EdDSA-signed id_token.
		claims := map[string]any{"iat": nowS()}
		for k, v := range entry.claims {
			claims[k] = v
		}
		answer := map[string]any{}
		for k, v := range claims {
			answer[k] = v
		}
		switch entry.idToken {
		case "forged":
			answer["id_token"] = signed("JWT", claims, f.forger)
		case "omit":
		default:
			answer["id_token"] = signed("JWT", claims, f.signing)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jsonOf(answer))
	default:
		http.NotFound(w, r)
	}
}

type harness struct {
	t     *testing.T
	auth  *fakeAuth
	pages *httptest.Server
	a     *auth.Authenticator
	d     *db.DB
	hits  map[string]int
	hitMu sync.Mutex
}

type resp struct {
	status  int
	headers http.Header
	body    string
}

func (h *harness) req(method, path string, cookie string, headers map[string]string, form url.Values) resp {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r, err := http.NewRequestWithContext(context.Background(), method, h.pages.URL+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	r.Host = "localhost"
	if cookie != "" {
		r.Header.Set("Cookie", cookie)
	}
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(r)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, string(b)}
}

func cookieFrom(r resp, name string) string {
	for _, c := range r.headers.Values("Set-Cookie") {
		first, _, _ := strings.Cut(c, ";")
		if strings.HasPrefix(first, name+"=") && first != name+"=" {
			return first
		}
	}
	return ""
}

func (h *harness) backchannel(form url.Values) int {
	return h.req("POST", "/auth/backchannel-logout", "", nil, form).status
}

type signInResult struct {
	callback  resp
	session   string
	authorize *url.URL
}

func (h *harness) signIn(sub, email, next string, extra map[string]any, idToken string) signInResult {
	h.t.Helper()
	if next == "" {
		next = "/admin"
	}
	start := h.req("GET", "/auth/login?next="+url.QueryEscape(next), "", nil, nil)
	if start.status != 303 {
		h.t.Fatalf("login start = %d", start.status)
	}
	authorize, _ := url.Parse(start.headers.Get("Location"))
	login := cookieFrom(start, "pages_login")
	if login == "" {
		h.t.Fatal("login transaction cookie not set")
	}
	code := randHex(16)
	claims := map[string]any{"iss": h.auth.srv.URL, "aud": "pages", "sub": sub, "email": email, "nonce": authorize.Query().Get("nonce"), "exp": nowS() + 300}
	for k, v := range extra {
		claims[k] = v
	}
	h.auth.mu.Lock()
	h.auth.codes[code] = codeEntry{challenge: authorize.Query().Get("code_challenge"), claims: claims, idToken: idToken}
	h.auth.mu.Unlock()
	state := authorize.Query().Get("state")
	cb := h.req("GET", "/auth/callback?code="+code+"&state="+url.QueryEscape(state)+"&iss="+url.QueryEscape(h.auth.srv.URL), login, nil, nil)
	return signInResult{callback: cb, session: cookieFrom(cb, "pages_session"), authorize: authorize}
}

func (h *harness) logoutToken(sub string, extra map[string]any) string {
	c := map[string]any{
		"iss": h.auth.srv.URL, "aud": "pages", "sub": sub, "jti": randHex(16), "iat": nowS(), "exp": nowS() + 120,
		"events": map[string]any{central.BackchannelLogoutEvent: map[string]any{}},
	}
	for k, v := range extra {
		c[k] = v
	}
	return signed("logout+jwt", c, h.auth.signing)
}

func (h *harness) accessToken(sub, email, action string, version int, key *kp) string {
	k := h.auth.signing
	if key != nil {
		k = *key
	}
	return signed("access+jwt", map[string]any{
		"iss": h.auth.srv.URL, "aud": "pages", "sub": sub, "email": email, "jti": randHex(16), "iat": nowS(), "exp": nowS() + 120,
		"events": map[string]any{central.ApplicationAccessEvent: map[string]any{"action": action, "version": version}},
	}, k)
}

func (h *harness) store() *central.Store { return h.a.Central().Store }

func (h *harness) counter(name string) central.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.hitMu.Lock()
			h.hits[name]++
			h.hitMu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
}

func newHarness(t *testing.T) *harness {
	d := testdb.New(t)
	fa := &fakeAuth{t: t, signing: genKey(t), rotated: genKey(t), forger: genKey(t), codes: map[string]codeEntry{}, tokenStatus: 200}
	fa.srv = httptest.NewServer(fa)
	t.Cleanup(fa.srv.Close)
	fa.callbackURL = "http://localhost/auth/callback"
	for k, v := range map[string]string{
		"PAGES_AUTH_MODE":          "central",
		"AUTH_ISSUER_URL":          fa.srv.URL,
		"AUTH_ALLOW_INSECURE_HTTP": "1",
		"PAGES_AUTH_COOKIE_SECURE": "0",
		"DASHBOARD_HOST":           "localhost",
		"PAGES_PUBLIC_URL":         "http://localhost",
		"AUTH_CLIENT_ID":           "pages",
		"AUTH_CLIENT_SECRET":       clientSecret,
		"AUTH_SIGNING_PUBKEY":      base64.StdEncoding.EncodeToString(fa.signing.pub),
		"PAGE_COOKIE_SECRET":       "central-test-secret",
		"PAGES_ENV":                "",
		"NODE_ENV":                 "",
	} {
		t.Setenv(k, v)
	}
	a, err := auth.NewFromEnv(d)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, auth: fa, a: a, d: d, hits: map[string]int{}}
	mux := http.NewServeMux()
	a.Central().Register(mux, central.RoutesOptions{
		Render: func(p central.ChromePage) string {
			return "<title>" + p.Title + "</title><h1>" + p.Heading + "</h1>" + p.BodyHTML
		},
		Signin:         h.counter("signin"),
		SigninCallback: h.counter("callback"),
		Signout:        h.counter("signout"),
		Backchannel:    h.counter("backchannel"),
	})
	mux.Handle("/admin", a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("admin")) })))
	mux.Handle("/admin/", a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("admin")) })))
	mux.Handle("/api/me", a.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := auth.User(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"email": s.Email, "tenant": s.Tenant, "admin": a.IsAdmin(s)})
	})))
	mux.Handle("/api/v1/admin/pages", csrf.RequireAdminJSON(a)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) })))
	h.pages = httptest.NewServer(mux)
	t.Cleanup(h.pages.Close)
	return h
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func tokenOf(cookie string) string { _, v, _ := strings.Cut(cookie, "="); return v }

func TestCentralAuthIntegration(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	issuer := h.auth.srv.URL

	t.Log("1. anonymous admin request → local sign-in; admin API → 401")
	anon := h.req("GET", "/admin/templates", "", nil, nil)
	if anon.status != 302 || anon.headers.Get("Location") != "/auth/login?next=%2Fadmin%2Ftemplates" {
		t.Fatalf("anon = %d %q", anon.status, anon.headers.Get("Location"))
	}
	if r := h.req("GET", "/api/v1/admin/pages", "", nil, nil); r.status != 401 || r.body != `{"error":"not signed in","code":"no_session"}` {
		t.Fatalf("admin API = %d %s", r.status, r.body)
	}

	t.Log("2. /auth/login builds the exact authorize request")
	nobody := h.signIn("sub-nobody", "nobody@northwind.example", "", nil, "")
	az := nobody.authorize
	if az.Scheme+"://"+az.Host+az.Path != issuer+"/authorize" {
		t.Fatalf("authorize = %s", az)
	}
	q := az.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "pages" || q.Get("redirect_uri") != "http://localhost/auth/callback" ||
		q.Get("scope") != "openid email" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) < 40 || len(q.Get("nonce")) < 40 {
		t.Fatalf("authorize query = %v", q)
	}
	if !strings.HasPrefix(az.RawQuery, "response_type=code&client_id=pages&redirect_uri=http%3A%2F%2Flocalhost%2Fauth%2Fcallback&scope=openid+email&state=") {
		t.Fatalf("authorize query order = %s", az.RawQuery)
	}

	t.Log("3. a valid identity not on the access list is refused")
	denied := h.signIn("sub-ana", "ana@northwind.example", "", nil, "")
	if denied.callback.status != 403 || !strings.Contains(denied.callback.body, "does not have access to Pages") || denied.session != "" {
		t.Fatalf("denied = %d %q", denied.callback.status, denied.callback.body)
	}
	if r := h.req("GET", "/auth/callback?error=access_denied&state=x", "", nil, nil); r.status != 403 {
		t.Fatalf("access_denied = %d", r.status)
	}

	t.Log("4. Auth's signed grant event puts the account on the list")
	if s := h.backchannel(url.Values{"access_token": {h.accessToken("sub-ana", "Ana@Northwind.example", "grant", 1, nil)}}); s != 204 {
		t.Fatalf("grant = %d", s)
	}
	if s := h.backchannel(url.Values{"access_token": {h.accessToken("sub-ana", "ana@northwind.example", "revoke", 1, nil)}}); s != 204 {
		t.Fatalf("stale = %d", s)
	}
	if list, _ := h.store().ListAccess(ctx); strings.Join(list, ",") != "ana@northwind.example" {
		t.Fatalf("list = %v", list)
	}

	t.Log("5. sign-in works and lands on the requested page")
	ok := h.signIn("sub-ana", "ana@northwind.example", "/admin/templates?x=1", nil, "")
	if ok.callback.status != 303 || ok.callback.headers.Get("Location") != "/admin/templates?x=1" || ok.session == "" {
		t.Fatalf("ok = %d %q %q", ok.callback.status, ok.callback.headers.Get("Location"), ok.session)
	}
	setCookies := strings.Join(ok.callback.headers.Values("Set-Cookie"), "\n")
	if !regexp.MustCompile(`pages_session=[^;]+; Path=/; HttpOnly; SameSite=Lax`).MatchString(setCookies) ||
		!strings.Contains(setCookies, "pages_login=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0") {
		t.Fatalf("set-cookie = %s", setCookies)
	}
	if ok.callback.headers.Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("referrer-policy")
	}
	if r := h.req("GET", "/admin", ok.session, nil, nil); r.status != 200 {
		t.Fatalf("admin = %d", r.status)
	}
	var me map[string]any
	_ = json.Unmarshal([]byte(h.req("GET", "/api/me", ok.session, nil, nil).body), &me)
	if me["email"] != "ana@northwind.example" || me["tenant"] != "" || me["admin"] != true {
		t.Fatalf("me = %v", me)
	}

	t.Log("6. the session is stored only as a hash, with the convention's limits")
	token := tokenOf(ok.session)
	var hash string
	var created, idle, absolute int64
	if err := h.d.QueryRow(ctx, `SELECT token_hash, created_at, idle_expires_at, absolute_expires_at FROM central_sessions WHERE email = 'ana@northwind.example'`).
		Scan(&hash, &created, &idle, &absolute); err != nil {
		t.Fatal(err)
	}
	if hash != sha(token) || absolute-created != 24*3600 || idle-created != 12*3600 {
		t.Fatalf("row = %s %d %d %d", hash, created, idle, absolute)
	}

	t.Log("7. already signed in: /auth/login goes straight to next")
	if r := h.req("GET", "/auth/login?next=%2Fadmin%2Fportals", ok.session, nil, nil); r.status != 303 || r.headers.Get("Location") != "/admin/portals" {
		t.Fatalf("again = %d %q", r.status, r.headers.Get("Location"))
	}
	if r := h.req("GET", "/auth/login?next=%2Fa%2F..%2F%2Fevil.example%2Fx", ok.session, nil, nil); r.headers.Get("Location") != "/admin" {
		t.Fatalf("dotted = %q", r.headers.Get("Location"))
	}

	t.Log("8. callback hardening")
	s1 := h.req("GET", "/auth/login?next=%2Fadmin", "", nil, nil)
	if r := h.req("GET", "/auth/callback?code=abc&state=nope", cookieFrom(s1, "pages_login"), nil, nil); r.status != 400 {
		t.Fatalf("wrong state = %d", r.status)
	}
	s1loc, _ := url.Parse(s1.headers.Get("Location"))
	if r := h.req("GET", "/auth/callback?code=abc&state="+url.QueryEscape(s1loc.Query().Get("state")), "", nil, nil); r.status != 400 {
		t.Fatalf("no transaction = %d", r.status)
	}
	if r := h.signIn("sub-ana", "ana@northwind.example", "//evil.example/x", nil, ""); r.callback.headers.Get("Location") != "/admin" {
		t.Fatalf("evil next = %q", r.callback.headers.Get("Location"))
	}
	if r := h.signIn("sub-ana", "ana@northwind.example", `/\evil.example`, nil, ""); r.callback.headers.Get("Location") != "/admin" {
		t.Fatalf("backslash next = %q", r.callback.headers.Get("Location"))
	}
	s2 := h.req("GET", "/auth/login?next=%2Fadmin", "", nil, nil)
	s2loc, _ := url.Parse(s2.headers.Get("Location"))
	if r := h.req("GET", "/auth/callback?code=abc&state="+url.QueryEscape(s2loc.Query().Get("state"))+"&iss="+url.QueryEscape("https://evil.example"), cookieFrom(s2, "pages_login"), nil, nil); r.status != 400 {
		t.Fatalf("other issuer = %d", r.status)
	}

	t.Log("9. token-endpoint answers Pages must not trust")
	if r := h.signIn("sub-ana", "ana@northwind.example", "", map[string]any{"nonce": "forged"}, ""); r.callback.status != 502 || r.session != "" {
		t.Fatalf("bad nonce = %d", r.callback.status)
	}
	if r := h.signIn("sub-ana", "ana@northwind.example", "", map[string]any{"aud": "explorer"}, ""); r.session != "" {
		t.Fatal("bad aud accepted")
	}
	if r := h.signIn("sub-ana", "ana@northwind.example", "", map[string]any{"iss": "https://other.example"}, ""); r.session != "" {
		t.Fatal("bad iss accepted")
	}
	if r := h.signIn("sub-ana", "ana@northwind.example", "", nil, "forged"); r.callback.status != 502 || r.session != "" {
		t.Fatalf("forged = %d", r.callback.status)
	}
	if r := h.signIn("sub-ana", "ana@northwind.example", "", nil, "omit"); r.session != "" {
		t.Fatal("unsigned accepted")
	}
	h.auth.mu.Lock()
	h.auth.tokenStatus = 500
	h.auth.mu.Unlock()
	if r := h.signIn("sub-ana", "ana@northwind.example", "", nil, ""); r.callback.status != 502 {
		t.Fatalf("outage = %d", r.callback.status)
	}
	h.auth.mu.Lock()
	h.auth.tokenStatus = 200
	h.auth.mu.Unlock()

	t.Log("11. back-channel logout ends every session of the subject, idempotently")
	second := h.signIn("sub-ana", "ana@northwind.example", "", nil, "")
	lt := h.logoutToken("sub-ana", nil)
	if s := h.backchannel(url.Values{"logout_token": {lt}}); s != 204 {
		t.Fatalf("logout = %d", s)
	}
	if s := h.backchannel(url.Values{"logout_token": {lt}}); s != 204 {
		t.Fatalf("replay = %d", s)
	}
	for _, c := range []string{ok.session, second.session} {
		if r := h.req("GET", "/admin", c, nil, nil); r.status != 302 {
			t.Fatalf("revoked session = %d", r.status)
		}
	}

	t.Log("11b. a sign-in racing a back-channel logout does not survive it")
	startedBefore := time.Now().UnixMilli() - 5000
	if _, err := h.store().ConsumeLogoutEvent(ctx, central.LogoutEvent{EventID: randHex(16), Subject: "sub-ana", Issuer: issuer, IssuedAt: nowS()}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store().CreateSession(ctx, "sub-ana", "ana@northwind.example", startedBefore); !errors.Is(err, central.ErrAuthTransaction) {
		t.Fatalf("racing sign-in: %v", err)
	}

	t.Log("12. back-channel input validation")
	other := genKey(t)
	logoutClaims := func(typ string, k kp) string {
		return signed(typ, map[string]any{"iss": issuer, "aud": "pages", "sub": "sub-ana", "jti": randHex(8), "iat": nowS(), "exp": nowS() + 60,
			"events": map[string]any{central.BackchannelLogoutEvent: map[string]any{}}}, k)
	}
	for name, c := range map[string]struct {
		form url.Values
		want int
	}{
		"no token":            {url.Values{}, 400},
		"extra field":         {url.Values{"logout_token": {h.logoutToken("sub-x", nil)}, "extra": {"1"}}, 400},
		"blank second field":  {url.Values{"logout_token": {h.logoutToken("sub-x", nil)}, "access_token": {""}}, 400},
		"both tokens":         {url.Values{"logout_token": {lt}, "access_token": {"x"}}, 400},
		"repeated field":      {url.Values{"logout_token": {lt, lt}}, 400},
		"garbage":             {url.Values{"logout_token": {"a.b.c"}}, 400},
		"unknown signing key": {url.Values{"logout_token": {logoutClaims("logout+jwt", other)}}, 400},
		"wrong audience":      {url.Values{"logout_token": {h.logoutToken("sub-ana", map[string]any{"aud": "explorer"})}}, 400},
		"expired":             {url.Values{"logout_token": {h.logoutToken("sub-ana", map[string]any{"exp": nowS() - 600})}}, 400},
		"nonce present":       {url.Values{"logout_token": {h.logoutToken("sub-ana", map[string]any{"nonce": "n"})}}, 400},
		"access+jwt typ":      {url.Values{"logout_token": {logoutClaims("access+jwt", h.auth.signing)}}, 400},
		"too many params":     {url.Values{"a": {"1"}, "b": {"2"}, "c": {"3"}, "d": {"4"}, "e": {"5"}}, 500}, // OnError default
	} {
		if s := h.backchannel(c.form); s != c.want {
			t.Errorf("%s: %d, want %d", name, s, c.want)
		}
	}
	if r := h.req("POST", "/auth/backchannel-logout", "", map[string]string{"Content-Type": "text/plain"}, nil); r.status != 400 || r.body != `{"error":"invalid_request"}` {
		t.Errorf("not a form: %d %s", r.status, r.body)
	}
	if r := h.req("GET", "/auth/backchannel-logout", "", nil, nil); r.status != 404 {
		t.Errorf("GET back-channel: %d", r.status)
	}

	t.Log("13. a key only Auth's JWKS publishes verifies")
	if s := h.backchannel(url.Values{"access_token": {h.accessToken("sub-ben", "ben@northwind.example", "grant", 1, &h.auth.rotated)}}); s != 204 {
		t.Fatalf("rotated grant = %d", s)
	}
	if list, _ := h.store().ListAccess(ctx); !contains(list, "ben@northwind.example") {
		t.Fatalf("list = %v", list)
	}

	t.Log("14. revoke event ends sessions and refuses the next sign-in")
	third := h.signIn("sub-ana", "ana@northwind.example", "", nil, "")
	if third.session == "" {
		t.Fatal("third sign-in")
	}
	if s := h.backchannel(url.Values{"access_token": {h.accessToken("sub-ana", "ana@northwind.example", "revoke", 2, nil)}}); s != 204 {
		t.Fatalf("revoke = %d", s)
	}
	var live int
	_ = h.d.QueryRow(ctx, `SELECT count(*) FROM central_sessions WHERE email = 'ana@northwind.example' AND revoked_at IS NULL`).Scan(&live)
	if live != 0 {
		t.Fatalf("live sessions after revoke: %d", live)
	}
	if r := h.req("GET", "/admin", third.session, nil, nil); r.status != 302 {
		t.Fatalf("revoked access = %d", r.status)
	}
	if r := h.signIn("sub-ana", "ana@northwind.example", "", nil, ""); r.callback.status != 403 {
		t.Fatalf("sign-in after revoke = %d", r.callback.status)
	}

	t.Log("15. a changed email moves access to the new address")
	if s := h.backchannel(url.Values{"access_token": {h.accessToken("sub-ben", "ben.new@northwind.example", "grant", 2, nil)}}); s != 204 {
		t.Fatalf("email change = %d", s)
	}
	if list, _ := h.store().ListAccess(ctx); !contains(list, "ben.new@northwind.example") || contains(list, "ben@northwind.example") {
		t.Fatalf("list = %v", list)
	}

	t.Log("16. idle and absolute expiry are enforced server-side; touch is throttled")
	if _, err := h.store().GrantAccess(ctx, "cy@northwind.example"); err != nil {
		t.Fatal(err)
	}
	cy := h.signIn("sub-cy", "cy@northwind.example", "", nil, "")
	cyHash := sha(tokenOf(cy.session))
	lastSeen := func(hash string) int64 {
		var v int64
		if err := h.d.QueryRow(ctx, `SELECT last_seen_at FROM central_sessions WHERE token_hash = $1`, hash).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := lastSeen(cyHash)
	h.req("GET", "/admin", cy.session, nil, nil)
	if lastSeen(cyHash) != before {
		t.Fatal("a request inside the touch interval wrote")
	}
	_, _ = h.d.Exec(ctx, `UPDATE central_sessions SET last_seen_at = last_seen_at - 120 WHERE token_hash = $1`, cyHash)
	h.req("GET", "/admin", cy.session, nil, nil)
	if lastSeen(cyHash) <= before-120 {
		t.Fatal("not touched after the interval")
	}
	_, _ = h.d.Exec(ctx, `UPDATE central_sessions SET idle_expires_at = $2 WHERE token_hash = $1`, cyHash, nowS()-1)
	if r := h.req("GET", "/admin", cy.session, nil, nil); r.status != 302 {
		t.Fatalf("idle-expired = %d", r.status)
	}
	cy2 := h.signIn("sub-cy", "cy@northwind.example", "", nil, "")
	_, _ = h.d.Exec(ctx, `UPDATE central_sessions SET absolute_expires_at = $2, idle_expires_at = $2 WHERE token_hash = $1`, sha(tokenOf(cy2.session)), nowS()-1)
	if r := h.req("GET", "/admin", cy2.session, nil, nil); r.status != 302 {
		t.Fatalf("absolute-expired = %d", r.status)
	}
	// Concurrent requests at the touch boundary write once and never move
	// the clock backwards.
	cy1b := h.signIn("sub-cy", "cy@northwind.example", "", nil, "")
	cy1bHash := sha(tokenOf(cy1b.session))
	_, _ = h.d.Exec(ctx, `UPDATE central_sessions SET last_seen_at = last_seen_at - 120 WHERE token_hash = $1`, cy1bHash)
	var wg sync.WaitGroup
	statuses := make([]int, 6)
	for i := range statuses {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i] = h.req("GET", "/api/me", cy1b.session, nil, nil).status
		}(i)
	}
	wg.Wait()
	for _, s := range statuses {
		if s != 200 {
			t.Fatalf("burst status %d", s)
		}
	}
	if lastSeen(cy1bHash) < nowS()-2 {
		t.Fatal("burst left the clock stale")
	}
	// Two lookups that both read the stale row: hold the row lock so both
	// UPDATEs queue, newer first. The older one must not move the clock back
	// (the SQL re-check, not only the Go pre-check, has to hold).
	t0 := nowS() + 1000
	cyToken := tokenOf(cy1b.session)
	_, _ = h.d.Exec(ctx, `UPDATE central_sessions SET last_seen_at = $2, absolute_expires_at = $3, idle_expires_at = $3 WHERE token_hash = $1`, cy1bHash, t0-200, t0+5000)
	holder, err := h.d.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT 1 FROM central_sessions WHERE token_hash = $1 FOR UPDATE`, cy1bHash); err != nil {
		t.Fatal(err)
	}
	waiting := func(n int) {
		for i := 0; i < 200; i++ {
			var c int
			_ = h.d.QueryRow(ctx, `SELECT count(*)::int FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'UPDATE central_sessions SET last_seen_at%'`).Scan(&c)
			if c >= n {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("touch UPDATEs never queued")
	}
	type result struct {
		id  *central.Identity
		err error
	}
	newer, older := make(chan result, 1), make(chan result, 1)
	go func() { id, err := h.store().IdentityAt(ctx, cyToken, t0); newer <- result{id, err} }()
	waiting(1)
	go func() { id, err := h.store().IdentityAt(ctx, cyToken, t0-100); older <- result{id, err} }()
	waiting(2)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, o := <-newer, <-older; n.id == nil || o.id == nil || n.err != nil || o.err != nil {
		t.Fatalf("identity: %+v %+v", n, o)
	}
	if kept := lastSeen(cy1bHash); kept != t0 {
		t.Fatalf("an older concurrent touch overwrote a newer one: %d != %d", kept, t0)
	}

	t.Log("17. CLI revoke ends sessions in the same transaction")
	cy3 := h.signIn("sub-cy", "cy@northwind.example", "", nil, "")
	if okRevoke, err := h.store().RevokeAccess(ctx, "CY@northwind.example"); err != nil || !okRevoke {
		t.Fatalf("revoke: %v %v", okRevoke, err)
	}
	if r := h.req("GET", "/admin", cy3.session, nil, nil); r.status != 302 {
		t.Fatalf("after CLI revoke = %d", r.status)
	}

	t.Log("18. sign-out: local session ended, browser handed to Auth's RP logout")
	if _, err := h.store().GrantAccess(ctx, "cy@northwind.example"); err != nil {
		t.Fatal(err)
	}
	cy4 := h.signIn("sub-cy", "cy@northwind.example", "", nil, "")
	if r := h.req("GET", "/logout", cy4.session, map[string]string{"Sec-Fetch-Site": "cross-site"}, nil); r.status != 303 || r.headers.Get("Location") != "/admin" {
		t.Fatalf("cross-site = %d %q", r.status, r.headers.Get("Location"))
	}
	if r := h.req("GET", "/admin", cy4.session, nil, nil); r.status != 200 {
		t.Fatal("cross-site logout not ignored")
	}
	if r := h.req("POST", "/logout", cy4.session, map[string]string{"Origin": "https://evil.example"}, nil); r.headers.Get("Location") != "/admin" {
		t.Fatal("foreign Origin not ignored")
	}
	out := h.req("POST", "/logout", cy4.session, map[string]string{"Origin": "http://localhost"}, nil)
	if out.status != 303 || out.headers.Get("Location") != issuer+"/logout?client_id=pages" {
		t.Fatalf("logout = %d %q", out.status, out.headers.Get("Location"))
	}
	sc := strings.Join(out.headers.Values("Set-Cookie"), "\n")
	if !strings.Contains(sc, "pages_session=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0") ||
		!strings.Contains(sc, "pages_login=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0") || strings.Contains(strings.ToLower(sc), "domain=") {
		t.Fatalf("logout cookies = %s", sc)
	}
	if out.headers.Get("Cache-Control") != "no-store" {
		t.Fatal("logout cache-control")
	}
	if r := h.req("GET", "/admin", cy4.session, nil, nil); r.status != 302 {
		t.Fatal("session not revoked server-side")
	}
	signedOut := h.req("GET", "/signed-out", "", nil, nil)
	if signedOut.status != 200 || !strings.Contains(signedOut.body, "signed out of Pages") || signedOut.headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("signed-out = %d %q", signedOut.status, signedOut.body)
	}

	t.Log("19. each route sits behind its own limiter")
	for _, name := range []string{"signin", "callback", "signout", "backchannel"} {
		if h.hits[name] == 0 {
			t.Errorf("limiter %s never ran", name)
		}
	}
	if h.hits["callback"] >= h.hits["signin"]+20 {
		t.Errorf("unexpected limiter counts %v", h.hits)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
