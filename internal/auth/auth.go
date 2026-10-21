// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package auth is Pages' sign-in and admin gate (lib/auth.js).
//
// PAGES_AUTH_MODE selects the sign-in model (default "elcano"):
//
//	elcano   Elcano's unified auth (home/server.js, "Pattern B": native
//	         Ed25519 cookie verification). Pages verifies the elcano_auth
//	         cookie minted by auth.elcanotek.com using only the auth
//	         service's PUBLIC key (AUTH_SIGNING_PUBKEY): it can verify a
//	         token but never mint one, so the value is safe to distribute
//	         and a leak here cannot forge sessions. An Elcano user (email in
//	         ADMIN_EMAIL_DOMAIN, default elcanotek.com) is an admin; that is
//	         the entire authorization model for /admin.
//	central  the application handoff of a password-mode Auth deployment
//	         (package central): Pages runs its own /auth/login and
//	         /auth/callback, keeps a local access list that Auth's signed
//	         events maintain, and mints a host-only Pages session. Everyone
//	         on that access list is an admin — Pages has one human role.
//
// Routes resolve the session through Resolve (cached on the request
// context), so both modes share RequireAuth and RequireAdmin.
package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/auth/central"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// The two sign-in modes.
const (
	ModeElcano  = "elcano"
	ModeCentral = "central"
)

// Session is the signed-in identity for a request.
//
//	elcano:  Email, Tenant and Exp from the shared cookie.
//	central: Email and Subject from a live Pages session whose email is
//	         still enabled on the access list (Tenant "").
type Session struct {
	Email   string
	Tenant  string
	Exp     float64
	Subject string
}

// Authenticator is the configured sign-in model. Build it once at start with
// NewFromEnv; it is safe for concurrent use.
type Authenticator struct {
	mode        string
	cookieName  string
	adminDomain string
	publicKey   ed25519.PublicKey // nil: verification fails closed
	loginURL    string
	origin      string
	central     *central.Runtime

	// ErrorHandler answers a failure to resolve the session (a database
	// error in central mode). Default: a plain 500.
	ErrorHandler func(w http.ResponseWriter, r *http.Request, err error)
	// Now is the clock for cookie expiry (default time.Now).
	Now func() time.Time
}

// ModeFromEnv is PAGES_AUTH_MODE, validated.
func ModeFromEnv() (string, error) {
	mode := strings.ToLower(jsstr.Trim(env.String("PAGES_AUTH_MODE", ModeElcano)))
	if mode != ModeElcano && mode != ModeCentral {
		return "", errors.New(`PAGES_AUTH_MODE must be "elcano" or "central"`)
	}
	return mode, nil
}

