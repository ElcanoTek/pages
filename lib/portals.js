// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/portals.js — partner portals: ONE shared client credential over an
// admin-curated SET of pages, plus the membership a page switcher will read.
//
// Authority is the whole point of this module. A partner portal decides which
// client's numbers a given credential opens, so an agent must never be able to
// add a page to one, or move a page between two of them — that is the difference
// between "West sees his six Fabrikam dashboards" and "West sees NWM's". Every
// mutation therefore calls assertPortalAdmin FIRST, before any validation or
// database work, and lib/mcp-tools.js registers nothing from this file: portals
// add zero agent-reachable surface.
//
// The transaction and audit SHAPE is copied from lib/workspaces.js; its guards
// deliberately are NOT. `assertOrganizer` there accepts actorType "agent", the
// exact inverse of what portals need, so portals carry their own guard and their
// own `portal_admin_only` error code — a grep for one can never turn up the
// other.
//
// This module does not participate in serving. Nothing on the content host reads
// page_portals or page_portal_members yet; the portal session, the serve()
// predicate and the in-page switcher land in later PRs. Adding a page to a
// portal today changes nothing a client can observe.

const crypto = require("node:crypto");
const db = require("./db");
const audit = require("./audit");
const pagecookie = require("./pagecookie");
const versions = require("./versions");
const { badRequest, forbidden, notFound, conflict } = require("./apierror");
const { CONTENT_ORIGIN } = require("./csp");

// A page in N portals means N candidate scrypt verifications on one password
// submission once the serve predicate lands (each 30-80 ms, and libuv's default
// threadpool is 4 threads), so fan-out is capped where it is created rather than
// where it would hurt. Four covers the real shape — a dashboard shared between a
// client portal, an agency portal and a per-person subset.
const MAX_PORTALS_PER_PAGE = 4;
const MAX_NAME_LENGTH = 100;
const MAX_LABEL_LENGTH = 200;
const MAX_SLUG_LENGTH = 64;
const MAX_SORT_ORDER = 9999;
// A portal password opens every dashboard in the portal, so it is worth more
// than a per-page one and is held to a floor versions.setPassword has never had.
// The generated form is the intended path; the floor exists for the admin who
// pastes their own.
const MIN_PASSWORD_LENGTH = 16;
const MAX_PASSWORD_LENGTH = 512; // pagecookie.hashPassword truncates past this
// 31 symbols with no look-alikes (no i/l/o/0/1): 16 characters is ~79 bits, well
// clear of the ~64-bit floor, and still readable down a phone line.
const PASSWORD_ALPHABET = "abcdefghjkmnpqrstuvwxyz23456789";
const PASSWORD_GROUPS = 4;
const PASSWORD_GROUP_LENGTH = 4;

// Every mutation in this module. Exported so the authority test can be written
// table-driven over the real list instead of a copy of it: a verb added here
// without a matching case fails the test, and a verb added to the module without
// being listed here fails it too.
const MUTATIONS = [
  "create",
  "rename",
  "setPassword",
  "addPage",
  "updatePage",
  "removePage",
  "setHome",
  "remove",
];

// A human admin, always. Not "an authenticated principal": the SSO audience is
// wider than Elcano staff and bearer tokens belong to agents, so this is the
// narrowest actor the codebase has (cf. lib/csrf.js requireAdminJSON, which is
// what puts actorType "user" on the request in the first place).
function assertPortalAdmin(actorCtx) {
  if (!actorCtx || actorCtx.actorType !== "user") {
    throw forbidden(
      "partner portals are admin-only: who may see which dashboards is a human decision",
      "portal_admin_only"
    );
  }
}

// ── validation ───────────────────────────────────────────────────────────────

function normalizeId(value, field = "portal_id") {
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) return value;
  if (typeof value === "string" && /^[1-9][0-9]*$/.test(value)) {
    const id = BigInt(value);
    if (id <= 9223372036854775807n) return id <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(id) : value;
  }
  throw badRequest(`${field} must be a positive database id`, `bad_${field}`);
}

