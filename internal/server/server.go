// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package server is Elcano Pages' HTTP front: ONE process, TWO registrable
// domains (PLAN.md §3, §7), ported from the Node release's server.js.
//
//	DASHBOARD_HOST — trusted auth zone: /view, /admin shells, /api/v1, /mcp.
//	                 Reads the SSO cookie; admins are @elcanotek.com.
//	CONTENT_HOST   — separate registrable domain, COOKIELESS: /raw, /assets
//	                 and the client pages. Renders untrusted agent HTML,
//	                 sandboxed, never touches the SSO cookie. This origin
//	                 boundary is the security model — a shared parent domain
//	                 would NOT be safe.
//
// Caddy terminates TLS for both names and proxies to this one process; the
// split is on the Host header (contentview.IsContentHost), never on
// X-Forwarded-Host: client attribution must not change the route split.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/adminapi"
	"github.com/ElcanoTek/pages/internal/api"
	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/assetver"
	"github.com/ElcanoTek/pages/internal/auth"
	"github.com/ElcanoTek/pages/internal/auth/central"
	"github.com/ElcanoTek/pages/internal/clientip"
	"github.com/ElcanoTek/pages/internal/compose"
	"github.com/ElcanoTek/pages/internal/contentview"
	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/csrf"
	"github.com/ElcanoTek/pages/internal/dataexport"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/ratelimit"
	"github.com/ElcanoTek/pages/internal/rawtoken"
	"github.com/ElcanoTek/pages/internal/reqbody"
	"github.com/ElcanoTek/pages/internal/route"
	"github.com/ElcanoTek/pages/internal/shell"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/uploads"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Config is what New needs. Only DB is required.
type Config struct {
	// DB is the process's pool.
	DB *db.DB
	// Auth is the configured sign-in model; nil builds auth.NewFromEnv(DB).
	Auth *auth.Authenticator
	// ClientIP is the PAGES_TRUST_PROXY policy (Express's req.ip); nil
	// compiles clientip.FromEnv().
	ClientIP *clientip.Policy
	// Ready answers /readyz on both hosts (readiness.Checker); nil leaves
	// the route out.
	Ready http.Handler
	// MCP serves every request under /mcp (full request path, not
	// stripped). It owns its Host/Origin validation, rate limit, bearer
	// auth and JSON parser, and runs before the dashboard's body parsers.
	// nil answers a JSON-RPC 503.
	MCP http.Handler
	// Public is the static tree (pages.Public()); nil uses the embedded one.
	Public fs.FS
	// Logger records unexpected failures; nil is slog.Default().
	Logger *slog.Logger
	// Now is the rate limiters' clock (tests); nil is time.Now.
	Now func() time.Time
}

// Server is the top-level handler: the Host split over the two zones.
type Server struct {
	cfg       Config
	log       *slog.Logger
	auth      *auth.Authenticator
	ip        func(*http.Request) string
	limiters  []*ratelimit.Limiter
	apiLim    *ratelimit.Limiter
	content   http.Handler
	dashboard http.Handler

	// Dashboard stages (see buildDashboard for the order).
	afterMounts http.Handler // central sign-in, compose, then the body parser
	parsed      http.Handler // reqbody.Middleware over routes
	routes      http.Handler
	central     *central.Handlers
	devLogin    *auth.DevLogin
	composeH    http.Handler
	apiH        http.Handler
	mcp         http.Handler
}