// DecodeElcanoPublicKey builds the verify-only key from the raw 32-byte
// Ed25519 public key (base64 in env). It decodes leniently, as Node's
// Buffer.from(x, "base64") did.
func DecodeElcanoPublicKey(encoded string) (ed25519.PublicKey, error) {
	raw := looseBase64(encoded)
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected 32 bytes, got %d", len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// NewFromEnv reads PAGES_AUTH_MODE and its settings. Central mode validates
// its whole configuration here, so a misconfigured box fails at start
// instead of on the first sign-in; d is its store (unused in elcano mode).
func NewFromEnv(d *db.DB) (*Authenticator, error) {
	mode, err := ModeFromEnv()
	if err != nil {
		return nil, err
	}
	a := &Authenticator{
		mode:        mode,
		cookieName:  env.String("AUTH_COOKIE_NAME", "elcano_auth"),
		adminDomain: strings.ToLower(env.String("ADMIN_EMAIL_DOMAIN", "elcanotek.com")),
	}
	if pub := env.String("AUTH_SIGNING_PUBKEY", ""); pub != "" {
		key, err := DecodeElcanoPublicKey(pub)
		if err != nil {
			slog.Warn(fmt.Sprintf("WARNING: AUTH_SIGNING_PUBKEY is malformed (%s); all auth will fail.", err))
		} else {
			a.publicKey = key
		}
	}
	if mode == ModeCentral {
		rt, err := central.RuntimeFromEnv(d)
		if err != nil {
			return nil, err
		}
		a.central = rt
		a.loginURL = rt.Config.Issuer
	} else {
		a.loginURL = strings.TrimSuffix(env.String("AUTH_LOGIN_URL", "https://auth.elcanotek.com"), "/")
	}
	u, err := url.Parse(a.loginURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("AUTH_LOGIN_URL is not an absolute URL: %q", a.loginURL)
	}
	a.origin = strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
	if (u.Scheme == "https" && strings.HasSuffix(a.origin, ":443")) || (u.Scheme == "http" && strings.HasSuffix(a.origin, ":80")) {
		a.origin = a.origin[:strings.LastIndexByte(a.origin, ':')]
	}
	if a.publicKey == nil && a.central == nil {
		slog.Warn("WARNING: AUTH_SIGNING_PUBKEY is unset or invalid — every admin request will be " +
			"redirected to auth. Set it to the auth service's AUTH_SIGNING_PUBKEY (public half).")
	}
	return a, nil
}

// Mode is ModeElcano or ModeCentral.
func (a *Authenticator) Mode() string { return a.mode }

// Central is the central-mode runtime (routes, store, cookie names), nil in
// elcano mode.
func (a *Authenticator) Central() *central.Runtime { return a.central }

// LoginURL is AUTH_LOGIN_URL (elcano) or Auth's issuer (central), without a
// trailing slash.
func (a *Authenticator) LoginURL() string { return a.loginURL }

// Origin is LoginURL's origin (the shell CSP's form-action source).
func (a *Authenticator) Origin() string { return a.origin }

// CookieName is the elcano SSO cookie's name (AUTH_COOKIE_NAME).
func (a *Authenticator) CookieName() string { return a.cookieName }

// ParseCookies is the cookie-jar parser shared by SSO auth and the content
// host's page sessions (see httpx.ParseCookies).
func ParseCookies(header string) map[string]string { return httpx.ParseCookies(header) }

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// looseBase64 is Buffer.from(s, "base64"/"base64url"): both alphabets,
// characters outside them skipped, stopping at padding, a dangling sextet
// ignored. It never fails.
func looseBase64(s string) []byte {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '=':
			i = len(s)
		case c == '-':
			b.WriteByte('+')
		case c == '_':
			b.WriteByte('/')
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/':
			b.WriteByte(c)
		}
	}
	text := b.String()
	if len(text)%4 == 1 {
		text = text[:len(text)-1]
	}
	out, err := base64.RawStdEncoding.DecodeString(text)
	if err != nil {
		return nil
	}
	return out
}

// VerifyElcanoSession mirrors auth/internal/token/token.go. Token format:
// base64url(payloadJSON) + "." + base64url(ed25519Signature); the signature
// covers the base64url-encoded body string, NOT the raw JSON. It returns the
// session, or nil for any failure (treated as "logged out").
func (a *Authenticator) VerifyElcanoSession(rawToken string) *Session {
	if a.publicKey == nil || rawToken == "" {
		return nil
	}
	dot := strings.IndexByte(rawToken, '.')
	if dot < 1 || dot == len(rawToken)-1 {
		return nil
	}
	body := rawToken[:dot]
	sig := looseBase64(rawToken[dot+1:])
	if !ed25519.Verify(a.publicKey, []byte(body), sig) {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(looseBase64(body), &payload); err != nil || payload == nil {
		return nil
	}
	email, ok := payload["email"].(string)
	if !ok || email == "" {
		return nil
	}
	exp, ok := payload["exp"].(float64)
	if !ok || exp <= float64(a.now().Unix()) {
		return nil
	}
	// `payload.tenant || ""`: Auth always sends a string; anything else
	// reads as no tenant.
	tenant, _ := payload["tenant"].(string)
	return &Session{Email: email, Tenant: tenant, Exp: exp}
}

// CurrentSession is the elcano-mode session from the request's SSO cookie.
func (a *Authenticator) CurrentSession(r *http.Request) *Session {
	return a.VerifyElcanoSession(httpx.RequestCookies(r)[a.cookieName])
}

// IsElcanoAdmin: an Elcano staffer (email in the admin domain) is an admin.
// This is the whole authorization model for /admin in elcano mode — no
// roles table.
func (a *Authenticator) IsElcanoAdmin(s *Session) bool {
	if s == nil {
		return false
	}
	at := strings.LastIndexByte(s.Email, '@')
	if at < 0 {
		return false
	}
	return strings.ToLower(s.Email[at+1:]) == a.adminDomain
}

// IsAdmin — may this session use /admin, /view and the admin API? In
// central mode the access list was checked when the session resolved, so any
// live session is an admin.
func (a *Authenticator) IsAdmin(s *Session) bool {
	if s == nil {
		return false
	}
	return a.central != nil || a.IsElcanoAdmin(s)
}

type ctxKey struct{}

type slot struct {
	session *Session
}

// WithSession returns r carrying s as its resolved session (what the
// middleware does; also for tests and callers that resolved it themselves).
func WithSession(r *http.Request, s *Session) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, &slot{session: s}))
}

