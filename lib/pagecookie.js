// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
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
const { promisify } = require("node:util");

// Async scrypt: runs in the libuv threadpool instead of blocking the event
// loop ~30-80ms per call. The sync variant on the PUBLIC password path was an
// event-loop-DoS surface (the per-IP rate limit doesn't bound aggregate load
// across IPs), and inside setPassword's row-lock txn it stretched the lock.
const scrypt = promisify(crypto.scrypt);

const SECRET = process.env.PAGE_COOKIE_SECRET || "";
const SCRYPT = { N: 16384, r: 8, p: 1, keylen: 32 };
const DEFAULT_TTL_DAYS = 30;
// Cheap input bound before the (deliberately expensive) KDF runs — scrypt cost
// doesn't scale with input length, but there's no reason to hash megabytes.
const MAX_PASSWORD_LENGTH = 512;

if (!SECRET) {
  // Fail CLOSED in production: an empty HMAC key lets anyone compute a valid
  // pgs<id> cookie and walk past the per-page password gate on confidential
  // client pages. Only tolerate the empty key in dev/test.
  if (process.env.NODE_ENV === "production") {
    throw new Error("PAGE_COOKIE_SECRET is required in production (an empty key lets anyone forge page-session cookies and bypass the password gate).");
  }
  console.warn("WARNING: PAGE_COOKIE_SECRET is unset — page-session cookies use an empty key (dev only).");
}

// ── password hashing (scrypt, async) ─────────────────────────────────────────
// Both functions are async (threadpool); the stored format is unchanged, so
// every existing hash keeps verifying.

async function hashPassword(plain) {
  const s = String(plain).slice(0, MAX_PASSWORD_LENGTH);
  const salt = crypto.randomBytes(16);
  const hash = await scrypt(s, salt, SCRYPT.keylen, { N: SCRYPT.N, r: SCRYPT.r, p: SCRYPT.p });
  return `scrypt$${SCRYPT.N}$${SCRYPT.r}$${SCRYPT.p}$${salt.toString("base64")}$${hash.toString("base64")}`;
}

async function verifyPassword(plain, stored) {
  if (typeof stored !== "string" || !stored.startsWith("scrypt$")) return false;
  const [, N, r, p, saltB64, hashB64] = stored.split("$");
  let salt, expected;
  try {
    salt = Buffer.from(saltB64, "base64");
    expected = Buffer.from(hashB64, "base64");
  } catch {
    return false;
  }
  let got;
  try {
    got = await scrypt(String(plain).slice(0, MAX_PASSWORD_LENGTH), salt, expected.length, { N: Number(N), r: Number(r), p: Number(p) });
  } catch {
    return false; // corrupt params in a stored hash → treat as no-match, not a 500
  }
  return got.length === expected.length && crypto.timingSafeEqual(got, expected);
}

// ── page-session cookie ──────────────────────────────────────────────────────

function cookieName(pageId) {
  return `pgs${pageId}`;
}

function sign(body) {
  return crypto.createHmac("sha256", SECRET).update(body).digest("base64url");
}

// credentialDigest — a short fingerprint of the page's CURRENT credential
// state, embedded in every session at mint time and re-checked on verify.
// Rotating the password changes password_hash (fresh salt even for the same
// password) ⇒ digest mismatch ⇒ every prior session is invalid. NULL (the
// Elcano-only/broker state) digests the empty string, so setting a first
// password — or an admin clearing one — also invalidates prior sessions,
// deliberately. Keyed with the cookie secret so the stored hash isn't
// recoverable-by-dictionary from a leaked cookie payload.
function credentialDigest(passwordHash) {
  return crypto.createHmac("sha256", SECRET).update(`cred:${passwordHash || ""}`).digest("hex").slice(0, 16);
}

// mint a session token for a page, bound to the page id AND the credential
// state that authorized it. ttlSeconds overrides the default (used for the
// short Elcano-broker session).
function mintSession(pageId, ttlSeconds = DEFAULT_TTL_DAYS * 86400, passwordHash = null) {
  const body = Buffer.from(
    JSON.stringify({
      pid: Number(pageId),
      exp: Math.floor(Date.now() / 1000) + ttlSeconds,
      cd: credentialDigest(passwordHash),
    })
  ).toString("base64url");
  return `${body}.${sign(body)}`;
}

function verifySession(token, pageId, passwordHash = null) {
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
  if (Number(p.pid) !== Number(pageId)) return false;
  // Credential binding: sessions minted before this field existed (no `cd`)
  // are simply invalid — the viewer re-authenticates once. Fail closed.
  return typeof p.cd === "string" && p.cd === credentialDigest(passwordHash);
}

// Set-Cookie header value for a page session (Secure unless dev-insecure).
// `passwordHash` must be the page's CURRENT password_hash (null for
// Elcano-only pages) so the session dies with the credential that minted it.
function sessionCookieHeader(pageId, { ttlSeconds, secure = true, passwordHash = null } = {}) {
  const token = mintSession(pageId, ttlSeconds, passwordHash);
  const maxAge = ttlSeconds || DEFAULT_TTL_DAYS * 86400;
  return `${cookieName(pageId)}=${token}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${maxAge}${secure ? "; Secure" : ""}`;
}

