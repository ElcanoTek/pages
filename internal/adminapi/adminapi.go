// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package adminapi is the admin-only JSON surface (PLAN §10), lib/adminapi.js,
// authenticated by the Elcano SSO cookie + CSRF (internal/csrf), NOT bearer
// tokens. These are the human actions deliberately kept off the agent API:
// approve/reject a pending version, publish/rollback from the shell, toggle
// the approval gate, take a page down, set the theme, and organize pages into
// workspaces. Version actions route through internal/versions; workspace
// actions are isolated from serving state in internal/workspaces. Every
// mutation is audit-logged.
//
// Partner portals (internal/portals) live here and ONLY here: which
// dashboards one partner credential opens is a human decision, so no bearer
// or MCP route reaches them.
//
// Mounted at /api/v1/admin (before the bearer router) on the dashboard host.
package adminapi

import (
	"errors"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/auth"
	"github.com/ElcanoTek/pages/internal/contentview"
	"github.com/ElcanoTek/pages/internal/csrf"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/portals"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/render"
	"github.com/ElcanoTek/pages/internal/reqbody"
	"github.com/ElcanoTek/pages/internal/route"
	"github.com/ElcanoTek/pages/internal/shell"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/versions"
	"github.com/ElcanoTek/pages/internal/workspaces"
)

// Prefix is where the server mounts Handler.
const Prefix = "/api/v1/admin"

// Deps are what the admin API needs from the server.
type Deps struct {
	// DB is the pool every route reads and mutates through.
	DB *db.DB
	// Auth resolves the Elcano admin session (csrf.RequireAdminJSON).
	Auth *auth.Authenticator
	// ClientIP is Express's req.ip (clientip.Policy.ClientIP) for the audit
	// trail; nil uses the connection's remote address.
	ClientIP func(*http.Request) string
	// Body are the dashboard's body-parser limits, used only when the body
	// was not already parsed upstream (reqbody.Middleware); the zero value
	// means reqbody.DashboardOptions().
	Body reqbody.Options
	// Next answers every request this router does not: paths outside
	// /api/v1/admin, and admin paths no route matched (Express fell through
	// to the bearer REST router mounted at /api/v1). nil writes
	// shell.NotFound().
	Next http.Handler
	// Logger records unexpected failures; nil is slog.Default().
	Logger *slog.Logger
}

type handlerFunc = func(h *handler, w http.ResponseWriter, r *http.Request, p route.Params) error

type adminRoute = route.Route[handlerFunc]

type handler struct {
	Deps
	// reads are declared before the CSRF gate, mutations after it — the
	// order lib/adminapi.js registered them around router.use(requireCsrf).
	reads, mutations []adminRoute
	adminGate        func(http.Handler) http.Handler
}

