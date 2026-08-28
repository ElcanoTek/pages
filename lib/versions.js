// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
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
const pagecookie = require("./pagecookie");
const pageData = require("./page-data");
const render = require("./render"); // owns #pages-nav: injects it, and strips a stored copy
const { ApiError, badRequest, forbidden, notFound, conflict } = require("./apierror");

const RENDER_MODES = new Set(["themed", "raw"]);
const VERSION_SOURCES = new Set(["api", "mcp", "admin"]);
const VERSION_STATUSES = new Set(["draft", "pending", "approved", "rejected"]);
const MAX_LIST_LIMIT = 101; // lets a public 100-row page fetch one look-ahead row

function sha256(text) {
  return crypto.createHash("sha256").update(text, "utf8").digest("hex");
}

// ── freshness ──────────────────────────────────────────────────────────────

// A dashboard frozen for six weeks looks exactly like one refreshed this
// morning. Pages has known otherwise the whole time — page_versions carries
// source_as_of and refreshed_at — it just never said so as a fact a caller
// could read. The one staleness signal that did exist (staleRefreshWarnings)
// fires only DURING a write, so it answers "did this refresh add anything?"
// and is structurally blind to the failure that matters: a refresh that never
// ran, or that correctly declined and therefore never called Pages at all.
//
// Deliberately NOT an "is this overdue?" verdict. Pages retired its scheduler
// on purpose and does not know how often any given page should refresh;
// inferring overdue is the consumer's job. These are the facts that let a
// consumer decide it in one query instead of by noticing.
const DAY_MS = 86400000;

function daysSince(value, now) {
  if (!value) return null;
  const then = value instanceof Date ? value.getTime() : Date.parse(value);
  if (!Number.isFinite(then)) return null;
  // Clamped at zero: a source_as_of a few seconds into the future (clock skew
  // within the write path's tolerance) is not "-1 days old".
  return Math.max(0, Math.floor((now - then) / DAY_MS));
}

function iso(value) {
  if (!value) return null;
  const date = value instanceof Date ? value : new Date(value);
  return Number.isNaN(date.getTime()) ? null : date.toISOString();
}

// freshnessOf builds the computed block from whatever the caller has: the
// published version's coverage/refresh stamps, plus the last recorded
// deliberate check. Returns null only when the page has neither — a page that
// has never been managed and never been checked has nothing to be stale about.
function freshnessOf(row, now = Date.now()) {
  const sourceAsOf = iso(row && row.source_as_of);
  const refreshedAt = iso(row && row.refreshed_at);
  const lastCheckAt = iso(row && row.last_check_at);
  if (!sourceAsOf && !refreshedAt && !lastCheckAt) return null;
  // checked_at is the last time anyone LOOKED, whether or not it produced a
  // version. That is the number that separates "we check daily and the
  // upstream is dead" from "nobody has run this in three weeks" — two states
  // that need different humans to act, and that were indistinguishable.
  const checkedAt =
    lastCheckAt && refreshedAt ? (Date.parse(lastCheckAt) > Date.parse(refreshedAt) ? lastCheckAt : refreshedAt) : lastCheckAt || refreshedAt;
  return {
    source_as_of: sourceAsOf,
    refreshed_at: refreshedAt,
    checked_at: checkedAt,
    last_check_outcome: (row && row.last_check_outcome) || null,
    last_check_detail: (row && row.last_check_detail) || null,
    last_check_source_as_of: iso(row && row.last_check_source_as_of),
    days_since_source: daysSince(sourceAsOf, now),
    days_since_refresh: daysSince(refreshedAt, now),
    days_since_check: daysSince(checkedAt, now),
  };
}

// ── reads ──────────────────────────────────────────────────────────────────

// listPages — newest first, with each page's currently-published version id.
// has_password / is_live are derived so an enumerating agent can tell a page's
// client-access and serving state at a glance (is_live = published AND not
// disabled → actually serving on the content host).
//
// Existing REST/admin callers intentionally receive the complete array. MCP
// callers can pass bounded keyset/filter options either as listPages(options)
// or listPages(executor, options); the latter keeps snapshot transactions
// available to the admin index.
async function listPages(executor = db, options = undefined) {
  if (!executor || typeof executor.query !== "function") {
    options = executor || {};
    executor = db;
  }
  options = options || {};

  const params = [];
  const where = ["p.deleted_at IS NULL"];
  const bind = (value) => {
    params.push(value);
    return `$${params.length}`;
  };

  const workspaceFilterSet =
    options.workspaceFilterSet === true || Object.prototype.hasOwnProperty.call(options, "workspaceId");
  if (workspaceFilterSet) {
    if (options.workspaceId === null || options.workspaceId === "ungrouped") {
      where.push("p.workspace_id IS NULL");
    } else {
      where.push(`p.workspace_id = ${bind(normalizeWorkspaceId(options.workspaceId))}`);
    }
  }

  const query = normalizeSearchQuery(options.query);
  if (query !== null) {
    const pattern = bind(`%${escapeLike(query)}%`);
    where.push(
      `(p.slug ILIKE ${pattern} ESCAPE '\\' OR p.title ILIKE ${pattern} ESCAPE '\\' ` +
      `OR COALESCE(p.client_id, '') ILIKE ${pattern} ESCAPE '\\' ` +
      `OR COALESCE(w.name, '') ILIKE ${pattern} ESCAPE '\\')`
    );
  }

  if (Object.prototype.hasOwnProperty.call(options, "clientId")) {
    if (options.clientId === null) {
      where.push("p.client_id IS NULL");
    } else {
      if (typeof options.clientId !== "string") {
        throw badRequest("client_id must be a string or null", "bad_client_id");
      }
      where.push(`p.client_id = ${bind(options.clientId)}`);
    }
  }
  if (Object.prototype.hasOwnProperty.call(options, "isLive")) {
    assertBooleanFilter(options.isLive, "is_live");
    where.push(`(p.published_version_id IS NOT NULL AND NOT p.disabled) = ${bind(options.isLive)}`);
  }
  if (Object.prototype.hasOwnProperty.call(options, "requireApproval")) {
    assertBooleanFilter(options.requireApproval, "require_approval");
    where.push(`p.require_approval = ${bind(options.requireApproval)}`);
  }
  if (Object.prototype.hasOwnProperty.call(options, "disabled")) {
    assertBooleanFilter(options.disabled, "disabled");
    where.push(`p.disabled = ${bind(options.disabled)}`);
  }

  const after = normalizeAfter(options.after);
  if (after) {
    const createdAt = bind(after.createdAt);
    const id = bind(after.id);
    where.push(
      `(date_trunc('milliseconds', p.created_at), p.id) < ` +
      `(date_trunc('milliseconds', ${createdAt}::timestamptz), ${id}::bigint)`
    );
  }
  const limit = normalizeListLimit(options.limit);
  const limitSql = limit === null ? "" : ` LIMIT ${bind(limit)}`;
  // The admin index presents page-local version ordinals, never the global
  // page_versions primary key. Keep these extra aggregates opt-in so the
  // existing REST/MCP page-summary contracts remain unchanged.
  const versionNumberFields = options.includeVersionNumbers === true
    ? `,
            (SELECT COUNT(*)::integer
               FROM page_versions versions
              WHERE versions.page_id = p.id) AS version_count,
            CASE WHEN p.published_version_id IS NULL THEN NULL ELSE
              (SELECT COUNT(*)::integer
                 FROM page_versions versions
                WHERE versions.page_id = p.id
                  AND versions.id <= p.published_version_id)
            END AS published_version_number`
    : "";

  const { rows } = await executor.query(
    `SELECT p.id, p.slug, p.title, p.client_id, p.workspace_id, w.name AS workspace_name,
            p.theme_id, COALESCE(t.name, 'flag') AS theme_name, p.require_approval, p.disabled,
            p.published_version_id, p.created_at, p.updated_at,
            (p.password_hash IS NOT NULL) AS has_password,
            (p.published_version_id IS NOT NULL AND NOT p.disabled) AS is_live,
            pv.source_as_of, pv.refreshed_at,
            p.last_check_at, p.last_check_outcome, p.last_check_detail,
            p.last_check_source_as_of${versionNumberFields}
       FROM pages p
       LEFT JOIN workspaces w ON w.id = p.workspace_id
       LEFT JOIN themes t ON t.id = p.theme_id
       LEFT JOIN page_versions pv ON pv.id = p.published_version_id
      WHERE ${where.join(" AND ")}
      ORDER BY date_trunc('milliseconds', p.created_at) DESC, p.id DESC${limitSql}`,
    params
  );
  // One list call now ranks the whole estate by staleness. The raw stamps stay
  // off the row: they are an implementation detail of the join, and every
  // caller wants the derived answer.
  const now = Date.now();
  return rows.map(
    ({ source_as_of, refreshed_at, last_check_at, last_check_outcome, last_check_detail, last_check_source_as_of, ...page }) => ({
      ...page,
      freshness: freshnessOf(
        { source_as_of, refreshed_at, last_check_at, last_check_outcome, last_check_detail, last_check_source_as_of },
        now
      ),
    })
  );
}

