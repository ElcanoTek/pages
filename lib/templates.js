// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/templates.js — one stored design, many pages.
//
// A template is a complete, self-validating managed page: it carries both
// managed pairs (see lib/page-data.js), its #pages-config holds a real reference
// config, and its #pages-data holds the empty-state envelope. That is what lets
// registration validate a template against its own schemas instead of trusting
// the uploader, and it means a template can be rendered and inspected on its own
// before any client page exists.
//
// The point is cost. Building a second dashboard from a known design used to
// mean transmitting the whole design again — for a 62 KB campaign dashboard,
// ~21k output tokens of base64, or a hand-edited copy of the file. With a
// template stored server-side, the same page costs the config that actually
// differs (kilobytes) and get_template answers "what does this design need?"
// with two schemas instead of 62 KB of HTML.
//
// This module deliberately owns no rendering policy beyond block substitution:
// there is no placeholder or template language anywhere. Untrusted config and
// data can only ever land inside a JSON script block, escaped by page-data, so
// they cannot become markup.

const crypto = require("node:crypto");
const db = require("./db");
const versions = require("./versions");
const audit = require("./audit");
const pageData = require("./page-data");
const preflight = require("./preflight");
const { badRequest, conflict, notFound } = require("./apierror");

// Mirrors the CHECK in migrations/015. No slashes: a template is a design, not a
// place in a hierarchy, and keeping it flat means a name can never be mistaken
// for a page slug in an upload row.
const NAME_RE = /^[a-z0-9]+(?:[-_][a-z0-9]+)*$/;
const MAX_NAME_CHARS = 64;
// "cli" is a file-backed registration by an operator with shell access
// (scripts/template.js) — the path that needs no agent and no model output.
const TEMPLATE_SOURCES = new Set(["api", "mcp", "admin", "cli"]);
// A page created without data represents no source coverage yet. The epoch is
// the lowest possible floor, so the first real ingest can never be rejected as a
// regression however old the data it covers.
const NO_SOURCE_COVERAGE = "1970-01-01T00:00:00Z";

function sha256(text) {
  return crypto.createHash("sha256").update(text, "utf8").digest("hex");
}

function normalizeTemplateName(name) {
  if (typeof name !== "string") throw badRequest("template is required", "template_required");
  const normalized = name.trim().toLowerCase();
  if (!NAME_RE.test(normalized) || normalized.length > MAX_NAME_CHARS) {
    throw badRequest(
      `template must be url-safe with no slashes (a-z 0-9 - _), at most ${MAX_NAME_CHARS} characters`,
      "bad_template_name"
    );
  }
  return normalized;
}

// parseTemplateHtml — the contract check. Everything it enforces is already
// hardened for pages: 2020-12 self-containment, the ReDoS pattern screen, size
// ceilings, and "the payload currently in the document must satisfy its own
// schema" — applied here to both pairs.
function parseTemplateHtml(html) {
  return pageData.parseManaged(html, pageData.TEMPLATE_SPEC);
}

function templateRow(row) {
  return {
    id: String(row.id),
    name: row.name,
    title: row.title,
    description: row.description,
    current_revision: row.current_revision === null ? null : Number(row.current_revision),
    current_version_id: row.current_version_id === null ? null : String(row.current_version_id),
    created_at: iso(row.created_at),
    updated_at: iso(row.updated_at),
  };
}

function revisionRow(row) {
  return {
    version_id: String(row.id),
    revision: Number(row.revision),
    content_sha256: row.content_sha256,
    config_schema_sha256: row.config_schema_sha256,
    data_schema_sha256: row.data_schema_sha256,
    author: row.author,
    source: row.source,
    note: row.note,
    // Whether, not what: an example dataset is kilobytes of rows and the callers
    // that matter (agents reading get_template) are the ones paying for context.
    // Either row shape works — the payload (register's RETURNING, loadCurrent) or
    // just the boolean, which is what the revisions list selects so it never
    // reads rows it would immediately throw away.
    has_sample_data: row.has_sample_data !== undefined
      ? Boolean(row.has_sample_data)
      : row.sample_data !== null && row.sample_data !== undefined,
    created_at: iso(row.created_at),
  };
}

function iso(value) {
  return value instanceof Date ? value.toISOString() : value;
}

// ── registration ────────────────────────────────────────────────────────────

