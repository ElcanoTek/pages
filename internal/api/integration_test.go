// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

// Port of test/api.integration.js: bearer auth → version state machine →
// pointer-is-truth → the SAME signed-token /raw render, end to end:
//
//	create page → deploy+publish → read back live → render at /raw →
//	deploy a 2nd version → history → rollback → dedupe → optimistic-concurrency
//	409 → auth 401 → approval-gate (agent gets `pending`, publish forbidden).
//
// The content host is internal/contentview's handler behind the same Host
// split the server does. Not covered here (server wiring, owned by
// internal/server): the dashboard's HTML 400 page for a malformed body on a
// non-API path (POST /admin), and /api/v1/admin/* actually reaching the admin
// router — its body-parser contract is asserted through this handler, which
// runs the same parsers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/contentview"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/routens"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

const (
	dashHost    = "localhost"
	contentHost = "content.localhost"
)

func pageHTML(n int) string {
	return fmt.Sprintf(`<!doctype html><html><head><title>Acme</title></head><body><h1>Report %d</h1>`+
		`<canvas id=c></canvas><script>chart(%d)</script></body></html>`, n, n)
}

type resp struct {
	status  int
	header  http.Header
	body    string
	json    map[string]any
	jsonErr error
}

func (r resp) str(path ...string) string {
	v := r.at(path...)
	s, _ := v.(string)
	return s
}

