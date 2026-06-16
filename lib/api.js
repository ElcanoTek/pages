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
const { ApiError, badRequest } = require("./apierror");

const router = express.Router();

// Everything under /api/v1 requires a valid agent bearer token.
router.use(tokens.requireBearer);

// actorCtx — the audit/authority identity derived from the bearer token, plus
// the request IP, threaded into every state-machine call.
function actorCtx(req) {
  return { actor: req.agent.actor, actorType: req.agent.actorType, ip: req.ip };
}

// async route wrapper → forwards rejections to the error handler below.
const h = (fn) => (req, res, next) => Promise.resolve(fn(req, res, next)).catch(next);

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

router.get("/pages/:slug", h(async (req, res) => {
  res.json(await versions.getPage(req.params.slug));
}));

// ── versions ───────────────────────────────────────────────────────────────

// deploy/update — the agent's create-or-update call. Same endpoint for both;
// every call appends a new version (dedupe collapses identical re-deploys).
router.post("/pages/:slug/versions", h(async (req, res) => {
  const b = req.body || {};
  const result = await versions.deploy(
    {
      slug: req.params.slug,
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
  });
}));

router.get("/pages/:slug/versions", h(async (req, res) => {
  res.json({ versions: await versions.listVersions(req.params.slug) });
}));

router.get("/pages/:slug/versions/:id", h(async (req, res) => {
  const id = toId(req.params.id);
  res.json({ version: await versions.getVersion(req.params.slug, id) });
}));

// ── pointer moves (open pages: agent fast path) ──────────────────────────────

router.post("/pages/:slug/publish", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.publish(
    { slug: req.params.slug, versionId: toId(b.version_id), expectedVersion: b.expected_version },
    actorCtx(req)
  );
  res.json({ version, published: true });
}));

router.post("/pages/:slug/rollback", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.rollback(
    {
      slug: req.params.slug,
      versionId: b.version_id != null ? toId(b.version_id) : null,
      expectedVersion: b.expected_version,
    },
    actorCtx(req)
  );
  res.json({ version, published: true });
}));

// ── helpers ──────────────────────────────────────────────────────────────────

function toId(v) {
  const n = Number(v);
  if (!Number.isInteger(n) || n <= 0) throw badRequest("version_id must be a positive integer", "bad_version_id");
  return n;
}

// Error handler — ApiError → its status; anything else → 500 (never leaked).
// eslint-disable-next-line no-unused-vars
router.use((err, req, res, _next) => {
  if (err instanceof ApiError) {
    return res.status(err.status).json({ error: err.message, code: err.code });
  }
  console.error("api error:", err.stack || err.message);
  res.status(500).json({ error: "internal error" });
});

module.exports = router;