// getPage — page metadata + the published version (or null). 404 if absent.
async function getPage(slug) {
  slug = normalizeSlug(slug);
  const { rows } = await db.query(
    `SELECT p.id, p.slug, p.title, p.client_id, p.workspace_id, w.name AS workspace_name,
            p.theme_id, COALESCE(t.name, 'flag') AS theme_name, p.require_approval, p.disabled, p.published_version_id,
            p.created_at, p.updated_at,
            (p.password_hash IS NOT NULL) AS has_password,
            p.last_check_at, p.last_check_outcome, p.last_check_detail, p.last_check_source_as_of
       FROM pages p
       LEFT JOIN workspaces w ON w.id = p.workspace_id
       LEFT JOIN themes t ON t.id = p.theme_id
      WHERE p.slug = $1 AND p.deleted_at IS NULL`,
    [slug]
  );
  const raw = rows[0];
  if (!raw) throw notFound(`page not found: ${slug}`, "page_not_found");
  const { last_check_at, last_check_outcome, last_check_detail, last_check_source_as_of, ...page } = raw;
  const lastCheck = { last_check_at, last_check_outcome, last_check_detail, last_check_source_as_of };
  let published = null;
  if (page.published_version_id) {
    try {
      published = await getVersion(slug, page.published_version_id);
    } catch (err) {
      // Only a genuinely missing version row may read as "nothing published"
      // (the pages_pubver_fk FK makes that impossible in practice; keep it as
      // defense). Anything else — a transient reset, statement/lock timeout,
      // pool exhaustion — must surface as an error (503 via fromDbError at the
      // API layer), never as a FALSE unpublished state that agents act on.
      if (!(err instanceof ApiError) || err.code !== "version_not_found") throw err;
    }
  }
  return { page, published, lastCheck };
}

// getPageData — read the data-management contract from the immutable version
// at the published pointer. The returned serving flags deliberately distinguish
// pointer truth from actual live serving when an admin has disabled the page.
async function getPageData(slug) {
  const { page, published, lastCheck } = await getPage(slug);
  if (!published) {
    throw conflict("page has no published template to manage", "page_not_data_managed");
  }
  const managed = pageData.parseManagedHtml(published.html);
  const { html: _html, ...version } = published;
  return {
    page,
    version,
    schema: managed.schema,
    envelope: managed.envelope,
    data_sha256: managed.data_sha256,
    schema_sha256: managed.schema_sha256,
    template_sha256: managed.template_sha256,
    data_profile: pageData.profileData(managed.envelope.data),
    freshness: freshnessOf({
      ...lastCheck,
      source_as_of: managed.envelope.source_as_of,
      refreshed_at: managed.envelope.refreshed_at,
    }),
  };
}

// recordRefreshCheck — stamp that someone looked, and what they saw, without
// creating a version.
//
// The healthy outcome of a daily refresh is often "the source has no new day,
// so I published nothing", and that outcome currently writes nothing anywhere.
// A page in that state is byte-identical to one whose job was deleted three
// weeks ago. This is the smallest thing that separates them: it moves
// checked_at without touching the published pointer, the data, or any hash, so
// a client keeps serving exactly what it was serving.
//
// Explicitly not a version: a no-op refresh has produced no new content, and
// minting an immutable row to say so would put a fresh entry in every page's
// history every day and defeat the dedupe that already exists.
const REFRESH_CHECK_OUTCOMES = new Set(["updated", "source_not_updated", "source_unreachable", "blocked", "failed"]);
const MAX_CHECK_DETAIL = 500;

async function recordRefreshCheck({ slug, outcome, detail = null, sourceAsOfSeen = null, now = Date.now() }, actorCtx) {
  slug = normalizeSlug(slug);
  if (!REFRESH_CHECK_OUTCOMES.has(outcome)) {
    throw badRequest(
      `outcome must be one of: ${[...REFRESH_CHECK_OUTCOMES].join(", ")}`,
      "refresh_check_outcome_invalid"
    );
  }
  if (detail !== null && detail !== undefined) {
    if (typeof detail !== "string") throw badRequest("detail must be a string", "refresh_check_detail_invalid");
    detail = detail.trim() || null;
    if (detail && detail.length > MAX_CHECK_DETAIL) {
      throw badRequest(`detail must be at most ${MAX_CHECK_DETAIL} characters`, "refresh_check_detail_invalid");
    }
  } else {
    detail = null;
  }
  let seen = null;
  if (sourceAsOfSeen !== null && sourceAsOfSeen !== undefined) {
    const parsed = Date.parse(sourceAsOfSeen);
    if (!Number.isFinite(parsed)) {
      throw badRequest("source_as_of_seen must be an RFC3339 timestamp", "refresh_check_source_invalid");
    }
    seen = new Date(parsed).toISOString();
  }
  const checkedAt = new Date(Number(now)).toISOString();

  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    await client.query(
      `UPDATE pages
          SET last_check_at = $1, last_check_outcome = $2,
              last_check_detail = $3, last_check_source_as_of = $4
        WHERE id = $5`,
      [checkedAt, outcome, detail, seen, page.id]
    );
    await audit.write(client, {
      ...actorCtx,
      action: "record_refresh_check",
      pageId: page.id,
      metadata: { outcome, detail, source_as_of_seen: seen },
    });
    // Read the freshness back rather than echoing the request: the caller's
    // next decision is "is this page now overdue by my rules", and that needs
    // the page's coverage, which this call did not change.
    const { rows } = await client.query(
      `SELECT pv.source_as_of, pv.refreshed_at
         FROM pages p LEFT JOIN page_versions pv ON pv.id = p.published_version_id
        WHERE p.id = $1`,
      [page.id]
    );
    const coverage = rows[0] || {};
    return {
      slug: page.slug,
      freshness: freshnessOf(
        {
          source_as_of: coverage.source_as_of,
          refreshed_at: coverage.refreshed_at,
          last_check_at: checkedAt,
          last_check_outcome: outcome,
          last_check_detail: detail,
          last_check_source_as_of: seen,
        },
        Number(now)
      ),
    };
  });
}

