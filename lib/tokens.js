// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
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
//   'data_update'       — optional caller-owned automation: only
//                        get_page_data/update_page_data and only for exact
//                        slugs explicitly granted by an operator.
//   'admin'            — reserved; admin actions still go through the cookie+CSRF
//                        path, not bearer (PLAN.md §6, §10).

const crypto = require("node:crypto");
const db = require("./db");
const { unauthorized, forbidden } = require("./apierror");

const PEPPER = process.env.API_TOKEN_PEPPER || "";
const SCOPES = new Set(["deploy", "data_update", "admin"]);
const LEGACY_BROAD_SCOPES = new Set(["deploy", "admin"]);
// The narrowest scope that runs a daily refresh. record_refresh_check belongs
// here because the refresh it runs most often is the one that publishes
// NOTHING — source unchanged, tool unavailable, gate hit — and that is exactly
// the outcome nothing recorded. It cannot create or move a version, cannot
// touch data or any hash, and is still slug-gated like the other two, so it
// adds no authoring reach.
const DATA_UPDATE_TOOLS = new Set(["get_page_data", "update_page_data", "record_refresh_check"]);

if (!PEPPER) {
  // Fail CLOSED in production: without the pepper an attacker with a DB dump can
  // brute/rainbow the token hashes offline. Dev/test may run without it.
  if (process.env.NODE_ENV === "production") {
    throw new Error("API_TOKEN_PEPPER is required in production (an empty pepper removes the offline-DB-dump defense for API tokens).");
  }
  console.warn(
    "WARNING: API_TOKEN_PEPPER is unset — API tokens are hashed with an empty " +
      "pepper. Fine for local dev; set it in production (bootstrap generates one)."
  );
}

function hashToken(rawToken) {
  return crypto.createHmac("sha256", PEPPER).update(rawToken).digest("hex");
}

function normalizeGrantSlug(slug) {
  if (typeof slug !== "string") throw new Error("allowed slugs must be strings");
  const normalized = slug.trim().toLowerCase();
  if (!/^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:\/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$/.test(normalized)) {
    throw new Error(`bad allowed slug: ${slug}`);
  }
  return normalized;
}

function normalizeAllowedSlugs(scope, allowedSlugs) {
  if (allowedSlugs === undefined || allowedSlugs === null) allowedSlugs = [];
  if (!Array.isArray(allowedSlugs)) throw new Error("allowedSlugs must be an array");
  const normalized = [...new Set(allowedSlugs.map(normalizeGrantSlug))].sort();
  if (scope !== "data_update" && normalized.length > 0) {
    throw new Error("exact slug grants are only valid for data_update tokens");
  }
  return normalized;
}

// mint — create a token row and return its metadata plus the one-time secret.
// `token` (the raw secret) is returned only here and never stored. Exact-slug
// grants bind to the page currently holding each slug when one exists; grants
// for a not-yet-created slug stay unbound and bind lazily at first use
// (verify), and once bound they never re-bind (migrations/011).
async function mint({ label, scope = "deploy", allowedSlugs = [] }) {
  if (!label || typeof label !== "string") throw new Error("label is required");
  if (!SCOPES.has(scope)) throw new Error(`bad scope: ${scope}`);
  const normalizedSlugs = normalizeAllowedSlugs(scope, allowedSlugs);
  const raw = `pgs_${crypto.randomBytes(24).toString("base64url")}`;
  const prefix = raw.slice(0, 12);
  const tokenHash = hashToken(raw);
  return db.withTransaction(async (client) => {
    const { rows } = await client.query(
      `INSERT INTO api_tokens (label, prefix, token_hash, scope)
       VALUES ($1, $2, $3, $4) RETURNING id`,
      [label, prefix, tokenHash, scope]
    );
    const id = rows[0].id;
    if (normalizedSlugs.length > 0) {
      await client.query(
        `INSERT INTO api_token_page_grants (token_id, slug, page_id)
         SELECT $1, s.slug, p.id
           FROM unnest($2::text[]) AS s(slug)
           LEFT JOIN pages p ON p.slug = s.slug AND p.deleted_at IS NULL`,
        [id, normalizedSlugs]
      );
    }
    return { id, label, scope, prefix, allowed_slugs: normalizedSlugs, token: raw };
  });
}

