// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package csp holds the per-zone Content-Security-Policy, the heart of the
// trust split (PLAN.md §7). Two zones:
//
//	shell (dashboard host): our trusted /view + /admin UI. Locked down, and
//	      crucially un-frameable (anti-clickjacking) — the danger is OUR
//	      authed admin UI being framed, not the untrusted content.
//
//	raw   (content host): untrusted, agent-generated HTML. Rendered opaque via
//	      the CSP `sandbox` directive set as a real RESPONSE HEADER (so even a
//	      direct top-level navigation runs in a null origin, not just when
//	      embedded by our iframe). script/style allow 'unsafe-inline'
//	      DELIBERATELY — charts need it — which is safe ONLY because this
//	      lives on a separate, cookieless registrable domain with
//	      connect-src 'none' (no exfiltration path).
//
// ShellHeaders is the dashboard host's fixed header set; RawHeaders is
// applied by hand on the content host so we control the `sandbox` directive
// and frame-ancestors precisely.
//
// Origins come from internal/env (DASHBOARD_ORIGIN/CONTENT_ORIGIN, defaulting
// to https://<DASHBOARD_HOST|CONTENT_HOST>) and are read at call time.
package csp

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/env"
)

// Header is one response header.
type Header struct {
	Name  string
	Value string
}

// Headers is an ordered header set (the order the JavaScript object had).
type Headers []Header

// Apply sets every header on h, replacing any existing value.
func (hs Headers) Apply(h http.Header) {
	for _, x := range hs {
		h.Set(x.Name, x.Value)
	}
}

// Get returns the value of the named header (case-insensitive), or "".
func (hs Headers) Get(name string) string {
	for _, x := range hs {
		if strings.EqualFold(x.Name, name) {
			return x.Value
		}
	}
	return ""
}

// Map returns the set as a name → value map (names as written here).
func (hs Headers) Map() map[string]string {
	m := make(map[string]string, len(hs))
	for _, x := range hs {
		m[x.Name] = x.Value
	}
	return m
}

func (hs Headers) with(name, value string) Headers {
	out := make(Headers, len(hs))
	copy(out, hs)
	for i := range out {
		if strings.EqualFold(out[i].Name, name) {
			out[i].Value = value
			return out
		}
	}
	return append(out, Header{name, value})
}

func insecureDashboard() bool { return strings.HasPrefix(env.DashboardOrigin(), "http://") }

// ShellHeaders is every response on the dashboard host. It is exactly the set
// helmet 8.3.0 emitted for the options Pages gave it (its defaults plus the
// directives below), written out so there is nothing to configure or upgrade;
// the tests hold it to helmet's recorded output in both the https and the
// plain-http dev shape.
//
// It returns an error when a directive value carries ';' or ',' — a configured
// origin smuggling in a directive — exactly as helmet refused it. The server
// builds this once at startup, so a bad configuration fails the boot.
func ShellHeaders(authOrigin string) (Headers, error) {
	// Dev runs over plain HTTP (DASHBOARD_ORIGIN=http://…). Two HTTPS-oriented
	// headers BREAK that — and break it badly, because browsers treat
	// `localhost` as a secure context and will HONOR these even over HTTP:
	//   • Strict-Transport-Security (HSTS) → pins localhost to https → tunnel dies
	//   • upgrade-insecure-requests       → rewrites every request to https
	// So we drop both when the origin is http; prod (https via Caddy) keeps them.
	insecure := insecureDashboard()
	type directive struct {
		name   string
		values []string
	}
	directives := []directive{
		{"default-src", []string{"'self'"}},
		{"base-uri", []string{"'self'"}},
		{"font-src", []string{"'self'"}},
		{"form-action", []string{"'self'", authOrigin}},
		// Our authed shell must never be framed by anyone (clickjacking).
		{"frame-ancestors", []string{"'none'"}},
		{"img-src", []string{"'self'", "data:"}},
		{"object-src", []string{"'none'"}},
		{"script-src", []string{"'self'"}},
		{"script-src-attr", []string{"'none'"}},
		{"style-src", []string{"'self'", "'unsafe-inline'"}},
	}
	if !insecure {
		directives = append(directives, directive{"upgrade-insecure-requests", nil})
	}
	directives = append(directives,
		// The sandboxed content iframe is sourced from the separate content host.
		directive{"frame-src", []string{env.ContentOrigin()}},
		directive{"connect-src", []string{"'self'"}},
	)
	parts := make([]string, 0, len(directives))
	for _, d := range directives {
		for _, v := range d.values {
			// A ';' or ',' would let a configured origin smuggle in a directive.
			if strings.ContainsAny(v, ";,") {
				return nil, fmt.Errorf("Content-Security-Policy received an invalid directive value for %s", strconv.Quote(d.name))
			}
		}
		if len(d.values) > 0 {
			parts = append(parts, d.name+" "+strings.Join(d.values, " "))
		} else {
			parts = append(parts, d.name)
		}
	}
	hs := Headers{
		{"Content-Security-Policy", strings.Join(parts, ";")},
		{"Cross-Origin-Opener-Policy", "same-origin"},
		{"Cross-Origin-Resource-Policy", "same-origin"},
		{"Origin-Agent-Cluster", "?1"},
		{"Referrer-Policy", "no-referrer"},
	}
	if !insecure {
		hs = append(hs, Header{"Strict-Transport-Security", "max-age=31536000; includeSubDomains"})
	}
	hs = append(hs,
		Header{"X-Content-Type-Options", "nosniff"},
		Header{"X-DNS-Prefetch-Control", "off"},
		Header{"X-Download-Options", "noopen"},
		Header{"X-Frame-Options", "SAMEORIGIN"},
		Header{"X-Permitted-Cross-Domain-Policies", "none"},
		Header{"X-XSS-Protection", "0"},
	)
	return hs, nil
}