// One segment only — the portal URL is <content-host>/portal/<slug>.
function normalizeSlug(value) {
  if (typeof value !== "string" || !value.trim()) {
    throw badRequest("portal slug is required", "portal_slug_required");
  }
  const slug = value.trim().toLowerCase();
  if (!/^[a-z0-9]+(?:[-_][a-z0-9]+)*$/.test(slug)) {
    throw badRequest("portal slug must be one url-safe segment (a-z 0-9 - _)", "bad_portal_slug");
  }
  if (slug.length > MAX_SLUG_LENGTH) {
    throw badRequest(`portal slug must be ${MAX_SLUG_LENGTH} characters or fewer`, "bad_portal_slug");
  }
  return slug;
}

function normalizeName(value) {
  if (typeof value !== "string") throw badRequest("portal name is required", "portal_name_required");
  if (/[\u0000-\u001f\u007f]/.test(value)) {
    throw badRequest("portal name contains unsupported control characters", "bad_portal_name");
  }
  const name = value.trim().replace(/\s+/g, " ");
  if (!name) throw badRequest("portal name is required", "portal_name_required");
  if (name.length > MAX_NAME_LENGTH) {
    throw badRequest(`portal name must be ${MAX_NAME_LENGTH} characters or fewer`, "portal_name_too_long");
  }
  return name;
}

// The partner-facing title for one membership. null/'' clears it, which means
// "fall back to pages.title".
function normalizeLabel(value) {
  if (value === null || value === undefined || value === "") return null;
  if (typeof value !== "string") throw badRequest("portal label must be a string or null", "bad_portal_label");
  if (/[\u0000-\u001f\u007f]/.test(value)) {
    throw badRequest("portal label contains unsupported control characters", "bad_portal_label");
  }
  const label = value.trim().replace(/\s+/g, " ");
  if (!label) return null;
  if (label.length > MAX_LABEL_LENGTH) {
    throw badRequest(`portal label must be ${MAX_LABEL_LENGTH} characters or fewer`, "bad_portal_label");
  }
  return label;
}

function normalizeSortOrder(value) {
  if (value === null || value === undefined || value === "") return 0;
  const n = typeof value === "string" && /^[0-9]+$/.test(value) ? Number(value) : value;
  if (typeof n !== "number" || !Number.isInteger(n) || n < 0 || n > MAX_SORT_ORDER) {
    throw badRequest(`sort_order must be an integer between 0 and ${MAX_SORT_ORDER}`, "bad_portal_sort_order");
  }
  return n;
}

// generatePassword — the default, because this one secret opens every dashboard
// in the portal and a human-chosen one would be the weakest part of the feature.
// Grouped so it can be read aloud and typed once.
function generatePassword() {
  const groups = [];
  for (let g = 0; g < PASSWORD_GROUPS; g++) {
    let group = "";
    for (let i = 0; i < PASSWORD_GROUP_LENGTH; i++) {
      group += PASSWORD_ALPHABET[crypto.randomInt(PASSWORD_ALPHABET.length)];
    }
    groups.push(group);
  }
  return groups.join("-");
}

// A supplied password is checked, never silently repaired: trimming one would
// change the secret behind the admin's back, and the copy-paste-a-newline case
// is far better as an error at set time than as "the password doesn't work".
function assertPasswordAcceptable(value) {
  if (typeof value !== "string") throw badRequest("portal password must be a string", "bad_portal_password");
  if (/[\u0000-\u001f\u007f]/.test(value)) {
    throw badRequest("portal password contains unsupported control characters", "bad_portal_password");
  }
  if (value !== value.trim()) {
    throw badRequest("portal password must not begin or end with whitespace", "bad_portal_password");
  }
  if (value.length < MIN_PASSWORD_LENGTH) {
    throw badRequest(
      `portal password must be at least ${MIN_PASSWORD_LENGTH} characters — omit it and Pages generates a strong one`,
      "portal_password_too_weak"
    );
  }
  if (value.length > MAX_PASSWORD_LENGTH) {
    throw badRequest(`portal password must be ${MAX_PASSWORD_LENGTH} characters or fewer`, "portal_password_too_long");
  }
  return value;
}

function sameId(a, b) {
  return a !== null && a !== undefined && b !== null && b !== undefined && String(a) === String(b);
}