// registerWithClient — transaction-aware so a staged upload can register inside
// the same transaction that verifies and consumes its bytes (see
// pageUploads.deploy): an ambiguous commit is then safely retryable.
async function registerWithClient(
  client,
  { name, html, title = null, description = null, note = null, source = "mcp" },
  actorCtx
) {
  name = normalizeTemplateName(name);
  if (typeof html !== "string" || html.trim().length === 0) {
    throw badRequest("html is required", "html_required");
  }
  if (!TEMPLATE_SOURCES.has(source)) throw badRequest(`bad source: ${source}`, "bad_source");

  const parsed = parseTemplateHtml(html);
  const contentSha = sha256(html);

  let template = (
    await client.query(
      `SELECT id, name, title, description, current_version_id
         FROM page_templates WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`,
      [name]
    )
  ).rows[0];
  let created = false;

  if (!template) {
    const inserted = await client.query(
      `INSERT INTO page_templates (name, title, description)
       VALUES ($1, $2, $3)
       ON CONFLICT (name) WHERE deleted_at IS NULL DO NOTHING
       RETURNING id, name, title, description, current_version_id`,
      [name, title || "", description || ""]
    );
    template = inserted.rows[0];
    if (template) {
      created = true;
    } else {
      // A concurrent register committed while the INSERT waited on the unique
      // index. Take the row lock and continue as a new revision.
      template = (
        await client.query(
          `SELECT id, name, title, description, current_version_id
             FROM page_templates WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`,
          [name]
        )
      ).rows[0];
      if (!template) throw conflict(`template name changed concurrently: ${name}`, "template_race");
    }
  }

  // An identical re-register is the same revision, not a new one. Scoped to the
  // newest revision so a genuine revert to older bytes still records a revision
  // — "current" must always be the most recently registered design.
  const newest = (
    await client.query(
      `SELECT id, revision, content_sha256, config_schema_sha256, data_schema_sha256,
              sample_data, author, source, note, created_at
         FROM page_template_versions
        WHERE template_id = $1
        ORDER BY revision DESC LIMIT 1`,
      [template.id]
    )
  ).rows[0];

  let version = newest && newest.content_sha256 === contentSha ? newest : null;
  const deduped = !!version;

  if (!version) {
    const inserted = await client.query(
      `INSERT INTO page_template_versions
         (template_id, revision, html, content_sha256, config_schema, data_schema,
          config_schema_sha256, data_schema_sha256, reference_config, sample_data,
          author, source, note)
       VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9::jsonb, $10::jsonb, $11, $12, $13)
       RETURNING id, revision, content_sha256, config_schema_sha256, data_schema_sha256,
                 sample_data, author, source, note, created_at`,
      [
        template.id,
        newest ? Number(newest.revision) + 1 : 1,
        html,
        contentSha,
        JSON.stringify(parsed.configSchema),
        JSON.stringify(parsed.schema),
        parsed.config_schema_sha256,
        parsed.schema_sha256,
        JSON.stringify(parsed.config),
        // Preview-only. page-data.js deletes the block it came from on every
        // materialization, so this can never reach a page.
        parsed.example === null ? null : JSON.stringify(parsed.example),
        actorCtx.actor,
        source,
        normalizeNote(note),
      ]
    );
    version = inserted.rows[0];
  }

  // The pointer always names the newest revision. Pages pin the exact revision
  // they were built from, so moving it never touches anything already deployed.
  const updated = (
    await client.query(
      `UPDATE page_templates
          SET current_version_id = $2,
              title = COALESCE($3, title),
              description = COALESCE($4, description),
              updated_at = now()
        WHERE id = $1
      RETURNING id, name, title, description, current_version_id, created_at, updated_at`,
      [template.id, version.id, title, description]
    )
  ).rows[0];

  if (!deduped) {
    await audit.write(client, {
      ...actorCtx,
      action: created ? "create_template" : "register_template",
      metadata: {
        template: name,
        template_version_id: String(version.id),
        revision: Number(version.revision),
        content_sha256: contentSha,
        config_schema_sha256: parsed.config_schema_sha256,
        data_schema_sha256: parsed.schema_sha256,
      },
    });
  }

  return {
    created,
    deduped,
    template: templateRow({ ...updated, current_revision: version.revision }),
    revision: revisionRow(version),
    config_schema: parsed.configSchema,
    data_schema: parsed.schema,
    reference_config: parsed.config,
    has_sample_data: parsed.example !== null,
    preflight: preflight.analyze(html, { renderMode: "themed" }),
  };
}

async function register(args, actorCtx) {
  return db.withTransaction((client) => registerWithClient(client, args, actorCtx));
}

function normalizeNote(note) {
  if (note === null || note === undefined) return null;
  const trimmed = String(note).trim();
  if (!trimmed) return null;
  return trimmed.slice(0, 500);
}

