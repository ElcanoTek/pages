// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package api is the /api/v1 REST surface (PLAN.md §10), lib/api.js. Mounted
// on the DASHBOARD host only. This is the agent slice: bearer-authenticated
// page + version management that routes every state change through the
// version state machine (internal/versions) — there is no privileged backdoor
// that skips it, so REST and MCP share identical gating and audit.
//
// Auth here is bearer-only (agents). The admin-cookie+CSRF endpoints live in
// the admin API at /api/v1/admin, a separate handler the server mounts in
// front of this one (see Handler).
package api

import (
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/pageurls"
	"github.com/ElcanoTek/pages/internal/preflight"
	"github.com/ElcanoTek/pages/internal/reqbody"
	"github.com/ElcanoTek/pages/internal/route"
	"github.com/ElcanoTek/pages/internal/shell"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Deps are what the REST API needs from the server.
type Deps struct {
	// DB is the pool every route reads and mutates through.
	DB *db.DB
	// RateLimit is the per-IP API limiter (ratelimit.NewAPI(...).Middleware).
	// It runs first, before the body is parsed, so an over-quota client
	// cannot make the server parse a max-size body. nil means none — pass nil
	// when the server already applied the limiter to all of /api/v1.
	RateLimit func(http.Handler) http.Handler
	// ClientIP is Express's req.ip (clientip.Policy.ClientIP) for the audit
	// trail; nil uses the connection's remote address.
	ClientIP func(*http.Request) string
	// Body are the dashboard's body-parser limits; the zero value means
	// reqbody.DashboardOptions(). A body already parsed upstream
	// (reqbody.Middleware) is not parsed again.
	Body reqbody.Options
	// NotFound answers a path no route matched (the dashboard's branded 404);
	// nil writes shell.NotFound().
	NotFound http.Handler
	// Logger records unexpected failures; nil is slog.Default().
	Logger *slog.Logger
}

type handler struct {
	Deps
	routes []apiRoute
	inner  http.Handler
}

// Handler is the /api/v1 router, expecting FULL request paths (it strips
// /api/v1 itself, case-insensitively, as Express's mount did; a request
// outside /api/v1 gets NotFound).
//
// Its layers run in server.js's order: RateLimit → body parsing (JSON and
// urlencoded; a parser failure is the JSON {error, code} envelope at 400/413/
// 415, before authentication) → bearer token (401) → REST scope (403 for
// scoped data_update tokens, which are MCP-only) → routes → NotFound.
//
// Mounting: server.js mounted the admin JSON API at /api/v1/admin BEFORE
// this router, behind the same limiter and body parsers, and anything the
// admin router did not answer fell through to here. The server must
// therefore (1) apply the API limiter and reqbody.Middleware once for all of
// /api/v1 (and pass RateLimit nil here), (2) give /api/v1/admin[/…] to the
// admin API first, with this Handler as its fall-through, and (3) send every
// other /api/v1 path here.
func Handler(d Deps) http.Handler {
	return newHandler(d, func(h *handler, next http.Handler) http.Handler {
		// Everything under /api/v1 requires a valid agent bearer token.
		// Scoped data-update tokens are intentionally MCP-only, which keeps
		// their authorization surface to exactly
		// get_page_data/update_page_data.
		return tokens.RequireBearer(d.DB, h.writeError)(tokens.RequireRestAccess(h.writeError)(next))
	})
}

// newHandler assembles the layers around auth (a seam for routing tests).
func newHandler(d Deps, auth func(*handler, http.Handler) http.Handler) *handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.NotFound == nil {
		d.NotFound = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.SendHTML(w, r, http.StatusNotFound, shell.NotFound())
		})
	}
	if d.Body == (reqbody.Options{}) {
		d.Body = reqbody.DashboardOptions()
	}
	h := &handler{Deps: d}
	h.routes = h.buildRoutes()

	inner := auth(h, http.HandlerFunc(h.dispatch))
	inner = reqbody.Middleware(d.Body, func(w http.ResponseWriter, r *http.Request, err *reqbody.Error) {
		writeAPIError(w, r, err.APIError())
	})(inner)
	if d.RateLimit != nil {
		inner = d.RateLimit(inner)
	}
	h.inner = inner
	return h
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := routerPath(r); !ok {
		h.NotFound.ServeHTTP(w, r)
		return
	}
	h.inner.ServeHTTP(w, r)
}

