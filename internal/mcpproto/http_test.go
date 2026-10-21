// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcpproto

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// Port of the protocol-level part of test/mcp-wire.integration.js: each case
// goes over real HTTP and its status, headers and exact body are compared
// with internal/mcp/testdata/mcp-wire.golden.json, recorded from the official SDK
// transport. Pages' tools are not in this package, so the tool calls run
// against stand-ins that answer as the real get_page / patch_page /
// list_themes did in the recording; the full catalog listings (pinned by
// digest) are only checked for status and headers here.

const wireVersion = "0.1.0"

type stubAgent struct{ scoped bool }

type agentKey struct{}

func pageNotFound(slug string) *CallToolResult {
	body := `{"ok":false,"error":"page not found: ` + slug + `","code":"page_not_found"}`
	return &CallToolResult{Content: []Content{TextContent(body)}, StructuredContent: json.RawMessage(body), IsError: true}
}

func slugTool(t testing.TB, name string) Tool {
	return Tool{
		Name: name, Description: name, InputSchema: json.RawMessage(`{"type":"object"}`),
		ParseInput: func(raw json.RawMessage) (json.RawMessage, error) {
			o := mustObject(t, raw)
			var issues []Issue
			slug, has := o.Get("slug")
			if _, ok := slug.(string); !ok {
				r := "undefined"
				if has {
					r = received(slug)
				}
				issues = append(issues, Issue{Path: []any{"slug"}, Message: "Invalid input: expected string, received " + r})
			}
			issues = append(issues, strictUnknown(o, "slug")...)
			if issues != nil {
				return nil, &ValidationError{Issues: issues}
			}
			return raw, nil
		},
		Handler: func(_ context.Context, args json.RawMessage, _ Extra) (*CallToolResult, error) {
			slug, _ := mustObject(t, args).Get("slug")
			return pageNotFound(slug.(string)), nil
		},
	}
}

func wireTools(t testing.TB, scoped bool) []Tool {
	if scoped {
		return []Tool{slugTool(t, "get_page_data")}
	}
	patch := slugTool(t, "patch_page")
	patch.ParseInput = func(json.RawMessage) (json.RawMessage, error) {
		return nil, &ValidationError{Issues: []Issue{
			{Path: []any{"edits", 0, "find"}, Message: "Invalid input: expected string, received number"},
			{Path: []any{"edits", 0, "replace"}, Message: "Invalid input: expected string, received undefined"},
		}}
	}
	themes := `{"themes":[{"id":"1","name":"flag","default_mode":"system"}]}`
	return []Tool{
		slugTool(t, "get_page"),
		patch,
		{
			Name: "list_themes", InputSchema: json.RawMessage(`{"type":"object"}`),
			Handler: func(context.Context, json.RawMessage, Extra) (*CallToolResult, error) {
				return &CallToolResult{Content: []Content{TextContent(themes)}, StructuredContent: json.RawMessage(themes)}, nil
			},
		},
		slugTool(t, "deploy_page"),
	}
}