// Handler is the /api/v1/admin router, expecting FULL request paths (it
// strips /api/v1/admin itself, case-insensitively, as Express's mount did).
//
// Its layers run in lib/adminapi.js's order: body parsing (skipped when the
// server already ran reqbody.Middleware) → admin session (401 no_session /
// 403 not_admin, JSON) for EVERY path under the mount → the GET routes →
// the CSRF gate (Origin + X-CSRF-Token; 403 bad_origin / bad_csrf) → the
// mutation routes → OPTIONS Allow → Next. So, exactly as in Express, an
// unmatched admin path still needs an admin session and (past the reads) a
// valid CSRF pair before it falls through to the bearer API.
func Handler(d Deps) http.Handler {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Next == nil {
		d.Next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.SendHTML(w, r, http.StatusNotFound, shell.NotFound())
		})
	}
	if d.Body == (reqbody.Options{}) {
		d.Body = reqbody.DashboardOptions()
	}
	h := &handler{Deps: d}
	h.reads, h.mutations = buildRoutes()
	h.adminGate = csrf.RequireAdminJSON(d.Auth)
	parse := reqbody.Middleware(d.Body, func(w http.ResponseWriter, r *http.Request, err *reqbody.Error) {
		writeAPIError(w, r, err.APIError())
	})
	inner := parse(h.adminGate(http.HandlerFunc(h.dispatch)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := routerPath(r); !ok {
			d.Next.ServeHTTP(w, r)
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// mountRE is app.use("/api/v1/admin", …): case-insensitive, a whole segment.
var mountRE = regexp.MustCompile(`(?i)^/api/v1/admin(?:/|$)`)

func routerPath(r *http.Request) (string, bool) {
	p := r.URL.EscapedPath()
	if !mountRE.MatchString(p) {
		return "", false
	}
	rest := p[len(Prefix):]
	if rest == "" {
		rest = "/"
	}
	return rest, true
}

// `*slug` splats arrive decoded and re-joined into the canonical nested slug
// ("northwind/q2"); versions.NormalizeSlug re-validates. The detail read
// GET /pages/*slug is greedy, so the one-version read MUST be declared
// before it; every mutation carries a distinct suffix and is POST-only.
func buildRoutes() (reads, mutations []adminRoute) {
	r := route.New[handlerFunc]
	get, post := http.MethodGet, http.MethodPost
	reads = []adminRoute{
		r(get, "/pages", (*handler).listPages),
		r(get, "/workspaces", (*handler).listWorkspaces),
		r(get, "/portals", (*handler).listPortals),
		r(get, "/portals/:id", (*handler).getPortal),
		r(get, "/pages/*slug/versions/:id", (*handler).getVersion),
		r(get, "/pages/*slug", (*handler).getPage),
		r(get, "/templates", (*handler).listTemplates),
		r(get, "/templates/:name", (*handler).getTemplate),
	}
	mutations = []adminRoute{
		r(post, "/workspaces", (*handler).createWorkspace),
		r(post, "/workspaces/:id/rename", (*handler).renameWorkspace),
		r(post, "/workspaces/:id/delete", (*handler).deleteWorkspace),
		r(post, "/portals", (*handler).createPortal),
		r(post, "/portals/:id/rename", (*handler).renamePortal),
		r(post, "/portals/:id/password", (*handler).setPortalPassword),
		r(post, "/portals/:id/reveal-password", (*handler).revealPortalPassword),
		r(post, "/portals/:id/pages", (*handler).addPortalPage),
		r(post, "/portals/:id/pages/update", (*handler).updatePortalPage),
		r(post, "/portals/:id/pages/remove", (*handler).removePortalPage),
		r(post, "/portals/:id/home", (*handler).setPortalHome),
		r(post, "/portals/:id/delete", (*handler).deletePortal),
		r(post, "/pages/*slug/versions/:id/approve", (*handler).approve),
		r(post, "/pages/*slug/versions/:id/reject", (*handler).reject),
		r(post, "/pages/*slug/publish", (*handler).publish),
		r(post, "/pages/*slug/rollback", (*handler).rollback),
		r(post, "/pages/*slug/approval", (*handler).setApproval),
		r(post, "/pages/*slug/disable", (*handler).disable),
		r(post, "/pages/*slug/enable", (*handler).enable),
		r(post, "/pages/*slug/theme", (*handler).setTheme),
		r(post, "/pages/*slug/password", (*handler).setPassword),
		r(post, "/pages/*slug/reveal-password", (*handler).revealPassword),
		r(post, "/pages", (*handler).createPage),
		r(post, "/pages/*slug/workspace", (*handler).assignWorkspace),
		r(post, "/pages/*slug/title", (*handler).setTitle),
		r(post, "/pages/*slug/delete", (*handler).deletePage),
		r(post, "/pages/*slug/restore", (*handler).restorePage),
		r(post, "/pages/*slug/edit-token", (*handler).editToken),
		r(post, "/pages/*slug/preview-token", (*handler).previewToken),
		r(post, "/templates/validate", (*handler).validateTemplate),
		r(post, "/templates", (*handler).registerTemplate),
		r(http.MethodDelete, "/templates/:name", (*handler).deleteTemplate),
		r(post, "/templates/:name/preview-token", (*handler).templatePreviewToken),
		r(post, "/pages/*slug/deploy-source", (*handler).deploySource),
	}
	return reads, mutations
}

// serve tries routes in order. It reports whether the request was answered
// and appends the methods of path-matching routes to allow (for OPTIONS).
func (h *handler) serve(routes []adminRoute, w http.ResponseWriter, r *http.Request, allow *[]string) bool {
	path, _ := routerPath(r)
	for i := range routes {
		rt := &routes[i]
		p, ok, err := rt.Match(path)
		if !ok {
			continue
		}
		if err != nil {
			// Express answered an undecodable parameter through this
			// router's error handler as a 500; it is the client's
			// malformed URL, so it is a 400 here (as in internal/api).
			writeAPIError(w, r, apierror.New(http.StatusBadRequest, "invalid request", "bad_request", nil))
			return true
		}
		if rt.Handles(r.Method) {
			if err := rt.Handler(h, w, r, p); err != nil {
				h.writeError(w, r, err)
			}
			return true
		}
		if r.Method == http.MethodOptions {
			*allow = append(*allow, rt.Methods()...)
		}
	}
	return false
}

func (h *handler) dispatch(w http.ResponseWriter, r *http.Request) {
	var allow []string
	if h.serve(h.reads, w, r, &allow) {
		return
	}
	// router.use(requireCsrf) applies to every request that gets this far,
	// matched or not.
	csrf.RequireCsrf(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.serve(h.mutations, w, r, &allow) {
			return
		}
		if len(allow) > 0 {
			route.ServeOptions(w, allow)
			return
		}
		h.Next.ServeHTTP(w, r)
	})).ServeHTTP(w, r)
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
	h.Logger.Error("admin api error", "err", err, "method", r.Method, "path", r.URL.Path)
	httpx.SendJSONBytes(w, r, http.StatusInternalServerError, []byte(`{"error":"internal error"}`))
}

// writeAPIError writes {error, code} — only these two, never the details
// (a template's 422 contract details included). JSON.stringify dropped an
// undefined code.
func writeAPIError(w http.ResponseWriter, r *http.Request, e *apierror.Error) {
	body := jsonv.ObjectOf("error", e.Message)
	if e.Code != "" {
		body.Set("code", e.Code)
	}
	httpx.SendJSONBytes(w, r, e.Status, []byte(jsonv.Stringify(body)))
}

// sendJSON is res.status(status).json(v) with JSON.stringify's exact output.
func sendJSON(w http.ResponseWriter, r *http.Request, status int, v any) error {
	s, err := jsonv.Marshal(v)
	if err != nil {
		return err
	}
	httpx.SendJSONBytes(w, r, status, []byte(s))
	return nil
}

// spread is `{ ...v, key: value, … }`: v's keys in order, then each pair
// (an existing key keeps its position and takes the new value).
func spread(v any, kv ...any) (*jsonv.Object, error) {
	s, err := jsonv.Marshal(v)
	if err != nil {
		return nil, err
	}
	parsed, err := jsonv.ParseString(s)
	if err != nil {
		return nil, err
	}
	o, ok := parsed.(*jsonv.Object)
	if !ok {
		return nil, errors.New("adminapi: spread of a non-object")
	}
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o, nil
}

// actor is the human identity on every state-machine call.
func (h *handler) actor(r *http.Request) versions.Actor {
	email := ""
	if s := auth.User(r); s != nil {
		email = s.Email
	}
	return versions.Actor{Actor: email, ActorType: "user", IP: h.clientIP(r)}
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

func (b body) has(key string) bool { return b.obj.Has(key) }

// toID is lib/adminapi.js toId: Number(v) must be a positive integer, else
// 400 bad_id. JavaScript coerced strings, booleans and one-element arrays;
// absent (undefined) is NaN. A value past BIGINT is refused here rather than
// failing in Postgres.
func toID(v any, present bool) (int64, error) {
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
		return 0, apierror.BadRequest("id must be a positive integer", "bad_id", nil)
	}
	return int64(n), nil
}

func paramID(p route.Params) (int64, error) { return toID(p.ID, true) }

// contentURL is a signed content-host link: <CONTENT_ORIGIN><path>?t=<token>.
func contentURL(path, token string) string {
	return env.ContentOrigin() + path + "?t=" + httpx.EncodeURIComponent(token)
}

// ── reads (cookie only; no CSRF needed for GET) ──────────────────────────

type pagesIndex struct {
	Pages      []versions.PageSummary `json:"pages"`
	Workspaces []workspaces.Workspace `json:"workspaces"`
}

// listPages is the landing index: every page (newest first) with its
// published pointer, so the /admin welcome shell can list them.
func (h *handler) listPages(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	ctx := r.Context()
	var out pagesIndex
	// One repeatable-read snapshot keeps workspace entities, page
	// memberships, and names coherent during concurrent assign/delete/rename
	// transactions.
	err := h.DB.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
			return err
		}
		var err error
		if out.Pages, err = versions.ListPages(ctx, tx, versions.ListPagesOptions{IncludeVersionNumbers: true}); err != nil {
			return err
		}
		out.Workspaces, err = workspaces.List(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	// Derive counts from these exact page rows as an additional guarantee
	// that navigation totals and rendered cards agree within the response.
	counts := map[jsx.ID]int{}
	for _, p := range out.Pages {
		if p.WorkspaceID != 0 {
			counts[p.WorkspaceID]++
		}
	}
	for i := range out.Workspaces {
		n := counts[out.Workspaces[i].ID]
		out.Workspaces[i].PageCount = &n
	}
	return sendJSON(w, r, http.StatusOK, out)
}

func (h *handler) listWorkspaces(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	list, err := workspaces.List(r.Context(), h.DB)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("workspaces", list))
}