// Slug routes use a multi-segment splat (`*slug`) so nested slugs
// (northwind/q2) resolve without URL-encoding; normalizeSlug re-validates
// downstream. `*slug` is greedy across segments, so the bare GET
// /pages/*slug MUST stay after the sub-resource GETs (/versions,
// /versions/:id) or it would swallow them.
func (h *handler) buildRoutes() []apiRoute {
	r := route.New[handlerFunc]
	return []apiRoute{
		r(http.MethodGet, "/pages", (*handler).listPages),
		r(http.MethodPost, "/pages", (*handler).createPage),
		r(http.MethodPost, "/pages/*slug/password", (*handler).setPassword),
		r(http.MethodPost, "/pages/*slug/title", (*handler).setTitle),
		r(http.MethodDelete, "/pages/*slug", (*handler).deletePage),
		r(http.MethodPost, "/pages/*slug/versions", (*handler).deploy),
		r(http.MethodGet, "/pages/*slug/versions", (*handler).listVersions),
		r(http.MethodGet, "/pages/*slug/preflight", (*handler).preflight),
		r(http.MethodGet, "/pages/*slug/versions/:id", (*handler).getVersion),
		r(http.MethodPost, "/pages/*slug/publish", (*handler).publish),
		r(http.MethodPost, "/pages/*slug/rollback", (*handler).rollback),
		r(http.MethodGet, "/pages/*slug", (*handler).getPage),
	}
}

func (h *handler) dispatch(w http.ResponseWriter, r *http.Request) {
	path, _ := routerPath(r)
	var allow []string
	for i := range h.routes {
		rt := &h.routes[i]
		p, ok, err := rt.Match(path)
		if !ok {
			continue
		}
		if err != nil {
			// Express answered an undecodable parameter through this
			// router's error handler as a 500; it is the client's
			// malformed URL, so it is a 400 here.
			writeAPIError(w, r, apierror.New(http.StatusBadRequest, "invalid request", "bad_request", nil))
			return
		}
		if rt.Handles(r.Method) {
			if err := rt.Handler(h, w, r, p); err != nil {
				h.writeError(w, r, err)
			}
			return
		}
		if r.Method == http.MethodOptions {
			allow = append(allow, rt.Methods()...)
		}
	}
	if len(allow) > 0 {
		route.ServeOptions(w, allow)
		return
	}
	h.NotFound.ServeHTTP(w, r)
}

// writeError is the router's error handler: an *apierror.Error → its status;
// bounded-wait DB failures → clean 503s; anything else → 500 (never leaked).
func (h *handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	if e, ok := apierror.As(err); ok {
		writeAPIError(w, r, e)
		return
	}
	if e := apierror.FromDB(err); e != nil {
		writeAPIError(w, r, e)
		return
	}
	h.Logger.Error("api error", "err", err, "method", r.Method, "path", r.URL.Path)
	httpx.SendJSONBytes(w, r, http.StatusInternalServerError, []byte(`{"error":"internal error"}`))
}

// writeAPIError writes {error, code} — REST serializes only these two; the
// structured details are an MCP affordance. JSON.stringify dropped an
// undefined code.
func writeAPIError(w http.ResponseWriter, r *http.Request, e *apierror.Error) {
	body := jsonv.ObjectOf("error", e.Message)
	if e.Code != "" {
		body.Set("code", e.Code)
	}
	httpx.SendJSONBytes(w, r, e.Status, []byte(jsonv.Stringify(body)))
}

// sendJSON is res.status(status).json(v) with JSON.stringify's exact output
// (key order, number formatting, no HTML or U+2028 escaping).
func sendJSON(w http.ResponseWriter, r *http.Request, status int, v any) error {
	s, err := jsonv.Marshal(v)
	if err != nil {
		return err
	}
	httpx.SendJSONBytes(w, r, status, []byte(s))
	return nil
}

// actor is the audit/authority identity derived from the bearer token, plus
// the request IP, threaded into every state-machine call.
func (h *handler) actor(r *http.Request) versions.Actor {
	a, _ := tokens.AgentFrom(r.Context())
	if a == nil {
		a = &tokens.Agent{}
	}
	return versions.Actor{Actor: a.Actor, ActorType: a.ActorType, TokenID: a.TokenID, IP: h.clientIP(r)}
}

