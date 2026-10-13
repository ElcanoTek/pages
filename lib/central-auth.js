// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/central-auth.js — sign-in through a password-mode Auth deployment
// (PAGES_AUTH_MODE=central). Mirrors the contract every Elcano application
// implements against Auth's application handoff (auth/docs/INTEGRATION.md,
// "Password mode: the application contract"), ported from Lens/Explorer:
//
//   1. /authorize with S256 PKCE, state and nonce;
//   2. /token with client_secret_basic, claims checked (iss, aud, exp, nonce);
//   3. a local access list (who may use THIS Pages), maintained by Auth's
//      signed access events and by `pages access` for bootstrap;
//   4. an opaque Pages session, stored only as its SHA-256 hash;
//   5. sign-out both ways: our logout hands the browser to Auth's RP logout,
//      and Auth's back-channel logout ends every session for the subject.
//
// Session conventions are the owner-decided ones shared by every Elcano
// service: 24 h absolute, 12 h idle, touched at most once a minute, the idle
// clock never extending past the absolute one. Auth's central session is the
// long one; expiry here costs a silent redirect while that session is live.
//
// Nothing in this file mints a credential another host accepts: the session
// cookie is host-only on the dashboard host, and the content host never reads
// it (server.js routes the two hosts to different apps).

const crypto = require("node:crypto");

const db = require("./db");

const BACKCHANNEL_LOGOUT_EVENT = "http://schemas.openid.net/event/backchannel-logout";
const APPLICATION_ACCESS_EVENT = "urn:elcanotek:event:application-access";
const CLOCK_SKEW_SECONDS = 60;
const IDLE_SECONDS = 12 * 60 * 60;
const ABSOLUTE_SECONDS = 24 * 60 * 60;
// A constant, never a setting: one write per minute per session at most, so
// the idle limit behaves as "12 hours minus at most one minute".
const SESSION_TOUCH_SECONDS = 60;
const LOGIN_TRANSACTION_SECONDS = 10 * 60;
const REVOCATION_EVENT_RETENTION_SECONDS = 7 * 24 * 60 * 60;
const MAX_TOKEN_RESPONSE_BYTES = 64 * 1024;
const MAX_JWKS_BYTES = 64 * 1024;
const MAX_SIGNED_TOKEN_CHARS = 16 * 1024;
const JWKS_CACHE_SECONDS = 10 * 60;
const JWKS_MIN_REFRESH_SECONDS = 60;
const HTTP_TIMEOUT_MS = 10 * 1000;
const JWKS_TIMEOUT_MS = 5 * 1000;

class CentralAuthError extends Error {}
// The identity is valid but not on this Pages access list.
class AccessDeniedError extends CentralAuthError {}
// The callback does not match a live login transaction in this browser.
class AuthTransactionError extends CentralAuthError {}
// Auth refused the code (consumed, expired, superseded by another tab): the
// person retries; it is not an outage.
class CodeExchangeRejectedError extends CentralAuthError {}

const now = () => Math.floor(Date.now() / 1000);
const isInt = (v) => typeof v === "number" && Number.isSafeInteger(v);

// stripTrailingSlashes — "https://auth.example/" names the same issuer as the
// configured origin. A loop, not /\/+$/: that pattern backtracks
// quadratically on a long run of slashes in an attacker-supplied value.
function stripTrailingSlashes(value) {
  let end = value.length;
  while (end > 0 && value.charCodeAt(end - 1) === 47) end--;
  return value.slice(0, end);
}

// normalizeEmail — Auth already sends a normalized address; this applies the
// same shape to CLI input so `pages access grant` and Auth's events agree.
function normalizeEmail(raw) {
  if (typeof raw !== "string") throw new CentralAuthError("a valid email address is required");
  const email = raw.normalize("NFC").trim().toLowerCase();
  const at = email.indexOf("@");
  if (
    !email ||
    email.length > 254 ||
    at < 1 ||
    at !== email.lastIndexOf("@") ||
    at === email.length - 1 ||
    email.endsWith(".") ||
    email[at + 1] === "." ||
    /[\p{C}\s]/u.test(email)
  ) {
    throw new CentralAuthError("a valid email address is required");
  }
  return email;
}