// Partner portals: the named sets of dashboards one shared client credential
// opens. Reads never return the credential — a portal password exists in
// plaintext exactly once, in the response that created or rotated it (or a
// deliberate, audited reveal). The partner-facing address is derived here
// rather than stored, for the same reason share URLs are: the origin belongs
// to the deployment, not to the row.
func portalURL(slug string) string { return env.ContentOrigin() + "/portal/" + slug }

func withURL(portal any, slug string) (*jsonv.Object, error) {
	return spread(portal, "url", portalURL(slug))
}

func (h *handler) listPortals(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	list, err := portals.List(r.Context(), h.DB)
	if err != nil {
		return err
	}
	out := make([]*jsonv.Object, 0, len(list))
	for _, p := range list {
		o, err := withURL(p, p.Slug)
		if err != nil {
			return err
		}
		out = append(out, o)
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("portals", out))
}

func (h *handler) getPortal(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	detail, err := portals.Get(r.Context(), h.DB, id)
	if err != nil {
		return err
	}
	portal, err := withURL(detail.Portal, detail.Portal.Slug)
	if err != nil {
		return err
	}
	out, err := spread(detail, "portal", portal)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, out)
}

// getVersion is one version's full row, INCLUDING its html. The detail read
// below strips `html` from every entry in `versions` on purpose — the whole
// history would be megabytes on a screen that renders it on every mutation —
// so the source editor needs this to open the version a reviewer is actually
// looking at rather than only the live one.
func (h *handler) getVersion(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	v, err := versions.GetVersion(r.Context(), h.DB, p.Slug, jsx.ID(id))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v))
}

