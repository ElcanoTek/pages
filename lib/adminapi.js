// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/adminapi.js — the admin-only mutation surface (PLAN §10), authenticated by
// the Elcano SSO cookie + CSRF (lib/csrf.js), NOT bearer tokens. These are the
// human actions deliberately kept off the agent API: approve/reject a pending
// version, publish/rollback from the shell, toggle the approval gate, take a
// page down, set the theme, and organize pages into workspaces. Version actions
// route through lib/versions.js; workspace actions are isolated from serving
// state in lib/workspaces.js. Every mutation is audit-logged.
//
// Partner portals (lib/portals.js) live here and ONLY here: which dashboards one
// partner credential opens is a human decision, so no bearer or MCP route
// reaches them.
//
// Mounted at /api/v1/admin (before the bearer router) on the dashboard host.

const express = require("express");
const db = require("./db");
const versions = require("./versions");
const workspaces = require("./workspaces");
const portals = require("./portals");
const templates = require("./templates");
const rawtoken = require("./rawtoken");
const { requireAdminJSON, requireCsrf } = require("./csrf");
const { ApiError, badRequest, fromDbError } = require("./apierror");
const { CONTENT_ORIGIN } = require("./csp"); // single source of truth for the public origins

const router = express.Router();
const h = (fn) => (req, res, next) => Promise.resolve(fn(req, res, next)).catch(next);

function actorCtx(req) {
  return { actor: req.user.email, actorType: "user", ip: req.ip };
}
function toId(v) {
  const n = Number(v);
  if (!Number.isInteger(n) || n <= 0) throw badRequest("id must be a positive integer", "bad_id");
  return n;
}
// `*slug` splat params arrive as an array of decoded segments — join back into
// the canonical nested slug ("northwind/q2"); normalizeSlug re-validates. All
// mutation routes here carry a distinct suffix (/publish, /approve, …), so
// unlike lib/api.js there is no greedy-match ordering hazard.
const slugOf = (req) => (Array.isArray(req.params.slug) ? req.params.slug.join("/") : req.params.slug);

// All admin endpoints require a valid Elcano admin session.
router.use(requireAdminJSON);

// ── reads (cookie only; no CSRF needed for GET) ──────────────────────────────

// The landing index: every page (newest first) with its published pointer, so
// the /admin/welcome shell can list them. No per-version detail here.
router.get("/pages", h(async (_req, res) => {
  // One repeatable-read snapshot keeps workspace entities, page memberships,
  // and names coherent during concurrent assign/delete/rename transactions.
  const { pages, workspaceRows } = await db.withTransaction(async (client) => {
    await client.query("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY");
    return {
      pages: await versions.listPages(client, { includeVersionNumbers: true }),
      workspaceRows: await workspaces.list(client),
    };
  });
  // Derive counts from these exact page rows as an additional guarantee that
  // navigation totals and rendered cards agree within the response.
  const counts = new Map();
  for (const page of pages) {
    if (page.workspace_id != null) {
      const id = String(page.workspace_id);
      counts.set(id, (counts.get(id) || 0) + 1);
    }
  }
  const indexedWorkspaces = workspaceRows.map((workspace) => ({
    ...workspace,
    page_count: counts.get(String(workspace.id)) || 0,
  }));
  res.json({ pages, workspaces: indexedWorkspaces });
}));

router.get("/workspaces", h(async (_req, res) => {
  res.json({ workspaces: await workspaces.list() });
}));

// Partner portals: the named sets of dashboards one shared client credential
// opens. Reads never return the credential — a portal password exists in
// plaintext exactly once, in the response that created or rotated it.
// The partner-facing address is derived here rather than stored, for the same
// reason share URLs are: the origin belongs to the deployment, not to the row.
const portalUrl = (portal) => `${CONTENT_ORIGIN}/portal/${portal.slug}`;
const withUrl = (portal) => ({ ...portal, url: portalUrl(portal) });

