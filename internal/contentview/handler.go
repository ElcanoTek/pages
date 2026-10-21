// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/auth/wire"
	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/render"
)

// DefaultMaxRequestBytes is MAX_HTML_BYTES' default ("2mb"), the deck save
// channel's body cap when Deps.MaxRequestBytes is 0.
const DefaultMaxRequestBytes = 2 * 1024 * 1024

// passwordFormLimit is the password form's body cap (express.urlencoded
// limit "1kb"): the only place the content host parses a form.
const passwordFormLimit = 1024

// DeckSave is one save from a Bento deck edit session, for Deps.SaveDeck. The
// actor is always a staff user ("user" actor type): the edit token names them.
type DeckSave struct {
	Slug   string
	PageID int64
	// BaseVersion is the X-Pages-Base-Version header, verbatim ("" when
	// absent); validating it is the saver's job.
	BaseVersion string
	// HTML is the deck with its collaboration keys already stripped.
	HTML  string
	Actor string
	IP    string
}

// DeckSaveResult is what a save produced.
type DeckSaveResult struct {
	VersionID int64
	Status    string
	Deduped   bool
}

// Deps are the content host's collaborators. Only DB is required.
type Deps struct {
	DB *db.DB
	// Store overrides the read path (tests force failures through it); nil
	// means NewStore(DB).
	Store Store
	// ContentLimit is the per-IP read limiter (internal/ratelimit content);
	// PasswordLimit the password form's brute-force guard. nil = none.
	ContentLimit  func(http.Handler) http.Handler
	PasswordLimit func(http.Handler) http.Handler
	// Ready answers /readyz; nil leaves the route out.
	Ready http.Handler
	// TemplatePreview returns the MATERIALIZED bytes of one template revision
	// (templates.previewBytes); an *apierror.Error with Status 404 means no
	// such revision. nil makes /raw-template answer 500.
	TemplatePreview func(ctx context.Context, templateVersionID int64) (string, error)
	// SaveDeck stores a deck save as a draft (versions.saveDeck). Errors that
	// are an *apierror.Error with a 4xx status reach the client verbatim. nil
	// makes the save channel answer 500.
	SaveDeck func(ctx context.Context, in DeckSave) (DeckSaveResult, error)
	// ClientIP attributes a request (trust-proxy aware); nil = RemoteAddr.
	ClientIP func(*http.Request) string
	// MaxRequestBytes caps a deck save body; 0 = DefaultMaxRequestBytes.
	MaxRequestBytes int64
	// Public is the static tree (/assets is its assets/); nil = pages.Public().
	Public fs.FS
}

type handler struct {
	Deps
	db         db.Querier
	store      Store
	assets     fs.FS
	contentLim func(http.Handler) http.Handler
	passLim    func(http.Handler) http.Handler
	// sleepHook replaces the backoff sleep in tests, which assert the delay
	// chosen rather than wait it out.
	sleepHook func(time.Duration)
}

func identity(h http.Handler) http.Handler { return h }

// ContentHandler is the whole content host: cookieless except for its own
// per-page and portal session cookies, never touching the SSO cookie, and
// rendering untrusted agent HTML only under the sandbox CSP. Every response
// carries the zone's header floor (csp.ContentBaseHeaders), including the
// shapes no route produces: a directory redirect, a body-parser error, a
// failed handler.
//
// Routes, in Express registration order: /healthz, /readyz, /assets/*,
// GET /raw/<slug>, OPTIONS|POST /raw/<slug>/versions, GET /raw-template/<id>,
// GET /favicon.ico, GET /<slug> (pages and /portal/<slug>), POST /<slug>
// (password forms and /portal/<slug>/lock), then a branded 404.
func ContentHandler(deps Deps) http.Handler {
	h := &handler{Deps: deps, contentLim: deps.ContentLimit, passLim: deps.PasswordLimit}
	if deps.DB != nil {
		h.db = deps.DB
	}
	h.store = deps.Store
	if h.store == nil {
		h.store = NewStore(h.db)
	}
	pub := deps.Public
	if pub == nil {
		pub = pages.Public()
	}
	if sub, err := fs.Sub(pub, "assets"); err == nil {
		h.assets = sub
	}
	if h.contentLim == nil {
		h.contentLim = identity
	}
	if h.passLim == nil {
		h.passLim = identity
	}
	return h
}

