// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/reqbody"
)

// express_routes.json was recorded against Express 5.3 / router 2.2 with
// lib/api.js's route table, server.js's mount and body parsers, a stub
// bearer check ("Bearer good") and a stub 404 ("NOTFOUND"); each route
// answered {route, slug, id}.
type routeVector struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	NoAuth      bool   `json:"noAuth"`
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
	Allow       string `json:"allow"`
	Body        string `json:"body"`
}

var routeNames = []string{
	"listPages", "createPage", "setPassword", "setTitle", "deletePage", "deploy",
	"listVersions", "preflight", "getVersion", "publish", "rollback", "getPage",
}

// stubRouter is the real router with every route answering its name and
// decoded params, behind a stub bearer check.
func stubRouter(t *testing.T) http.Handler {
	t.Helper()
	notFound := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SendHTML(w, r, http.StatusNotFound, "NOTFOUND")
	})
	h := newHandler(Deps{NotFound: notFound, Body: reqbody.Options{JSONLimit: 2 << 20, FormLimit: 64 << 10}},
		func(h *handler, next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer good" {
					h.writeError(w, r, apierror.Unauthorized("missing bearer token", "no_token", nil))
					return
				}
				next.ServeHTTP(w, r)
			})
		})
	if len(h.routes) != len(routeNames) {
		t.Fatalf("%d routes, %d names", len(h.routes), len(routeNames))
	}
	for i := range h.routes {
		name := routeNames[i]
		h.routes[i].Handler = func(_ *handler, w http.ResponseWriter, r *http.Request, p params) error {
			if name == "createPage" {
				v, ok := reqbody.From(r)
				var b any = "<undefined>"
				if ok {
					b = v
				}
				return sendJSON(w, r, 200, jsonv.ObjectOf("route", name, "body", b))
			}
			var slug, id any
			if strings.Contains(h.routes[i].Re.String(), "<slug>") {
				slug = p.Slug
			}
			if strings.Contains(h.routes[i].Re.String(), "<id>") {
				id = p.ID
			}
			return sendJSON(w, r, 200, jsonv.ObjectOf("route", name, "slug", slug, "id", id))
		}
	}
	return h
}

func TestRoutingMatchesExpress(t *testing.T) {
	raw, err := os.ReadFile("testdata/express_routes.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []routeVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	h := stubRouter(t)
	ran := 0
	for _, v := range vectors {
		if _, err := url.ParseRequestURI(v.Path); err != nil {
			continue // net/http refuses the request line before any handler
		}
		ran++
		req := httptest.NewRequest(v.Method, v.Path, nil)
		req.Host = "localhost"
		if !v.NoAuth {
			req.Header.Set("Authorization", "Bearer good")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		res := rec.Result()
		body, _ := io.ReadAll(res.Body)
		got := string(body)
		if v.Method == http.MethodHead {
			got = ""
		}
		if res.StatusCode != v.Status || got != v.Body || res.Header.Get("Content-Type") != v.ContentType || res.Header.Get("Allow") != v.Allow {
			t.Errorf("%s %s noAuth=%v:\n got %d %q allow=%q %s\nwant %d %q allow=%q %s", v.Method, v.Path, v.NoAuth,
				res.StatusCode, res.Header.Get("Content-Type"), res.Header.Get("Allow"), got,
				v.Status, v.ContentType, v.Allow, v.Body)
		}
	}
	if ran < 300 {
		t.Fatalf("only %d vectors ran", ran)
	}
}

// An undecodable parameter was a 500 "internal error" in Express (the API
// error handler ignored the decode error's 400); it is a 400 here.
func TestUndecodableParamIs400(t *testing.T) {
	h := stubRouter(t)
	for _, method := range []string{"GET", "DELETE", "OPTIONS"} {
		req := httptest.NewRequest(method, "/api/v1/pages/%C3", nil)
		req.Header.Set("Authorization", "Bearer good")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 || rec.Body.String() != `{"error":"invalid request","code":"bad_request"}` {
			t.Errorf("%s: %d %s", method, rec.Code, rec.Body)
		}
	}
}

// Parser failures precede auth but keep the API contract.
func TestBodyErrorsPrecedeAuth(t *testing.T) {
	h := stubRouter(t)
	req := httptest.NewRequest("POST", "/api/v1/pages", strings.NewReader("{bad"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 || rec.Body.String() != `{"error":"invalid JSON request body","code":"bad_json"}` ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestToID(t *testing.T) {
	ok := map[string]struct {
		v       any
		present bool
		want    int64
	}{
		"number":       {12.0, true, 12},
		"string":       {" 12 ", true, 12},
		"true":         {true, true, 1},
		"one element":  {[]any{"7"}, true, 7},
		"hex string":   {"0x10", true, 16},
		"float as int": {3.0, true, 3},
	}
	for name, c := range ok {
		id, err := toID(c.v, c.present)
		if err != nil || int64(id) != c.want {
			t.Errorf("%s: %v %v", name, id, err)
		}
	}
	bad := map[string]struct {
		v       any
		present bool
	}{
		"absent": {nil, false}, "null": {nil, true}, "zero": {0.0, true}, "negative": {-1.0, true},
		"fraction": {1.5, true}, "word": {"abc", true}, "empty": {"", true}, "false": {false, true},
		"object": {jsonv.NewObject(), true}, "two elements": {[]any{"1", "2"}, true}, "huge": {1e19, true},
	}
	for name, c := range bad {
		_, err := toID(c.v, c.present)
		if e, ok := apierror.As(err); !ok || e.Code != "bad_version_id" || e.Message != "version_id must be a positive integer" {
			t.Errorf("%s: %v", name, err)
		}
	}
}
