// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package csrf_test

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/auth"
	"github.com/ElcanoTek/pages/internal/csrf"
)

func setSecret(t *testing.T, s string) {
	t.Setenv("ADMIN_CSRF_SECRET", s)
	t.Setenv("PAGE_COOKIE_SECRET", "")
	t.Setenv("RAW_TOKEN_SECRET", "")
}

func TestMintVerify(t *testing.T) {
	setSecret(t, "csrf-test-secret")
	tok := csrf.Mint("ana@elcanotek.com")
	if !csrf.Verify(tok, "ana@elcanotek.com") {
		t.Fatal("fresh token refused")
	}
	if csrf.Verify(tok, "ben@elcanotek.com") {
		t.Fatal("token for another email accepted")
	}
	body, sig, _ := strings.Cut(tok, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(body)
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	if !strings.HasPrefix(string(raw), `{"sub":"ana@elcanotek.com","exp":`) || !expNear(int64(p["exp"].(float64)), time.Now().Unix()+csrf.TTLSeconds) {
		t.Fatalf("payload = %s", raw)
	}
	// Flip the first signature character: it always changes the decoded
	// bytes, where the last one may only carry padding bits.
	flipped := "A"
	if sig[0] == 'A' {
		flipped = "B"
	}
	for name, bad := range map[string]string{
		"empty": "", "no dot": body, "bad sig": body + "." + flipped + sig[1:], "short sig": body + "." + sig[:10],
	} {
		if csrf.Verify(bad, "ana@elcanotek.com") {
			t.Errorf("%s accepted", name)
		}
	}
	// A token minted under one secret is refused under another, and the
	// fallback chain is ADMIN_CSRF_SECRET → PAGE_COOKIE_SECRET → RAW_TOKEN_SECRET.
	setSecret(t, "")
	t.Setenv("RAW_TOKEN_SECRET", "csrf-test-secret")
	if !csrf.Verify(tok, "ana@elcanotek.com") {
		t.Fatal("RAW_TOKEN_SECRET fallback")
	}
	t.Setenv("PAGE_COOKIE_SECRET", "other")
	if csrf.Verify(tok, "ana@elcanotek.com") {
		t.Fatal("PAGE_COOKIE_SECRET must take precedence over RAW_TOKEN_SECRET")
	}
}

// A token Node minted (same HMAC over the same body) verifies here.
func TestVerifyNodeShapedToken(t *testing.T) {
	setSecret(t, "k")
	body := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"ana@elcanotek.com","exp":` + itoa(time.Now().Unix()+60) + `}`))
	m := hmac.New(sha256.New, []byte("k"))
	m.Write([]byte(body))
	if !csrf.Verify(body+"."+base64.RawURLEncoding.EncodeToString(m.Sum(nil)), "ana@elcanotek.com") {
		t.Fatal("node-shaped token refused")
	}
	old := csrf.Now
	csrf.Now = func() time.Time { return time.Now().Add(61 * time.Second) }
	defer func() { csrf.Now = old }()
	if csrf.Verify(body+"."+base64.RawURLEncoding.EncodeToString(m.Sum(nil)), "ana@elcanotek.com") {
		t.Fatal("expired token accepted")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestRequireAdminJSONAndCsrf(t *testing.T) {
	setSecret(t, "csrf-test-secret")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("PAGES_AUTH_MODE", "")
	t.Setenv("AUTH_SIGNING_PUBKEY", base64.StdEncoding.EncodeToString(pub))
	t.Setenv("AUTH_COOKIE_NAME", "")
	t.Setenv("ADMIN_EMAIL_DOMAIN", "")
	a, err := auth.NewFromEnv(nil)
	if err != nil {
		t.Fatal(err)
	}
	cookie := func(email string) string {
		b, _ := json.Marshal(map[string]any{"email": email, "exp": time.Now().Unix() + 600})
		body := base64.RawURLEncoding.EncodeToString(b)
		return "elcano_auth=" + body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(body)))
	}
	h := csrf.RequireAdminJSON(a)(csrf.RequireCsrf(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })))
	do := func(cookie string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/admin/pages/x/approve", nil)
		r.Host = "pages.northwind.example"
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	type want struct {
		status int
		body   string
	}
	staff := cookie("ana@elcanotek.com")
	tok := csrf.Mint("ana@elcanotek.com")
	for name, c := range map[string]struct {
		cookie  string
		headers map[string]string
		want    want
	}{
		"anonymous":      {"", nil, want{401, `{"error":"not signed in","code":"no_session"}`}},
		"not admin":      {cookie("ana@northwind.example"), nil, want{403, `{"error":"Elcano staff only","code":"not_admin"}`}},
		"no origin":      {staff, map[string]string{"X-CSRF-Token": tok}, want{403, `{"error":"cross-origin request blocked","code":"bad_origin"}`}},
		"foreign origin": {staff, map[string]string{"Origin": "https://evil.example", "X-CSRF-Token": tok}, want{403, `{"error":"cross-origin request blocked","code":"bad_origin"}`}},
		"null origin":    {staff, map[string]string{"Origin": "null", "X-CSRF-Token": tok}, want{403, `{"error":"cross-origin request blocked","code":"bad_origin"}`}},
		"no token":       {staff, map[string]string{"Origin": "http://pages.northwind.example"}, want{403, `{"error":"missing or invalid CSRF token","code":"bad_csrf"}`}},
		"other's token":  {staff, map[string]string{"Origin": "http://pages.northwind.example", "X-CSRF-Token": csrf.Mint("ben@elcanotek.com")}, want{403, `{"error":"missing or invalid CSRF token","code":"bad_csrf"}`}},
		"ok":             {staff, map[string]string{"Origin": "http://pages.northwind.example", "X-CSRF-Token": tok}, want{204, ""}},
		"forwarded": {staff, map[string]string{
			"Origin": "https://pages.northwind.example", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "pages.northwind.example", "X-CSRF-Token": tok,
		}, want{204, ""}},
	} {
		w := do(c.cookie, c.headers)
		if w.Code != c.want.status || w.Body.String() != c.want.body {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
		if c.want.status == 401 && (w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("ETag") != `W/"2d-KKuoxe4wVK6iamsFJ3hMqswicGM"`) {
			t.Errorf("401 headers %v (Express: application/json; charset=utf-8, W/\"2d-KKuoxe4wVK6iamsFJ3hMqswicGM\")", w.Header())
		}
	}
	// RequireCsrf without RequireAdminJSON in front refuses.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("Origin", "http://example.com")
	csrf.RequireCsrf(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("reached") })).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("no session: %d", w.Code)
	}
}

// expNear allows for the clock ticking a second between Mint and the check.
func expNear(got, want int64) bool { return got == want || got == want-1 }