func (r resp) at(path ...string) any {
	var cur any = r.json
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

type client struct {
	t   *testing.T
	srv *httptest.Server
}

type reqOpts struct {
	host    string
	token   string
	body    any
	rawBody *string
}

func (c client) do(method, path string, o reqOpts) resp {
	c.t.Helper()
	var payload []byte
	switch {
	case o.rawBody != nil:
		payload = []byte(*o.rawBody)
	case o.body != nil:
		var err error
		if payload, err = json.Marshal(o.body); err != nil {
			c.t.Fatal(err)
		}
	}
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, c.srv.URL+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Host = dashHost
	if o.host != "" {
		req.Host = o.host
	}
	if o.token != "" {
		req.Header.Set("Authorization", "Bearer "+o.token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.srv.Client().Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	out := resp{status: res.StatusCode, header: res.Header, body: string(b)}
	if len(b) > 0 {
		out.jsonErr = json.Unmarshal(b, &out.json)
	}
	return out
}

func expectStatus(t *testing.T, r resp, status int, what string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("%s: status %d, want %d (%s)", what, r.status, status, r.body)
	}
}

func expectCode(t *testing.T, r resp, status int, code, what string) {
	t.Helper()
	expectStatus(t, r, status, what)
	if got := r.str("code"); got != code {
		t.Fatalf("%s: code %q, want %q (%s)", what, got, code, r.body)
	}
}

func TestAPIIntegration(t *testing.T) {
	t.Setenv("RAW_TOKEN_SECRET", "integration-secret")
	t.Setenv("API_TOKEN_PEPPER", "integration-pepper")
	t.Setenv("PAGE_COOKIE_SECRET", "integration-page-secret")
	t.Setenv("DASHBOARD_HOST", dashHost)
	t.Setenv("CONTENT_HOST", contentHost)
	t.Setenv("DASHBOARD_ORIGIN", "")
	t.Setenv("CONTENT_ORIGIN", "")
	t.Setenv("PG_LOCK_TIMEOUT_MS", "2000")
	t.Setenv("MAX_HTML_BYTES", "")
	d := testdb.New(t)
	ctx := context.Background()

	apiHandler := Handler(Deps{DB: d})
	content := contentview.ContentHandler(contentview.Deps{DB: d})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if contentview.IsContentHost(r) {
			content.ServeHTTP(w, r)
			return
		}
		apiHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := client{t: t, srv: srv}

	minted, err := tokens.Mint(ctx, d, tokens.MintParams{Label: "test-agent", Scope: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	token := minted.Token

	// Parser failures precede router handlers but must retain the API contract.
	// (/api/v1/admin/pages runs the same parsers in front of the admin router.)
	for _, path := range []string{"/api/v1/pages", "/api/v1/admin/pages"} {
		bad := `{"html": malformed`
		malformed := c.do("POST", path, reqOpts{token: token, rawBody: &bad})
		expectStatus(t, malformed, 400, "malformed body")
		if !strings.HasPrefix(malformed.header.Get("Content-Type"), "application/json") {
			t.Fatalf("malformed content type %q", malformed.header.Get("Content-Type"))
		}
		if malformed.body != `{"error":"invalid JSON request body","code":"bad_json"}` {
			t.Fatalf("malformed body: %s", malformed.body)
		}
		oversized := c.do("POST", path, reqOpts{token: token, body: map[string]any{"html": strings.Repeat("x", datalimits.MaxRequestBytes())}})
		expectStatus(t, oversized, 413, "oversized body")
		if oversized.body != `{"error":"request body too large","code":"body_too_large"}` || len(oversized.body) >= 200 {
			t.Fatalf("oversized body: %s", oversized.body)
		}
	}
	t.Log("✓ malformed/oversized REST bodies retain JSON errors")

	// 0. No token → 401.
	expectCode(t, c.do("POST", "/api/v1/pages", reqOpts{body: map[string]any{"slug": "x"}}), 401, "no_token", "missing bearer")

	// Boolean intent must survive JSON transport. Rejected inputs may not
	// create a page/version or write a mutation audit row.
	mutationCounts := func() [3]int64 {
		var n [3]int64
		if err := d.QueryRow(ctx, `SELECT (SELECT count(*) FROM pages), (SELECT count(*) FROM page_versions),
		       (SELECT count(*) FROM audit_log)`).Scan(&n[0], &n[1], &n[2]); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := mutationCounts()
	for _, invalid := range []any{"false", "true", 0, 1, nil, map[string]any{}, []any{}} {
		createBad := c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "northwind/bool-invalid", "require_approval": invalid}})
		expectCode(t, createBad, 400, "bad_boolean", fmt.Sprintf("require_approval=%v", invalid))
		deployBad := c.do("POST", "/api/v1/pages/northwind/bool-invalid/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(81), "publish": invalid}})
		expectCode(t, deployBad, 400, "bad_boolean", fmt.Sprintf("publish=%v", invalid))
	}
	if after := mutationCounts(); after != before {
		t.Fatalf("invalid booleans mutated: %v → %v", before, after)
	}
	boolPage := c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "northwind/bools", "require_approval": false}})
	expectStatus(t, boolPage, 201, "bool page")
	if boolPage.at("page", "require_approval") != false {
		t.Fatalf("require_approval: %s", boolPage.body)
	}
	for _, publish := range []any{nil, false} {
		n := 82
		b := map[string]any{"html": pageHTML(n)}
		if publish == false {
			b = map[string]any{"html": pageHTML(83), "publish": false}
		}
		draft := c.do("POST", "/api/v1/pages/northwind/bools/versions", reqOpts{token: token, body: b})
		expectStatus(t, draft, 201, "draft")
		if draft.at("published") != false || draft.str("version", "status") != "draft" {
			t.Fatalf("draft: %s", draft.body)
		}
	}
	if v := c.do("GET", "/api/v1/pages/northwind/bools", reqOpts{token: token}).at("page", "published_version_id"); v != nil {
		t.Fatalf("bools pointer: %v", v)
	}

	// 1. Create an open page.
	create := c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "acme", "title": "Acme"}})
	expectStatus(t, create, 201, "create page")
	if create.str("page", "slug") != "acme" || create.at("page", "published_version_id") != nil {
		t.Fatalf("create: %s", create.body)
	}

	// 2. Deploy + publish in one call (open-page fast path).
	d1 := c.do("POST", "/api/v1/pages/acme/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(1), "publish": true}})
	expectStatus(t, d1, 201, "deploy")
	if d1.at("published") != true || d1.str("version", "status") != "approved" {
		t.Fatalf("d1: %s", d1.body)
	}
	v1 := d1.str("version", "id")

	// 3. Read back: published pointer == v1.
	g1 := c.do("GET", "/api/v1/pages/acme", reqOpts{token: token})
	if g1.str("page", "published_version_id") != v1 || g1.str("published", "id") != v1 {
		t.Fatalf("g1: %s", g1.body)
	}

	// 3b. Slug is normalized: a mixed-case lookup resolves to the same page.
	mixed := c.do("GET", "/api/v1/pages/ACME", reqOpts{token: token})
	expectStatus(t, mixed, 200, "mixed-case slug")
	if mixed.str("page", "id") != g1.str("page", "id") {
		t.Fatal("mixed-case resolves to another page")
	}

	// 4. Full render loop: mint a /raw view token for the live version, fetch it
	//    on the cookieless content host → themed render with charts intact.
	rawGet := func(slug, pageID, versionID string) resp {
		t.Helper()
		pid, _ := jsx.ParseID(pageID)
		vid, _ := jsx.ParseID(versionID)
		tok, err := rawtoken.Mint(rawtoken.MintParams{PageID: int64(pid), VersionID: int64(vid), Purpose: rawtoken.View, RenderMode: "themed"}, rawtoken.DefaultTTL)
		if err != nil {
			t.Fatal(err)
		}
		return c.do("GET", "/raw/"+slug+"?t="+url.QueryEscape(tok), reqOpts{host: contentHost})
	}
	raw := rawGet("acme", g1.str("page", "id"), v1)
	expectStatus(t, raw, 200, "/raw render")
	if !regexp.MustCompile(`design-tokens\.css" data-flag-injected`).MatchString(raw.body) ||
		!strings.Contains(raw.body, `<canvas id=c></canvas><script>chart(1)</script>`) {
		t.Fatalf("/raw body: %s", raw.body)
	}

	// 5. Deploy a second version and publish → pointer moves to v2.
	d2 := c.do("POST", "/api/v1/pages/acme/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(2), "publish": true}})
	expectStatus(t, d2, 201, "deploy v2")
	v2 := d2.str("version", "id")
	if v2 == v1 {
		t.Fatal("v2 is not a new row")
	}
	if got := c.do("GET", "/api/v1/pages/acme", reqOpts{token: token}).str("page", "published_version_id"); got != v2 {
		t.Fatalf("pointer %s, want v2 %s", got, v2)
	}

	// 6. History: newest first, two versions.
	hist := c.do("GET", "/api/v1/pages/acme/versions", reqOpts{token: token})
	list, _ := hist.at("versions").([]any)
	if len(list) != 2 || list[0].(map[string]any)["id"] != v2 {
		t.Fatalf("history: %s", hist.body)
	}

	// 7. Rollback to v1 (no status change, pointer only).
	expectStatus(t, c.do("POST", "/api/v1/pages/acme/rollback", reqOpts{token: token, body: map[string]any{"version_id": v1}}), 200, "rollback")
	if got := c.do("GET", "/api/v1/pages/acme", reqOpts{token: token}).str("page", "published_version_id"); got != v1 {
		t.Fatalf("rolled back to %s, want %s", got, v1)
	}

	// 8. Dedupe: re-deploying v1's exact HTML returns the existing row, 200.
	dup := c.do("POST", "/api/v1/pages/acme/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(1)}})
	expectStatus(t, dup, 200, "identical re-deploy")
	if dup.at("deduped") != true || dup.str("version", "id") != v1 {
		t.Fatalf("dup: %s", dup.body)
	}

	// 9. Optimistic concurrency: stale expected_version → 409.
	expectCode(t, c.do("POST", "/api/v1/pages/acme/rollback", reqOpts{token: token, body: map[string]any{"version_id": v2, "expected_version": 999999}}),
		409, "stale_version", "stale expected_version")

	// 10. Approval gate: agent deploy+publish yields a PENDING version (not
	//     live), and an explicit publish is forbidden for agents.
	c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "gated", "title": "Gated", "require_approval": true}})
	gd := c.do("POST", "/api/v1/pages/gated/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(9), "publish": true}})
	expectStatus(t, gd, 201, "gated deploy")
	if gd.at("gated") != true || gd.at("published") != false || gd.str("version", "status") != "pending" {
		t.Fatalf("gated deploy: %s", gd.body)
	}
	expectCode(t, c.do("POST", "/api/v1/pages/gated/publish", reqOpts{token: token, body: map[string]any{"version_id": gd.str("version", "id")}}),
		403, "approval_required", "agent publish on gated page")
	if v := c.do("GET", "/api/v1/pages/gated", reqOpts{token: token}).at("page", "published_version_id"); v != nil {
		t.Fatalf("gated page published: %v", v)
	}

	// 11. rename + soft-delete + slug reuse.
	rn := c.do("POST", "/api/v1/pages/acme/title", reqOpts{token: token, body: map[string]any{"title": "Acme Renamed"}})
	expectStatus(t, rn, 200, "rename")
	if rn.str("title") != "Acme Renamed" {
		t.Fatalf("rename: %s", rn.body)
	}
	del := c.do("DELETE", "/api/v1/pages/acme", reqOpts{token: token})
	expectStatus(t, del, 200, "delete")
	if del.at("deleted") != true {
		t.Fatalf("delete: %s", del.body)
	}
	expectStatus(t, c.do("GET", "/api/v1/pages/acme", reqOpts{token: token}), 404, "deleted page")
	expectStatus(t, c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "acme", "title": "Acme 2"}}), 201, "recreate freed slug")

	// 12. getPage now carries routing urls (parity with MCP).
	withURLs := c.do("GET", "/api/v1/pages/acme", reqOpts{token: token})
	if !strings.HasSuffix(withURLs.str("urls", "live"), "/acme") {
		t.Fatalf("urls: %s", withURLs.body)
	}
	if withURLs.str("urls", "admin") != "https://localhost/admin/acme" || withURLs.str("urls", "view") != "https://localhost/view/acme" ||
		withURLs.str("urls", "live") != "https://content.localhost/acme" {
		t.Fatalf("urls: %s", withURLs.body)
	}

	// 13. password: agent may SET, but CLEARING is admin-only (403 admin_only).
	setPw := c.do("POST", "/api/v1/pages/acme/password", reqOpts{token: token, body: map[string]any{"password": "letmein"}})
	expectStatus(t, setPw, 200, "set password")
	if setPw.at("has_password") != true {
		t.Fatalf("set password: %s", setPw.body)
	}
	expectCode(t, c.do("POST", "/api/v1/pages/acme/password", reqOpts{token: token, body: map[string]any{"password": ""}}), 403, "admin_only", "clear password")

	// 14. agents may NOT delete an approval-gated page.
	expectCode(t, c.do("DELETE", "/api/v1/pages/gated", reqOpts{token: token}), 403, "approval_required", "delete gated page")

	// 15. agents may NOT delete a disabled (taken-down) page — this is what
	//     stops the delete→recreate slug-reuse bypass of an admin takedown.
	if _, err := d.Exec(ctx, "UPDATE pages SET disabled = true WHERE slug = 'acme'"); err != nil {
		t.Fatal(err)
	}
	expectCode(t, c.do("DELETE", "/api/v1/pages/acme", reqOpts{token: token}), 403, "disabled_takedown", "delete disabled page")

	// 16. nested slugs are first-class (PLAN §13): every REST route + /raw
	//     resolves with the raw (unencoded) nested path.
	expectStatus(t, c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "nested/q2", "title": "Nested"}}), 201, "create nested")
	nd := c.do("POST", "/api/v1/pages/nested/q2/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(7), "publish": true}})
	expectStatus(t, nd, 201, "deploy nested")
	if nd.at("published") != true {
		t.Fatalf("nested deploy: %s", nd.body)
	}
	nv := nd.str("version", "id")
	ng := c.do("GET", "/api/v1/pages/nested/q2", reqOpts{token: token})
	expectStatus(t, ng, 200, "get nested")
	if ng.str("page", "slug") != "nested/q2" || !strings.HasSuffix(ng.str("urls", "admin"), "/admin/nested/q2") ||
		!strings.HasSuffix(ng.str("urls", "live"), "/nested/q2") {
		t.Fatalf("nested get: %s", ng.body)
	}
	expectStatus(t, c.do("GET", "/api/v1/pages/nested%2Fq2", reqOpts{token: token}), 200, "encoded slash")
	nl := c.do("GET", "/api/v1/pages/nested/q2/versions", reqOpts{token: token})
	expectStatus(t, nl, 200, "list nested versions")
	if l, _ := nl.at("versions").([]any); len(l) != 1 {
		t.Fatalf("nested versions: %s", nl.body)
	}
	n1 := c.do("GET", "/api/v1/pages/nested/q2/versions/"+nv, reqOpts{token: token})
	expectStatus(t, n1, 200, "get nested version")
	if n1.str("version", "id") != nv {
		t.Fatalf("nested version: %s", n1.body)
	}
	nRaw := rawGet("nested/q2", ng.str("page", "id"), nv)
	expectStatus(t, nRaw, 200, "/raw nested")
	if !strings.Contains(nRaw.body, "Report 7") {
		t.Fatalf("nested raw: %s", nRaw.body)
	}

	// 17. reserved slugs (route collisions) are rejected at creation and
	//     restoration, and existing collisions block the migration.
	for _, segment := range routens.ReservedSlugSegments {
		bad := "northwind/" + segment + "/report"
		expectCode(t, c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": bad}}), 400, "reserved_slug", "reserved "+bad)
	}
	routeAdmin := versions.Actor{Actor: "admin@example.test", ActorType: audit.User, IP: "127.0.0.1"}
	for _, segment := range routens.ReservedSlugSegments {
		_, err := versions.RestorePage(ctx, d, "northwind/"+segment+"/report", routeAdmin)
		if e, ok := apierror.As(err); !ok || e.Status != 400 || e.Code != "reserved_slug" {
			t.Fatalf("restore reserved %s: %v", segment, err)
		}
	}
	collisionCheck, err := fs.ReadFile(pages.Migrations(), "023_reserve_application_routes.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, segment := range []string{"raw-template", "preflight", "edit-token", "readyz"} {
		tx, err := d.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO pages (slug, title) VALUES ($1, 'Old route fixture')", "northwind/"+segment+"/legacy"); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, string(collisionCheck))
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "P0001" || !strings.Contains(pg.Message, "active page slugs conflict") || !strings.Contains(pg.Hint, "current release") {
			t.Fatalf("collision %s: %v", segment, err)
		}
		_ = tx.Rollback(ctx)
	}
	tx, err := d.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO pages (slug, title, deleted_at) VALUES ('raw-template/retired', 'Retired fixture', now())"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(collisionCheck)); err != nil {
		t.Fatalf("a retired collision must not block: %v", err)
	}
	_ = tx.Rollback(ctx)

	// 18. dedupe scoping: same bytes + same mode still dedupes (idempotent),
	//     but a render_mode change is a NEW row with the REQUESTED mode, and
	//     rejected content re-enters as a fresh publishable row.
	c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "dedup", "title": "Dedup"}})
	w1 := c.do("POST", "/api/v1/pages/dedup/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(42)}})
	expectStatus(t, w1, 201, "w1")
	w2 := c.do("POST", "/api/v1/pages/dedup/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(42)}})
	expectStatus(t, w2, 200, "w2 dedupe")
	if w2.at("deduped") != true || w2.str("version", "id") != w1.str("version", "id") {
		t.Fatalf("w2: %s", w2.body)
	}
	w3 := c.do("POST", "/api/v1/pages/dedup/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(42), "render_mode": "raw"}})
	expectStatus(t, w3, 201, "w3 mode change")
	if w3.at("deduped") != false || w3.str("version", "render_mode") != "raw" || w3.str("version", "id") == w1.str("version", "id") {
		t.Fatalf("w3: %s", w3.body)
	}
	adminCtx := versions.Actor{Actor: "admin@elcanotek.com", ActorType: audit.User, IP: "127.0.0.1"}
	w1id, _ := jsx.ParseID(w1.str("version", "id"))
	if _, err := versions.Reject(ctx, d, versions.ReviewInput{Slug: "dedup", VersionID: w1id}, adminCtx); err != nil {
		t.Fatal(err)
	}
	w4 := c.do("POST", "/api/v1/pages/dedup/versions", reqOpts{token: token, body: map[string]any{"html": pageHTML(42), "publish": true}})
	expectStatus(t, w4, 201, "w4 after reject")
	if w4.at("deduped") != false || w4.str("version", "id") == w1.str("version", "id") || w4.at("published") != true || w4.at("live") != true {
		t.Fatalf("w4: %s", w4.body)
	}

	// 19. bounded lock waits (PG_LOCK_TIMEOUT_MS=2000): a wedged transaction
	//     holding one page's row lock makes concurrent mutations on THAT page
	//     fail fast with a clean 503 — while other pages keep succeeding.
	func() {
		wedge, err := d.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = wedge.Rollback(ctx) }()
		if _, err := wedge.Exec(ctx, "SELECT id FROM pages WHERE slug = 'dedup' FOR UPDATE"); err != nil {
			t.Fatal(err)
		}
		t0 := time.Now()
		blocked := c.do("POST", "/api/v1/pages/dedup/title", reqOpts{token: token, body: map[string]any{"title": "Blocked"}})
		waited := time.Since(t0)
		expectCode(t, blocked, 503, "db_lock_timeout", "wedged page")
		if waited < time.Second || waited >= 10*time.Second {
			t.Fatalf("waited %v", waited)
		}
		expectStatus(t, c.do("POST", "/api/v1/pages/nested/q2/title", reqOpts{token: token, body: map[string]any{"title": "Unaffected"}}), 200, "other page")
	}()

	// 20. expected_version must hold on EVERY deploy, not only the ones that
	//     move the pointer: publish:false and an approval-gated page too.
	//     The gated page is the worse half: agent A reads live v1, a human
	//     approves pending v2, and A's write would land as pending v3 built
	//     from v1 — no 409 — and approving v3 would revert v2.
	ctxUser := versions.Actor{Actor: "t@elcanotek.com", ActorType: audit.User, IP: "127.0.0.1"}
	c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "stalegate", "title": "Stale Gate"}})
	sg1 := c.do("POST", "/api/v1/pages/stalegate/versions", reqOpts{token: token, body: map[string]any{"html": "<h1>one</h1>", "publish": true}})
	expectStatus(t, sg1, 201, "sg1")
	staleID := sg1.str("version", "id")
	c.do("POST", "/api/v1/pages/stalegate/versions", reqOpts{token: token, body: map[string]any{"html": "<h1>two</h1>", "publish": true}})
	expectCode(t, c.do("POST", "/api/v1/pages/stalegate/versions", reqOpts{token: token, body: map[string]any{"html": "<h1>three</h1>", "publish": false, "expected_version": staleID}}),
		409, "stale_version", "publish:false honours expected_version")
	if _, err := versions.SetApproval(ctx, d, "stalegate", true, ctxUser); err != nil {
		t.Fatal(err)
	}
	gatedStale := c.do("POST", "/api/v1/pages/stalegate/versions", reqOpts{token: token, body: map[string]any{"html": "<h1>four</h1>", "publish": true, "expected_version": staleID}})
	expectCode(t, gatedStale, 409, "stale_version", "gated deploy honours expected_version")
	// REST serializes {error, code} only; the message still names the recovery.
	if !strings.Contains(gatedStale.str("error"), "retry once with expected_version") {
		t.Fatalf("stale message: %s", gatedStale.body)
	}
	if len(gatedStale.json) != 2 {
		t.Fatalf("REST error carries only error+code: %s", gatedStale.body)
	}
	live := c.do("GET", "/api/v1/pages/stalegate", reqOpts{token: token})
	fresh := c.do("POST", "/api/v1/pages/stalegate/versions", reqOpts{token: token, body: map[string]any{"html": "<h1>five</h1>", "expected_version": live.str("page", "published_version_id")}})
	expectStatus(t, fresh, 201, "current expected_version on a gated page")
	if fresh.str("version", "status") != "pending" {
		t.Fatalf("fresh: %s", fresh.body)
	}
	if _, err := versions.SetApproval(ctx, d, "stalegate", false, ctxUser); err != nil {
		t.Fatal(err)
	}
}