// One membership is addressed either by the page's slug (the normal path) or by
// its database id. The id path exists for a membership row whose page has since
// been soft-deleted: its slug no longer resolves — and may even belong to a
// different, newer page — so without it that row could never be removed without
// hand-written SQL.
function normalizeMemberTarget({ slug, pageId }) {
  const hasSlug = slug !== null && slug !== undefined && slug !== "";
  const hasId = pageId !== null && pageId !== undefined && pageId !== "";
  if (hasSlug && hasId) throw badRequest("pass either slug or page_id, not both", "bad_portal_member_target");
  if (hasSlug) return { slug: versions.normalizeSlug(slug), pageId: null };
  if (hasId) return { slug: null, pageId: normalizeId(pageId, "page_id") };
  throw badRequest("a page slug or page_id is required", "portal_member_target_required");
}

// ── reads ────────────────────────────────────────────────────────────────────

const PORTAL_COLUMNS = "id, slug, name, home_page_id, created_at, updated_at";

async function list(executor = db) {
  const { rows } = await executor.query(
    `SELECT pp.id, pp.slug, pp.name, pp.home_page_id, hp.slug AS home_page_slug,
            pp.created_at, pp.updated_at,
            COUNT(p.id) FILTER (WHERE p.deleted_at IS NULL)::int AS page_count
       FROM page_portals pp
       LEFT JOIN page_portal_members m ON m.portal_id = pp.id
       LEFT JOIN pages p ON p.id = m.page_id
       LEFT JOIN pages hp ON hp.id = pp.home_page_id AND hp.deleted_at IS NULL
      WHERE pp.deleted_at IS NULL
      GROUP BY pp.id, hp.slug
      ORDER BY lower(pp.name), pp.id`
  );
  return rows;
}

// get — one portal and its membership, for the admin screen. Members whose page
// has been soft-deleted are RETURNED, flagged `page_deleted`, so a stale row is
// visible and removable instead of invisible and permanent.
async function get({ id }, executor = db) {
  id = normalizeId(id);
  const found = await executor.query(
    `SELECT pp.id, pp.slug, pp.name, pp.home_page_id, hp.slug AS home_page_slug,
            pp.created_at, pp.updated_at
       FROM page_portals pp
       LEFT JOIN pages hp ON hp.id = pp.home_page_id AND hp.deleted_at IS NULL
      WHERE pp.id = $1 AND pp.deleted_at IS NULL`,
    [id]
  );
  const portal = found.rows[0];
  if (!portal) throw notFound("portal not found", "portal_not_found");
  const { rows: members } = await executor.query(
    `SELECT m.page_id, p.slug, p.title, m.label,
            COALESCE(m.label, NULLIF(p.title, ''), p.slug) AS display_title,
            m.sort_order, m.added_at,
            (p.password_hash IS NOT NULL) AS has_password,
            p.disabled,
            (p.published_version_id IS NOT NULL) AS published,
            (p.deleted_at IS NOT NULL) AS page_deleted,
            -- Will this dashboard show the Page menu, and whose menu is it? Since
            -- #125 EVERY portal-authorised render gets one — themed via the
            -- template block or Pages' built-in control, raw via the injected
            -- fixed-position control — so any published member shows it, and the
            -- old "raw gets nothing" warning would now be telling admins to
            -- redeploy pages that are fine. What is still worth surfacing is
            -- whose menu it is — matched on actually CONSUMING the block (a DOM
            -- read or an element carrying the id), mirroring render.readsNavBlock:
            -- authoring boilerplate mentions #pages-nav in a CSS comment without
            -- ever building the control, and a bare-mention match called that
            -- "owns its menu" while the page showed none. The regex runs in the
            -- database, so the 2 MiB document stays there.
            (v.id IS NOT NULL) AS shows_switcher,
            (v.html ~ 'getElementById\\s*\\(\\s*["'']pages-nav["'']\\s*\\)|querySelector(All)?\\s*\\(\\s*["'']#?pages-nav["'']\\s*\\)|\\yid\\s*=\\s*["'']pages-nav["'']') AS switcher_is_own,
            v.render_mode
       FROM page_portal_members m
       JOIN pages p ON p.id = m.page_id
       LEFT JOIN page_versions v ON v.id = p.published_version_id
      WHERE m.portal_id = $1
      ORDER BY m.sort_order, lower(COALESCE(m.label, NULLIF(p.title, ''), p.slug)), p.slug`,
    [id]
  );
  return {
    portal: { ...portal, page_count: members.filter((row) => !row.page_deleted).length },
    members,
    link_audit: await linkAudit(executor, portal, members),
  };
}