const sha256Hex = (value) => crypto.createHash("sha256").update(value, "utf8").digest("hex");

// ── Configuration ──────────────────────────────────────────────────────────

function truthy(value) {
  return ["1", "true", "yes", "on"].includes(String(value || "").trim().toLowerCase());
}

function parseOrigin(name, raw, allowHttp) {
  let url;
  try {
    url = new URL(String(raw || "").trim());
  } catch {
    throw new Error(`${name} must be an absolute https:// origin`);
  }
  const schemes = allowHttp ? ["https:", "http:"] : ["https:"];
  if (
    !schemes.includes(url.protocol) ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    (url.pathname !== "/" && url.pathname !== "")
  ) {
    throw new Error(`${name} must be an ${allowHttp ? "http(s)" : "https"}:// origin without a path, query or credentials`);
  }
  return url.origin;
}

function decodePublicKey(encoded) {
  const text = String(encoded).trim();
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(text)) return null;
  const raw = Buffer.from(text, "base64");
  return raw.length === 32 ? raw : null;
}

// kid = base64url(sha256(raw public key)[:16]), Auth's key identifier.
function kidForKey(raw) {
  return crypto.createHash("sha256").update(raw).digest().subarray(0, 16).toString("base64url");
}

function publicKeyObject(raw) {
  return crypto.createPublicKey({ key: { kty: "OKP", crv: "Ed25519", x: raw.toString("base64url") }, format: "jwk" });
}

// staticSigningKeys — AUTH_SIGNING_PUBKEY plus AUTH_SIGNING_PREVIOUS_PUBKEYS.
// Central mode refuses to start without one valid key, so back-channel
// verification works even when Auth is unreachable at boot.
function staticSigningKeys(env) {
  const encoded = [env.AUTH_SIGNING_PUBKEY || "", ...String(env.AUTH_SIGNING_PREVIOUS_PUBKEYS || "").split(",")]
    .map((k) => k.trim())
    .filter(Boolean);
  if (encoded.length === 0) {
    throw new Error("AUTH_SIGNING_PUBKEY is required in central mode: run `auth pubkey` on the Auth host");
  }
  return encoded.map((k) => {
    const raw = decodePublicKey(k);
    if (!raw) throw new Error("AUTH_SIGNING_PUBKEY / AUTH_SIGNING_PREVIOUS_PUBKEYS must each be a base64 32-byte Ed25519 key");
    return raw;
  });
}

// loadConfig — every central-mode setting, validated. Throws on any problem
// so a misconfigured box fails at start, not on the first sign-in.
function loadConfig(env = process.env) {
  const allowHttp = truthy(env.AUTH_ALLOW_INSECURE_HTTP);
  const cookieSecure = env.PAGES_AUTH_COOKIE_SECURE === undefined ? true : truthy(env.PAGES_AUTH_COOKIE_SECURE);
  if (!cookieSecure && !allowHttp) {
    throw new Error("PAGES_AUTH_COOKIE_SECURE=0 needs AUTH_ALLOW_INSECURE_HTTP=1 (local development only)");
  }
  const dashboardHost = (env.DASHBOARD_HOST || "").trim().toLowerCase();
  if (!dashboardHost) throw new Error("central mode needs DASHBOARD_HOST: the sign-in callback must land on it");
  const publicUrl = env.PAGES_PUBLIC_URL || (dashboardHost ? `https://${dashboardHost}` : "");
  const clientId = String(env.AUTH_CLIENT_ID || "pages").trim();
  const clientSecret = String(env.AUTH_CLIENT_SECRET || "");
  if (!clientId || clientId.includes(":") || clientId.length > 128) {
    throw new Error("AUTH_CLIENT_ID must be 1-128 characters without ':'");
  }
  const secretBytes = Buffer.byteLength(clientSecret);
  if (secretBytes < 32 || secretBytes > 256 || clientSecret.includes(":")) {
    throw new Error("AUTH_CLIENT_SECRET must be 32-256 bytes without ':' (it is shown once by `auth app create`)");
  }
  const cookieSecret = env.PAGE_COOKIE_SECRET || "";
  if (Buffer.byteLength(cookieSecret) < 16) {
    throw new Error("central mode signs its login transaction with PAGE_COOKIE_SECRET; it must be set");
  }
  if (env.NODE_ENV === "production" && (allowHttp || !cookieSecure)) {
    throw new Error("AUTH_ALLOW_INSECURE_HTTP / PAGES_AUTH_COOKIE_SECURE=0 are for local development and refused in production");
  }
  const parsedPublic = parseOrigin("PAGES_PUBLIC_URL", publicUrl, allowHttp);
  // The login cookie is host-only, so the registered callback must land on
  // the host that set it.
  if (new URL(parsedPublic).host !== dashboardHost) {
    throw new Error(`PAGES_PUBLIC_URL must be the dashboard host (${dashboardHost}); the sign-in cookie is host-only`);
  }
  return {
    issuer: parseOrigin("AUTH_ISSUER_URL", env.AUTH_ISSUER_URL, allowHttp),
    publicUrl: parsedPublic,
    clientId,
    clientSecret,
    staticKeys: staticSigningKeys(env),
    cookieSecure,
    // Distinct key for the login-transaction cookie, derived so no new
    // secret has to be provisioned and no other purpose shares the key.
    transactionKey: Buffer.from(
      crypto.hkdfSync("sha256", Buffer.from(cookieSecret), Buffer.alloc(0), "pages/central-auth/login-transaction/v1", 32)
    ),
  };
}

