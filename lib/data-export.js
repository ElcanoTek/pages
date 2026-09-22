// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/data-export.js — short-lived, read-only URLs for one published version's
// managed-data schema and envelope, so a refresh run can put them in a workspace
// file instead of its context.
//
// WHY. get_page_data is the first and most repeated call of every refresh, and
// the full answer (schema, profile, 150–400 KB of rows) is re-sent on every later
// model step. A client that fetches URLs host-side — Fleet's download_url, which
// takes a URL and nothing else, no headers — can land these bytes on disk where
// run_python reads them, and the model only ever handles a link. That client
// constraint is why the credential is IN the URL: an Authorization-header-only
// endpoint would be unreachable from the one seam that keeps bytes out of
// context, and a sandbox curl needs network egress most refresh runs do not have.
//
// So the URL is a bearer capability, like a signed object-store link, and it is
// bounded to make a leak uninteresting:
//   • read-only   — GET/HEAD of three derived JSON documents; nothing else.
//   • one version — bound to the page id and the immutable version id that was
//                   live when get_page_data minted it. page_versions content is
//                   append-only, so the bytes behind a URL never change.
//   • minutes     — EXPORT_TTL_SECONDS, then refused.
//   • re-authorized on every fetch — the minting agent token must still be
//                   unrevoked and still allowed to call get_page_data on this
//                   page (a data_update grant that no longer holds the slug ends
//                   its URLs too). A URL can never reach more than the token that
//                   minted it could read at that moment.
//   • its own key — HMAC under a key DERIVED from RAW_TOKEN_SECRET for this
//                   audience alone, so no /raw view/session/edit token verifies
//                   here and no export token verifies at /raw.
//   • dashboard host only — mounted on the trusted host beside /api/v1 and
//                   /upload, reads no cookie, and serves attachment JSON with
//                   nosniff, never HTML. No new content-host route exists.

const crypto = require("node:crypto");
const express = require("express");
const db = require("./db");
const tokens = require("./tokens");
const pageData = require("./page-data");
const { ApiError, conflict, unauthorized, notFound, fromDbError } = require("./apierror");
const { DASHBOARD_ORIGIN } = require("./csp");

const AUDIENCE = "pages-data-export";
// Long enough for a run to fetch three files one model step apart; short enough
// that a link copied out of a transcript is dead before anyone reads it.
const EXPORT_TTL_SECONDS = 10 * 60;
const PARTS = Object.freeze(["schema", "data", "envelope"]);

const SECRET = process.env.RAW_TOKEN_SECRET || "";
const KEY = SECRET ? crypto.createHmac("sha256", SECRET).update(`${AUDIENCE}/v1`).digest() : null;

function hmac(body) {
  return crypto.createHmac("sha256", KEY).update(body).digest();
}

function mint({ pageId, versionId, tokenId }, ttlSeconds = EXPORT_TTL_SECONDS) {
  if (!KEY) {
    throw conflict("data export URLs are unavailable: this server has no RAW_TOKEN_SECRET", "data_export_unavailable");
  }
  for (const [name, value] of Object.entries({ pageId, versionId, tokenId })) {
    if (!/^[1-9][0-9]*$/.test(String(value))) throw new Error(`data export ${name} is missing`);
  }
  const exp = Math.floor(Date.now() / 1000) + ttlSeconds;
  const claims = { aud: AUDIENCE, pid: String(pageId), vid: String(versionId), tid: String(tokenId), exp };
  const body = Buffer.from(JSON.stringify(claims)).toString("base64url");
  return { token: `${body}.${hmac(body).toString("base64url")}`, expiresAt: new Date(exp * 1000).toISOString() };
}