// New builds the server. It fails on configuration the JS server refused at
// startup (PAGES_TRUST_PROXY, central-mode settings, the shell CSP's auth
// origin).
func New(cfg Config) (*Server, error) {
	if cfg.DB == nil {
		return nil, errors.New("server: Config.DB is required")
	}
	s := &Server{cfg: cfg, log: cfg.Logger, auth: cfg.Auth, mcp: cfg.MCP}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.auth == nil {
		a, err := auth.NewFromEnv(cfg.DB)
		if err != nil {
			return nil, err
		}
		s.auth = a
	}
	s.auth.ErrorHandler = s.fail
	policy := cfg.ClientIP
	if policy == nil {
		p, err := clientip.FromEnv()
		if err != nil {
			return nil, err
		}
		policy = p
	}
	s.ip = policy.ClientIP
	if s.mcp == nil {
		s.mcp = http.HandlerFunc(mcpUnavailable)
	}

	rl := func(exceeded http.Handler) ratelimit.Config {
		return ratelimit.Config{ClientIP: s.ip, Exceeded: exceeded, Now: cfg.Now}
	}
	keep := func(l *ratelimit.Limiter) *ratelimit.Limiter { s.limiters = append(s.limiters, l); return l }

	// ── Content host ───────────────────────────────────────────────────────
	contentLim := keep(ratelimit.NewContent(rl(http.HandlerFunc(contentBusy))))
	passwordLim := keep(ratelimit.NewPassword(rl(http.HandlerFunc(passwordBusy))))
	d := cfg.DB
	s.content = contentview.ContentHandler(contentview.Deps{
		DB:            d,
		ContentLimit:  contentLim.Middleware,
		PasswordLimit: passwordLim.Middleware,
		Ready:         cfg.Ready,
		TemplatePreview: func(ctx context.Context, id int64) (string, error) {
			p, err := templates.PreviewBytes(ctx, d, jsx.ID(id), time.Time{})
			if err != nil {
				return "", err
			}
			return p.HTML, nil
		},
		SaveDeck: func(ctx context.Context, in contentview.DeckSave) (contentview.DeckSaveResult, error) {
			res, err := versions.SaveDeck(ctx, d, versions.SaveDeckInput{
				Slug: in.Slug, PageID: jsx.ID(in.PageID), BaseVersion: in.BaseVersion, HTML: in.HTML,
			}, versions.Actor{Actor: in.Actor, ActorType: "user", IP: in.IP})
			if err != nil {
				return contentview.DeckSaveResult{}, err
			}
			return contentview.DeckSaveResult{VersionID: int64(res.Version.ID), Status: res.Version.Status, Deduped: res.Deduped}, nil
		},
		ClientIP:        s.ip,
		MaxRequestBytes: int64(datalimits.MaxRequestBytes()),
		Public:          cfg.Public,
	})

	// ── Dashboard host ─────────────────────────────────────────────────────
	s.apiLim = keep(ratelimit.NewAPI(rl(nil)))
	if err := s.buildDashboard(rl, keep); err != nil {
		return nil, err
	}
	return s, nil
}

// Close stops the rate limiters' sweepers.
func (s *Server) Close() {
	for _, l := range s.limiters {
		l.Stop()
	}
}

// ServeHTTP is the top-level vhost split. Go's server sends no framework
// banner, so there is no X-Powered-By to disable.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Body readers below arm a size-scaled read deadline through this, so a
	// declared but stalled body cannot hold the connection open forever.
	r = httpx.WithBodyReadControl(w, r)
	defer httpx.BoundUnreadBody(r)
	if contentview.IsContentHost(r) {
		s.content.ServeHTTP(w, r)
		return
	}
	s.dashboard.ServeHTTP(w, r)
}