type pageDetail struct {
	Page      versions.Page            `json:"page"`
	Deck      bool                     `json:"deck"`
	Published *versions.VersionDetail  `json:"published"`
	Versions  []versions.ListedVersion `json:"versions"`
	Pending   []versions.ListedVersion `json:"pending"`
	Themes    []versions.Theme         `json:"themes"`
	Portals   []portals.PageMembership `json:"portals"`
	CsrfOK    bool                     `json:"csrfOk"`
}

// getPage is everything the shell needs to render: page meta, full history,
// pending queue, the published version, the theme list for the picker, and
// which portals expose the page.
func (h *handler) getPage(w http.ResponseWriter, r *http.Request, p route.Params) error {
	ctx := r.Context()
	view, err := versions.GetPage(ctx, h.DB, p.Slug)
	if err != nil {
		return err
	}
	all, err := versions.ListVersions(ctx, h.DB, p.Slug, versions.ListVersionsOptions{})
	if err != nil {
		return err
	}
	themes, err := versions.ListThemes(ctx, h.DB)
	if err != nil {
		return err
	}
	// `portals` is per-PAGE membership on purpose: looked at portal-first
	// only, a page missing from one audience's portal is invisible.
	memberOf, err := portals.ListForPage(ctx, h.DB, view.Page.ID)
	if err != nil {
		return err
	}
	// Whether the page's newest version is a Bento deck decides which editor
	// the admin offers: a deck opens in Bento with a save channel back to
	// Pages; anything else gets the source textarea.
	deck := false
	if len(all) > 0 {
		newest, err := versions.GetVersion(ctx, h.DB, p.Slug, all[0].ID)
		if err != nil {
			return err
		}
		deck = render.IsBentoDeck(newest.HTML)
	}
	pending := []versions.ListedVersion{}
	for _, v := range all {
		if v.Status == "pending" {
			pending = append(pending, v)
		}
	}
	return sendJSON(w, r, http.StatusOK, pageDetail{
		Page: view.Page, Deck: deck, Published: view.Published, Versions: all,
		Pending: pending, Themes: themes, Portals: memberOf, CsrfOK: true,
	})
}

// Template library reads. GET only, so no CSRF. Neither returns the design
// bytes: the library shows what a template REQUIRES (its schemas and
// reference config), and previewing goes through a signed content-host URL.
func (h *handler) listTemplates(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	list, err := templates.List(r.Context(), h.DB)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, list)
}

func (h *handler) getTemplate(w http.ResponseWriter, r *http.Request, p route.Params) error {
	ctx := r.Context()
	var revision *int
	if q := httpx.QueryParam(r, "revision"); q.Present {
		var raw any = q.Values[0]
		if len(q.Values) > 1 {
			arr := make([]any, len(q.Values))
			for i, v := range q.Values {
				arr[i] = v
			}
			raw = arr
		}
		n, err := toID(raw, true)
		if err != nil {
			return err
		}
		rev := int(n)
		revision = &rev
	}
	detail, err := templates.Get(ctx, h.DB, p.Name, revision, false)
	if err != nil {
		return err
	}
	revs, err := templates.Revisions(ctx, h.DB, p.Name)
	if err != nil {
		return err
	}
	pages, err := templates.ListTemplatePages(ctx, h.DB, p.Name)
	if err != nil {
		return err
	}
	out, err := spread(detail, "revisions", revs.Revisions, "pages", pages.Pages)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, out)
}

// ── mutations (cookie + CSRF) ────────────────────────────────────────────

// One-level workspaces organize the landing index only. They never change
// public URLs, version history, or serving state. Deletion safely detaches
// all member pages to Ungrouped inside the same audited transaction.
func (h *handler) createWorkspace(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	ws, err := workspaces.Create(r.Context(), h.DB, bodyOf(r).arg("name"), h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusCreated, jsonv.ObjectOf("workspace", ws))
}

func (h *handler) renameWorkspace(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	ws, err := workspaces.Rename(r.Context(), h.DB, id, bodyOf(r).arg("name"), h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("workspace", ws))
}

func (h *handler) deleteWorkspace(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	ws, err := workspaces.Remove(r.Context(), h.DB, id, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("workspace", ws))
}

