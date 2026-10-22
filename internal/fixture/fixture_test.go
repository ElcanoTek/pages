// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/assetver"
)

func do(t *testing.T, h http.Handler, method, path, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBentoBlocksInflate(t *testing.T) {
	for _, c := range []struct{ block, want string }{{bentoCSSBlock, bentoCSS}, {bentoRuntimeBlock, bentoRuntime}} {
		raw, err := base64.StdEncoding.DecodeString(c.block)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Fatalf("block inflates to %q, want %q", got, c.want)
		}
	}
}

func TestPayloads(t *testing.T) {
	app := New(3210)
	cases := []struct {
		name, method, path, ct, body string
		status                       int
		contains                     string
	}{
		{"healthz", "GET", "/healthz", "", "", 200, "ok"},
		{"page row key order and numeric ids", "GET", "/api/v1/admin/pages", "", "", 200,
			`{"pages":[{"id":1,"slug":"long/client/q2-report","title":"North America Programmatic Revenue and Performance Review — Quarter Two Executive Detail","client_id":null,"workspace_id":1,"workspace_name":"Campaign operations","theme_id":null,"theme_name":"flag","require_approval":true,"disabled":false,"published_version_id":102,"has_password":true,"is_live":true,"created_at":"2026-07-18T22:10:00.000Z","updated_at":"2026-07-22T09:25:00.000Z","freshness":{"source_as_of":"2026-06-06T09:30:00.000Z"`},
		{"published version number", "GET", "/api/v1/admin/pages", "", "", 200, `"days_since_check":0},"version_count":6,"published_version_number":2}`},
		{"template ids are strings", "GET", "/api/v1/admin/templates", "", "", 200, `{"templates":[{"id":"1","name":"nwm-campaign-dashboard"`},
		{"unknown page", "GET", "/api/v1/admin/pages/nope", "", "", 404, `{"error":"page not found"}`},
		{"non-POST action", "PUT", "/api/v1/admin/pages/client-01", "", "", 405, `{"error":"method not allowed"}`},
		{"theme left undefined drops the key", "POST", "/api/v1/admin/pages/client-01/theme", "application/json", `{}`, 200, `"theme_id":null,"require_approval"`},
		{"portal 404", "GET", "/api/v1/admin/portals/404404", "", "", 404, `{"error":"portal not found","code":"portal_not_found"}`},
		{"admin shell", "GET", "/admin/long/client/q2-report", "", "", 200, "<!doctype html>"},
		{"unknown route", "GET", "/nope", "", "", 404, "<!doctype html>"},
		{"bad JSON", "POST", "/api/v1/admin/pages", "application/json", `{`, 400, "SyntaxError"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, app, c.method, c.path, c.ct, c.body)
			if rec.Code != c.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, c.status, rec.Body.String())
			}
			if !strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(c.contains)) {
				t.Fatalf("body lacks %q:\n%.600s", c.contains, rec.Body.String())
			}
		})
	}
}

func TestEventsEchoBodiesInOrder(t *testing.T) {
	app := New(3210)
	do(t, app, "POST", "/api/v1/admin/workspaces", "application/json", `{"name":"  Ops ","z":[1,2],"2":"x","a":{"b":null}}`)
	rec := do(t, app, "GET", "/__fixture/events", "", "")
	want := `{"events":[{"method":"POST","path":"/workspaces","body":{"2":"x","name":"  Ops ","z":[1,2],"a":{"b":null}}}]}`
	if rec.Body.String() != want {
		t.Fatalf("got %s\nwant %s", rec.Body.String(), want)
	}
	do(t, app, "POST", "/__fixture/reset", "", "")
	if got := do(t, app, "GET", "/__fixture/events", "", "").Body.String(); got != `{"events":[]}` {
		t.Fatalf("reset left %s", got)
	}
}

func TestPortalMembersResort(t *testing.T) {
	app := New(3210)
	rec := do(t, app, "POST", "/api/v1/admin/portals/7/pages/update", "application/json", `{"page_id":"73","sort_order":-1}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	body := do(t, app, "GET", "/api/v1/admin/portals/7", "", "").Body.String()
	if i, j := strings.Index(body, `"page_id":73`), strings.Index(body, `"page_id":71`); i < 0 || j < 0 || i > j {
		t.Fatalf("member 73 was not moved first: %s", body)
	}
	do(t, app, "POST", "/__fixture/reset-portals", "", "")
	body = do(t, app, "GET", "/api/v1/admin/portals/7", "", "").Body.String()
	if strings.Index(body, `"page_id":73`) < strings.Index(body, `"page_id":71`) {
		t.Fatal("reset-portals did not restore the order")
	}
}

func TestDeckWithoutBlobDropsOnlyTheGrant(t *testing.T) {
	app := New(3210)
	with := do(t, app, "GET", "/bento/deck", "", "").Header().Get("Content-Security-Policy")
	without := do(t, app, "GET", "/bento/deck-without-blob", "", "").Header().Get("Content-Security-Policy")
	if !strings.Contains(with, "blob:") || strings.Contains(without, "script-src 'self' 'unsafe-inline' blob:") || with == without {
		t.Fatalf("with=%q without=%q", with, without)
	}
	edit := do(t, app, "GET", "/bento/edit", "", "").Header().Get("Content-Security-Policy")
	if !strings.Contains(edit, "connect-src http://127.0.0.1:3210") {
		t.Fatalf("edit CSP %q", edit)
	}
}

// The fixture serves the content host's versioned asset URLs the way the
// server does: immutable when the version is current, revalidating otherwise.
func TestVersionedAssets(t *testing.T) {
	app := New(3210)
	for _, c := range []struct{ path, cache string }{
		{assetver.ContentFlag() + "/tokens/design-tokens.css", assetver.Immutable},
		{"/assets/_v/000000000000/flag/tokens/design-tokens.css", assetver.Revalidate},
		{"/assets/flag/tokens/design-tokens.css", assetver.Revalidate},
		{assetver.ShellFlag() + "/tokens/design-tokens.css", assetver.Immutable},
	} {
		rec := do(t, app, "GET", c.path, "", "")
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != c.cache || !strings.Contains(rec.Body.String(), "--color") {
			t.Errorf("%s = %d %q", c.path, rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
}