// ShellHeadersMiddleware is the dashboard host's middleware: the fixed set
// above on every response, and no framework banner (X-Powered-By is removed
// in case anything upstream set it). The set is built once, so a bad
// configured origin is an error here, at startup.
func ShellHeadersMiddleware(authOrigin string) (func(http.Handler) http.Handler, error) {
	headers, err := ShellHeaders(authOrigin)
	if err != nil {
		return nil, err
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			headers.Apply(h)
			h.Del("X-Powered-By")
			next.ServeHTTP(w, r)
		})
	}, nil
}

// FrameAncestors is the frame-ancestors source list for /raw: only the
// dashboard origin may embed the preview iframe. localhost and 127.0.0.1 are
// interchangeable to browsers but are DISTINCT CSP origins — so in DEV (http)
// allow both, otherwise the /admin preview iframe is blocked when the operator
// browses via the other one (a blank preview). Prod (https) keeps the single
// canonical origin unchanged.
func FrameAncestors() string {
	dash := env.DashboardOrigin()
	origins := []string{dash}
	if strings.HasPrefix(dash, "http://") {
		if u, err := url.Parse(dash); err == nil && u.Host != "" {
			host := strings.ToLower(u.Hostname())
			switch host {
			case "localhost":
				host = "127.0.0.1"
			case "127.0.0.1":
				host = "localhost"
			}
			if alt := jsOrigin(u, host); alt != dash {
				origins = append(origins, alt)
			}
		}
		// malformed origin — keep the single value
	}
	return strings.Join(origins, " ")
}

