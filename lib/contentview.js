"use strict";
// lib/contentview.js — serve the live client page DIRECTLY on the content host
// (PLAN §6b, direct-serve decision). elcano-pages.com/<slug> is the client URL.
//
// Gate (the content host owns it; SSO cookie never reaches here):
//   • disabled / unknown / nothing-published  → 404
//   • ?t=<broker token> (minted by the dashboard /view after SSO, for Elcano-only
//     pages) → set a short page-session cookie, redirect to the clean URL
//   • valid page-session cookie → render the published version (sandbox CSP)
//   • password page, no session → show the password form (trusted, NOT sandboxed)
//   • Elcano-only page (no password), no session → 403 "open it from the dashboard"
//
// Two response flavors: the rendered agent page gets the `sandbox` CSP
// (rawHeaders), the gate/form gets the trusted gateHeaders (so it can submit).

const db = require("./db");
const render = require("./render");
const rawtoken = require("./rawtoken");
const pagecookie = require("./pagecookie");
const auth = require("./auth");
const { rawHeaders, gateHeaders } = require("./csp");

const FLAG_BASE = "/assets/flag"; // same-origin on the content host

function slugFromReq(req) {
  const raw = req.params.slug;
  const s = (Array.isArray(raw) ? raw.join("/") : raw || "").toLowerCase();
  // url-safe segments only (mirrors versions.normalizeSlug); anything else → no page
  return /^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:\/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$/.test(s) ? s : null;
}
function isSecure(req) {
  return req.secure || (req.headers["x-forwarded-proto"] || "").split(",")[0] === "https";
}
function notFound(res) {
  return res.status(404).set(rawHeaders()).type("html").send(notFoundPage());
}

function renderLive(res, page) {
  res.set(rawHeaders());
  res.type("html").send(render.renderVersion({ html: page.html, render_mode: page.render_mode, override_css: page.override_css || "" }));
}

// A tasteful inline lock mark — inline so it needs no fetch (the gate CSP is
// default-src 'none'); stroke icon per the Flag icon spec (1.75–2 stroke width).
const LOCK_SVG = `<svg class="brand__icon" viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="4" y="10" width="16" height="11" rx="2"/><path d="M8 10V7a4 4 0 0 1 8 0v3"/></svg>`;

// chrome — the trusted, Flag-themed shell for every content-host gate page
// (password prompt, staff-only notice, 404). Scriptless by design: it runs under
// gateHeaders()/rawHeaders() CSP (default-src 'none', NO script-src), so all
// styling is an inline <style> (style-src 'unsafe-inline' is allowed) and the
// only assets are same-origin Flag tokens/fonts. The agent's page content is
// NEVER rendered here — this is our chrome only (invariant #2).
function chrome({ title, kicker, heading, bodyHtml = "", tone = "" }) {
  return `<!doctype html><html lang="en" data-theme="dark"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>${escapeHtml(title)}</title>
<link rel="stylesheet" href="${FLAG_BASE}/fonts/dubai-fonts.css">
<link rel="stylesheet" href="${FLAG_BASE}/tokens/design-tokens.css">
<style>
  *{box-sizing:border-box}
  body{margin:0;min-height:100vh;display:grid;place-items:center;padding:var(--space-5);
       background:radial-gradient(120% 120% at 50% 0%, var(--color-surface-1) 0%, var(--color-bg) 60%);
       color:var(--color-text-primary);font-family:"Dubai",system-ui,sans-serif;line-height:1.5}
  .card{width:100%;max-width:400px;background:var(--color-surface-1);border:1px solid var(--color-border);
        border-radius:var(--radius-lg);box-shadow:var(--shadow-lg);padding:var(--space-7);
        display:grid;gap:var(--space-4)}
  .brand{display:flex;align-items:center;gap:var(--space-2);color:var(--color-text-muted);
         font-size:var(--font-size-caption);font-weight:600;letter-spacing:.02em}
  .brand__icon{color:var(--color-accent)}
  .kicker{margin:0;font-size:var(--font-size-overline);text-transform:uppercase;letter-spacing:.08em;
          color:var(--color-text-muted)}
  h1{margin:0;font-size:var(--font-size-title)}
  .sub{margin:0;color:var(--color-text-secondary)}
  form{display:grid;gap:var(--space-4);margin:var(--space-2) 0 0}
  label{display:grid;gap:var(--space-2);color:var(--color-text-secondary);font-size:var(--font-size-caption)}
  input{width:100%;padding:var(--space-3);min-height:2.75rem;border-radius:var(--radius-md);
        border:1px solid var(--color-border-strong);background:var(--color-surface-2);
        color:var(--color-text-primary);font:inherit;transition:var(--transition-fast)}
  input:focus{outline:none;border-color:var(--color-accent)}
  button{width:100%;padding:var(--space-3);min-height:2.75rem;border:1px solid var(--color-primary);
         border-radius:var(--radius-md);background:var(--color-primary);color:var(--color-white,#fff);
         font:inherit;font-weight:600;cursor:pointer;transition:var(--transition-fast)}
  button:hover{background:var(--color-primary-hover,var(--color-primary))}
  .msg{margin:0;padding:var(--space-2) var(--space-3);border-radius:var(--radius-md);
       font-size:var(--font-size-caption)}
  .msg.error{color:var(--color-status-error-fg);background:var(--color-status-error-bg);
             border:1px solid var(--color-status-error-border)}
  .foot{margin:0;color:var(--color-text-muted);font-size:var(--font-size-caption)}
</style></head>
<body><main class="card">
  <div class="brand">${LOCK_SVG}<span>Elcano&nbsp;Pages</span></div>
  ${kicker ? `<p class="kicker">${escapeHtml(kicker)}</p>` : ""}
  <h1>${escapeHtml(heading)}</h1>
  ${bodyHtml}
</main></body></html>`;
}