// validateHtml — "is this our template format?", WITHOUT writing anything.
//
// The library's upload path calls this first so a human sees exactly what is
// wrong before anything is registered. It answers two different questions and
// keeps them apart, because they have different consequences:
//
//   contract — does this satisfy the four-block template contract? A failure here
//              means it cannot be registered at all.
//   preflight — does the design actually work in the sandbox+CSP it will be
//              served under? Advisory for a page, but a template's findings are
//              INHERITED by every page built from it, so the library shows them
//              as blocking-by-default and makes the operator override.
function validateHtml(html, { name = null } = {}) {
  const report = {
    name: null,
    name_error: null,
    contract_ok: false,
    contract_error: null,
    config_schema: null,
    data_schema: null,
    reference_config: null,
    data_keys: null,
    ships_empty: null,
    has_sample_data: null,
    sample_data_keys: null,
    hardcoded_config_values: null,
    bytes: null,
    preflight: null,
  };

  if (typeof html !== "string" || html.trim().length === 0) {
    report.contract_error = { code: "html_required", message: "no HTML was supplied" };
    return report;
  }
  report.bytes = Buffer.byteLength(html, "utf8");

  if (name !== null && name !== undefined && String(name).trim() !== "") {
    try {
      report.name = normalizeTemplateName(name);
    } catch (error) {
      report.name_error = { code: error.code || "bad_template_name", message: error.message };
    }
  }

  let parsed;
  try {
    parsed = parseTemplateHtml(html);
  } catch (error) {
    report.contract_error = {
      code: error.code || "template_contract_invalid",
      message: error.message,
      ...(error.details === undefined ? {} : { details: error.details }),
    };
    // Still preflight it: an operator fixing the blocks wants to know about the
    // broken chart control in the same pass, not on the next upload.
    report.preflight = preflight.analyze(html, { renderMode: "themed" });
    return report;
  }

  report.contract_ok = true;
  report.config_schema = parsed.configSchema;
  report.data_schema = parsed.schema;
  report.reference_config = parsed.config;
  report.data_keys = Object.keys(parsed.envelope.data);
  // A template that ships real rows would publish one campaign's numbers into
  // every page built from it until the first refresh.
  const rows = parsed.envelope.data.rows;
  report.ships_empty = Array.isArray(rows) ? rows.length === 0 : null;
  // An optional example dataset is what lets the library show this design
  // populated. Report that it was found and validated against the data schema;
  // the rows themselves are not echoed back.
  report.has_sample_data = parsed.example !== null;
  report.sample_data_keys = parsed.example === null ? null : Object.keys(parsed.example);
  // Values that are in the config AND still written into the design. Every page
  // built from this would show the hardcoded copy.
  report.hardcoded_config_values = hardcodedConfigValues(html, parsed);
  report.preflight = preflight.analyze(html, { renderMode: "themed" });
  return report;
}

// ── reads ───────────────────────────────────────────────────────────────────

// loadCurrent — the row a create/rerender builds from. `revision` pins an exact
// one; omitted means the current pointer.
async function loadCurrent(executor, name, revision = null) {
  name = normalizeTemplateName(name);
  const { rows } = await executor.query(
    `SELECT t.id AS template_id, t.name, t.title, t.description,
            t.current_version_id, t.created_at, t.updated_at,
            v.id, v.revision, v.html, v.content_sha256, v.config_schema, v.data_schema,
            v.config_schema_sha256, v.data_schema_sha256, v.reference_config,
            v.sample_data, v.author, v.source, v.note, v.created_at AS revision_created_at
       FROM page_templates t
       JOIN page_template_versions v ON v.template_id = t.id
      WHERE t.name = $1 AND t.deleted_at IS NULL
        AND (
          ($2::int IS NULL AND v.id = t.current_version_id)
          OR ($2::int IS NOT NULL AND v.revision = $2::int)
        )`,
    [name, revision === null || revision === undefined ? null : Number(revision)]
  );
  const row = rows[0];
  if (!row) {
    throw notFound(
      revision ? `template ${name} has no revision ${revision}` : `template not found: ${name}`,
      revision ? "template_revision_not_found" : "template_not_found"
    );
  }
  return row;
}

async function get(name, { revision = null, includeHtml = false } = {}) {
  const row = await loadCurrent(db, name, revision);
  return {
    template: templateRow({
      id: row.template_id,
      name: row.name,
      title: row.title,
      description: row.description,
      current_revision: row.revision,
      current_version_id: row.current_version_id,
      created_at: row.created_at,
      updated_at: row.updated_at,
    }),
    revision: revisionRow({
      id: row.id,
      revision: row.revision,
      content_sha256: row.content_sha256,
      config_schema_sha256: row.config_schema_sha256,
      data_schema_sha256: row.data_schema_sha256,
      sample_data: row.sample_data,
      author: row.author,
      source: row.source,
      note: row.note,
      created_at: row.revision_created_at,
    }),
    config_schema: row.config_schema,
    data_schema: row.data_schema,
    reference_config: row.reference_config,
    ...(includeHtml ? { html: row.html } : {}),
  };
}

// getRevisionHtml — the bytes for a preview. Returned with the revision's
// identity so the caller can bind a signed token to exactly these bytes.
async function getRevisionHtml(name, revision = null) {
  const row = await loadCurrent(db, name, revision);
  return {
    template: row.name,
    template_version_id: String(row.id),
    revision: Number(row.revision),
    content_sha256: row.content_sha256,
    // So the caller minting the token can say which of the two things the
    // preview will show before it is opened.
    has_sample_data: row.sample_data !== null && row.sample_data !== undefined,
    html: row.html,
  };
}

// previewBytes — what a preview actually renders, by template_version_id (which
// is what a signed template token names).
//
// A preview is the template MATERIALIZED, not its stored bytes: the example
// block is deleted and, when the revision carries an example dataset, it is
// poured into #pages-data. So the previewed document is exactly the shape a page
// built from this design would have — with rows in it, rather than the empty
// state a template necessarily ships.
async function previewBytes(templateVersionId) {
  const { rows } = await db.query(
    `SELECT v.id, v.html, v.sample_data, v.revision, t.name
       FROM page_template_versions v
       JOIN page_templates t ON t.id = v.template_id
      WHERE v.id = $1 AND t.deleted_at IS NULL`,
    [Number(templateVersionId)]
  );
  const row = rows[0];
  if (!row) throw notFound(`template revision not found: ${templateVersionId}`, "template_revision_not_found");

  const managed = pageData.parseManaged(row.html, pageData.TEMPLATE_SPEC);
  const usedSampleData = row.sample_data !== null && row.sample_data !== undefined;
  const now = Date.now();
  // No data key when there is no example dataset: materializeBlocks then leaves
  // the empty envelope byte-identical and only deletes the example block.
  const materialized = pageData.materializeBlocks(
    managed,
    usedSampleData ? { data: row.sample_data } : {},
    { sourceAsOf: new Date(now).toISOString(), now }
  );
  return {
    template: row.name,
    revision: Number(row.revision),
    html: materialized.html,
    used_sample_data: usedSampleData,
  };
}

