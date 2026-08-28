// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/csp.js — per-zone Content-Security-Policy, the heart of the trust split
// (see PLAN.md §7). Two zones:
//
//   shell   (dashboard host): our trusted /view + /admin UI. Locked down,
//           and crucially un-frameable (anti-clickjacking) — the danger is
//           OUR authed admin UI being framed, not the untrusted content.
//
//   raw     (content host): untrusted, agent-generated HTML. Rendered opaque
//           via the CSP `sandbox` directive set as a real RESPONSE HEADER
//           (so even a direct top-level navigation runs in a null origin,
//           not just when embedded by our iframe). script/style allow
//           'unsafe-inline' DELIBERATELY — charts need it — which is safe
//           ONLY because this lives on a separate, cookieless registrable
//           domain with connect-src 'none' (no exfiltration path).
//
// shellHelmetOptions() feeds helmet on the dashboard host; rawHeaders() is
// applied by hand on the content host so we control the `sandbox` directive
// and frame-ancestors precisely.

const helmet = require("helmet");

// Origins are derived from the configured hostnames (set per deployment in
// /etc/default/pages) so there's one source of truth — DASHBOARD_ORIGIN/
// CONTENT_ORIGIN can still override explicitly if a non-https scheme is needed.
const DASHBOARD_HOST = process.env.DASHBOARD_HOST || "pages.elcanotek.com";
const CONTENT_HOST = process.env.CONTENT_HOST || "elcano-pages.com";
// Exact scheme+host the sandboxed iframe is embedded from (no port drift).
const DASHBOARD_ORIGIN = process.env.DASHBOARD_ORIGIN || `https://${DASHBOARD_HOST}`;
const CONTENT_ORIGIN = process.env.CONTENT_ORIGIN || `https://${CONTENT_HOST}`;

function shellHelmetOptions(authOrigin) {
  // Dev runs over plain HTTP (DASHBOARD_ORIGIN=http://…). Two of helmet's
  // HTTPS-oriented defaults BREAK that — and break it badly, because browsers
  // treat `localhost` as a secure context and will HONOR these even over HTTP:
  //   • Strict-Transport-Security (HSTS) → pins localhost to https → tunnel dies
  //   • upgrade-insecure-requests       → rewrites every request to https
  // So we drop both when the origin is http; prod (https via Caddy) keeps them.
  const insecure = DASHBOARD_ORIGIN.startsWith("http://");
  const directives = {
    "default-src": ["'self'"],
    "script-src": ["'self'"],
    "style-src": ["'self'", "'unsafe-inline'"],
    "img-src": ["'self'", "data:"],
    "font-src": ["'self'"],
    // The sandboxed content iframe is sourced from the separate content host.
    "frame-src": [CONTENT_ORIGIN],
    "connect-src": ["'self'"],
    "form-action": ["'self'", authOrigin],
    // Our authed shell must never be framed by anyone (clickjacking).
    "frame-ancestors": ["'none'"],
    "base-uri": ["'self'"],
    "object-src": ["'none'"],
  };
  if (insecure) directives["upgrade-insecure-requests"] = null; // remove helmet's default
  const opts = { contentSecurityPolicy: { useDefaults: true, directives } };
  if (insecure) opts.hsts = false;
  return opts;
}

// frame-ancestors for /raw: only the dashboard origin may embed the preview
// iframe. localhost and 127.0.0.1 are interchangeable to browsers but are
// DISTINCT CSP origins — so in DEV (http) allow both, otherwise the /admin
// preview iframe is blocked when the operator browses via the other one (a
// blank preview). Prod (https) keeps the single canonical origin unchanged.
function frameAncestors() {
  const origins = [DASHBOARD_ORIGIN];
  if (DASHBOARD_ORIGIN.startsWith("http://")) {
    try {
      const u = new URL(DASHBOARD_ORIGIN);
      if (u.hostname === "localhost") u.hostname = "127.0.0.1";
      else if (u.hostname === "127.0.0.1") u.hostname = "localhost";
      if (u.origin !== DASHBOARD_ORIGIN) origins.push(u.origin);
    } catch {
      /* malformed origin — keep the single value */
    }
  }
  return origins.join(" ");
}