router.get("/portals", h(async (_req, res) => {
  res.json({ portals: (await portals.list()).map(withUrl) });
}));

router.get("/portals/:id", h(async (req, res) => {
  const detail = await portals.get({ id: toId(req.params.id) });
  res.json({ ...detail, portal: withUrl(detail.portal) });
}));

// One version's full row, INCLUDING its html. The detail read below strips
// `html` from every entry in `versions` on purpose — the whole history would be
// megabytes on a screen that renders it on every mutation — so the source editor
// needs this to open the version a reviewer is actually looking at rather than
// only the live one. `published` still carries its own html, so the live seed and
// a selected version that IS live cost no extra request.
//
// Declared BEFORE /pages/*slug: `*slug` is greedy, so the detail route would
// otherwise swallow "…/versions/12" as part of the slug and 404. (The mutation
// routes further down are POST-only and cannot collide with this GET.)
router.get("/pages/*slug/versions/:id", h(async (req, res) => {
  res.json({ version: await versions.getVersion(slugOf(req), toId(req.params.id)) });
}));

// Everything the shell needs to render: page meta, full history, pending queue,
// the published version, and the theme list for the picker.
router.get("/pages/*slug", h(async (req, res) => {
  const slug = slugOf(req);
  const { page, published } = await versions.getPage(slug);
  // `portals` is per-PAGE membership on purpose: looked at portal-first only, a
  // page missing from one audience's portal is invisible.
  const [all, themes, memberOf] = await Promise.all([
    versions.listVersions(slug),
    versions.listThemes(),
    portals.listForPage(page.id),
  ]);
  res.json({
    page,
    published,
    versions: all,
    pending: all.filter((v) => v.status === "pending"),
    themes,
    portals: memberOf,
    csrfOk: true,
  });
}));

// Template library reads. GET only, so no CSRF — same rule as the page reads
// above. Neither returns the design bytes: the library shows what a template
// REQUIRES (its schemas and reference config), and previewing goes through a
// signed content-host URL instead.
router.get("/templates", h(async (_req, res) => {
  res.json(await templates.list());
}));

router.get("/templates/:name", h(async (req, res) => {
  const revision = req.query.revision === undefined ? null : toId(req.query.revision);
  const detail = await templates.get(req.params.name, { revision });
  const [revisions, pages] = await Promise.all([
    templates.revisions(req.params.name),
    templates.listTemplatePages(req.params.name),
  ]);
  res.json({ ...detail, revisions: revisions.revisions, pages: pages.pages });
}));

// ── mutations (cookie + CSRF) ────────────────────────────────────────────────
router.use(requireCsrf); // applies to every route declared below

// One-level workspaces organize the landing index only. They never change
// public URLs, version history, or serving state. Deletion safely detaches all
// member pages to Ungrouped inside the same audited transaction.
router.post("/workspaces", h(async (req, res) => {
  const workspace = await workspaces.create({ name: (req.body || {}).name }, actorCtx(req));
  res.status(201).json({ workspace });
}));

router.post("/workspaces/:id/rename", h(async (req, res) => {
  const workspace = await workspaces.rename(
    { id: toId(req.params.id), name: (req.body || {}).name },
    actorCtx(req)
  );
  res.json({ workspace });
}));

router.post("/workspaces/:id/delete", h(async (req, res) => {
  const workspace = await workspaces.remove({ id: toId(req.params.id) }, actorCtx(req));
  res.json({ workspace });
}));

// ── partner portals ──────────────────────────────────────────────────────────
// One shared credential over an admin-curated set of dashboards. Unlike
// workspaces, these are not organization: membership decides which client's
// numbers a partner's password opens, so lib/portals.js refuses any actor that
// is not a human admin and there is no agent-facing equivalent of these routes.
// The page in a membership call travels in the BODY, not the path, because page
// slugs nest ("nwm/contoso") and would be ambiguous against the action suffix.