// jsOrigin is WHATWG URL#origin for an http URL with its hostname replaced:
// scheme://host[:port], the default port (80) omitted.
func jsOrigin(u *url.URL, host string) string {
	port := u.Port()
	if port != "" {
		if n, err := strconv.Atoi(port); err == nil {
			port = strconv.Itoa(n)
			if n == 80 {
				port = ""
			}
		}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return "http://" + host
}

// SandboxTokens is the `sandbox` allow-list for the content host.
//
// `allow-scripts` is what makes a dashboard a dashboard. The other two buy back
// capabilities real dashboards need and that we were silently swallowing — an
// "Export CSV" button whose click produced no file, and a "Download PDF" button
// Chromium answered with `Ignored call to print()`. Neither grants the document
// any reach it did not already have:
//   - allow-downloads — saves a blob the page already built. connect-src 'none'
//     means it cannot fetch anything to put in one, so this is not an exfil path.
//   - allow-modals — print()/alert()/confirm(). Annoyance only; no data access.
//
// DELIBERATELY ABSENT, and load-bearing:
//   - allow-same-origin — with allow-scripts this is a documented sandbox ESCAPE
//     (Chromium warns about the pair by name). It is the single token that
//     would hand agent HTML a real origin, storage and cookies. Never add it.
//   - allow-popups, allow-top-navigation, allow-forms — no dashboard has needed
//     them; each is a navigation/redirect surface. Add only on a concrete ask.
//
// Preflight reads this list so authoring guidance can never drift from what we
// actually serve.
func SandboxTokens() []string {
	return []string{"allow-scripts", "allow-downloads", "allow-modals"}
}

// ContentBaseHeaders is the content zone's floor, applied to EVERY response
// the host emits before any route runs. Routes then layer their own CSP on top
// (RawHeaders/GateHeaders) or let a more specific Cache-Control win (the
// static handler's revalidating default).
//
// Why a floor rather than per-route headers: the shapes that shipped bare were
// never decided on — /healthz, a Set-Cookie redirect, the 301 for a directory,
// the default error document. Each was missing because a middleware was
// mounted with different options, not because anyone weighed it, and an
// exception nobody chose is one nobody can find. Setting the floor once makes
// "no exceptions" true by construction instead of by enumeration.
func ContentBaseHeaders() Headers {
	return Headers{
		// Overridden by RawHeaders/GateHeaders on the routes that render. The
		// floor is the strictest thing that cannot break a response: no
		// scripts, no styles, no subresources of any kind.
		{"Content-Security-Policy", "default-src 'none'"},
		{"Referrer-Policy", "no-referrer"},
		{"X-Content-Type-Options", "nosniff"},
		{"X-Robots-Tag", "noindex, noarchive"},
		{"Cache-Control", "no-store"},
	}
}

func contentHeaders(csp string) Headers {
	return Headers{
		{"Content-Security-Policy", csp},
		{"Referrer-Policy", "no-referrer"},
		{"X-Content-Type-Options", "nosniff"},
		{"X-Robots-Tag", "noindex, noarchive"},
		{"Cache-Control", "no-store"},
	}
}

func rawCSP() string {
	return strings.Join([]string{
		"sandbox " + strings.Join(SandboxTokens(), " "),
		"default-src 'none'",
		// blob: — a document that carries its runtime compressed and boots it
		// with `import(URL.createObjectURL(new Blob([js])))`. A Bento deck is
		// exactly this. Without blob: such a page dies with "Failed to fetch
		// dynamically imported module: blob:null/…", and nothing static can warn
		// about it, because the code it would warn about is compressed. Threat
		// model: 'unsafe-inline' is already here, so anyone who can put a script
		// tag into a page can already run any code they like — blob: hands that
		// attacker nothing new. What it hands a legitimate document is module
		// import() and Workers. connect-src 'none' still holds: a blob is
		// manufactured from bytes the page already has, never fetched.
		"script-src 'self' 'unsafe-inline' blob:",
		"style-src 'self' 'unsafe-inline'",
		// blob: — charts that rasterise to an object URL before drawing.
		"img-src 'self' data: blob:",
		// data:/blob: — audio and video embedded in the document, and only that.
		// A remote media URL is a beacon that reports who opened a page and when.
		"media-src data: blob:",
		// data: — a self-contained @font-face, the only way to ship a brand face
		// when no remote origin is reachable.
		"font-src 'self' data:",
		"connect-src 'none'",
		"form-action 'none'",
		"base-uri 'none'",
		"object-src 'none'",
		"frame-ancestors " + FrameAncestors(),
	}, "; ")
}

// RawHeaders is the header set for /raw on the content host. Set as real
// response headers so they bind even on a direct navigation, not only inside
// our iframe.
func RawHeaders() Headers { return contentHeaders(rawCSP()) }

// RawEditHeaders is the header set for a deck EDIT SESSION at /raw — the one
// response on the content host that may talk back to Pages. Identical to
// RawHeaders except connect-src names this host's own origin, so the save
// channel can POST the edited deck and nothing else can be reached: not
// bento.page (the update check), not sync.bento.page (live collaboration), not
// anywhere. The sandbox is untouched, so the document is still an opaque
// origin with no cookies; the request is authorised by the edit token alone. A
// partner or client viewing the same deck never gets this header — they get
// RawHeaders, connect-src 'none', and a Save that downloads.
func RawEditHeaders() Headers {
	csp := strings.Replace(rawCSP(), "connect-src 'none'", "connect-src "+env.ContentOrigin(), 1)
	return RawHeaders().with("Content-Security-Policy", csp)
}

// GateOptions configures GateHeaders.
type GateOptions struct {
	// Framable is for the one gate page that can legitimately appear inside
	// the /admin preview iframe: the read limiter also guards /raw, and
	// refusing the frame there would answer an operator with a blank preview
	// instead of the explanation. It allows the same origins a rendered /raw
	// response does — never a wider set — and nothing that accepts input uses
	// it.
	Framable bool
}

// GateHeaders is the header set for the content host's TRUSTED gate pages
// (password form, "staff-only" notice). Unlike RawHeaders there is NO
// `sandbox` — the form must be able to submit (form-action 'self') and is our
// own first-party HTML, not agent content. Still locked down: no scripts,
// same-origin styles only (Flag tokens), un-frameable.
func GateHeaders(opts GateOptions) Headers {
	fa := "frame-ancestors 'none'"
	if opts.Framable {
		fa = "frame-ancestors " + FrameAncestors()
	}
	return contentHeaders(strings.Join([]string{
		"default-src 'none'",
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"form-action 'self'",
		"base-uri 'none'",
		"object-src 'none'",
		fa,
	}, "; "))
}