func (s *Server) buildDashboard(rl func(http.Handler) ratelimit.Config, keep func(*ratelimit.Limiter) *ratelimit.Limiter) error {
	shellHeaders, err := csp.ShellHeadersMiddleware(s.auth.Origin())
	if err != nil {
		return err
	}
	notFound := http.HandlerFunc(dashboardNotFound)

	// REST + admin JSON API. The admin router (cookie + CSRF) sees /api/v1
	// first so its /api/v1/admin/* paths win; everything else falls through
	// to the bearer agent API. The limiter and body parser ran upstream.
	rest := api.Handler(api.Deps{DB: s.cfg.DB, ClientIP: s.ip, NotFound: notFound, Logger: s.log})
	s.apiH = adminapi.Handler(adminapi.Deps{DB: s.cfg.DB, Auth: s.auth, ClientIP: s.ip, Next: rest, Logger: s.log})

	if dl, ok := s.auth.DevLoginFromEnv(); ok {
		s.devLogin = &dl
	}
	if compose.Enabled() {
		s.log.Warn(fmt.Sprintf("⚠ COMPOSE ENABLED [driver: %s]: POST /api/v1/admin/compose runs Cutlass. Dev/test only — never set PAGES_COMPOSE=1 in production.", compose.DriverLabel()))
		s.composeH = compose.Handler(csrf.RequireAdminJSON(s.auth), csrf.RequireCsrf)
	}

	s.routes = http.HandlerFunc(s.serveRoutes)
	if s.devLogin != nil {
		s.routes = s.devLogin.Intercept(s.routes)
	}
	s.routes = contentview.ShellAssets(s.routes)
	s.parsed = reqbody.MiddlewareFor(s.bodyLimits, func(w http.ResponseWriter, r *http.Request, err *reqbody.Error) {
		s.fail(w, r, err)
	})(s.routes)

	if rt := s.auth.Central(); rt != nil {
		signout := keep(ratelimit.NewSignout(rl(http.HandlerFunc(signinBusy)), func(r *http.Request) string {
			return httpx.RequestCookies(r)[rt.SessionCookie]
		}))
		h := rt.Handlers(central.RoutesOptions{
			Render:         renderChrome,
			Signin:         keep(ratelimit.NewSignin(rl(http.HandlerFunc(signinBusy)))).Middleware,
			SigninCallback: keep(ratelimit.NewSigninCallback(rl(http.HandlerFunc(signinBusy)))).Middleware,
			Signout:        signout.Middleware,
			Backchannel:    keep(ratelimit.NewBackchannel(rl(nil))).Middleware,
			OnError:        s.fail,
			NotFound:       s.parsed,
			Logger:         s.log,
		})
		s.central = &h
	}
	s.afterMounts = http.HandlerFunc(s.serveAfterMounts)

	uploadH := stripMount("/upload", uploads.NewHandler(uploads.HandlerOptions{
		DB: s.cfg.DB, Limit: s.apiLim.Middleware, Next: restoreMount(s.afterMounts), Logger: s.log,
	}))
	exportH := s.apiLim.Middleware(stripMount("/export", &dataexport.Handler{
		DB: s.cfg.DB, Next: restoreMount(s.afterMounts), Logger: s.log,
	}))
	limitedRest := s.apiLim.Middleware(s.afterMounts)

	top := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		// MCP is its own remote-protocol security boundary. It runs BEFORE
		// the broad body parsers so Host/Origin validation, rate limiting
		// and bearer auth all happen before an untrusted HTML-sized body is
		// parsed.
		case under(p, "/mcp"):
			s.mcp.ServeHTTP(w, r)
		// The agent surfaces' rate limit, also BEFORE the body parsers, so
		// an unauthenticated client cannot make the server parse a max-size
		// body on every request before the per-IP limiter rejects it.
		case under(p, "/api/v1"):
			limitedRest.ServeHTTP(w, r)
		// Out-of-band page-content upload: owns its raw body reader and its
		// own (ticket, not agent-token) auth, behind the same limiter.
		case under(p, "/upload"):
			uploadH.ServeHTTP(w, r)
		// The read-direction counterpart: short-lived GET-only URLs for one
		// published version's managed data. Reads no cookie.
		case under(p, "/export"):
			exportH.ServeHTTP(w, r)
		default:
			s.afterMounts.ServeHTTP(w, r)
		}
	})
	s.dashboard = shellHeaders(s.recoverer(top))
	return nil
}

