// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcpproto

// The MCP Streamable HTTP endpoint around Server: stateless, one JSON-RPC
// message per POST, JSON responses. This is lib/mcp.js's router minus the
// tools: strict Host/Origin boundary, rate limit, bearer auth (supplied by the
// caller), media types, body parsing and the status codes, with the response
// framing each step had in Express — res.json bodies carry
// "application/json; charset=utf-8" and a weak ETag; bodies past the boundary
// keep the bare "application/json" the SDK transport used.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// Framing is how a Response is written.
type Framing int

const (
	// FramingExpress is res.json: "application/json; charset=utf-8" and a
	// weak ETag.
	FramingExpress Framing = iota
	// FramingTransport is the SDK transport's bare "application/json".
	FramingTransport
	// FramingAccepted is the empty 202 for notifications and client
	// responses: "text/plain; charset=UTF-8", no body.
	FramingAccepted
)

// Response is the HTTP answer to one POST body.
type Response struct {
	Status  int
	Framing Framing
	Body    []byte
}

// Write sends r.
func (r Response) Write(w http.ResponseWriter) {
	switch r.Framing {
	case FramingExpress:
		httpx.WriteJSON(w, r.Status, r.Body)
	case FramingTransport:
		writeBody(w, r.Status, "application/json", r.Body)
	case FramingAccepted:
		writeBody(w, r.Status, "text/plain; charset=UTF-8", nil)
	}
}

