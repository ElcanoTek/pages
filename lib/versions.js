"use strict";
// lib/versions.js — the version mutation state machine (PLAN.md §5).
//
// "Live" is pointer-is-truth: pages.published_version_id is the ONLY truth for
// what a viewer gets. Content rows (page_versions) are append-only — every
// deploy/edit is a new row; only status/reviewed_* ever mutate (DB trigger
// enforces the rest). publish / approve / rollback each move the pointer in a
// single transaction, and the pointer may only ever reference an `approved`
// version.
//
// Every mutation here:
//   1. runs in a transaction that does `SELECT … FROM pages … FOR UPDATE`
//      FIRST, serializing all writers for that page (no two-live-rows race);
//   2. honors optimistic concurrency: if the caller passes expectedVersion and
//      it != the page's current published_version_id → 409 (reload);
//   3. writes an audit_log row in the SAME transaction (lib/audit.js).
//
// Agent authority depends on the page's approval gate (PLAN.md §5):
//   • open page (require_approval = false): an agent may deploy, publish, roll
//     back — the "make/update the dashboard and it's live" fast path.
//   • approval-gated page: an agent may only create a `pending` version; it
//     cannot publish/approve/rollback. approve/reject are ALWAYS admin-only.

const crypto = require("node:crypto");
const db = require("./db");
const audit = require("./audit");
const { badRequest, forbidden, notFound, conflict } = require("./apierror");

const RENDER_MODES = new Set(["themed", "raw"]);
const VERSION_SOURCES = new Set(["api", "mcp", "admin"]);

function sha256(text) {
  return crypto.createHash("sha256").update(text, "utf8").digest("hex");
}

// ── reads ──────────────────────────────────────────────────────────────────

// listPages — newest first, with each page's currently-published version id.
async function listPages() {
  const { rows } = await db.query(
    `SELECT id, slug, title, client_id, theme_id, require_approval, disabled,
            published_version_id, created_at, updated_at
       FROM pages
      WHERE deleted_at IS NULL
      ORDER BY created_at DESC`
  );
  return rows;
}

// getPage — page metadata + the published version (or null). 404 if absent.
async function getPage(slug) {
  slug = normalizeSlug(slug);
  const { rows } = await db.query(
    `SELECT id, slug, title, client_id, theme_id, require_approval, disabled,
            published_version_id, created_at, updated_at
       FROM pages
      WHERE slug = $1 AND deleted_at IS NULL`,
    [slug]
  );
  const page = rows[0];
  if (!page) throw notFound(`page not found: ${slug}`, "page_not_found");
  let published = null;
  if (page.published_version_id) {
    published = await getVersion(slug, page.published_version_id).catch(() => null);
  }
  return { page, published };
}

// listVersions — full history for a page, newest first.
async function listVersions(slug) {
  const page = await loadPage(slug);
  const { rows } = await db.query(
    `SELECT id, page_id, content_sha256, status, render_mode, author, source,
            note, reviewed_by, reviewed_at, created_at
       FROM page_versions
      WHERE page_id = $1
      ORDER BY created_at DESC, id DESC`,
    [page.id]
  );
  return rows;
}

// getVersion — one version's full row (including html). 404 if not on this page.
async function getVersion(slug, versionId) {
  const page = await loadPage(slug);
  const { rows } = await db.query(
    `SELECT id, page_id, html, content_sha256, status, render_mode, author,
            source, note, reviewed_by, reviewed_at, created_at
       FROM page_versions
      WHERE id = $1 AND page_id = $2`,
    [versionId, page.id]
  );
  const v = rows[0];
  if (!v) throw notFound(`version ${versionId} not found on page ${slug}`, "version_not_found");
  return v;
}

// loadPage — fetch a live (non-deleted) page row or 404. Read-only helper.
async function loadPage(slug) {
  slug = normalizeSlug(slug);
  const { rows } = await db.query(
    `SELECT id, slug, require_approval, disabled, published_version_id
       FROM pages WHERE slug = $1 AND deleted_at IS NULL`,
    [slug]
  );
  if (!rows[0]) throw notFound(`page not found: ${slug}`, "page_not_found");
  return rows[0];
}

// ── page creation ────────────────────────────────────────────────────────────

