// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package central

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/assetver"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/httpx"
)

// Runtime is central mode, configured: the validated settings, the client,
// the one key set that verifies both the id_token at sign-in and Auth's
// back-channel tokens (so a rotation reaches both at once), the store and
// the cookie names.
type Runtime struct {
	Config Config
	Client *Client
	Keys   *KeyResolver
	Store  *Store
	// SessionCookie and LoginCookie carry the __Host- prefix when cookies
	// are Secure: it pins them to this exact host over HTTPS with Path=/, so
	// nothing on a sibling subdomain can set or shadow them.
	SessionCookie string
	LoginCookie   string
}

// NewRuntime assembles a Runtime from a validated Config and the database.
func NewRuntime(config Config, d *db.DB) *Runtime {
	keys := NewKeyResolver(config.Issuer, config.StaticKeys, nil, nil)
	client, _ := NewClient(config, keys) // keys is non-nil
	return &Runtime{
		Config:        config,
		Client:        client,
		Keys:          keys,
		Store:         NewStore(d),
		SessionCookie: SessionCookieName(config.CookieSecure),
		LoginCookie:   LoginCookieName(config.CookieSecure),
	}
}

// RuntimeFromEnv is LoadConfigFromEnv + NewRuntime.
func RuntimeFromEnv(d *db.DB) (*Runtime, error) {
	config, err := LoadConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return NewRuntime(config, d), nil
}

// SessionCookieName is the Pages session cookie's name.
func SessionCookieName(secure bool) string {
	if secure {
		return "__Host-pages_session"
	}
	return "pages_session"
}

// LoginCookieName is the login-transaction cookie's name.
func LoginCookieName(secure bool) string {
	if secure {
		return "__Host-pages_login"
	}
	return "pages_login"
}

// SessionToken is the request's session cookie value ("" when none).
func (rt *Runtime) SessionToken(r *http.Request) string {
	return httpx.RequestCookies(r)[rt.SessionCookie]
}

// Cookie is a Set-Cookie value with the attributes every central-mode
// cookie carries. maxAge < 0 omits Max-Age (a session cookie).
func (rt *Runtime) Cookie(name, value string, maxAge int) string {
	parts := []string{name + "=" + value, "Path=/", "HttpOnly", "SameSite=Lax"}
	if rt.Config.CookieSecure {
		parts = append(parts, "Secure")
	}
	if maxAge >= 0 {
		parts = append(parts, "Max-Age="+strconv.Itoa(maxAge))
	}
	return strings.Join(parts, "; ")
}

func (rt *Runtime) clearCookie(name string) string { return rt.Cookie(name, "", 0) }

// ── Routes ────────────────────────────────────────────────────────────────

// ChromePage is what a standalone "chrome" page shows (lib/standalone-chrome.js
// render's options).
type ChromePage struct {
	Title      string
	Kicker     string
	Heading    string
	BodyHTML   string
	AssetsBase string
	CTA        *CTA
}

// CTA is the page's one call-to-action link.
type CTA struct {
	Href  string
	Label string
}

// ChromeRenderer renders a standalone page to HTML (internal/shell owns the
// real one).
type ChromeRenderer func(p ChromePage) string

// Middleware wraps a handler (a rate limiter); nil means none.
type Middleware func(http.Handler) http.Handler