// listVersions — full history for a page, newest first. Optional bounded
// keyset/status options are for MCP; omission retains the legacy full array.
async function listVersions(slug, options = {}) {
  const page = await loadPage(slug);
  options = options || {};
  const params = [page.id];
  const where = ["v.page_id = $1"];
  if (options.status !== undefined) {
    if (!VERSION_STATUSES.has(options.status)) {
      throw badRequest(`bad version status: ${options.status}`, "bad_version_status");
    }
    params.push(options.status);
    where.push(`v.status = $${params.length}`);
  }
  const after = normalizeAfter(options.after);
  if (after) {
    params.push(after.createdAt, after.id);
    where.push(
      `(date_trunc('milliseconds', v.created_at), v.id) < ` +
      `(date_trunc('milliseconds', $${params.length - 1}::timestamptz), $${params.length}::bigint)`
    );
  }
  const limit = normalizeListLimit(options.limit);
  let limitSql = "";
  if (limit !== null) {
    params.push(limit);
    limitSql = ` LIMIT $${params.length}`;
  }
  const { rows } = await db.query(
    `SELECT v.id, v.page_id, v.content_sha256, v.status, v.render_mode, v.author, v.source,
            v.note, v.reviewed_by, v.reviewed_at, v.created_at
       FROM page_versions v
      WHERE ${where.join(" AND ")}
      ORDER BY date_trunc('milliseconds', v.created_at) DESC, v.id DESC${limitSql}`,
    params
  );
  // Published means pointer equality even during an admin takedown. Live is the
  // stricter serving truth: pointer equality AND the page is not disabled.
  return rows.map((r) => {
    const isPublished = sameDbId(r.id, page.published_version_id);
    return { ...r, is_published: isPublished, is_live: isPublished && !page.disabled };
  });
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

// createPage — { slug, title?, clientId?, workspaceId?, requireApproval? }.
// Workspace selection is reversible organization metadata, so both human
// admins and authenticated agents may choose it. theme/password are set via
// their own endpoints later. Slug must be unique.
async function createPage({ slug, title = "", clientId = null, workspaceId = null, requireApproval = false }, actorCtx) {
  slug = normalizeSlug(slug);
  assertSlugNotReserved(slug);
  if (workspaceId !== null && workspaceId !== undefined) {
    assertWorkspaceOrganizer(actorCtx);
    workspaceId = normalizeWorkspaceId(workspaceId);
  } else {
    workspaceId = null;
  }
  return db.withTransaction(async (client) => {
    let workspaceName = null;
    if (workspaceId !== null) {
      const workspace = await client.query(
        `SELECT id, name FROM workspaces WHERE id = $1 FOR KEY SHARE`,
        [workspaceId]
      );
      if (!workspace.rows[0]) throw notFound("workspace not found", "workspace_not_found");
      workspaceName = workspace.rows[0].name;
    }
    // Only LIVE pages hold a slug (soft-deleted rows keep theirs but the partial
    // unique index frees it for reuse — migrations/002), so scope the dup check.
    // The pre-check is a fast path; the partial unique index is the real guard,
    // so a concurrent create racing under READ COMMITTED surfaces as a 23505 that
    // we map to the same 409 (not a raw 500).
    const dup = await client.query("SELECT 1 FROM pages WHERE slug = $1 AND deleted_at IS NULL", [slug]);
    if (dup.rowCount) throw conflict(`slug already exists: ${slug}`, "slug_exists");
    let rows;
    try {
      ({ rows } = await client.query(
        `INSERT INTO pages (slug, title, client_id, workspace_id, require_approval)
         VALUES ($1, $2, $3, $4, $5)
         RETURNING id, slug, title, client_id, workspace_id, theme_id,
                   require_approval, disabled, published_version_id, created_at, updated_at`,
        [slug, title, clientId, workspaceId, !!requireApproval]
      ));
    } catch (err) {
      if (err && err.code === "23505") throw conflict(`slug already exists: ${slug}`, "slug_exists");
      throw err;
    }
    const page = rows[0];
    await audit.write(client, {
      ...actorCtx,
      action: "create_page",
      pageId: page.id,
      metadata: workspaceId === null ? null : { workspace_id: workspaceId },
    });
    return { ...page, workspace_name: workspaceName };
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
// The write and read paths disagreed about what a managed page is, and the write
// path was the lenient one. deploy_page, patch_page and rollback_page published
// whatever bytes they were handed; only get_page_data / get_page_config /
// update_page_data ever parsed the managed blocks. So a patch that broke the JSON
// inside #pages-data, or a deploy carrying refreshed_at:null, produced a version
// that published happily and then dead-ended the entire managed-data toolchain on
// that page with data_contract_invalid — and the live page threw at JSON.parse and
// rendered blank.
//
// A page in that state is broken no matter who looks at it, so refuse the write
// instead of discovering it on the next read. The error taxonomy already draws
// the right line: page_not_data_managed / page_not_template_managed mean "this is
// an ordinary raw page", which is a perfectly good thing to deploy and must keep
// working. Anything else means the author reached for the managed contract and
// got it wrong.
function assertManagedBlocksDeployable(html) {
  try {
    pageData.parseManagedHtml(html);
  } catch (error) {
    const code = error && error.code;
    if (code === "page_not_data_managed" || code === "page_not_template_managed") return;
    if (!code) throw error;
    throw conflict(
      `refusing to publish: this document's Pages-managed blocks are not valid, so the page would render ` +
        `blank and every managed-data tool would fail on it. ${error.message}`,
      code,
      error.details
    );
  }
}

function prepareDeploy(
  {
    slug,
    html,
    renderMode = "themed",
    note = null,
    author,
    source = "api",
    publish = false,
    expectedVersion,
    dataMetadata = null,
    templateBinding = null,
  },
  actorCtx
) {
  if (typeof html !== "string" || html.length === 0) throw badRequest("html is required", "html_required");
  if (!RENDER_MODES.has(renderMode)) throw badRequest(`bad render_mode: ${renderMode}`, "bad_render_mode");
  if (!VERSION_SOURCES.has(source)) throw badRequest(`bad source: ${source}`, "bad_source");
  note = normalizeNote(note);

  // A page that separates its per-instance values into #pages-config may ship
  // without a hand-written #pages-config-schema; one is derived from those values
  // and written into the document here, BEFORE the content hash, so the stored
  // bytes and their identity always agree. Every deploy path routes through this
  // function, so a page is normalized once and every reader downstream sees a
  // complete pair. A no-op for the pages that carry no config at all.
  const normalized = pageData.ensureConfigSchema(html);
  assertManagedBlocksDeployable(normalized.html);
  // `#pages-nav` is Pages' id, injected at render time with the CURRENT sibling
  // list of whichever portal authorised the request. A document shipping its own
  // copy would be a second answer to "which dashboards can this viewer open",
  // written by whoever authored the page — so it is removed here, before the
  // content hash, exactly like the config-schema normalisation above.
  const deployHtml = render.stripInjectedNav(normalized.html);

  return {
    slug: normalizeSlug(slug),
    html: deployHtml,
    configSchemaGenerated: normalized.generated,
    renderMode,
    note,
    versionAuthor: author || actorCtx.actor,
    source,
    publish: !!publish,
    expectedVersion,
    contentSha: sha256(deployHtml),
    dataMetadata,
    templateBinding,
  };
}

async function deploy(args, actorCtx) {
  const prepared = prepareDeploy(args, actorCtx);
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, prepared.slug);
    return withConfigSchemaNotice(prepared, await deployLocked(client, page, prepared, actorCtx));
  });
}

// Only the paths that accept RAW html can derive a config schema; everything else
// materializes from stored bytes that already carry one. Attaching the notice here
// rather than inside deployLocked keeps it out of the six strict tool schemas that
// spread a deploy result and could never see it set.
function withConfigSchemaNotice(prepared, result) {
  return prepared.configSchemaGenerated ? { ...result, config_schema_generated: true } : result;
}

// createAndDeploy — MCP's create-or-update fast path as one transaction. A
// failed version insert/publish cannot strand an empty page, and concurrent
// callers converge through the partial live-slug unique index.
async function createAndDeploy(
  { title = "", clientId = null, requireApproval = false, ...deployArgs },
  actorCtx
) {
  return db.withTransaction((client) =>
    createAndDeployWithClient(
      client,
      { title, clientId, requireApproval, ...deployArgs },
      actorCtx
    )
  );
}

// Transaction-aware variant used by Pages-owned staged uploads. Keeping the
// verified upload consumption, immutable version, pointer move, and audit row
// in one PostgreSQL transaction makes an ambiguous commit safely retryable.
async function createAndDeployWithClient(
  client,
  { title = "", clientId = null, requireApproval = false, ...deployArgs },
  actorCtx
) {
  const prepared = prepareDeploy(deployArgs, actorCtx);
  let page = (
    await client.query(
      `SELECT id, slug, require_approval, disabled, published_version_id
         FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`,
      [prepared.slug]
    )
  ).rows[0];
  let created = false;

  if (!page) {
    assertSlugNotReserved(prepared.slug);
    const inserted = await client.query(
      `INSERT INTO pages (slug, title, client_id, require_approval)
       VALUES ($1, $2, $3, $4)
       ON CONFLICT (slug) WHERE deleted_at IS NULL DO NOTHING
       RETURNING id, slug, require_approval, disabled, published_version_id`,
      [prepared.slug, title, clientId, !!requireApproval]
    );
    page = inserted.rows[0];
    if (page) {
      if (prepared.expectedVersion !== undefined && prepared.expectedVersion !== null) {
        throw conflict("expected_version cannot be used when creating a missing page", "stale_version");
      }
      created = true;
      await audit.write(client, { ...actorCtx, action: "create_page", pageId: page.id });
    } else {
      // A concurrent create committed while INSERT waited on the unique
      // index. Take the normal page lock and continue as an update.
      page = (
        await client.query(
          `SELECT id, slug, require_approval, disabled, published_version_id
             FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`,
          [prepared.slug]
        )
      ).rows[0];
      if (!page) throw conflict(`slug changed concurrently: ${prepared.slug}`, "slug_race");
    }
  }

  return withConfigSchemaNotice(prepared, { created, ...(await deployLocked(client, page, prepared, actorCtx)) });
}

async function deployLocked(client, page, prepared, actorCtx) {
  const {
    html,
    renderMode,
    note,
    versionAuthor,
    source,
    publish,
    expectedVersion,
    contentSha,
    dataMetadata,
    templateBinding,
  } = prepared;

  // Optimistic concurrency, before anything is read or written. It used to be
  // enforced only inside movePointer, which meant the two deploy paths that do
  // NOT move the pointer silently dropped the caller's check: publish:false, and
  // every write to an approval-gated page. Both docs/API.md and docs/SECURITY.md
  // state it unconditionally, and the gated page is the case that matters most —
  // it is where a human is trusting the review queue. Agent A reads live v1 and
  // computes a change; a human approves pending v2; agent A's write lands as
  // pending v3 built from v1's bytes with no 409, and approving v3 reverts v2.
  //
  // Every caller has already locked the page row FOR UPDATE, so this reads a
  // stable pointer. For the publish path it is the same assertion movePointer
  // will make again on the same locked row — redundant, not different.
  //
  // It now also precedes the takedown and rejected-terminal checks, so a stale
  // expected_version against a DISABLED page answers 409 stale_version where it
  // used to answer 403 disabled_takedown. That costs one round trip — the agent
  // rereads, and get_page reports `disabled` — and it is the right way round:
  // the concurrency check applies to every deploy, the takedown check only to
  // the ones that would move the pointer.
  assertExpectedVersion(page, expectedVersion);

    // Dedupe: identical content for this page returns the existing version —
    // scoped to the SAME render_mode (mode is immutable per row, so a mode
    // change on identical bytes must be a NEW row, not a silent drop of the
    // caller's request) and never against a `rejected` row: rejected is
    // terminal for the ROW, not the content (PLAN §5 — "clone into a new
    // draft"), so re-deploying once-rejected bytes inserts a fresh
    // draft/pending row that re-enters the normal flow. On an approval-gated
    // page, open-era `draft` rows are also skipped — there a draft can be
    // neither approved (not_pending) nor agent-published, so deduping onto it
    // would be a dead end; the deploy lands `pending` instead. Symmetrically,
    // open pages skip gated-era `pending` rows so publish:false returns a usable
    // draft rather than directing the caller to publish_page on a non-draft.
    const existing = dataMetadata
      ? await client.query(
          `SELECT id, page_id, content_sha256, status, render_mode, author, source,
                  note, reviewed_by, reviewed_at, created_at
             FROM page_versions
            WHERE page_id = $1
              AND data_sha256 = $2
              AND data_template_sha256 = $3
              AND source_as_of = $4::timestamptz
              AND render_mode = $5
              AND status <> 'rejected'
              ${page.require_approval ? "AND status <> 'draft'" : "AND status <> 'pending'"}
            ORDER BY id DESC LIMIT 1`,
          [page.id, dataMetadata.dataSha, dataMetadata.templateSha, dataMetadata.sourceAsOf, renderMode]
        )
      : await client.query(
          `SELECT id, page_id, content_sha256, status, render_mode, author, source,
                  note, reviewed_by, reviewed_at, created_at
             FROM page_versions
            WHERE page_id = $1 AND content_sha256 = $2
              AND render_mode = $3
              AND status <> 'rejected'
              ${page.require_approval ? "AND status <> 'draft'" : "AND status <> 'pending'"}
            ORDER BY id DESC LIMIT 1`,
          [page.id, contentSha, renderMode]
        );
    let version = existing.rows[0];
    let deduped = false;

    if (version) {
      deduped = true;
    } else {
      const gated = page.require_approval;
      const status = gated ? "pending" : "draft";
      const ins = await client.query(
        `INSERT INTO page_versions
           (page_id, html, content_sha256, status, render_mode, author, source, note,
            data_sha256, data_template_sha256, source_as_of, refreshed_at,
            template_version_id, config_sha256)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
         RETURNING id, page_id, content_sha256, status, render_mode, author, source,
                   note, reviewed_by, reviewed_at, created_at`,
        [
          page.id,
          html,
          contentSha,
          status,
          renderMode,
          versionAuthor,
          source,
          note,
          dataMetadata ? dataMetadata.dataSha : null,
          dataMetadata ? dataMetadata.templateSha : null,
          dataMetadata ? dataMetadata.sourceAsOf : null,
          dataMetadata ? dataMetadata.refreshedAt : null,
          templateBinding ? templateBinding.templateVersionId : null,
          templateBinding ? templateBinding.configSha : null,
        ]
      );
      version = ins.rows[0];
      await audit.write(client, {
        ...actorCtx,
        // A managed write names what it actually did (data refresh, config edit,
        // template build) rather than being flattened into one label.
        action: dataMetadata ? dataMetadata.action || "data_update" : "deploy",
        pageId: page.id,
        versionId: version.id,
        metadata: {
          status,
          render_mode: renderMode,
          deduped: false,
          ...(dataMetadata
            ? {
                data_sha256: dataMetadata.dataSha,
                schema_sha256: dataMetadata.schemaSha,
                template_sha256: dataMetadata.templateSha,
                source_as_of: dataMetadata.sourceAsOf,
                refreshed_at: dataMetadata.refreshedAt,
              }
            : {}),
          ...(templateBinding
            ? {
                template: templateBinding.templateName,
                template_version_id: String(templateBinding.templateVersionId),
                template_revision: templateBinding.revision,
                config_sha256: templateBinding.configSha,
              }
            : {}),
        },
      });
    }

    // Fast-path publish on open pages only. On approval-gated pages publish is
    // silently ignored (the version waits in the pending queue for a human).
    let publishedNow = false;
    if (publish && !page.require_approval) {
      assertAgentMayMovePointer(page, actorCtx); // open page → ok for agents
      // The pointer may only ever reference an `approved` version, so persist
      // that status before moving it. `rejected` is terminal — the dedupe
      // above never matches a rejected row, so this guard is defense in depth.
      if (version.status === "rejected") {
        throw conflict("cannot publish a rejected version; deploy new content", "rejected_terminal");
      }
      if (version.status !== "approved") {
        await client.query(`UPDATE page_versions SET status = 'approved' WHERE id = $1`, [version.id]);
        version.status = "approved";
      }
      // An exact data retry against the already-live immutable version is a
      // semantic no-op: do not emit another pointer/audit write.
      //
      // Note what this does NOT decide: the dedupe above can land on an OLDER
      // version (it keys on content/data identity, not on the pointer), and
      // publishing that moves the live pointer BACKWARD. For a caller that
      // supplied the bytes, that is the content it asked for. For a caller whose
      // contract says "retrying is a no-op" it is a silent revert, so
      // templates.createPage scopes its own idempotency check to the published
      // version before it ever gets here — see the sameBuild query there.
      if (!(dataMetadata && sameDbId(page.published_version_id, version.id))) {
        await movePointer(client, page, version.id, expectedVersion, actorCtx, "publish");
        publishedNow = true;
      }
    }

    // `live` is the truthful serving state: published this call, OR the dedupe
    // matched the version the pointer already references (page row was locked,
    // so the comparison can't race).
    const live = !page.disabled && (publishedNow || sameDbId(page.published_version_id, version.id));
    return {
      version,
      deduped,
      published: publishedNow,
      gated: page.require_approval,
      live,
    };
}

// updatePageData — serialize writers on the page, use the currently published
// version as the immutable layout/schema template, validate structured data,
// then enter the ordinary version/publish state machine. expectedVersion is
// mandatory even for publish:false so canaries cannot be built from stale
// layout bytes.
async function updatePageData(args, actorCtx) {
  return db.withTransaction((client) => updatePageDataWithClient(client, args, actorCtx));
}

// Transaction-aware variant, for the same reason createAndDeployWithClient
// exists: a staged data upload has to consume its verified bytes, write the
// immutable version, move the pointer, and drop the chunks in ONE transaction,
// or an ambiguous commit leaves a spent upload with no version behind it.
//
// Both entry points run this exact body — there is no second validation path,
// no second dedupe rule, and no second reconciliation. The staged route differs
// from the inline one only in where the payload came from.
async function updatePageDataWithClient(
  client,
  { slug, data, sourceAsOf, expectedVersion, publish = true, note = null, expect = null, now = Date.now() },
  actorCtx
) {
  slug = normalizeSlug(slug);
  if (expectedVersion === undefined || expectedVersion === null) {
    throw badRequest("expected_version is required for data updates", "expected_version_required");
  }

  {
    const page = await lockPage(client, slug);
    assertExpectedVersion(page, expectedVersion);
    if (!page.published_version_id) {
      throw conflict("page has no published template to manage", "page_not_data_managed");
    }
    const templateResult = await client.query(
      `SELECT id, html, render_mode, template_version_id, config_sha256
         FROM page_versions
        WHERE id = $1 AND page_id = $2`,
      [page.published_version_id, page.id]
    );
    const template = templateResult.rows[0];
    if (!template) {
      throw conflict("published template is unavailable", "page_not_data_managed");
    }

    const managedTemplate = pageData.parseManagedHtml(template.html);

    // Profile the incoming payload and reconcile it against whatever the caller
    // said it would contain, BEFORE anything is written: a refresh that fails
    // its own arithmetic must not reach a client, and a refused write leaves the
    // live page exactly where it was. The previous payload is already parsed
    // here for materialization, so the regression comparison is free.
    const previousProfile = pageData.profileData(managedTemplate.envelope.data);
    const dataProfile = pageData.profileData(data);
    pageData.assertExpectedProfile(dataProfile, expect);
    const dataWarnings = pageData.compareDataProfiles(previousProfile, dataProfile, {
      expected: expect,
      previousDataSha: managedTemplate.data_sha256,
      nextDataSha: pageData.semanticHash(data),
    });

    const materialized = pageData.materialize(managedTemplate, data, sourceAsOf, now);

    // Source coverage is monotonic across every non-rejected data version, not
    // just the live pointer. This also serializes publish:false canaries safely.
    const sourceFloorResult = await client.query(
      `SELECT max(source_as_of) AS source_as_of
         FROM page_versions
        WHERE page_id = $1 AND source_as_of IS NOT NULL AND status <> 'rejected'`,
      [page.id]
    );
    const storedFloor = sourceFloorResult.rows[0] && sourceFloorResult.rows[0].source_as_of;
    const currentSource = new Date(managedTemplate.envelope.source_as_of).toISOString();
    const storedSource = storedFloor ? new Date(storedFloor).toISOString() : null;
    const sourceFloor = storedSource && Date.parse(storedSource) > Date.parse(currentSource) ? storedSource : currentSource;
    if (Date.parse(materialized.envelope.source_as_of) < Date.parse(sourceFloor)) {
      throw conflict(
        `source_as_of cannot move backward (current coverage is ${sourceFloor})`,
        "source_regression",
        { current_source_as_of: sourceFloor }
      );
    }

    const prepared = prepareDeploy(
      {
        slug,
        html: materialized.html,
        renderMode: template.render_mode,
        note,
        source: "mcp",
        publish,
        expectedVersion,
        dataMetadata: {
          dataSha: materialized.data_sha256,
          schemaSha: materialized.schema_sha256,
          templateSha: materialized.template_sha256,
          sourceAsOf: materialized.envelope.source_as_of,
          refreshedAt: materialized.envelope.refreshed_at,
        },
        // Provenance survives a refresh: a template-built page stays attributable
        // to the revision that produced its design.
        templateBinding: carryTemplateBinding(template),
      },
      actorCtx
    );
    const result = await deployLocked(client, page, prepared, actorCtx);
    const versionHtml = await client.query(
      `SELECT html FROM page_versions WHERE id = $1 AND page_id = $2`,
      [result.version.id, page.id]
    );
    const managedVersion = pageData.parseManagedHtml(versionHtml.rows[0].html);
    return {
      ...result,
      schema: managedVersion.schema,
      envelope: managedVersion.envelope,
      data_sha256: managedVersion.data_sha256,
      schema_sha256: managedVersion.schema_sha256,
      template_sha256: managedVersion.template_sha256,
      data_profile: dataProfile,
      // A dedupe already says "the exact data and source coverage were already
      // live" in its own field and its next_step, and data_unchanged's message
      // ("a version was still created") would be flatly untrue there. Drop the
      // stale-refresh pair in that case; every other warning still applies.
      data_warnings: result.deduped
        ? dataWarnings.filter((w) => w.code !== "data_unchanged" && w.code !== "coverage_did_not_advance")
        : dataWarnings,
    };
  }
}

// carryTemplateBinding — reuse the published version's provenance for the next
// version. A refresh or a config edit does not change which template revision
// produced the design, so the binding must not be dropped on the way through.
function carryTemplateBinding(version) {
  if (!version || !version.template_version_id) return null;
  return {
    templateVersionId: version.template_version_id,
    configSha: version.config_sha256,
  };
}

// parseConfigManaged — read a page's config contract. A page with no managed
// blocks at all and a page with only the data pair are the same answer from a
// config caller's point of view: it was not built from a template. Reporting
// page_not_data_managed here would send them to fix the wrong thing.
function parseConfigManaged(html) {
  let managed;
  try {
    managed = pageData.parseManagedHtml(html);
  } catch (error) {
    if (error && error.code === "page_not_data_managed") {
      throw conflict(
        `page has no #${pageData.CONFIG_ID} block; it was not built from a template`,
        "page_not_template_managed"
      );
    }
    throw error;
  }
  if (!managed.configBlock) {
    throw conflict(
      `page has no #${pageData.CONFIG_ID} block; it was not built from a template`,
      "page_not_template_managed"
    );
  }
  return managed;
}

// getPageConfig — the deploy-time settings half of a template-built page. Kept
// separate from getPageData so the data path is untouched and so an agent that
// only needs to change the campaign's identity never reads its rows.
async function getPageConfig(slug) {
  const { page, published } = await getPage(slug);
  if (!published) {
    throw conflict("page has no published version", "update_page_not_published");
  }
  const managed = parseConfigManaged(published.html);
  const { html: _html, ...version } = published;
  return {
    page,
    version,
    config_schema: managed.configSchema,
    config: managed.config,
    config_sha256: managed.config_sha256,
    config_schema_sha256: managed.config_schema_sha256,
    template_sha256: managed.template_sha256,
  };
}

// updatePageConfig — replace ONLY the config block of the published version.
// Structurally the same as updatePageData (lock the page, use the published
// version as the immutable template, validate, enter the normal state machine),
// with one deliberate difference: the data block is left byte-for-byte alone, so
// editing a campaign's identity can never disturb, restate, or roll back its
// numbers. source coverage is inherited unchanged for the same reason — a config
// edit represents no new source data.
async function updatePageConfig(
  { slug, config, expectedVersion, publish = true, note = null },
  actorCtx
) {
  slug = normalizeSlug(slug);
  if (expectedVersion === undefined || expectedVersion === null) {
    throw badRequest("expected_version is required for config updates", "expected_version_required");
  }

  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    assertExpectedVersion(page, expectedVersion);
    if (!page.published_version_id) {
      throw conflict("page has no published version to update", "update_page_not_published");
    }
    const publishedResult = await client.query(
      `SELECT id, html, render_mode, template_version_id, config_sha256
         FROM page_versions
        WHERE id = $1 AND page_id = $2`,
      [page.published_version_id, page.id]
    );
    const current = publishedResult.rows[0];
    if (!current) throw conflict("published version is unavailable", "update_page_not_published");

    const managed = parseConfigManaged(current.html);
    const materialized = pageData.materializeBlocks(managed, { config });

    const binding = carryTemplateBinding(current);
    const prepared = prepareDeploy(
      {
        slug,
        html: materialized.html,
        renderMode: current.render_mode,
        note,
        source: "mcp",
        publish,
        expectedVersion,
        // The identity moves because config is part of the template a later
        // refresh pours rows into (see page-data templateIdentity), so this
        // cannot silently dedupe onto the pre-edit version. Source coverage is
        // carried across verbatim.
        dataMetadata: {
          action: "config_update",
          dataSha: materialized.data_sha256,
          schemaSha: materialized.schema_sha256,
          templateSha: materialized.template_sha256,
          sourceAsOf: materialized.envelope.source_as_of,
          refreshedAt: materialized.envelope.refreshed_at,
        },
        templateBinding: binding
          ? { ...binding, configSha: materialized.config_sha256 }
          : null,
      },
      actorCtx
    );
    const result = await deployLocked(client, page, prepared, actorCtx);
    const versionHtml = await client.query(
      `SELECT html FROM page_versions WHERE id = $1 AND page_id = $2`,
      [result.version.id, page.id]
    );
    const managedVersion = pageData.parseManagedHtml(versionHtml.rows[0].html);
    return {
      ...result,
      config_schema: managedVersion.configSchema,
      config: managedVersion.config,
      config_sha256: managedVersion.config_sha256,
      config_schema_sha256: managedVersion.config_schema_sha256,
      envelope: managedVersion.envelope,
      data_sha256: managedVersion.data_sha256,
      template_sha256: managedVersion.template_sha256,
    };
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
async function rollback({ slug, versionId = null, expectedVersion, note = null }, actorCtx) {
  note = normalizeNote(note);
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
    await movePointer(client, page, targetId, expectedVersion, actorCtx, "rollback", note);
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
  note = normalizeNote(note);
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
    await audit.write(client, {
      ...actorCtx,
      action: disabled ? "disable" : "enable",
      pageId: page.id,
      metadata: null,
    });
    return { slug: page.slug, disabled: !!disabled };
  });
}