// ── Signed-token verification (back-channel logout + access events) ────────

function decodeSegment(segment) {
  if (!segment || !/^[A-Za-z0-9_-]+$/.test(segment)) throw new CentralAuthError("invalid token");
  return Buffer.from(segment, "base64url");
}

function parseJson(buf) {
  try {
    const value = JSON.parse(buf.toString("utf8"));
    return value && typeof value === "object" && !Array.isArray(value) ? value : null;
  } catch {
    return null;
  }
}

function tokenKid(raw) {
  const parts = String(raw || "").split(".");
  if (parts.length !== 3) return null;
  try {
    const header = parseJson(decodeSegment(parts[0]));
    return header && typeof header.kid === "string" ? header.kid : null;
  } catch {
    return null;
  }
}

// verifySigned — signature, typ/alg and the claims both event kinds share.
// Returns the claims; the caller checks its event-specific shape.
function verifySigned(raw, typ, { issuer, audience, keys, at = now() }) {
  if (typeof raw !== "string" || raw.length > MAX_SIGNED_TOKEN_CHARS) throw new CentralAuthError("invalid token");
  const parts = raw.split(".");
  if (parts.length !== 3) throw new CentralAuthError("invalid token");
  const header = parseJson(decodeSegment(parts[0]));
  const claims = parseJson(decodeSegment(parts[1]));
  const signature = decodeSegment(parts[2]);
  if (!header || !claims || header.typ !== typ || header.alg !== "EdDSA" || typeof header.kid !== "string") {
    throw new CentralAuthError("invalid token");
  }
  const signed = Buffer.from(`${parts[0]}.${parts[1]}`, "ascii");
  const verified = keys.some((key) => {
    if (kidForKey(key) !== header.kid) return false;
    try {
      return crypto.verify(null, signed, publicKeyObject(key), signature);
    } catch {
      return false;
    }
  });
  if (!verified) throw new CentralAuthError("invalid token");
  const { sub, jti, iat, exp, iss, aud } = claims;
  if (
    !isInt(exp) ||
    exp + CLOCK_SKEW_SECONDS <= at ||
    typeof iss !== "string" ||
    stripTrailingSlashes(iss) !== issuer ||
    aud !== audience ||
    typeof sub !== "string" ||
    !sub ||
    sub.length > 255 ||
    typeof jti !== "string" ||
    !jti ||
    jti.length > 255 ||
    !isInt(iat) ||
    iat <= 0 ||
    iat > at + CLOCK_SKEW_SECONDS ||
    Object.hasOwn(claims, "nonce")
  ) {
    throw new CentralAuthError("invalid token");
  }
  return claims;
}

