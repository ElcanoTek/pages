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
  return { contentSecurityPolicy: { useDefaults: true, directives } };
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
    `frame-ancestors ${DASHBOARD_ORIGIN}`,
  ].join("; ");
  return {
    "Content-Security-Policy": csp,
    "Referrer-Policy": "no-referrer",
    "X-Content-Type-Options": "nosniff",
    "X-Robots-Tag": "noindex, noarchive",
    "Cache-Control": "no-store",
  };
}

module.exports = { shellHelmetOptions, rawHeaders };
