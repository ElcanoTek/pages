// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package csrf is admin-cookie mutation protection (PLAN §7). Admin actions
// (approve/reject/disable/approval-toggle/theme/publish/rollback from the
// shell) authenticate with the session cookie (package auth), which rides
// automatically — so they MUST also carry a CSRF defense. Two independent
// checks:
//
//  1. Origin must equal the request's own scheme+host (reject
//     missing/null/cross-origin).
//  2. A signed double-submit token in the X-CSRF-Token header: HMAC over
//     the authenticated email + expiry. The admin shell embeds a freshly
//     minted token; an attacker site can neither read it nor set a custom
//     header cross-origin (it would need a CORS preflight we never grant).
//
// Bearer/agent routes don't use this — they carry no ambient cookie (the
// REST API and MCP), so they're CSRF-immune by construction.
package csrf

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/pages/internal/auth"
	"github.com/ElcanoTek/pages/internal/auth/wire"
	"github.com/ElcanoTek/pages/internal/env"
)

// TTLSeconds is a shell token's life: good for a working session.
const TTLSeconds = 12 * 3600

// Now is the clock (tests move it).
var Now = time.Now

var warnOnce sync.Once

// secret is ADMIN_CSRF_SECRET, else PAGE_COOKIE_SECRET, else
// RAW_TOKEN_SECRET, read at call time.
func secret() []byte {
	s := env.String("ADMIN_CSRF_SECRET", env.String("PAGE_COOKIE_SECRET", env.String("RAW_TOKEN_SECRET", "")))
	if s == "" {
		warnOnce.Do(func() {
			slog.Warn("WARNING: no ADMIN_CSRF_SECRET/PAGE_COOKIE_SECRET set — admin CSRF tokens use an empty key (dev only).")
		})
	}
	return []byte(s)
}

func sign(body string) string {
	m := hmac.New(sha256.New, secret())
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Mint returns a CSRF token bound to the admin's email:
// base64url({"sub":email,"exp":…}) "." HMAC-SHA256.
func Mint(email string) string {
	exp := Now().Unix() + TTLSeconds
	payload := `{"sub":` + wire.JSONString(email) + `,"exp":` + strconv.FormatInt(exp, 10) + `}`
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return body + "." + sign(body)
}

// Verify checks the token's signature, expiry, and that it was minted for
// email.
func Verify(token, email string) bool {
	dot := strings.IndexByte(token, '.')
	if dot < 1 {
		return false
	}
	body, given := token[:dot], token[dot+1:]
	expect := sign(body)
	if len(given) != len(expect) || subtle.ConstantTimeCompare([]byte(given), []byte(expect)) != 1 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(body, "="))
	if err != nil {
		return false
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return false
	}
	exp, ok := p["exp"].(float64)
	if !ok || exp <= float64(Now().Unix()) {
		return false
	}
	sub, ok := p["sub"].(string)
	return ok && sub == email
}

// RequireAdminJSON is auth.RequireAdmin for API/JSON callers: 401/403 JSON
// instead of a redirect to the login page. The session is attached to the
// request context (auth.User).
func RequireAdminJSON(a *auth.Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s, err := a.Resolve(r)
			if err != nil {
				if a.ErrorHandler != nil {
					a.ErrorHandler(w, r, err)
				} else {
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
				return
			}
			if s == nil {
				wire.SendJSON(w, r, http.StatusUnauthorized, errBody{"not signed in", "no_session"})
				return
			}
			if !a.IsAdmin(s) {
				wire.SendJSON(w, r, http.StatusForbidden, errBody{"Elcano staff only", "not_admin"})
				return
			}
			next.ServeHTTP(w, auth.WithSession(r, s))
		})
	}
}

type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// RequireCsrf is the Origin check plus the signed token. It must run AFTER
// RequireAdminJSON (it reads the session that attached); a request without
// one is refused as bad_csrf.
func RequireCsrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := wire.Header(r, "Origin")
		// Expected origin = this request's own scheme+host (Caddy sets
		// X-Forwarded-*).
		proto := wire.Header(r, "X-Forwarded-Proto")
		if proto == "" {
			proto = "http"
			if r.TLS != nil {
				proto = "https"
			}
		}
		host := wire.Header(r, "X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		if origin == "" || origin != proto+"://"+host {
			wire.SendJSON(w, r, http.StatusForbidden, errBody{"cross-origin request blocked", "bad_origin"})
			return
		}
		s := auth.User(r)
		if s == nil || !Verify(wire.Header(r, "X-CSRF-Token"), s.Email) {
			wire.SendJSON(w, r, http.StatusForbidden, errBody{"missing or invalid CSRF token", "bad_csrf"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
