// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package mcp is Pages' MCP tool server (lib/mcp-tools.js and the
// tool-specific half of lib/mcp.js): the typed tool catalog, its handlers
// over the versions/workspaces/templates/uploads state machines, per-tool
// authorization, and the /mcp HTTP endpoint built on package mcpproto.
//
// The catalog's schemas are written in a small zod-like language (schema.go)
// that emits the exact JSON Schema the JavaScript advertised and validates
// arguments with zod's messages, so tools/list and "Input validation error"
// texts stay byte-compatible with the recorded SDK behaviour.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/mcpproto"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/version"
	"github.com/ElcanoTek/pages/internal/versions"
)

// ServerName and ServerTitle are initialize's serverInfo (the version is
// version.String()).
const (
	ServerName  = "pages"
	ServerTitle = "Elcano Pages"
)

// ServerInfo is initialize's serverInfo.
func ServerInfo() mcpproto.Implementation {
	return mcpproto.Implementation{Name: ServerName, Title: ServerTitle, Version: version.String()}
}

// authInfo is what reaches tool handlers as Extra.AuthInfo: the verified
// token identity (never the bearer secret) and the client address.
type authInfo struct {
	agent *tokens.Agent
	ip    string
}

// Subtrees Pages echoes back VERBATIM because the caller owns them: the
// managed data payload, the configs and schemas around it, and the profile
// computed from that payload. The id canonicalization below is a key-NAME
// heuristic, and a name heuristic has no business inside somebody else's
// document — a client's `campaign_id: 12345` once came back as "12345" from
// every read and write, so the documented read-modify-write loop handed a
// string to a schema that says {"type":"integer"} and was refused on data
// Pages had just served.
var callerOwnedKeys = map[string]bool{
	"data":             true, // the managed payload
	"config":           true, // a template-built page's deploy-time config
	"reference_config": true, // the config a template ships as a starting point
	"schema":           true, // an embedded JSON Schema
	"config_schema":    true,
	"data_profile":     true, // computed FROM the payload; its scalar keys are payload paths
}

// canonicalizeIDs renders positive safe-integer `id` / `*_id` values as the
// decimal strings node-postgres produced for BIGINT rows, outside the
// caller-owned subtrees. It walks plain JSON only.
func canonicalizeIDs(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonicalizeIDs(e)
		}
		return out
	case *jsonv.Object:
		out := jsonv.NewObject()
		x.Range(func(k string, item any) bool {
			switch {
			case callerOwnedKeys[k]:
				out.Set(k, item)
			case (k == "id" || strings.HasSuffix(k, "_id")) && jsonv.IsSafeInteger(item) && item.(float64) > 0:
				out.Set(k, jsonv.FormatNumber(item.(float64)))
			default:
				out.Set(k, canonicalizeIDs(item))
			}
			return true
		})
		return out
	}
	return v
}

// JSONValue is lib/mcp.js jsonValue: the structured form of a tool result
// (JSON round trip, then id canonicalization).
func JSONValue(v any) (any, error) {
	parsed, err := toValue(v)
	if err != nil {
		return nil, err
	}
	return canonicalizeIDs(parsed), nil
}

func toolResult(data any, render func(any) string) (*mcpproto.CallToolResult, error) {
	structured, err := JSONValue(data)
	if err != nil {
		return nil, err
	}
	serialized := jsonv.Stringify(structured)
	text := serialized
	if render != nil {
		text = render(data)
	}
	return &mcpproto.CallToolResult{
		Content:           []mcpproto.Content{mcpproto.TextContent(text)},
		StructuredContent: json.RawMessage(serialized),
	}, nil
}

func errorResult(body *jsonv.Object) *mcpproto.CallToolResult {
	text := jsonv.Stringify(body)
	return &mcpproto.CallToolResult{
		Content:           []mcpproto.Content{mcpproto.TextContent(text)},
		StructuredContent: json.RawMessage(text),
		IsError:           true,
	}
}

