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

const AUTH_LOGIN_URL = (process.env.AUTH_LOGIN_URL || "https://auth.elcanotek.com").replace(/\/$/, "");
const AUTH_ORIGIN = new URL(AUTH_LOGIN_URL).origin;

if (!AUTH_PUBLIC_KEY) {
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
      acc[key] = decodeURIComponent(value);
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
// whole authorization model for /admin — no roles table in v1.
function isElcanoAdmin(session) {
  if (!session || typeof session.email !== "string") return false;
  const at = session.email.lastIndexOf("@");
  if (at < 0) return false;
  return session.email.slice(at + 1).toLowerCase() === ADMIN_DOMAIN;
}

function loginRedirectURL(req) {
  const protocol = req.secure ? "https" : "http";
  const host = req.headers.host || "localhost";
  const returnTo = `${protocol}://${host}${req.originalUrl}`;
  return `${AUTH_LOGIN_URL}/?return_to=${encodeURIComponent(returnTo)}`;
}

// requireAuth — any valid Elcano session may pass (attaches req.user).
function requireAuth(req, res, next) {
  const session = currentSession(req);
  if (session) {
    req.user = session;
    return next();
  }
  return res.redirect(loginRedirectURL(req));
}

// requireAdmin — must be a valid session AND in the admin domain. Not
// logged in → bounce to auth; logged in but wrong tenant → 403.
function requireAdmin(req, res, next) {
  const session = currentSession(req);
  if (session && isElcanoAdmin(session)) {
    req.user = session;
    return next();
  }
  if (!session) return res.redirect(loginRedirectURL(req));
  return res.status(403).send("Forbidden — Elcano staff only.");
}

module.exports = {
  AUTH_LOGIN_URL,
  AUTH_ORIGIN,
  parseCookies,
  verifyElcanoSession,
  currentSession,
  isElcanoAdmin,
  loginRedirectURL,
  requireAuth,
  requireAdmin,
};