// ── portal-session cookie ────────────────────────────────────────────────────
// A portal session is a DIFFERENT credential from a page session: it proves
// knowledge of one PORTAL's shared password, and it authorizes whatever that
// portal contains at the moment of each request. Cookie name is `pgp<portal_id>`.
//
// Three deliberate choices, each of which was the alternative to something worse:
//
//   * The two token types are domain-separated in the MAC input (`portal.` ‖ body)
//     and in the credential digest (`cred:portal:`) — NOT discriminated by a field
//     inside the payload. A discriminator would have to appear in the PAGE token
//     too to mean anything, and verifySession fails closed on anything
//     unexpected, so shipping that would have logged out every live client on
//     deploy. Domain separation gives the new type the same guarantee and leaves
//     the old one byte-identical.
//   * The payload names the portal `poid`, not `pid`, so a cross-type replay
//     still fails on the field name alone if the domain separation were ever
//     removed. Belt and braces on the one property that keeps a page credential
//     from being spent as a portal credential.
//   * The verify/mint bodies below duplicate their page equivalents rather than
//     sharing a parameterised helper. That is on purpose: the page session is the
//     credential every live client already holds, and no refactor of it is worth
//     the blast radius of a mistake.
//
// Path=/ is load-bearing here, not a copy-paste: the cookie has to be sent on
// requests for member pages at arbitrary (nested) slugs. It is also why portals
// IMPROVE the cookie-header exhaustion story — one cookie per portal instead of
// one per page, where ~130 accumulated `pgs<id>` cookies make Node answer 431
// before Express (and therefore the content zone's header floor) ever runs.

function portalCookieName(portalId) {
  return `pgp${portalId}`;
}

function signPortal(body) {
  return crypto.createHmac("sha256", SECRET).update(`portal.${body}`).digest("base64url");
}

// Rotating a portal password invalidates every session for it, exactly as
// credentialDigest does per page. `page_portals.password_hash` is NOT NULL and
// non-empty by constraint, so the empty-string branch is unreachable from a real
// row — it exists so that a caller who passes null can never mint a token that
// verifies against the digest of "no credential at all".
function portalCredentialDigest(passwordHash) {
  return crypto.createHmac("sha256", SECRET).update(`cred:portal:${passwordHash || ""}`).digest("hex").slice(0, 16);
}

function mintPortalSession(portalId, ttlSeconds = DEFAULT_TTL_DAYS * 86400, passwordHash = null) {
  const body = Buffer.from(
    JSON.stringify({
      poid: Number(portalId),
      exp: Math.floor(Date.now() / 1000) + ttlSeconds,
      cd: portalCredentialDigest(passwordHash),
    })
  ).toString("base64url");
  return `${body}.${signPortal(body)}`;
}

function verifyPortalSession(token, portalId, passwordHash = null) {
  if (typeof token !== "string") return false;
  const dot = token.indexOf(".");
  if (dot < 1) return false;
  const body = token.slice(0, dot);
  const a = Buffer.from(token.slice(dot + 1));
  const b = Buffer.from(signPortal(body));
  if (a.length !== b.length || !crypto.timingSafeEqual(a, b)) return false;
  let p;
  try {
    p = JSON.parse(Buffer.from(body, "base64url").toString("utf8"));
  } catch {
    return false;
  }
  if (typeof p.exp !== "number" || p.exp <= Math.floor(Date.now() / 1000)) return false;
  if (Number(p.poid) !== Number(portalId)) return false;
  return typeof p.cd === "string" && p.cd === portalCredentialDigest(passwordHash);
}

function portalSessionCookieHeader(portalId, { ttlSeconds, secure = true, passwordHash = null } = {}) {
  const token = mintPortalSession(portalId, ttlSeconds, passwordHash);
  const maxAge = ttlSeconds || DEFAULT_TTL_DAYS * 86400;
  return `${portalCookieName(portalId)}=${token}; Path=/; HttpOnly; SameSite=Lax; Max-Age=${maxAge}${secure ? "; Secure" : ""}`;
}

// clearedCookieHeader — the exact inverse of the two minting helpers above.
//
// A sign-out is only real if the browser drops the cookie, and a browser only
// drops one when the clearing header matches the original on name, Path and
// Domain. So this is written from the same pieces rather than as a second
// hand-rolled string: get one attribute wrong and the cookie survives while the
// page cheerfully says the reader is signed out — the worst possible failure on
// the shared machine this exists for.
function clearedCookieHeader(name, { secure = true } = {}) {
  return `${name}=; Path=/; HttpOnly; SameSite=Lax; Max-Age=0${secure ? "; Secure" : ""}`;
}

function clearedPortalSessionCookieHeader(portalId, options) {
  return clearedCookieHeader(portalCookieName(portalId), options);
}

function clearedSessionCookieHeader(pageId, options) {
  return clearedCookieHeader(cookieName(pageId), options);
}

module.exports = {
  hashPassword,
  verifyPassword,
  cookieName,
  mintSession,
  verifySession,
  sessionCookieHeader,
  credentialDigest,
  portalCookieName,
  mintPortalSession,
  verifyPortalSession,
  portalSessionCookieHeader,
  portalCredentialDigest,
  clearedPortalSessionCookieHeader,
  clearedSessionCookieHeader,
  // The portal index tells the reader how long their session lasts, and must
  // read the number rather than restate it.
  DEFAULT_TTL_DAYS,
};