// createPage — { slug, title?, clientId?, requireApproval? }. theme/password are
// set via their own endpoints later. Slug must be unique.
async function createPage({ slug, title = "", clientId = null, requireApproval = false }, actorCtx) {
  slug = normalizeSlug(slug);
  return db.withTransaction(async (client) => {
    const dup = await client.query("SELECT 1 FROM pages WHERE slug = $1", [slug]);
    if (dup.rowCount) throw conflict(`slug already exists: ${slug}`, "slug_exists");
    const { rows } = await client.query(
      `INSERT INTO pages (slug, title, client_id, require_approval)
       VALUES ($1, $2, $3, $4)
       RETURNING id, slug, title, client_id, theme_id, require_approval, disabled,
                 published_version_id, created_at, updated_at`,
      [slug, title, clientId, !!requireApproval]
    );
    const page = rows[0];
    await audit.write(client, { ...actorCtx, action: "create_page", pageId: page.id });
    return page;
  });
}

// ── deploy (insert a new version) ────────────────────────────────────────────

// deploy — the agent's "make/update the dashboard" call. Inserts a new version.
//   { slug, html, renderMode?, note?, author?, source?, publish?, expectedVersion? }
// Rules (PLAN.md §5):
//   • require_approval page  → status 'pending', `publish` ignored (human gate).
//   • open page              → status 'draft'; if publish=true, promote to
//                              'approved' and move the pointer in this same txn.
//   • dedupe by content_sha256: an identical re-deploy returns the existing row
//     (idempotent) rather than inserting a duplicate.
//   • expectedVersion: optimistic-concurrency guard on the publish step.
async function deploy({ slug, html, renderMode = "themed", note = null, author, source = "api", publish = false, expectedVersion }, actorCtx) {
  if (typeof html !== "string" || html.length === 0) throw badRequest("html is required", "html_required");
  if (!RENDER_MODES.has(renderMode)) throw badRequest(`bad render_mode: ${renderMode}`, "bad_render_mode");
  if (!VERSION_SOURCES.has(source)) throw badRequest(`bad source: ${source}`, "bad_source");
  const versionAuthor = author || actorCtx.actor;
  const contentSha = sha256(html);

  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);

    // Dedupe: identical content for this page returns the existing version.
    const existing = await client.query(
      `SELECT id, page_id, content_sha256, status, render_mode, author, source,
              note, reviewed_by, reviewed_at, created_at
         FROM page_versions
        WHERE page_id = $1 AND content_sha256 = $2
        ORDER BY id DESC LIMIT 1`,
      [page.id, contentSha]
    );
    let version = existing.rows[0];
    let deduped = false;

    if (version) {
      deduped = true;
    } else {
      const gated = page.require_approval;
      const status = gated ? "pending" : "draft";
      const ins = await client.query(
        `INSERT INTO page_versions (page_id, html, content_sha256, status, render_mode, author, source, note)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
         RETURNING id, page_id, content_sha256, status, render_mode, author, source,
                   note, reviewed_by, reviewed_at, created_at`,
        [page.id, html, contentSha, status, renderMode, versionAuthor, source, note]
      );
      version = ins.rows[0];
      await audit.write(client, {
        ...actorCtx,
        action: "deploy",
        pageId: page.id,
        versionId: version.id,
        metadata: { status, render_mode: renderMode, deduped: false },
      });
    }

    // Fast-path publish on open pages only. On approval-gated pages publish is
    // silently ignored (the version waits in the pending queue for a human).
    let publishedNow = false;
    if (publish && !page.require_approval) {
      assertAgentMayMovePointer(page, actorCtx); // open page → ok for agents
      // The pointer may only ever reference an `approved` version, so persist
      // that status before moving it. `rejected` is terminal — never resurrect
      // it via a dedupe+publish; deploy new content instead.
      if (version.status === "rejected") {
        throw conflict("cannot publish a rejected version; deploy new content", "rejected_terminal");
      }
      if (version.status !== "approved") {
        await client.query(`UPDATE page_versions SET status = 'approved' WHERE id = $1`, [version.id]);
        version.status = "approved";
      }
      await movePointer(client, page, version.id, expectedVersion, actorCtx, "publish");
      publishedNow = true;
    }

    return { version, deduped, published: publishedNow, gated: page.require_approval };
  });
}

// ── publish / rollback / approve / reject ────────────────────────────────────