function verifyLogoutToken(raw, opts) {
  const claims = verifySigned(raw, "logout+jwt", opts);
  const events = claims.events;
  if (!events || typeof events !== "object" || !events[BACKCHANNEL_LOGOUT_EVENT] || typeof events[BACKCHANNEL_LOGOUT_EVENT] !== "object") {
    throw new CentralAuthError("invalid token");
  }
  return { eventId: claims.jti, subject: claims.sub, issuer: opts.issuer, issuedAt: claims.iat };
}

function verifyAccessToken(raw, opts) {
  const claims = verifySigned(raw, "access+jwt", opts);
  const access = claims.events && typeof claims.events === "object" ? claims.events[APPLICATION_ACCESS_EVENT] : null;
  if (!access || typeof access !== "object" || !isInt(access.version) || access.version <= 0 || !["grant", "revoke"].includes(access.action)) {
    throw new CentralAuthError("invalid token");
  }
  let email;
  try {
    email = normalizeEmail(claims.email);
  } catch {
    throw new CentralAuthError("invalid token");
  }
  // `settings` (application-defined key/values) carries nothing Pages uses —
  // it has one human role, so a grant is the whole decision — but its shape
  // is still the contract's string-to-string map.
  if (access.settings !== undefined) {
    const settings = access.settings;
    if (!settings || typeof settings !== "object" || Array.isArray(settings) || Object.values(settings).some((v) => typeof v !== "string")) {
      throw new CentralAuthError("invalid token");
    }
  }
  return {
    eventId: claims.jti,
    subject: claims.sub,
    email,
    issuer: opts.issuer,
    issuedAt: claims.iat,
    version: access.version,
    allowed: access.action === "grant",
  };
}

// ── HTTP to Auth ───────────────────────────────────────────────────────────

// readCapped — read at most `limit` bytes; a larger body is an error, never
// buffered whole.
async function readCapped(response, limit) {
  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > limit) {
      await reader.cancel().catch(() => {});
      throw new CentralAuthError("response too large");
    }
    chunks.push(Buffer.from(value));
  }
  return Buffer.concat(chunks);
}

// KeyResolver — static env keys plus Auth's published /jwks.json, so an Auth
// signing-key rotation needs no env edit here. Cached 10 minutes; a token
// naming an unknown kid forces at most one fetch a minute; a failed fetch
// keeps the cache. A token whose kid is already known never waits on Auth.
class KeyResolver {
  constructor(issuer, staticKeys, { fetchImpl = globalThis.fetch, clock = now } = {}) {
    this.url = `${issuer}/jwks.json`;
    this.staticKeys = staticKeys;
    this.remoteKeys = [];
    this.fetchedAt = null;
    this.lastAttempt = null;
    this.fetchImpl = fetchImpl;
    this.clock = clock;
  }

  known() {
    const seen = new Set();
    return [...this.staticKeys, ...this.remoteKeys].filter((k) => {
      const id = k.toString("base64");
      if (seen.has(id)) return false;
      seen.add(id);
      return true;
    });
  }

  async refresh() {
    const at = this.clock();
    if (this.lastAttempt !== null && at - this.lastAttempt < JWKS_MIN_REFRESH_SECONDS) return false;
    this.lastAttempt = at;
    try {
      const res = await this.fetchImpl(this.url, {
        headers: { Accept: "application/json" },
        redirect: "manual",
        signal: AbortSignal.timeout(JWKS_TIMEOUT_MS),
      });
      if (!res.ok) return false;
      const doc = parseJson(await readCapped(res, MAX_JWKS_BYTES));
      if (!doc || !Array.isArray(doc.keys)) return false;
      const keys = [];
      for (const entry of doc.keys) {
        if (!entry || entry.kty !== "OKP" || entry.crv !== "Ed25519" || typeof entry.x !== "string") continue;
        if (!/^[A-Za-z0-9_-]+$/.test(entry.x)) continue;
        const raw = Buffer.from(entry.x, "base64url");
        if (raw.length === 32) keys.push(raw);
      }
      this.remoteKeys = keys;
      this.fetchedAt = at;
      return true;
    } catch {
      return false;
    }
  }