// ── link audit (read-only) ─────────────────────────────────────────────────
// The observed shape of portal drift (Fabrikam, 2026-08-19): the home page is a
// hand-authored hub whose links agents keep current, while membership — the
// thing that actually authorises a partner and carries the Page menu — is
// curated by a human and silently falls behind. A delete→recreate even drops a
// member with nobody touching the list, because membership binds to page_id.
// The partner (or a staff viewer) then clicks a link the hub proudly shows and
// the nav vanishes — or, for a partner, a password wall appears. This audit
// makes that drift visible exactly where the human curates: same-host links in
// the HOME page's published HTML that resolve to live pages not in the member
// list. Read-only by design — membership stays a human decision; the screen
// offers the click.

const LINK_AUDIT_MAX_CANDIDATES = 200;

// One nested slug: segments of [a-z0-9] with inner -/_, joined by "/". The same
// shape pages.slug enforces, so anything else cannot be a page and is skipped
// without a database round-trip.
const LINKABLE_SLUG_RE = /^[a-z0-9]+([-_][a-z0-9]+)*(\/[a-z0-9]+([-_][a-z0-9]+)*)*$/;

// Paths under the content host that are routes, not page slugs.
const NON_PAGE_PREFIXES = ["portal/", "raw/", "raw-template/", "shell-assets/"];