func (h *handler) clientIP(r *http.Request) string {
	if h.ClientIP != nil {
		return h.ClientIP(r)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// body is `req.body || {}` read field by field: only a JSON or urlencoded
// object has fields (an array or an absent body reads as empty).
type body struct{ obj *jsonv.Object }

func bodyOf(r *http.Request) body {
	v, _ := reqbody.From(r)
	o, _ := v.(*jsonv.Object)
	return body{o}
}

// arg is a field in the versions.Arg convention (absent ⇒ nil, null ⇒ Null).
func (b body) arg(key string) any { return versions.Arg(b.obj.Get(key)) }

func (b body) raw(key string) (any, bool) { return b.obj.Get(key) }

// toID is lib/api.js toId: Number(v) must be a positive integer, else 400
// bad_version_id. JavaScript coerced booleans, strings and one-element
// arrays the same way; a value past BIGINT is refused here rather than
// failing in Postgres.
func toID(v any, present bool) (jsx.ID, error) {
	n := math.NaN()
	if present {
		switch x := v.(type) {
		case nil:
			n = 0
		case float64:
			n = x
		case string:
			n = jsstr.Number(x)
		case bool:
			n = 0
			if x {
				n = 1
			}
		case []any:
			n = jsstr.Number(jsonv.ToString(x))
		}
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) || n <= 0 || n >= 9223372036854775807 {
		return 0, apierror.BadRequest("version_id must be a positive integer", "bad_version_id", nil)
	}
	return jsx.ID(int64(n)), nil
}

// ── pages ────────────────────────────────────────────────────────────────

func (h *handler) listPages(w http.ResponseWriter, r *http.Request, _ params) error {
	pages, err := versions.ListPages(r.Context(), h.DB, versions.ListPagesOptions{})
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("pages", pages))
}

func (h *handler) createPage(w http.ResponseWriter, r *http.Request, _ params) error {
	b := bodyOf(r)
	page, err := versions.CreatePage(r.Context(), h.DB, versions.CreatePageInput{
		Slug:            b.arg("slug"),
		Title:           b.arg("title"),
		ClientID:        b.arg("client_id"),
		RequireApproval: b.arg("require_approval"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusCreated, jsonv.ObjectOf("page", page))
}

// setPassword sets the per-page client password ({password: "..."}).
// Setting/changing is agent-allowed; CLEARING (empty/null) is admin-only (the
// state machine enforces it, so a bearer clear → 403 admin_only).
func (h *handler) setPassword(w http.ResponseWriter, r *http.Request, p params) error {
	res, err := versions.SetPassword(r.Context(), h.DB, p.Slug, bodyOf(r).arg("password"), h.actor(r))
	if err != nil {
		return err
	}
	// The answer to a request that set a credential: no-store, and no ETag
	// hashed from it (httpx.SendCredentialJSON).
	s, err := jsonv.Marshal(res)
	if err != nil {
		return err
	}
	httpx.SendCredentialJSON(w, r, http.StatusOK, []byte(s))
	return nil
}

// setTitle renames a page (agent-owned authoring metadata).
func (h *handler) setTitle(w http.ResponseWriter, r *http.Request, p params) error {
	res, err := versions.SetTitle(r.Context(), h.DB, p.Slug, bodyOf(r).arg("title"), h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, res)
}

// deletePage soft-deletes a page. Agents may delete OPEN pages;
// approval-gated pages 403 (admin-only). Reversible via the admin restore
// endpoint.
func (h *handler) deletePage(w http.ResponseWriter, r *http.Request, p params) error {
	res, err := versions.DeletePage(r.Context(), h.DB, p.Slug, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, res)
}

// ── versions ─────────────────────────────────────────────────────────────

type deployResponse struct {
	Version   versions.Version `json:"version"`
	Deduped   bool             `json:"deduped"`
	Published bool             `json:"published"`
	Gated     bool             `json:"gated"`
	Live      bool             `json:"live"`
}

// deploy is the agent's create-or-update call. Every call appends a new
// version (dedupe collapses identical re-deploys: 200 instead of 201).
func (h *handler) deploy(w http.ResponseWriter, r *http.Request, p params) error {
	b := bodyOf(r)
	html, _ := b.obj.Value("html").(string)
	res, err := versions.Deploy(r.Context(), h.DB, versions.DeployInput{
		Slug:            p.Slug,
		HTML:            html,
		RenderMode:      b.arg("render_mode"),
		Note:            b.arg("note"),
		Author:          b.arg("author"),
		Source:          "api",
		Publish:         b.arg("publish"),
		ExpectedVersion: b.arg("expected_version"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if res.Deduped {
		status = http.StatusOK
	}
	return sendJSON(w, r, status, deployResponse{
		Version: res.Version, Deduped: res.Deduped, Published: res.Published, Gated: res.Gated, Live: res.Live,
	})
}

func (h *handler) listVersions(w http.ResponseWriter, r *http.Request, p params) error {
	list, err := versions.ListVersions(r.Context(), h.DB, p.Slug, versions.ListVersionsOptions{})
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("versions", list))
}

type preflightResponse struct {
	VersionID jsx.ID           `json:"version_id"`
	Preflight preflight.Report `json:"preflight"`
}

// preflight is a static preflight of a stored version against the
// CSP/sandbox the content host serves it under. Read-only; it returns
// findings rather than HTML (see internal/preflight for why this lives
// server-side). Without ?version_id it checks the published version.
func (h *handler) preflight(w http.ResponseWriter, r *http.Request, p params) error {
	ctx := r.Context()
	var versionID jsx.ID
	if q := httpx.QueryParam(r, "version_id"); q.Present {
		// JavaScript handed the raw query value to SQL (a non-numeric one
		// was a 500); it is validated like every other version id here.
		var raw any = q.Values[0]
		if len(q.Values) > 1 {
			raw = []any{}
			for _, v := range q.Values {
				raw = append(raw.([]any), v)
			}
		}
		id, err := toID(raw, true)
		if err != nil {
			return err
		}
		versionID = id
	} else {
		view, err := versions.GetPage(ctx, h.DB, p.Slug)
		if err != nil {
			return err
		}
		if view.Page.PublishedVersionID == 0 {
			return apierror.BadRequest("page "+p.Slug+" has no published version; pass version_id to preflight a draft", "no_published_version", nil)
		}
		versionID = view.Page.PublishedVersionID
	}
	v, err := versions.GetVersion(ctx, h.DB, p.Slug, versionID)
	if err != nil {
		return err
	}
	mode := v.RenderMode
	if mode == "" {
		mode = "themed"
	}
	return sendJSON(w, r, http.StatusOK, preflightResponse{
		VersionID: v.ID,
		Preflight: preflight.Analyze(v.HTML, PreflightOptions(mode)),
	})
}

// PreflightOptions are the preflight.Options the server-side check runs with
// (REST and MCP alike): the managed-block rule judged on the bytes as Pages
// will store them, and the content host's real /raw policy.
func PreflightOptions(renderMode string) preflight.Options {
	return preflight.Options{
		RenderMode:    renderMode,
		ManagedCheck:  managedCheck,
		ContentPolicy: rawContentPolicy(),
	}
}

func managedCheck(html string) error {
	ensured, err := pagedata.EnsureConfigSchema(html)
	if err != nil {
		return err
	}
	_, err = pagedata.ParseManagedHTML(ensured.HTML)
	return err
}

func rawContentPolicy() string {
	for _, hd := range csp.RawHeaders() {
		if hd.Name == "Content-Security-Policy" {
			return hd.Value
		}
	}
	return ""
}

func (h *handler) getVersion(w http.ResponseWriter, r *http.Request, p params) error {
	id, err := toID(p.ID, true)
	if err != nil {
		return err
	}
	v, err := versions.GetVersion(r.Context(), h.DB, p.Slug, id)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v))
}

// ── pointer moves (open pages: agent fast path) ──────────────────────────

func (h *handler) publish(w http.ResponseWriter, r *http.Request, p params) error {
	b := bodyOf(r)
	id, err := toID(b.raw("version_id"))
	if err != nil {
		return err
	}
	v, err := versions.Publish(r.Context(), h.DB, versions.PublishInput{
		Slug: p.Slug, VersionID: id, ExpectedVersion: b.arg("expected_version"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v, "published", true))
}

func (h *handler) rollback(w http.ResponseWriter, r *http.Request, p params) error {
	b := bodyOf(r)
	var id jsx.ID
	// version_id != null: omitted or null rolls back to the most recent
	// approved version that isn't live.
	if raw, ok := b.raw("version_id"); ok && raw != nil {
		var err error
		if id, err = toID(raw, true); err != nil {
			return err
		}
	}
	v, err := versions.Rollback(r.Context(), h.DB, versions.RollbackInput{
		Slug: p.Slug, VersionID: id, ExpectedVersion: b.arg("expected_version"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v, "published", true))
}

// getPage is the bare page read — registered LAST among the slug GETs. It
// carries the routing urls (parity with MCP).
func (h *handler) getPage(w http.ResponseWriter, r *http.Request, p params) error {
	view, err := versions.GetPage(r.Context(), h.DB, p.Slug)
	if err != nil {
		return err
	}
	s, err := jsonv.Marshal(view)
	if err != nil {
		return err
	}
	parsed, err := jsonv.ParseString(s)
	if err != nil {
		return err
	}
	out, ok := parsed.(*jsonv.Object)
	if !ok {
		return errors.New("api: page view is not an object")
	}
	out.Set("urls", pageurls.For(view.Page.Slug))
	return sendJSON(w, r, http.StatusOK, out)
}