// statusError carries the HTTP status a failure should keep (a body-parser
// 413, an unsatisfiable Range's 416).
type statusError struct {
	status int
	msg    string
}

func (e *statusError) Error() string { return e.msg }

// trackingWriter records whether the response has started.
type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wrote = true
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

type routeFunc func(w http.ResponseWriter, r *http.Request) error

// run adapts a route to http.Handler with the content host's error handler:
// a failure answers with the branded 500 (or the 4xx it carried) under the
// sandbox headers — never a default error document, never a stack trace.
func (h *handler) run(fn routeFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw, ok := w.(*trackingWriter)
		if !ok {
			tw = &trackingWriter{ResponseWriter: w}
		}
		if err := fn(tw, r); err != nil {
			h.fail(tw, r, err)
		}
	})
}

func (h *handler) fail(w *trackingWriter, r *http.Request, err error) {
	slog.Error("content host error", "err", err)
	// Once the response has started there is nothing left to render: abort the
	// connection rather than leave the client hanging.
	if w.wrote {
		panic(http.ErrAbortHandler)
	}
	// Preserve a client-error status instead of flattening everything to 500:
	// a CDN or a resumable download is entitled to a 416 or 412, and a 500
	// invites a retry loop for a request that can never succeed.
	status := http.StatusInternalServerError
	var se *statusError
	if errors.As(err, &se) {
		status = se.status
	} else if ae, ok := apierror.As(err); ok {
		status = ae.Status
	}
	if status < 400 || status >= 500 {
		status = http.StatusInternalServerError
	}
	sendWith(w, r, csp.RawHeaders(), status, ServerErrorPage())
}

// pathIs is Express's non-strict, case-insensitive match of a literal path:
// one optional trailing slash.
func pathIs(path, literal string) bool {
	return strings.EqualFold(path, literal) || strings.EqualFold(path, literal+"/")
}

// decodeSplat is path-to-regexp's splat decoding: split on "/", decode each
// segment, join. A malformed escape is a 400.
func decodeSplat(raw string) (string, error) {
	parts := strings.Split(raw, "/")
	for i, p := range parts {
		d, err := wire.DecodeURIComponent(p)
		if err != nil {
			return "", &statusError{status: http.StatusBadRequest, msg: "failed to decode param"}
		}
		parts[i] = d
	}
	return strings.Join(parts, "/"), nil
}

