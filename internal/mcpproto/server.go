// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package mcpproto is the stateless MCP server core behind POST /mcp, and the
// HTTP boundary around it.
//
// Pages used to run the official MCP TypeScript SDK's McpServer and
// StreamableHTTPServerTransport. In stateless JSON-response mode (one
// transport per request, no sessions, no SSE, no batches) almost all of that
// machinery is idle: what remains is JSON-RPC 2.0 dispatch of four methods
// (initialize, ping, tools/list, tools/call). This package is that remainder,
// written to answer exactly as SDK 1.32.1 did — methods, error codes and
// messages, result shapes and key order — and the tests hold it to recordings
// of the SDK itself (testdata/protocol-corpus.json) and of the SDK transport
// over HTTP (test/mcp-wire.golden.json).
//
// The tools are not here: a Tool carries its advertised JSON Schemas as raw
// JSON and its own argument parser and handler (package mcp registers Pages'
// catalog). Everything on the wire is serialized as JavaScript would
// (package jsjson), so key order, number formatting and escaping match.
package mcpproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/ElcanoTek/pages/internal/mcpproto/jsjson"
)

// LatestProtocolVersion is the newest MCP revision; initialize answers with it
// when the client asks for one this server does not know.
const LatestProtocolVersion = "2025-11-25"

// JSONRPCVersion is the only JSON-RPC version spoken.
const JSONRPCVersion = "2.0"

var supportedProtocolVersions = []string{LatestProtocolVersion, "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"}

// SupportedProtocolVersions returns every MCP revision SDK 1.32.1 negotiated,
// newest first.
func SupportedProtocolVersions() []string { return slices.Clone(supportedProtocolVersions) }

// IsSupportedProtocolVersion reports whether v is one of them, exactly.
func IsSupportedProtocolVersion(v string) bool { return slices.Contains(supportedProtocolVersions, v) }

// JSON-RPC error codes.
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// Error is the SDK's McpError: its message is "MCP error <code>: <message>",
// which is the text a model sees when a tools/call fails before (or after) a
// Pages handler runs.
type Error struct {
	Code    int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

// Issue is one schema violation: the path into the value (string keys and int
// indexes) and zod's message for it.
type Issue struct {
	Path    []any
	Message string
}

// ValidationError is returned by Tool.ParseInput and Tool.ValidateOutput when
// the value does not fit the schema. Its text is the SDK's rendering of zod
// issues: "<message> at <path>" per issue (path omitted when empty), joined
// by newlines.
type ValidationError struct {
	Issues []Issue
}

func (e *ValidationError) Error() string {
	lines := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		if len(issue.Path) > 0 {
			lines[i] = issue.Message + " at " + DotPath(issue.Path)
		} else {
			lines[i] = issue.Message
		}
	}
	return strings.Join(lines, "\n")
}

// DotPath formats an issue path as the SDK did: `edits[0].find`, or
// "object root" for the empty path.
func DotPath(path []any) string {
	if len(path) == 0 {
		return "object root"
	}
	var b strings.Builder
	for i, seg := range path {
		switch s := seg.(type) {
		case int:
			if i == 0 {
				fmt.Fprint(&b, s)
			} else {
				fmt.Fprintf(&b, "[%d]", s)
			}
		default:
			if i > 0 {
				b.WriteByte('.')
			}
			fmt.Fprint(&b, s)
		}
	}
	return b.String()
}

// Content is one content block of a tool result (Pages only emits text).
type Content struct {
	Type string
	Text string
}

// TextContent is a {"type":"text"} block.
func TextContent(text string) Content { return Content{Type: "text", Text: text} }

// CallToolResult is a tools/call result. StructuredContent is raw JSON (nil
// when absent); it is re-serialized in JavaScript form on the way out.
type CallToolResult struct {
	Content           []Content
	StructuredContent json.RawMessage
	IsError           bool
}

