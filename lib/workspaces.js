// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/workspaces.js — one-level page organization. Workspaces do
// not participate in serving or versioning: moving a page only changes its
// nullable workspace_id. Reversible organization is available to authenticated
// users and agents; destructive removal remains human-admin-only. Every
// mutation is transactional and audit-logged with the data change.

const db = require("./db");
const audit = require("./audit");
const versions = require("./versions");
const { badRequest, forbidden, notFound, conflict } = require("./apierror");

// Creating, renaming, and assigning only change reversible organization
// metadata. An authenticated agent has the same bounded authority as a user.
function assertOrganizer(actorCtx) {
  if (!actorCtx || (actorCtx.actorType !== "user" && actorCtx.actorType !== "agent")) {
    throw forbidden("workspace organization requires an authenticated user or agent", "organizer_only");
  }
}

// Removing a workspace detaches every member in bulk and is intentionally a
// separate, stricter trust boundary: only a human admin may do it.
function assertHumanAdmin(actorCtx) {
  if (!actorCtx || actorCtx.actorType !== "user") {
    throw forbidden("removing a workspace is admin-only", "admin_only");
  }
}

function normalizeId(value, { nullable = false } = {}) {
  if (nullable && (value === null || value === undefined || value === "")) return null;
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) return value;
  if (typeof value === "string" && /^[1-9][0-9]*$/.test(value)) {
    const id = BigInt(value);
    if (id <= 9223372036854775807n) return id <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(id) : value;
  }
  throw badRequest("workspace_id must be a positive database id or null", "bad_workspace_id");
}

function normalizeName(value) {
  if (typeof value !== "string") {
    throw badRequest("workspace name is required", "workspace_name_required");
  }
  if (/[\u0000-\u001f\u007f]/.test(value)) {
    throw badRequest("workspace name contains unsupported control characters", "bad_workspace_name");
  }
  const name = value.trim().replace(/\s+/g, " ");
  if (!name) throw badRequest("workspace name is required", "workspace_name_required");
  if (name.length > 100) {
    throw badRequest("workspace name must be 100 characters or fewer", "workspace_name_too_long");
  }
  return name;
}

async function list(executor = db) {
  const { rows } = await executor.query(
    `SELECT w.id, w.name, w.created_at, w.updated_at,
            COUNT(p.id) FILTER (WHERE p.deleted_at IS NULL)::int AS page_count
       FROM workspaces w
       LEFT JOIN pages p ON p.workspace_id = w.id
      GROUP BY w.id
      ORDER BY lower(w.name), w.id`
  );
  return rows;
}

async function create({ name }, actorCtx) {
  assertOrganizer(actorCtx);
  name = normalizeName(name);
  return db.withTransaction(async (client) => {
    let row;
    try {
      const { rows } = await client.query(
        `INSERT INTO workspaces (name) VALUES ($1)
         RETURNING id, name, created_at, updated_at`,
        [name]
      );
      row = rows[0];
    } catch (err) {
      if (err && err.code === "23505") {
        throw conflict(`workspace already exists: ${name}`, "workspace_exists");
      }
      throw err;
    }
    await audit.write(client, {
      ...actorCtx,
      action: "create_workspace",
      metadata: { workspace_id: row.id, name: row.name },
    });
    return { ...row, page_count: 0 };
  });
}

async function rename({ id, name }, actorCtx) {
  assertOrganizer(actorCtx);
  id = normalizeId(id);
  name = normalizeName(name);
  return db.withTransaction(async (client) => {
    const current = await client.query(
      `SELECT id, name, created_at, updated_at FROM workspaces WHERE id = $1 FOR UPDATE`,
      [id]
    );
    if (!current.rows[0]) throw notFound("workspace not found", "workspace_not_found");
    if (current.rows[0].name === name) return current.rows[0];
    let row;
    try {
      const { rows } = await client.query(
        `UPDATE workspaces SET name = $2, updated_at = now() WHERE id = $1
         RETURNING id, name, created_at, updated_at`,
        [id, name]
      );
      row = rows[0];
    } catch (err) {
      if (err && err.code === "23505") {
        throw conflict(`workspace already exists: ${name}`, "workspace_exists");
      }
      throw err;
    }
    await audit.write(client, {
      ...actorCtx,
      action: "rename_workspace",
      metadata: { workspace_id: row.id, from: current.rows[0].name, to: row.name },
    });
    return row;
  });
}

async function remove({ id }, actorCtx) {
  assertHumanAdmin(actorCtx);
  id = normalizeId(id);
  return db.withTransaction(async (client) => {
    const current = await client.query(
      `SELECT id, name FROM workspaces WHERE id = $1 FOR UPDATE`,
      [id]
    );
    if (!current.rows[0]) throw notFound("workspace not found", "workspace_not_found");

    // Detach explicitly so the membership change can update page timestamps and
    // receive a per-page audit entry. The FK's ON DELETE SET NULL remains a
    // defense-in-depth guarantee for any future deletion path.
    const detached = await client.query(
      `UPDATE pages
          SET workspace_id = NULL, updated_at = now()
        WHERE workspace_id = $1
        RETURNING id, deleted_at`,
      [id]
    );
    for (const page of detached.rows) {
      await audit.write(client, {
        ...actorCtx,
        action: "set_workspace",
        pageId: page.id,
        metadata: { from: id, to: null, reason: "workspace_removed" },
      });
    }
    await client.query(`DELETE FROM workspaces WHERE id = $1`, [id]);
    const activeCount = detached.rows.filter((p) => !p.deleted_at).length;
    await audit.write(client, {
      ...actorCtx,
      action: "delete_workspace",
      metadata: {
        workspace_id: id,
        name: current.rows[0].name,
        ungrouped_page_count: activeCount,
      },
    });
    return {
      id: current.rows[0].id,
      name: current.rows[0].name,
      deleted: true,
      ungrouped_page_count: activeCount,
    };
  });
}

async function assignPage({ slug, workspaceId }, actorCtx) {
  assertOrganizer(actorCtx);
  slug = versions.normalizeSlug(slug);
  workspaceId = normalizeId(workspaceId, { nullable: true });
  return db.withTransaction(async (client) => {
    let workspace = null;
    if (workspaceId !== null) {
      // Lock the workspace before the page. remove() takes the same order,
      // preventing assign/delete deadlocks and making deletion races deterministic.
      const found = await client.query(
        `SELECT id, name FROM workspaces WHERE id = $1 FOR KEY SHARE`,
        [workspaceId]
      );
      workspace = found.rows[0] || null;
      if (!workspace) throw notFound("workspace not found", "workspace_not_found");
    }
    const foundPage = await client.query(
      `SELECT id, slug, workspace_id
         FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`,
      [slug]
    );
    const page = foundPage.rows[0];
    if (!page) throw notFound(`page not found: ${slug}`, "page_not_found");

    if (String(page.workspace_id || "") !== String(workspaceId || "")) {
      await client.query(
        `UPDATE pages SET workspace_id = $1, updated_at = now() WHERE id = $2`,
        [workspaceId, page.id]
      );
      await audit.write(client, {
        ...actorCtx,
        action: "set_workspace",
        pageId: page.id,
        metadata: { from: page.workspace_id || null, to: workspaceId },
      });
    }
    return {
      slug: page.slug,
      workspace_id: workspaceId,
      workspace_name: workspace ? workspace.name : null,
    };
  });
}

module.exports = {
  assertOrganizer,
  assertHumanAdmin,
  normalizeId,
  normalizeName,
  list,
  create,
  rename,
  remove,
  assignPage,
};
