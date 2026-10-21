// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// The HTTP test harness: the real /mcp handler (plus the /upload ticket and
// /export download endpoints a run uses) over httptest, against a fresh
// migrated database. Requests are sent the way test/mcp.integration.js sent
// them: Host localhost, both Accept types, a bearer token.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/dataexport"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/uploads"
)

const (
	dashOrigin    = "http://localhost:3100"
	contentOrigin = "http://content.localhost:3100"
	protocol      = "2025-11-25"
)

// setTestEnv is the integration environment test/run-integration.sh set.
func setTestEnv(t testing.TB) {
	t.Helper()
	for k, v := range map[string]string{
		"DASHBOARD_ORIGIN":    dashOrigin,
		"CONTENT_ORIGIN":      contentOrigin,
		"DASHBOARD_HOST":      "localhost",
		"CONTENT_HOST":        "content.localhost",
		"RAW_TOKEN_SECRET":    "integration-secret",
		"API_TOKEN_PEPPER":    "integration-pepper",
		"PAGE_COOKIE_SECRET":  "integration-page-secret",
		"PG_LOCK_TIMEOUT_MS":  "2000",
		"MCP_ALLOWED_HOSTS":   "",
		"MCP_ALLOWED_ORIGINS": "",
	} {
		t.Setenv(k, v)
	}
}

type harness struct {
	t       *testing.T
	d       *db.DB
	srv     *httptest.Server
	token   string
	tokenID jsx.ID
	nextID  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	setTestEnv(t)
	d := testdb.New(t)
	h := &harness{t: t, d: d, nextID: 1}
	mux := http.NewServeMux()
	mcpHandler := Handler(Deps{DB: d, ClientIP: func(*http.Request) string { return "127.0.0.1" }})
	mux.Handle("/mcp", mcpHandler)
	mux.Handle("/mcp/", mcpHandler)
	mux.Handle("/upload/", http.StripPrefix("/upload", uploads.NewHandler(uploads.HandlerOptions{DB: d})))
	mux.Handle("/export/", http.StripPrefix("/export", &dataexport.Handler{DB: d}))
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	m := h.mint("mcp-agent", tokens.ScopeDeploy)
	h.token, h.tokenID = m.Token, m.ID
	return h
}

func (h *harness) mint(label, scope string, slugs ...string) *tokens.Minted {
	h.t.Helper()
	m, err := tokens.Mint(context.Background(), h.d, tokens.MintParams{Label: label, Scope: scope, AllowedSlugs: slugs})
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

// reqOpts mirror the JS request() options. token "-" sends no bearer.
type reqOpts struct {
	host        string
	token       string
	noToken     bool
	accept      *string
	origin      *string
	contentType *string
	headers     map[string]string
	body        any // marshalled with jsonv.Stringify
	rawBody     *string
}

type response struct {
	status int
	header http.Header
	body   string
	json   any
}

func (r response) get(path ...any) any { return dig(r.json, path...) }

func dig(v any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				return nil
			}
			v = a[k]
		}
	}
	return v
}

func strAt(v any, path ...any) string {
	s, _ := dig(v, path...).(string)
	return s
}

func numAt(v any, path ...any) float64 {
	n, _ := dig(v, path...).(float64)
	return n
}

