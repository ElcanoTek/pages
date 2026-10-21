// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package adminapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/api"
	"github.com/ElcanoTek/pages/internal/auth"
	"github.com/ElcanoTek/pages/internal/contentview"
	"github.com/ElcanoTek/pages/internal/csrf"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/reqbody"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/versions"
)

const (
	dashHost    = "localhost"
	contentHost = "content.localhost"
	// origin matches Host: localhost (no port), so the csrf Origin check
	// passes.
	origin = "http://localhost"
	admin  = "admin@elcanotek.com"
)

// seed is the agent identity the JS tests used to set up state directly
// through the state machine.
var seed = versions.Actor{Actor: "seed@agent", ActorType: "agent", IP: "127.0.0.1"}

// env sets up a fresh database, an Ed25519 SSO keypair, and a server with
// the dashboard's mount order (reqbody.Middleware → admin API → REST API)
// and the content host behind the same Host split server.go does.
type testEnv struct {
	t         *testing.T
	d         *db.DB
	srv       *httptest.Server
	priv      ed25519.PrivateKey
	auth      *auth.Authenticator
	adminCk   string
	outsideCk string
}

func setup(t *testing.T) *testEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"PAGES_AUTH_MODE":     "",
		"AUTH_SIGNING_PUBKEY": base64.StdEncoding.EncodeToString(pub),
		"AUTH_COOKIE_NAME":    "",
		"ADMIN_EMAIL_DOMAIN":  "",
		"PAGES_DEV_LOGIN":     "",
		"DEV_ADMIN_COOKIE":    "",
		"PAGES_ENV":           "",
		"NODE_ENV":            "",
		"ADMIN_CSRF_SECRET":   "",
		"PAGE_COOKIE_SECRET":  "admin-test-csrf-secret",
		"RAW_TOKEN_SECRET":    "admin-test-raw-secret",
		"API_TOKEN_PEPPER":    "admin-test-pepper",
		"DASHBOARD_HOST":      dashHost,
		"CONTENT_HOST":        contentHost,
		"DASHBOARD_ORIGIN":    "",
		"CONTENT_ORIGIN":      "",
		"PG_LOCK_TIMEOUT_MS":  "2000",
	} {
		t.Setenv(k, v)
	}
	d := testdb.New(t)
	a, err := auth.NewFromEnv(d)
	if err != nil {
		t.Fatal(err)
	}
	rest := api.Handler(api.Deps{DB: d})
	dash := reqbody.Middleware(reqbody.DashboardOptions(), func(w http.ResponseWriter, r *http.Request, err *reqbody.Error) {
		writeAPIError(w, r, err.APIError())
	})(Handler(Deps{DB: d, Auth: a, Next: rest}))
	content := contentview.ContentHandler(contentview.Deps{
		DB: d,
		TemplatePreview: func(ctx context.Context, id int64) (string, error) {
			p, err := templates.PreviewBytes(ctx, d, jsx.ID(id), time.Now())
			if err != nil {
				return "", err
			}
			return p.HTML, nil
		},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentview.IsContentHost(r) {
			content.ServeHTTP(w, r)
			return
		}
		dash.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e := &testEnv{t: t, d: d, srv: srv, priv: priv, auth: a}
	e.adminCk = "elcano_auth=" + e.mintCookie(admin)
	e.outsideCk = "elcano_auth=" + e.mintCookie("outsider@example.com")
	return e
}

// mintCookie signs an SSO cookie the way auth.elcanotek.com does.
func (e *testEnv) mintCookie(email string) string {
	now := time.Now().Unix()
	b, _ := json.Marshal(map[string]any{"email": email, "tenant": "", "iat": now, "exp": now + 3600})
	body := base64.RawURLEncoding.EncodeToString(b)
	return body + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(e.priv, []byte(body)))
}

type resp struct {
	status int
	header http.Header
	body   string
	json   any
}