// The remaining routes the JS integration test did not drive, plus the
// response shapes lib/api.js chose by hand.
func TestAPIRoutesAndShapes(t *testing.T) {
	t.Setenv("API_TOKEN_PEPPER", "integration-pepper")
	t.Setenv("DASHBOARD_HOST", dashHost)
	t.Setenv("CONTENT_HOST", contentHost)
	t.Setenv("DASHBOARD_ORIGIN", "")
	t.Setenv("CONTENT_ORIGIN", "")
	d := testdb.New(t)
	ctx := context.Background()
	srv := httptest.NewServer(Handler(Deps{DB: d, ClientIP: func(*http.Request) string { return "203.0.113.9" }}))
	defer srv.Close()
	c := client{t: t, srv: srv}
	minted, err := tokens.Mint(ctx, d, tokens.MintParams{Label: "shape-agent", Scope: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	token := minted.Token

	expectStatus(t, c.do("POST", "/api/v1/pages", reqOpts{token: token, body: map[string]any{"slug": "contoso/q3", "title": "Q3", "client_id": "contoso"}}), 201, "create")

	// GET /pages lists summaries.
	list := c.do("GET", "/api/v1/pages", reqOpts{token: token})
	expectStatus(t, list, 200, "list")
	if pagesList, _ := list.at("pages").([]any); len(pagesList) != 1 || pagesList[0].(map[string]any)["slug"] != "contoso/q3" {
		t.Fatalf("list: %s", list.body)
	}
	if !strings.HasPrefix(list.header.Get("ETag"), `W/"`) || list.header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("express json headers: %v", list.header)
	}

	// Preflight with no published version is a 400 naming the slug.
	noPub := c.do("GET", "/api/v1/pages/contoso/q3/preflight", reqOpts{token: token})
	expectCode(t, noPub, 400, "no_published_version", "preflight unpublished")
	if noPub.str("error") != "page contoso/q3 has no published version; pass version_id to preflight a draft" {
		t.Fatalf("preflight message: %s", noPub.body)
	}

	// The deploy response carries exactly these keys, in this order.
	dep := c.do("POST", "/api/v1/pages/contoso/q3/versions", reqOpts{token: token, body: map[string]any{"html": `<h1 onclick="x()">Q3</h1><script>fetch("https://fabrikam.test/x")</script>`}})
	expectStatus(t, dep, 201, "deploy")
	if !regexp.MustCompile(`^\{"version":\{.*\},"deduped":false,"published":false,"gated":false,"live":false\}$`).MatchString(dep.body) {
		t.Fatalf("deploy shape: %s", dep.body)
	}
	draftID := dep.str("version", "id")

	// Preflight a draft by version_id; the report is the content host's view.
	pf := c.do("GET", "/api/v1/pages/contoso/q3/preflight?version_id="+draftID, reqOpts{token: token})
	expectStatus(t, pf, 200, "preflight draft")
	if pf.str("version_id") != draftID || pf.at("preflight", "ok") != false || pf.str("preflight", "render_mode") != "themed" {
		t.Fatalf("preflight: %s", pf.body)
	}
	expectCode(t, c.do("GET", "/api/v1/pages/contoso/q3/preflight?version_id=abc", reqOpts{token: token}), 400, "bad_version_id", "preflight bad id")
	expectCode(t, c.do("GET", "/api/v1/pages/contoso/q3/preflight?version_id=999999", reqOpts{token: token}), 404, "version_not_found", "preflight missing id")

	// GET version: html included; bad ids are 400.
	gv := c.do("GET", "/api/v1/pages/contoso/q3/versions/"+draftID, reqOpts{token: token})
	expectStatus(t, gv, 200, "get version")
	if !strings.Contains(gv.str("version", "html"), "fabrikam.test") || !strings.Contains(gv.body, `<h1 onclick=`) {
		t.Fatalf("version html (unescaped like JSON.stringify): %s", gv.body)
	}
	expectCode(t, c.do("GET", "/api/v1/pages/contoso/q3/versions/0", reqOpts{token: token}), 400, "bad_version_id", "version 0")
	expectCode(t, c.do("GET", "/api/v1/pages/contoso/q3/versions/x", reqOpts{token: token}), 400, "bad_version_id", "version x")

	// Publish the draft: {version, published:true}; then preflight defaults
	// to the published version.
	pub := c.do("POST", "/api/v1/pages/contoso/q3/publish", reqOpts{token: token, body: map[string]any{"version_id": draftID}})
	expectStatus(t, pub, 200, "publish")
	if pub.at("published") != true || pub.str("version", "status") != "approved" || !strings.HasSuffix(pub.body, `,"published":true}`) {
		t.Fatalf("publish: %s", pub.body)
	}
	expectCode(t, c.do("POST", "/api/v1/pages/contoso/q3/publish", reqOpts{token: token, body: map[string]any{}}), 400, "bad_version_id", "publish without id")
	if got := c.do("GET", "/api/v1/pages/contoso/q3/preflight", reqOpts{token: token}).str("version_id"); got != draftID {
		t.Fatalf("preflight default version %s", got)
	}

	// Rollback with version_id null means "previous approved" — there is none.
	rb := c.do("POST", "/api/v1/pages/contoso/q3/rollback", reqOpts{token: token, body: map[string]any{"version_id": nil}})
	if rb.status < 400 {
		t.Fatalf("rollback with no earlier approved version: %s", rb.body)
	}

	// The audit trail records the agent and the request IP.
	var actor, ip string
	if err := d.QueryRow(ctx, `SELECT actor, ip FROM audit_log WHERE actor_type = 'agent' ORDER BY id DESC LIMIT 1`).Scan(&actor, &ip); err != nil {
		t.Fatal(err)
	}
	if actor != "shape-agent" || ip != "203.0.113.9" {
		t.Fatalf("audit actor=%q ip=%q", actor, ip)
	}

	// Unknown routes inside the router fall through to the dashboard 404
	// (after auth); OPTIONS lists the matched routes' methods.
	nf := c.do("GET", "/api/v1/nope", reqOpts{token: token})
	expectStatus(t, nf, 404, "unknown route")
	if !strings.HasPrefix(nf.header.Get("Content-Type"), "text/html") {
		t.Fatalf("404 content type %q", nf.header.Get("Content-Type"))
	}
	expectStatus(t, c.do("PUT", "/api/v1/pages", reqOpts{token: token}), 404, "unknown method")
	opt := c.do("OPTIONS", "/api/v1/pages/contoso/q3", reqOpts{token: token})
	if opt.status != 200 || opt.header.Get("Allow") != "DELETE, GET, HEAD" {
		t.Fatalf("options: %d %v", opt.status, opt.header)
	}

	// Scoped data_update tokens are MCP-only.
	scoped, err := tokens.Mint(ctx, d, tokens.MintParams{Label: "data-agent", Scope: "data_update", AllowedSlugs: []string{"contoso/q3"}})
	if err != nil {
		t.Fatal(err)
	}
	expectCode(t, c.do("GET", "/api/v1/pages", reqOpts{token: scoped.Token}), 403, "token_scope_denied", "scoped token")
	expectCode(t, c.do("GET", "/api/v1/pages", reqOpts{token: "pages_bogus"}), 401, "bad_token", "bad token")
}