async function list() {
  const { rows } = await db.query(
    `SELECT t.id, t.name, t.title, t.description, t.current_version_id,
            t.created_at, t.updated_at,
            v.revision AS current_revision,
            v.config_schema_sha256, v.data_schema_sha256,
            -- Two different questions, and conflating them is what hid template
            -- drift: page_count is every live page whose history touches this
            -- design, serving_count only those whose PUBLISHED version still
            -- comes from it. page_count - serving_count is the drift.
            (SELECT count(DISTINCT pv.page_id)
               FROM page_versions pv
               JOIN page_template_versions tv ON tv.id = pv.template_version_id
               JOIN pages p ON p.id = pv.page_id AND p.deleted_at IS NULL
              WHERE tv.template_id = t.id) AS page_count,
            (SELECT count(DISTINCT pv.page_id)
               FROM page_versions pv
               JOIN page_template_versions tv ON tv.id = pv.template_version_id
               JOIN pages p ON p.id = pv.page_id AND p.deleted_at IS NULL
              WHERE tv.template_id = t.id
                AND pv.id = p.published_version_id) AS serving_count
       FROM page_templates t
       LEFT JOIN page_template_versions v ON v.id = t.current_version_id
      WHERE t.deleted_at IS NULL
      ORDER BY t.name`
  );
  return {
    templates: rows.map((row) => ({
      ...templateRow(row),
      config_schema_sha256: row.config_schema_sha256,
      data_schema_sha256: row.data_schema_sha256,
      page_count: Number(row.page_count),
      serving_count: Number(row.serving_count),
      drifted_count: Number(row.page_count) - Number(row.serving_count),
    })),
  };
}

async function revisions(name) {
  const normalized = normalizeTemplateName(name);
  const { rows } = await db.query(
    `SELECT v.id, v.revision, v.content_sha256, v.config_schema_sha256, v.data_schema_sha256,
            v.author, v.source, v.note, v.created_at,
            (v.sample_data IS NOT NULL) AS has_sample_data,
            (v.id = t.current_version_id) AS is_current
       FROM page_templates t
       JOIN page_template_versions v ON v.template_id = t.id
      WHERE t.name = $1 AND t.deleted_at IS NULL
      ORDER BY v.revision DESC`,
    [normalized]
  );
  if (rows.length === 0) throw notFound(`template not found: ${normalized}`, "template_not_found");
  return {
    template: normalized,
    revisions: rows.map((row) => ({ ...revisionRow(row), is_current: row.is_current })),
  };
}

// hardcodedConfigValues — the honest limit of promoting a page.
//
// Moving a value into #pages-config makes it vary per instance ONLY if the design
// reads it from there. A page that also has it written into its markup —
// <title>Acme</title>, a heading, a legend — keeps that copy, and every page built
// from the design will show it. That is invisible until a client sees another
// client's name, so it is reported: for each config STRING value, does the same
// text appear anywhere outside the config block?
//
// Advisory, never blocking. A false positive is possible and harmless (a value
// that genuinely belongs to the design), and the caller is told where to look
// rather than being second-guessed.
const HARDCODED_MIN_CHARS = 4;
const HARDCODED_MAX_FINDINGS = 25;

function collectStrings(value, path, out) {
  if (typeof value === "string") {
    if (value.trim().length >= HARDCODED_MIN_CHARS) out.push({ path, value });
    return;
  }
  if (Array.isArray(value)) {
    value.forEach((entry, index) => collectStrings(entry, `${path}[${index}]`, out));
    return;
  }
  if (isPlainObject(value)) {
    for (const [key, entry] of Object.entries(value)) {
      collectStrings(entry, path === "" ? key : `${path}.${key}`, out);
    }
  }
}

function hardcodedConfigValues(html, parsed) {
  if (!parsed.configBlock || !isPlainObject(parsed.config)) return [];
  // Everything except the config block itself, so the value's own declaration
  // is not reported as a duplicate of itself.
  const outside =
    html.slice(0, parsed.configBlock.elementStart) + html.slice(parsed.configBlock.elementEnd);
  const candidates = [];
  collectStrings(parsed.config, "", candidates);

  const findings = [];
  const seen = new Set();
  for (const candidate of candidates) {
    if (findings.length >= HARDCODED_MAX_FINDINGS) break;
    if (seen.has(candidate.value)) continue;
    seen.add(candidate.value);
    let occurrences = 0;
    let at = outside.indexOf(candidate.value);
    while (at !== -1) {
      occurrences += 1;
      if (occurrences > 9) break;
      at = outside.indexOf(candidate.value, at + candidate.value.length);
    }
    if (occurrences > 0) findings.push({ path: candidate.path, value: candidate.value, occurrences });
  }
  return findings;
}