// FromContext is the session a middleware resolved for this request (JS
// req.user), nil when none was attached.
func FromContext(ctx context.Context) *Session {
	if sl, ok := ctx.Value(ctxKey{}).(*slot); ok {
		return sl.session
	}
	return nil
}

// User is FromContext(r.Context()).
func User(r *http.Request) *Session { return FromContext(r.Context()) }

// Resolve is the signed-in identity for this request, or nil. A session
// already resolved into the request context (by a middleware or
// WithSession) is returned without another lookup.
func (a *Authenticator) Resolve(r *http.Request) (*Session, error) {
	if sl, ok := r.Context().Value(ctxKey{}).(*slot); ok {
		return sl.session, nil
	}
	if a.central == nil {
		return a.CurrentSession(r), nil
	}
	token := a.central.SessionToken(r)
	if token == "" {
		return nil, nil
	}
	id, err := a.central.Store.Identity(r.Context(), token)
	if err != nil || id == nil {
		return nil, err
	}
	return &Session{Email: id.Email, Subject: id.Subject}, nil
}

// LoginRedirectURL is where an anonymous request is sent to sign in.
func (a *Authenticator) LoginRedirectURL(r *http.Request) string {
	target := httpx.OriginalURL(r)
	if a.central != nil {
		if target == "" {
			target = "/admin"
		}
		if jsstr.UTF16Len(target) > 2048 {
			target = "/admin"
		}
		return "/auth/login?next=" + httpx.EncodeURIComponent(target)
	}
	// Caddy terminates TLS and proxies to us over plain HTTP, so the
	// connection reads as plain on a genuinely https request. Trust the
	// X-Forwarded-Proto Caddy sets (same check as contentview's isSecure).
	// Without this the return_to goes out as http://pages.elcanotek.com/...;
	// the auth service then bounces the user to its home default instead of
	// back here, so staff land on home.elcanotek.com after sign-in instead of
	// the page they asked for.
	xf, _, _ := strings.Cut(httpx.Header(r, "X-Forwarded-Proto"), ",")
	protocol := "http"
	if r.TLS != nil || strings.ToLower(jsstr.Trim(xf)) == "https" {
		protocol = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	returnTo := protocol + "://" + host + target
	return a.loginURL + "/?return_to=" + httpx.EncodeURIComponent(returnTo)
}

func (a *Authenticator) fail(w http.ResponseWriter, r *http.Request, err error) {
	if a.ErrorHandler != nil {
		a.ErrorHandler(w, r, err)
		return
	}
	slog.Error("auth: session lookup failed", "err", err)
	http.Error(w, "Internal Server Error", http.StatusInternalServerError)
}

// RequireAuth lets any valid session pass, attaching it to the request
// context (see User); an anonymous request is redirected to sign in.
func (a *Authenticator) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := a.Resolve(r)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if s == nil {
			httpx.Redirect(w, r, http.StatusFound, a.LoginRedirectURL(r))
			return
		}
		next.ServeHTTP(w, WithSession(r, s))
	})
}

// RequireAdmin requires a valid session AND an admin. Not signed in →
// sign-in; signed in but not an admin → 403.
func (a *Authenticator) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := a.Resolve(r)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		if s != nil && a.IsAdmin(s) {
			next.ServeHTTP(w, WithSession(r, s))
			return
		}
		if s == nil {
			httpx.Redirect(w, r, http.StatusFound, a.LoginRedirectURL(r))
			return
		}
		httpx.SendHTML(w, r, http.StatusForbidden, "Forbidden — Elcano staff only.")
	})
}

// Redirect is Express's res.redirect(status, url) (see httpx.Redirect).
func Redirect(w http.ResponseWriter, r *http.Request, status int, url string) {
	httpx.Redirect(w, r, status, url)
}