func writeBody(w http.ResponseWriter, status int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// RPCError serializes {"jsonrpc":"2.0","id":id,"error":{"code","message"}};
// id is a parsed JSON value (nil for null).
func RPCError(id any, code int, message string) []byte { return errorResponse(id, code, message) }

// WriteRPCError is lib/mcp.js sendRpcError: an Express-framed JSON-RPC error
// with a null id.
func WriteRPCError(w http.ResponseWriter, status, code int, message string) {
	httpx.WriteJSON(w, status, RPCError(nil, code, message))
}

// transportError is the SDK transport's error shape (its own key order).
func transportError(status, code int, message string) Response {
	b := fmt.Appendf(nil, `{"jsonrpc":"2.0","error":{"code":%d,"message":`, code)
	b = jsonv.AppendString(b, message)
	b = append(b, `},"id":null}`...)
	return Response{Status: status, Framing: FramingTransport, Body: b}
}

func expressRPC(status int, id any, code int, message string) Response {
	return Response{Status: status, Framing: FramingExpress, Body: RPCError(id, code, message)}
}

// PostOptions are the per-request inputs of AnswerPost.
type PostOptions struct {
	// ProtocolVersion is the MCP-Protocol-Version header; HasProtocolVersion
	// says whether it was sent at all (an empty header is sent).
	ProtocolVersion    string
	HasProtocolVersion bool
	// KnownTool reports whether name is in the static tool catalog. A
	// tools/call naming anything else is refused before NewServer runs.
	KnownTool func(name string) bool
	// NewServer builds the request's server (the token's tools).
	NewServer func() (*Server, error)
	// AuthInfo reaches tool handlers as Extra.AuthInfo.
	AuthInfo any
}

// AnswerPost answers one parsed POST body (nil when there was none) with
// the status and bytes lib/mcp.js's POST route sent, in the order it applied
// its checks.
func AnswerPost(ctx context.Context, body any, opts PostOptions) Response {
	if _, isBatch := body.([]any); isBatch {
		return expressRPC(400, nil, InvalidRequest, "Invalid Request: JSON-RPC batches are not supported")
	}
	if !IsJSONRPCMessage(body) {
		return expressRPC(400, nil, InvalidRequest, "Invalid Request")
	}
	msg := body.(*jsonv.Object)
	id, _ := msg.Get("id")
	methodValue, _ := msg.Get("method")
	hasID := IsRequestID(id)
	method := ""
	if methodValue != nil {
		method = jsonv.ToString(methodValue)
	}
	_, isRequestMethod := methodValue.(string)

	if hasID && strings.HasPrefix(method, "notifications/") {
		return expressRPC(200, id, InvalidRequest, "Invalid Request: notifications must not include an id")
	}
	if schema := methodSchema(method); hasID && isRequestMethod && schema != nil && !schema(msg) {
		return expressRPC(200, id, InvalidParams, "Invalid params for "+method)
	}
	params, _ := msg.Get("params")
	paramsObj, _ := asObject(params)
	if hasID && method == "tools/list" && paramsObj.Has("cursor") {
		return expressRPC(200, id, InvalidParams, "Invalid cursor: the static tool list has no next page")
	}
	// Unknown and prototype-named tools are protocol Invalid Params, decided
	// against the static catalog before any per-token server exists.
	if hasID && method == "tools/call" && paramsObj != nil {
		if name, ok := paramsObj.Get("name"); ok {
			if s, isStr := name.(string); isStr && (opts.KnownTool == nil || !opts.KnownTool(s)) {
				return expressRPC(200, id, InvalidParams, "Unknown tool: "+s)
			}
		}
	}
	// Streamable HTTP rules, in the order the SDK transport applied them. An
	// initialize request negotiates its version in the body, so only later
	// messages are held to the MCP-Protocol-Version header (absent is
	// accepted).
	if !IsInitializeRequest(msg) && opts.HasProtocolVersion && !IsSupportedProtocolVersion(opts.ProtocolVersion) {
		return transportError(400, -32000, fmt.Sprintf("Bad Request: Unsupported protocol version: %s (supported versions: %s)",
			opts.ProtocolVersion, strings.Join(supportedProtocolVersions, ", ")))
	}
	// Notifications and client responses need no reply and run nothing:
	// Pages registers no notification handlers, so a request-only method
	// sent without an id cannot mutate state.
	if !IsJSONRPCRequest(msg) {
		return Response{Status: 202, Framing: FramingAccepted}
	}
	if opts.NewServer == nil {
		return expressRPC(500, nil, InternalError, "Internal error")
	}
	srv, err := opts.NewServer()
	if err != nil {
		slog.Error("mcp transport error", "err", err)
		return expressRPC(500, nil, InternalError, "Internal error")
	}
	return Response{Status: 200, Framing: FramingTransport, Body: srv.HandleRequest(ctx, msg, opts.AuthInfo)}
}

// Boundary is the Host/Origin allow-list of the MCP endpoint (DNS-rebinding
// and cross-site protection).
type Boundary struct {
	hosts   map[string]bool
	origins map[string]bool
}

// NewBoundary normalizes the allowed hostnames and origins; entries that do
// not normalize are dropped.
func NewBoundary(hosts, origins []string) *Boundary {
	b := &Boundary{hosts: map[string]bool{}, origins: map[string]bool{}}
	for _, h := range hosts {
		if n := NormalizeHostname(h); n != "" {
			b.hosts[n] = true
		}
	}
	for _, o := range origins {
		if o = jsTrim(o); o == "" {
			continue
		}
		if n := NormalizeOrigin(o); n != "" {
			b.origins[n] = true
		}
	}
	return b
}

// BoundaryFromEnv is lib/mcp.js's allow-lists: the dashboard host, the
// hostname of DASHBOARD_ORIGIN and MCP_ALLOWED_HOSTS (comma-separated); the
// dashboard origin and MCP_ALLOWED_ORIGINS.
func BoundaryFromEnv() *Boundary {
	origin := env.DashboardOrigin()
	hosts := []string{env.DashboardHost(), urlHostname(origin)}
	hosts = append(hosts, strings.Split(env.String("MCP_ALLOWED_HOSTS", ""), ",")...)
	origins := append([]string{origin}, strings.Split(env.String("MCP_ALLOWED_ORIGINS", ""), ",")...)
	return NewBoundary(hosts, origins)
}

// urlHostname is new URL(value).hostname for an http(s) URL, or "".
func urlHostname(value string) string {
	scheme, rest, ok := strings.Cut(jsTrim(value), "://")
	if !ok {
		return ""
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	if i := strings.IndexAny(rest, `/?#\`); i >= 0 {
		rest = rest[:i]
	}
	hostport, ok := parseAuthority(rest, scheme)
	if !ok {
		return ""
	}
	return hostnameOf(hostport)
}

// Check applies the boundary to r, setting Cache-Control and, for an allowed
// Origin, the CORS headers. It writes the 403 itself and returns false when
// the request is refused.
func (b *Boundary) Check(w http.ResponseWriter, r *http.Request) bool {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	hostname := RequestHostname(r.Host)
	if hostname == "" || !b.hosts[hostname] {
		WriteRPCError(w, 403, -32000, "Forbidden: invalid Host header")
		return false
	}
	if values, sent := r.Header["Origin"]; sent {
		normalized := NormalizeOrigin(strings.Join(values, ", "))
		if normalized == "" || !b.origins[normalized] {
			WriteRPCError(w, 403, -32000, "Forbidden: invalid Origin header")
			return false
		}
		h.Set("Access-Control-Allow-Origin", normalized)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version, MCP-Session-Id")
		h.Set("Access-Control-Expose-Headers", "MCP-Protocol-Version, MCP-Session-Id, WWW-Authenticate")
	}
	return true
}

// Middleware is Check as middleware.
func (b *Boundary) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.Check(w, r) {
			next.ServeHTTP(w, r)
		}
	})
}

// CheckPostMediaType is lib/mcp.js validatePostMediaType for a POST: the
// body must be application/json and the client must accept both JSON and
// SSE. It writes the 415/406 and returns false on refusal.
func CheckPostMediaType(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	if lowerASCII(jsTrim(mediaType)) != "application/json" {
		WriteRPCError(w, 415, -32000, "Unsupported Media Type: Content-Type must be application/json")
		return false
	}
	accepted := map[string]bool{}
	for _, entry := range strings.Split(lowerASCII(strings.Join(r.Header.Values("Accept"), ", ")), ",") {
		parts := strings.Split(entry, ";")
		for i := range parts {
			parts[i] = jsTrim(parts[i])
		}
		typ, params := parts[0], parts[1:]
		refused := false
		for _, p := range params {
			if strings.HasPrefix(p, "q=") {
				q := jsstr.Number(latin1(p[2:]))
				refused = !(q > 0 && q <= 1) // NaN and ±Infinity fail too
				break
			}
		}
		if !refused && typ != "" {
			accepted[typ] = true
		}
	}
	if !accepted["application/json"] || !accepted["text/event-stream"] {
		WriteRPCError(w, 406, -32000, "Not Acceptable: client must accept both application/json and text/event-stream")
		return false
	}
	return true
}

// lowerASCII is toLowerCase for the code points that can matter here: no
// Latin-1 letter lower-cases to ASCII.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// MethodNotAllowed is the 405 for GET, DELETE and every other non-POST,
// non-OPTIONS method.
func MethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", "POST, OPTIONS")
	WriteRPCError(w, 405, -32000, "Method not allowed; POST JSON-RPC to /mcp")
}

// WriteBoundaryError is lib/mcp.js's error middleware: every endpoint
// failure stays in JSON-RPC form. Body-parser failures, API errors (-32001,
// with WWW-Authenticate on a 401), database unavailability, else a logged 500.
func WriteBoundaryError(w http.ResponseWriter, err error, logger *slog.Logger) {
	switch {
	case errors.Is(err, ErrBodyParse):
		WriteRPCError(w, 400, ParseError, "Parse error: invalid JSON")
		return
	case errors.Is(err, ErrBodyTooLarge):
		WriteRPCError(w, 413, -32000, "Request body too large")
		return
	}
	mapped, ok := apierror.As(err)
	if !ok {
		mapped = apierror.FromDB(err)
	}
	if mapped != nil {
		if mapped.Status == 401 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="pages"`)
		}
		WriteRPCError(w, mapped.Status, -32001, mapped.Message)
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("mcp boundary error", "err", err)
	WriteRPCError(w, 500, InternalError, "Internal error")
}

// HandlerOptions configures NewHandler. Everything tool- and token-specific
// is supplied by the caller (package mcp).
type HandlerOptions struct {
	// MountPath is where the endpoint lives; "/mcp" when empty. The path
	// itself and MountPath+"/" are the endpoint; any other path under it
	// passes the boundary, limit, auth and body steps and then NotFound, as
	// requests falling out of the Express router did.
	MountPath string
	// Boundary defaults to BoundaryFromEnv() at NewHandler time (the JS
	// computed its allow-lists at load).
	Boundary *Boundary
	// MaxBodyBytes is the JSON body limit (datalimits' MAX_REQUEST_BYTES).
	MaxBodyBytes int64
	// RateLimit wraps everything after the boundary and the OPTIONS
	// preflight (ratelimit's MCP limiter). nil applies no limit.
	RateLimit func(http.Handler) http.Handler
	// Authenticate runs after the rate limit (tokens.requireBearer). It
	// returns the request to continue with (typically carrying the agent in
	// its context) or an error, which is answered by WriteBoundaryError.
	// nil skips authentication.
	Authenticate func(r *http.Request) (*http.Request, error)
	// NewServer builds the per-request server: the token's visible tools.
	NewServer func(r *http.Request) (*Server, error)
	// KnownTool reports whether name is in the static catalog.
	KnownTool func(name string) bool
	// AuthInfo derives Extra.AuthInfo for tool handlers from the
	// authenticated request.
	AuthInfo func(r *http.Request) any
	// NotFound answers paths under MountPath other than the endpoint;
	// http.NotFound when nil.
	NotFound http.Handler
	Logger   *slog.Logger
}

// NewHandler is the /mcp endpoint.
func NewHandler(opts HandlerOptions) http.Handler {
	if opts.MountPath == "" {
		opts.MountPath = "/mcp"
	}
	if opts.Boundary == nil {
		opts.Boundary = BoundaryFromEnv()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.NotFound == nil {
		opts.NotFound = http.NotFoundHandler()
	}
	inner := http.Handler(http.HandlerFunc(opts.serveLimited))
	if opts.RateLimit != nil {
		inner = opts.RateLimit(inner)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !opts.Boundary.Check(w, r) {
			return
		}
		if r.Method == http.MethodOptions && opts.isEndpoint(r) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// isEndpoint matches the router's "/" route: the mount path with or without
// a trailing slash, case-insensitively (Express routing is).
func (o *HandlerOptions) isEndpoint(r *http.Request) bool {
	p := strings.ToLower(r.URL.Path)
	m := strings.ToLower(strings.TrimSuffix(o.MountPath, "/"))
	return p == m || p == m+"/"
}

func (o *HandlerOptions) serveLimited(w http.ResponseWriter, r *http.Request) {
	if o.Authenticate != nil {
		authed, err := o.Authenticate(r)
		if err != nil {
			WriteBoundaryError(w, err, o.Logger)
			return
		}
		r = authed
	}
	if r.Method == http.MethodPost && !CheckPostMediaType(w, r) {
		return
	}
	body, err := ReadJSONBody(r, o.MaxBodyBytes)
	if err != nil {
		WriteBoundaryError(w, err, o.Logger)
		return
	}
	if !o.isEndpoint(r) {
		o.NotFound.ServeHTTP(w, r)
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, r)
		return
	}
	version, hasVersion := r.Header["Mcp-Protocol-Version"]
	opts := PostOptions{
		ProtocolVersion:    strings.Join(version, ", "),
		HasProtocolVersion: hasVersion,
		KnownTool:          o.KnownTool,
	}
	if o.NewServer != nil {
		opts.NewServer = func() (*Server, error) { return o.NewServer(r) }
	}
	if o.AuthInfo != nil {
		opts.AuthInfo = o.AuthInfo(r)
	}
	AnswerPost(r.Context(), body, opts).Write(w)
}