// ── partner portals ──────────────────────────────────────────────────────
// One shared credential over an admin-curated set of dashboards. Unlike
// workspaces, these are not organization: membership decides which client's
// numbers a partner's password opens, so internal/portals refuses any actor
// that is not a human admin and there is no agent-facing equivalent of these
// routes. The page in a membership call travels in the BODY, not the path,
// because page slugs nest ("nwm/contoso") and would be ambiguous against the
// action suffix.

// createPortal: omit `password` and Pages generates a strong one. The
// plaintext is in this response and never in the audit log; staff can show
// it again later with POST /portals/:id/reveal-password.
func (h *handler) createPortal(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	b := bodyOf(r)
	created, err := portals.Create(r.Context(), h.DB, portals.CreateInput{
		Slug: b.arg("slug"), Name: b.arg("name"), Password: b.arg("password"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	portal, err := withURL(created.Portal, created.Portal.Slug)
	if err != nil {
		return err
	}
	out, err := spread(created, "portal", portal)
	return h.replyCredential(w, r, http.StatusCreated, out, err)
}

func (h *handler) renamePortal(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	res, err := portals.Rename(r.Context(), h.DB, portals.RenameInput{ID: id, Name: bodyOf(r).arg("name")}, h.actor(r))
	return h.reply(w, r, res, err)
}

// setPortalPassword: rotation is also revocation — every live session for
// this portal is bound to the credential digest of the hash it replaces.
func (h *handler) setPortalPassword(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	res, err := portals.SetPassword(r.Context(), h.DB, portals.SetPasswordInput{ID: id, Password: bodyOf(r).arg("password")}, h.actor(r))
	return h.replyCredential(w, r, http.StatusOK, res, err)
}

// revealPortalPassword shows the current portal password so staff can
// resend it without rotating, which would sign out every partner device.
// POST + CSRF, never a GET: each reveal writes an audit row, and a
// credential must not sit in a cache.
func (h *handler) revealPortalPassword(w http.ResponseWriter, r *http.Request, p route.Params) error {
	w.Header().Set("Cache-Control", "no-store")
	id, err := paramID(p)
	if err != nil {
		return err
	}
	res, err := portals.RevealPassword(r.Context(), h.DB, id, h.actor(r))
	return h.replyCredential(w, r, http.StatusOK, res, err)
}

func (h *handler) addPortalPage(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	b := bodyOf(r)
	res, err := portals.AddPage(r.Context(), h.DB, portals.AddPageInput{
		ID: id, Slug: b.arg("slug"), Label: b.arg("label"), SortOrder: b.arg("sort_order"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusCreated, res)
}

// updatePortalPage is partial by design: an absent key is left alone, an
// explicit null label falls back to the page title.
func (h *handler) updatePortalPage(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	b := bodyOf(r)
	res, err := portals.UpdatePage(r.Context(), h.DB, portals.UpdatePageInput{
		ID: id, Slug: b.arg("slug"), PageID: b.arg("page_id"),
		Label: b.arg("label"), SortOrder: b.arg("sort_order"),
	}, h.actor(r))
	return h.reply(w, r, res, err)
}

func (h *handler) removePortalPage(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	b := bodyOf(r)
	res, err := portals.RemovePage(r.Context(), h.DB, portals.RemovePageInput{
		ID: id, Slug: b.arg("slug"), PageID: b.arg("page_id"),
	}, h.actor(r))
	return h.reply(w, r, res, err)
}

func (h *handler) setPortalHome(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	b := bodyOf(r)
	if !b.has("slug") {
		return apierror.BadRequest("slug is required (use null to clear the home page)", "portal_home_slug_required", nil)
	}
	res, err := portals.SetHome(r.Context(), h.DB, portals.SetHomeInput{ID: id, Slug: b.arg("slug")}, h.actor(r))
	return h.reply(w, r, res, err)
}

// deletePortal retires a portal: soft delete, so the row, its membership and
// its audit trail stay and the slug frees for reuse. Member pages keep their
// own passwords and versions.
func (h *handler) deletePortal(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	res, err := portals.Remove(r.Context(), h.DB, id, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("portal", res))
}

// ── page version actions ─────────────────────────────────────────────────

func (h *handler) approve(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	v, err := versions.Approve(r.Context(), h.DB, versions.ReviewInput{
		Slug: p.Slug, VersionID: jsx.ID(id), ExpectedVersion: bodyOf(r).arg("expected_version"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v))
}

func (h *handler) reject(w http.ResponseWriter, r *http.Request, p route.Params) error {
	id, err := paramID(p)
	if err != nil {
		return err
	}
	v, err := versions.Reject(r.Context(), h.DB, versions.ReviewInput{
		Slug: p.Slug, VersionID: jsx.ID(id), Note: bodyOf(r).arg("note"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v))
}

func (h *handler) publish(w http.ResponseWriter, r *http.Request, p route.Params) error {
	b := bodyOf(r)
	id, err := toID(b.raw("version_id"))
	if err != nil {
		return err
	}
	v, err := versions.Publish(r.Context(), h.DB, versions.PublishInput{
		Slug: p.Slug, VersionID: jsx.ID(id), ExpectedVersion: b.arg("expected_version"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v))
}

func (h *handler) rollback(w http.ResponseWriter, r *http.Request, p route.Params) error {
	b := bodyOf(r)
	var id int64
	// version_id != null: omitted or null rolls back to the most recent
	// approved version that isn't live.
	if raw, ok := b.raw("version_id"); ok && raw != nil {
		var err error
		if id, err = toID(raw, true); err != nil {
			return err
		}
	}
	v, err := versions.Rollback(r.Context(), h.DB, versions.RollbackInput{
		Slug: p.Slug, VersionID: jsx.ID(id), ExpectedVersion: b.arg("expected_version"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("version", v))
}

func (h *handler) setApproval(w http.ResponseWriter, r *http.Request, p route.Params) error {
	res, err := versions.SetApproval(r.Context(), h.DB, p.Slug, bodyOf(r).arg("require_approval"), h.actor(r))
	return h.reply(w, r, res, err)
}

func (h *handler) disable(w http.ResponseWriter, r *http.Request, p route.Params) error {
	res, err := versions.SetDisabled(r.Context(), h.DB, p.Slug, true, h.actor(r))
	return h.reply(w, r, res, err)
}

func (h *handler) enable(w http.ResponseWriter, r *http.Request, p route.Params) error {
	res, err := versions.SetDisabled(r.Context(), h.DB, p.Slug, false, h.actor(r))
	return h.reply(w, r, res, err)
}

// setTheme: JavaScript treated any falsy theme as the Flag default and
// interpolated anything else as a string (String(value)).
func (h *handler) setTheme(w http.ResponseWriter, r *http.Request, p route.Params) error {
	theme := ""
	if v, ok := bodyOf(r).raw("theme"); ok && jsTruthy(v) {
		theme = jsonv.ToString(v)
	}
	res, err := versions.SetTheme(r.Context(), h.DB, p.Slug, theme, h.actor(r))
	return h.reply(w, r, res, err)
}

func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

func (h *handler) setPassword(w http.ResponseWriter, r *http.Request, p route.Params) error {
	res, err := versions.SetPassword(r.Context(), h.DB, p.Slug, bodyOf(r).arg("password"), h.actor(r))
	return h.replyCredential(w, r, http.StatusOK, res, err)
}

// revealPassword shows the page's current client password
// (internal/passwordvault). Admin cookie + CSRF only, like the portal one:
// no bearer or MCP route returns a password, and every reveal is audited.
func (h *handler) revealPassword(w http.ResponseWriter, r *http.Request, p route.Params) error {
	w.Header().Set("Cache-Control", "no-store")
	res, err := versions.RevealPassword(r.Context(), h.DB, p.Slug, h.actor(r))
	return h.replyCredential(w, r, http.StatusOK, res, err)
}

// createPage creates an empty page from the shell (admins with no agent
// wired up still need the C in CRUD). Content is deployed separately (Edit
// source / an agent).
func (h *handler) createPage(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	b := bodyOf(r)
	page, err := versions.CreatePage(r.Context(), h.DB, versions.CreatePageInput{
		Slug:            b.arg("slug"),
		Title:           b.arg("title"),
		ClientID:        b.arg("client_id"),
		WorkspaceID:     b.arg("workspace_id"),
		RequireApproval: b.arg("require_approval"),
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusCreated, jsonv.ObjectOf("page", page))
}

func (h *handler) assignWorkspace(w http.ResponseWriter, r *http.Request, p route.Params) error {
	b := bodyOf(r)
	if !b.has("workspace_id") {
		return apierror.BadRequest("workspace_id is required (use null for Ungrouped)", "workspace_id_required", nil)
	}
	page, err := workspaces.AssignPage(r.Context(), h.DB, p.Slug, b.arg("workspace_id"), h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("page", page))
}

func (h *handler) setTitle(w http.ResponseWriter, r *http.Request, p route.Params) error {
	res, err := versions.SetTitle(r.Context(), h.DB, p.Slug, bodyOf(r).arg("title"), h.actor(r))
	return h.reply(w, r, res, err)
}

// deletePage / restorePage: soft-delete and restore. Admins may delete ANY
// page (including approval-gated); restore is admin-only and undeletes the
// most-recently deleted row for the slug.
func (h *handler) deletePage(w http.ResponseWriter, r *http.Request, p route.Params) error {
	res, err := versions.DeletePage(r.Context(), h.DB, p.Slug, h.actor(r))
	return h.reply(w, r, res, err)
}

func (h *handler) restorePage(w http.ResponseWriter, r *http.Request, p route.Params) error {
	res, err := versions.RestorePage(r.Context(), h.DB, p.Slug, h.actor(r))
	return h.reply(w, r, res, err)
}

// editTokenTTL is a deck editing session, not a standing credential.
const editTokenTTL = 8 * 3600

// previewTTL bounds every preview link.
const previewTTL = 300

// editToken opens a Bento deck page in its own editor with a save channel
// back to Pages. Staff only (admin cookie + CSRF); the token names the actor
// so every save's audit row names a person; bound to the page, not a
// version, because every save makes a new one. Refused unless the page's
// newest version IS a deck: the channel exists for one thing, and a view
// token is what every other page gets.
func (h *handler) editToken(w http.ResponseWriter, r *http.Request, p route.Params) error {
	ctx := r.Context()
	view, err := versions.GetPage(ctx, h.DB, p.Slug)
	if err != nil {
		return err
	}
	latest, err := contentview.GetLatestRenderable(ctx, h.DB, int64(view.Page.ID))
	if err != nil {
		return err
	}
	if latest == nil || !render.IsBentoDeck(latest.HTML) {
		return apierror.BadRequest("only a Bento deck opens in its own editor; every other page has Edit source", "not_a_bento_deck", nil)
	}
	email := ""
	if s := auth.User(r); s != nil {
		email = s.Email
	}
	token, err := rawtoken.Mint(rawtoken.MintParams{
		PageID: int64(view.Page.ID), VersionID: latest.ID, Purpose: rawtoken.Edit, RenderMode: "raw", Actor: email,
	}, editTokenTTL)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("url", contentURL("/raw/"+view.Page.Slug, token)))
}

// previewToken mints a short-TTL signed /raw view token for ANY version
// (incl. drafts/pending) so the shell can preview it in a sandboxed iframe on
// the content host. The version must belong to the page.
func (h *handler) previewToken(w http.ResponseWriter, r *http.Request, p route.Params) error {
	ctx := r.Context()
	id, err := toID(bodyOf(r).raw("version_id"))
	if err != nil {
		return err
	}
	v, err := versions.GetVersion(ctx, h.DB, p.Slug, jsx.ID(id)) // 404s if not on this page
	if err != nil {
		return err
	}
	view, err := versions.GetPage(ctx, h.DB, p.Slug)
	if err != nil {
		return err
	}
	token, err := rawtoken.Mint(rawtoken.MintParams{
		PageID: int64(view.Page.ID), VersionID: int64(v.ID), Purpose: rawtoken.View, RenderMode: v.RenderMode,
	}, previewTTL)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, jsonv.ObjectOf("url", contentURL("/raw/"+view.Page.Slug, token)))
}

// ── template library ─────────────────────────────────────────────────────
// Human-facing surface for the stored designs: browse them, read what each
// one requires, preview a revision, and upload a new one. Uploading runs the
// format checks FIRST and reports them; nothing is written until the caller
// has seen them, which is the difference between "your template was
// rejected" and "here is which block is missing".

// validateTemplate is the dry run: "is this our template format?" — writes
// nothing, ever.
func (h *handler) validateTemplate(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	b := bodyOf(r)
	return sendJSON(w, r, http.StatusOK, templates.ValidateHTML(b.arg("html"), b.arg("name")))
}

// optString is a template metadata field: absent/null ⇒ nil, anything else
// as String(value) (node-postgres stringified a non-string parameter).
func optString(v any) *string {
	if v == nil || v == versions.Null {
		return nil
	}
	s := jsonv.ToString(v)
	return &s
}

// registerTemplate registers a template. The contract is re-checked here
// rather than trusted from a prior /validate call: the two requests are
// independent, and the bytes could differ.
func (h *handler) registerTemplate(w http.ResponseWriter, r *http.Request, _ route.Params) error {
	b := bodyOf(r)
	html := b.arg("html")
	report := templates.ValidateHTML(html, b.arg("name"))
	if report.NameError != nil {
		return apierror.BadRequest(report.NameError.Message, report.NameError.Code, nil)
	}
	if report.Name == nil {
		return apierror.BadRequest("a template name is required", "template_required", nil)
	}
	if !report.ContractOK {
		e := report.ContractError
		return apierror.New(http.StatusUnprocessableEntity, e.Message, e.Code, e.Details)
	}
	// Preflight findings in a template are inherited by every page built from
	// it, so the library refuses by default and makes the override explicit
	// and audited rather than letting a human click past a warning strip.
	if report.Preflight != nil && !report.Preflight.OK && b.obj.Value("allow_preflight_errors") != true {
		return apierror.New(http.StatusUnprocessableEntity,
			"preflight found "+jsonv.FormatNumber(float64(len(report.Preflight.Errors)))+
				" error(s); every page built from this template inherits them",
			"template_preflight_failed", map[string]any{"preflight": report.Preflight})
	}
	s, _ := html.(string) // ContractOK implies a string
	res, err := templates.Register(r.Context(), h.DB, templates.RegisterInput{
		Name:        *report.Name,
		HTML:        s,
		Title:       optString(b.arg("title")),
		Description: optString(b.arg("description")),
		Note:        optString(b.arg("note")),
		Source:      "admin",
	}, h.actor(r))
	if err != nil {
		return err
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	out, err := spread(res, "validation", report)
	if err != nil {
		return err
	}
	return sendJSON(w, r, status, out)
}

// deleteTemplate retires a template. The library is where a mistyped name is
// noticed, so this is reachable from there and not only over MCP. Soft
// delete: revisions stay, pages built from it keep serving, and the name
// becomes reusable. Refused when pages exist unless the caller passes force —
// which the UI only offers after showing how many there are.
func (h *handler) deleteTemplate(w http.ResponseWriter, r *http.Request, p route.Params) error {
	force := false
	if q := httpx.QueryParam(r, "force"); q.Present && len(q.Values) == 1 && q.Values[0] == "true" {
		force = true
	}
	if bodyOf(r).obj.Value("force") == true {
		force = true
	}
	res, err := templates.Remove(r.Context(), h.DB, p.Name, force, h.actor(r))
	return h.reply(w, r, res, err)
}

type templatePreview struct {
	Template      string `json:"template"`
	Revision      int    `json:"revision"`
	RenderMode    string `json:"render_mode"`
	ContentSHA256 string `json:"content_sha256"`
	// HasSampleData lets the shell label what it is about to show: a
	// populated design, or the empty state a template ships when it carries
	// no example dataset.
	HasSampleData    bool   `json:"has_sample_data"`
	URL              string `json:"url"`
	ExpiresInSeconds int    `json:"expires_in_seconds"`
}

// templatePreviewToken is a short-TTL signed URL on the CONTENT host. Same
// trust split as a page preview: untrusted design bytes never render in the
// dashboard origin.
func (h *handler) templatePreviewToken(w http.ResponseWriter, r *http.Request, p route.Params) error {
	b := bodyOf(r)
	var revision *int
	if raw, ok := b.raw("revision"); ok {
		n, err := toID(raw, true)
		if err != nil {
			return err
		}
		rev := int(n)
		revision = &rev
	}
	renderMode := "themed"
	if b.obj.Value("render_mode") == "raw" {
		renderMode = "raw"
	}
	target, err := templates.GetRevisionHTML(r.Context(), h.DB, p.Name, revision)
	if err != nil {
		return err
	}
	token, err := rawtoken.Mint(rawtoken.MintParams{
		PageID: 0, VersionID: int64(target.TemplateVersionID), Purpose: rawtoken.Template, RenderMode: renderMode,
	}, previewTTL)
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, templatePreview{
		Template:         target.Template,
		Revision:         target.Revision,
		RenderMode:       renderMode,
		ContentSHA256:    target.ContentSHA256,
		HasSampleData:    target.HasSampleData,
		URL:              contentURL("/raw-template/"+target.TemplateVersionID.String(), token),
		ExpiresInSeconds: previewTTL,
	})
}

type deploySourceResponse struct {
	Version   versions.Version `json:"version"`
	Deduped   bool             `json:"deduped"`
	Published bool             `json:"published"`
	Gated     bool             `json:"gated"`
}

// deploySource is the source editor's save. Deploys a new version from
// edited HTML (admin cookie + CSRF, source "admin"). Routes through the same
// state machine as every other deploy: dedupe, append-only, audit log, and
// the approval gate. publish is always false — the admin previews the saved
// draft and clicks Publish separately (the "fix small things" flow). PLAN.md
// §8: edits the stored *source*, never the rendered DOM (lossless, charts
// survive).
func (h *handler) deploySource(w http.ResponseWriter, r *http.Request, p route.Params) error {
	b := bodyOf(r)
	html, _ := b.obj.Value("html").(string)
	if html == "" {
		return apierror.BadRequest("html is required", "html_required", nil)
	}
	renderMode := "themed"
	if b.obj.Value("render_mode") == "raw" {
		renderMode = "raw"
	}
	note := versions.Null
	if s, ok := b.obj.Value("note").(string); ok {
		note = jsstr.Slice(s, 0, 500)
	}
	res, err := versions.Deploy(r.Context(), h.DB, versions.DeployInput{
		Slug:       p.Slug,
		HTML:       html,
		RenderMode: renderMode,
		Note:       note,
		Source:     "admin",
		Publish:    false,
	}, h.actor(r))
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, deploySourceResponse{
		Version: res.Version, Deduped: res.Deduped, Published: res.Published, Gated: res.Gated,
	})
}

// reply is res.json(result) for a (result, err) pair.
func (h *handler) reply(w http.ResponseWriter, r *http.Request, v any, err error) error {
	if err != nil {
		return err
	}
	return sendJSON(w, r, http.StatusOK, v)
}

// replyCredential is reply for a route whose answer carries a password or
// answers the request that set one: no-store, and no ETag hashed from it
// (httpx.SendCredentialJSON).
func (h *handler) replyCredential(w http.ResponseWriter, r *http.Request, status int, v any, err error) error {
	if err != nil {
		return err
	}
	s, err := jsonv.Marshal(v)
	if err != nil {
		return err
	}
	httpx.SendCredentialJSON(w, r, status, []byte(s))
	return nil
}