var (
	rawRE     = regexp.MustCompile(`(?i)^/raw/(.+)$`)
	rawSaveRE = regexp.MustCompile(`(?i)^/raw/(.+)/versions/?$`)
	rawTmplRE = regexp.MustCompile(`(?i)^/raw-template/([^/]+)/?$`)
	bearerRE  = regexp.MustCompile(`(?i)^Bearer\s+(\S+)$`)
)

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The zone's header floor first, so EVERY response carries it. Routes that
	// render layer RawHeaders/GateHeaders on top.
	csp.ContentBaseHeaders().Apply(w.Header())
	tw := &trackingWriter{ResponseWriter: w}
	w = tw
	path := r.URL.EscapedPath()
	get := r.Method == http.MethodGet || r.Method == http.MethodHead

	switch {
	case get && pathIs(path, "/healthz"):
		wire.Send(w, r, http.StatusOK, "text/plain; charset=utf-8", []byte("ok"))
		return
	case get && h.Ready != nil && pathIs(path, "/readyz"):
		h.Ready.ServeHTTP(w, r)
		return
	}

	// Vendored Flag tokens/fonts/icons, served from the content host so /raw
	// pages can reference them CSP-clean. See serveAssets for the headers.
	if rel, ok := mountRel(path, "/assets"); ok && h.assets != nil {
		handled, err := serveStatic(w, r, h.assets, rel, setAssetHeaders)
		if err != nil {
			h.fail(tw, r, err)
			return
		}
		if handled {
			return
		}
	}

	if get {
		if m := rawRE.FindStringSubmatch(path); m != nil {
			h.contentLim(h.run(func(w http.ResponseWriter, r *http.Request) error {
				slug, err := decodeSplat(m[1])
				if err != nil {
					return err
				}
				return h.raw(w, r, slug)
			})).ServeHTTP(w, r)
			return
		}
	}
	if r.Method == http.MethodOptions || r.Method == http.MethodPost {
		if m := rawSaveRE.FindStringSubmatch(path); m != nil {
			h.contentLim(h.run(func(w http.ResponseWriter, r *http.Request) error {
				slug, err := decodeSplat(m[1])
				if err != nil {
					return err
				}
				if r.Method == http.MethodOptions {
					saveChannelCORS(w.Header())
					w.WriteHeader(http.StatusNoContent)
					return nil
				}
				return h.saveDeck(w, r, slug)
			})).ServeHTTP(w, r)
			return
		}
	}
	if get {
		if m := rawTmplRE.FindStringSubmatch(path); m != nil {
			h.contentLim(h.run(func(w http.ResponseWriter, r *http.Request) error {
				id, err := wire.DecodeURIComponent(m[1])
				if err != nil {
					return &statusError{status: http.StatusBadRequest, msg: "failed to decode param"}
				}
				return h.rawTemplate(w, r, id)
			})).ServeHTTP(w, r)
			return
		}
		// The browser asks for this unprompted on every page a partner opens.
		// Without a route it fell to the slug wildcard, where a full HTML 404
		// was rendered and thrown away — and the tab showed a blank icon.
		if pathIs(path, "/favicon.ico") {
			h.contentLim(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire.Redirect(w, r, http.StatusMovedPermanently, FlagBase+"/logos/elcano-mark-favicon.svg")
			})).ServeHTTP(w, r)
			return
		}
		// Direct-serve the live client page. Registered LAST so /healthz,
		// /assets and /raw win first.
		h.contentLim(h.run(func(w http.ResponseWriter, r *http.Request) error {
			slug, err := decodeSplat(strings.TrimPrefix(path, "/"))
			if err != nil {
				return err
			}
			return h.serve(w, r, slug)
		})).ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost {
		// The password form — the only place the content host parses a form
		// body, behind the brute-force guard.
		h.passLim(h.run(func(w http.ResponseWriter, r *http.Request) error {
			slug, err := decodeSplat(strings.TrimPrefix(path, "/"))
			if err != nil {
				return err
			}
			password, err := readPassword(r)
			if err != nil {
				return err
			}
			return h.unlock(w, r, slug, password)
		})).ServeHTTP(w, r)
		return
	}
	// Terminal 404 for anything the routes above did not claim — in practice
	// a method other than GET/POST. Same body as the routed 404, with the
	// zone's headers (not the limiter's RateLimit-*).
	notFound(w, r)
}