  async keysForToken(raw) {
    const kid = tokenKid(raw);
    const stale = this.fetchedAt === null || this.clock() - this.fetchedAt > JWKS_CACHE_SECONDS;
    // A static key never waits on Auth. A key known only from the JWKS is
    // re-checked once the cache is stale, so a key Auth retires stops being
    // trusted within the cache window (a failed fetch keeps the cache).
    if (kid && this.staticKeys.some((k) => kidForKey(k) === kid)) return this.known();
    const remote = kid && this.remoteKeys.some((k) => kidForKey(k) === kid);
    if (stale || (kid && !remote)) await this.refresh();
    return this.known();
  }
}

class Client {
  constructor(config, { fetchImpl = globalThis.fetch, clock = now } = {}) {
    this.config = config;
    this.fetchImpl = fetchImpl;
    this.clock = clock;
  }

  get callbackUrl() {
    return `${this.config.publicUrl}/auth/callback`;
  }

  authorizationUrl({ state, nonce, codeChallenge }) {
    const q = new URLSearchParams({
      response_type: "code",
      client_id: this.config.clientId,
      redirect_uri: this.callbackUrl,
      scope: "openid email",
      state,
      code_challenge: codeChallenge,
      code_challenge_method: "S256",
      nonce,
    });
    return `${this.config.issuer}/authorize?${q}`;
  }

  logoutUrl() {
    return `${this.config.issuer}/logout?client_id=${encodeURIComponent(this.config.clientId)}`;
  }

  // exchange — the code for Auth's identity claims. Returns { subject, email }.
  async exchange({ code, verifier, nonce }) {
    const { issuer, clientId, clientSecret } = this.config;
    let res;
    try {
      res = await this.fetchImpl(`${issuer}/token`, {
        method: "POST",
        redirect: "manual",
        signal: AbortSignal.timeout(HTTP_TIMEOUT_MS),
        headers: {
          Authorization: `Basic ${Buffer.from(`${clientId}:${clientSecret}`).toString("base64")}`,
          "Content-Type": "application/x-www-form-urlencoded",
          Accept: "application/json",
          "Cache-Control": "no-store",
        },
        body: new URLSearchParams({
          grant_type: "authorization_code",
          code,
          redirect_uri: this.callbackUrl,
          client_id: clientId,
          code_verifier: verifier,
        }).toString(),
      });
    } catch {
      throw new CentralAuthError("the authentication service is unreachable");
    }
    // 400 is invalid_grant: consumed, expired, or superseded by a newer
    // /authorize in another tab. A retry, not an outage.
    if (res.status === 400) throw new CodeExchangeRejectedError("the sign-in code was rejected");
    if (res.status !== 200) throw new CentralAuthError("the authentication service rejected the code exchange");
    const payload = parseJson(await readCapped(res, MAX_TOKEN_RESPONSE_BYTES));
    const at = this.clock();
    if (
      !payload ||
      typeof payload.sub !== "string" ||
      !payload.sub ||
      payload.sub.length > 255 ||
      typeof payload.email !== "string" ||
      typeof payload.nonce !== "string" ||
      !safeEqual(payload.nonce, nonce) ||
      typeof payload.iss !== "string" ||
      stripTrailingSlashes(payload.iss) !== issuer ||
      payload.aud !== clientId ||
      !isInt(payload.exp) ||
      payload.exp + CLOCK_SKEW_SECONDS <= at
    ) {
      throw new CentralAuthError("the authentication response was invalid");
    }
    // Claims arrive directly from Auth's token endpoint over TLS with client
    // authentication, so they are trusted without re-verifying id_token (the
    // OIDC code-flow allowance Explorer and Lens also rely on).
    let email;
    try {
      email = normalizeEmail(payload.email);
    } catch {
      throw new CentralAuthError("the authentication response was invalid");
    }
    return { subject: payload.sub, email };
  }
}

