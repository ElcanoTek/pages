// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Unit tests for the central-mode client (no DB): signed back-channel tokens,
// the JWKS key resolver, the code exchange and its id_token, the login
// transaction, redirect safety and configuration validation. Ported from
// test/central-auth.test.js. The Postgres store and the routes are covered by
// integration_test.go.
package central

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
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const issuer = "https://auth.northwind.example"

type keyPair struct {
	priv ed25519.PrivateKey
	raw  ed25519.PublicKey
}

func newKeyPair(t testing.TB) keyPair {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return keyPair{priv: priv, raw: pub}
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

type claims map[string]any

func (c claims) with(extra claims) claims {
	out := claims{}
	for k, v := range c {
		out[k] = v
	}
	for k, v := range extra {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// sign builds a compact JWS the way Auth does; headerExtra overrides header
// fields (a nil value is kept as JSON null only via explicit any(nil)).
func sign(typ string, c claims, key keyPair, headerExtra map[string]any) string {
	header := map[string]any{"alg": "EdDSA", "typ": typ, "kid": KidForKey(key.raw)}
	for k, v := range headerExtra {
		header[k] = v
	}
	h := b64u(mustJSON(header))
	body := b64u(mustJSON(c))
	sig := ed25519.Sign(key.priv, []byte(h+"."+body))
	return h + "." + body + "." + b64u(sig)
}

func unix() int64 { return time.Now().Unix() }

func base(extra claims) claims {
	return claims{"iss": issuer, "aud": "pages", "sub": "sub-1", "jti": "jti-1", "iat": unix(), "exp": unix() + 120}.with(extra)
}

func logoutClaims(extra claims) claims {
	return base(claims{"events": map[string]any{BackchannelLogoutEvent: map[string]any{}}}).with(extra)
}

func accessClaims(action string, version any, extra claims) claims {
	return base(claims{"email": "Ana@Northwind.example", "events": map[string]any{ApplicationAccessEvent: map[string]any{"action": action, "version": version}}}).with(extra)
}

func opts(keys ...ed25519.PublicKey) VerifyOptions {
	return VerifyOptions{Issuer: issuer, Audience: "pages", Keys: keys}
}

func isCentralErr(err error) bool { return errors.Is(err, ErrCentralAuth) }

func TestVerifyLogoutTokenAccepts(t *testing.T) {
	primary := newKeyPair(t)
	ev, err := VerifyLogoutToken(sign("logout+jwt", logoutClaims(nil), primary, nil), opts(primary.raw))
	if err != nil {
		t.Fatal(err)
	}
	if ev.EventID != "jti-1" || ev.Subject != "sub-1" || ev.Issuer != issuer {
		t.Fatalf("event = %+v", ev)
	}
}

func TestVerifyLogoutTokenRefusesMalformedOrForeign(t *testing.T) {
	primary, other := newKeyPair(t), newKeyPair(t)
	bad := map[string]string{
		"wrong key": sign("logout+jwt", logoutClaims(nil), other, nil),
		"kid of a trusted key, signature of another": sign("logout+jwt", logoutClaims(nil), other, map[string]any{"kid": KidForKey(primary.raw)}),
		"wrong typ":            sign("access+jwt", logoutClaims(nil), primary, nil),
		"wrong alg":            sign("logout+jwt", logoutClaims(nil), primary, map[string]any{"alg": "none"}),
		"wrong audience":       sign("logout+jwt", logoutClaims(claims{"aud": "explorer"}), primary, nil),
		"audience array":       sign("logout+jwt", logoutClaims(claims{"aud": []string{"pages"}}), primary, nil),
		"wrong issuer":         sign("logout+jwt", logoutClaims(claims{"iss": "https://evil.example"}), primary, nil),
		"expired":              sign("logout+jwt", logoutClaims(claims{"exp": unix() - 61}), primary, nil),
		"issued in the future": sign("logout+jwt", logoutClaims(claims{"iat": unix() + 600}), primary, nil),
		"nonce present":        sign("logout+jwt", logoutClaims(claims{"nonce": "n"}), primary, nil),
		"no event":             sign("logout+jwt", base(nil), primary, nil),
		"string exp":           sign("logout+jwt", logoutClaims(claims{"exp": strconv.FormatInt(unix()+60, 10)}), primary, nil),
		"empty sub":            sign("logout+jwt", logoutClaims(claims{"sub": ""}), primary, nil),
		"events array":         sign("logout+jwt", logoutClaims(claims{"events": []any{}}), primary, nil),
		"fractional exp":       sign("logout+jwt", logoutClaims(claims{"exp": float64(unix()) + 120.5}), primary, nil),
	}
	for name, token := range bad {
		if _, err := VerifyLogoutToken(token, opts(primary.raw)); !isCentralErr(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	good := sign("logout+jwt", logoutClaims(nil), primary, nil)
	parts := strings.Split(good, ".")
	more := map[string]string{
		"bad signature": parts[0] + "." + parts[1] + "." + strings.Repeat("A", 86),
		"two segments":  "a.b",
		"oversized":     strings.Repeat("x", 20000),
	}
	for name, token := range more {
		if _, err := VerifyLogoutToken(token, opts(primary.raw)); !isCentralErr(err) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := VerifyLogoutToken(good, opts()); !isCentralErr(err) {
		t.Errorf("no keys: err = %v", err)
	}
}

func TestVerifyLogoutTokenTrailingSlashIssuer(t *testing.T) {
	primary := newKeyPair(t)
	ev, err := VerifyLogoutToken(sign("logout+jwt", logoutClaims(claims{"iss": issuer + "/"}), primary, nil), opts(primary.raw))
	if err != nil || ev.Subject != "sub-1" {
		t.Fatalf("ev=%+v err=%v", ev, err)
	}
}

func TestVerifyAccessToken(t *testing.T) {
	primary := newKeyPair(t)
	grant, err := VerifyAccessToken(sign("access+jwt", accessClaims("grant", 3, nil), primary, nil), opts(primary.raw))
	if err != nil {
		t.Fatal(err)
	}
	if !grant.Allowed || grant.Version != 3 || grant.Email != "ana@northwind.example" {
		t.Fatalf("grant = %+v", grant)
	}
	revoke, err := VerifyAccessToken(sign("access+jwt", accessClaims("revoke", 4, nil), primary, nil), opts(primary.raw))
	if err != nil || revoke.Allowed {
		t.Fatalf("revoke = %+v, %v", revoke, err)
	}
	for _, c := range []struct {
		action  string
		version any
	}{{"grant", 0}, {"grant", -1}, {"grant", 1.5}, {"grant", "2"}, {"delete", 1}} {
		if _, err := VerifyAccessToken(sign("access+jwt", accessClaims(c.action, c.version, nil), primary, nil), opts(primary.raw)); !isCentralErr(err) {
			t.Errorf("%v: err = %v", c, err)
		}
	}
	if _, err := VerifyAccessToken(sign("access+jwt", accessClaims("grant", 1, claims{"email": "not-an-email"}), primary, nil), opts(primary.raw)); err == nil {
		t.Error("bad email accepted")
	}
	if _, err := VerifyAccessToken(sign("logout+jwt", accessClaims("grant", 1, nil), primary, nil), opts(primary.raw)); err == nil {
		t.Error("logout typ accepted")
	}
	withSettings := func(settings any) string {
		return sign("access+jwt", base(claims{"email": "a@northwind.example", "events": map[string]any{
			ApplicationAccessEvent: map[string]any{"action": "grant", "version": 1, "settings": settings},
		}}), primary, nil)
	}
	if ev, err := VerifyAccessToken(withSettings(map[string]any{"team": "ops"}), opts(primary.raw)); err != nil || !ev.Allowed {
		t.Errorf("string settings: %+v %v", ev, err)
	}
	for _, bad := range []any{nil, []any{}, "x", map[string]any{"team": 1}} {
		if _, err := VerifyAccessToken(withSettings(bad), opts(primary.raw)); !isCentralErr(err) {
			t.Errorf("settings %v: err = %v", bad, err)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func respond(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func jwksBody(keys ...ed25519.PublicKey) string {
	entries := []map[string]any{}
	for _, k := range keys {
		entries = append(entries, map[string]any{"kty": "OKP", "crv": "Ed25519", "x": b64u(k), "kid": KidForKey(k)})
	}
	return string(mustJSON(map[string]any{"keys": entries}))
}

func client(f rtFunc) *http.Client {
	return &http.Client{Transport: f, CheckRedirect: noRedirects}
}

func TestKeyResolverKnownKidNeverWaitsUnknownFetchesOncePerMinute(t *testing.T) {
	primary, rotated := newKeyPair(t), newKeyPair(t)
	fetches := 0
	clock := int64(1_000_000)
	hc := client(func(r *http.Request) (*http.Response, error) {
		fetches++
		if r.URL.String() != issuer+"/jwks.json" {
			t.Errorf("url = %s", r.URL)
		}
		return respond(200, jwksBody(rotated.raw)), nil
	})
	res := NewKeyResolver(issuer, []ed25519.PublicKey{primary.raw}, hc, func() int64 { return clock })
	ctx := context.Background()
	res.KeysForToken(ctx, sign("logout+jwt", logoutClaims(nil), primary, nil))
	if fetches != 0 {
		t.Fatalf("static key's kid → fetch")
	}
	rotatedToken := sign("logout+jwt", logoutClaims(nil), rotated, nil)
	keys := res.KeysForToken(ctx, rotatedToken)
	if fetches != 1 {
		t.Fatalf("fetches = %d", fetches)
	}
	if ev, err := VerifyLogoutToken(rotatedToken, opts(keys...)); err != nil || ev.Subject != "sub-1" {
		t.Fatalf("JWKS-only key: %v", err)
	}
	res.KeysForToken(ctx, sign("logout+jwt", logoutClaims(nil), newKeyPair(t), nil))
	if fetches != 1 {
		t.Fatal("a second unknown kid inside a minute refetched")
	}
	clock += 61
	res.KeysForToken(ctx, sign("logout+jwt", logoutClaims(nil), newKeyPair(t), nil))
	if fetches != 2 {
		t.Fatal("after a minute an unknown kid may refetch")
	}
}

func TestKeyResolverStaleRemoteKeyRecheckedAndDropped(t *testing.T) {
	primary, rotated := newKeyPair(t), newKeyPair(t)
	published := []ed25519.PublicKey{rotated.raw}
	fetches := 0
	clock := int64(0)
	hc := client(func(*http.Request) (*http.Response, error) {
		fetches++
		return respond(200, jwksBody(published...)), nil
	})
	res := NewKeyResolver(issuer, []ed25519.PublicKey{primary.raw}, hc, func() int64 { return clock })
	ctx := context.Background()
	token := sign("logout+jwt", logoutClaims(nil), rotated, nil)
	res.KeysForToken(ctx, token)
	clock += 300
	res.KeysForToken(ctx, token)
	if fetches != 1 {
		t.Fatal("inside the cache window a known remote key was refetched")
	}
	published = nil
	clock += 601
	keys := res.KeysForToken(ctx, token)
	if fetches != 2 {
		t.Fatal("a stale cache is refreshed even for a known kid")
	}
	if _, err := VerifyLogoutToken(token, opts(keys...)); !isCentralErr(err) {
		t.Fatal("retired key still verifies")
	}
	res.KeysForToken(ctx, sign("logout+jwt", logoutClaims(nil), primary, nil))
	if fetches != 2 {
		t.Fatal("a static key waited on Auth")
	}
}

func TestKeyResolverFailedOrHostileJWKSKeepsCache(t *testing.T) {
	primary, rotated := newKeyPair(t), newKeyPair(t)
	mode := "ok"
	clock := int64(0)
	hc := client(func(*http.Request) (*http.Response, error) {
		switch mode {
		case "fail":
			return nil, errors.New("ECONNREFUSED")
		case "huge":
			return respond(200, strings.Repeat("x", 70*1024)), nil
		case "redirect":
			r := respond(302, "")
			r.Header.Set("Location", "https://evil.example/jwks.json")
			return r, nil
		}
		return respond(200, jwksBody(rotated.raw)), nil
	})
	res := NewKeyResolver(issuer, []ed25519.PublicKey{primary.raw}, hc, func() int64 { return clock })
	ctx := context.Background()
	res.KeysForToken(ctx, sign("logout+jwt", logoutClaims(nil), newKeyPair(t), nil))
	if n := len(res.Known()); n != 2 {
		t.Fatalf("known = %d", n)
	}
	for _, mode = range []string{"fail", "huge", "redirect"} {
		clock += 61
		res.KeysForToken(ctx, sign("logout+jwt", logoutClaims(nil), newKeyPair(t), nil))
		if n := len(res.Known()); n != 2 {
			t.Fatalf("%s: cache not kept (%d)", mode, n)
		}
	}
}

func testConfig(t *testing.T, primary keyPair) Config {
	t.Helper()
	cfg, err := LoadConfig(MapEnv(map[string]string{
		"AUTH_ISSUER_URL":     issuer,
		"DASHBOARD_HOST":      "pages.northwind.example",
		"AUTH_CLIENT_SECRET":  strings.Repeat("s", 40),
		"AUTH_SIGNING_PUBKEY": base64.StdEncoding.EncodeToString(primary.raw),
		"PAGE_COOKIE_SECRET":  "unit-test-secret",
	}))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

type exchangeFixture struct {
	primary, rotated keyPair
}

func (fx exchangeFixture) client(t *testing.T, f rtFunc) *Client {
	cfg := testConfig(t, fx.primary)
	jwks := client(func(*http.Request) (*http.Response, error) { return respond(200, jwksBody(fx.rotated.raw)), nil })
	keys := NewKeyResolver(cfg.Issuer, cfg.StaticKeys, jwks, nil)
	c, err := NewClient(cfg, keys)
	if err != nil {
		t.Fatal(err)
	}
	c.HTTP = client(f)
	return c
}

func idClaims(extra claims) claims {
	return claims{
		"iss": issuer, "aud": "pages", "sub": "sub-1", "email": "Ana@Northwind.example", "iat": unix(), "exp": unix() + 60,
		"nonce": "n1", "auth_time": unix(), "amr": []string{"pwd"}, "acr": "urn:elcanotek:loa:1",
	}.with(extra)
}

// tokenResponse is Auth's /token answer: the claims in the clear and as a
// signed id_token.
func tokenResponse(c claims, idToken any) *http.Response {
	answer := c.with(nil)
	answer["id_token"] = idToken
	return respond(200, string(mustJSON(answer)))
}

func TestClientExchangePostsCodeAndVerifiesIDToken(t *testing.T) {
	fx := exchangeFixture{newKeyPair(t), newKeyPair(t)}
	var seen *http.Request
	var form url.Values
	c := fx.client(t, func(r *http.Request) (*http.Response, error) {
		seen = r
		b, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(b))
		return tokenResponse(idClaims(nil), sign("JWT", idClaims(nil), fx.primary, nil)), nil
	})
	p, err := c.Exchange(context.Background(), "c1", "v1", "n1")
	if err != nil {
		t.Fatal(err)
	}
	if p != (Principal{Subject: "sub-1", Email: "ana@northwind.example"}) {
		t.Fatalf("principal = %+v", p)
	}
	if seen.URL.String() != issuer+"/token" || seen.Method != http.MethodPost {
		t.Fatalf("request = %s %s", seen.Method, seen.URL)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("pages:"+strings.Repeat("s", 40)))
	if seen.Header.Get("Authorization") != want {
		t.Fatalf("authorization = %q", seen.Header.Get("Authorization"))
	}
	if form.Get("redirect_uri") != "https://pages.northwind.example/auth/callback" || form.Get("code_verifier") != "v1" || form.Get("grant_type") != "authorization_code" {
		t.Fatalf("form = %v", form)
	}
}

func TestClientExchangeJWKSOnlyKey(t *testing.T) {
	fx := exchangeFixture{newKeyPair(t), newKeyPair(t)}
	c := fx.client(t, func(*http.Request) (*http.Response, error) {
		return tokenResponse(idClaims(nil), sign("JWT", idClaims(nil), fx.rotated, nil)), nil
	})
	if p, err := c.Exchange(context.Background(), "c", "v", "n1"); err != nil || p.Subject != "sub-1" {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestClientExchangeIdentityFromSignedClaimsOnly(t *testing.T) {
	fx := exchangeFixture{newKeyPair(t), newKeyPair(t)}
	c := fx.client(t, func(*http.Request) (*http.Response, error) {
		return tokenResponse(idClaims(claims{"sub": "sub-evil", "email": "evil@northwind.example"}), sign("JWT", idClaims(nil), fx.primary, nil)), nil
	})
	p, err := c.Exchange(context.Background(), "c", "v", "n1")
	if err != nil || p != (Principal{Subject: "sub-1", Email: "ana@northwind.example"}) {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestClientExchangeRefusesUntrustedAnswers(t *testing.T) {
	fx := exchangeFixture{newKeyPair(t), newKeyPair(t)}
	other := newKeyPair(t)
	signedWith := func(c claims) *http.Response { return tokenResponse(idClaims(nil), sign("JWT", c, fx.primary, nil)) }
	cases := map[string]func() *http.Response{
		"nonce mismatch":               func() *http.Response { return signedWith(idClaims(claims{"nonce": "n2"})) },
		"no nonce":                     func() *http.Response { return signedWith(idClaims(claims{"nonce": nil})) },
		"other audience":               func() *http.Response { return signedWith(idClaims(claims{"aud": "explorer"})) },
		"audience array":               func() *http.Response { return signedWith(idClaims(claims{"aud": []string{"pages"}})) },
		"other issuer":                 func() *http.Response { return signedWith(idClaims(claims{"iss": "https://evil.example"})) },
		"issuer with a trailing slash": func() *http.Response { return signedWith(idClaims(claims{"iss": issuer + "/"})) },
		"expired":                      func() *http.Response { return signedWith(idClaims(claims{"exp": unix() - 120})) },
		"issued in the future":         func() *http.Response { return signedWith(idClaims(claims{"iat": unix() + 600})) },
		"no subject":                   func() *http.Response { return signedWith(idClaims(claims{"sub": ""})) },
		"bad email":                    func() *http.Response { return signedWith(idClaims(claims{"email": "nope"})) },
		"missing id_token":             func() *http.Response { return respond(200, string(mustJSON(idClaims(nil)))) },
		"id_token not a string":        func() *http.Response { return tokenResponse(idClaims(nil), map[string]any{"a": 1}) },
		"unknown key":                  func() *http.Response { return tokenResponse(idClaims(nil), sign("JWT", idClaims(nil), other, nil)) },
		"trusted kid, other key's signature": func() *http.Response {
			return tokenResponse(idClaims(nil), sign("JWT", idClaims(nil), other, map[string]any{"kid": KidForKey(fx.primary.raw)}))
		},
		"wrong typ": func() *http.Response {
			return tokenResponse(idClaims(nil), sign("logout+jwt", idClaims(nil), fx.primary, nil))
		},
		"alg none": func() *http.Response {
			return tokenResponse(idClaims(nil), sign("JWT", idClaims(nil), fx.primary, map[string]any{"alg": "none"}))
		},
		"tampered claims": func() *http.Response {
			parts := strings.Split(sign("JWT", idClaims(nil), fx.primary, nil), ".")
			body := b64u(mustJSON(idClaims(claims{"sub": "sub-evil"})))
			return tokenResponse(idClaims(nil), parts[0]+"."+body+"."+parts[2])
		},
		"redirect not followed": func() *http.Response {
			r := respond(302, "")
			r.Header.Set("Location", "https://evil.example")
			return r
		},
		"oversized": func() *http.Response { return respond(200, strings.Repeat("x", 70*1024)) },
	}
	for name, answer := range cases {
		c := fx.client(t, func(*http.Request) (*http.Response, error) { return answer(), nil })
		if _, err := c.Exchange(context.Background(), "c", "v", "n1"); !isCentralErr(err) || errors.Is(err, ErrCodeExchangeRejected) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	c := fx.client(t, func(*http.Request) (*http.Response, error) { return respond(400, "{}"), nil })
	if _, err := c.Exchange(context.Background(), "c", "v", "n1"); !errors.Is(err, ErrCodeExchangeRejected) {
		t.Errorf("400 is a retry: %v", err)
	}
	c = fx.client(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("ECONNREFUSED") })
	if _, err := c.Exchange(context.Background(), "c", "v", "n1"); !isCentralErr(err) {
		t.Errorf("unreachable: %v", err)
	}
}

func TestNewClientNeedsKeyResolver(t *testing.T) {
	if _, err := NewClient(Config{}, nil); err == nil {
		t.Fatal("client without a key resolver")
	}
}

func TestLoginTransactionSealedSingleStateExpires(t *testing.T) {
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	tx := NewTransaction("/admin/northwind?x=1", 1000)
	sum := sha256.Sum256([]byte(tx.Verifier))
	if tx.Challenge != b64u(sum[:]) {
		t.Fatal("S256 challenge")
	}
	sealed := SealTransaction(tx, key)
	got, err := OpenTransaction(sealed, tx.State, key, 1000)
	if err != nil || got != (OpenedTransaction{Nonce: tx.Nonce, Verifier: tx.Verifier, Next: "/admin/northwind?x=1"}) {
		t.Fatalf("%+v %v", got, err)
	}
	otherKey := make([]byte, 32)
	_, _ = rand.Read(otherKey)
	body, mac, _ := strings.Cut(sealed, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	m["next"] = "//evil.example"
	tampered := b64u(mustJSON(m))
	for name, c := range map[string]struct {
		sealed, state string
		key           []byte
		at            int64
	}{
		"state mismatch":  {sealed, "other-state", key, 1000},
		"other key":       {sealed, tx.State, otherKey, 1000},
		"expired":         {sealed, tx.State, key, 1000 + 600},
		"tampered":        {tampered + "." + mac, tx.State, key, 1000},
		"missing cookie":  {"", tx.State, key, 1000},
		"from the future": {sealed, tx.State, key, 1000 - 61},
	} {
		if _, err := OpenTransaction(c.sealed, c.state, c.key, c.at); !errors.Is(err, ErrAuthTransaction) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestSealTransactionMatchesJSLayout(t *testing.T) {
	tx := Transaction{State: "s", Nonce: "n", Verifier: "v", Next: "/admin?a=<b>&c", IAT: 7}
	body, _, _ := strings.Cut(SealTransaction(tx, []byte("k")), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	if string(raw) != `{"state":"s","nonce":"n","verifier":"v","next":"/admin?a=<b>&c","iat":7}` {
		t.Fatalf("body = %s", raw)
	}
}

func TestSafeLocalPath(t *testing.T) {
	if got := SafeLocalPath("/admin/northwind?x=1"); got != "/admin/northwind?x=1" {
		t.Fatal(got)
	}
	for _, raw := range []string{"//evil.example", `/\evil.example`, "https://evil.example/", "admin", "", "/a\x00b", "/a\nb", "/" + strings.Repeat("a", 2100), "/%2F%2Fevil.example"} {
		out := SafeLocalPath(raw)
		if !strings.HasPrefix(out, "/") || strings.HasPrefix(out, "//") {
			t.Errorf("%q → %q", raw, out)
		}
		u, _ := url.Parse("https://pages.northwind.example" + out)
		if u.Host != "pages.northwind.example" {
			t.Errorf("%q escaped to %s", raw, u.Host)
		}
	}
	for _, raw := range []string{"//evil.example", "/a/..//evil.example/x", "/%2e//evil.example/x"} {
		if got := SafeLocalPath(raw); got != "/admin" {
			t.Errorf("%q → %q", raw, got)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	primary := newKeyPair(t)
	env := map[string]string{
		"AUTH_ISSUER_URL":     issuer,
		"DASHBOARD_HOST":      "pages.northwind.example",
		"AUTH_CLIENT_SECRET":  strings.Repeat("s", 40),
		"AUTH_SIGNING_PUBKEY": base64.StdEncoding.EncodeToString(primary.raw),
		"PAGE_COOKIE_SECRET":  "unit-test-secret",
	}
	with := func(override map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range env {
			out[k] = v
		}
		for k, v := range override {
			out[k] = v
		}
		return out
	}
	cfg, err := LoadConfig(MapEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicURL != "https://pages.northwind.example" || cfg.ClientID != "pages" || !cfg.CookieSecure || len(cfg.TransactionKey) != 32 {
		t.Fatalf("cfg = %+v", cfg)
	}
	broken := map[string]map[string]string{
		"http issuer":                      {"AUTH_ISSUER_URL": "http://auth.northwind.example"},
		"issuer with path":                 {"AUTH_ISSUER_URL": issuer + "/x"},
		"short secret":                     {"AUTH_CLIENT_SECRET": "short"},
		"colon in secret":                  {"AUTH_CLIENT_SECRET": strings.Repeat("s", 40) + ":"},
		"no pubkey":                        {"AUTH_SIGNING_PUBKEY": ""},
		"bad pubkey":                       {"AUTH_SIGNING_PUBKEY": "AAAA"},
		"bad previous pubkey":              {"AUTH_SIGNING_PREVIOUS_PUBKEYS": "AAAA"},
		"insecure cookie without dev flag": {"PAGES_AUTH_COOKIE_SECURE": "0"},
		"no cookie secret":                 {"PAGE_COOKIE_SECRET": ""},
		"public URL on another host":       {"PAGES_PUBLIC_URL": "https://pages.other.example"},
		"no dashboard host":                {"DASHBOARD_HOST": "", "PAGES_PUBLIC_URL": "https://pages.northwind.example"},
		"insecure options in production":   {"NODE_ENV": "production", "AUTH_ISSUER_URL": "http://127.0.0.1:9000", "AUTH_ALLOW_INSECURE_HTTP": "1"},
		"issuer with query":                {"AUTH_ISSUER_URL": issuer + "?x=1"},
		"issuer with credentials":          {"AUTH_ISSUER_URL": "https://u:p@auth.northwind.example"},
		"issuer not a URL":                 {"AUTH_ISSUER_URL": "auth.northwind.example"},
	}
	for name, override := range broken {
		if _, err := LoadConfig(MapEnv(with(override))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	dev, err := LoadConfig(MapEnv(with(map[string]string{"AUTH_ISSUER_URL": "http://127.0.0.1:9000", "AUTH_ALLOW_INSECURE_HTTP": "1", "PAGES_AUTH_COOKIE_SECURE": "0"})))
	if err != nil || dev.CookieSecure {
		t.Fatalf("dev: %+v %v", dev, err)
	}
	if dev.Issuer != "http://127.0.0.1:9000" {
		t.Fatalf("issuer = %q", dev.Issuer)
	}
	canon, err := LoadConfig(MapEnv(with(map[string]string{"AUTH_ISSUER_URL": "HTTPS://Auth.Northwind.Example:443/"})))
	if err != nil || canon.Issuer != issuer {
		t.Fatalf("canonical issuer: %q %v", canon.Issuer, err)
	}
}

// The HKDF-derived transaction key must equal Node's
// crypto.hkdfSync("sha256", secret, empty salt, info, 32).
func TestTransactionKeyMatchesNode(t *testing.T) {
	cfg := testConfig(t, newKeyPair(t))
	// node -e 'console.log(Buffer.from(crypto.hkdfSync("sha256",Buffer.from("unit-test-secret"),Buffer.alloc(0),"pages/central-auth/login-transaction/v1",32)).toString("hex"))'
	const nodeHKDF = "429eb651916f58396b6b53e920d42a1fce77edaf9e3174fc1bdccb5e3f2203e7"
	if got := hex.EncodeToString(cfg.TransactionKey); got != nodeHKDF {
		t.Fatalf("transaction key = %s, Node %s", got, nodeHKDF)
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got, err := NormalizeEmail("  Ana@Northwind.Example "); err != nil || got != "ana@northwind.example" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"", "ana", "@northwind.example", "ana@", "a@b@c", "ana@.example", "ana@example.", "a b@example.com", "a\x07@example.com"} {
		if _, err := NormalizeEmail(bad); !isCentralErr(err) {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestStripTrailingSlashes(t *testing.T) {
	for in, want := range map[string]string{"https://auth.example///": "https://auth.example", "https://auth.example": "https://auth.example", "///": ""} {
		if got := StripTrailingSlashes(in); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
	long := strings.Repeat("/", 200000) + "x"
	start := time.Now()
	if StripTrailingSlashes(long) != long || time.Since(start) > time.Second {
		t.Fatal("not linear")
	}
}