// toolError is the isError result for a failed handler: a Pages API error
// (or a database unavailability) keeps its message, code and details;
// anything else is logged and reported as an internal error.
func toolError(err error, name, requestID string, logger *slog.Logger) *mcpproto.CallToolResult {
	mapped, ok := apierror.As(err)
	if !ok {
		mapped = apierror.FromDB(err)
	}
	if mapped != nil {
		code := mapped.Code
		if code == "" {
			code = "request_failed"
		}
		body := jsonv.ObjectOf("ok", false, "error", mapped.Message, "code", code)
		if mapped.Details != nil {
			details, derr := toValue(mapped.Details)
			if derr == nil {
				body.Set("details", details)
			}
		}
		return errorResult(body)
	}
	logger.Error("mcp tool error", "tool", name, "request", requestID, "err", err)
	return errorResult(jsonv.ObjectOf("ok", false, "error", "internal error", "code", "internal_error"))
}

var errNoActor = errors.New("authenticated MCP actor context is missing")

func (t *toolDef) protoTool(logger *slog.Logger) mcpproto.Tool {
	def := t
	return mcpproto.Tool{
		Name:         def.name,
		Title:        def.title,
		Description:  def.description,
		InputSchema:  json.RawMessage(jsonv.Stringify(def.input.JSONSchema(ioInput))),
		OutputSchema: json.RawMessage(jsonv.Stringify(def.output.JSONSchema(ioOutput))),
		Annotations:  json.RawMessage(jsonv.Stringify(def.annotations)),
		ParseInput: func(args json.RawMessage) (json.RawMessage, error) {
			v, err := jsonv.Parse(args)
			if err != nil {
				return nil, err
			}
			parsed, err := def.input.Parse(v)
			if err != nil {
				return nil, err
			}
			return json.RawMessage(jsonv.Stringify(parsed)), nil
		},
		ValidateOutput: func(structured json.RawMessage) error {
			v, err := jsonv.ParseLossless(structured)
			if err != nil {
				return err
			}
			_, err = def.output.Parse(v)
			return err
		},
		Handler: func(ctx context.Context, raw json.RawMessage, extra mcpproto.Extra) (*mcpproto.CallToolResult, error) {
			ai, _ := extra.AuthInfo.(*authInfo)
			if ai == nil || ai.agent == nil || ai.agent.ActorType != "agent" {
				return toolError(errNoActor, def.name, extra.RequestID, logger), nil
			}
			c := &call{
				d:         handlerDB(ctx),
				agent:     ai.agent,
				actor:     versions.Actor{Actor: ai.agent.Actor, ActorType: "agent", TokenID: ai.agent.TokenID, IP: ai.ip},
				requestID: extra.RequestID,
				logger:    logger,
			}
			parsed, err := jsonv.Parse(raw)
			if err != nil {
				return toolError(err, def.name, extra.RequestID, logger), nil
			}
			args, _ := parsed.(*jsonv.Object)
			if args == nil {
				args = jsonv.NewObject()
			}
			authz := map[string]any{}
			if s, ok := args.Get("slug"); ok {
				authz["slug"] = s
			}
			if err := tokens.AuthorizeMcpTool(ai.agent, def.name, authz); err != nil {
				return toolError(err, def.name, extra.RequestID, logger), nil
			}
			out, err := def.handler(ctx, c, args)
			if err != nil {
				return toolError(err, def.name, extra.RequestID, logger), nil
			}
			res, err := toolResult(out, def.renderText)
			if err != nil {
				return toolError(err, def.name, extra.RequestID, logger), nil
			}
			return res, nil
		},
	}
}

// ── per-request servers ─────────────────────────────────────────────────────

type dbKey struct{}

// handlerDB is the database the request's handler was built with.
func handlerDB(ctx context.Context) *db.DB {
	d, _ := ctx.Value(dbKey{}).(*db.DB)
	return d
}

type serverKey struct {
	cat   *catalog
	tools string
}

var (
	serversMu sync.Mutex
	servers   = map[serverKey]*mcpproto.Server{}
)

// visibleTools is the catalog a token may see: every tool for a broad
// token, the managed-data tools for a data_update token, none otherwise.
func visibleTools(cat *catalog, agent *tokens.Agent) []*toolDef {
	var out []*toolDef
	for _, t := range cat.tools {
		if agent == nil || tokens.IsMcpToolAllowed(agent, t.name) {
			out = append(out, t)
		}
	}
	return out
}

