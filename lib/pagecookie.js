"use strict";
// lib/pagecookie.js — the per-page client session for the CONTENT host (PLAN
// §6b, amended for direct-serve). Two pieces:
//
//   1. Password hashing — scrypt (Node built-in; no native dep). The stored
//      `pages.password_hash` is `scrypt$N$r$p$saltB64$hashB64`, self-describing
//      so params can evolve. (PLAN said "bcrypt"; scrypt is an equivalent
//      memory-hard KDF and keeps the zero-extra-deps property — same column.)
//
//   2. Page-session cookie — after a client enters the right password the
//      content host sets a signed, HttpOnly cookie scoped to the CONTENT origin
//      (its own cookie jar — NOT the SSO cookie). Signed with PAGE_COOKIE_SECRET,
//      bound to the page id, ~30d. The content origin is sandboxed (opaque) so
//      page JS can't read it; HttpOnly belt-and-suspenders.
//
// The session cookie is keyed by page id (slugs may contain '/', illegal in a
// cookie name): cookie name = `pgs<id>`.

const crypto = require("node:crypto");

const SECRET = process.env.PAGE_COOKIE_SECRET || "";
const SCRYPT = { N: 16384, r: 8, p: 1, keylen: 32 };
const DEFAULT_TTL_DAYS = 30;

if (!SECRET) {
  console.warn("WARNING: PAGE_COOKIE_SECRET is unset — page-session cookies use an empty key (dev only).");
}

// ── password hashing (scrypt) ────────────────────────────────────────────────

function hashPassword(plain) {
  const salt = crypto.randomBytes(16);
  const hash = crypto.scryptSync(String(plain), salt, SCRYPT.keylen, { N: SCRYPT.N, r: SCRYPT.r, p: SCRYPT.p });
  return `scrypt$${SCRYPT.N}$${SCRYPT.r}$${SCRYPT.p}$${salt.toString("base64")}$${hash.toString("base64")}`;
}

function verifyPassword(plain, stored) {
  if (typeof stored !== "string" || !stored.startsWith("scrypt$")) return false;
  const [, N, r, p, saltB64, hashB64] = stored.split("$");
  let salt, expected;
  try {
    salt = Buffer.from(saltB64, "base64");
    expected = Buffer.from(hashB64, "base64");
  } catch {
    return false;
  }
  const got = crypto.scryptSync(String(plain), salt, expected.length, { N: Number(N), r: Number(r), p: Number(p) });
  return got.length === expected.length && crypto.timingSafeEqual(got, expected);
}

// ── page-session cookie ──────────────────────────────────────────────────────

function cookieName(pageId) {
  return `pgs${pageId}`;
}

function sign(body) {
  return crypto.createHmac("sha256", SECRET).update(body).digest("base64url");
}

// mint a session token for a page. ttlSeconds overrides the default (used for
// the short Elcano-broker session).
function mintSession(pageId, ttlSeconds = DEFAULT_TTL_DAYS * 86400) {
  const body = Buffer.from(JSON.stringify({ pid: Number(pageId), exp: Math.floor(Date.now() / 1000) + ttlSeconds })).toString("base64url");
  return `${body}.${sign(body)}`;
}

function verifySession(token, pageId) {
  if (typeof token !== "string") return false;
  const dot = token.indexOf(".");
  if (dot < 1) return false;
  const body = token.slice(0, dot);
  const a = Buffer.from(token.slice(dot + 1));
  const b = Buffer.from(sign(body));
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) return false;
  let p;
  try {
    p = JSON.parse(Buffer.from(body, "base64url").toString("utf8"));
  } catch {
    return false;
  }
  if (typeof p.exp !== "number" || p.exp <= Math.floor(Date.now() / 1000)) return false;
  return Number(p.pid) === Number(pageId);
}

// Set-Cookie header value for a page session (Secure unless dev-insecure).
function sessionCookieHeader(pageId, { ttlSeconds, secure = true } = {}) {
  const token = mintSession(pageId, ttlSeconds);
  const maxAge = ttlSeconds || DEFAULT_TTL_DAYS * 86400;
  return `${cookieName(pageId)}=${token}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${maxAge}${secure ? "; Secure" : ""}`;
}

module.exports = { hashPassword, verifyPassword, cookieName, mintSession, verifySession, sessionCookieHeader };