// verify → claims or null. Constant-time signature check, then audience and
// expiry. Says nothing about whether the minting token is still good; that
// needs the database and happens per request below.
function verify(token) {
  if (!KEY || typeof token !== "string") return null;
  const dot = token.indexOf(".");
  if (dot < 1 || dot === token.length - 1 || token.indexOf(".", dot + 1) !== -1) return null;
  const body = token.slice(0, dot);
  const given = Buffer.from(token.slice(dot + 1), "base64url");
  const expected = hmac(body);
  if (given.length !== expected.length || !crypto.timingSafeEqual(given, expected)) return null;
  let claims;
  try {
    claims = JSON.parse(Buffer.from(body, "base64url").toString("utf8"));
  } catch {
    return null;
  }
  if (!claims || claims.aud !== AUDIENCE) return null;
  if (typeof claims.exp !== "number" || claims.exp <= Math.floor(Date.now() / 1000)) return null;
  for (const key of ["pid", "vid", "tid"]) {
    if (!/^[1-9][0-9]*$/.test(String(claims[key]))) return null;
  }
  return claims;
}

function exportUrls(token) {
  const base = `${DASHBOARD_ORIGIN}/export/${token}`;
  return {
    schema_url: `${base}/schema.json`,
    data_url: `${base}/data.json`,
    envelope_url: `${base}/envelope.json`,
  };
}

// The bytes each part serves. schema and data are the exact canonical JSON the
// semantic hashes are computed over, so sha256(file) === schema_sha256 /
// data_sha256 and a caller can prove it downloaded the version it was told about.
function partBody(managed, part) {
  if (part === "schema") return pageData.canonicalJson(managed.schema);
  if (part === "data") return pageData.canonicalJson(managed.envelope.data);
  return pageData.canonicalJson(managed.envelope);
}

// One non-revealing answer for a forged, expired, cross-audience or revoked
// credential, like the upload-ticket endpoint.
const invalid = () => unauthorized("unknown or expired data export URL", "data_export_invalid");

async function resolve(token, part) {
  const claims = verify(token);
  if (!claims) throw invalid();
  const agentToken = await tokens.verifyId(claims.tid);
  if (!agentToken) throw invalid();
  const { rows } = await db.query(
    `SELECT p.slug, v.html
       FROM page_versions v
       JOIN pages p ON p.id = v.page_id
      WHERE v.id = $1 AND p.id = $2 AND p.deleted_at IS NULL`,
    [claims.vid, claims.pid]
  );
  if (!rows[0]) throw notFound("page data not found", "page_not_found");
  const agent = { scope: agentToken.scope, allowedSlugs: agentToken.allowed_slugs || [] };
  try {
    tokens.authorizeMcpTool(agent, "get_page_data", { slug: rows[0].slug });
  } catch {
    throw invalid();
  }
  const managed = pageData.parseManagedHtml(rows[0].html);
  const safeSlug = rows[0].slug.replace(/[^a-z0-9_-]+/g, "-");
  return { body: partBody(managed, part), filename: `${safeSlug}.v${claims.vid}.${part}.json` };
}

const router = express.Router();

router.get("/:token/:file", (req, res, next) => {
  const match = /^(schema|data|envelope)\.json$/.exec(req.params.file);
  if (!match) return next(notFound("unknown export file", "not_found"));
  Promise.resolve(resolve(req.params.token, match[1]))
    .then(({ body, filename }) => {
      res.set({
        "Cache-Control": "no-store",
        "Content-Disposition": `attachment; filename="${filename}"`,
        "X-Content-Type-Options": "nosniff",
      });
      res.type("application/json; charset=utf-8").send(body);
    })
    .catch(next);
});

router.all("/:token/:file", (_req, res) =>
  res.status(405).set("Allow", "GET, HEAD").json({ error: "export URLs are read-only", code: "method_not_allowed" })
);

// eslint-disable-next-line no-unused-vars
router.use((err, _req, res, _next) => {
  const mapped = err instanceof ApiError ? err : fromDbError(err);
  if (mapped) return res.status(mapped.status).json({ error: mapped.message, code: mapped.code });
  console.error("data export error:", err.stack || err.message);
  res.status(500).json({ error: "internal error" });
});

module.exports = { AUDIENCE, EXPORT_TTL_SECONDS, PARTS, mint, verify, exportUrls, router };