// createFromPage — promote a page you already like into a reusable design.
//
// This is the flow that should have existed first: you build a dashboard, it is
// good, and you want the next client's version of it. Nothing is re-authored and
// NOTHING MOVES — the design is already on this server, so the promotion reads
// the published bytes in place. The alternative was pulling ~90 KB through the
// model and pushing it straight back.
//
// It requires only that the page already separates its per-instance values into
// #pages-config, which is the shape every page should be authored in anyway (a
// derived config schema means that costs no schema authoring). A page that
// hardcodes its identity is refused with the reason, because promoting it would
// produce a "template" whose every instance said the same client's name.
async function createFromPage(
  { slug, name, empty_data: emptyData, title = null, description = null, note = null, example_from_current_data: exampleFromCurrent = false },
  actorCtx
) {
  name = normalizeTemplateName(name);
  if (!isPlainObject(emptyData)) {
    throw badRequest("empty_data must be the object a page built from this shows before its first refresh", "empty_data_required");
  }
  return db.withTransaction(async (client) => {
    const { rows } = await client.query(
      `SELECT p.slug, v.id AS version_id, v.html
         FROM pages p
         JOIN page_versions v ON v.id = p.published_version_id
        WHERE p.slug = $1 AND p.deleted_at IS NULL`,
      [versions.normalizeSlug(slug)]
    );
    const row = rows[0];
    if (!row) {
      throw notFound(`no published version to promote for page: ${slug}`, "page_not_published");
    }

    // The page's live data doubles as a ready-made example dataset — but it is a
    // real client's numbers, and a template is shared, so lifting it is opt-in
    // and the result says it happened.
    let exampleData = null;
    if (exampleFromCurrent === true) {
      exampleData = pageData.parseManagedHtml(row.html).envelope.data;
    }

    const assembled = pageData.assembleTemplate(row.html, { emptyData, exampleData });
    const result = await registerWithClient(
      client,
      { name, html: assembled.html, title, description, note, source: "mcp" },
      actorCtx
    );
    return {
      ...result,
      from_page: row.slug,
      from_version_id: String(row.version_id),
      example_from_current_data: exampleData !== null,
      hardcoded_config_values: hardcodedConfigValues(
        assembled.html,
        parseTemplateHtml(assembled.html)
      ),
    };
  });
}

// remove — retire a template and free its name.
//
// This exists because registering is easy and a name is permanent otherwise: a
// typo'd template used to need SQL to undo, and the library now shows it, with a
// rendered preview, to everyone.
//
// Soft delete, like a page: the revisions stay, so pages that pinned one keep
// their provenance row and the deleted_at partial index frees the name for
// re-registration. Refused by default when pages were built from it, because that
// is nearly always the typo case being confused with a real retirement — `force`
// is how you say you meant a retirement. Those pages keep serving either way:
// they carry their own materialized HTML and never read the template at runtime.
async function remove({ template, force = false }, actorCtx) {
  const name = normalizeTemplateName(template);
  return db.withTransaction(async (client) => {
    const { rows } = await client.query(
      `SELECT id, name FROM page_templates WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`,
      [name]
    );
    const row = rows[0];
    if (!row) throw notFound(`template not found: ${name}`, "template_not_found");

    const built = Number(
      (
        await client.query(
          `SELECT count(DISTINCT pv.page_id) AS pages
             FROM page_versions pv
             JOIN page_template_versions tv ON tv.id = pv.template_version_id
             JOIN pages p ON p.id = pv.page_id AND p.deleted_at IS NULL
            WHERE tv.template_id = $1`,
          [row.id]
        )
      ).rows[0].pages
    );
    if (built > 0 && force !== true) {
      throw conflict(
        `${built} page${built === 1 ? " was" : "s were"} built from ${name}; pass force to retire it anyway. ` +
          `Those pages keep serving, but they lose their design provenance and can no longer be re-rendered ` +
          `from it. Call list_template_pages first to see which.`,
        "template_has_pages",
        { pages: built }
      );
    }

    await client.query(
      `UPDATE page_templates SET deleted_at = now(), updated_at = now() WHERE id = $1`,
      [row.id]
    );
    await audit.write(client, {
      ...actorCtx,
      action: "delete_template",
      metadata: { template: name, template_id: String(row.id), pages_built: built, forced: force === true },
    });
    return { template: name, deleted: true, pages_built: built };
  });
}

// ── building a page ─────────────────────────────────────────────────────────