// gatePage — password prompt or staff-only notice, wrapped in the shared chrome.
function gatePage({ slug, message, showForm }) {
  const msg = message ? `<p class="msg error">${escapeHtml(message)}</p>` : "";
  const body = showForm
    ? `<p class="sub">Enter the password you were given to view this page.</p>${msg}
       <form method="post" action="/${escapeAttr(slug)}">
         <label>Password
           <input type="password" name="password" autofocus autocomplete="current-password" required>
         </label>
         <button type="submit">View page</button>
       </form>`
    : `${msg}<p class="foot">If you're Elcano staff, open this page from the Pages dashboard.</p>`;
  return chrome({
    title: "Protected page · Elcano Pages",
    kicker: showForm ? "Protected" : "Restricted",
    heading: showForm ? "This page is protected" : "Staff-only page",
    bodyHtml: body,
  });
}

// notFoundPage — a styled 404 in the same chrome (was a bare one-liner).
function notFoundPage() {
  return chrome({
    title: "Not found · Elcano Pages",
    kicker: "404",
    heading: "Page not found",
    bodyHtml: `<p class="sub">This page doesn't exist, isn't published, or has been taken down.</p>`,
  });
}

// GET /<slug> — the direct-serve entry point.
async function serve(req, res) {
  const slug = slugFromReq(req);
  if (!slug) return notFound(res);
  const page = await db.getPublicPage(slug);
  if (!page || page.disabled || !page.published_version_id) return notFound(res);

  // Elcano-broker token: exchange for a short session cookie, then clean URL.
  if (req.query && typeof req.query.t === "string") {
    const claims = rawtoken.verify(req.query.t);
    if (claims && claims.purpose === "view" && Number(claims.pid) === Number(page.id)) {
      res.setHeader("Set-Cookie", pagecookie.sessionCookieHeader(page.id, { ttlSeconds: 3600, secure: isSecure(req) }));
      return res.redirect(302, "/" + slug);
    }
  }

  const cookies = auth.parseCookies(req.headers.cookie);
  if (pagecookie.verifySession(cookies[pagecookie.cookieName(page.id)], page.id)) {
    return renderLive(res, page);
  }

  if (page.password_hash) {
    return res.status(401).set(gateHeaders()).type("html").send(gatePage({ slug, showForm: true }));
  }
  // Elcano-only (no password): no SSO on this origin → route via the dashboard.
  return res.status(403).set(gateHeaders()).type("html").send(gatePage({ slug, showForm: false }));
}

// POST /<slug> — password form submission.
async function unlock(req, res) {
  const slug = slugFromReq(req);
  if (!slug) return notFound(res);
  const page = await db.getPublicPage(slug);
  if (!page || page.disabled || !page.published_version_id || !page.password_hash) return notFound(res);

  const password = (req.body && req.body.password) || "";
  if (!pagecookie.verifyPassword(password, page.password_hash)) {
    return res.status(401).set(gateHeaders()).type("html").send(gatePage({ slug, message: "Incorrect password.", showForm: true }));
  }
  res.setHeader("Set-Cookie", pagecookie.sessionCookieHeader(page.id, { secure: isSecure(req) }));
  return res.redirect(303, "/" + slug); // 303 → browser GETs the page
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function escapeAttr(s) {
  return escapeHtml(s);
}

module.exports = { serve, unlock };
