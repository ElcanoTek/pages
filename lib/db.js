"use strict";
// lib/db.js — Postgres access. Pool is created lazily-ish (no connection until
// the first query), so the dashboard host boots even before DB config exists.
// Uses DATABASE_URL when set, else falls back to libpq PG* env vars.

const { Pool } = require("pg");

const pool = process.env.DATABASE_URL
  ? new Pool({ connectionString: process.env.DATABASE_URL })
  : new Pool();

pool.on("error", (err) => console.error("pg pool error:", err.message));

function query(text, params) {
  return pool.query(text, params);
}

// withTransaction(fn) — run fn(client) inside BEGIN/COMMIT, rolling back on any
// throw and always releasing the client. Used by the version state machine so a
// mutation and its audit_log row commit atomically (PLAN.md §5).
async function withTransaction(fn) {
  const client = await pool.connect();
  try {
    await client.query("BEGIN");
    const result = await fn(client);
    await client.query("COMMIT");
    return result;
  } catch (err) {
    try {
      await client.query("ROLLBACK");
    } catch {
      /* connection may be dead; release below regardless */
    }
    throw err;
  } finally {
    client.release();
  }
}

// getRenderable — everything /raw needs to render one version: the source,
// its mode, the owning page's slug/disabled flag, and the resolved theme.
async function getRenderable(versionId) {
  const { rows } = await query(
    `SELECT v.id, v.html, v.render_mode, v.page_id,
            p.slug, p.disabled,
            t.override_css, t.default_mode
       FROM page_versions v
       JOIN pages p  ON p.id = v.page_id
       LEFT JOIN themes t ON t.id = p.theme_id
      WHERE v.id = $1 AND p.deleted_at IS NULL`,
    [versionId]
  );
  return rows[0] || null;
}

// getPublicPage — everything the content host needs to serve a slug DIRECTLY
// (PLAN §6b direct-serve): the page's gate state (disabled, password_hash), its
// published pointer, and the published version's renderable fields. Returns the
// row even when nothing is published (html/render_mode null) so the caller can
// distinguish "no such page" (null) from "published nothing yet".
async function getPublicPage(slug) {
  const { rows } = await query(
    `SELECT p.id, p.slug, p.disabled, p.password_hash, p.published_version_id,
            v.html, v.render_mode, t.override_css
       FROM pages p
       LEFT JOIN page_versions v ON v.id = p.published_version_id
       LEFT JOIN themes t ON t.id = p.theme_id
      WHERE p.slug = $1 AND p.deleted_at IS NULL`,
    [slug]
  );
  return rows[0] || null;
}

module.exports = { pool, query, withTransaction, getRenderable, getPublicPage };