// createPage — the cost win, in one call. The caller sends the config that
// actually differs plus (optionally) data; the server renders the design it
// already stores. `config` is ALWAYS required in full and never inherited from
// the template's reference config: a partially-specified campaign that silently
// kept another client's identity is exactly the failure a template makes easy,
// so it is refused by construction.
async function createPage(
  {
    template,
    revision = null,
    slug,
    config,
    data = null,
    sourceAsOf = null,
    title = "",
    clientId = null,
    requireApproval = false,
    renderMode = "themed",
    publish = true,
    note = null,
    now = Date.now(),
  },
  actorCtx
) {
  const normalizedSlug = versions.normalizeSlug(slug);
  if (!isPlainObject(config)) {
    throw badRequest("config is required and must be a complete object", "config_required");
  }

  return db.withTransaction(async (client) => {
    const templateRow = await loadCurrent(client, template, revision);
    const parsed = parseTemplateHtml(templateRow.html);

    // Data is optional: a campaign is normally deployed empty and filled by the
    // first update_page_data. When omitted, the template's own empty-state data
    // is kept — that is what makes its awaiting-first-ingest states render
    // instead of fabricated zeros — but its source coverage is reset to the
    // epoch rather than inherited. Source coverage is monotonic per page, so
    // inheriting whatever timestamp the template happened to ship would put a
    // floor under the page and make the first ingest of older data fail with
    // source_regression.
    const materialized = pageData.materializeBlocks(
      parsed,
      data === null ? { config, data: parsed.envelope.data } : { config, data },
      { sourceAsOf: data === null ? NO_SOURCE_COVERAGE : sourceAsOf, now }
    );

    const existing = (
      await client.query(
        `SELECT id, published_version_id FROM pages WHERE slug = $1 AND deleted_at IS NULL FOR UPDATE`,
        [normalizedSlug]
      )
    ).rows[0];

    if (existing) {
      // Is this the SAME build arriving twice? Compare the semantic identity the
      // data path already dedupes on — not the rendered bytes, which differ every
      // call because Pages stamps refreshed_at. template_sha256 covers the design
      // and the config, so an equal triple means nothing about this page would
      // change. deployLocked then dedupes onto that existing immutable version.
      //
      // Scoped to what the page is CURRENTLY SERVING, and that scope is the whole
      // guard. Matching any historical version made the retry allowance mean
      // something it was never meant to: after the page took a refresh and a
      // config edit, replaying the original create matched the FIRST version,
      // deployLocked deduped onto it, and — because that row is not the pointer —
      // publish moved the live pointer BACKWARD. A real client dashboard reverted
      // to its awaiting-first-ingest state, discarding every refresh since, and
      // the result said deduped:true, version_is_live:true and "share urls.live".
      //
      // When nothing is published (pointer NULL) the match must instead be the
      // page's NEWEST version. Three things reach a NULL pointer and only two of
      // them are a died-mid-turn retry: a brand-new gated page whose version is
      // still pending, and a create that died before publishing — in both the
      // build IS the newest row. The third is an admin `restore_page`, which
      // clears `deleted_at` but leaves the pointer NULL (versions.js restorePage),
      // so a restored page has a NULL pointer AND a full history. Allowing any
      // match there would republish the original empty state over a page with
      // refreshes behind it — the same revert by a different route.
      const sameBuild = await client.query(
        `SELECT 1 FROM page_versions
          WHERE page_id = $1 AND data_sha256 = $2 AND data_template_sha256 = $3
            AND source_as_of = $4::timestamptz AND status <> 'rejected'
            AND id = COALESCE(
              $5::bigint,
              (SELECT MAX(id) FROM page_versions WHERE page_id = $1 AND status <> 'rejected')
            )
          LIMIT 1`,
        [
          existing.id,
          materialized.data_sha256,
          materialized.template_sha256,
          materialized.envelope.source_as_of,
          existing.published_version_id,
        ]
      );
      // A turn that died after the commit may safely repeat itself. Anything
      // else is refused: this tool builds a page, and quietly replacing BOTH
      // halves of a live client dashboard is not that. The dedicated tools say
      // what they do.
      if (sameBuild.rows.length === 0) {
        throw conflict(
          `page ${normalizedSlug} already exists; use update_page_config to change its settings, ` +
            `update_page_data to refresh its numbers, rerender_page_from_template to move it to a new design, ` +
            `or rollback_page to deliberately return it to an earlier version`,
          "page_exists"
        );
      }
    }

    const binding = {
      templateName: templateRow.name,
      templateVersionId: templateRow.id,
      revision: Number(templateRow.revision),
      configSha: materialized.config_sha256,
    };

    const result = await versions.createAndDeployWithClient(
      client,
      {
        slug: normalizedSlug,
        html: materialized.html,
        renderMode,
        note,
        source: "mcp",
        publish,
        title,
        clientId,
        requireApproval,
        dataMetadata: {
          action: "template_build",
          dataSha: materialized.data_sha256,
          schemaSha: materialized.schema_sha256,
          templateSha: materialized.template_sha256,
          sourceAsOf: materialized.envelope.source_as_of,
          refreshedAt: materialized.envelope.refreshed_at,
        },
        templateBinding: binding,
      },
      actorCtx
    );

    return {
      ...result,
      template: templateRow.name,
      template_revision: binding.revision,
      template_version_id: String(templateRow.id),
      config: materialized.config,
      config_sha256: materialized.config_sha256,
      config_schema_sha256: parsed.config_schema_sha256,
      envelope: materialized.envelope,
      data_sha256: materialized.data_sha256,
      schema_sha256: materialized.schema_sha256,
      template_sha256: materialized.template_sha256,
      html: materialized.html,
    };
  });
}

