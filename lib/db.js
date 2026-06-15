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

module.exports = { pool, query, getRenderable };