// Omit `password` and Pages generates a strong one; either way the plaintext is
// in this response and nowhere else (never in the audit log). Losing it means
// rotating.
router.post("/portals", h(async (req, res) => {
  const b = req.body || {};
  const created = await portals.create({ slug: b.slug, name: b.name, password: b.password }, actorCtx(req));
  res.status(201).json({ ...created, portal: withUrl(created.portal) });
}));

router.post("/portals/:id/rename", h(async (req, res) => {
  res.json(await portals.rename({ id: toId(req.params.id), name: (req.body || {}).name }, actorCtx(req)));
}));

// Rotation is also revocation: every live session for this portal is bound to
// the credential digest of the hash it replaces.
router.post("/portals/:id/password", h(async (req, res) => {
  res.json(await portals.setPassword({ id: toId(req.params.id), password: (req.body || {}).password }, actorCtx(req)));
}));

router.post("/portals/:id/pages", h(async (req, res) => {
  const b = req.body || {};
  const result = await portals.addPage(
    { id: toId(req.params.id), slug: b.slug, label: b.label, sortOrder: b.sort_order },
    actorCtx(req)
  );
  res.status(201).json(result);
}));

// Partial by design: an absent key is left alone, an explicit null label falls
// back to the page title.
router.post("/portals/:id/pages/update", h(async (req, res) => {
  const b = req.body || {};
  const input = { id: toId(req.params.id), slug: b.slug, pageId: b.page_id };
  if (Object.prototype.hasOwnProperty.call(b, "label")) input.label = b.label;
  if (Object.prototype.hasOwnProperty.call(b, "sort_order")) input.sortOrder = b.sort_order;
  res.json(await portals.updatePage(input, actorCtx(req)));
}));

router.post("/portals/:id/pages/remove", h(async (req, res) => {
  const b = req.body || {};
  res.json(await portals.removePage({ id: toId(req.params.id), slug: b.slug, pageId: b.page_id }, actorCtx(req)));
}));

router.post("/portals/:id/home", h(async (req, res) => {
  const b = req.body || {};
  if (!Object.prototype.hasOwnProperty.call(b, "slug")) {
    throw badRequest("slug is required (use null to clear the home page)", "portal_home_slug_required");
  }
  res.json(await portals.setHome({ id: toId(req.params.id), slug: b.slug }, actorCtx(req)));
}));

// Retire: soft delete, so the row, its membership and its audit trail stay and
// the slug frees for reuse. Member pages keep their own passwords and versions.
router.post("/portals/:id/delete", h(async (req, res) => {
  res.json({ portal: await portals.remove({ id: toId(req.params.id) }, actorCtx(req)) });
}));

