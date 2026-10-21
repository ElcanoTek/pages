// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth

import (
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/ElcanoTek/pages/internal/auth/wire"
	"github.com/ElcanoTek/pages/internal/env"
)

// DevLogin is the local dev-login (GATED). The real auth service mints the
// elcano_auth cookie; locally scripts/dev.sh mints a dev one (DEV_ADMIN_COOKIE,
// signed by a throwaway key whose public half it exports as
// AUTH_SIGNING_PUBKEY) and this exposes it so /admin works in a browser
// without auth.elcanotek.com.
type DevLogin struct {
	// Intercept keeps auth-failure redirects on the SAME host (relative), so
	// localhost, 127.0.0.1, or a VM IP all work without bouncing to a pinned
	// host or the real auth. Mount it ahead of the /admin and /view routes,
	// so a no-session GET is intercepted here instead of by RequireAdmin.
	Intercept func(http.Handler) http.Handler
	// Login is GET /__dev/login.
	Login http.Handler
}

// DevLoginFromEnv returns the dev-login pieces, or ok=false when they must
// not exist. They exist ONLY when PAGES_DEV_LOGIN=1 and DEV_ADMIN_COOKIE is
// set, in elcano mode, and never in production (PAGES_ENV / NODE_ENV =
// production) — the last check is new in the Go port: the JS relied on
// nobody setting the flag there.
func (a *Authenticator) DevLoginFromEnv() (DevLogin, bool) {
	cookie := os.Getenv("DEV_ADMIN_COOKIE")
	if os.Getenv("PAGES_DEV_LOGIN") != "1" || cookie == "" || a.central != nil {
		return DevLogin{}, false
	}
	if env.Production() {
		slog.Warn("PAGES_DEV_LOGIN=1 is ignored in production; /__dev/login is not mounted.")
		return DevLogin{}, false
	}
	slog.Warn("⚠ DEV LOGIN ENABLED: GET /__dev/login sets a local admin cookie. Do NOT set PAGES_DEV_LOGIN=1 in production.")
	intercept := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if r.Method == http.MethodGet && (strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/view/")) && a.CurrentSession(r) == nil {
				wire.Redirect(w, r, http.StatusFound, "/__dev/login?next="+wire.EncodeURIComponent(wire.OriginalURL(r)))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	login := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", a.cookieName+"="+cookie+"; Path=/; HttpOnly; SameSite=Lax")
		// Accept `next` (our own param) or `return_to` (what
		// LoginRedirectURL sends when RequireAdmin bounces here). Always
		// reduce to a LOCAL path so a no-cookie /admin visit round-trips
		// through here and back — never to the real auth/home.
		raw := "/"
		if v, ok := wire.QueryParam(r, "next").String(); ok && v != "" {
			raw = v
		} else if v, ok := wire.QueryParam(r, "return_to").String(); ok && v != "" {
			raw = v
		}
		wire.Redirect(w, r, http.StatusFound, devLocalPath(raw))
	})
	return DevLogin{Intercept: intercept, Login: login}, true
}

// devLocalPath keeps only the path and query of raw. The JS passed any
// "/"-prefixed value through, so "//evil.example" was an open redirect; here
// a protocol-relative or backslash form also reduces to its path.
func devLocalPath(raw string) string {
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") && !strings.HasPrefix(raw, `/\`) {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme == "" && !strings.HasPrefix(raw, "//")) {
		return "/"
	}
	dest := u.EscapedPath()
	if dest == "" {
		dest = "/"
	}
	if u.RawQuery != "" {
		dest += "?" + u.RawQuery
	}
	if dest == "" || !strings.HasPrefix(dest, "/") || strings.HasPrefix(dest, "//") {
		return "/"
	}
	return dest
}