// bodyLimits chooses the dashboard body parser's limits for r BEFORE any of
// its body is read. Only /api/v1 reads request bodies on the dashboard, and
// there only with a credential: a valid agent bearer token (the REST API) or
// an admin session (the admin JSON API). Every other request — a path that
// reads no body, or an /api/v1 request without a valid credential, which the
// routes refuse anyway — gets reqbody.UnauthenticatedOptions, so an
// anonymous client cannot make the server inflate and parse a
// MAX_REQUEST_BYTES document. The credential check runs only under /api/v1,
// which is already behind the per-IP API limiter, so an unauthenticated
// client cannot use it to drive token lookups without being rate limited.
//
// The token it verifies travels on the returned request (tokens.WithVerified),
// so tokens.RequireBearer reuses it instead of looking it up (and touching
// last_used_at) a second time.
func (s *Server) bodyLimits(r *http.Request) (*http.Request, reqbody.Options) {
	if !under(r.URL.EscapedPath(), "/api/v1") {
		return r, reqbody.UnauthenticatedOptions()
	}
	if raw, ok := tokens.BearerToken(r); ok {
		t, err := tokens.Verify(r.Context(), s.cfg.DB, raw)
		if err == nil && t != nil {
			return r.WithContext(tokens.WithVerified(r.Context(), raw, t)), reqbody.DashboardOptions()
		}
	}
	if sess, err := s.auth.Resolve(r); err == nil && s.auth.IsAdmin(sess) {
		return r, reqbody.DashboardOptions()
	}
	return r, reqbody.UnauthenticatedOptions()
}

// serveAfterMounts is the central sign-in surface (mounted before the body
// parsers because the back-channel receiver owns a smaller one), then the
// compose panel, then everything behind the dashboard body parser.
func (s *Server) serveAfterMounts(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	if c := s.central; c != nil {
		var h http.Handler
		switch {
		case pathIs(p, "/auth/login"):
			h = c.Login
		case pathIs(p, "/auth/callback"):
			h = c.Callback
		case pathIs(p, "/auth/backchannel-logout"):
			h = c.Backchannel
		case pathIs(p, "/logout"):
			h = c.Logout
		case pathIs(p, "/signed-out"):
			h = c.SignedOut
		}
		if h != nil {
			h.ServeHTTP(w, r)
			return
		}
	}
	// DEV/TEST ONLY: "compose a page with Cutlass". server.js mounted it
	// after the body parsers; the Go handler reads its own JSON body (with
	// the same 400/413 envelopes), so it runs ahead of reqbody — the one
	// difference is that a malformed body without a session is now a 401
	// rather than a 400.
	if s.composeH != nil && under(p, compose.Prefix) {
		s.composeH.ServeHTTP(w, r)
		return
	}
	s.parsed.ServeHTTP(w, r)
}

type routeFunc func(w http.ResponseWriter, r *http.Request, p route.Params)

var (
	rtHealthz   = route.New[routeFunc](http.MethodGet, "/healthz", nil)
	rtReadyz    = route.New[routeFunc](http.MethodGet, "/readyz", nil)
	rtDevLogin  = route.New[routeFunc](http.MethodGet, "/__dev/login", nil)
	rtMe        = route.New[routeFunc](http.MethodGet, "/api/me", nil)
	rtView      = route.New[routeFunc](http.MethodGet, "/view/*slug", nil)
	rtAdmin     = route.New[routeFunc](http.MethodGet, "/admin", nil)
	rtWelcome   = route.New[routeFunc](http.MethodGet, "/admin/welcome", nil)
	rtTemplates = route.New[routeFunc](http.MethodGet, "/admin/templates", nil)
	rtPortals   = route.New[routeFunc](http.MethodGet, "/admin/portals", nil)
	rtAdminSlug = route.New[routeFunc](http.MethodGet, "/admin/*slug", nil)
	rtRoot      = route.New[routeFunc](http.MethodGet, "/", nil)
	logoutRE    = route.Pattern("/logout")
)