func (h *handler) clientIP(r *http.Request) string {
	if h.ClientIP != nil {
		return h.ClientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// raw is GET /raw/<slug> — the sandboxed render target. The signed token is
// the ONLY authorization (no cookies on this host). It binds the exact
// version and a single audience, so it cannot be replayed for another version
// or redeemed as a page credential (PLAN §6d). Headers are set on EVERY path.
func (h *handler) raw(w http.ResponseWriter, r *http.Request, slug string) error {
	csp.RawHeaders().Apply(w.Header())
	t, _ := wire.QueryParam(r, "t").String()
	var c *rawtoken.Claims
	if t != "" {
		c = rawtoken.Verify(t)
	}
	// Allow-list, not deny-list: this route renders one page version, so it
	// accepts "view" and — for a Bento deck — "edit". A "template" token names
	// a different id space, and a "session" token is a page credential, not a
	// render credential. A purpose added later is refused here by default.
	if c == nil || (c.Purpose != rawtoken.View && c.Purpose != rawtoken.Edit) {
		wire.SendHTML(w, r, http.StatusForbidden, ExpiredLinkPage())
		return nil
	}
	if c.Purpose == rawtoken.Edit {
		return h.deckEditSession(w, r, slug, t, c)
	}
	v, err := h.store.GetRenderable(r.Context(), c.VersionID)
	if err != nil {
		slog.Error("render error", "err", err)
		wire.SendHTML(w, r, http.StatusInternalServerError, ServerErrorPage())
		return nil
	}
	// The token's version must match what we loaded, belong to this slug, and
	// the page must not be disabled (takedown switch).
	if v == nil || v.ID != c.VersionID || v.Slug != slug || v.Disabled {
		wire.SendHTML(w, r, http.StatusNotFound, NotFoundPage())
		return nil
	}
	wire.SendHTML(w, r, http.StatusOK, render.RenderVersion(render.Renderable{
		HTML: v.HTML, RenderMode: v.RenderMode, OverrideCSS: v.OverrideCSS,
	}))
	return nil
}

// deckEditSession: the NEWEST version of the page (saves land as drafts, and
// an editor that reopened on the live version would show the author their own
// work undone), under the one header on this host that may talk back to
// Pages, with the save channel wired in. Only a deck: the token was minted for
// one, and if the page has since become something else the session is over.
func (h *handler) deckEditSession(w http.ResponseWriter, r *http.Request, slug, token string, c *rawtoken.Claims) error {
	v, err := h.store.GetLatestRenderable(r.Context(), c.PageID)
	if err != nil {
		slog.Error("edit session error", "err", err)
		wire.SendHTML(w, r, http.StatusInternalServerError, ServerErrorPage())
		return nil
	}
	if v == nil || v.Slug != slug || v.Disabled {
		wire.SendHTML(w, r, http.StatusNotFound, NotFoundPage())
		return nil
	}
	if !render.IsBentoDeck(v.HTML) {
		wire.SendHTML(w, r, http.StatusForbidden, ExpiredLinkPage())
		return nil
	}
	csp.RawEditHeaders().Apply(w.Header())
	origin := env.ContentOrigin()
	wire.SendHTML(w, r, http.StatusOK, render.EditSession(v.HTML, render.EditSessionOptions{
		SaveURL:       origin + "/raw/" + slug + "/versions",
		Token:         token,
		VersionID:     strconv.FormatInt(v.ID, 10),
		ContentOrigin: origin,
	}))
	return nil
}

// saveChannelCORS — a deck in an edit session is an opaque origin, so its
// request arrives with `Origin: null`, no cookies, and a CORS preflight; this
// is the one place on the content host that answers one. CORS decides only
// whether the page may READ the reply: authorisation is the edit token.
func saveChannelCORS(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "null")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Pages-Base-Version")
	h.Set("Access-Control-Max-Age", "600")
	h.Set("Vary", "Origin")
}

type saveReply struct {
	VersionID         string `json:"version_id"`
	Status            string `json:"status"`
	Deduped           bool   `json:"deduped"`
	CollabKeysRemoved bool   `json:"collab_keys_removed"`
}

type saveError struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details any    `json:"details,omitempty"`
}