// setApproval — toggle the per-page approval gate.
async function setApproval({ slug, requireApproval }, actorCtx) {
  assertAdminAction(actorCtx);
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    await client.query(`UPDATE pages SET require_approval = $1, updated_at = now() WHERE id = $2`, [!!requireApproval, page.id]);
    await audit.write(client, {
      ...actorCtx,
      action: "set_approval",
      pageId: page.id,
      metadata: { require_approval: !!requireApproval },
    });
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

// setPassword — set or clear the per-page client password (PLAN §6b). The hash
// is scrypt (lib/pagecookie).
//
// Authority (asymmetric by risk): SETTING/changing a password is agent-capable
// (bearer) — it's the "make this dashboard client-accessible" authoring step.
// CLEARING it (empty/null) is admin-only: on a live client page a clear flips it
// back to Elcano-only and the content host then 403s every real client, i.e. a
// silent takedown — so a stray/injected agent must not be able to do it.
async function setPassword({ slug, password }, actorCtx) {
  const clearing = password === null || password === undefined || password === "";
  if (clearing) assertAdminAction(actorCtx, "clearing a page password is admin-only");
  if (!clearing && (typeof password !== "string" || password.length < 1)) {
    throw badRequest("password must be a non-empty string", "bad_password");
  }
  if (!clearing && password.length > 512) {
    throw badRequest("password must be 512 characters or fewer", "password_too_long");
  }
  // Hash BEFORE opening the transaction: scrypt is deliberately expensive and
  // must not run while holding the page's row lock (it would serialize every
  // other mutation on the page behind the KDF).
  const hash = clearing ? null : await pagecookie.hashPassword(password);
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    await client.query(`UPDATE pages SET password_hash = $1, updated_at = now() WHERE id = $2`, [hash, page.id]);
    await audit.write(client, { ...actorCtx, action: hash ? "set_password" : "clear_password", pageId: page.id });
    return { slug: page.slug, has_password: !!hash };
  });
}

