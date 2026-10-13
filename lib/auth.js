// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/auth.js — Elcano unified auth, copied verbatim from `home/server.js`
// (Pattern B: native Ed25519 cookie verification) and extended with the
// admin gate Pages needs. See home/server.js for the canonical commentary.
//
// Pages verifies the `elcano_auth` cookie minted by auth.elcanotek.com using
// only the auth service's PUBLIC key (AUTH_SIGNING_PUBKEY): it can verify a
// token but never mint one, so the value is safe to distribute and a leak
// here cannot forge sessions. An Elcano user (email @elcanotek.com) is an
// admin; that is the entire authorization model for /admin.
//
// PAGES_AUTH_MODE selects the sign-in model (default "elcano", unchanged):
//
//   elcano   the shared magic-link cookie above (Elcano's own deployment).
//   central  the application handoff of a password-mode Auth deployment
//            (lib/central-auth.js): Pages runs its own /auth/login and
//            /auth/callback, keeps a local access list that Auth's signed
//            events maintain, and mints a host-only Pages session. Everyone
//            on that access list is an admin — Pages has one human role.
//
// Routes resolve the session through resolveSession(req) (async, cached on
// the request) so both modes share requireAuth/requireAdmin.

const crypto = require("node:crypto");

const AUTH_SIGNING_PUBKEY = process.env.AUTH_SIGNING_PUBKEY || "";
const AUTH_COOKIE_NAME = process.env.AUTH_COOKIE_NAME || "elcano_auth";
const ADMIN_DOMAIN = (process.env.ADMIN_EMAIL_DOMAIN || "elcanotek.com").toLowerCase();

// Build a verify-only KeyObject from the raw 32-byte Ed25519 public key
// (base64-std in env). null if unset/malformed, which makes verification
// fail closed (every request bounces to the auth login).
const AUTH_PUBLIC_KEY = (() => {
  if (!AUTH_SIGNING_PUBKEY) return null;
  try {
    const raw = Buffer.from(AUTH_SIGNING_PUBKEY, "base64");
    if (raw.length !== 32) throw new Error(`expected 32 bytes, got ${raw.length}`);
    return crypto.createPublicKey({
      key: { kty: "OKP", crv: "Ed25519", x: raw.toString("base64url") },
      format: "jwk",
    });
  } catch (err) {
    console.warn(`WARNING: AUTH_SIGNING_PUBKEY is malformed (${err.message}); all auth will fail.`);
    return null;
  }
})();

const MODE = (process.env.PAGES_AUTH_MODE || "elcano").trim().toLowerCase();
if (MODE !== "elcano" && MODE !== "central") {
  throw new Error('PAGES_AUTH_MODE must be "elcano" or "central"');
}
// Central mode validates its whole configuration here, at require time, so a
// misconfigured box fails at start instead of on the first sign-in.
const central = MODE === "central" ? centralRuntime() : null;

function centralRuntime() {
  const centralAuth = require("./central-auth");
  const config = centralAuth.loadConfig();
  // One key set verifies both the id_token at sign-in and Auth's back-channel
  // tokens, so a rotation reaches both at once.
  const keys = new centralAuth.KeyResolver(config.issuer, config.staticKeys);
  return {
    config,
    client: new centralAuth.Client(config, { keys }),
    keys,
    // __Host- pins the cookie to this exact host over HTTPS with Path=/, so
    // nothing on a sibling subdomain can set or shadow it.
    sessionCookie: config.cookieSecure ? "__Host-pages_session" : "pages_session",
    loginCookie: config.cookieSecure ? "__Host-pages_login" : "pages_login",
  };
}

const AUTH_LOGIN_URL = central
  ? central.config.issuer
  : (process.env.AUTH_LOGIN_URL || "https://auth.elcanotek.com").replace(/\/$/, "");
const AUTH_ORIGIN = new URL(AUTH_LOGIN_URL).origin;

if (!AUTH_PUBLIC_KEY && !central) {
  console.warn(
    "WARNING: AUTH_SIGNING_PUBKEY is unset or invalid — every admin request will be " +
      "redirected to auth. Set it to the auth service's AUTH_SIGNING_PUBKEY (public half)."
  );
}

function parseCookies(cookieHeader) {
  if (!cookieHeader) return {};
  return cookieHeader
    .split(";")
    .map((entry) => entry.trim())
    .filter(Boolean)
    .reduce((acc, pair) => {
      const equalsIndex = pair.indexOf("=");
      if (equalsIndex < 1) return acc;
      const key = pair.slice(0, equalsIndex).trim();
      const value = pair.slice(equalsIndex + 1).trim();
      // decodeURIComponent throws URIError on malformed percent-encoding
      // (e.g. a bare '%'). Anything on the domain can set such a cookie, and
      // an unhandled throw here would 500 EVERY cookie-reading route for that
      // browser. Fall back to the raw value: downstream verification treats
      // an undecodable cookie as opaque/invalid, which fails closed.
      // (home/server.js, where this was copied from, has the same bug.)
      try {
        acc[key] = decodeURIComponent(value);
      } catch {
        acc[key] = value;
      }
      return acc;
    }, {});
}