// extractLinkedSlugs — pure. Pulls candidate page slugs out of href attributes:
// absolute links on the content origin and root-relative links. Bare-relative
// hrefs are skipped on purpose — slugs nest, so on /a/b/c a relative "d"
// resolves to /a/b/d and naming the author's intent from here is a guess.
function extractLinkedSlugs(html, contentOrigin = CONTENT_ORIGIN) {
  const seen = new Set();
  const re = /href\s*=\s*(?:"([^"]*)"|'([^']*)')/gi;
  let match;
  while ((match = re.exec(String(html || ""))) !== null && seen.size < LINK_AUDIT_MAX_CANDIDATES) {
    let target = (match[1] ?? match[2] ?? "").trim();
    if (target.startsWith(contentOrigin + "/")) {
      target = target.slice(contentOrigin.length);
    } else if (target.startsWith("//") || /^[a-z][a-z0-9+.-]*:/i.test(target)) {
      continue; // another host or scheme (https://elsewhere, mailto:, …)
    }
    if (!target.startsWith("/")) continue;
    const slug = target.slice(1).split(/[?#]/, 1)[0].replace(/\/+$/, "");
    if (!slug || !LINKABLE_SLUG_RE.test(slug)) continue;
    if (NON_PAGE_PREFIXES.some((prefix) => slug === prefix.slice(0, -1) || slug.startsWith(prefix))) continue;
    seen.add(slug);
  }
  return [...seen];
}

// linkAudit — the detail-read companion of extractLinkedSlugs. Never throws:
// an audit that cannot run must not take the admin screen down with it, so any
// failure degrades to { scanned: false } and a log line.
async function linkAudit(executor, portal, members) {
  try {
    if (!portal.home_page_id) return { scanned: false, missing: [] };
    const { rows } = await executor.query(
      `SELECT v.html
         FROM pages p
         JOIN page_versions v ON v.id = p.published_version_id
        WHERE p.id = $1 AND p.deleted_at IS NULL`,
      [portal.home_page_id]
    );
    if (!rows[0]) return { scanned: false, missing: [] };
    const slugs = extractLinkedSlugs(rows[0].html);
    if (slugs.length === 0) return { scanned: true, missing: [] };
    const memberIds = new Set(members.map((m) => String(m.page_id)));
    const { rows: linked } = await executor.query(
      `SELECT id, slug, title
         FROM pages
        WHERE slug = ANY($1)
          AND deleted_at IS NULL
          AND NOT disabled
          AND published_version_id IS NOT NULL
        ORDER BY slug`,
      [slugs]
    );
    return {
      scanned: true,
      missing: linked
        .filter((p) => !memberIds.has(String(p.id)) && String(p.id) !== String(portal.home_page_id))
        .map((p) => ({ page_id: p.id, slug: p.slug, title: p.title })),
    };
  } catch (err) {
    console.error("portals: link audit unavailable:", err.message);
    return { scanned: false, missing: [] };
  }
}

// listForPage — the other direction: which portals expose this page. The admin
// page detail needs it, because a page missing from one audience's portal is
// invisible when you only ever look portal-first (design risk: portal sprawl).
async function listForPage(pageId, executor = db) {
  const { rows } = await executor.query(
    `SELECT pp.id, pp.slug, pp.name, m.label, m.sort_order,
            (pp.home_page_id = m.page_id) AS is_home
       FROM page_portal_members m
       JOIN page_portals pp ON pp.id = m.portal_id
      WHERE m.page_id = $1 AND pp.deleted_at IS NULL
      ORDER BY lower(pp.name), pp.id`,
    [pageId]
  );
  return rows;
}

// ── mutations (admin only, every one audited in its own transaction) ──────────

async function create({ slug, name, password }, actorCtx) {
  assertPortalAdmin(actorCtx);
  slug = normalizeSlug(slug);
  name = normalizeName(name);
  const generated = password === null || password === undefined || password === "";
  const secret = generated ? generatePassword() : assertPasswordAcceptable(password);
  // scrypt is deliberately expensive: hash BEFORE opening the transaction so the
  // KDF never runs while holding a row lock (mirrors versions.setPassword).
  const passwordHash = await pagecookie.hashPassword(secret);
  return db.withTransaction(async (client) => {
    let portal;
    try {
      const { rows } = await client.query(
        `INSERT INTO page_portals (slug, name, password_hash) VALUES ($1, $2, $3)
         RETURNING ${PORTAL_COLUMNS}`,
        [slug, name, passwordHash]
      );
      portal = rows[0];
    } catch (err) {
      throw uniqueViolation(err, { slug, name }) || err;
    }
    await audit.write(client, {
      ...actorCtx,
      action: "create_portal",
      metadata: { portal_id: portal.id, slug: portal.slug, name: portal.name },
    });
    // The plaintext is returned exactly once, here, and never written to the
    // audit log. Losing it means rotating, which is the correct cost.
    return {
      portal: { ...portal, home_page_slug: null, page_count: 0 },
      password: secret,
      password_generated: generated,
    };
  });
}

async function rename({ id, name }, actorCtx) {
  assertPortalAdmin(actorCtx);
  id = normalizeId(id);
  name = normalizeName(name);
  return db.withTransaction(async (client) => {
    const current = await lockPortal(client, id);
    if (current.name === name) return { portal: current };
    let portal;
    try {
      const { rows } = await client.query(
        `UPDATE page_portals SET name = $2, updated_at = now() WHERE id = $1
         RETURNING ${PORTAL_COLUMNS}`,
        [current.id, name]
      );
      portal = rows[0];
    } catch (err) {
      throw uniqueViolation(err, { slug: current.slug, name }) || err;
    }
    await audit.write(client, {
      ...actorCtx,
      action: "rename_portal",
      metadata: { portal_id: portal.id, slug: portal.slug, from: current.name, to: portal.name },
    });
    return { portal };
  });
}

// setPassword — rotate the portal credential. There is no clear path: the column
// is NOT NULL because an empty credential digest would collide with every
// staff-only page (see migrations/017). Rotating is also the revocation
// primitive — a portal session embeds credentialDigest(password_hash), so a new
// hash (fresh salt even for the same password) invalidates every live session
// for this portal the moment it commits.
async function setPassword({ id, password }, actorCtx) {
  assertPortalAdmin(actorCtx);
  id = normalizeId(id);
  const generated = password === null || password === undefined || password === "";
  const secret = generated ? generatePassword() : assertPasswordAcceptable(password);
  const passwordHash = await pagecookie.hashPassword(secret);
  return db.withTransaction(async (client) => {
    const current = await lockPortal(client, id);
    const { rows } = await client.query(
      `UPDATE page_portals SET password_hash = $2, updated_at = now() WHERE id = $1
       RETURNING ${PORTAL_COLUMNS}`,
      [current.id, passwordHash]
    );
    await audit.write(client, {
      ...actorCtx,
      action: "set_portal_password",
      metadata: { portal_id: current.id, slug: current.slug, generated },
    });
    return { portal: rows[0], password: secret, password_generated: generated };
  });
}

// addPage — the one mutation that changes who can see what, which is why the
// whole module is admin-only. Reports back whether it just reclassified a
// staff-only page as client-readable so the admin UI can say so at the moment of
// adding; the same fact is stamped into the audit metadata.
async function addPage({ id, slug, label, sortOrder }, actorCtx) {
  assertPortalAdmin(actorCtx);
  id = normalizeId(id);
  slug = versions.normalizeSlug(slug);
  label = normalizeLabel(label);
  sortOrder = normalizeSortOrder(sortOrder);
  return db.withTransaction(async (client) => {
    // Portal first, then page — the same lock order as every function here, so
    // concurrent membership changes serialize instead of deadlocking.
    const portal = await lockPortal(client, id);
    const page = await versions.lockPage(client, slug);
    const { rows } = await client.query(
      `SELECT (p.password_hash IS NOT NULL) AS has_password,
              (SELECT count(*)::int FROM page_portal_members m
                 JOIN page_portals pp ON pp.id = m.portal_id AND pp.deleted_at IS NULL
                WHERE m.page_id = p.id) AS portal_count,
              EXISTS (SELECT 1 FROM page_portal_members m2
                       WHERE m2.page_id = p.id AND m2.portal_id = $2) AS already_member
         FROM pages p WHERE p.id = $1`,
      [page.id, portal.id]
    );
    const state = rows[0];
    if (state.already_member) {
      throw conflict(`${page.slug} is already in this portal`, "portal_page_exists");
    }
    // Counted under the page's row lock, so two admins adding the same page to
    // two different portals cannot both pass the cap.
    if (state.portal_count >= MAX_PORTALS_PER_PAGE) {
      throw conflict(
        `${page.slug} is already in ${state.portal_count} portals (limit ${MAX_PORTALS_PER_PAGE})`,
        "portal_fanout_exceeded"
      );
    }
    // Both checks above are made under the page's row lock, so a concurrent add
    // of the same page cannot get past them — but the primary key is the actual
    // guarantee, and a raced insert should read as the same 409 rather than a raw
    // 500 (same reasoning as versions.restorePage's advisory pre-check).
    let inserted;
    try {
      inserted = await client.query(
        `INSERT INTO page_portal_members (portal_id, page_id, label, sort_order)
         VALUES ($1, $2, $3, $4)
         RETURNING page_id, label, sort_order, added_at`,
        [portal.id, page.id, label, sortOrder]
      );
    } catch (err) {
      if (err && err.code === "23505") {
        throw conflict(`${page.slug} is already in this portal`, "portal_page_exists");
      }
      throw err;
    }
    await client.query(`UPDATE page_portals SET updated_at = now() WHERE id = $1`, [portal.id]);
    // A staff-only page (password_hash IS NULL) becomes readable by anyone
    // holding the portal password once the serve predicate lands. That is the
    // intended end state — it is how a partner-facing dashboard gets made — but
    // it is a reclassification of confidential content, so it is recorded as one.
    const reclassifiesStaffOnly = !state.has_password;
    await audit.write(client, {
      ...actorCtx,
      action: "add_portal_page",
      pageId: page.id,
      metadata: {
        portal_id: portal.id,
        portal_slug: portal.slug,
        page_slug: page.slug,
        label,
        sort_order: sortOrder,
        page_had_password: state.has_password,
        reclassifies_staff_only: reclassifiesStaffOnly,
      },
    });
    return {
      member: {
        ...inserted.rows[0],
        slug: page.slug,
        title: page.title,
        display_title: label || page.title || page.slug,
      },
      portal_id: portal.id,
      reclassifies_staff_only: reclassifiesStaffOnly,
    };
  });
}

// updatePage — edit the curated half of a membership (its partner-facing label
// and its order) without a remove/add round trip, which would drop the home
// page and read as two unrelated events in the audit log. Absent fields are
// left alone; an explicit null label falls back to pages.title.
async function updatePage({ id, slug, pageId, label, sortOrder }, actorCtx) {
  assertPortalAdmin(actorCtx);
  id = normalizeId(id);
  const target = normalizeMemberTarget({ slug, pageId });
  const setsLabel = label !== undefined;
  const setsOrder = sortOrder !== undefined;
  if (!setsLabel && !setsOrder) {
    throw badRequest("pass a label or a sort_order to change", "portal_member_nothing_to_update");
  }
  const nextLabel = setsLabel ? normalizeLabel(label) : null;
  const nextOrder = setsOrder ? normalizeSortOrder(sortOrder) : 0;
  return db.withTransaction(async (client) => {
    const portal = await lockPortal(client, id);
    const page = await resolveMemberPage(client, target);
    const current = await client.query(
      `SELECT label, sort_order FROM page_portal_members
        WHERE portal_id = $1 AND page_id = $2 FOR UPDATE`,
      [portal.id, page.id]
    );
    if (!current.rows[0]) {
      throw notFound(`${page.slug} is not in this portal`, "portal_page_not_found");
    }
    const { rows } = await client.query(
      `UPDATE page_portal_members
          SET label = CASE WHEN $3 THEN $4 ELSE label END,
              sort_order = CASE WHEN $5 THEN $6 ELSE sort_order END
        WHERE portal_id = $1 AND page_id = $2
        RETURNING page_id, label, sort_order, added_at`,
      [portal.id, page.id, setsLabel, nextLabel, setsOrder, nextOrder]
    );
    await client.query(`UPDATE page_portals SET updated_at = now() WHERE id = $1`, [portal.id]);
    const member = rows[0];
    await audit.write(client, {
      ...actorCtx,
      action: "update_portal_page",
      pageId: page.id,
      metadata: {
        portal_id: portal.id,
        portal_slug: portal.slug,
        page_slug: page.slug,
        label_from: current.rows[0].label,
        label_to: member.label,
        sort_order_from: current.rows[0].sort_order,
        sort_order_to: member.sort_order,
      },
    });
    return {
      member: {
        ...member,
        slug: page.slug,
        title: page.title,
        display_title: member.label || page.title || page.slug,
      },
      portal_id: portal.id,
    };
  });
}

// removePage — takes the page out of this portal only. Membership is read live
// per request (no portal-authorized render mints a page cookie), so this is
// effective on the member's very next request rather than in 30 days.
async function removePage({ id, slug, pageId }, actorCtx) {
  assertPortalAdmin(actorCtx);
  id = normalizeId(id);
  const target = normalizeMemberTarget({ slug, pageId });
  return db.withTransaction(async (client) => {
    const portal = await lockPortal(client, id);
    const page = await resolveMemberPage(client, target);
    const { rowCount } = await client.query(
      `DELETE FROM page_portal_members WHERE portal_id = $1 AND page_id = $2`,
      [portal.id, page.id]
    );
    if (!rowCount) throw notFound(`${page.slug} is not in this portal`, "portal_page_not_found");
    // A home page that is no longer a member would send every arriving partner
    // to a page this portal no longer authorizes, so it goes with it.
    const homeCleared = sameId(portal.home_page_id, page.id);
    await client.query(
      `UPDATE page_portals
          SET home_page_id = CASE WHEN home_page_id = $2 THEN NULL ELSE home_page_id END,
              updated_at = now()
        WHERE id = $1`,
      [portal.id, page.id]
    );
    await audit.write(client, {
      ...actorCtx,
      action: "remove_portal_page",
      pageId: page.id,
      metadata: {
        portal_id: portal.id,
        portal_slug: portal.slug,
        page_slug: page.slug,
        home_cleared: homeCleared,
      },
    });
    return { portal_id: portal.id, page_slug: page.slug, removed: true, home_cleared: homeCleared };
  });
}

// setHome — which of the portal's dashboards a partner lands on. Pass a null
// slug to clear it. Must be a current member: a home page outside the portal is
// a link to a password wall.
async function setHome({ id, slug }, actorCtx) {
  assertPortalAdmin(actorCtx);
  id = normalizeId(id);
  const clearing = slug === null || slug === undefined || slug === "";
  const wanted = clearing ? null : versions.normalizeSlug(slug);
  return db.withTransaction(async (client) => {
    const portal = await lockPortal(client, id);
    let page = null;
    if (!clearing) {
      page = await versions.lockPage(client, wanted);
      const member = await client.query(
        `SELECT 1 FROM page_portal_members WHERE portal_id = $1 AND page_id = $2`,
        [portal.id, page.id]
      );
      if (!member.rowCount) {
        throw badRequest(`${page.slug} is not in this portal — add it first`, "portal_page_not_found");
      }
    }
    if (sameId(portal.home_page_id, page && page.id) || (clearing && portal.home_page_id === null)) {
      return { portal: { ...portal, home_page_slug: page ? page.slug : null } };
    }
    const { rows } = await client.query(
      `UPDATE page_portals SET home_page_id = $2, updated_at = now() WHERE id = $1
       RETURNING ${PORTAL_COLUMNS}`,
      [portal.id, page ? page.id : null]
    );
    const fromSlug = await slugOfPage(client, portal.home_page_id);
    await audit.write(client, {
      ...actorCtx,
      action: "set_portal_home",
      pageId: page ? page.id : null,
      metadata: {
        portal_id: portal.id,
        portal_slug: portal.slug,
        from: fromSlug,
        to: page ? page.slug : null,
      },
    });
    return { portal: { ...rows[0], home_page_slug: page ? page.slug : null } };
  });
}

// remove — retire a portal. Soft delete, like a page: the row, its membership
// and its audit trail stay, and the slug and name free for reuse. It is also the
// kill switch for the credential, since every read filters `deleted_at IS NULL`
// — so retiring a portal ends every live session for it. Member pages are
// untouched: their own passwords, versions and pointers are not this table's
// business.
async function remove({ id }, actorCtx) {
  assertPortalAdmin(actorCtx);
  id = normalizeId(id);
  return db.withTransaction(async (client) => {
    const portal = await lockPortal(client, id);
    const { rows } = await client.query(
      `SELECT count(*)::int AS member_count FROM page_portal_members WHERE portal_id = $1`,
      [portal.id]
    );
    const memberCount = rows[0].member_count;
    await client.query(
      `UPDATE page_portals SET deleted_at = now(), updated_at = now() WHERE id = $1`,
      [portal.id]
    );
    await audit.write(client, {
      ...actorCtx,
      action: "delete_portal",
      metadata: {
        portal_id: portal.id,
        slug: portal.slug,
        name: portal.name,
        member_count: memberCount,
      },
    });
    return { id: portal.id, slug: portal.slug, name: portal.name, deleted: true, member_count: memberCount };
  });
}

// ── internal helpers (all operate on the txn client) ─────────────────────────

// lockPortal — SELECT … FOR UPDATE on the portal, the first statement of every
// mutation above, so the row lock is held for the whole transaction.
async function lockPortal(client, id) {
  const { rows } = await client.query(
    `SELECT ${PORTAL_COLUMNS} FROM page_portals
      WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
    [id]
  );
  if (!rows[0]) throw notFound("portal not found", "portal_not_found");
  return rows[0];
}

// The slug path goes through versions.lockPage, so a live page 404s with the
// same code and locks the same way as everywhere else. The id path deliberately
// does NOT filter deleted_at — it is the only way to reach a membership row
// whose page has been soft-deleted.
async function resolveMemberPage(client, target) {
  if (target.slug) return versions.lockPage(client, target.slug);
  const { rows } = await client.query(
    `SELECT id, slug, title FROM pages WHERE id = $1 FOR UPDATE`,
    [target.pageId]
  );
  if (!rows[0]) throw notFound(`page not found: ${target.pageId}`, "page_not_found");
  return rows[0];
}

async function slugOfPage(client, pageId) {
  if (pageId === null || pageId === undefined) return null;
  const { rows } = await client.query(`SELECT slug FROM pages WHERE id = $1`, [pageId]);
  return rows[0] ? rows[0].slug : null;
}

// Both live-uniqueness indexes land on 23505; the constraint name says which.
function uniqueViolation(err, { slug, name }) {
  if (!err || err.code !== "23505") return null;
  if (err.constraint === "page_portals_name_live_uidx") {
    return conflict(`a portal is already named: ${name}`, "portal_name_exists");
  }
  if (err.constraint === "page_portals_slug_live_uidx") {
    return conflict(`portal already exists: ${slug}`, "portal_exists");
  }
  return null;
}

module.exports = {
  MUTATIONS,
  MAX_PORTALS_PER_PAGE,
  // Exported so test/unit.test.js can hold the browser module's own copy of this
  // ceiling to it. The admin UI has to know it to append a member without
  // proposing a value the write would reject, and the only guard on the two
  // agreeing used to be a native max="9999" on an input that no longer exists.
  MAX_SORT_ORDER,
  MIN_PASSWORD_LENGTH,
  assertPortalAdmin,
  normalizeId,
  normalizeSlug,
  normalizeName,
  normalizeLabel,
  normalizeSortOrder,
  generatePassword,
  assertPasswordAcceptable,
  list,
  get,
  listForPage,
  extractLinkedSlugs,
  create,
  rename,
  setPassword,
  addPage,
  updatePage,
  removePage,
  setHome,
  remove,
};
