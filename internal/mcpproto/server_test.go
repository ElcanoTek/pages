// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcpproto

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/mcpproto/jsjson"
)

// Port of test/mcp-protocol.test.js. The JS ran each message through Pages'
// handler and through the SDK's own McpServer side by side; here the SDK's
// answers are recorded in testdata/protocol-corpus.json (by running the same
// corpus through SDK 1.32.1 McpServer with the same synthetic zod tools) and
// every answer must match byte for byte, key order included.

var testInfo = Implementation{Name: "northwind", Title: "Northwind", Version: "9.9.9"}

const testInstructions = "Use the Northwind tools."

// The synthetic tools' schemas as z.toJSONSchema printed them (draft-07).
const (
	echoIn    = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"value":{"type":"string","minLength":1},"n":{"default":3,"type":"integer","minimum":-9007199254740991,"maximum":9007199254740991}},"required":["value"],"additionalProperties":false}`
	echoOut   = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"value":{"type":"string"},"n":{"type":"number"}},"required":["value","n"],"additionalProperties":false}`
	emptyIn   = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{},"additionalProperties":false}`
	transIn   = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"id":{"anyOf":[{"type":"integer","minimum":-9007199254740991,"maximum":9007199254740991},{"type":"string"}]}},"required":["id"],"additionalProperties":false}`
	transOut  = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"id":{"type":"string"}},"required":["id"],"additionalProperties":false}`
	badOut    = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"ok":{"type":"boolean","const":true},"items":{"type":"array","items":{"type":"string"}}},"required":["ok","items"],"additionalProperties":false}`
	okBoolOut = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`
	okTrueOut = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"ok":{"type":"boolean","const":true}},"required":["ok"],"additionalProperties":false}`
)

func received(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case float64:
		return "number"
	case string:
		return "string"
	case bool:
		return "boolean"
	case []any:
		return "array"
	}
	return "object"
}

func mustObject(t testing.TB, raw json.RawMessage) *jsjson.Object {
	t.Helper()
	v, err := jsjson.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	o, ok := v.(*jsjson.Object)
	if !ok {
		t.Fatalf("not an object: %s", raw)
	}
	return o
}

func strictUnknown(o *jsjson.Object, known ...string) []Issue {
	var extra []string
	for _, k := range o.Keys() {
		if !slices.Contains(known, k) {
			extra = append(extra, `"`+k+`"`)
		}
	}
	if len(extra) == 0 {
		return nil
	}
	word := "key"
	if len(extra) > 1 {
		word = "keys"
	}
	return []Issue{{Message: "Unrecognized " + word + ": " + strings.Join(extra, ", ")}}
}

func strictEmpty(t testing.TB) func(json.RawMessage) (json.RawMessage, error) {
	return func(raw json.RawMessage) (json.RawMessage, error) {
		if issues := strictUnknown(mustObject(t, raw)); issues != nil {
			return nil, &ValidationError{Issues: issues}
		}
		return raw, nil
	}
}