// verifyElcanoSession mirrors auth/internal/token/token.go. Token format:
// base64url(payloadJSON) + "." + base64url(ed25519Signature); the signature
// covers the base64url-encoded body string, NOT the raw JSON. Returns the
// session on success or null for any failure (treated as "logged out").
function verifyElcanoSession(rawToken) {
  if (!AUTH_PUBLIC_KEY || !rawToken) return null;

  const dot = rawToken.indexOf(".");
  if (dot < 1 || dot === rawToken.length - 1) return null;

  const body = rawToken.slice(0, dot);
  const providedSig = rawToken.slice(dot + 1);

  let givenSig;
  try {
    givenSig = Buffer.from(providedSig, "base64url");
  } catch {
    return null;
  }

  let sigOk = false;
  try {
    sigOk = crypto.verify(null, Buffer.from(body), AUTH_PUBLIC_KEY, givenSig);
  } catch {
    return null;
  }
  if (!sigOk) return null;

  let payload;
  try {
    payload = JSON.parse(Buffer.from(body, "base64url").toString("utf8"));
  } catch {
    return null;
  }

  if (!payload || typeof payload.email !== "string" || payload.email === "") return null;
  if (typeof payload.exp !== "number" || payload.exp <= Math.floor(Date.now() / 1000)) return null;

  return { email: payload.email, tenant: payload.tenant || "", exp: payload.exp };
}

function currentSession(req) {
  const cookies = parseCookies(req.headers.cookie);
  return verifyElcanoSession(cookies[AUTH_COOKIE_NAME]);
}

// An Elcano staffer (email in the admin domain) is an admin. This is the
// whole authorization model for /admin in elcano mode — no roles table.
function isElcanoAdmin(session) {
  if (!session || typeof session.email !== "string") return false;
  const at = session.email.lastIndexOf("@");
  if (at < 0) return false;
  return session.email.slice(at + 1).toLowerCase() === ADMIN_DOMAIN;
}

// resolveSession — the signed-in identity for this request, or null. Cached
// on the request, so a route and its middleware share one lookup.
//   elcano:  { email, tenant, exp } from the shared cookie.
//   central: { email, tenant: "", subject } from a live Pages session whose
//            email is still enabled on the access list.
async function resolveSession(req) {
  if (req._pagesSession !== undefined) return req._pagesSession;
  let session = null;
  if (central) {
    const token = parseCookies(req.headers.cookie)[central.sessionCookie];
    const identity = token ? await require("./central-auth").store.identity(token) : null;
    if (identity) session = { email: identity.email, tenant: "", subject: identity.subject };
  } else {
    session = currentSession(req);
  }
  req._pagesSession = session;
  return session;
}

// isAdmin — may this session use /admin, /view and the admin API? In central
// mode the access list was checked when the session resolved, so any live
// session is an admin.
function isAdmin(session) {
  if (!session) return false;
  return central ? true : isElcanoAdmin(session);
}

function loginRedirectURL(req) {
  if (central) {
    const target = String(req.originalUrl || "/admin");
    return `/auth/login?next=${encodeURIComponent(target.length > 2048 ? "/admin" : target)}`;
  }
  // Caddy terminates TLS and proxies to us over plain HTTP, so req.secure can
  // read false on a genuinely https request. Trust the X-Forwarded-Proto Caddy
  // sets (same check as contentview.isSecure). Without this the return_to goes
  // out as http://pages.elcanotek.com/...; the auth service then bounces the
  // user to its home default instead of back here, so staff land on
  // home.elcanotek.com after sign-in instead of the page they asked for.
  const xfProto = String(req.headers["x-forwarded-proto"] || "").split(",")[0].trim().toLowerCase();
  const protocol = req.secure || xfProto === "https" ? "https" : "http";
  const host = req.headers.host || "localhost";
  const returnTo = `${protocol}://${host}${req.originalUrl}`;
  return `${AUTH_LOGIN_URL}/?return_to=${encodeURIComponent(returnTo)}`;
}

// requireAuth — any valid session may pass (attaches req.user).
async function requireAuth(req, res, next) {
  const session = await resolveSession(req);
  if (session) {
    req.user = session;
    return next();
  }
  return res.redirect(loginRedirectURL(req));
}

// requireAdmin — must be a valid session AND an admin. Not logged in →
// sign-in; logged in but not an admin → 403.
async function requireAdmin(req, res, next) {
  const session = await resolveSession(req);
  if (session && isAdmin(session)) {
    req.user = session;
    return next();
  }
  if (!session) return res.redirect(loginRedirectURL(req));
  return res.status(403).send("Forbidden — Elcano staff only.");
}

module.exports = {
  MODE,
  central,
  AUTH_LOGIN_URL,
  AUTH_ORIGIN,
  parseCookies,
  verifyElcanoSession,
  currentSession,
  resolveSession,
  isAdmin,
  isElcanoAdmin,
  loginRedirectURL,
  requireAuth,
  requireAdmin,
};