// pageTemplateBinding — which template revision produced the page currently
// being served, or null when it was authored directly. Cheap: reads the
// provenance columns instead of parsing the page's HTML.
async function pageTemplateBinding(slug) {
  const normalizedSlug = versions.normalizeSlug(slug);
  const { rows } = await db.query(
    `SELECT t.name AS template, tv.revision, tv.config_schema_sha256, tv.data_schema_sha256,
            tv.id AS template_version_id, v.config_sha256,
            (tv.id = t.current_version_id) AS is_current,
            current.revision AS current_revision
       FROM pages p
       JOIN page_versions v ON v.id = p.published_version_id
       JOIN page_template_versions tv ON tv.id = v.template_version_id
       JOIN page_templates t ON t.id = tv.template_id
       LEFT JOIN page_template_versions current ON current.id = t.current_version_id
      WHERE p.slug = $1 AND p.deleted_at IS NULL`,
    [normalizedSlug]
  );
  const row = rows[0];
  if (!row) return null;
  return {
    template: row.template,
    revision: Number(row.revision),
    template_version_id: String(row.template_version_id),
    config_schema_sha256: row.config_schema_sha256,
    data_schema_sha256: row.data_schema_sha256,
    config_sha256: row.config_sha256,
    is_current_revision: row.is_current === true,
    current_revision: row.current_revision === null ? null : Number(row.current_revision),
  };
}

// ── propagation (always user-initiated) ─────────────────────────────────────
//
// A new revision never moves a deployed page by itself. These two calls exist so
// a human can SEE who is behind and then move pages one at a time, previewing
// each: listPages is read-only, and rerender defaults to publish=false. There is
// deliberately no fan-out primitive — no single call can rewrite every live
// client dashboard, because a bad design revision would then be a fleet-wide
// incident instead of one page to fix.

// Answering "what does a design fix have to move?" needs BOTH populations, and
// the old query returned only one of them. It required v.id =
// p.published_version_id, so a page that was built from the template and later
// overwritten by a raw deploy_page or patch_page silently vanished: it appeared in
// no tool at all, while list_templates still counted it. On the live server that
// read as page_count 5 against 2 listed pages, with the 3 forked pages
// unreachable — the exact question "which pages drifted off this template?" had no
// answer anywhere in the API.
//
// So enumerate every live page whose history touches the template, and say for
// each whether the version a VIEWER IS SERVED still comes from it. A drifted page
// keeps last_revision, which is what rerender_page_from_template needs to pull it
// back onto the shared design.
async function listTemplatePages(name) {
  const normalized = normalizeTemplateName(name);
  const template = await loadCurrent(db, normalized);
  const { rows } = await db.query(
    `WITH built AS (
        SELECT DISTINCT pv.page_id
          FROM page_versions pv
          JOIN page_template_versions tv ON tv.id = pv.template_version_id
         WHERE tv.template_id = $1
     )
     SELECT p.slug, p.title, p.disabled, p.published_version_id,
            live.id AS live_version_id,
            live.config_sha256,
            livetv.revision AS live_revision,
            livetv.template_id AS live_template_id,
            (SELECT max(tv2.revision)
               FROM page_versions pv2
               JOIN page_template_versions tv2 ON tv2.id = pv2.template_version_id
              WHERE pv2.page_id = p.id AND tv2.template_id = $1) AS last_revision
       FROM built b
       JOIN pages p ON p.id = b.page_id AND p.deleted_at IS NULL
       LEFT JOIN page_versions live ON live.id = p.published_version_id
       LEFT JOIN page_template_versions livetv ON livetv.id = live.template_version_id
      ORDER BY p.slug`,
    [template.template_id]
  );
  const currentRevision = Number(template.revision);
  const pages = rows.map((row) => {
    // Serving this design means the PUBLISHED version is bound to a revision of
    // THIS template. A page published from another template's revision, or from a
    // hand-deployed version with no binding at all, or with nothing published, is
    // no longer being served the shared design.
    const serving =
      row.live_template_id !== null &&
      row.live_template_id !== undefined &&
      String(row.live_template_id) === String(template.template_id);
    return {
      slug: row.slug,
      title: row.title,
      live_version_id: row.live_version_id === null ? null : String(row.live_version_id),
      revision: serving ? Number(row.live_revision) : null,
      // "Behind" is about the DESIGN a viewer is currently served, so it is
      // measured on the published version only — and a drifted page is not
      // "behind" the current revision, it is off the design entirely.
      behind: serving ? Number(row.live_revision) < currentRevision : false,
      drifted: !serving,
      last_revision: row.last_revision === null ? null : Number(row.last_revision),
      page_is_live: !!row.published_version_id && !row.disabled,
      config_sha256: serving ? row.config_sha256 : null,
    };
  });
  const drifted = pages.filter((page) => page.drifted).length;
  return {
    template: normalized,
    current_revision: currentRevision,
    serving_count: pages.length - drifted,
    drifted_count: drifted,
    pages,
  };
}