// publish — promote a `draft` to `approved` and make it live. Open pages only
// for agents (PLAN.md §5). { slug, versionId, expectedVersion? }.
async function publish({ slug, versionId, expectedVersion }, actorCtx) {
  if (!versionId) throw badRequest("version_id is required", "version_id_required");
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    assertAgentMayMovePointer(page, actorCtx);
    const v = await lockVersion(client, page.id, versionId);
    if (v.status !== "draft") {
      throw conflict(`can only publish a draft version (status=${v.status})`, "not_draft");
    }
    await client.query(
      `UPDATE page_versions SET status = 'approved' WHERE id = $1`,
      [versionId]
    );
    await movePointer(client, page, versionId, expectedVersion, actorCtx, "publish");
    return { ...v, status: "approved" };
  });
}

// rollback — move the pointer to an already-`approved` version (no status
// change). { slug, versionId?, expectedVersion? }. If versionId is omitted,
// rolls back to the most recent approved version that isn't currently live.
async function rollback({ slug, versionId = null, expectedVersion }, actorCtx) {
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    assertAgentMayMovePointer(page, actorCtx);

    let targetId = versionId;
    if (!targetId) {
      const prev = await client.query(
        `SELECT id FROM page_versions
          WHERE page_id = $1 AND status = 'approved'
            AND id IS DISTINCT FROM $2
          ORDER BY created_at DESC, id DESC LIMIT 1`,
        [page.id, page.published_version_id]
      );
      if (!prev.rows[0]) throw conflict("no earlier approved version to roll back to", "no_rollback_target");
      targetId = prev.rows[0].id;
    }

    const v = await lockVersion(client, page.id, targetId);
    if (v.status !== "approved") {
      throw conflict(`can only roll back to an approved version (status=${v.status})`, "not_approved");
    }
    await movePointer(client, page, targetId, expectedVersion, actorCtx, "rollback");
    return { ...v };
  });
}

// approve — admin-only: stamp a reviewer on a `pending` version, set it
// `approved`, and make it live. The caller (API layer) enforces admin+CSRF;
// here we just require an actorType of 'user'.
async function approve({ slug, versionId, expectedVersion }, actorCtx) {
  assertAdminAction(actorCtx);
  if (!versionId) throw badRequest("version_id is required", "version_id_required");
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    const v = await lockVersion(client, page.id, versionId);
    if (v.status !== "pending") {
      throw conflict(`can only approve a pending version (status=${v.status})`, "not_pending");
    }
    await client.query(
      `UPDATE page_versions SET status = 'approved', reviewed_by = $2, reviewed_at = now() WHERE id = $1`,
      [versionId, actorCtx.actor]
    );
    await movePointer(client, page, versionId, expectedVersion, actorCtx, "approve");
    return { ...v, status: "approved", reviewed_by: actorCtx.actor };
  });
}

// reject — admin-only: mark a draft/pending version `rejected` (terminal; clone
// to reuse). Does NOT move the pointer.
async function reject({ slug, versionId, note = null }, actorCtx) {
  assertAdminAction(actorCtx);
  if (!versionId) throw badRequest("version_id is required", "version_id_required");
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    const v = await lockVersion(client, page.id, versionId);
    if (v.status !== "pending" && v.status !== "draft") {
      throw conflict(`can only reject a draft/pending version (status=${v.status})`, "not_rejectable");
    }
    await client.query(
      `UPDATE page_versions SET status = 'rejected', reviewed_by = $2, reviewed_at = now() WHERE id = $1`,
      [versionId, actorCtx.actor]
    );
    await audit.write(client, {
      ...actorCtx,
      action: "reject",
      pageId: page.id,
      versionId,
      metadata: note ? { note } : null,
    });
    return { ...v, status: "rejected", reviewed_by: actorCtx.actor };
  });
}

// ── admin-only page settings (PLAN §5, §7) ───────────────────────────────────
// These never touch the pointer or version content — they flip page flags /
// theme. Admin-cookie+CSRF is enforced at the API layer; here we require an
// actorType of 'user' as defense in depth.

// setDisabled — the takedown kill switch. disabled pages refuse to render.
async function setDisabled({ slug, disabled }, actorCtx) {
  assertAdminAction(actorCtx);
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    await client.query(`UPDATE pages SET disabled = $1, updated_at = now() WHERE id = $2`, [!!disabled, page.id]);
    await audit.write(client, { ...actorCtx, action: disabled ? "disable" : "enable", pageId: page.id });
    return { slug: page.slug, disabled: !!disabled };
  });
}