function safeEqual(a, b) {
  const x = Buffer.from(String(a));
  const y = Buffer.from(String(b));
  return x.length === y.length && crypto.timingSafeEqual(x, y);
}

// ── Login transaction (signed cookie) ──────────────────────────────────────

function newTransaction(next, at = now()) {
  const verifier = crypto.randomBytes(48).toString("base64url");
  return {
    state: crypto.randomBytes(32).toString("base64url"),
    nonce: crypto.randomBytes(32).toString("base64url"),
    verifier,
    challenge: crypto.createHash("sha256").update(verifier, "ascii").digest("base64url"),
    next: safeLocalPath(next),
    iat: at,
  };
}

function sealTransaction(tx, key) {
  const body = Buffer.from(
    JSON.stringify({ state: tx.state, nonce: tx.nonce, verifier: tx.verifier, next: tx.next, iat: tx.iat })
  ).toString("base64url");
  const mac = crypto.createHmac("sha256", key).update(body).digest("base64url");
  return `${body}.${mac}`;
}

// openTransaction — verify the cookie and the callback's state. Throws
// AuthTransactionError for anything that does not match a live transaction.
function openTransaction(sealed, state, key, at = now()) {
  const fail = () => {
    throw new AuthTransactionError("sign-in expired");
  };
  if (typeof sealed !== "string" || typeof state !== "string" || !state) fail();
  const dot = sealed.indexOf(".");
  if (dot < 1) fail();
  const body = sealed.slice(0, dot);
  const mac = crypto.createHmac("sha256", key).update(body).digest("base64url");
  if (!safeEqual(mac, sealed.slice(dot + 1))) fail();
  let tx;
  try {
    tx = parseJson(decodeSegment(body));
  } catch {
    fail();
  }
  if (
    !tx ||
    !isInt(tx.iat) ||
    tx.iat + LOGIN_TRANSACTION_SECONDS <= at ||
    tx.iat > at + CLOCK_SKEW_SECONDS ||
    typeof tx.state !== "string" ||
    !safeEqual(tx.state, state) ||
    typeof tx.nonce !== "string" ||
    typeof tx.verifier !== "string"
  ) {
    fail();
  }
  return { nonce: tx.nonce, verifier: tx.verifier, next: safeLocalPath(tx.next) };
}

// safeLocalPath — a same-host path, or "/admin". Rejects scheme/authority,
// protocol-relative and backslash forms, control characters, and anything
// long enough to push the transaction cookie past the browser's 4 KB limit.
function safeLocalPath(raw) {
  const fallback = "/admin";
  if (typeof raw !== "string" || !raw || raw.length > 2048) return fallback;
  if (!raw.startsWith("/") || raw.startsWith("//") || raw.includes("\\") || /[\u0000-\u001f\u007f]/.test(raw)) {
    return fallback;
  }
  let out;
  try {
    const url = new URL(raw, "http://pages.invalid");
    if (url.origin !== "http://pages.invalid") return fallback;
    out = url.pathname + url.search;
  } catch {
    return fallback;
  }
  // Dot-segment normalization can turn "/a/..//evil.example" into
  // "//evil.example", so the result is checked again, not only the input.
  if (!out.startsWith("/") || out.startsWith("//") || out.includes("\\")) return fallback;
  return out;
}

// ── Store (Postgres) ───────────────────────────────────────────────────────

// lockSubject — serialize session issuance and back-channel logout for one
// Auth subject (transaction-scoped).
function lockSubject(client, subject) {
  return client.query(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, [`pages-central-subject|${subject}`]);
}

