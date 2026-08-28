// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/db.js — Postgres access. Pool is created lazily-ish (no connection until
// the first query), so the dashboard host boots even before DB config exists.
// Uses DATABASE_URL when set, else falls back to libpq PG* env vars.

const { Pool } = require("pg");

// Bounded waits (issue #10). Every mutation starts with SELECT … FOR UPDATE —
// deliberate per-page writer serialization — so without bounds one wedged
// transaction pins a pool client per waiter and a single page can exhaust the
// pool (default size 10) and hang BOTH hosts. Defaults comfortably exceed
// legitimate work (2 MB HTML insert); the goal is bounding pathological waits,
// not tuning. Env-overridable; see .env.example.
function envMs(name, def) {
  const v = Number(process.env[name]);
  return Number.isFinite(v) && v >= 0 ? v : def;
}
const CONNECT_TIMEOUT_MS = envMs("PG_CONNECT_TIMEOUT_MS", 5000); // fail fast when PG is down
const STATEMENT_TIMEOUT_MS = envMs("PG_STATEMENT_TIMEOUT_MS", 15000); // runaway query bound
const LOCK_TIMEOUT_MS = envMs("PG_LOCK_TIMEOUT_MS", 10000); // row-lock convoy bound
const IDLE_TXN_TIMEOUT_MS = envMs("PG_IDLE_TXN_TIMEOUT_MS", 30000); // wedged-txn reaper

const poolConfig = {
  connectionTimeoutMillis: CONNECT_TIMEOUT_MS,
  // Server-side per-session bounds via libpq startup options, so every pooled
  // connection carries them (0 = disabled, matching PG semantics).
  options:
    `-c statement_timeout=${STATEMENT_TIMEOUT_MS}` +
    ` -c lock_timeout=${LOCK_TIMEOUT_MS}` +
    ` -c idle_in_transaction_session_timeout=${IDLE_TXN_TIMEOUT_MS}`,
};

const pool = process.env.DATABASE_URL
  ? new Pool({ connectionString: process.env.DATABASE_URL, ...poolConfig })
  : new Pool(poolConfig);

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

// getPublicPortal — everything the content host needs to gate
// /portal/<slug>: the portal's identity and the credential that opens it.
// Retired portals are invisible here, which is what makes retiring a portal the
// kill switch for its credential.
async function getPublicPortal(slug) {
  const { rows } = await query(
    `SELECT id, slug, name, password_hash, home_page_id
       FROM page_portals
      WHERE slug = $1 AND deleted_at IS NULL`,
    [slug]
  );
  return rows[0] || null;
}

// getPortalPages — the dashboards a portal may actually open, in curated order.
//
// The filter matches serve()'s 404 conditions exactly (soft-deleted, taken down,
// or nothing published), because a portal index that lists a page which then
// answers 404 is worse than one that omits it: the partner cannot tell a
// permissions problem from a broken link. `label` wins over `pages.title`
// because a title is agent-settable. The home page sorts first — that is what
// home_page_id is FOR, so the macro view is not buried among the campaign
// dashboards — and sort_order decides the rest.
//
// The live version is joined for its timestamps, which are the partner's only
// freshness signal (#176): source_as_of says when the DATA is from, created_at
// when the live version was made. LEFT, not INNER, even though the WHERE already
// requires published_version_id — the invariant above is that this list matches
// serve()'s 404 conditions exactly, and an inner join would let a dangling
// pointer silently drop a page the partner can still open.
async function getPortalPages(portalId) {
  const { rows } = await query(
    `SELECT p.id, p.slug,
            COALESCE(m.label, NULLIF(p.title, ''), p.slug) AS title,
            m.sort_order,
            (pp.home_page_id = p.id) AS is_home,
            v.created_at AS published_at,
            v.source_as_of AS source_as_of
       FROM page_portal_members m
       JOIN page_portals pp ON pp.id = m.portal_id
       JOIN pages p ON p.id = m.page_id
       LEFT JOIN page_versions v ON v.id = p.published_version_id
      WHERE m.portal_id = $1
        AND p.deleted_at IS NULL
        AND NOT p.disabled
        AND p.published_version_id IS NOT NULL
      ORDER BY (pp.home_page_id = p.id) DESC,
               m.sort_order,
               lower(COALESCE(m.label, NULLIF(p.title, ''), p.slug)),
               p.slug`,
    [portalId]
  );
  return rows;
}

// getPortalsForPage — every live portal that currently contains this page, in a
// deterministic order. This is the membership half of the serve predicate, and it
// is read per request rather than baked into a session: removing a page from a
// portal has to be effective on the very next request, not in thirty days.
//
// Ordered by id because a viewer can legitimately hold cookies for several
// portals — a page may sit in more than one — and the portal that authorizes a
// request decides which sibling list they are shown. That answer must be the same
// on every request, so the tie-break is the lowest portal id, always.
async function getPortalsForPage(pageId) {
  const { rows } = await query(
    `SELECT pp.id, pp.slug, pp.name, pp.password_hash
       FROM page_portal_members m
       JOIN page_portals pp ON pp.id = m.portal_id
      WHERE m.page_id = $1 AND pp.deleted_at IS NULL
      ORDER BY pp.id`,
    [pageId]
  );
  return rows;
}

module.exports = {
  pool,
  query,
  withTransaction,
  getRenderable,
  getPublicPage,
  getPublicPortal,
  getPortalPages,
  getPortalsForPage,
};