func (h *harness) request(method, path string, o reqOpts) response {
	h.t.Helper()
	var payload []byte
	hasBody := false
	switch {
	case o.rawBody != nil:
		payload, hasBody = []byte(*o.rawBody), true
	case o.body != nil:
		payload, hasBody = []byte(jsonv.Stringify(o.body)), true
	}
	var rd io.Reader
	if hasBody {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Host = "localhost"
	if o.host != "" {
		req.Host = o.host
	}
	accept := "application/json, text/event-stream"
	if o.accept != nil {
		accept = *o.accept
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	token := h.token
	if o.token != "" {
		token = o.token
	}
	if !o.noToken && token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if o.origin != nil {
		req.Header.Set("Origin", *o.origin)
	}
	if hasBody {
		ct := "application/json"
		if o.contentType != nil {
			ct = *o.contentType
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
	}
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	res, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	out := response{status: res.StatusCode, header: res.Header, body: string(b)}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &out.json)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

type rpcOpts struct {
	reqOpts
	notification   bool
	id             any
	protocolHeader *string // nil ⇒ the current version; "" ⇒ none
}

func (h *harness) rpc(method string, params any, o rpcOpts) response {
	h.t.Helper()
	msg := jsonv.ObjectOf("jsonrpc", "2.0", "method", method)
	if params != nil {
		msg.Set("params", params)
	}
	if !o.notification {
		if o.id != nil {
			msg.Set("id", o.id)
		} else {
			msg.Set("id", float64(h.nextID))
			h.nextID++
		}
	}
	headers := map[string]string{}
	for k, v := range o.headers {
		headers[k] = v
	}
	if method != "initialize" {
		switch {
		case o.protocolHeader == nil:
			headers["MCP-Protocol-Version"] = protocol
		case *o.protocolHeader != "":
			headers["MCP-Protocol-Version"] = *o.protocolHeader
		}
	}
	ro := o.reqOpts
	ro.headers = headers
	ro.body = msg
	return h.request("POST", "/mcp", ro)
}

func (h *harness) initialize(version string, o rpcOpts) response {
	return h.rpc("initialize", jsonv.ObjectOf(
		"protocolVersion", version,
		"capabilities", jsonv.NewObject(),
		"clientInfo", jsonv.ObjectOf("name", "pages-integration", "title", "Pages Integration", "version", "1.0.0"),
	), o)
}

func (h *harness) call(name string, args any, opts ...rpcOpts) response {
	h.t.Helper()
	var o rpcOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	if args == nil {
		args = jsonv.NewObject()
	}
	return h.rpc("tools/call", jsonv.ObjectOf("name", name, "arguments", args), o)
}

// as calls with another bearer token.
func as(token string) rpcOpts { return rpcOpts{reqOpts: reqOpts{token: token}} }

// data is toolData: a successful result whose text mirrors structuredContent.
func (h *harness) data(r response) map[string]any {
	h.t.Helper()
	if r.status != 200 {
		h.t.Fatalf("tool HTTP status %d: %s", r.status, r.body)
	}
	result, _ := r.get("result").(map[string]any)
	if result == nil {
		h.t.Fatalf("expected a tool result, got %s", r.body)
	}
	if result["isError"] == true {
		h.t.Fatalf("unexpected tool error: %s", clipText(r.body))
	}
	text := strAt(result, "content", 0, "text")
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		h.t.Fatalf("text content is not JSON: %v", err)
	}
	if !jsonEqual(parsed, result["structuredContent"]) {
		h.t.Fatalf("structuredContent does not mirror the text JSON: %s", clipText(r.body))
	}
	out, _ := parsed.(map[string]any)
	return out
}

// ordered is a tool result's text content parsed with key order intact.
func ordered(t *testing.T, r response) *jsonv.Object {
	t.Helper()
	v, err := jsonv.ParseLossless([]byte(strAt(r.json, "result", "content", 0, "text")))
	if err != nil {
		t.Fatal(err)
	}
	obj, _ := v.(*jsonv.Object)
	return obj
}

func clipText(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

type toolFailure struct {
	text string
	data map[string]any // nil for SDK argument-validation text
}

func (f toolFailure) code() string { return strAt(f.data, "code") }

// fails is toolError: an isError result; data is its JSON body when it has
// one.
func (h *harness) fails(r response) toolFailure {
	h.t.Helper()
	if r.status != 200 {
		h.t.Fatalf("tool error HTTP status %d: %s", r.status, r.body)
	}
	if r.get("result", "isError") != true {
		h.t.Fatalf("expected a tool error, got %s", clipText(r.body))
	}
	text := strAt(r.json, "result", "content", 0, "text")
	f := toolFailure{text: text}
	var parsed map[string]any
	if json.Unmarshal([]byte(text), &parsed) == nil {
		f.data = parsed
		if sc := r.get("result", "structuredContent"); sc != nil && !jsonEqual(sc, parsed) {
			h.t.Fatalf("structured tool error does not mirror its text JSON")
		}
	}
	return f
}

func (h *harness) query(sql string, args ...any) []map[string]any {
	h.t.Helper()
	rows, err := h.d.Query(context.Background(), sql, args...)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			h.t.Fatal(err)
		}
		m := map[string]any{}
		for i, f := range rows.FieldDescriptions() {
			m[f.Name] = vals[i]
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *harness) scalar(sql string, args ...any) any {
	h.t.Helper()
	rows := h.query(sql, args...)
	if len(rows) == 0 {
		return nil
	}
	for _, v := range rows[0] {
		return v
	}
	return nil
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	switch v := h.scalar(sql, args...).(type) {
	case int32:
		return int(v)
	case int64:
		return int(v)
	}
	h.t.Fatalf("count query did not return an integer: %s", sql)
	return 0
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.d.Exec(context.Background(), sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func shaHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func pageHTML(n int) string {
	return fmt.Sprintf(`<!doctype html><html><head><title>MCP</title></head><body><h1>v%d</h1><canvas id=c></canvas><script>chart(%d)</script></body></html>`, n, n)
}

// o builds an ordered JSON object for arguments.
func o(kv ...any) *jsonv.Object { return jsonv.ObjectOf(kv...) }

// arr is a JSON array literal.
func arr(items ...any) []any { return items }

// num is a JSON number.
func num(n float64) float64 { return n }

func mustMatch(t *testing.T, pattern, s string, msg ...any) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(s) {
		t.Fatalf("%v: %q does not match /%s/", msg, clipText(s), pattern)
	}
}

func mustNotMatch(t *testing.T, pattern, s string, msg ...any) {
	t.Helper()
	if regexp.MustCompile(pattern).MatchString(s) {
		t.Fatalf("%v: %q matches /%s/", msg, clipText(s), pattern)
	}
}

func eq(t *testing.T, got, want any, msg ...any) {
	t.Helper()
	if !jsonEqual(normalizeNum(got), normalizeNum(want)) {
		t.Fatalf("%v: got %s, want %s", msg, clipText(fmt.Sprint(jsonText(got))), clipText(fmt.Sprint(jsonText(want))))
	}
}

func normalizeNum(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int32:
		return float64(x)
	case int64:
		return float64(x)
	}
	return v
}

func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func truthyAt(v any, path ...any) bool {
	switch x := dig(v, path...).(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	}
	return true
}

var _ = strings.Contains