const store = {
  async grantAccess(rawEmail) {
    const email = normalizeEmail(rawEmail);
    await db.query(
      `INSERT INTO central_access (email, enabled) VALUES ($1, TRUE)
       ON CONFLICT (email) DO UPDATE SET enabled = TRUE, updated_at = now()`,
      [email]
    );
    return email;
  },

  // revokeAccess — disable the email and end its sessions in one transaction.
  async revokeAccess(rawEmail, at = now()) {
    const email = normalizeEmail(rawEmail);
    return db.withTransaction(async (c) => {
      const { rowCount } = await c.query(
        `UPDATE central_access SET enabled = FALSE, updated_at = now() WHERE email = $1 AND enabled`,
        [email]
      );
      await c.query(`UPDATE central_sessions SET revoked_at = $2 WHERE email = $1 AND revoked_at IS NULL`, [email, at]);
      return rowCount > 0;
    });
  },

  async listAccess() {
    const { rows } = await db.query(`SELECT email FROM central_access WHERE enabled ORDER BY email`);
    return rows.map((r) => r.email);
  },

  // createSession — the local access decision and the session insert commit
  // together. FOR SHARE makes a concurrent revoke wait for this insert, so its
  // session sweep cannot miss the new row.
  //
  // `exchangeStartedMs` closes the logout race: Auth refuses a code once the
  // central session is revoked, so the only gap is a logout that Pages
  // receives after /token succeeded but before this insert. The per-subject
  // lock (shared with consumeLogoutEvent) orders the two; if a logout for the
  // subject arrived since the exchange began, the sign-in is refused and the
  // person simply signs in again.
  async createSession(subject, rawEmail, { at = now(), exchangeStartedMs = Date.now() } = {}) {
    const email = normalizeEmail(rawEmail);
    if (typeof subject !== "string" || !subject || subject.length > 255) {
      throw new CentralAuthError("the authenticated subject was invalid");
    }
    const token = crypto.randomBytes(32).toString("base64url");
    const absolute = at + ABSOLUTE_SECONDS;
    const idle = Math.min(at + IDLE_SECONDS, absolute);
    // Validation only marks rows revoked; a login is the cheap moment to drop
    // rows that can never validate again. Its own statement, outside the
    // transaction below: holding these row locks while waiting for the
    // subject lock would deadlock against a logout sweeping the same rows.
    await db.query(`DELETE FROM central_sessions WHERE revoked_at IS NOT NULL OR absolute_expires_at <= $1`, [at]);
    await db.withTransaction(async (c) => {
      await lockSubject(c, subject);
      const loggedOut = await c.query(
        `SELECT 1 FROM central_revocation_events WHERE subject = $1 AND received_ms >= $2 LIMIT 1`,
        [subject, exchangeStartedMs]
      );
      if (loggedOut.rows.length > 0) throw new AuthTransactionError("signed out during sign-in");
      const { rows } = await c.query(`SELECT 1 FROM central_access WHERE email = $1 AND enabled FOR SHARE`, [email]);
      if (rows.length === 0) throw new AccessDeniedError("this account does not have access to Pages");
      await c.query(
        `INSERT INTO central_sessions
           (token_hash, subject, email, created_at, last_seen_at, idle_expires_at, absolute_expires_at)
         VALUES ($1, $2, $3, $4, $4, $5, $6)`,
        [sha256Hex(token), subject, email, at, idle, absolute]
      );
    });
    return token;
  },

  // identity — { subject, email } for a live session, else null. A plain
  // read on most requests; the write happens at most once a minute.
  async identity(token, at = now()) {
    if (typeof token !== "string" || !token || token.length > 128) return null;
    const hash = sha256Hex(token);
    const { rows } = await db.query(
      `SELECT s.subject, s.email, s.last_seen_at, s.idle_expires_at, s.absolute_expires_at, a.enabled
         FROM central_sessions s
         JOIN central_access a ON a.email = s.email
        WHERE s.token_hash = $1 AND s.revoked_at IS NULL`,
      [hash]
    );
    const row = rows[0];
    if (!row) return null;
    const idleAt = Number(row.idle_expires_at);
    const absoluteAt = Number(row.absolute_expires_at);
    if (!row.enabled || at >= idleAt || at >= absoluteAt) {
      await db.query(`UPDATE central_sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`, [hash, at]);
      return null;
    }
    if (at - Number(row.last_seen_at) >= SESSION_TOUCH_SECONDS) {
      await db.query(
        `UPDATE central_sessions SET last_seen_at = $2, idle_expires_at = $3
          WHERE token_hash = $1 AND revoked_at IS NULL AND last_seen_at <= $4`,
        // Conditional on the row still being stale, so a burst of concurrent
        // requests writes once and a slow request never moves the clock back.
        [hash, at, Math.min(at + IDLE_SECONDS, absoluteAt), at - SESSION_TOUCH_SECONDS]
      );
    }
    return { subject: row.subject, email: row.email };
  },

  async revokeSession(token, at = now()) {
    if (typeof token !== "string" || !token || token.length > 128) return false;
    const { rowCount } = await db.query(
      `UPDATE central_sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`,
      [sha256Hex(token), at]
    );
    return rowCount > 0;
  },

  // consumeLogoutEvent — idempotent by jti; ends every session of the subject.
  async consumeLogoutEvent(event, at = now(), receivedMs = Date.now()) {
    return db.withTransaction(async (c) => {
      await lockSubject(c, event.subject);
      // Replay protection only has to outlive a token's acceptance window;
      // keep a week for forensics.
      await c.query(`DELETE FROM central_revocation_events WHERE received_at < $1`, [at - REVOCATION_EVENT_RETENTION_SECONDS]);
      const { rowCount } = await c.query(
        `INSERT INTO central_revocation_events (event_id, issuer, subject, issued_at, received_at, received_ms)
         VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (event_id) DO NOTHING`,
        [event.eventId, event.issuer, event.subject, event.issuedAt, at, receivedMs]
      );
      if (rowCount > 0) {
        await c.query(`UPDATE central_sessions SET revoked_at = $2 WHERE subject = $1 AND revoked_at IS NULL`, [event.subject, at]);
      }
      return rowCount > 0;
    });
  },

  // applyAccessEvent — Auth's desired state for one subject. Ordered by
  // version per issuer+subject; the same or an older version is ignored. A
  // changed email disables the old address. Revoke ends the sessions.
  async applyAccessEvent(event, at = now()) {
    return db.withTransaction(async (c) => {
      await c.query(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, [`pages-central-access|${event.issuer}|${event.subject}`]);
      const { rows } = await c.query(
        `SELECT email, version FROM central_access_provisioning WHERE issuer = $1 AND subject = $2`,
        [event.issuer, event.subject]
      );
      const current = rows[0];
      if (current && Number(current.version) >= event.version) return false;
      if (current && current.email !== event.email) {
        await c.query(`UPDATE central_access SET enabled = FALSE, updated_at = now() WHERE email = $1`, [current.email]);
        await c.query(`UPDATE central_sessions SET revoked_at = $2 WHERE email = $1 AND revoked_at IS NULL`, [current.email, at]);
      }
      await c.query(
        `INSERT INTO central_access (email, enabled) VALUES ($1, $2)
         ON CONFLICT (email) DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`,
        [event.email, event.allowed]
      );
      if (!event.allowed) {
        await c.query(`UPDATE central_sessions SET revoked_at = $2 WHERE email = $1 AND revoked_at IS NULL`, [event.email, at]);
      }
      await c.query(
        `INSERT INTO central_access_provisioning (issuer, subject, email, version, allowed, event_id, issued_at, received_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
         ON CONFLICT (issuer, subject) DO UPDATE SET
           email = EXCLUDED.email, version = EXCLUDED.version, allowed = EXCLUDED.allowed,
           event_id = EXCLUDED.event_id, issued_at = EXCLUDED.issued_at, received_at = EXCLUDED.received_at`,
        [event.issuer, event.subject, event.email, event.version, event.allowed, event.eventId, event.issuedAt, at]
      );
      return true;
    });
  },
};

module.exports = {
  ABSOLUTE_SECONDS,
  IDLE_SECONDS,
  SESSION_TOUCH_SECONDS,
  LOGIN_TRANSACTION_SECONDS,
  AccessDeniedError,
  AuthTransactionError,
  CentralAuthError,
  CodeExchangeRejectedError,
  Client,
  KeyResolver,
  kidForKey,
  loadConfig,
  newTransaction,
  normalizeEmail,
  stripTrailingSlashes,
  openTransaction,
  safeLocalPath,
  sealTransaction,
  store,
  verifyAccessToken,
  verifyLogoutToken,
};