// setTitle — rename a page. Agent-capable authoring metadata (like set_password
// setting): title is content-adjacent, not trust/safety governance. Does not
// touch the pointer or versions.
async function setTitle({ slug, title }, actorCtx) {
  if (typeof title !== "string" || !title.trim()) throw badRequest("title is required", "title_required");
  const clean = title.trim().slice(0, 200);
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    if (page.title === clean) return { slug: page.slug, title: clean };
    await client.query(`UPDATE pages SET title = $1, updated_at = now() WHERE id = $2`, [clean, page.id]);
    await audit.write(client, { ...actorCtx, action: "set_title", pageId: page.id, metadata: { title: clean } });
    return { slug: page.slug, title: clean };
  });
}

// deletePage — SOFT delete (PLAN §5). Sets deleted_at so every read/serve query
// (which all filter `deleted_at IS NULL`) stops returning the page, and NULLs the
// live pointer so nothing serves and any future hard-purge won't trip the
// published_version_id RESTRICT FK. The row + its versions + audit trail stay,
// and the partial unique index (migrations/002) frees the slug for reuse.
// Reversible via restorePage. Authority mirrors publish/rollback: agents may
// delete OPEN pages; approval-gated pages are admin-only.
async function deletePage({ slug }, actorCtx) {
  return db.withTransaction(async (client) => {
    const page = await lockPage(client, slug);
    assertAgentMayMovePointer(page, actorCtx);
    await client.query(
      `UPDATE pages SET published_version_id = NULL, deleted_at = now(), updated_at = now() WHERE id = $1`,
      [page.id]
    );
    await audit.write(client, {
      ...actorCtx,
      action: "delete_page",
      pageId: page.id,
      metadata: { had_published: page.published_version_id || null },
    });
    return { slug: page.slug, deleted: true };
  });
}