// rerender — move ONE page onto a template revision, keeping its own config and
// data. The page's published HTML is the source of both: nothing is re-derived
// from a stored copy that could have drifted from what is actually serving.
// publish defaults to false so a human previews the new design before a client
// sees it.
async function rerenderPage(
  { slug, template = null, revision = null, expectedVersion = null, publish = false, note = null },
  actorCtx
) {
  const normalizedSlug = versions.normalizeSlug(slug);

  return db.withTransaction(async (client) => {
    const page = await versions.lockPage(client, normalizedSlug);
    if (expectedVersion !== undefined && expectedVersion !== null) {
      versions.assertExpectedVersion(page, expectedVersion);
    }
    if (!page.published_version_id) {
      throw conflict("page has no published version to rerender", "update_page_not_published");
    }
    const currentResult = await client.query(
      `SELECT id, html, render_mode, template_version_id
         FROM page_versions
        WHERE id = $1 AND page_id = $2`,
      [page.published_version_id, page.id]
    );
    const current = currentResult.rows[0];
    if (!current) throw conflict("published version is unavailable", "update_page_not_published");
    // A DRIFTED page has no binding on its published version: a later raw
    // deploy_page or patch_page replaced the template-built version, and
    // prepareDeploy writes no binding. Since list_template_pages now reports those
    // pages, this is the call that has to be able to act on them — otherwise the
    // report is a dead end that names a problem with no remedy. Re-attaching needs
    // the caller to say WHICH design, because there is no binding left to infer it
    // from.
    if (!current.template_version_id && !template) {
      throw conflict(
        "page's published version is not bound to any template, so there is no design to move it to. " +
          "If it drifted off one — list_template_pages reports drifted:true and the last_revision it was " +
          "on — name that template explicitly to re-attach it.",
        "page_not_template_managed"
      );
    }

    // Which template? Default to the one this page is already on, so a caller
    // only has to name the revision they want.
    const owner = current.template_version_id
      ? (
          await client.query(
            `SELECT t.name FROM page_template_versions tv
               JOIN page_templates t ON t.id = tv.template_id
              WHERE tv.id = $1`,
            [current.template_version_id]
          )
        ).rows[0]
      : null;
    const targetRow = await loadCurrent(client, template || owner.name, revision);

    // The page's own config and data come from what is SERVING, never from a
    // stored copy that could have drifted from it. That is also what decides
    // whether a drifted page is recoverable: patch_page edits the design around
    // these blocks and leaves them intact, so re-attaching works. A page
    // hand-deployed all the way down to plain HTML has no config or data left to
    // carry, and saying that plainly beats failing on a block-shape error.
    let live;
    try {
      live = pageData.parseManaged(current.html, pageData.TEMPLATE_SPEC);
    } catch (error) {
      if (!current.template_version_id && error && error.code) {
        throw conflict(
          `page ${normalizedSlug} drifted off its template and its published HTML no longer carries the ` +
            `#pages-config and #pages-data blocks a rerender reads, so its config and data cannot be ` +
            `recovered from what is serving. Roll back to the last template-built version (list_versions), ` +
            `or rebuild it with create_page_from_template. (${error.message})`,
          "page_not_template_managed"
        );
      }
      throw error;
    }
    const target = parseTemplateHtml(targetRow.html);

    if (String(targetRow.id) === String(current.template_version_id)) {
      throw conflict(
        `page ${normalizedSlug} is already on ${targetRow.name} revision ${targetRow.revision}`,
        "template_revision_unchanged"
      );
    }

    // The page's own config and data are re-validated against the TARGET
    // revision's schemas. A revision that tightened its contract fails here,
    // loudly and before anything is written, instead of producing a page whose
    // payload its own schema rejects.
    const materialized = pageData.materializeBlocks(target, {
      config: live.config,
      data: live.envelope.data,
    }, { sourceAsOf: live.envelope.source_as_of, now: Date.now() });

    const prepared = versions.prepareDeploy(
      {
        slug: normalizedSlug,
        html: materialized.html,
        renderMode: current.render_mode,
        note,
        source: "mcp",
        publish,
        expectedVersion: expectedVersion === null ? undefined : expectedVersion,
        dataMetadata: {
          action: "template_rerender",
          dataSha: materialized.data_sha256,
          schemaSha: materialized.schema_sha256,
          templateSha: materialized.template_sha256,
          sourceAsOf: materialized.envelope.source_as_of,
          refreshedAt: materialized.envelope.refreshed_at,
        },
        templateBinding: {
          templateName: targetRow.name,
          templateVersionId: targetRow.id,
          revision: Number(targetRow.revision),
          configSha: materialized.config_sha256,
        },
      },
      actorCtx
    );
    const result = await versions.deployLocked(client, page, prepared, actorCtx);
    return {
      ...result,
      template: targetRow.name,
      // null when the page was drifted: it was serving no revision of any design.
      from_revision: current.template_version_id
        ? Number(
            (
              await client.query(`SELECT revision FROM page_template_versions WHERE id = $1`, [
                current.template_version_id,
              ])
            ).rows[0].revision
          )
        : null,
      reattached: !current.template_version_id,
      template_revision: Number(targetRow.revision),
      template_version_id: String(targetRow.id),
      config: materialized.config,
      config_sha256: materialized.config_sha256,
      envelope: materialized.envelope,
      data_sha256: materialized.data_sha256,
      template_sha256: materialized.template_sha256,
      html: materialized.html,
    };
  });
}

function isPlainObject(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

module.exports = {
  NAME_RE,
  MAX_NAME_CHARS,
  TEMPLATE_SOURCES,
  createPage,
  pageTemplateBinding,
  listTemplatePages,
  rerenderPage,
  normalizeTemplateName,
  parseTemplateHtml,
  validateHtml,
  getRevisionHtml,
  previewBytes,
  // Exported for the unit test that pins this shape to the MCP output schema.
  revisionRow,
  register,
  registerWithClient,
  createFromPage,
  remove,
  loadCurrent,
  get,
  list,
  revisions,
};