// setApproval — toggle the per-page approval gate.
async function setApproval({ slug, requireApproval }, actorCtx) {
  assertAdminAction(actorCtx);
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    await client.query(`UPDATE pages SET require_approval = $1, updated_at = now() WHERE id = $2`, [!!requireApproval, page.id]);
    await audit.write(client, { ...actorCtx, action: "set_approval", pageId: page.id, metadata: { require_approval: !!requireApproval } });
    return { slug: page.slug, require_approval: !!requireApproval };
  });
}

// setTheme — point the page at a curated theme by name (NULL/'flag' ⇒ Flag default).
async function setTheme({ slug, theme }, actorCtx) {
  assertAdminAction(actorCtx);
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    let themeId = null;
    if (theme && theme !== "flag") {
      const { rows } = await client.query(`SELECT id FROM themes WHERE name = $1`, [theme]);
      if (!rows[0]) throw badRequest(`unknown theme: ${theme}`, "unknown_theme");
      themeId = rows[0].id;
    }
    await client.query(`UPDATE pages SET theme_id = $1, updated_at = now() WHERE id = $2`, [themeId, page.id]);
    await audit.write(client, { ...actorCtx, action: "set_theme", pageId: page.id, metadata: { theme: theme || "flag" } });
    return { slug: page.slug, theme: theme || "flag" };
  });
}

// listThemes — names available to the theme picker.
async function listThemes() {
  const { rows } = await db.query(`SELECT id, name, default_mode FROM themes ORDER BY name`);
  return rows;
}

// ── internal helpers (all operate on the txn client) ─────────────────────────

// lockPage — SELECT … FOR UPDATE, serializing writers for this page. The first
// statement of every mutation, so the row lock is held for the whole txn.
async function lockPage(client, slug) {
  slug = normalizeSlug(slug);
  const { rows } = await client.query(
    `SELECT id, slug, require_approval, disabled, published_version_id
       FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`,
    [slug]
  );
  if (!rows[0]) throw notFound(`page not found: ${slug}`, "page_not_found");
  return rows[0];
}

async function lockVersion(client, pageId, versionId) {
  const { rows } = await client.query(
    `SELECT id, page_id, content_sha256, status, render_mode, author, source,
            note, reviewed_by, reviewed_at, created_at
       FROM page_versions WHERE id = $1 AND page_id = $2 FOR UPDATE`,
    [versionId, pageId]
  );
  if (!rows[0]) throw notFound(`version ${versionId} not found on this page`, "version_not_found");
  return rows[0];
}

// movePointer — the single statement that changes what's "live": set
// pages.published_version_id. Validates the optimistic-concurrency token and
// writes the audit row, all inside the caller's txn.
async function movePointer(client, page, versionId, expectedVersion, actorCtx, action) {
  if (expectedVersion !== undefined && expectedVersion !== null) {
    if (Number(expectedVersion) !== Number(page.published_version_id || 0)) {
      throw conflict(
        `published version changed (expected ${expectedVersion}, is ${page.published_version_id})`,
        "stale_version"
      );
    }
  }
  await client.query(
    `UPDATE pages SET published_version_id = $1, updated_at = now() WHERE id = $2`,
    [versionId, page.id]
  );
  await audit.write(client, {
    ...actorCtx,
    action,
    pageId: page.id,
    versionId,
    metadata: { from: page.published_version_id || null, to: versionId },
  });
}

// On an approval-gated page, agents may not move the pointer (publish/rollback).
// Humans (admin) always may. open page → anyone authorized may.
function assertAgentMayMovePointer(page, actorCtx) {
  if (page.require_approval && actorCtx.actorType !== "user") {
    throw forbidden(
      "this page requires approval — agents may only deploy a pending version; a human must approve",
      "approval_required"
    );
  }
}

function assertAdminAction(actorCtx) {
  if (actorCtx.actorType !== "user") {
    throw forbidden("approve/reject are admin-only", "admin_only");
  }
}

function normalizeSlug(slug) {
  if (typeof slug !== "string") throw badRequest("slug is required", "slug_required");
  const s = slug.trim().toLowerCase();
  // flat ('omnicom') or nested ('omnicom/q2-report'); url-safe segments only.
  if (!/^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:\/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$/.test(s)) {
    throw badRequest("slug must be url-safe (a-z 0-9 - _ /)", "bad_slug");
  }
  return s;
}

module.exports = {
  sha256,
  normalizeSlug,
  listPages,
  getPage,
  listVersions,
  getVersion,
  createPage,
  deploy,
  publish,
  rollback,
  approve,
  reject,
  setDisabled,
  setApproval,
  setTheme,
  listThemes,
};