func syntheticTools(t testing.TB) []Tool {
	text := func(s string) []Content { return []Content{TextContent(s)} }
	return []Tool{
		{
			Name: "echo", Title: "Echo", Description: "Echo a value.",
			InputSchema: json.RawMessage(echoIn), OutputSchema: json.RawMessage(echoOut),
			Annotations: json.RawMessage(`{"readOnlyHint":true}`),
			ParseInput: func(raw json.RawMessage) (json.RawMessage, error) {
				o := mustObject(t, raw)
				var issues []Issue
				value, has := o.Get("value")
				switch s, ok := value.(string); {
				case !has:
					issues = append(issues, Issue{Path: []any{"value"}, Message: "Invalid input: expected string, received undefined"})
				case !ok:
					issues = append(issues, Issue{Path: []any{"value"}, Message: "Invalid input: expected string, received " + received(value)})
				case s == "":
					issues = append(issues, Issue{Path: []any{"value"}, Message: "Too small: expected string to have >=1 characters"})
				}
				n, has := o.Get("n")
				if !has {
					n = 3.0
				} else if !jsjson.IsSafeInteger(n) {
					issues = append(issues, Issue{Path: []any{"n"}, Message: "Invalid input: expected int, received " + received(n)})
				}
				issues = append(issues, strictUnknown(o, "value", "n")...)
				if issues != nil {
					return nil, &ValidationError{Issues: issues}
				}
				out := jsjson.NewObject()
				out.Set("value", value)
				out.Set("n", n)
				return jsjson.MustMarshal(out), nil
			},
			Handler: func(_ context.Context, args json.RawMessage, _ Extra) (*CallToolResult, error) {
				return &CallToolResult{Content: text(string(args)), StructuredContent: args}, nil
			},
		},
		{
			Name: "transform", Title: "Transform", Description: "Arguments reach the handler parsed.",
			InputSchema: json.RawMessage(transIn), OutputSchema: json.RawMessage(transOut),
			ParseInput: func(raw json.RawMessage) (json.RawMessage, error) {
				id, _ := mustObject(t, raw).Get("id")
				out := jsjson.NewObject()
				out.Set("id", jsjson.ToString(id))
				return jsjson.MustMarshal(out), nil
			},
			Handler: func(_ context.Context, args json.RawMessage, _ Extra) (*CallToolResult, error) {
				id, _ := mustObject(t, args).Get("id")
				return &CallToolResult{Content: text(id.(string)), StructuredContent: args}, nil
			},
		},
		{
			Name: "bad_output", Title: "Bad output", Description: "Returns structured content its schema refuses.",
			InputSchema: json.RawMessage(emptyIn), OutputSchema: json.RawMessage(badOut), ParseInput: strictEmpty(t),
			ValidateOutput: func(json.RawMessage) error {
				return &ValidationError{Issues: []Issue{
					{Path: []any{"ok"}, Message: "Invalid input: expected true"},
					{Path: []any{"items", 0}, Message: "Invalid input: expected string, received number"},
				}}
			},
			Handler: func(context.Context, json.RawMessage, Extra) (*CallToolResult, error) {
				return &CallToolResult{Content: text("{}"), StructuredContent: json.RawMessage(`{"ok":false,"items":[1]}`)}, nil
			},
		},
		{
			Name: "no_structured", Title: "No structured content", Description: "Omits structuredContent despite an output schema.",
			InputSchema: json.RawMessage(emptyIn), OutputSchema: json.RawMessage(okBoolOut), ParseInput: strictEmpty(t),
			Handler: func(context.Context, json.RawMessage, Extra) (*CallToolResult, error) {
				return &CallToolResult{Content: text("plain")}, nil
			},
		},
		{
			Name: "domain_error", Title: "Domain error", Description: "An isError result skips output validation.",
			InputSchema: json.RawMessage(emptyIn), OutputSchema: json.RawMessage(okTrueOut), ParseInput: strictEmpty(t),
			ValidateOutput: func(json.RawMessage) error { return errors.New("must not run") },
			Handler: func(context.Context, json.RawMessage, Extra) (*CallToolResult, error) {
				return &CallToolResult{Content: text(`{"ok":false}`), StructuredContent: json.RawMessage(`{"ok":false}`), IsError: true}, nil
			},
		},
		{
			Name: "throws", Title: "Throws", Description: "A handler that throws becomes a tool error.",
			InputSchema: json.RawMessage(emptyIn), OutputSchema: json.RawMessage(emptyIn), ParseInput: strictEmpty(t),
			Handler: func(context.Context, json.RawMessage, Extra) (*CallToolResult, error) { return nil, errors.New("boom") },
		},
		{
			Name: "throws_value", Title: "Throws a value", Description: "A non-Error throw is stringified.",
			InputSchema: json.RawMessage(emptyIn), OutputSchema: json.RawMessage(emptyIn), ParseInput: strictEmpty(t),
			Handler: func(context.Context, json.RawMessage, Extra) (*CallToolResult, error) {
				return nil, errors.New("plain string")
			},
		},
	}
}

type recordedExchange struct {
	Request  string `json:"request"`
	Response string `json:"response"`
}

func loadProtocolCorpus(t *testing.T) map[string][]recordedExchange {
	t.Helper()
	raw, err := os.ReadFile("testdata/protocol-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var c map[string][]recordedExchange
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func assertSameAnswers(t *testing.T, srv *Server, corpus []recordedExchange) {
	t.Helper()
	for _, ex := range corpus {
		msg := mustObject(t, json.RawMessage(ex.Request))
		got := string(srv.HandleRequest(context.Background(), msg, nil))
		if got != ex.Response {
			t.Errorf("%s\n got %s\nwant %s", ex.Request, got, ex.Response)
		}
	}
}

func TestAnswersLikeTheSDKForEveryBranch(t *testing.T) {
	srv := MustNewServer(ServerOptions{Info: testInfo, Instructions: testInstructions, Tools: syntheticTools(t)})
	assertSameAnswers(t, srv, loadProtocolCorpus(t)["synthetic"])
}

func TestNoVisibleToolsAnswersLikeTheSDK(t *testing.T) {
	srv := MustNewServer(ServerOptions{Info: testInfo, Instructions: testInstructions})
	assertSameAnswers(t, srv, loadProtocolCorpus(t)["empty"])
}

func TestVersionNegotiation(t *testing.T) {
	srv := MustNewServer(ServerOptions{Info: testInfo, Instructions: testInstructions, Tools: syntheticTools(t)})
	if got := SupportedProtocolVersions(); !slices.Equal(got, []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}) {
		t.Fatalf("supported versions %v", got)
	}
	for _, version := range append(SupportedProtocolVersions(), "2023-01-01", "latest", "2025-11-25 ") {
		req := jsjson.NewObject()
		req.Set("jsonrpc", "2.0")
		req.Set("id", 1.0)
		req.Set("method", "initialize")
		params := mustObject(t, json.RawMessage(`{"capabilities":{},"clientInfo":{"name":"contoso","version":"1"}}`))
		params.Set("protocolVersion", version)
		req.Set("params", params)
		resp := mustObject(t, srv.HandleRequest(context.Background(), req, nil))
		result, _ := resp.Get("result")
		got, _ := result.(*jsjson.Object).Get("protocolVersion")
		want := LatestProtocolVersion
		if IsSupportedProtocolVersion(version) {
			want = version
		}
		if got != want {
			t.Errorf("%q negotiated %v, want %s", version, got, want)
		}
		caps, _ := result.(*jsjson.Object).Get("capabilities")
		if s := string(jsjson.MustMarshal(caps)); s != `{"tools":{"listChanged":false}}` {
			t.Errorf("capabilities %s", s)
		}
	}
}

func TestRegistrationRefusesDuplicatesAndNonObjectSchemas(t *testing.T) {
	tool := syntheticTools(t)[0]
	if _, err := NewServer(ServerOptions{Tools: []Tool{tool, tool}}); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("duplicate: %v", err)
	}
	bad := tool
	bad.InputSchema = json.RawMessage(`{"type":"string"}`)
	if _, err := NewServer(ServerOptions{Tools: []Tool{bad}}); err == nil || !strings.Contains(err.Error(), "object") {
		t.Errorf("string input schema: %v", err)
	}
	bad = tool
	bad.OutputSchema = json.RawMessage(`{"type":"array","items":{"type":"string"}}`)
	if _, err := NewServer(ServerOptions{Tools: []Tool{bad}}); err == nil || !strings.Contains(err.Error(), "object") {
		t.Errorf("array output schema: %v", err)
	}
}