// The `sandbox` allow-list for the content host, as tokens.
//
// `allow-scripts` is what makes a dashboard a dashboard. The other two buy back
// capabilities real dashboards need and that we were silently swallowing —
// an "Export CSV" button whose click produced no file, and a "Download PDF"
// button Chromium answered with `Ignored call to print()`. Neither grants the
// document any reach it did not already have:
//   • allow-downloads — saves a blob the page already built. connect-src 'none'
//     means it cannot fetch anything to put in one, so this is not an exfil path.
//   • allow-modals    — print()/alert()/confirm(). Annoyance only; no data access.
//
// DELIBERATELY ABSENT, and load-bearing:
//   • allow-same-origin — with allow-scripts this is a documented sandbox ESCAPE
//     (Chromium warns about the pair by name). It is the single token that would
//     hand agent HTML a real origin, storage and cookies. Never add it.
//   • allow-popups, allow-top-navigation, allow-forms — no dashboard has needed
//     them; each is a navigation/redirect surface. Add only on a concrete ask.
//
// lib/preflight.js reads this list so authoring guidance can never drift from
// what we actually serve.
function sandboxTokens() {
  return ["allow-scripts", "allow-downloads", "allow-modals"];
}

// The content zone's floor, applied to EVERY response the host emits before any
// route runs. Routes then layer their own CSP on top (rawHeaders/gateHeaders) or
// let a more specific Cache-Control win (express.static's revalidating default).
//
// Why a floor rather than per-route headers: the shapes that shipped bare were
// never decided on. They were `/healthz`, a `Set-Cookie` redirect, the 301
// express.static emits for a directory (its `setHeaders` hook runs only on the
// file path), and the default error document. Each was missing because a
// middleware was mounted with different options, not because anyone weighed it —
// and an exception nobody chose is one nobody can find. Setting the floor once
// makes "no exceptions" true by construction instead of by enumeration.
function contentBaseHeaders() {
  return {
    // Overridden by rawHeaders()/gateHeaders() on the routes that render. The
    // floor is the strictest thing that cannot break a response: no scripts, no
    // styles, no subresources of any kind.
    "Content-Security-Policy": "default-src 'none'",
    "Referrer-Policy": "no-referrer",
    "X-Content-Type-Options": "nosniff",
    "X-Robots-Tag": "noindex, noarchive",
    "Cache-Control": "no-store",
  };
}

// Headers for /raw on the content host. Set as real response headers so they
// bind even on a direct navigation, not only inside our iframe.
function rawHeaders() {
  const csp = [
    `sandbox ${sandboxTokens().join(" ")}`,
    "default-src 'none'",
    "script-src 'self' 'unsafe-inline'",
    "style-src 'self' 'unsafe-inline'",
    // blob: — charts that rasterise to an object URL before drawing.
    "img-src 'self' data: blob:",
    // data: — a self-contained @font-face, the only way to ship a brand face
    // when no remote origin is reachable.
    "font-src 'self' data:",
    "connect-src 'none'",
    "form-action 'none'",
    "base-uri 'none'",
    "object-src 'none'",
    `frame-ancestors ${frameAncestors()}`,
  ].join("; ");
  return {
    "Content-Security-Policy": csp,
    "Referrer-Policy": "no-referrer",
    "X-Content-Type-Options": "nosniff",
    "X-Robots-Tag": "noindex, noarchive",
    "Cache-Control": "no-store",
  };
}

// Headers for the content host's TRUSTED gate pages (password form, "staff-only"
// notice). Unlike rawHeaders() there is NO `sandbox` — the form must be able to
// submit (form-action 'self') and is our own first-party HTML, not agent
// content. Still locked down: no scripts, same-origin styles only (Flag tokens),
// un-frameable.
//
// `framable: true` is for the one gate page that can legitimately appear inside
// the /admin preview iframe: the read limiter also guards /raw, and refusing the
// frame there would answer an operator with a blank preview instead of the
// explanation. It allows the same origins a rendered /raw response does — never
// a wider set — and nothing that accepts input uses it.
function gateHeaders({ framable = false } = {}) {
  const csp = [
    "default-src 'none'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
    "form-action 'self'",
    "base-uri 'none'",
    "object-src 'none'",
    framable ? `frame-ancestors ${frameAncestors()}` : "frame-ancestors 'none'",
  ].join("; ");
  return {
    "Content-Security-Policy": csp,
    "Referrer-Policy": "no-referrer",
    "X-Content-Type-Options": "nosniff",
    "X-Robots-Tag": "noindex, noarchive",
    "Cache-Control": "no-store",
  };
}

module.exports = {
  shellHelmetOptions,
  contentBaseHeaders,
  rawHeaders,
  gateHeaders,
  sandboxTokens,
  frameAncestors,
  CONTENT_ORIGIN,
  DASHBOARD_ORIGIN,
};
