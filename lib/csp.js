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

// Headers for /raw on the content host. Set as real response headers so they
// bind even on a direct navigation, not only inside our iframe.
function rawHeaders() {
  const csp = [
    "sandbox allow-scripts",
    "default-src 'none'",
    "script-src 'self' 'unsafe-inline'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
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
function gateHeaders() {
  const csp = [
    "default-src 'none'",
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
    "form-action 'self'",
    "base-uri 'none'",
    "object-src 'none'",
    "frame-ancestors 'none'",
  ].join("; ");
  return {
    "Content-Security-Policy": csp,
    "Referrer-Policy": "no-referrer",
    "X-Content-Type-Options": "nosniff",
    "X-Robots-Tag": "noindex, noarchive",
    "Cache-Control": "no-store",
  };
}

module.exports = { shellHelmetOptions, rawHeaders, gateHeaders, CONTENT_ORIGIN, DASHBOARD_ORIGIN };