// NewServer builds (or reuses) the MCP server for agent: the tools its token
// may see and the matching initialize instructions. A nil agent sees the
// whole catalog.
func NewServer(agent *tokens.Agent, logger *slog.Logger) (*mcpproto.Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cat := currentCatalog()
	tools := visibleTools(cat, agent)
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.name
	}
	instructions := cat.instructions
	if agent != nil && agent.Scope == tokens.ScopeDataUpdate {
		instructions = cat.dataUpdateInstructions
	}
	key := serverKey{cat: cat, tools: instructions + "\x00" + strings.Join(names, ",")}
	serversMu.Lock()
	defer serversMu.Unlock()
	if s, ok := servers[key]; ok {
		return s, nil
	}
	proto := make([]mcpproto.Tool, len(tools))
	for i, t := range tools {
		proto[i] = t.protoTool(logger)
	}
	s, err := mcpproto.NewServer(mcpproto.ServerOptions{Info: ServerInfo(), Instructions: instructions, Tools: proto, Logger: logger})
	if err != nil {
		return nil, err
	}
	// Servers are cached per catalog; drop those of a superseded catalog.
	for k := range servers {
		if k.cat != cat {
			delete(servers, k)
		}
	}
	servers[key] = s
	return s, nil
}

// KnownTool reports whether name is in the static catalog.
func KnownTool(name string) bool {
	_, ok := currentCatalog().byName[name]
	return ok
}

// ── HTTP ────────────────────────────────────────────────────────────────────

// Deps is what the /mcp endpoint needs from the server.
type Deps struct {
	DB *db.DB
	// RateLimit is the MCP limiter's middleware (ratelimit.NewMCP); nil
	// applies none.
	RateLimit func(http.Handler) http.Handler
	// ClientIP is Express req.ip (clientip.Policy.ClientIP); it is recorded
	// on audit rows.
	ClientIP func(*http.Request) string
	// MaxBodyBytes is the JSON body limit; datalimits.MaxRequestBytes()
	// when zero.
	MaxBodyBytes int64
	// Boundary is the Host/Origin allow-list; mcpproto.BoundaryFromEnv()
	// when nil.
	Boundary *mcpproto.Boundary
	// NotFound answers paths under /mcp/ other than the endpoint.
	NotFound http.Handler
	Logger   *slog.Logger
}

type discardWriter struct{ h http.Header }

func (w *discardWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}
func (w *discardWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *discardWriter) WriteHeader(int)             {}

// Handler is the MCP Streamable HTTP endpoint. Mount it at both "/mcp" and
// "/mcp/" (it matches the path itself, case-insensitively, with or without
// the trailing slash).
func Handler(deps Deps) http.Handler {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBody := deps.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = int64(datalimits.MaxRequestBytes())
	}
	authenticate := func(r *http.Request) (*http.Request, error) {
		var out *http.Request
		var failure error
		mw := tokens.RequireBearer(deps.DB, func(_ http.ResponseWriter, _ *http.Request, err error) { failure = err })
		mw(http.HandlerFunc(func(_ http.ResponseWriter, r2 *http.Request) { out = r2 })).ServeHTTP(&discardWriter{}, r)
		if failure != nil {
			return nil, failure
		}
		if out == nil {
			return nil, apierror.Unauthorized("missing bearer token", "no_token", nil)
		}
		return out.WithContext(context.WithValue(out.Context(), dbKey{}, deps.DB)), nil
	}
	return mcpproto.NewHandler(mcpproto.HandlerOptions{
		MountPath:    "/mcp",
		Boundary:     deps.Boundary,
		MaxBodyBytes: maxBody,
		RateLimit:    deps.RateLimit,
		Authenticate: authenticate,
		NewServer: func(r *http.Request) (*mcpproto.Server, error) {
			agent, _ := tokens.AgentFrom(r.Context())
			if agent == nil {
				return nil, errNoActor
			}
			return NewServer(agent, logger)
		},
		KnownTool: KnownTool,
		AuthInfo: func(r *http.Request) any {
			agent, _ := tokens.AgentFrom(r.Context())
			ip := ""
			if deps.ClientIP != nil {
				ip = deps.ClientIP(r)
			}
			return &authInfo{agent: agent, ip: ip}
		},
		NotFound: deps.NotFound,
		Logger:   logger,
	})
}

// sortedNames is for tests and diagnostics.
func sortedNames(tools []*toolDef) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.name
	}
	slices.Sort(names)
	return names
}