func readText(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wireHandler(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("DASHBOARD_ORIGIN", "http://localhost:3101")
	t.Setenv("DASHBOARD_HOST", "")
	t.Setenv("MCP_ALLOWED_HOSTS", "")
	t.Setenv("MCP_ALLOWED_ORIGINS", "")
	instructions := readText(t, "testdata/instructions.txt")
	dataInstructions := readText(t, "testdata/data-update-instructions.txt")
	known := map[string]bool{"get_page": true, "patch_page": true, "list_themes": true, "deploy_page": true, "get_page_data": true}
	return NewHandler(HandlerOptions{
		MaxBodyBytes: 2 << 20,
		Authenticate: func(r *http.Request) (*http.Request, error) {
			switch r.Header.Get("Authorization") {
			case "Bearer full":
				return r.WithContext(context.WithValue(r.Context(), agentKey{}, stubAgent{})), nil
			case "Bearer scoped":
				return r.WithContext(context.WithValue(r.Context(), agentKey{}, stubAgent{scoped: true})), nil
			case "":
				return nil, apierror.Unauthorized("missing bearer token", "no_token", nil)
			}
			return nil, apierror.Unauthorized("invalid or revoked token", "bad_token", nil)
		},
		KnownTool: func(name string) bool { return known[name] },
		NewServer: func(r *http.Request) (*Server, error) {
			agent := r.Context().Value(agentKey{}).(stubAgent)
			text := instructions
			if agent.scoped {
				text = dataInstructions
			}
			return NewServer(ServerOptions{
				Info:         Implementation{Name: "pages", Title: "Elcano Pages", Version: wireVersion},
				Instructions: text,
				Tools:        wireTools(t, agent.scoped),
			})
		},
	})
}

type wireCase struct {
	name    string
	method  string
	body    any // marshalled with jsonv unless rawBody is set
	rawBody *string
	headers map[string]*string
	scoped  bool
	// headersOnly: the body needs Pages' real tool catalog.
	headersOnly bool
}

func str(s string) *string { return &s }

func obj(kv ...any) *jsonv.Object {
	o := jsonv.NewObject()
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func wireCases() []wireCase {
	const v = "2025-11-25"
	client := func() *jsonv.Object { return obj("name", "wire", "version", "1.0.0") }
	request := func(id any, method string, params ...any) *jsonv.Object {
		o := obj("jsonrpc", "2.0", "id", id, "method", method)
		if len(params) > 0 {
			o.Set("params", params[0])
		}
		return o
	}
	note := func(method string, params ...any) *jsonv.Object {
		o := obj("jsonrpc", "2.0", "method", method)
		if len(params) > 0 {
			o.Set("params", params[0])
		}
		return o
	}
	init := func(version string, caps ...*jsonv.Object) *jsonv.Object {
		c := obj()
		if len(caps) > 0 {
			c = caps[0]
		}
		return request(1.0, "initialize", obj("protocolVersion", version, "capabilities", c, "clientInfo", client()))
	}
	pv := map[string]*string{"MCP-Protocol-Version": str(v)}
	with := func(extra map[string]*string) map[string]*string {
		h := map[string]*string{"MCP-Protocol-Version": str(v)}
		for k, val := range extra {
			h[k] = val
		}
		return h
	}
	withExtra := func(o *jsonv.Object, k string, val any) *jsonv.Object { o.Set(k, val); return o }
	args := obj
	return []wireCase{
		{name: "initialize latest", body: init(v)},
		{name: "initialize 2025-06-18", body: init("2025-06-18")},
		{name: "initialize 2025-03-26", body: init("2025-03-26")},
		{name: "initialize 2024-11-05", body: init("2024-11-05")},
		{name: "initialize 2024-10-07", body: init("2024-10-07")},
		{name: "initialize unknown version falls back to latest", body: init("2099-01-01")},
		{name: "initialize ignores a bogus protocol header", body: init(v), headers: map[string]*string{"MCP-Protocol-Version": str("1999-01-01")}},
		{name: "initialize with client capabilities", body: init(v, obj("roots", obj("listChanged", true), "sampling", obj(), "elicitation", obj()))},
		{name: "initialize missing capabilities", body: request(1.0, "initialize", obj("protocolVersion", v, "clientInfo", client()))},
		{name: "initialize bad clientInfo", body: request(1.0, "initialize", obj("protocolVersion", v, "capabilities", obj(), "clientInfo", obj("name", 1.0)))},
		{name: "initialize bad capability shape", body: init(v, obj("roots", 5.0))},
		{name: "initialize string id", body: withExtra(init(v), "id", "abc")},
		{name: "initialize as a notification", body: note("initialize", obj("protocolVersion", v, "capabilities", obj(), "clientInfo", client()))},
		{name: "initialized notification", body: note("notifications/initialized"), headers: pv},
		{name: "initialized notification with params", body: note("notifications/initialized", obj()), headers: pv},
		{name: "unknown notification", body: note("notifications/whatever", obj("a", 1.0)), headers: pv},
		{name: "request method sent as a notification", body: note("tools/list"), headers: pv},
		{name: "notification with an unsupported protocol header", body: note("notifications/initialized"), headers: map[string]*string{"MCP-Protocol-Version": str("2099-01-01")}},
		{name: "client result response", body: obj("jsonrpc", "2.0", "id", 9.0, "result", obj()), headers: pv},
		{name: "client error response", body: obj("jsonrpc", "2.0", "id", 9.0, "error", obj("code", -1.0, "message", "x")), headers: pv},

		{name: "ping", body: request(2.0, "ping"), headers: pv},
		{name: "ping without a protocol header", body: request(2.0, "ping")},
		{name: "ping with empty params", body: request(2.0, "ping", obj()), headers: pv},
		{name: "ping with _meta", body: request(2.0, "ping", obj("_meta", obj("progressToken", 7.0))), headers: pv},
		{name: "ping with bad _meta", body: request(2.0, "ping", obj("_meta", obj("progressToken", 1.5))), headers: pv},
		{name: "ping with task augmentation", body: request(2.0, "ping", obj("task", obj())), headers: pv},
		{name: "ping with array params", body: request(2.0, "ping", []any{1.0}), headers: pv},
		{name: "ping negative id", body: request(-4.0, "ping"), headers: pv},
		{name: "ping float id", body: request(1.5, "ping"), headers: pv},
		{name: "ping null id", body: request(nil, "ping"), headers: pv},
		{name: "ping with an extra top-level key", body: withExtra(request(2.0, "ping"), "extra", true), headers: pv},
		{name: "ping unsupported protocol header", body: request(2.0, "ping"), headers: map[string]*string{"MCP-Protocol-Version": str("2099-01-01")}},
		{name: "ping empty protocol header", body: request(2.0, "ping"), headers: map[string]*string{"MCP-Protocol-Version": str("")}},
		{name: "ping legacy protocol header", body: request(2.0, "ping"), headers: map[string]*string{"MCP-Protocol-Version": str("2024-11-05")}},
		{name: "unknown method", body: request(3.0, "frobnicate", obj()), headers: pv},
		{name: "unknown method without params", body: request(3.0, "resources/list"), headers: pv},
		{name: "notification-shaped method with an id", body: request(3.0, "notifications/initialized"), headers: pv},
		{name: "wrong jsonrpc version", body: obj("jsonrpc", "1.0", "id", 3.0, "method", "ping"), headers: pv},
		{name: "missing method", body: obj("jsonrpc", "2.0", "id", 3.0), headers: pv},

		{name: "tools/list", body: request(4.0, "tools/list"), headers: pv, headersOnly: true},
		{name: "tools/list with _meta", body: request(4.0, "tools/list", obj("_meta", obj())), headers: pv, headersOnly: true},
		{name: "tools/list cursor", body: request(4.0, "tools/list", obj("cursor", "x")), headers: pv},
		{name: "tools/list bad cursor type", body: request(4.0, "tools/list", obj("cursor", 5.0)), headers: pv},
		{name: "tools/call unknown tool", body: request(5.0, "tools/call", obj("name", "nope", "arguments", obj())), headers: pv},
		{name: "tools/call prototype tool name", body: request(5.0, "tools/call", obj("name", "__proto__", "arguments", obj())), headers: pv},
		{name: "tools/call missing name", body: request(5.0, "tools/call", obj("arguments", obj())), headers: pv},
		{name: "tools/call array arguments", body: request(5.0, "tools/call", obj("name", "get_page", "arguments", []any{})), headers: pv},
		{name: "tools/call without params", body: request(5.0, "tools/call"), headers: pv},
		{name: "tools/call invalid arguments", body: request(5.0, "tools/call", obj("name", "get_page", "arguments", args("slug", "x", "bogus", 1.0))), headers: pv},
		{name: "tools/call missing required argument", body: request(5.0, "tools/call", obj("name", "get_page", "arguments", obj())), headers: pv},
		{name: "tools/call omitted arguments", body: request(5.0, "tools/call", obj("name", "get_page")), headers: pv},
		{name: "tools/call nested invalid argument", body: request(5.0, "tools/call", obj("name", "patch_page", "arguments", args("slug", "x", "edits", []any{obj("find", 1.0)}))), headers: pv},
		{name: "tools/call domain error", body: request(5.0, "tools/call", obj("name", "get_page", "arguments", args("slug", "wire-no-such-page"))), headers: pv},
		{name: "tools/call success", body: request(5.0, "tools/call", obj("name", "list_themes", "arguments", obj())), headers: pv},
		{name: "tools/call task augmentation", body: request(5.0, "tools/call", obj("name", "list_themes", "arguments", obj(), "task", obj())), headers: pv},
		{name: "tools/call bad task", body: request(5.0, "tools/call", obj("name", "list_themes", "arguments", obj(), "task", 5.0)), headers: pv},
		{name: "tools/call as a notification", body: note("tools/call", obj("name", "list_themes", "arguments", obj())), headers: pv},
		{name: "tools/call unsupported protocol header", body: request(5.0, "tools/call", obj("name", "list_themes", "arguments", obj())), headers: map[string]*string{"MCP-Protocol-Version": str("2099-01-01")}},

		{name: "malformed JSON", rawBody: str("{"), headers: pv},
		{name: "empty body", rawBody: str(""), headers: pv},
		{name: "JSON null", rawBody: str("null"), headers: pv},
		{name: "JSON string", rawBody: str(`"ping"`), headers: pv},
		{name: "JSON number", rawBody: str("7"), headers: pv},
		{name: "empty batch", rawBody: str("[]"), headers: pv},
		{name: "batch of one", body: []any{request(6.0, "ping")}, headers: pv},
		{name: "batch of notifications", body: []any{note("notifications/initialized")}, headers: pv},

		{name: "Content-Type with charset", body: request(7.0, "ping"), headers: with(map[string]*string{"Content-Type": str("application/json; charset=utf-8")})},
		{name: "Content-Type upper case", body: request(7.0, "ping"), headers: with(map[string]*string{"Content-Type": str("APPLICATION/JSON")})},
		{name: "Content-Type text/plain", body: request(7.0, "ping"), headers: with(map[string]*string{"Content-Type": str("text/plain")})},
		{name: "Content-Type missing", body: request(7.0, "ping"), headers: with(map[string]*string{"Content-Type": nil})},
		{name: "Accept JSON only", body: request(7.0, "ping"), headers: with(map[string]*string{"Accept": str("application/json")})},
		{name: "Accept wildcard", body: request(7.0, "ping"), headers: with(map[string]*string{"Accept": str("*/*")})},
		{name: "Accept upper case", body: request(7.0, "ping"), headers: with(map[string]*string{"Accept": str("APPLICATION/JSON, TEXT/EVENT-STREAM")})},
		{name: "Accept with q=0", body: request(7.0, "ping"), headers: with(map[string]*string{"Accept": str("application/json, text/event-stream;q=0")})},
		{name: "Accept missing", body: request(7.0, "ping"), headers: with(map[string]*string{"Accept": nil})},
		{name: "GET", method: "GET", headers: map[string]*string{"Content-Type": nil}},
		{name: "DELETE", method: "DELETE", headers: map[string]*string{"Content-Type": nil}},
		{name: "PUT", method: "PUT", body: request(8.0, "ping"), headers: pv},
		{name: "unauthenticated", body: request(8.0, "ping"), headers: with(map[string]*string{"Authorization": nil})},

		{name: "scoped tools/list", scoped: true, body: request(9.0, "tools/list"), headers: pv, headersOnly: true},
		{name: "scoped call to an unregistered tool", scoped: true, body: request(9.0, "tools/call", obj("name", "deploy_page", "arguments", obj())), headers: pv},
		{name: "scoped call to a registered tool", scoped: true, body: request(9.0, "tools/call", obj("name", "get_page_data", "arguments", args("slug", "wire-no-such-page"))), headers: pv},
		{name: "scoped initialize", scoped: true, body: init(v)},
	}
}

type observed struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

var volatileHeaders = map[string]bool{"date": true, "connection": true, "keep-alive": true, "ratelimit": true, "ratelimit-policy": true, "transfer-encoding": true}

func send(t *testing.T, base string, c wireCase) observed {
	t.Helper()
	method := c.method
	if method == "" {
		method = "POST"
	}
	var payload []byte
	hasPayload := false
	switch {
	case c.rawBody != nil:
		payload, hasPayload = []byte(*c.rawBody), true
	case c.body != nil:
		payload, hasPayload = []byte(jsonv.Stringify(c.body)), true
	}
	req, err := http.NewRequest(method, base+"/mcp", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost"
	token := "full"
	if c.scoped {
		token = "scoped"
	}
	headers := map[string]*string{"Accept": str("application/json, text/event-stream"), "Authorization": str("Bearer " + token)}
	if hasPayload {
		headers["Content-Type"] = str("application/json")
	}
	for k, val := range c.headers {
		headers[k] = val
	}
	for k, val := range headers {
		if val != nil {
			req.Header.Set(k, *val)
		}
	}
	if !hasPayload {
		req.Body, req.ContentLength = nil, 0
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	data := strings.ReplaceAll(string(raw), `"version":"`+wireVersion+`"`, `"version":"<package.json version>"`)
	kept := map[string]string{}
	for k, vals := range res.Header {
		k = strings.ToLower(k)
		if !volatileHeaders[k] {
			kept[k] = strings.Join(vals, ", ")
		}
	}
	if kept["content-length"] == fmt.Sprint(len(raw)) {
		kept["content-length"] = "<body length>"
	}
	body := data
	if jsstr.UTF16Len(data) > 4096 {
		sum := sha256.Sum256([]byte(data))
		body = fmt.Sprintf("sha256:%s bytes:%d", hex.EncodeToString(sum[:]), len(data))
	}
	return observed{Status: res.StatusCode, Headers: kept, Body: body}
}

// expectedFor is the JS test's list of deliberate, reviewed departures from
// the recorded SDK behaviour.
func expectedFor(name string, golden observed) observed {
	if name == "Accept upper case" {
		// Media types are case-insensitive (RFC 9110); the SDK transport
		// re-read the raw header and refused upper case.
		return observed{Status: 200, Headers: map[string]string{
			"cache-control": "no-store", "content-length": "<body length>", "content-type": "application/json",
		}, Body: `{"result":{},"jsonrpc":"2.0","id":7}`}
	}
	if golden.Status == 202 {
		// A 202 ends with Content-Length: 0 rather than an empty chunked body.
		h := map[string]string{}
		for k, v := range golden.Headers {
			h[k] = v
		}
		h["content-length"] = "<body length>"
		golden.Headers = h
	}
	return golden
}

func loadGolden(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("../mcp/testdata/mcp-wire.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var g map[string]json.RawMessage
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func headerKeys(h map[string]string) string {
	keys := make([]string, 0, len(h))
	for k, v := range h {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(keys, "; ")
}

func TestWireMatchesTheRecordedSDKTransport(t *testing.T) {
	golden := loadGolden(t)
	srv := httptest.NewServer(wireHandler(t))
	defer srv.Close()
	cases := wireCases()
	if len(cases) != len(golden)-1 {
		t.Fatalf("%d cases, golden has %d", len(cases), len(golden)-1)
	}
	for _, c := range cases {
		raw, ok := golden[c.name]
		if !ok {
			t.Errorf("case %q is not in the golden", c.name)
			continue
		}
		var g observed
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatal(err)
		}
		want := expectedFor(c.name, g)
		got := send(t, srv.URL, c)
		if got.Status != want.Status || headerKeys(got.Headers) != headerKeys(want.Headers) {
			t.Errorf("%s:\n got %d %s\nwant %d %s", c.name, got.Status, headerKeys(got.Headers), want.Status, headerKeys(want.Headers))
		}
		if !c.headersOnly && got.Body != want.Body {
			t.Errorf("%s body:\n got %s\nwant %s", c.name, got.Body, want.Body)
		}
	}
}

func TestBoundaryHostsAndOrigins(t *testing.T) {
	t.Setenv("DASHBOARD_HOST", "Pages.Northwind.test")
	t.Setenv("DASHBOARD_ORIGIN", "https://pages.northwind.test:8443")
	t.Setenv("MCP_ALLOWED_HOSTS", " Agents.Contoso.test , [::1], bad host, user@x, 127.1")
	t.Setenv("MCP_ALLOWED_ORIGINS", "https://app.contoso.test:443, http://[0:0::1]:80 ,ftp://x.test, https://u:p@x.test, https://x.test/path")
	b := BoundaryFromEnv()
	h := b.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(299) }))
	cases := []struct {
		host, origin string
		status       int
		allowOrigin  string
	}{
		{"pages.northwind.test", "", 299, ""},
		{"PAGES.NORTHWIND.TEST:443", "", 299, ""},
		{"pages.northwind.test.", "", 403, ""},
		{"agents.contoso.test:3000", "", 299, ""},
		{"[::1]:8080", "", 299, ""},
		{"127.0.0.1", "", 299, ""},
		{"0x7f.1", "", 299, ""},
		{"evil.test", "", 403, ""},
		{"", "", 403, ""},
		{"user@pages.northwind.test", "", 403, ""},
		{"pages.northwind.test", "https://pages.northwind.test:8443", 299, "https://pages.northwind.test:8443"},
		{"pages.northwind.test", "HTTPS://APP.CONTOSO.TEST", 299, "https://app.contoso.test"},
		{"pages.northwind.test", "http://[::1]", 299, "http://[::1]"},
		{"pages.northwind.test", "https://pages.northwind.test", 403, ""},
		{"pages.northwind.test", "null", 403, ""},
		{"pages.northwind.test", "https://app.contoso.test/", 403, ""},
		{"pages.northwind.test", "https://app.contoso.test\\@evil.test", 299, "https://app.contoso.test"},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Host = c.host
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.status || w.Header().Get("Access-Control-Allow-Origin") != c.allowOrigin {
			t.Errorf("host %q origin %q: %d %q, want %d %q", c.host, c.origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"), c.status, c.allowOrigin)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("host %q: no Cache-Control", c.host)
		}
		if c.allowOrigin != "" && w.Header().Get("Access-Control-Expose-Headers") != "MCP-Protocol-Version, MCP-Session-Id, WWW-Authenticate" {
			t.Errorf("CORS headers missing for %q", c.origin)
		}
	}
	// An empty Origin header is still an Origin header.
	req := httptest.NewRequest("POST", "/mcp", nil)
	req.Host = "pages.northwind.test"
	req.Header["Origin"] = []string{""}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "invalid Origin header") {
		t.Errorf("empty origin: %d %s", w.Code, w.Body.String())
	}
}

func TestBodyParserEdges(t *testing.T) {
	handler := wireHandler(t)
	post := func(body []byte, mutate func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
		req.Host = "localhost"
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer full")
		req.Header.Set("Content-Type", "application/json")
		if mutate != nil {
			mutate(req)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	ping := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	pong := `{"result":{},"jsonrpc":"2.0","id":1}`

	if w := post(append([]byte("\xef\xbb\xbf"), ping...), nil); w.Body.String() != pong {
		t.Errorf("BOM: %d %s", w.Code, w.Body.String())
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(ping)
	_ = zw.Close()
	if w := post(gz.Bytes(), func(r *http.Request) { r.Header.Set("Content-Encoding", "GZIP") }); w.Body.String() != pong {
		t.Errorf("gzip: %d %s", w.Code, w.Body.String())
	}
	if w := post(ping, func(r *http.Request) { r.Header.Set("Content-Encoding", "compress") }); w.Code != 500 {
		t.Errorf("unknown encoding: %d", w.Code)
	}
	if w := post(ping, func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=latin1") }); w.Code != 500 {
		t.Errorf("latin1 charset: %d", w.Code)
	}
	if w := post(ping, func(r *http.Request) { r.Header.Set("Content-Type", `application/json; charset="UTF-8"`) }); w.Body.String() != pong {
		t.Errorf("quoted charset: %d %s", w.Code, w.Body.String())
	}
	if w := post(ping, func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset") }); w.Code != 500 {
		t.Errorf("malformed parameters: %d", w.Code)
	}
	big := append([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`), bytes.Repeat([]byte("x"), 2<<20)...)
	big = append(big, `"}}`...)
	if w := post(big, nil); w.Code != 413 || w.Body.String() != `{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Request body too large"}}` {
		t.Errorf("too large: %d %s", w.Code, w.Body.String())
	}
	if w := post([]byte(`{"jsonrpc":"2.0","id":"\ud800<&>\u2028","method":"ping"}`), nil); w.Body.String() != `{"result":{},"jsonrpc":"2.0","id":"\ud800<&>`+"\u2028"+`"}` {
		t.Errorf("id echo: %s", w.Body.String())
	}
	if w := post(ping, func(r *http.Request) { r.Header.Set("Accept", "application/json;q=0x1, text/event-stream;q=.5") }); w.Body.String() != pong {
		t.Errorf("Number() q values: %d %s", w.Code, w.Body.String())
	}
	// OPTIONS preflight: 204 before the limiter and auth.
	req := httptest.NewRequest("OPTIONS", "/mcp/", nil)
	req.Host = "localhost"
	req.Header.Set("Origin", "http://localhost:3101")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != "http://localhost:3101" {
		t.Errorf("preflight: %d %v", w.Code, w.Header())
	}
	// Paths under the mount fall through to NotFound after auth.
	req = httptest.NewRequest("GET", "/mcp/other", nil)
	req.Host = "localhost"
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Errorf("unauthenticated subpath: %d", w.Code)
	}
	req.Header.Set("Authorization", "Bearer full")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("subpath: %d", w.Code)
	}
}
