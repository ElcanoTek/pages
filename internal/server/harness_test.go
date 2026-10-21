// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/versions"
)

const (
	dashHost    = "localhost"
	contentHost = "content.localhost"
	origin      = "http://localhost"
	adminEmail  = "admin@elcanotek.com"
)

var seed = versions.Actor{Actor: "seed@agent", ActorType: "agent", IP: "127.0.0.1"}

// baseEnv is what test/run-integration.sh exported, with every setting a
// test may flip reset so the developer's environment cannot leak in.
func baseEnv(t *testing.T, pub ed25519.PublicKey) {
	t.Helper()
	for k, v := range map[string]string{
		"PAGES_AUTH_MODE":     "",
		"AUTH_SIGNING_PUBKEY": base64.StdEncoding.EncodeToString(pub),
		"AUTH_COOKIE_NAME":    "",
		"AUTH_LOGIN_URL":      "",
		"ADMIN_EMAIL_DOMAIN":  "",
		"PAGES_DEV_LOGIN":     "",
		"DEV_ADMIN_COOKIE":    "",
		"PAGES_COMPOSE":       "",
		"PAGES_ENV":           "",
		"NODE_ENV":            "",
		"PAGES_TRUST_PROXY":   "",
		"CONTENT_HOST_ALSO":   "",
		"PAGE_COOKIE_SECRET":  "server-test-page-secret",
		"RAW_TOKEN_SECRET":    "server-test-raw-secret",
		"API_TOKEN_PEPPER":    "server-test-pepper",
		"DASHBOARD_HOST":      dashHost,
		"CONTENT_HOST":        contentHost,
		"DASHBOARD_ORIGIN":    "",
		"CONTENT_ORIGIN":      "",
		"PG_LOCK_TIMEOUT_MS":  "2000",
		"RL_API_PER_MIN":      "600",
		"RL_CONTENT_PER_MIN":  "",
		"RL_PASSWORD_TRIES":   "",
		"RL_MCP_PER_MIN":      "",
		"MAX_HTML_BYTES":      "",
	} {
		t.Setenv(k, v)
	}
}

type testEnv struct {
	t       *testing.T
	d       *db.DB
	srv     *httptest.Server
	s       *Server
	priv    ed25519.PrivateKey
	adminCk string
	outCk   string
}

// setup builds a server on a fresh database. tweak runs after the base
// environment and before New, so a test can set limits or modes.
func setup(t *testing.T, tweak func(), cfg ...func(*Config)) *testEnv {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	baseEnv(t, pub)
	if tweak != nil {
		tweak()
	}
	d := testdb.New(t)
	c := Config{DB: d, Ready: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})}
	for _, f := range cfg {
		f(&c)
	}
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	e := &testEnv{t: t, d: d, srv: srv, s: s, priv: priv}
	e.adminCk = "elcano_auth=" + e.mintCookie(adminEmail)
	e.outCk = "elcano_auth=" + e.mintCookie("outsider@example.com")
	return e
}

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
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("not JSON (%d): %s", r.status, r.body)
	}
	return m
}

type opt struct {
	host        string
	cookie      string
	body        string
	contentType string
	headers     map[string]string
}

func (e *testEnv) do(method, path string, o opt) resp {
	e.t.Helper()
	var body io.Reader
	if o.body != "" {
		body = strings.NewReader(o.body)
	}
	r, err := http.NewRequestWithContext(context.Background(), method, e.srv.URL+path, body)
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
	if o.body != "" {
		ct := o.contentType
		if ct == "" {
			ct = "application/json"
		}
		r.Header.Set("Content-Type", ct)
	}
	for k, v := range o.headers {
		r.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return resp{res.StatusCode, res.Header, string(b)}
}

func (e *testEnv) get(path string, o opt) resp { e.t.Helper(); return e.do(http.MethodGet, path, o) }

// publish creates slug with one published version.
func (e *testEnv) publish(slug, html string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := versions.CreatePage(ctx, e.d, versions.CreatePageInput{Slug: slug, Title: slug}, seed); err != nil {
		e.t.Fatal(err)
	}
	if _, err := versions.Deploy(ctx, e.d, versions.DeployInput{Slug: slug, HTML: html, Publish: true}, seed); err != nil {
		e.t.Fatal(err)
	}
}

func ed25519GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}