router.post("/pages/*slug/versions/:id/approve", h(async (req, res) => {
  const version = await versions.approve(
    { slug: slugOf(req), versionId: toId(req.params.id), expectedVersion: (req.body || {}).expected_version },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/*slug/versions/:id/reject", h(async (req, res) => {
  const version = await versions.reject(
    { slug: slugOf(req), versionId: toId(req.params.id), note: (req.body || {}).note },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/*slug/publish", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.publish(
    { slug: slugOf(req), versionId: toId(b.version_id), expectedVersion: b.expected_version },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/*slug/rollback", h(async (req, res) => {
  const b = req.body || {};
  const version = await versions.rollback(
    { slug: slugOf(req), versionId: b.version_id != null ? toId(b.version_id) : null, expectedVersion: b.expected_version },
    actorCtx(req)
  );
  res.json({ version });
}));

router.post("/pages/*slug/approval", h(async (req, res) => {
  res.json(await versions.setApproval({ slug: slugOf(req), requireApproval: !!(req.body || {}).require_approval }, actorCtx(req)));
}));

router.post("/pages/*slug/disable", h(async (req, res) => {
  res.json(await versions.setDisabled({ slug: slugOf(req), disabled: true }, actorCtx(req)));
}));
router.post("/pages/*slug/enable", h(async (req, res) => {
  res.json(await versions.setDisabled({ slug: slugOf(req), disabled: false }, actorCtx(req)));
}));

router.post("/pages/*slug/theme", h(async (req, res) => {
  res.json(await versions.setTheme({ slug: slugOf(req), theme: (req.body || {}).theme }, actorCtx(req)));
}));

router.post("/pages/*slug/password", h(async (req, res) => {
  res.json(await versions.setPassword({ slug: slugOf(req), password: (req.body || {}).password }, actorCtx(req)));
}));

// create an empty page from the shell (admins with no agent wired up still need
// the C in CRUD). Content is deployed separately (Edit source / an agent).
router.post("/pages", h(async (req, res) => {
  const b = req.body || {};
  const page = await versions.createPage(
    {
      slug: b.slug,
      title: b.title,
      clientId: b.client_id,
      workspaceId: b.workspace_id,
      requireApproval: b.require_approval,
    },
    actorCtx(req)
  );
  res.status(201).json({ page });
}));

router.post("/pages/*slug/workspace", h(async (req, res) => {
  const b = req.body || {};
  if (!Object.prototype.hasOwnProperty.call(b, "workspace_id")) {
    throw badRequest("workspace_id is required (use null for Ungrouped)", "workspace_id_required");
  }
  const page = await workspaces.assignPage(
    { slug: slugOf(req), workspaceId: b.workspace_id },
    actorCtx(req)
  );
  res.json({ page });
}));

router.post("/pages/*slug/title", h(async (req, res) => {
  res.json(await versions.setTitle({ slug: slugOf(req), title: (req.body || {}).title }, actorCtx(req)));
}));

// soft-delete / restore. Admins may delete ANY page (including approval-gated);
// restore is admin-only and undeletes the most-recently deleted row for the slug.
router.post("/pages/*slug/delete", h(async (req, res) => {
  res.json(await versions.deletePage({ slug: slugOf(req) }, actorCtx(req)));
}));
router.post("/pages/*slug/restore", h(async (req, res) => {
  res.json(await versions.restorePage({ slug: slugOf(req) }, actorCtx(req)));
}));

// preview-token — mint a short-TTL signed /raw view token for ANY version
// (incl. drafts/pending) so the shell can preview it in a sandboxed iframe on
// the content host. The version must belong to the page.
router.post("/pages/*slug/preview-token", h(async (req, res) => {
  const versionId = toId((req.body || {}).version_id);
  const v = await versions.getVersion(slugOf(req), versionId); // 404s if not on this page
  const { page } = await versions.getPage(slugOf(req));
  const token = rawtoken.mint(
    { pageId: page.id, versionId: v.id, purpose: "view", renderMode: v.render_mode },
    300
  );
  res.json({ url: `${CONTENT_ORIGIN}/raw/${page.slug}?t=${encodeURIComponent(token)}` });
}));

// deploy-source — the Phase 4 source editor's save. Deploys a new version from
// edited HTML (admin cookie + CSRF, source: "admin"). Routes through the same
// state machine as every other deploy: dedupe, append-only, audit log, and the
// approval gate. publish defaults to false — the admin previews the saved draft
// and clicks Publish separately (the "fix small things" flow). PLAN.md §8:
// edits the stored *source*, never the rendered DOM (lossless, charts survive).
// ── template library ────────────────────────────────────────────────────────
// Human-facing surface for the stored designs: browse them, read what each one
// requires, preview a revision, and upload a new one. Uploading runs the format
// checks FIRST and reports them; nothing is written until the caller has seen
// them, which is the difference between "your template was rejected" and "here
// is which block is missing".

// Dry run: "is this our template format?" — writes nothing, ever.
router.post("/templates/validate", h(async (req, res) => {
  const { html, name } = req.body || {};
  res.json(templates.validateHtml(html, { name }));
}));

// Register. The contract is re-checked here rather than trusted from a prior
// /validate call: the two requests are independent, and the bytes could differ.
router.post("/templates", h(async (req, res) => {
  const { html, name, title, description, note, allow_preflight_errors: allowPreflightErrors } = req.body || {};
  const report = templates.validateHtml(html, { name });
  if (report.name_error) throw badRequest(report.name_error.message, report.name_error.code);
  if (!report.name) throw badRequest("a template name is required", "template_required");
  if (!report.contract_ok) {
    throw new ApiError(422, report.contract_error.message, report.contract_error.code, report.contract_error.details);
  }
  // Preflight findings in a template are inherited by every page built from it,
  // so the library refuses by default and makes the override explicit and
  // audited rather than letting a human click past a warning strip.
  if (report.preflight && report.preflight.ok === false && allowPreflightErrors !== true) {
    throw new ApiError(
      422,
      `preflight found ${report.preflight.errors.length} error(s); every page built from this template inherits them`,
      "template_preflight_failed",
      { preflight: report.preflight }
    );
  }
  const result = await templates.register(
    { name: report.name, html, title, description, note, source: "admin" },
    actorCtx(req)
  );
  res.status(result.created ? 201 : 200).json({ ...result, validation: report });
}));

// Retire a template. The library is where a mistyped name is noticed, so this is
// reachable from there and not only over MCP. Soft delete: revisions stay, pages
// built from it keep serving, and the name becomes reusable. Refused when pages
// exist unless the caller passes force — which the UI only offers after showing
// how many there are.
router.delete("/templates/:name", h(async (req, res) => {
  const force = req.query.force === "true" || (req.body || {}).force === true;
  res.json(await templates.remove({ template: req.params.name, force }, actorCtx(req)));
}));

// A short-TTL signed URL on the CONTENT host. Same trust split as a page
// preview: untrusted design bytes never render in the dashboard origin.
router.post("/templates/:name/preview-token", h(async (req, res) => {
  const revision = req.body && req.body.revision !== undefined ? toId(req.body.revision) : null;
  const renderMode = req.body && req.body.render_mode === "raw" ? "raw" : "themed";
  const target = await templates.getRevisionHtml(req.params.name, revision);
  const token = rawtoken.mint(
    { pageId: 0, versionId: Number(target.template_version_id), purpose: "template", renderMode },
    300
  );
  res.json({
    template: target.template,
    revision: target.revision,
    render_mode: renderMode,
    content_sha256: target.content_sha256,
    // So the shell can label what it is about to show: a populated design, or
    // the empty state a template ships when it carries no example dataset.
    has_sample_data: target.has_sample_data,
    url: `${CONTENT_ORIGIN}/raw-template/${target.template_version_id}?t=${encodeURIComponent(token)}`,
    expires_in_seconds: 300,
  });
}));

router.post("/pages/*slug/deploy-source", h(async (req, res) => {
  const b = req.body || {};
  if (typeof b.html !== "string" || b.html.length === 0) {
    throw badRequest("html is required", "html_required");
  }
  const r = await versions.deploy(
    {
      slug: slugOf(req),
      html: b.html,
      renderMode: b.render_mode === "raw" ? "raw" : "themed",
      note: typeof b.note === "string" ? b.note.slice(0, 500) : null,
      source: "admin",
      publish: false,
    },
    actorCtx(req)
  );
  res.json({
    version: r.version,
    deduped: r.deduped,
    published: r.published,
    gated: r.gated,
  });
}));

// eslint-disable-next-line no-unused-vars
router.use((err, req, res, _next) => {
  const mapped = err instanceof ApiError ? err : fromDbError(err);
  if (mapped) return res.status(mapped.status).json({ error: mapped.message, code: mapped.code });
  console.error("admin api error:", err.stack || err.message);
  res.status(500).json({ error: "internal error" });
});

module.exports = router;