// serveRoutes is the dashboard's routes behind the body parser, /shell-assets
// and the dev-login intercept, in server.js's registration order.
func (s *Server) serveRoutes(w http.ResponseWriter, r *http.Request) {
	p := r.URL.EscapedPath()
	// run tries one route: false when its method or path does not match.
	run := func(rt *route.Route[routeFunc], h routeFunc) bool {
		if !rt.Handles(r.Method) {
			return false
		}
		params, ok, err := rt.Match(p)
		if !ok {
			return false
		}
		if err != nil {
			// Express: a parameter that does not decode is a 400 error.
			s.fail(w, r, apierror.New(http.StatusBadRequest, "invalid request", "bad_request", nil))
			return true
		}
		h(w, r, params)
		return true
	}

	switch {
	case run(&rtHealthz, func(w http.ResponseWriter, r *http.Request, _ route.Params) {
		httpx.Send(w, r, http.StatusOK, "text/plain; charset=utf-8", []byte("ok"))
	}):
	case s.cfg.Ready != nil && run(&rtReadyz, func(w http.ResponseWriter, r *http.Request, _ route.Params) {
		s.cfg.Ready.ServeHTTP(w, r)
	}):
	case s.devLogin != nil && run(&rtDevLogin, func(w http.ResponseWriter, r *http.Request, _ route.Params) {
		s.devLogin.Login.ServeHTTP(w, r)
	}):
	// Who am I — proves the SSO cookie path works end to end.
	case run(&rtMe, func(w http.ResponseWriter, r *http.Request, _ route.Params) {
		s.auth.RequireAuth(http.HandlerFunc(s.me)).ServeHTTP(w, r)
	}):
	case run(&rtView, func(w http.ResponseWriter, r *http.Request, p route.Params) {
		s.auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.view(w, r, p.Slug) })).ServeHTTP(w, r)
	}):
	// Admin landing, registered BEFORE /admin/*slug so "welcome" resolves to
	// the index, not a page slug (createPage refuses the 'welcome' slug).
	case run(&rtAdmin, s.admin(s.welcome)), run(&rtWelcome, s.admin(s.welcome)):
	// The template library and partner portals, BEFORE /admin/*slug for the
	// same reason (both are reserved slug segments).
	case run(&rtTemplates, s.admin(func(w http.ResponseWriter, r *http.Request, _ route.Params) {
		email := auth.User(r).Email
		httpx.SendHTML(w, r, http.StatusOK, shell.Templates(csrf.Mint(email), email, env.ContentOrigin()))
	})):
	case run(&rtPortals, s.admin(func(w http.ResponseWriter, r *http.Request, _ route.Params) {
		email := auth.User(r).Email
		httpx.SendHTML(w, r, http.StatusOK, shell.Portals(csrf.Mint(email), email, env.ContentOrigin()))
	})):
	// Admin shell — a bootstrap that drives the admin JSON API.
	case run(&rtAdminSlug, s.admin(func(w http.ResponseWriter, r *http.Request, p route.Params) {
		email := auth.User(r).Email
		httpx.SendHTML(w, r, http.StatusOK, shell.Admin(p.Slug, csrf.Mint(email), email, env.ContentOrigin()))
	})):
	case under(p, "/api/v1"):
		s.apiH.ServeHTTP(w, r)
	// Logout is owned by the auth service (it clears the shared cookie). 303
	// so the browser re-issues as GET. In central mode the sign-in surface
	// owns /logout (served upstream).
	case s.central == nil && logoutRE.MatchString(p):
		httpx.Redirect(w, r, http.StatusSeeOther, s.auth.LoginURL()+"/logout")
	// Root is not a public surface: bounce to /admin, which is admin-gated —
	// staff land on the welcome index, everyone else is sent to sign-in.
	case run(&rtRoot, func(w http.ResponseWriter, r *http.Request, _ route.Params) {
		httpx.Redirect(w, r, http.StatusFound, "/admin")
	}):
	default:
		dashboardNotFound(w, r)
	}
}

// admin gates a shell route on an Elcano admin session.
func (s *Server) admin(h routeFunc) routeFunc {
	return func(w http.ResponseWriter, r *http.Request, p route.Params) {
		s.auth.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h(w, r, p) })).ServeHTTP(w, r)
	}
}

func (s *Server) welcome(w http.ResponseWriter, r *http.Request, _ route.Params) {
	email := auth.User(r).Email
	httpx.SendHTML(w, r, http.StatusOK, shell.Welcome(email, env.ContentOrigin(), shell.WelcomeOptions{
		CSRF: csrf.Mint(email), Compose: compose.Enabled(),
	}))
}

type meBody struct {
	Email  string `json:"email"`
	Tenant string `json:"tenant"`
	Admin  bool   `json:"admin"`
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u := auth.User(r)
	httpx.SendJSON(w, r, http.StatusOK, meBody{Email: u.Email, Tenant: u.Tenant, Admin: s.auth.IsAdmin(u)})
}

