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
  return res.status(404).set(rawHeaders()).type("html").send("<!doctype html><title>pages</title><p>Not found.</p>");
}

function renderLive(res, page) {
  res.set(rawHeaders());
  res.type("html").send(render.renderVersion({ html: page.html, render_mode: page.render_mode, override_css: page.override_css || "" }));
}

// gatePage — the trusted, Flag-styled password form (or a notice). Posts the
// password back to the SAME url; no scripts.
function gatePage({ slug, message, showForm }) {
  const msg = message ? `<p class="msg">${escapeHtml(message)}</p>` : "";
  const form = showForm
    ? `<form method="post" action="/${escapeAttr(slug)}">
         <label>Password<input type="password" name="password" autofocus autocomplete="current-password"></label>
         <button type="submit">View page</button>
       </form>`
    : "";
  return `<!doctype html><html lang="en" data-theme="dark"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Protected page</title>
<link rel="stylesheet" href="${FLAG_BASE}/fonts/dubai-fonts.css">
<link rel="stylesheet" href="${FLAG_BASE}/tokens/design-tokens.css">
<style>
  body{margin:0;min-height:100vh;display:grid;place-items:center;background:var(--color-bg);
       color:var(--color-text-primary);font-family:"Dubai",system-ui,sans-serif}
  .box{background:var(--color-surface-1);border:1px solid var(--color-border);border-radius:var(--radius-lg);
       padding:var(--space-7);max-width:360px;width:90%}
  h1{font-size:var(--font-size-subtitle);margin:0 0 var(--space-4)}
  label{display:block;margin-bottom:var(--space-4);color:var(--color-text-secondary)}
  input{display:block;width:100%;margin-top:var(--space-2);padding:var(--space-3);min-height:2.5rem;
        border-radius:var(--radius-md);border:1px solid var(--color-border-strong);
        background:var(--color-surface-2);color:var(--color-text-primary);font:inherit}
  button{width:100%;padding:var(--space-3);border:0;border-radius:var(--radius-md);
         background:var(--color-primary);color:#fff;font:inherit;cursor:pointer}
  .msg{color:var(--color-status-error-fg);margin:0 0 var(--space-4)}
</style></head>
<body><div class="box"><h1>This page is protected</h1>${msg}${form}</div></body></html>`;
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
  return res
    .status(403)
    .set(gateHeaders())
    .type("html")
    .send(gatePage({ slug, message: "This page is staff-only — open it from the Pages dashboard.", showForm: false }));
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