// restorePage — admin-only undelete: clear deleted_at on the most-recently
// deleted row for this slug, provided no LIVE page has taken the slug since.
async function restorePage({ slug }, actorCtx) {
  assertAdminAction(actorCtx);
  slug = normalizeSlug(slug);
  // Reservation is a creation-time rule, so a row predating a newly reserved
  // segment can exist — soft-deleted, serving nothing, harmless. Restoring one
  // is not a read: it would put a live page at an address a route now owns, i.e.
  // a page nobody can open. Refuse instead, and keep the row recoverable by
  // redeploying its content at an unreserved slug.
  assertSlugNotReserved(slug);
  return db.withTransaction(async (client) => {
    const live = await client.query("SELECT 1 FROM pages WHERE slug = $1 AND deleted_at IS NULL", [slug]);
    if (live.rowCount) throw conflict(`a live page already uses this slug: ${slug}`, "slug_taken");
    const { rows } = await client.query(
      `SELECT id FROM pages WHERE slug = $1 AND deleted_at IS NOT NULL
        ORDER BY deleted_at DESC LIMIT 1 FOR UPDATE`,
      [slug]
    );
    if (!rows[0]) throw notFound(`no deleted page to restore: ${slug}`, "page_not_found");
    // The pre-check above is advisory; if a create/restore raced in and took the
    // slug between the check and here, the partial unique index rejects the
    // un-delete — map that to the same 409 instead of a raw 500.
    try {
      await client.query(`UPDATE pages SET deleted_at = NULL, updated_at = now() WHERE id = $1`, [rows[0].id]);
    } catch (err) {
      if (err && err.code === "23505") throw conflict(`a live page already uses this slug: ${slug}`, "slug_taken");
      throw err;
    }
    await audit.write(client, { ...actorCtx, action: "restore_page", pageId: rows[0].id });
    return { slug, restored: true };
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
    `SELECT id, slug, title, require_approval, disabled, published_version_id
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
// note is optional and lands in the AUDIT metadata, never on the version: a
// rollback republishes bytes that already exist and page_versions is append-only
// and immutable, so the only honest place to record why the pointer moved is the
// log entry for the move. Every other write tool takes a note; a rollback — the
// one write whose reason is never inferable from a diff — could not say anything.
async function movePointer(client, page, versionId, expectedVersion, actorCtx, action, note = null) {
  assertExpectedVersion(page, expectedVersion);
  await client.query(
    `UPDATE pages SET published_version_id = $1, updated_at = now() WHERE id = $2`,
    [versionId, page.id]
  );
  await audit.write(client, {
    ...actorCtx,
    action,
    pageId: page.id,
    versionId,
    metadata: {
      from: page.published_version_id || null,
      to: versionId,
      ...(note === null ? {} : { note }),
    },
  });
}

// A bare "published version changed (expected 143, is 145)" is a dead end: it
// tells the caller its write lost a race but not what to do next, and an agent
// that reads it usually gives up or — worse — re-runs the whole job. It is also
// the guaranteed outcome of a host that stages writes for human approval, since
// expected_version is frozen when the card is staged and a human clicks Approve
// minutes later, after some other queued write has already landed. So name the
// version that overtook it and the one call that recovers. The "confirm first"
// clause is load-bearing: retrying blind with the new id is exactly how one
// agent silently clobbers another's deploy, which is what this check exists to
// prevent.
function assertExpectedVersion(page, expectedVersion) {
  if (
    expectedVersion !== undefined &&
    expectedVersion !== null &&
    !sameDbId(expectedVersion, page.published_version_id)
  ) {
    const current = page.published_version_id === null ? null : String(page.published_version_id);
    throw conflict(
      `published version changed (expected ${expectedVersion}, is ${page.published_version_id}) — ` +
        `another write landed after you read version ${expectedVersion}. Reread the page, confirm ` +
        `version ${page.published_version_id} is not already the change you intended, and only then ` +
        `retry once with expected_version ${page.published_version_id}.`,
      "stale_version",
      {
        expected_version: String(expectedVersion),
        published_version_id: current,
        retry_with_expected_version: current,
      }
    );
  }
}

// The shared gate for every agent action that publishes, rolls back, or deletes
// a page (deploy's publish path, publish, rollback, deletePage). Humans (admin)
// may always. Agents are refused on two admin-controlled states:
//   • disabled — an admin takedown kill switch. Agents cannot publish/rollback/
//     delete a taken-down page; blocking DELETE here is what stops the
//     delete→recreate slug-reuse bypass of the takedown.
//   • require_approval — a human must approve every live change.
function assertAgentMayMovePointer(page, actorCtx) {
  if (actorCtx.actorType === "user") return;
  if (page.disabled) {
    throw forbidden(
      "this page has been disabled (taken down) by an admin — agents cannot publish, roll back, or delete it",
      "disabled_takedown"
    );
  }
  if (page.require_approval) {
    throw forbidden(
      "this page requires approval — agents may only deploy a pending version; a human must approve",
      "approval_required"
    );
  }
}

function assertAdminAction(actorCtx, message) {
  if (actorCtx.actorType !== "user") {
    throw forbidden(message || "this action is admin-only", "admin_only");
  }
}

// Workspace membership changes only organization metadata and are reversible;
// authenticated agents share this bounded authority with human admins. This is
// deliberately separate from assertAdminAction, which protects governance and
// destructive operations.
function assertWorkspaceOrganizer(actorCtx) {
  if (!actorCtx || (actorCtx.actorType !== "user" && actorCtx.actorType !== "agent")) {
    throw forbidden("workspace organization requires an authenticated user or agent", "organizer_only");
  }
}

function normalizeNote(note) {
  if (note === null || note === undefined) return null;
  if (typeof note !== "string") throw badRequest("note must be a string or null", "bad_note");
  if (note.length > 500) {
    throw badRequest("note must be 500 characters or fewer", "note_too_long");
  }
  return note;
}

function normalizeListLimit(value) {
  if (value === null || value === undefined) return null;
  if (!Number.isInteger(value) || value < 1 || value > MAX_LIST_LIMIT) {
    throw badRequest(`limit must be an integer from 1 to ${MAX_LIST_LIMIT}`, "bad_limit");
  }
  return value;
}

function normalizeAfter(value) {
  if (value === null || value === undefined) return null;
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw badRequest("after must contain createdAt and id", "bad_cursor");
  }
  const date = value.createdAt instanceof Date ? value.createdAt : new Date(value.createdAt);
  if (!Number.isFinite(date.getTime())) {
    throw badRequest("after.createdAt must be a valid timestamp", "bad_cursor");
  }
  let id;
  if (typeof value.id === "bigint") {
    id = value.id;
  } else if (typeof value.id === "number" && Number.isSafeInteger(value.id)) {
    id = BigInt(value.id);
  } else if (typeof value.id === "string" && /^[1-9][0-9]*$/.test(value.id)) {
    id = BigInt(value.id);
  } else {
    throw badRequest("after.id must be a positive database id", "bad_cursor");
  }
  if (id <= 0n || id > 9223372036854775807n) {
    throw badRequest("after.id must be a positive database id", "bad_cursor");
  }
  return { createdAt: date.toISOString(), id: id.toString() };
}

function normalizeSearchQuery(value) {
  if (value === null || value === undefined) return null;
  if (typeof value !== "string") throw badRequest("query must be a string", "bad_query");
  const query = value.trim();
  if (!query) return null;
  if (query.length > 200) throw badRequest("query must be 200 characters or fewer", "query_too_long");
  return query;
}

function escapeLike(value) {
  return value.replace(/[\\%_]/g, "\\$&");
}

function assertBooleanFilter(value, field) {
  if (typeof value !== "boolean") throw badRequest(`${field} must be boolean`, `bad_${field}`);
}

function sameDbId(a, b) {
  return a !== null && a !== undefined && b !== null && b !== undefined && String(a) === String(b);
}

function normalizeSlug(slug) {
  if (typeof slug !== "string") throw badRequest("slug is required", "slug_required");
  const s = slug.trim().toLowerCase();
  // flat ('northwind') or nested ('northwind/q2-report'); url-safe segments only.
  if (!/^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:\/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$/.test(s)) {
    throw badRequest("slug must be url-safe (a-z 0-9 - _ /)", "bad_slug");
  }
  return s;
}

function normalizeWorkspaceId(value) {
  if (typeof value === "number" && Number.isSafeInteger(value) && value > 0) return value;
  if (typeof value === "string" && /^[1-9][0-9]*$/.test(value)) {
    const id = BigInt(value);
    if (id <= 9223372036854775807n) return id <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(id) : value;
  }
  throw badRequest("workspace_id must be a positive database id", "bad_workspace_id");
}

// Slug segments that collide with real routes. A page created with one of
// these would be unreachable (or would shadow an API action): the content host
// serves /raw, /assets and /healthz ahead of the direct-serve wildcard;
// /admin/welcome is the dashboard index alias; and the REST/admin routers hang
// sub-resource actions (versions, publish, …) off /pages/<slug>/…, where a
// nested slug containing one of them is ambiguous against the action routes.
// Enforced at page CREATION (and restore), so reads of any legacy row keep working.
const RESERVED_SLUG_SEGMENTS = new Set([
  "raw", "assets", "healthz", "welcome",
  "versions", "publish", "rollback", "approve", "reject", "approval",
  "password", "title", "theme", "disable", "enable", "delete", "restore",
  "preview-token", "deploy-source", "workspace",
  // /admin/templates is the template library shell, registered BEFORE
  // /admin/*slug — a page holding this slug would be unreachable in admin.
  "templates",
  // <content-host>/portal/<portal-slug> is the partner entry point, matched
  // inside the direct-serve wildcard BEFORE the page lookup. Without this an
  // agent could create a page at `portal/nwm` and seize a partner's bookmarked
  // URL — and an agent can create pages. Reserving the segment closes that at
  // every creation path instead of relying on branch ordering alone;
  // migrations/018 refuses to ship the route while a live page still holds one.
  "portal",
  // /admin/portals is the partner-portal screen, registered BEFORE /admin/*slug —
  // same shape as `templates`. A page holding this slug would still serve to
  // clients but would be unreachable in admin.
  "portals",
]);

function assertSlugNotReserved(slug) {
  for (const seg of slug.split("/")) {
    if (RESERVED_SLUG_SEGMENTS.has(seg)) {
      throw badRequest(`slug segment '${seg}' is reserved (collides with a route)`, "reserved_slug");
    }
  }
}

module.exports = {
  freshnessOf,
  updatePageDataWithClient,
  recordRefreshCheck,
  REFRESH_CHECK_OUTCOMES,
  sha256,
  normalizeSlug,
  normalizeWorkspaceId,
  assertSlugNotReserved,
  RESERVED_SLUG_SEGMENTS,
  listPages,
  getPage,
  getPageData,
  getPageConfig,
  updatePageConfig,
  carryTemplateBinding,
  listVersions,
  getVersion,
  createPage,
  createAndDeploy,
  createAndDeployWithClient,
  deploy,
  // Exported for lib/templates.js, which composes the same transaction-scoped
  // primitives rather than reimplementing the state machine.
  lockPage,
  assertExpectedVersion,
  prepareDeploy,
  deployLocked,
  updatePageData,
  publish,
  rollback,
  approve,
  reject,
  setDisabled,
  setApproval,
  setTheme,
  setPassword,
  setTitle,
  deletePage,
  restorePage,
  listThemes,
};
