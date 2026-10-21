// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/mcp-wire.integration.js: every case is sent over real HTTP and
// its status, response headers and EXACT body are compared with
// internal/mcp/testdata/mcp-wire.golden.json, recorded from the official MCP TypeScript SDK
// transport (1.32.1). The dashboard's security headers are server wiring
// (internal/server) and are not part of this handler, so they are not
// compared here.

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/nodedefaults"
	"github.com/ElcanoTek/pages/internal/pagepatch"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/version"
)

type wireCase struct {
	name    string
	method  string
	body    any     // a jsonv value, serialized as JSON.stringify would
	raw     *string // a raw body
	headers map[string]*string
	scoped  bool
}

var volatileHeaders = map[string]bool{"date": true, "connection": true, "keep-alive": true, "ratelimit": true, "ratelimit-policy": true, "transfer-encoding": true}

func wireCases() []wireCase {
	const v = protocol
	pv := map[string]*string{"MCP-Protocol-Version": ptr(v)}
	with := func(extra map[string]*string) map[string]*string {
		out := map[string]*string{"MCP-Protocol-Version": ptr(v)}
		for k, x := range extra {
			out[k] = x
		}
		return out
	}
	req := func(id any, method string, params any) *jsonv.Object {
		m := o("jsonrpc", "2.0", "id", id, "method", method)
		if params != nil {
			m.Set("params", params)
		}
		return m
	}
	note := func(method string, params any) *jsonv.Object {
		m := o("jsonrpc", "2.0", "method", method)
		if params != nil {
			m.Set("params", params)
		}
		return m
	}
	client := func() *jsonv.Object { return o("name", "wire", "version", "1.0.0") }
	init := func(version string, extra ...any) *jsonv.Object {
		params := o("protocolVersion", version, "capabilities", o(), "clientInfo", client())
		for i := 0; i+1 < len(extra); i += 2 {
			params.Set(extra[i].(string), extra[i+1])
		}
		return req(float64(1), "initialize", params)
	}
	withID := func(m *jsonv.Object, id any) *jsonv.Object {
		c := m.Clone()
		c.Set("id", id)
		return c
	}
	withKey := func(m *jsonv.Object, k string, val any) *jsonv.Object {
		c := m.Clone()
		c.Set(k, val)
		return c
	}
	n := func(f float64) float64 { return f }
	return []wireCase{
		{name: "initialize latest", body: init(v)},
		{name: "initialize 2025-06-18", body: init("2025-06-18")},
		{name: "initialize 2025-03-26", body: init("2025-03-26")},
		{name: "initialize 2024-11-05", body: init("2024-11-05")},
		{name: "initialize 2024-10-07", body: init("2024-10-07")},
		{name: "initialize unknown version falls back to latest", body: init("2099-01-01")},
		{name: "initialize ignores a bogus protocol header", body: init(v), headers: map[string]*string{"MCP-Protocol-Version": ptr("1999-01-01")}},
		{name: "initialize with client capabilities", body: init(v, "capabilities", o("roots", o("listChanged", true), "sampling", o(), "elicitation", o()))},
		{name: "initialize missing capabilities", body: req(n(1), "initialize", o("protocolVersion", v, "clientInfo", client()))},
		{name: "initialize bad clientInfo", body: req(n(1), "initialize", o("protocolVersion", v, "capabilities", o(), "clientInfo", o("name", n(1))))},
		{name: "initialize bad capability shape", body: init(v, "capabilities", o("roots", n(5)))},
		{name: "initialize string id", body: withID(init(v), "abc")},
		{name: "initialize as a notification", body: note("initialize", o("protocolVersion", v, "capabilities", o(), "clientInfo", client()))},
		{name: "initialized notification", body: note("notifications/initialized", nil), headers: pv},
		{name: "initialized notification with params", body: note("notifications/initialized", o()), headers: pv},
		{name: "unknown notification", body: note("notifications/whatever", o("a", n(1))), headers: pv},
		{name: "request method sent as a notification", body: note("tools/list", nil), headers: pv},
		{name: "notification with an unsupported protocol header", body: note("notifications/initialized", nil), headers: map[string]*string{"MCP-Protocol-Version": ptr("2099-01-01")}},
		{name: "client result response", body: o("jsonrpc", "2.0", "id", n(9), "result", o()), headers: pv},
		{name: "client error response", body: o("jsonrpc", "2.0", "id", n(9), "error", o("code", n(-1), "message", "x")), headers: pv},

		{name: "ping", body: req(n(2), "ping", nil), headers: pv},
		{name: "ping without a protocol header", body: req(n(2), "ping", nil)},
		{name: "ping with empty params", body: req(n(2), "ping", o()), headers: pv},
		{name: "ping with _meta", body: req(n(2), "ping", o("_meta", o("progressToken", n(7)))), headers: pv},
		{name: "ping with bad _meta", body: req(n(2), "ping", o("_meta", o("progressToken", 1.5))), headers: pv},
		{name: "ping with task augmentation", body: req(n(2), "ping", o("task", o())), headers: pv},
		{name: "ping with array params", body: req(n(2), "ping", arr(n(1))), headers: pv},
		{name: "ping negative id", body: req(n(-4), "ping", nil), headers: pv},
		{name: "ping float id", body: req(1.5, "ping", nil), headers: pv},
		{name: "ping null id", body: req(nil, "ping", nil), headers: pv},
		{name: "ping with an extra top-level key", body: withKey(req(n(2), "ping", nil), "extra", true), headers: pv},
		{name: "ping unsupported protocol header", body: req(n(2), "ping", nil), headers: map[string]*string{"MCP-Protocol-Version": ptr("2099-01-01")}},
		{name: "ping empty protocol header", body: req(n(2), "ping", nil), headers: map[string]*string{"MCP-Protocol-Version": ptr("")}},
		{name: "ping legacy protocol header", body: req(n(2), "ping", nil), headers: map[string]*string{"MCP-Protocol-Version": ptr("2024-11-05")}},
		{name: "unknown method", body: req(n(3), "frobnicate", o()), headers: pv},
		{name: "unknown method without params", body: req(n(3), "resources/list", nil), headers: pv},
		{name: "notification-shaped method with an id", body: req(n(3), "notifications/initialized", nil), headers: pv},
		{name: "wrong jsonrpc version", body: o("jsonrpc", "1.0", "id", n(3), "method", "ping"), headers: pv},
		{name: "missing method", body: o("jsonrpc", "2.0", "id", n(3)), headers: pv},

		{name: "tools/list", body: req(n(4), "tools/list", nil), headers: pv},
		{name: "tools/list with _meta", body: req(n(4), "tools/list", o("_meta", o())), headers: pv},
		{name: "tools/list cursor", body: req(n(4), "tools/list", o("cursor", "x")), headers: pv},
		{name: "tools/list bad cursor type", body: req(n(4), "tools/list", o("cursor", n(5))), headers: pv},
		{name: "tools/call unknown tool", body: req(n(5), "tools/call", o("name", "nope", "arguments", o())), headers: pv},
		{name: "tools/call prototype tool name", body: req(n(5), "tools/call", o("name", "__proto__", "arguments", o())), headers: pv},
		{name: "tools/call missing name", body: req(n(5), "tools/call", o("arguments", o())), headers: pv},
		{name: "tools/call array arguments", body: req(n(5), "tools/call", o("name", "get_page", "arguments", arr())), headers: pv},
		{name: "tools/call without params", body: req(n(5), "tools/call", nil), headers: pv},
		{name: "tools/call invalid arguments", body: req(n(5), "tools/call", o("name", "get_page", "arguments", o("slug", "x", "bogus", n(1)))), headers: pv},
		{name: "tools/call missing required argument", body: req(n(5), "tools/call", o("name", "get_page", "arguments", o())), headers: pv},
		{name: "tools/call omitted arguments", body: req(n(5), "tools/call", o("name", "get_page")), headers: pv},
		{name: "tools/call nested invalid argument", body: req(n(5), "tools/call", o("name", "patch_page", "arguments", o("slug", "x", "edits", arr(o("find", n(1)))))), headers: pv},
		{name: "tools/call domain error", body: req(n(5), "tools/call", o("name", "get_page", "arguments", o("slug", "wire-no-such-page"))), headers: pv},
		{name: "tools/call success", body: req(n(5), "tools/call", o("name", "list_themes", "arguments", o())), headers: pv},
		{name: "tools/call task augmentation", body: req(n(5), "tools/call", o("name", "list_themes", "arguments", o(), "task", o())), headers: pv},
		{name: "tools/call bad task", body: req(n(5), "tools/call", o("name", "list_themes", "arguments", o(), "task", n(5))), headers: pv},
		{name: "tools/call as a notification", body: note("tools/call", o("name", "list_themes", "arguments", o())), headers: pv},
		{name: "tools/call unsupported protocol header", body: req(n(5), "tools/call", o("name", "list_themes", "arguments", o())), headers: map[string]*string{"MCP-Protocol-Version": ptr("2099-01-01")}},

		{name: "malformed JSON", raw: ptr("{"), headers: pv},
		{name: "empty body", raw: ptr(""), headers: pv},
		{name: "JSON null", raw: ptr("null"), headers: pv},
		{name: "JSON string", raw: ptr(`"ping"`), headers: pv},
		{name: "JSON number", raw: ptr("7"), headers: pv},
		{name: "empty batch", raw: ptr("[]"), headers: pv},
		{name: "batch of one", body: arr(req(n(6), "ping", nil)), headers: pv},
		{name: "batch of notifications", body: arr(note("notifications/initialized", nil)), headers: pv},

		{name: "Content-Type with charset", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Content-Type": ptr("application/json; charset=utf-8")})},
		{name: "Content-Type upper case", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Content-Type": ptr("APPLICATION/JSON")})},
		{name: "Content-Type text/plain", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Content-Type": ptr("text/plain")})},
		{name: "Content-Type missing", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Content-Type": nil})},
		{name: "Accept JSON only", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Accept": ptr("application/json")})},
		{name: "Accept wildcard", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Accept": ptr("*/*")})},
		{name: "Accept upper case", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Accept": ptr("APPLICATION/JSON, TEXT/EVENT-STREAM")})},
		{name: "Accept with q=0", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Accept": ptr("application/json, text/event-stream;q=0")})},
		{name: "Accept missing", body: req(n(7), "ping", nil), headers: with(map[string]*string{"Accept": nil})},
		{name: "GET", method: "GET", headers: map[string]*string{"Content-Type": nil}},
		{name: "DELETE", method: "DELETE", headers: map[string]*string{"Content-Type": nil}},
		{name: "PUT", method: "PUT", body: req(n(8), "ping", nil), headers: pv},
		{name: "unauthenticated", body: req(n(8), "ping", nil), headers: with(map[string]*string{"Authorization": nil})},

		{name: "scoped tools/list", scoped: true, body: req(n(9), "tools/list", nil), headers: pv},
		{name: "scoped call to an unregistered tool", scoped: true, body: req(n(9), "tools/call", o("name", "deploy_page", "arguments", o())), headers: pv},
		{name: "scoped call to a registered tool", scoped: true, body: req(n(9), "tools/call", o("name", "get_page_data", "arguments", o("slug", "wire-no-such-page"))), headers: pv},
		{name: "scoped initialize", scoped: true, body: init(v)},
	}
}

type wireResult struct {
	status  int
	headers map[string]string
	body    string
}

func (h *harness) send(c wireCase, token string) wireResult {
	h.t.Helper()
	method := c.method
	if method == "" {
		method = "POST"
	}
	var payload *string
	switch {
	case c.raw != nil:
		payload = c.raw
	case c.body != nil:
		payload = ptr(jsonv.Stringify(c.body))
	}
	var rd io.Reader
	if payload != nil {
		rd = strings.NewReader(*payload)
	}
	req, err := http.NewRequest(method, h.srv.URL+"/mcp", rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Host = "localhost"
	headers := map[string]*string{
		"Accept":        ptr("application/json, text/event-stream"),
		"Authorization": ptr("Bearer " + token),
	}
	if payload != nil {
		headers["Content-Type"] = ptr("application/json")
	}
	for k, v := range c.headers {
		headers[k] = v
	}
	for k, v := range headers {
		if v != nil {
			req.Header.Set(k, *v)
		}
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	data := strings.ReplaceAll(string(raw), `"version":"`+version.String()+`"`, `"version":"<package.json version>"`)
	kept := map[string]string{}
	for k, vals := range res.Header {
		lk := strings.ToLower(k)
		if !volatileHeaders[lk] {
			kept[lk] = strings.Join(vals, ", ")
		}
	}
	if kept["content-length"] == strconv.Itoa(len(raw)) {
		kept["content-length"] = "<body length>"
	}
	body := data
	if len(data) > 4096 {
		body = digest(data)
	}
	return wireResult{status: res.StatusCode, headers: kept, body: body}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:]) + " bytes:" + strconv.Itoa(len(s))
}

// legacyToolsList undoes the reviewed additions to a tools/list body (issue
// #109's data_usage, and pagepatch's raised bounds) so it can be compared
// with the SDK recording.
func legacyToolsList(t *testing.T, body string) string {
	t.Helper()
	v, err := jsonv.ParseLossless([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	resp := v.(*jsonv.Object)
	tools := resp.Value("result").(*jsonv.Object).Value("tools").([]any)
	restore := map[float64]float64{
		float64(pagepatch.MaxEdits): 25, float64(pagepatch.MaxFindChars): 8192,
		float64(pagepatch.MaxReplaceChars): 128 * 1024, float64(pagepatch.MaxExpectedCount): 100,
	}
	var walk func(x any)
	walk = func(x any) {
		switch y := x.(type) {
		case *jsonv.Object:
			for _, k := range []string{"maxItems", "maxLength", "maximum"} {
				if n, ok := y.Value(k).(float64); ok {
					if old, raised := restore[n]; raised {
						y.Set(k, old)
					}
				}
			}
			y.Range(func(_ string, e any) bool { walk(e); return true })
		case []any:
			for _, e := range y {
				walk(e)
			}
		}
	}
	for _, e := range tools {
		tool := e.(*jsonv.Object)
		tool.Set("description", strings.Replace(tool.Value("description").(string), usageDescription, "", 1))
		if props, ok := tool.Value("outputSchema").(*jsonv.Object).Value("properties").(*jsonv.Object); ok {
			props.Delete("data_usage")
		}
		if name := tool.Value("name"); name == "patch_page" || name == "find_in_version" {
			walk(tool.Value("inputSchema"))
		}
	}
	return jsonv.Stringify(resp)
}

func TestWireMatchesGolden(t *testing.T) {
	nodedefaults.Setenv(t)
	h := newHarness(t)
	scoped := h.mint("mcp-wire-data", tokens.ScopeDataUpdate, "wire-no-such-page")

	raw, err := os.ReadFile("testdata/mcp-wire.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	gv, err := jsonv.ParseLossless(raw)
	if err != nil {
		t.Fatal(err)
	}
	golden := gv.(*jsonv.Object)
	cases := wireCases()
	if len(cases)+1 != golden.Len() {
		t.Fatalf("%d cases, golden has %d entries", len(cases), golden.Len()-1)
	}
	for _, c := range cases {
		token := h.token
		if c.scoped {
			token = scoped.Token
		}
		got := h.send(c, token)
		if c.name == "tools/list" || c.name == "tools/list with _meta" || c.name == "scoped tools/list" {
			// Re-send to get the full body, then compare it with the
			// recording once the reviewed additions are undone.
			full := h.sendFull(c, token)
			got.body = digest(legacyToolsList(t, full))
		}
		g, ok := golden.Value(c.name).(*jsonv.Object)
		if !ok {
			t.Errorf("golden has no case %q", c.name)
			continue
		}
		want := wireResult{status: int(g.Value("status").(float64)), headers: map[string]string{}, body: g.Value("body").(string)}
		g.Value("headers").(*jsonv.Object).Range(func(k string, v any) bool {
			want.headers[k] = v.(string)
			return true
		})
		switch {
		case c.name == "Accept upper case":
			// Media types are case-insensitive (RFC 9110); the SDK
			// transport refused upper case. A reviewed departure.
			want = wireResult{status: 200, headers: map[string]string{"cache-control": "no-store", "content-length": "<body length>", "content-type": "application/json"},
				body: `{"result":{},"jsonrpc":"2.0","id":7}`}
		case want.status == 202:
			// A 202 now ends with Content-Length: 0 rather than an empty
			// chunked body.
			want.headers["content-length"] = "<body length>"
		}
		if got.status != want.status || got.body != want.body || !sameHeaders(got.headers, want.headers) {
			t.Errorf("wire case %q drifted:\n got: %d %v %s\nwant: %d %v %s", c.name, got.status, got.headers, got.body, want.status, want.headers, want.body)
		}
	}
}

func (h *harness) sendFull(c wireCase, token string) string {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.srv.URL+"/mcp", strings.NewReader(jsonv.Stringify(c.body)))
	req.Host = "localhost"
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", protocol)
	res, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func sameHeaders(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}