// saveDeck is POST /raw/<slug>/versions — the save channel. Authorisation is
// the edit token in the Authorization header and nothing else, and a request
// without a valid one is refused before anything is written. The body must
// itself be a deck, and lands as a DRAFT attributed to the staff member the
// token names. Publishing stays a human act in the admin.
func (h *handler) saveDeck(w http.ResponseWriter, r *http.Request, slug string) error {
	limit := h.MaxRequestBytes
	if limit <= 0 {
		limit = DefaultMaxRequestBytes
	}
	body, hasBody, err := readText(r, limit)
	if err != nil {
		return err
	}
	saveChannelCORS(w.Header())
	var c *rawtoken.Claims
	if m := bearerRE.FindStringSubmatch(wire.Header(r, "Authorization")); m != nil {
		c = rawtoken.Verify(m[1])
	}
	if c == nil || c.Purpose != rawtoken.Edit || c.Actor == "" {
		wire.SendJSON(w, r, http.StatusForbidden, saveError{Error: "this edit session has expired — reopen the deck from the admin"})
		return nil
	}
	if !hasBody || !render.IsBentoDeck(body) {
		wire.SendJSON(w, r, http.StatusBadRequest, saveError{Error: "only a Bento deck can be saved through this channel"})
		return nil
	}
	cleaned, stripped := render.StripCollab(body)
	var res DeckSaveResult
	if h.SaveDeck == nil {
		err = errors.New("contentview: no deck saver configured")
	} else {
		res, err = h.SaveDeck(r.Context(), DeckSave{
			Slug: slug, PageID: c.PageID, BaseVersion: wire.Header(r, "X-Pages-Base-Version"),
			HTML: cleaned, Actor: c.Actor, IP: h.clientIP(r),
		})
	}
	if err != nil {
		status := http.StatusInternalServerError
		ae, ok := apierror.As(err)
		if ok && ae.Status > 0 {
			status = ae.Status
		}
		if status >= 500 {
			slog.Error("deck save error", "err", err)
			wire.SendJSON(w, r, status, saveError{Error: "Pages could not store the version"})
			return nil
		}
		reply := saveError{Error: ae.Message}
		if ae.Code != "" {
			reply.Code, reply.Details = ae.Code, ae.Details
		}
		wire.SendJSON(w, r, status, reply)
		return nil
	}
	status := http.StatusCreated
	if res.Deduped {
		status = http.StatusOK
	}
	wire.SendJSON(w, r, status, saveReply{
		VersionID: strconv.FormatInt(res.VersionID, 10), Status: res.Status,
		Deduped: res.Deduped, CollabKeysRemoved: stripped,
	})
	return nil
}

// jsNumber is Number(s) for the shapes a path segment can take; NaN when it
// is not a number.
func jsNumber(s string) float64 {
	s = wire.TrimJS(s)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		if n, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
			return float64(n)
		}
		return math.NaN()
	}
	low := strings.ToLower(strings.TrimLeft(s, "+-"))
	if strings.HasPrefix(low, "inf") || strings.HasPrefix(low, "nan") {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

// rawTemplate is GET /raw-template/<template_version_id> — preview a
// REGISTERED template revision under the same sandbox a page gets, because a
// template is the same untrusted HTML. Authorization is the signed token
// alone, bound to this exact revision with purpose "template".
func (h *handler) rawTemplate(w http.ResponseWriter, r *http.Request, id string) error {
	csp.RawHeaders().Apply(w.Header())
	t, _ := wire.QueryParam(r, "t").String()
	var c *rawtoken.Claims
	if t != "" {
		c = rawtoken.Verify(t)
	}
	if c == nil || c.Purpose != rawtoken.Template || jsNumber(id) != float64(c.VersionID) {
		wire.SendHTML(w, r, http.StatusForbidden, ExpiredLinkPage())
		return nil
	}
	// Not the stored bytes: the template MATERIALIZED, so a preview shows the
	// design populated, in exactly the shape a page built from it would have.
	var (
		html string
		err  = errors.New("contentview: no template previewer configured")
	)
	if h.TemplatePreview != nil {
		html, err = h.TemplatePreview(r.Context(), c.VersionID)
	}
	if err != nil {
		if ae, ok := apierror.As(err); ok && ae.Status == http.StatusNotFound {
			wire.SendHTML(w, r, http.StatusNotFound, NotFoundPage())
			return nil
		}
		slog.Error("template preview error", "err", err)
		wire.SendHTML(w, r, http.StatusInternalServerError, ServerErrorPage())
		return nil
	}
	// The token carries the mode the previewer asked for, so a themed template
	// is previewed the way create_page_from_template would render it.
	wire.SendHTML(w, r, http.StatusOK, render.RenderVersion(render.Renderable{HTML: html, RenderMode: c.Mode}))
	return nil
}
