// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/csrf.js — admin-cookie mutation protection (PLAN §7). Admin actions
// (approve/reject/disable/approval-toggle/theme/publish/rollback from the shell)
// authenticate with the Elcano SSO cookie, which rides automatically — so they
// MUST also carry a CSRF defense. We use two independent checks:
//
//   1. Origin must equal the dashboard host (reject missing/null/cross-origin).
//   2. A signed double-submit token in the `X-CSRF-Token` header: HMAC over the
//      authenticated email + expiry. The admin shell embeds a freshly-minted
//      token; an attacker site can neither read it nor set a custom header
//      cross-origin (it would need a CORS preflight we never grant).
//
// Bearer/agent routes don't use this — they carry no ambient cookie (lib/api.js,
// lib/mcp.js), so they're CSRF-immune by construction.

const crypto = require("node:crypto");
const auth = require("./auth");

const SECRET = process.env.ADMIN_CSRF_SECRET || process.env.PAGE_COOKIE_SECRET || process.env.RAW_TOKEN_SECRET || "";
const TTL_SECONDS = 12 * 3600; // a shell token is good for a working session

if (!SECRET) {
  console.warn("WARNING: no ADMIN_CSRF_SECRET/PAGE_COOKIE_SECRET set — admin CSRF tokens use an empty key (dev only).");
}

function sign(body) {
  return crypto.createHmac("sha256", SECRET).update(body).digest("base64url");
}

// mint a CSRF token bound to the admin's email.
function mint(email) {
  const payload = { sub: email, exp: Math.floor(Date.now() / 1000) + TTL_SECONDS };
  const body = Buffer.from(JSON.stringify(payload)).toString("base64url");
  return `${body}.${sign(body)}`;
}

// verify token signature, expiry, and that it was minted for `email`.
function verify(token, email) {
  if (typeof token !== "string") return false;
  const dot = token.indexOf(".");
  if (dot < 1) return false;
  const body = token.slice(0, dot);
  const givenSig = token.slice(dot + 1);
  const expectSig = sign(body);
  // constant-time compare
  const a = Buffer.from(givenSig);
  const b = Buffer.from(expectSig);
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) return false;
  let p;
  try {
    p = JSON.parse(Buffer.from(body, "base64url").toString("utf8"));
  } catch {
    return false;
  }
  if (typeof p.exp !== "number" || p.exp <= Math.floor(Date.now() / 1000)) return false;
  return p.sub === email;
}

// requireAdminJSON — like auth.requireAdmin but for API/JSON callers: returns
// 401/403 JSON instead of redirecting to the login page.
function requireAdminJSON(req, res, next) {
  const session = auth.currentSession(req);
  if (!session) return res.status(401).json({ error: "not signed in", code: "no_session" });
  if (!auth.isElcanoAdmin(session)) return res.status(403).json({ error: "Elcano staff only", code: "not_admin" });
  req.user = session;
  next();
}

// requireCsrf — Origin check + signed token. Must run AFTER requireAdminJSON.
function requireCsrf(req, res, next) {
  const origin = req.get("origin");
  // Expected origin = this request's own scheme+host (Caddy sets X-Forwarded-*).
  const proto = req.get("x-forwarded-proto") || (req.secure ? "https" : "http");
  const host = req.get("x-forwarded-host") || req.get("host") || "";
  const expected = `${proto}://${host}`;
  if (!origin || origin !== expected) {
    return res.status(403).json({ error: "cross-origin request blocked", code: "bad_origin" });
  }
  if (!verify(req.get("x-csrf-token"), req.user.email)) {
    return res.status(403).json({ error: "missing or invalid CSRF token", code: "bad_csrf" });
  }
  next();
}

module.exports = { mint, verify, requireAdminJSON, requireCsrf };