func TestArgumentsReachTheHandlerParsedWithExtra(t *testing.T) {
	probe := syntheticTools(t)[0]
	probe.Name = "probe"
	var seenArgs string
	var seenExtra Extra
	probe.Handler = func(_ context.Context, args json.RawMessage, extra Extra) (*CallToolResult, error) {
		seenArgs, seenExtra = string(args), extra
		return &CallToolResult{Content: []Content{TextContent("{}")}, StructuredContent: json.RawMessage(`{"value":"x","n":1}`)}, nil
	}
	srv := MustNewServer(ServerOptions{Tools: []Tool{probe}})
	msg := mustObject(t, json.RawMessage(`{"jsonrpc":"2.0","id":"req-7","method":"tools/call","params":{"name":"probe","arguments":{"value":"v"}}}`))
	srv.HandleRequest(context.Background(), msg, "opaque")
	if seenArgs != `{"value":"v","n":3}` {
		t.Errorf("args %s: defaults not applied", seenArgs)
	}
	if seenExtra.RequestID != "req-7" || seenExtra.AuthInfo != "opaque" {
		t.Errorf("extra %+v", seenExtra)
	}
}

func TestDotPath(t *testing.T) {
	cases := []struct {
		path []any
		want string
	}{
		{nil, "object root"},
		{[]any{"slug"}, "slug"},
		{[]any{"edits", 0, "find"}, "edits[0].find"},
		{[]any{0, "a"}, "0.a"},
	}
	for _, c := range cases {
		if got := DotPath(c.path); got != c.want {
			t.Errorf("DotPath(%v) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestPanickingHandlerIsAToolError(t *testing.T) {
	tool := syntheticTools(t)[5]
	tool.Handler = func(context.Context, json.RawMessage, Extra) (*CallToolResult, error) { panic("nil map") }
	srv := MustNewServer(ServerOptions{Tools: []Tool{tool}})
	msg := mustObject(t, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"throws","arguments":{}}}`))
	got := string(srv.HandleRequest(context.Background(), msg, nil))
	if got != `{"result":{"content":[{"type":"text","text":"internal error"}],"isError":true},"jsonrpc":"2.0","id":1}` {
		t.Errorf("got %s", got)
	}
}

// The vendored envelope schemas must accept and refuse exactly what the
// SDK's do: the HTTP layer uses them to decide 400 / -32600 / -32602 before
// dispatch. testdata/envelope-verdicts.json is the SDK's verdict per probe
// (the JS test's probes plus more).
func TestEnvelopeSchemasAgreeWithTheSDK(t *testing.T) {
	raw, err := os.ReadFile("testdata/envelope-verdicts.json")
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]func(any) bool{
		"RequestIdSchema":                  IsRequestID,
		"JSONRPCRequestSchema":             IsJSONRPCRequest,
		"JSONRPCMessageSchema":             IsJSONRPCMessage,
		"TaskAugmentedRequestParamsSchema": IsTaskAugmentedParams,
		"InitializeRequestSchema":          IsInitializeRequest,
		"PingRequestSchema":                IsPingRequest,
		"ListToolsRequestSchema":           IsListToolsRequest,
		"CallToolRequestSchema":            IsCallToolRequest,
	}
	for _, row := range rows {
		text := row["value"].(string)
		v, err := jsjson.Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		for name, schema := range schemas {
			if got, want := schema(v), row[name].(bool); got != want {
				t.Errorf("%s on %s: got %v, SDK says %v", name, text, got, want)
			}
		}
	}
	if len(rows) < 300 {
		t.Fatalf("only %d probes", len(rows))
	}
}