// verify — look up a presented bearer token. Returns { id, label, scope } or
// null (revoked/unknown). Updates last_used_at as a side effect on success.
//
// allowed_slugs carries the grants that currently authorize a page:
//   • bound grant (page_id set) → denied ONLY when a different live page now
//     holds the slug (delete→recreate rebinds nothing: the recreated page is a
//     new row). A deleted page with no live holder stays allowed and surfaces
//     page_not_found from the handler, exactly as before.
//   • unbound grant (page_id NULL — minted before the page existed) → stays
//     allowed and binds lazily to the first live page holding the slug, then
//     behaves as bound (never re-binds).
async function verify(rawToken) {
  if (typeof rawToken !== "string" || !rawToken.startsWith("pgs_")) return null;
  const tokenHash = hashToken(rawToken);
  const { rows } = await db.query(
    `SELECT t.id, t.label, t.scope,
            COALESCE(array_agg(g.slug ORDER BY g.slug) FILTER (
              WHERE g.slug IS NOT NULL
                AND (g.page_id IS NULL OR live.id IS NULL OR g.page_id = live.id)
            ), ARRAY[]::text[]) AS allowed_slugs,
            COALESCE(array_agg(g.slug ORDER BY g.slug) FILTER (
              WHERE g.page_id IS NULL AND live.id IS NOT NULL
            ), ARRAY[]::text[]) AS bindable_slugs
       FROM api_tokens t
       LEFT JOIN api_token_page_grants g ON g.token_id = t.id
       LEFT JOIN pages live ON live.slug = g.slug AND live.deleted_at IS NULL
      WHERE t.token_hash = $1 AND t.revoked_at IS NULL
      GROUP BY t.id, t.label, t.scope`,
    [tokenHash]
  );
  const t = rows[0];
  if (!t) return null;
  // Persist lazy binds (guarded, idempotent) so the next request reads the
  // bound state directly. Never touches already-bound grants.
  if (t.bindable_slugs.length > 0) {
    await db.query(
      `UPDATE api_token_page_grants g SET page_id = live.id
        FROM pages live
       WHERE g.token_id = $1 AND g.slug = ANY($2::text[]) AND g.page_id IS NULL
         AND live.slug = g.slug AND live.deleted_at IS NULL`,
      [t.id, t.bindable_slugs]
    );
  }
  // Best-effort and throttled: authentication is a hot path, so do not create
  // a row update/WAL write for every MCP call in a multi-step agent run.
  db.query(
    `UPDATE api_tokens SET last_used_at = now()
      WHERE id = $1
        AND (last_used_at IS NULL OR last_used_at < now() - interval '5 minutes')`,
    [t.id]
  ).catch(() => {});
  return t;
}

async function list() {
  const { rows } = await db.query(
    `SELECT t.id, t.label, t.prefix, t.scope, t.last_used_at, t.created_at, t.revoked_at,
            COALESCE(array_agg(g.slug ORDER BY g.slug) FILTER (WHERE g.slug IS NOT NULL), ARRAY[]::text[]) AS allowed_slugs
       FROM api_tokens t
       LEFT JOIN api_token_page_grants g ON g.token_id = t.id
      GROUP BY t.id, t.label, t.prefix, t.scope, t.last_used_at, t.created_at, t.revoked_at
      ORDER BY t.created_at DESC`
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
      req.agent = {
        actor: token.label,
        actorType: "agent",
        scope: token.scope,
        tokenId: token.id,
        allowedSlugs: token.allowed_slugs || [],
      };
      next();
    })
    .catch(next);
}

function requireRestAccess(req, _res, next) {
  if (req.agent && LEGACY_BROAD_SCOPES.has(req.agent.scope)) return next();
  return next(forbidden("this bearer token may only use its scoped Pages MCP data tools", "token_scope_denied"));
}

function isMcpToolAllowed(agent, toolName) {
  if (!agent || !SCOPES.has(agent.scope)) return false;
  if (LEGACY_BROAD_SCOPES.has(agent.scope)) return true;
  return agent.scope === "data_update" && DATA_UPDATE_TOOLS.has(toolName);
}

function authorizeMcpTool(agent, toolName, args) {
  if (!isMcpToolAllowed(agent, toolName)) {
    throw forbidden(`token scope ${agent && agent.scope ? agent.scope : "unknown"} cannot call ${toolName}`, "tool_not_allowed");
  }
  if (agent.scope !== "data_update") return;

  let slug;
  try {
    slug = normalizeGrantSlug(args && args.slug);
  } catch {
    throw forbidden("data_update tools require an exactly granted page slug", "slug_not_allowed");
  }
  if (!(agent.allowedSlugs || []).includes(slug)) {
    throw forbidden(`this token is not authorized for page slug ${slug}`, "slug_not_allowed");
  }
}

module.exports = {
  SCOPES,
  DATA_UPDATE_TOOLS,
  hashToken,
  normalizeGrantSlug,
  mint,
  verify,
  list,
  revoke,
  requireBearer,
  requireRestAccess,
  isMcpToolAllowed,
  authorizeMcpTool,
};
