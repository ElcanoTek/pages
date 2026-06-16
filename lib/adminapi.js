"use strict";
// lib/adminapi.js — the admin-only mutation surface (PLAN §10), authenticated by
// the Elcano SSO cookie + CSRF (lib/csrf.js), NOT bearer tokens. These are the
// human actions deliberately kept off the agent API: approve/reject a pending
// version, publish/rollback from the shell, toggle the approval gate, take a
// page down, set the theme. Everything routes through the same state machine
// (lib/versions.js) and is audit-logged.
//
// Mounted at /api/v1/admin (before the bearer router) on the dashboard host.

const express = require("express");
const versions = require("./versions");
const rawtoken = require("./rawtoken");
const { requireAdminJSON, requireCsrf } = require("./csrf");
const { ApiError, badRequest } = require("./apierror");

const router = express.Router();
const h = (fn) => (req, res, next) => Promise.resolve(fn(req, res, next)).catch(next);

const CONTENT_HOST = (process.env.CONTENT_HOST || "elcano-pages.com").toLowerCase();
const CONTENT_ORIGIN =
  process.env.CONTENT_ORIGIN ||
  `${CONTENT_HOST.startsWith("localhost") || CONTENT_HOST.startsWith("content.localhost") ? "http" : "https"}://${CONTENT_HOST}`;

function actorCtx(req) {
  return { actor: req.user.email, actorType: "user", ip: req.ip };
}
function toId(v) {
  const n = Number(v);
  if (!Number.isInteger(n) || n <= 0) throw badRequest("id must be a positive integer", "bad_id");
  return n;
}

// All admin endpoints require a valid Elcano admin session.
router.use(requireAdminJSON);

// ── reads (cookie only; no CSRF needed for GET) ──────────────────────────────

// Everything the shell needs to render: page meta, full history, pending queue,
// the published version, and the theme list for the picker.
router.get("/pages/:slug", h(async (req, res) => {
  const { page, published } = await versions.getPage(req.params.slug);
  const all = await versions.listVersions(req.params.slug);
  res.json({
    page,
    published,
    versions: all,
    pending: all.filter((v) => v.status === "pending"),
    themes: await versions.listThemes(),
    csrfOk: true,
  });
}));

// ── mutations (cookie + CSRF) ────────────────────────────────────────────────
router.use(requireCsrf); // applies to every route declared below

router.post("/pages/:slug/versions/:id/approve", h(async (req, res) => {
  const version = await versions.approve(
    { slug: req.params.slug, versionId: toId(req.params.id), expectedVersion: (req.body || {}).expected_version },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/:slug/versions/:id/reject", h(async (req, res) => {
  const version = await versions.reject(
    { slug: req.params.slug, versionId: toId(req.params.id), note: (req.body || {}).note },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/:slug/publish", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.publish(
    { slug: req.params.slug, versionId: toId(b.version_id), expectedVersion: b.expected_version },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/:slug/rollback", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.rollback(
    { slug: req.params.slug, versionId: b.version_id != null ? toId(b.version_id) : null, expectedVersion: b.expected_version },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/:slug/approval", h(async (req, res) => {
  res.json(await versions.setApproval({ slug: req.params.slug, requireApproval: !!(req.body || {}).require_approval }, actorCtx(req)));
}));

router.post("/pages/:slug/disable", h(async (req, res) => {
  res.json(await versions.setDisabled({ slug: req.params.slug, disabled: true }, actorCtx(req)));
}));
router.post("/pages/:slug/enable", h(async (req, res) => {
  res.json(await versions.setDisabled({ slug: req.params.slug, disabled: false }, actorCtx(req)));
}));

router.post("/pages/:slug/theme", h(async (req, res) => {
  res.json(await versions.setTheme({ slug: req.params.slug, theme: (req.body || {}).theme }, actorCtx(req)));
}));

router.post("/pages/:slug/password", h(async (req, res) => {
  res.json(await versions.setPassword({ slug: req.params.slug, password: (req.body || {}).password }, actorCtx(req)));
}));

// preview-token — mint a short-TTL signed /raw view token for ANY version
// (incl. drafts/pending) so the shell can preview it in a sandboxed iframe on
// the content host. The version must belong to the page.
router.post("/pages/:slug/preview-token", h(async (req, res) => {
  const versionId = toId((req.body || {}).version_id);
  const v = await versions.getVersion(req.params.slug, versionId); // 404s if not on this page
  const { page } = await versions.getPage(req.params.slug);
  const token = rawtoken.mint(
    { pageId: page.id, versionId: v.id, purpose: "view", renderMode: v.render_mode },
    300
  );
  res.json({ url: `${CONTENT_ORIGIN}/raw/${page.slug}?t=${encodeURIComponent(token)}` });
}));

// eslint-disable-next-line no-unused-vars
router.use((err, req, res, _next) => {
  if (err instanceof ApiError) return res.status(err.status).json({ error: err.message, code: err.code });
  console.error("admin api error:", err.stack || err.message);
  res.status(500).json({ error: "internal error" });
});

module.exports = router;
