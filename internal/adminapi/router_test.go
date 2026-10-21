// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package adminapi

// Express routing as a client could observe it: the mount, the auth layer
// in front of every admin path, reads before the CSRF gate and mutations
// after it, fall-through to the REST router, HEAD/OPTIONS, and standalone
// body parsing.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/pages/internal/reqbody"
)

func TestRouting(t *testing.T) {
	base := setup(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, present := reqbody.From(r)
		w.Header().Set("X-Next", "1")
		if present && v != nil {
			w.Header().Set("X-Next-Body", "1")
		}
		w.WriteHeader(299)
	})
	// No upstream reqbody.Middleware: the handler parses on its own.
	srv := httptest.NewServer(Handler(Deps{DB: base.d, Auth: base.auth, Next: next}))
	t.Cleanup(srv.Close)
	e := *base
	e.srv = srv
	e.t = t

	// Outside the mount (whole-segment, case-insensitive) → Next, untouched.
	for _, p := range []string{"/api/v1/pages", "/api/v1/administrator", "/admin"} {
		if r := e.req("GET", p, opts{}); r.status != 299 {
			t.Fatalf("%s: %d, want fall-through", p, r.status)
		}
	}

	// Every admin path needs the session first — even unmatched ones.
	expectCode(t, e.req("GET", "/api/v1/admin/nope", opts{}), 401, "no_session", "unmatched, no cookie")
	expectCode(t, e.req("OPTIONS", "/api/v1/admin/pages", opts{}), 401, "no_session", "OPTIONS, no cookie")
	// Past the reads the CSRF gate applies to whatever reaches it.
	expectCode(t, e.get("/api/v1/admin/nope"), 403, "bad_origin", "unmatched GET hits the CSRF gate")
	expectCode(t, e.req("OPTIONS", "/api/v1/admin/pages", opts{cookie: e.adminCk}), 403, "bad_origin", "OPTIONS hits the CSRF gate")
	// With both, an unmatched path falls through to the REST router.
	if r := e.post("/api/v1/admin/pages/x/refresh/run", nil); r.status != 299 || r.header.Get("X-Next-Body") != "1" {
		t.Fatalf("fall-through with the parsed body: %d %v", r.status, r.header)
	}

	// Case-insensitive mount and routes, optional trailing slash, HEAD.
	for _, p := range []string{"/api/v1/admin/pages", "/API/V1/ADMIN/PAGES", "/api/v1/admin/pages/", "/api/v1/Admin/Workspaces"} {
		expect(t, e.get(p), 200, p)
	}
	head := e.req("HEAD", "/api/v1/admin/pages", opts{cookie: e.adminCk})
	expect(t, head, 200, "HEAD via GET")
	if head.body != "" {
		t.Fatal("HEAD has no body")
	}

	// OPTIONS with the CSRF pair: the Allow list of every matching route.
	opt := e.req("OPTIONS", "/api/v1/admin/pages", opts{cookie: e.adminCk, csrf: true, origin: origin})
	expect(t, opt, 200, "OPTIONS")
	eq(t, opt.header.Get("Allow"), "GET, HEAD, POST", "Allow")
	eq(t, opt.body, "GET, HEAD, POST", "OPTIONS body")
	eq(t, e.req("OPTIONS", "/api/v1/admin/templates/x", opts{cookie: e.adminCk, csrf: true, origin: origin}).header.Get("Allow"), "DELETE, GET, HEAD", "template Allow")

	// A malformed escape in a parameter is the client's error.
	expectCode(t, e.get("/api/v1/admin/pages/%E0%A4"), 400, "bad_request", "undecodable slug")
	// A method no route has for the path falls through.
	if r := e.req("PUT", "/api/v1/admin/pages", opts{cookie: e.adminCk, csrf: true, origin: origin}); r.status != 299 {
		t.Fatalf("PUT: %d", r.status)
	}

	// Standalone body parsing keeps the API's JSON error contract and runs
	// before authentication, as the dashboard-wide parsers did.
	bad := `{"name": nope`
	r := e.req("POST", "/api/v1/admin/workspaces", opts{raw: &bad})
	eq(t, r.status, 400, "malformed JSON status")
	eq(t, r.body, `{"error":"invalid JSON request body","code":"bad_json"}`, "malformed JSON body")
	created := e.post("/api/v1/admin/workspaces", map[string]any{"name": "Contoso"})
	expect(t, created, 201, "standalone parse")
	eq(t, created.str("workspace", "name"), "Contoso", "parsed name")
	// A urlencoded body reads the same fields.
	form := e.req("POST", "/api/v1/admin/workspaces", opts{cookie: e.adminCk, csrf: true, origin: origin, form: map[string][]string{"name": {"Fabrikam"}}})
	expect(t, form, 201, "form body")
	eq(t, form.str("workspace", "name"), "Fabrikam", "form name")
}