// view is the client view broker (PLAN §6b). For Elcano-only pages the
// content host can't do SSO — so staff come here: the session is verified,
// a short broker token for the published version is minted, and the browser
// bounces to the content host, which exchanges it for a page-session cookie.
//
// RequireAdmin, not RequireAuth: this route hands out a session cookie for
// the live page WITHOUT the per-page client password, so it is the one
// credential that reads every hosted dashboard. The shared SSO's audience is
// wider than Elcano staff, so accepting any valid session here would let a
// signed-in outsider read every client's numbers.
func (s *Server) view(w http.ResponseWriter, r *http.Request, slug string) {
	pv, err := versions.GetPage(r.Context(), s.cfg.DB, slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	page := pv.Page
	if page.PublishedVersionID == 0 {
		httpx.Send(w, r, http.StatusNotFound, "text/plain; charset=utf-8", []byte("nothing published yet"))
		return
	}
	// purpose "session": the only token the content host will exchange for
	// a page-session cookie, and the only place it is minted.
	token, err := rawtoken.Mint(rawtoken.MintParams{
		PageID: int64(page.ID), VersionID: int64(page.PublishedVersionID), Purpose: rawtoken.Session, RenderMode: "themed",
	}, 120)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	httpx.Redirect(w, r, http.StatusFound, env.ContentOrigin()+"/"+page.Slug+"?t="+httpx.EncodeURIComponent(token))
}

func dashboardNotFound(w http.ResponseWriter, r *http.Request) {
	httpx.SendHTML(w, r, http.StatusNotFound, shell.NotFound())
}

var apiPathRE = regexp.MustCompile(`(?i)^/api/v1(?:/|$)`)

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// fail is the dashboard's error boundary (server.js's error middleware):
// API errors keep their status, a bounded database wait is a 503, body
// parser failures are 400/413 with fixed messages, anything else is a logged
// 500. /api/v1 paths answer the machine-readable {error, code}; every other
// path the branded error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var mapped *apierror.Error
	var be *reqbody.Error
	if errors.As(err, &be) {
		mapped = be.APIError()
	} else if ae, ok := apierror.As(err); ok {
		mapped = ae
	} else if m := apierror.FromDB(err); m != nil {
		mapped = m
	}
	if mapped == nil {
		s.log.Error("dashboard error", "err", err)
		mapped = apierror.New(http.StatusInternalServerError, "internal error", "internal_error", nil)
	}
	if apiPathRE.MatchString(r.URL.EscapedPath()) {
		httpx.SendJSON(w, r, mapped.Status, errorBody{Error: mapped.Message, Code: mapped.Code})
		return
	}
	httpx.SendHTML(w, r, mapped.Status, shell.RequestError(mapped.Status))
}

// recoverer turns a panic into the error boundary's 500, as Express caught a
// thrown error. Once the response has started there is nothing left to
// render: the connection is aborted.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw := &trackingWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler || tw.wrote { //nolint:errorlint // sentinel identity, as net/http compares it
				panic(v)
			}
			s.fail(tw, r, fmt.Errorf("panic: %v", v))
		}()
		next.ServeHTTP(tw, r)
	})
}

type trackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (t *trackingWriter) WriteHeader(code int) {
	if code >= 200 {
		t.wrote = true
	}
	t.ResponseWriter.WriteHeader(code)
}

// Write forwards the body bytes unchanged, as a plain byte stream: the
// wrapper only notes that the response started. It never chooses a content
// type or adds content; whatever wrote b set the Content-Type on the shared
// header map and escaped (or chose a non-HTML type) at its own Write, which is
// where the HTML-injection checks belong.
func (t *trackingWriter) Write(b []byte) (int, error) {
	t.wrote = true
	var out io.Writer = t.ResponseWriter
	return out.Write(b)
}

func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		t.wrote = true
		f.Flush()
	}
}