// RoutesOptions are the pieces of the sign-in surface that belong to other
// packages.
type RoutesOptions struct {
	// Render draws the no-access, sign-in-failed and signed-out pages.
	Render ChromeRenderer
	// Per-route rate limiters (lib/ratelimit.js signin, signinCallback,
	// signout, backchannel); nil = unlimited.
	Signin, SigninCallback, Signout, Backchannel Middleware
	// OnError answers an unexpected failure (a database error): the server's
	// error boundary. Body-parser refusals arrive as *apierror.Error with
	// Status 413/415. Default: a plain 500.
	OnError func(w http.ResponseWriter, r *http.Request, err error)
	// NotFound answers a request whose method a route does not take (Express
	// falls through to the next route; Pages' fallback is its 404 page).
	// Default: http.NotFound.
	NotFound http.Handler
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

type routes struct {
	rt   *Runtime
	opts RoutesOptions
}

// Register mounts the dashboard host's sign-in surface on mux:
//
//	GET  /auth/login               start the handoff (state, nonce, PKCE)
//	GET  /auth/callback            finish it, mint the Pages session
//	POST /auth/backchannel-logout  Auth's signed logout and access events
//	ANY  /logout                   end this session, then Auth's RP logout
//	GET  /signed-out               public landing after sign-out
//
// A trailing slash matches too, as Express's non-strict routing did. Mount it
// on the dashboard host only, never the content host.
func (rt *Runtime) Register(mux *http.ServeMux, opts RoutesOptions) {
	h := rt.Handlers(opts)
	for path, handler := range map[string]http.Handler{
		"/auth/login":              h.Login,
		"/auth/callback":           h.Callback,
		"/auth/backchannel-logout": h.Backchannel,
		"/logout":                  h.Logout,
		"/signed-out":              h.SignedOut,
	} {
		mux.Handle(path, handler)
		mux.Handle(path+"/{$}", handler)
	}
}

// Handlers are the five routes, each already wrapped in its rate limiter and
// method check.
type Handlers struct {
	Login, Callback, Backchannel, Logout, SignedOut http.Handler
}

// Handlers builds the routes without mounting them.
func (rt *Runtime) Handlers(opts RoutesOptions) Handlers {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.NotFound == nil {
		opts.NotFound = http.NotFoundHandler()
	}
	if opts.OnError == nil {
		opts.OnError = func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}
	if opts.Render == nil {
		opts.Render = func(p ChromePage) string { return "<h1>" + p.Heading + "</h1>" + p.BodyHTML }
	}
	x := &routes{rt: rt, opts: opts}
	wrap := func(m Middleware, h http.Handler) http.Handler {
		if m == nil {
			return h
		}
		return m(h)
	}
	// The method check runs before the limiter: Express only enters a
	// route's middleware once the method matched.
	method := func(allowed func(string) bool, h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allowed(r.Method) {
				opts.NotFound.ServeHTTP(w, r)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	isGet := func(m string) bool { return m == http.MethodGet || m == http.MethodHead }
	return Handlers{
		Login:       method(isGet, wrap(opts.Signin, http.HandlerFunc(x.login))),
		Callback:    method(isGet, wrap(opts.SigninCallback, http.HandlerFunc(x.callback))),
		Backchannel: method(func(m string) bool { return m == http.MethodPost }, wrap(opts.Backchannel, http.HandlerFunc(x.backchannel))),
		Logout:      wrap(opts.Signout, http.HandlerFunc(x.logout)),
		SignedOut:   method(isGet, http.HandlerFunc(x.signedOut)),
	}
}

func (x *routes) page(w http.ResponseWriter, r *http.Request, status int, title, heading, body string, cta *CTA) {
	w.Header().Set("Cache-Control", "no-store")
	html := x.opts.Render(ChromePage{
		Title:      title + " · Pages",
		Kicker:     title,
		Heading:    heading,
		BodyHTML:   "<p>" + body + "</p>",
		AssetsBase: assetver.ShellFlag(),
		CTA:        cta,
	})
	httpx.SendHTML(w, r, status, html)
}

var retry = &CTA{Href: "/auth/login", Label: "Sign in again"}

func (x *routes) noAccess(w http.ResponseWriter, r *http.Request) {
	x.page(w, r, http.StatusForbidden, "No access",
		"Your account does not have access to Pages",
		"You are signed in, but this account has not been granted Pages. Ask an administrator to grant it in the account console.",
		&CTA{Href: x.rt.Config.Issuer + "/account", Label: "Your account"})
}

func (x *routes) login(w http.ResponseWriter, r *http.Request) {
	nextRaw := "/admin"
	if q := httpx.QueryParam(r, "next"); q.Present {
		nextRaw, _ = q.String() // an array is not a string: SafeLocalPath("") → /admin
	}
	next := SafeLocalPath(nextRaw)
	id, err := x.rt.Store.Identity(r.Context(), x.rt.SessionToken(r))
	if err != nil {
		x.opts.OnError(w, r, err)
		return
	}
	if id != nil {
		httpx.Redirect(w, r, http.StatusSeeOther, next)
		return
	}
	tx := NewTransaction(next, 0)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Set-Cookie", x.rt.Cookie(x.rt.LoginCookie, SealTransaction(tx, x.rt.Config.TransactionKey), LoginTransactionSeconds))
	httpx.Redirect(w, r, http.StatusSeeOther, x.rt.Client.AuthorizationURL(tx.State, tx.Nonce, tx.Challenge))
}

func (x *routes) callback(w http.ResponseWriter, r *http.Request) {
	// The transaction is single-use whatever happens next.
	w.Header().Set("Set-Cookie", x.rt.clearCookie(x.rt.LoginCookie))
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := httpx.ParseQuery(r.URL.RawQuery)
	one := func(name string) (string, bool) {
		v, ok := q[name]
		if !ok || len(v) != 1 {
			return "", false
		}
		return v[0], true
	}
	errVals, errPresent := q["error"]
	if len(errVals) == 1 && errVals[0] == "access_denied" {
		x.noAccess(w, r)
		return
	}
	// Auth names itself in the response (RFC 9207); a different issuer means
	// the response is not from the Auth this deployment trusts.
	issVals, issPresent := q["iss"]
	wrongIssuer := issPresent && (len(issVals) != 1 || StripTrailingSlashes(issVals[0]) != x.rt.Config.Issuer)
	code, codeOK := one("code")
	state, stateOK := one("state")
	// `error` is truthy unless it is a single empty string.
	errTruthy := errPresent && (len(errVals) != 1 || errVals[0] != "")
	if errTruthy || wrongIssuer || !codeOK || code == "" || !stateOK || state == "" {
		x.page(w, r, http.StatusBadRequest, "Sign-in", "Sign-in was not completed", "Start again from the sign-in page.", retry)
		return
	}
	err := func() error {
		sealed := httpx.RequestCookies(r)[x.rt.LoginCookie]
		tx, err := OpenTransaction(sealed, state, x.rt.Config.TransactionKey, 0)
		if err != nil {
			return err
		}
		exchangeStartedMs := x.rt.Store.nowMs()
		principal, err := x.rt.Client.Exchange(r.Context(), code, tx.Verifier, tx.Nonce)
		if err != nil {
			return err
		}
		token, err := x.rt.Store.CreateSession(r.Context(), principal.Subject, principal.Email, exchangeStartedMs)
		if err != nil {
			return err
		}
		w.Header().Add("Set-Cookie", x.rt.Cookie(x.rt.SessionCookie, token, -1))
		httpx.Redirect(w, r, http.StatusSeeOther, tx.Next)
		return nil
	}()
	switch {
	case err == nil:
	case errors.Is(err, ErrAccessDenied):
		x.noAccess(w, r)
	case errors.Is(err, ErrAuthTransaction), errors.Is(err, ErrCodeExchangeRejected):
		x.page(w, r, http.StatusBadRequest, "Sign-in", "Sign-in expired", "That sign-in link is no longer valid. Try again.", retry)
	case errors.Is(err, ErrCentralAuth):
		x.opts.Logger.Error("central auth: code exchange failed: " + err.Error())
		x.page(w, r, http.StatusBadGateway, "Sign-in", "The sign-in service is unavailable",
			"Pages could not reach the account service. Try again shortly.", retry)
	default:
		x.opts.OnError(w, r, err)
	}
}

// maxBackchannelBody is the receiver's own small body cap (20kb in
// body-parser terms: 20 × 1024 bytes).
const maxBackchannelBody = 20 * 1024

// parseBackchannelForm is express.urlencoded({ extended: false, limit:
// "20kb", parameterLimit: 4 }): nil fields (and no error) when the body is
// not a form, a 413 for an oversized body or too many parameters, a 415 for
// an unsupported charset or content encoding.
func parseBackchannelForm(r *http.Request) (map[string][]string, error) {
	// An unparsable Content-Type is simply not a form (body-parser skips it).
	mt, params, perr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if perr != nil {
		mt = ""
	}
	if mt != "application/x-www-form-urlencoded" {
		return nil, nil
	}
	if r.ContentLength == 0 || (r.ContentLength < 0 && len(r.TransferEncoding) == 0) {
		return map[string][]string{}, nil
	}
	charset := strings.ToLower(params["charset"])
	if charset != "" && charset != "utf-8" && charset != "iso-8859-1" {
		return nil, apierror.New(http.StatusUnsupportedMediaType, "invalid request", "bad_request", nil)
	}
	if enc := strings.ToLower(r.Header.Get("Content-Encoding")); enc != "" && enc != "identity" {
		return nil, apierror.New(http.StatusUnsupportedMediaType, "invalid request", "bad_request", nil)
	}
	tooLarge := apierror.New(http.StatusRequestEntityTooLarge, "request body too large", "body_too_large", nil)
	if r.ContentLength > maxBackchannelBody {
		return nil, tooLarge
	}
	defer httpx.ArmBodyReadDeadline(r, maxBackchannelBody)()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBackchannelBody+1))
	if err != nil {
		return nil, apierror.New(http.StatusBadRequest, "invalid request", "bad_request", nil)
	}
	if len(raw) > maxBackchannelBody {
		return nil, tooLarge
	}
	body := string(raw)
	if charset == "iso-8859-1" {
		runes := make([]rune, len(raw))
		for i, b := range raw {
			runes[i] = rune(b)
		}
		body = string(runes)
	}
	if body == "" {
		return map[string][]string{}, nil
	}
	if strings.Count(body, "&")+1 > 4 {
		return nil, apierror.New(http.StatusRequestEntityTooLarge, "invalid request", "bad_request", nil)
	}
	return httpx.ParseQuery(body), nil
}

// backchannel is Auth's server-to-server channel. Exactly one form field:
// logout_token (end the subject's sessions) or access_token (grant/revoke
// this application). 204 once durable; 400 for a token that will never
// verify; 503 for a failure worth retrying (Auth retries every non-2xx with
// backoff).
func (x *routes) backchannel(w http.ResponseWriter, r *http.Request) {
	form, err := parseBackchannelForm(r)
	if err != nil {
		x.opts.OnError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	invalidRequest := map[string]string{"error": "invalid_request"}
	if len(form) != 1 {
		httpx.SendJSON(w, r, http.StatusBadRequest, invalidRequest)
		return
	}
	var field string
	var values []string
	for k, v := range form {
		field, values = k, v
	}
	// A key with a bracket is an object or array under qs, never a string.
	if (field != "logout_token" && field != "access_token") || len(values) != 1 || values[0] == "" {
		httpx.SendJSON(w, r, http.StatusBadRequest, invalidRequest)
		return
	}
	raw := values[0]
	opts := VerifyOptions{
		Issuer:   x.rt.Config.Issuer,
		Audience: x.rt.Config.ClientID,
		Keys:     x.rt.Keys.KeysForToken(r.Context(), raw),
	}
	if field == "logout_token" {
		ev, err := VerifyLogoutToken(raw, opts)
		if err != nil {
			httpx.SendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_token"})
			return
		}
		if _, err := x.rt.Store.ConsumeLogoutEvent(r.Context(), ev); err != nil {
			x.unavailable(w, r, err)
			return
		}
	} else {
		ev, err := VerifyAccessToken(raw, opts)
		if err != nil {
			httpx.SendJSON(w, r, http.StatusBadRequest, map[string]string{"error": "invalid_token"})
			return
		}
		if _, err := x.rt.Store.ApplyAccessEvent(r.Context(), ev); err != nil {
			x.unavailable(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (x *routes) unavailable(w http.ResponseWriter, r *http.Request, err error) {
	x.opts.Logger.Error("central auth: back-channel event not applied: " + err.Error())
	httpx.SendJSON(w, r, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
}

// logout ends this session and hands the browser to Auth's RP-initiated
// logout, which ends the central session and fans a back-channel logout out
// to every application. Ending only the local session would be undone by the
// next silent sign-in. A link, not a form, so the shell stays unchanged. A
// request that a browser marks as cross-site (Fetch Metadata) or that carries
// a foreign Origin is ignored. A client sending neither header can still be
// made to sign out: a nuisance, not a takeover, and the same one Auth's own
// GET /logout?client_id accepts.
func (x *routes) logout(w http.ResponseWriter, r *http.Request) {
	site := httpx.Header(r, "Sec-Fetch-Site")
	origin := httpx.Header(r, "Origin")
	if (site != "" && site != "same-origin" && site != "none") || (origin != "" && origin != x.rt.Config.PublicURL) {
		httpx.Redirect(w, r, http.StatusSeeOther, "/admin")
		return
	}
	if token := x.rt.SessionToken(r); token != "" {
		if _, err := x.rt.Store.RevokeSession(r.Context(), token); err != nil {
			x.opts.OnError(w, r, err)
			return
		}
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Set-Cookie", x.rt.clearCookie(x.rt.SessionCookie))
	h.Add("Set-Cookie", x.rt.clearCookie(x.rt.LoginCookie))
	httpx.Redirect(w, r, http.StatusSeeOther, x.rt.Client.LogoutURL())
}

// signedOut is registered with Auth as the post-logout landing. Public and
// identity-free: bouncing to /admin would start a silent sign-in at once.
func (x *routes) signedOut(w http.ResponseWriter, r *http.Request) {
	x.page(w, r, http.StatusOK, "Signed out", "You are signed out of Pages", "Sign in again to manage dashboards.", retry)
}