// at walks objects by key and arrays by decimal index.
func (r resp) at(path ...any) any { return at(r.json, path...) }

func (r resp) str(path ...any) string { s, _ := r.at(path...).(string); return s }

func at(cur any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = m[k]
		case int:
			a, ok := cur.([]any)
			if !ok || k >= len(a) {
				return nil
			}
			cur = a[k]
		}
	}
	return cur
}

func list(v any) []any { a, _ := v.([]any); return a }

// find is the first element of xs whose key equals want.
func find(xs any, key string, want any) map[string]any {
	for _, x := range list(xs) {
		if m, ok := x.(map[string]any); ok && m[key] == want {
			return m
		}
	}
	return nil
}

func pluck(xs any, key string) []string {
	out := []string{}
	for _, x := range list(xs) {
		s, _ := at(x, key).(string)
		out = append(out, s)
	}
	return out
}

type opts struct {
	host   string
	cookie string
	csrf   bool
	origin string
	body   any
	raw    *string
	form   url.Values
	bearer string
}

func (e *testEnv) req(method, path string, o opts) resp {
	e.t.Helper()
	var payload []byte
	ctype := ""
	switch {
	case o.raw != nil:
		payload, ctype = []byte(*o.raw), "application/json"
	case o.form != nil:
		payload, ctype = []byte(o.form.Encode()), "application/x-www-form-urlencoded"
	case o.body != nil:
		var err error
		if payload, err = json.Marshal(o.body); err != nil {
			e.t.Fatal(err)
		}
		ctype = "application/json"
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	r, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	r.Host = dashHost
	if o.host != "" {
		r.Host = o.host
	}
	if o.cookie != "" {
		r.Header.Set("Cookie", o.cookie)
	}
	if o.csrf {
		r.Header.Set("X-CSRF-Token", csrf.Mint(admin))
	}
	if o.origin != "" {
		r.Header.Set("Origin", o.origin)
	}
	if o.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+o.bearer)
	}
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	client := e.srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, header: res.Header, body: string(b)}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out.json)
	}
	return out
}

func (e *testEnv) get(path string) resp { return e.req("GET", path, opts{cookie: e.adminCk}) }

func (e *testEnv) post(path string, body any) resp {
	if body == nil {
		body = map[string]any{}
	}
	return e.req("POST", path, opts{cookie: e.adminCk, csrf: true, origin: origin, body: body})
}

func expect(t *testing.T, r resp, status int, what string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: status %d, want %d (%s)", what, r.status, status, r.body)
	}
}

func expectCode(t *testing.T, r resp, status int, code, what string) {
	t.Helper()
	expect(t, r, status, what)
	if got := r.str("code"); got != code {
		t.Fatalf("%s: code %q, want %q (%s)", what, got, code, r.body)
	}
}

func eq(t *testing.T, got, want any, what string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func html(n any) string {
	return fmt.Sprintf(`<!doctype html><html><head><title>Adm</title></head><body><h1>v%v</h1></body></html>`, n)
}

// createPage / deploy are the seed helpers (agent actor, straight through
// the state machine).
func (e *testEnv) createPage(in versions.CreatePageInput) *versions.CreatedPage {
	e.t.Helper()
	return must[*versions.CreatedPage](e.t)(versions.CreatePage(context.Background(), e.d, in, seed))
}

func (e *testEnv) deploy(slug, h string, publish bool) versions.Version {
	e.t.Helper()
	return must[*versions.DeployResult](e.t)(versions.Deploy(context.Background(), e.d,
		versions.DeployInput{Slug: slug, HTML: h, Publish: publish}, seed)).Version
}

func (e *testEnv) auditCount(action string) int {
	e.t.Helper()
	var n int
	if err := e.d.QueryRow(context.Background(), `SELECT count(*)::int FROM audit_log WHERE action = $1`, action).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// contentPath strips the content origin from a minted URL.
func contentPath(t *testing.T, u string) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return p.RequestURI()
}