func (r *CallToolResult) appendJSON(b []byte) ([]byte, error) {
	b = append(b, `{"content":[`...)
	for i, c := range r.Content {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"type":`...)
		b = jsjson.AppendString(b, c.Type)
		b = append(b, `,"text":`...)
		b = jsjson.AppendString(b, c.Text)
		b = append(b, '}')
	}
	b = append(b, ']')
	if r.hasStructured() {
		b = append(b, `,"structuredContent":`...)
		var err error
		if b, err = jsjson.AppendValue(b, r.StructuredContent); err != nil {
			return nil, fmt.Errorf("structuredContent: %w", err)
		}
	}
	if r.IsError {
		b = append(b, `,"isError":true`...)
	}
	return append(b, '}'), nil
}

// hasStructured mirrors the SDK's `!result.structuredContent`.
func (r *CallToolResult) hasStructured() bool {
	s := strings.TrimSpace(string(r.StructuredContent))
	return s != "" && s != "null" && s != "false" && s != "0" && s != `""`
}

func toolErrorResult(message string) *CallToolResult {
	return &CallToolResult{Content: []Content{TextContent(message)}, IsError: true}
}

// Extra is what a tool handler learns about its request.
type Extra struct {
	// RequestID is String(id) of the JSON-RPC request, for logs.
	RequestID string
	// AuthInfo is whatever the transport attached (the HTTP handler passes
	// HandlerOptions.AuthInfo's value).
	AuthInfo any
}

// Tool is one registered tool.
type Tool struct {
	Name        string
	Title       string // omitted from tools/list when empty
	Description string // omitted from tools/list when empty
	// InputSchema is the advertised JSON Schema of the arguments: a JSON
	// object with "type":"object". Required.
	InputSchema json.RawMessage
	// OutputSchema is the advertised JSON Schema of structuredContent, or
	// nil. When set, a non-error result must carry structuredContent.
	OutputSchema json.RawMessage
	// Annotations is the tool's annotations object, or nil.
	Annotations json.RawMessage
	// ParseInput validates the arguments (a JSON object; {} when the client
	// sent none) and returns them parsed — defaults applied, transforms run —
	// as the handler will see them. A *ValidationError becomes the SDK's
	// "Input validation error" result. nil passes the arguments through.
	ParseInput func(args json.RawMessage) (json.RawMessage, error)
	// ValidateOutput checks a non-error result's structuredContent against
	// the output schema; a *ValidationError becomes the SDK's "Output
	// validation error" result. nil skips the check.
	ValidateOutput func(structured json.RawMessage) error
	// Handler runs the tool. A returned error becomes an isError result
	// whose text is err.Error(), as a throwing SDK handler did.
	Handler func(ctx context.Context, args json.RawMessage, extra Extra) (*CallToolResult, error)
}

// Implementation is the serverInfo of initialize.
type Implementation struct {
	Name    string
	Title   string // omitted when empty
	Version string
}

// Server answers JSON-RPC requests for one registry of tools. It is cheap to
// build: Pages builds one per HTTP request, filtered to the token's tools.
type Server struct {
	info         Implementation
	instructions string
	tools        []*registered
	byName       map[string]*registered
	logger       *slog.Logger
}

type registered struct {
	Tool
	definition []byte // tools/list entry, serialized once
}

// ServerOptions configures NewServer.
type ServerOptions struct {
	Info         Implementation
	Instructions string // initialize omits it when empty
	Tools        []Tool
	Logger       *slog.Logger // default slog.Default()
}

// NewServer registers tools. Duplicate names and non-object schemas are
// refused, as the SDK's registerTool refused them.
func NewServer(opts ServerOptions) (*Server, error) {
	s := &Server{info: opts.Info, instructions: opts.Instructions, byName: map[string]*registered{}, logger: opts.Logger}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	for _, t := range opts.Tools {
		if _, dup := s.byName[t.Name]; dup {
			return nil, fmt.Errorf("tool %s is already registered", t.Name)
		}
		if !isObjectSchema(t.InputSchema) || (t.OutputSchema != nil && !isObjectSchema(t.OutputSchema)) {
			return nil, fmt.Errorf("tool %s must declare object input and output schemas", t.Name)
		}
		if t.Handler == nil {
			return nil, fmt.Errorf("tool %s has no handler", t.Name)
		}
		def, err := toolDefinition(t)
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", t.Name, err)
		}
		r := &registered{Tool: t, definition: def}
		s.tools = append(s.tools, r)
		s.byName[t.Name] = r
	}
	return s, nil
}

// MustNewServer is NewServer for registries fixed at compile time.
func MustNewServer(opts ServerOptions) *Server {
	s, err := NewServer(opts)
	if err != nil {
		panic(err)
	}
	return s
}

// ToolNames lists the registered tools in registration order.
func (s *Server) ToolNames() []string {
	names := make([]string, len(s.tools))
	for i, t := range s.tools {
		names[i] = t.Name
	}
	return names
}

func isObjectSchema(raw json.RawMessage) bool {
	v, err := jsjson.Parse(raw)
	if err != nil {
		return false
	}
	o, ok := v.(*jsjson.Object)
	if !ok {
		return false
	}
	typ, _ := o.Get("type")
	return typ == "object"
}

// toolDefinition is the tools/list entry. Key order is the SDK's: name,
// title, description, inputSchema, annotations, execution, outputSchema.
func toolDefinition(t Tool) ([]byte, error) {
	b := []byte(`{"name":`)
	b = jsjson.AppendString(b, t.Name)
	if t.Title != "" {
		b = append(b, `,"title":`...)
		b = jsjson.AppendString(b, t.Title)
	}
	if t.Description != "" {
		b = append(b, `,"description":`...)
		b = jsjson.AppendString(b, t.Description)
	}
	var err error
	b = append(b, `,"inputSchema":`...)
	if b, err = jsjson.AppendValue(b, t.InputSchema); err != nil {
		return nil, fmt.Errorf("inputSchema: %w", err)
	}
	if len(t.Annotations) > 0 {
		b = append(b, `,"annotations":`...)
		if b, err = jsjson.AppendValue(b, t.Annotations); err != nil {
			return nil, fmt.Errorf("annotations: %w", err)
		}
	}
	// The SDK's tool definitions carry this for every registerTool() tool.
	b = append(b, `,"execution":{"taskSupport":"forbidden"}`...)
	if t.OutputSchema != nil {
		b = append(b, `,"outputSchema":`...)
		if b, err = jsjson.AppendValue(b, t.OutputSchema); err != nil {
			return nil, fmt.Errorf("outputSchema: %w", err)
		}
	}
	return append(b, '}'), nil
}

// HandleRequest answers one JSON-RPC request (a message that passed
// IsJSONRPCRequest; the HTTP handler checks that) with the serialized
// JSON-RPC response. It never fails: every failure is a JSON-RPC error or an
// isError tool result, as with the SDK.
func (s *Server) HandleRequest(ctx context.Context, request *jsjson.Object, authInfo any) []byte {
	id, _ := request.Get("id")
	methodValue, _ := request.Get("method")
	method, _ := methodValue.(string)
	params, _ := request.Get("params")

	known := method == "initialize" || method == "ping" ||
		(len(s.tools) > 0 && (method == "tools/list" || method == "tools/call"))
	if !known {
		return errorResponse(id, MethodNotFound, "Method not found")
	}
	// Task-augmented requests ("params.task") need a tasks capability that
	// Pages does not offer; refused before the handler, as the SDK did.
	if po, ok := asObject(params); ok && IsTaskAugmentedParams(params) && po.Has("task") {
		return errorResponse(id, InternalError, fmt.Sprintf("Server does not support task creation (required for %s)", method))
	}
	if !methodSchema(method)(request) {
		// The SDK answered with zod's issue dump here. Unreachable through
		// the HTTP handler, which refuses the same envelope earlier with
		// "Invalid params for <method>".
		return errorResponse(id, InternalError, "Invalid params for "+method)
	}
	var result []byte
	switch method {
	case "initialize":
		result = s.initialize(params.(*jsjson.Object))
	case "ping":
		result = []byte("{}")
	case "tools/list":
		result = s.listTools()
	case "tools/call":
		var err error
		result, err = s.callTool(ctx, params.(*jsjson.Object), Extra{RequestID: jsjson.ToString(id), AuthInfo: authInfo}).appendJSON(nil)
		if err != nil {
			s.logger.Error("mcp tool result is not serializable", "err", err)
			return errorResponse(id, InternalError, "Internal error")
		}
	}
	b := append([]byte(`{"result":`), result...)
	b = append(b, `,"jsonrpc":"2.0","id":`...)
	b = appendID(b, id)
	return append(b, '}')
}

func (s *Server) initialize(params *jsjson.Object) []byte {
	requested, _ := params.Get("protocolVersion")
	version := LatestProtocolVersion
	if v, ok := requested.(string); ok && IsSupportedProtocolVersion(v) {
		version = v
	}
	// Pages' registry is immutable for a process lifetime, so tools never
	// change under a client: listChanged is false. The capability is declared
	// even when a token can see no tools, as the SDK server did.
	b := []byte(`{"protocolVersion":`)
	b = jsjson.AppendString(b, version)
	b = append(b, `,"capabilities":{"tools":{"listChanged":false}},"serverInfo":{"name":`...)
	b = jsjson.AppendString(b, s.info.Name)
	if s.info.Title != "" {
		b = append(b, `,"title":`...)
		b = jsjson.AppendString(b, s.info.Title)
	}
	b = append(b, `,"version":`...)
	b = jsjson.AppendString(b, s.info.Version)
	b = append(b, '}')
	if s.instructions != "" {
		b = append(b, `,"instructions":`...)
		b = jsjson.AppendString(b, s.instructions)
	}
	return append(b, '}')
}

func (s *Server) listTools() []byte {
	b := []byte(`{"tools":[`)
	for i, t := range s.tools {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, t.definition...)
	}
	return append(b, "]}"...)
}

func (s *Server) callTool(ctx context.Context, params *jsjson.Object, extra Extra) (result *CallToolResult) {
	nameValue, _ := params.Get("name")
	name, _ := nameValue.(string)
	tool := s.byName[name]
	// Tool-level failures stay on the result channel so the model sees them.
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("mcp tool panicked", "tool", name, "request", extra.RequestID, "panic", r)
			result = toolErrorResult("internal error")
		}
	}()
	if tool == nil {
		return toolErrorResult((&Error{InvalidParams, fmt.Sprintf("Tool %s not found", name)}).Error())
	}
	args := []byte("{}")
	if a, ok := params.Get("arguments"); ok && a != nil {
		args = jsjson.MustMarshal(a)
	}
	if tool.ParseInput != nil {
		parsed, err := tool.ParseInput(args)
		if err != nil {
			return toolErrorResult(validationFailure(err, fmt.Sprintf("Input validation error: Invalid arguments for tool %s: ", name)))
		}
		args = parsed
	}
	res, err := tool.Handler(ctx, args, extra)
	if err != nil {
		return toolErrorResult(err.Error())
	}
	if res == nil {
		res = &CallToolResult{Content: []Content{}}
	}
	if tool.OutputSchema != nil && !res.IsError {
		if !res.hasStructured() {
			return toolErrorResult((&Error{InvalidParams, fmt.Sprintf("Output validation error: Tool %s has an output schema but no structured content was provided", name)}).Error())
		}
		if tool.ValidateOutput != nil {
			if err := tool.ValidateOutput(res.StructuredContent); err != nil {
				return toolErrorResult(validationFailure(err, fmt.Sprintf("Output validation error: Invalid structured content for tool %s: ", name)))
			}
		}
	}
	return res
}

// validationFailure is the McpError text for a parse failure; any other
// error is reported by its own text, as a throwing safeParse would be.
func validationFailure(err error, prefix string) string {
	var v *ValidationError
	if errors.As(err, &v) {
		return (&Error{InvalidParams, prefix + v.Error()}).Error()
	}
	return err.Error()
}

func appendID(b []byte, id any) []byte {
	out, err := jsjson.AppendValue(b, id)
	if err != nil {
		return append(b, "null"...)
	}
	return out
}

// errorResponse is {"jsonrpc":"2.0","id":…,"error":{"code":…,"message":…}}.
func errorResponse(id any, code int, message string) []byte {
	b := []byte(`{"jsonrpc":"2.0","id":`)
	b = appendID(b, id)
	b = fmt.Appendf(b, `,"error":{"code":%d,"message":`, code)
	b = jsjson.AppendString(b, message)
	return append(b, "}}"...)
}
