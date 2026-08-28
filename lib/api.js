// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/api.js — the /api/v1 REST surface (PLAN.md §10). Mounted on the DASHBOARD
// host only (server.js). This is the Phase-2 agent slice: bearer-authenticated
// page + version management that routes every state change through the version
// state machine (lib/versions.js) — there is no privileged backdoor that skips
// it, so REST and MCP share identical gating and audit.
//
// Auth here is bearer-only (agents). Admin-cookie+CSRF endpoints (approve,
// reject, disable, approval-toggle) are intentionally NOT in this router yet —
// they belong to the trusted shell and arrive with the /admin UI.

const express = require("express");
const versions = require("./versions");
const tokens = require("./tokens");
const preflight = require("./preflight");
const { pageUrls } = require("./mcp");
const { ApiError, badRequest, fromDbError } = require("./apierror");

const router = express.Router();

// Everything under /api/v1 requires a valid agent bearer token.
router.use(tokens.requireBearer);
// Scoped data-update tokens are intentionally MCP-only. This keeps the
// authorization surface to exactly get_page_data/update_page_data.
router.use(tokens.requireRestAccess);

// actorCtx — the audit/authority identity derived from the bearer token, plus
// the request IP, threaded into every state-machine call.
function actorCtx(req) {
  return { actor: req.agent.actor, actorType: req.agent.actorType, tokenId: req.agent.tokenId, ip: req.ip };
}

// async route wrapper → forwards rejections to the error handler below.
const h = (fn) => (req, res, next) => Promise.resolve(fn(req, res, next)).catch(next);

// Slug routes use a multi-segment splat (`*slug`) so nested slugs (northwind/q2)
// resolve without URL-encoding. Express 5 hands the splat over as an array of
// decoded segments — join back into the canonical slug (normalizeSlug
// re-validates downstream). NOTE: `*slug` is greedy across segments, so the
// bare GET /pages/*slug MUST be registered after the sub-resource GETs
// (/versions, /versions/:id) or it would swallow them.
const slugOf = (req) => (Array.isArray(req.params.slug) ? req.params.slug.join("/") : req.params.slug);

// ── pages ────────────────────────────────────────────────────────────────────

router.get("/pages", h(async (_req, res) => {
  res.json({ pages: await versions.listPages() });
}));

router.post("/pages", h(async (req, res) => {
  const { slug, title, client_id, require_approval } = req.body || {};
  const page = await versions.createPage(
    { slug, title, clientId: client_id, requireApproval: require_approval },
    actorCtx(req)
  );
  res.status(201).json({ page });
}));

// set the per-page client password ({password: "..."}). Setting/changing is
// agent-allowed; CLEARING (empty/null) is admin-only (state machine enforces it,
// so a bearer clear → 403 admin_only).
router.post("/pages/*slug/password", h(async (req, res) => {
  res.json(await versions.setPassword({ slug: slugOf(req), password: (req.body || {}).password }, actorCtx(req)));
}));

// rename a page (agent-owned authoring metadata).
router.post("/pages/*slug/title", h(async (req, res) => {
  res.json(await versions.setTitle({ slug: slugOf(req), title: (req.body || {}).title }, actorCtx(req)));
}));

// soft-delete a page. Agents may delete OPEN pages; approval-gated pages 403
// (admin-only). Reversible via the admin restore endpoint.
router.delete("/pages/*slug", h(async (req, res) => {
  res.json(await versions.deletePage({ slug: slugOf(req) }, actorCtx(req)));
}));

// ── versions ───────────────────────────────────────────────────────────────

// deploy/update — the agent's create-or-update call. Same endpoint for both;
// every call appends a new version (dedupe collapses identical re-deploys).
router.post("/pages/*slug/versions", h(async (req, res) => {
  const b = req.body || {};
  const result = await versions.deploy(
    {
      slug: slugOf(req),
      html: b.html,
      renderMode: b.render_mode,
      note: b.note,
      author: b.author,
      source: "api",
      publish: !!b.publish,
      expectedVersion: b.expected_version,
    },
    actorCtx(req)
  );
  res.status(result.deduped ? 200 : 201).json({
    version: result.version,
    deduped: result.deduped,
    published: result.published,
    gated: result.gated,
    live: result.live,
  });
}));

router.get("/pages/*slug/versions", h(async (req, res) => {
  res.json({ versions: await versions.listVersions(slugOf(req)) });
}));

// Static preflight of a stored version against the CSP/sandbox the content host
// serves it under. Read-only, returns findings rather than HTML — see
// lib/preflight.js for why this lives server-side.
router.get("/pages/*slug/preflight", h(async (req, res) => {
  const slug = slugOf(req);
  let versionId = req.query.version_id;
  if (versionId === undefined) {
    const { page } = await versions.getPage(slug);
    if (!page.published_version_id) {
      throw badRequest(`page ${slug} has no published version; pass version_id to preflight a draft`, "no_published_version");
    }
    versionId = page.published_version_id;
  }
  const version = await versions.getVersion(slug, versionId);
  res.json({
    version_id: version.id,
    preflight: preflight.analyze(version.html, { renderMode: version.render_mode || "themed" }),
  });
}));

router.get("/pages/*slug/versions/:id", h(async (req, res) => {
  const id = toId(req.params.id);
  res.json({ version: await versions.getVersion(slugOf(req), id) });
}));

// ── pointer moves (open pages: agent fast path) ──────────────────────────────

router.post("/pages/*slug/publish", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.publish(
    { slug: slugOf(req), versionId: toId(b.version_id), expectedVersion: b.expected_version },
    actorCtx(req)
  );
  res.json({ version, published: true });
}));

router.post("/pages/*slug/rollback", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.rollback(
    {
      slug: slugOf(req),
      versionId: b.version_id != null ? toId(b.version_id) : null,
      expectedVersion: b.expected_version,
    },
    actorCtx(req)
  );
  res.json({ version, published: true });
}));

// The bare page read — registered LAST among the slug GETs (see slugOf note:
// `*slug` would otherwise swallow /pages/<slug>/versions[/:id]).
router.get("/pages/*slug", h(async (req, res) => {
  const r = await versions.getPage(slugOf(req));
  res.json({ ...r, urls: pageUrls(r.page.slug) });
}));

// ── helpers ──────────────────────────────────────────────────────────────────

function toId(v) {
  const n = Number(v);
  if (!Number.isInteger(n) || n <= 0) throw badRequest("version_id must be a positive integer", "bad_version_id");
  return n;
}

// Error handler — ApiError → its status; bounded-wait DB failures → clean
// 503s (lib/db.js timeouts); anything else → 500 (never leaked).
// eslint-disable-next-line no-unused-vars
router.use((err, req, res, _next) => {
  const mapped = err instanceof ApiError ? err : fromDbError(err);
  if (mapped) {
    return res.status(mapped.status).json({ error: mapped.message, code: mapped.code });
  }
  console.error("api error:", err.stack || err.message);
  res.status(500).json({ error: "internal error" });
});

module.exports = router;