func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// mcpUnavailable stands in for the MCP endpoint when none is wired.
func mcpUnavailable(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusServiceUnavailable,
		[]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"MCP endpoint unavailable"}}`))
}

// ── Rate-limit pages ───────────────────────────────────────────────────────

// slugOf is the limiter handler's view of the request: req.path without its
// leading slashes (the limiters run on full-path routes).
func slugOf(r *http.Request) string { return strings.TrimLeft(r.URL.EscapedPath(), "/") }

// contentBusy is the content READ limiter's answer. HTML, not JSON: every
// request it rejects is a person or an iframe loading a client's dashboard.
// Framable, because this limiter also guards /raw, which the /admin preview
// frames: a frame-ancestors 'none' rejection would show an operator a blank
// preview instead of the explanation.
func contentBusy(w http.ResponseWriter, r *http.Request) {
	csp.GateHeaders(csp.GateOptions{Framable: true}).Apply(w.Header())
	w.Header().Set("Retry-After", "60")
	httpx.SendHTML(w, r, http.StatusTooManyRequests, contentview.BusyPage(slugOf(r)))
}

// passwordBusy is the password form's brute-force guard answer: the page can
// link back and say the real wait, and Retry-After reaches a client that
// honours it.
func passwordBusy(w http.ResponseWriter, r *http.Request) {
	minutes := ratelimit.PasswordWindowMinutes()
	csp.GateHeaders(csp.GateOptions{}).Apply(w.Header())
	w.Header().Set("Retry-After", strconv.FormatFloat(minutes*60, 'f', -1, 64))
	// No ETag: a 429 is never fresh, so a validator could never serve a 304.
	httpx.SendNoValidator(w, r, http.StatusTooManyRequests, "text/html; charset=utf-8", []byte(contentview.RateLimitPage(slugOf(r), int(minutes))))
}

// signinBusy answers the central sign-in limiters: HTML, because a browser
// is always the caller.
func signinBusy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "60")
	w.Header().Set("Cache-Control", "no-store")
	httpx.SendHTML(w, r, http.StatusTooManyRequests, shell.Standalone(shell.StandaloneOptions{
		Title:      "Sign-in · Pages",
		Kicker:     "Sign-in",
		Heading:    "Too many sign-in attempts",
		BodyHTML:   "<p>Wait a minute, then try again.</p>",
		AssetsBase: assetver.ShellFlag(),
	}))
}

// renderChrome draws central sign-in's standalone pages.
func renderChrome(p central.ChromePage) string {
	o := shell.StandaloneOptions{Title: p.Title, Kicker: p.Kicker, Heading: p.Heading, BodyHTML: p.BodyHTML, AssetsBase: p.AssetsBase}
	if p.CTA != nil {
		o.CTA = &shell.CTA{Href: p.CTA.Href, Label: p.CTA.Label}
	}
	return shell.Standalone(o)
}

// ── Express mount semantics ────────────────────────────────────────────────

// under is Express's app.use(prefix) match: the prefix itself or a path
// below it, case-insensitively, against the raw path.
func under(path, prefix string) bool {
	if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

// pathIs is a literal route path: case-insensitive, trailing slash optional.
func pathIs(path, literal string) bool {
	return strings.EqualFold(path, literal) || strings.EqualFold(path, literal+"/")
}

type mountKey struct{}

// stripMount serves h with prefix removed from the path, as an Express mount
// does ("" becomes "/"); restoreMount gives a fall-through the original.
func stripMount(prefix string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest := r.URL.EscapedPath()[len(prefix):]
		if rest == "" {
			rest = "/"
		}
		r2 := r.WithContext(context.WithValue(r.Context(), mountKey{}, r))
		u := *r.URL
		u.RawPath = rest
		if p, err := url.PathUnescape(rest); err == nil {
			u.Path = p
		} else {
			u.Path = rest
		}
		if u.EscapedPath() != rest {
			u.RawPath = rest
		}
		r2.URL = &u
		h.ServeHTTP(w, r2)
	})
}

func restoreMount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if orig, ok := r.Context().Value(mountKey{}).(*http.Request); ok {
			r = orig.WithContext(r.Context())
		}
		next.ServeHTTP(w, r)
	})
}
