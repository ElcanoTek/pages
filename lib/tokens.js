"use strict";
// lib/tokens.js — agent API bearer tokens (PLAN.md §6c, §5 api_tokens).
//
// One token per agent (chat, cutlass) so they revoke independently. The raw
// token is shown ONCE at creation; we store only HMAC-SHA256(token, pepper) and
// a short display prefix. Verification hashes the presented token and looks it
// up — constant-time is provided by the hash + unique-ish lookup; we never
// compare raw secrets directly.
//
//   token  = "pgs_" + base64url(24 random bytes)
//   stored = { prefix: first 12 chars, token_hash: HMAC-SHA256(token, pepper) }
//
// scope:
//   'deploy' (default) — agent: create drafts/pending, and on OPEN pages
//                        publish/rollback. Never approve/reject/disable.
//   'admin'            — reserved; admin actions still go through the cookie+CSRF
//                        path, not bearer (PLAN.md §6, §10).

const crypto = require("node:crypto");
const db = require("./db");
const { unauthorized } = require("./apierror");

const PEPPER = process.env.API_TOKEN_PEPPER || "";
const SCOPES = new Set(["deploy", "admin"]);

if (!PEPPER) {
  console.warn(
    "WARNING: API_TOKEN_PEPPER is unset — API tokens are hashed with an empty " +
      "pepper. Fine for local dev; set it in production (bootstrap generates one)."
  );
}

function hashToken(rawToken) {
  return crypto.createHmac("sha256", PEPPER).update(rawToken).digest("hex");
}

// mint — create a token row and return { id, label, scope, prefix, token }.
// `token` (the raw secret) is returned only here and never stored.
async function mint({ label, scope = "deploy" }) {
  if (!label || typeof label !== "string") throw new Error("label is required");
  if (!SCOPES.has(scope)) throw new Error(`bad scope: ${scope}`);
  const raw = `pgs_${crypto.randomBytes(24).toString("base64url")}`;
  const prefix = raw.slice(0, 12);
  const tokenHash = hashToken(raw);
  const { rows } = await db.query(
    `INSERT INTO api_tokens (label, prefix, token_hash, scope)
     VALUES ($1, $2, $3, $4) RETURNING id`,
    [label, prefix, tokenHash, scope]
  );
  return { id: rows[0].id, label, scope, prefix, token: raw };
}

// verify — look up a presented bearer token. Returns { id, label, scope } or
// null (revoked/unknown). Updates last_used_at as a side effect on success.
async function verify(rawToken) {
  if (typeof rawToken !== "string" || !rawToken.startsWith("pgs_")) return null;
  const tokenHash = hashToken(rawToken);
  const { rows } = await db.query(
    `SELECT id, label, scope FROM api_tokens
      WHERE token_hash = $1 AND revoked_at IS NULL`,
    [tokenHash]
  );
  const t = rows[0];
  if (!t) return null;
  // best-effort; don't block the request on the bookkeeping write
  db.query("UPDATE api_tokens SET last_used_at = now() WHERE id = $1", [t.id]).catch(() => {});
  return t;
}

async function list() {
  const { rows } = await db.query(
    `SELECT id, label, prefix, scope, last_used_at, created_at, revoked_at
       FROM api_tokens ORDER BY created_at DESC`
  );
  return rows;
}

async function revoke(id) {
  const { rowCount } = await db.query(
    `UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`,
    [id]
  );
  return rowCount > 0;
}

// requireBearer — Express middleware. Validates `Authorization: Bearer <token>`
// and attaches req.agent = { actor, actorType: 'agent', scope }. 401 otherwise.
// Bearer auth carries no ambient cookies, so it is CSRF-immune by construction
// (PLAN.md §7) — these routes never trust the SSO cookie.
function requireBearer(req, res, next) {
  const header = req.headers.authorization || "";
  const m = header.match(/^Bearer\s+(.+)$/i);
  if (!m) return next(unauthorized("missing bearer token", "no_token"));
  verify(m[1])
    .then((token) => {
      if (!token) return next(unauthorized("invalid or revoked token", "bad_token"));
      req.agent = { actor: token.label, actorType: "agent", scope: token.scope, tokenId: token.id };
      next();
    })
    .catch(next);
}

module.exports = { hashToken, mint, verify, list, revoke, requireBearer };
