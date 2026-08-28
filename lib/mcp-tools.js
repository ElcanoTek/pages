// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Typed Pages tool registry for MCP. The transport lives in lib/mcp.js; this
// module owns the domain-facing schemas and handlers so every tool is a thin,
// validated adapter over the same versions/workspaces state machines used by
// REST and the admin UI.

const crypto = require("node:crypto");
const z = require("zod/v4");
const versions = require("./versions");
const workspaces = require("./workspaces");
const updatePrompts = require("./update-prompts");
const pageUploads = require("./page-uploads");
const templates = require("./templates");
const preflight = require("./preflight");
const uploadTicket = require("./uploadticket");
const pagePatch = require("./page-patch");
const rawtoken = require("./rawtoken");
const { badRequest, conflict } = require("./apierror");
const { DASHBOARD_ORIGIN, CONTENT_ORIGIN } = require("./csp");

// Matches the admin library's preview-token TTL. Long enough to open a link,
// short enough that a leaked URL is not a standing capability.
const TEMPLATE_PREVIEW_TTL_SECONDS = 300;

const hasOwn = (value, key) => Object.prototype.hasOwnProperty.call(value, key);

function pageUrls(slug) {
  return {
    admin: `${DASHBOARD_ORIGIN}/admin/${slug}`,
    view: `${DASHBOARD_ORIGIN}/view/${slug}`,
    live: `${CONTENT_ORIGIN}/${slug}`,
  };
}

// PostgreSQL BIGINT values are decimal strings in node-postgres. Advertise
// those strings in results, while accepting either a returned string or a safe
// positive integer from clients.
const IdOut = z.string().regex(/^[1-9][0-9]*$/);
const DecimalIdArg = z
  .string()
  .regex(/^[1-9][0-9]*$/)
  .refine(
    (value) => !/^[1-9][0-9]*$/.test(value) || BigInt(value) <= 9223372036854775807n,
    "ID exceeds PostgreSQL's BIGINT range"
  );
const IdArg = z
  .union([z.number().int().positive().safe(), DecimalIdArg])
  .transform((value) => String(value));
const NullableIdArg = z.union([IdArg, z.null()]);
const DateTime = z.string().datetime({ offset: true });
const NullableDateTime = DateTime.nullable();
const Slug = z.string().min(1).max(512).describe("Page slug, for example 'northwind' or 'northwind/q2'.");
const Sha256 = z.string().regex(/^[0-9a-f]{64}$/);
const Cursor = z.string().min(1).max(2048);
const PageLimit = z.number().int().min(1).max(100).default(50);

const UrlsSchema = z
  .object({
    admin: z.string().url().describe("Elcano-staff admin screen for this page (SSO, staff only)."),
    view: z
      .string()
      .url()
      .describe(
        "Elcano-staff shortcut that opens the live page without its client password. Staff only — do NOT share this with a client; give them `live`."
      ),
    live: z
      .string()
      .url()
      .describe("The client link. Share this one; a password-protected page prompts for its password here."),
  })
  .strict();

const PageSchema = z
  .object({
    id: IdOut,
    slug: z.string(),
    title: z.string(),
    client_id: z.string().nullable(),
    workspace_id: IdOut.nullable(),
    workspace_name: z.string().nullable(),
    theme_id: IdOut.nullable(),
    theme_name: z.string(),
    require_approval: z.boolean(),
    disabled: z.boolean(),
    published_version_id: IdOut.nullable(),
    created_at: DateTime,
    updated_at: DateTime,
    has_password: z.boolean(),
  })
  .strict();

// FreshnessSchema — the computed answer to "when was this last actually
// right?". Both stamps are already columns on page_versions; the days_* fields
// are derived at read time so a consumer can rank an estate by staleness in one
// call instead of noticing a frozen dashboard when a client asks.
//
// Pages states no opinion on whether a number is too high: it retired its
// scheduler deliberately and does not know any page's expected cadence. It
// reports; the consumer decides.
const FreshnessSchema = z
  .object({
    source_as_of: NullableDateTime.describe("Latest source coverage the published data represents."),
    refreshed_at: NullableDateTime.describe("When that data was written to Pages."),
    checked_at: NullableDateTime.describe(
      "Last time anyone LOOKED, whether or not it produced a version — max(refreshed_at, recorded check)."
    ),
    last_check_outcome: z.string().nullable(),
    last_check_detail: z.string().nullable(),
    last_check_source_as_of: NullableDateTime,
    days_since_source: z.number().int().nonnegative().nullable(),
    days_since_refresh: z.number().int().nonnegative().nullable(),
    days_since_check: z.number().int().nonnegative().nullable(),
  })
  .strict()
  .nullable();

// Mirrors the CHECK constraint in migrations/020_page_refresh_checks.sql. Closed
// on purpose: these values are scanned by humans and branched on by consumers,
// and free text would serve neither.
const RefreshCheckOutcome = z
  .enum([...versions.REFRESH_CHECK_OUTCOMES])
  .describe(
    "updated: a version was published. source_not_updated: the source held no coverage this page lacks. " +
      "source_unreachable: the bound source could not be reached or authenticated. blocked: a gate stopped the run " +
      "(schema drift, approval required, missing tool). failed: the run errored."
  );

const PageSummarySchema = PageSchema.extend({
  is_live: z.boolean(),
  urls: UrlsSchema,
  freshness: FreshnessSchema,
}).strict();

const VersionSchema = z
  .object({
    id: IdOut,
    page_id: IdOut,
    content_sha256: z.string(),
    status: z.enum(["draft", "pending", "approved", "rejected"]),
    render_mode: z.enum(["themed", "raw"]),
    author: z.string().nullable(),
    source: z.enum(["api", "mcp", "admin"]),
    note: z.string().nullable(),
    reviewed_by: z.string().nullable(),
    reviewed_at: NullableDateTime,
    created_at: DateTime,
  })
  .strict();

const VersionListSchema = VersionSchema.extend({
  is_published: z.boolean(),
  is_live: z.boolean(),
}).strict();

const VersionWithHtmlSchema = VersionSchema.extend({ html: z.string() }).strict();

const WorkspaceSchema = z
  .object({
    id: IdOut,
    name: z.string().min(1).max(100),
    page_count: z.number().int().min(0),
    created_at: DateTime,
    updated_at: DateTime,
  })
  .strict();

const ThemeSchema = z
  .object({
    id: IdOut,
    name: z.string(),
    default_mode: z.string(),
  })
  .strict();

// JSON-RPC has already constrained these values to JSON. Keep the MCP schema
// object-rooted while allowing each page's embedded JSON Schema to own the
// actual property contract.
const JsonObjectSchema = z.record(z.string(), z.unknown());
const DataEnvelopeSchema = z
  .object({
    contract_version: z.literal(1),
    refreshed_at: DateTime,
    source_as_of: DateTime,
    data: JsonObjectSchema,
  })
  .strict();

const TemplateName = z
  .string()
  .min(1)
  .max(templates.MAX_NAME_CHARS)
  .describe("Template name, for example 'nwm-campaign-dashboard'. No slashes.");

const TemplateSchema = z
  .object({
    id: IdOut,
    name: z.string(),
    title: z.string(),
    description: z.string(),
    current_revision: z.number().int().positive().nullable(),
    current_version_id: IdOut.nullable(),
    created_at: DateTime,
    updated_at: DateTime,
  })
  .strict();

const TemplateRevisionSchema = z
  .object({
    version_id: IdOut,
    revision: z.number().int().positive(),
    content_sha256: Sha256,
    config_schema_sha256: Sha256,
    data_schema_sha256: Sha256,
    author: z.string(),
    // MUST stay in step with templates.TEMPLATE_SOURCES. "cli" is a file-backed
    // registration by an operator (scripts/template.js); it was added to the
    // domain enum without being added here, so every template registered by
    // `pages template sync` — i.e. every template this repo ships — made
    // get_template and list_template_revisions fail output validation with
    // -32602. A unit test now asserts the two sets are equal.
    source: z.enum([...templates.TEMPLATE_SOURCES]),
    note: z.string().nullable(),
    // Whether the revision carries a preview-only example dataset. The rows are
    // never returned — they exist so the library can show the design populated,
    // and they are deleted from every page built from it. This schema is strict,
    // so a field added to templates.revisionRow() and not added here fails output
    // validation with -32602 on three tools; a unit test pins the two together.
    has_sample_data: z.boolean(),
    created_at: DateTime,
  })
  .strict();

// What the payload CONTAINS, alongside the hashes that say what it IS. A schema
// validates shape; #102 shipped two schema-perfect payloads to a client (a
// 24-day coverage gap and a dropped deal worth ~7% of spend) because nothing
// reported the contents. Bounded in lib/page-data.js so a big payload cannot
// flood a model's context.
const DataProfileFieldSchema = z
  .object({
    kind: z.enum(["number", "date", "key", "text"]),
    sum: z.number().nullable().optional(),
    min: z.union([z.number(), z.string()]).nullable().optional(),
    max: z.union([z.number(), z.string()]).nullable().optional(),
    distinct: z.number().int().nonnegative().optional(),
    distinct_overflow: z.boolean().optional(),
    values: z.record(z.string(), z.number().int().nonnegative()).optional(),
    values_omitted: z.number().int().nonnegative().optional(),
    nulls: z.number().int().nonnegative(),
  })
  .strict();

const DataProfileSchema = z
  .object({
    arrays: z.record(
      z.string(),
      z.object({ count: z.number().int().nonnegative(), fields: z.record(z.string(), DataProfileFieldSchema) }).strict()
    ),
    scalars: z.record(z.string(), z.union([z.string(), z.number(), z.boolean(), z.null()])),
  })
  .strict();

const DataWarningSchema = z
  .object({
    code: z.enum([
      "row_count_dropped",
      "coverage_start_regressed",
      "coverage_end_regressed",
      "dimension_values_missing",
      "coverage_unverified",
      // "This refresh added nothing" — a real new version whose numbers did not
      // move. Suppressed on a dedupe, which reports that in its own fields.
      "data_unchanged",
      "coverage_did_not_advance",
      // Emitted only when the list itself was capped; see MAX_DATA_WARNINGS.
      "warnings_truncated",
    ]),
    path: z.string(),
    message: z.string(),
    previous: z.union([z.number(), z.string(), z.array(z.string())]),
    current: z.union([z.number(), z.string(), z.array(z.string())]),
  })
  .strict();

const PageDataStateShape = {
  version: VersionSchema,
  envelope: DataEnvelopeSchema,
  data_sha256: z.string().regex(/^[0-9a-f]{64}$/),
  schema_sha256: z.string().regex(/^[0-9a-f]{64}$/),
  template_sha256: z.string().regex(/^[0-9a-f]{64}$/),
  version_is_live: z.boolean(),
  page_is_live: z.boolean(),
  live_version_id: IdOut.nullable(),
  urls: UrlsSchema,
};

const ReadAnnotations = Object.freeze({
  readOnlyHint: true,
  destructiveHint: false,
  idempotentHint: true,
  openWorldHint: false,
});

const WriteAnnotations = Object.freeze({
  readOnlyHint: false,
  destructiveHint: true,
  idempotentHint: false,
  openWorldHint: false,
});

const IdempotentWriteAnnotations = Object.freeze({
  readOnlyHint: false,
  destructiveHint: true,
  idempotentHint: true,
  openWorldHint: false,
});

const AdditiveAnnotations = Object.freeze({
  readOnlyHint: false,
  destructiveHint: false,
  idempotentHint: false,
  openWorldHint: false,
});

const IdempotentAdditiveAnnotations = Object.freeze({
  readOnlyHint: false,
  destructiveHint: false,
  idempotentHint: true,
  openWorldHint: false,
});

function cursorHash(filters) {
  return crypto.createHash("sha256").update(JSON.stringify(filters)).digest("base64url").slice(0, 22);
}

function encodeCursor(kind, after, filters) {
  return Buffer.from(JSON.stringify({ v: 1, kind, after, filters: cursorHash(filters) }), "utf8").toString("base64url");
}

function decodeCursor(encoded, kind, filters) {
  if (!encoded) return null;
  try {
    if (!/^[A-Za-z0-9_-]+$/.test(encoded) || encoded.length > 2048) throw new Error("bad encoding");
    const parsed = JSON.parse(Buffer.from(encoded, "base64url").toString("utf8"));
    if (
      !parsed ||
      parsed.v !== 1 ||
      parsed.kind !== kind ||
      parsed.filters !== cursorHash(filters) ||
      !parsed.after ||
      typeof parsed.after !== "object"
    ) {
      throw new Error("bad cursor payload");
    }
    return parsed.after;
  } catch {
    throw badRequest("invalid or stale pagination cursor", "invalid_cursor");
  }
}

function pageFilters(args) {
  return {
    query: args.query || null,
    workspace_id: hasOwn(args, "workspace_id") ? args.workspace_id : "*",
    client_id: hasOwn(args, "client_id") ? args.client_id : "*",
    is_live: hasOwn(args, "is_live") ? args.is_live : "*",
    require_approval: hasOwn(args, "require_approval") ? args.require_approval : "*",
    disabled: hasOwn(args, "disabled") ? args.disabled : "*",
  };
}

function versionFilters(args) {
  return { slug: args.slug, status: args.status || null };
}

function listPageOptions(args, after) {
  const options = { limit: args.limit + 1 };
  if (after) options.after = { createdAt: after.created_at, id: after.id };
  if (args.query !== undefined) options.query = args.query;
  if (hasOwn(args, "workspace_id")) {
    options.workspaceFilterSet = true;
    options.workspaceId = args.workspace_id;
  }
  if (hasOwn(args, "client_id")) options.clientId = args.client_id;
  if (hasOwn(args, "is_live")) options.isLive = args.is_live;
  if (hasOwn(args, "require_approval")) options.requireApproval = args.require_approval;
  if (hasOwn(args, "disabled")) options.disabled = args.disabled;
  return options;
}

async function listPages(args) {
  const filters = pageFilters(args);
  const after = decodeCursor(args.cursor, "pages", filters);
  if (after && (typeof after.created_at !== "string" || !/^[1-9][0-9]*$/.test(String(after.id)))) {
    throw badRequest("invalid pagination cursor", "invalid_cursor");
  }
  const rows = await versions.listPages(listPageOptions(args, after));
  const hasMore = rows.length > args.limit;
  const pageRows = rows.slice(0, args.limit);
  const pages = pageRows.map((page) => ({ ...page, urls: pageUrls(page.slug) }));
  const last = pageRows[pageRows.length - 1];
  return {
    pages,
    next_cursor:
      hasMore && last
        ? encodeCursor("pages", { created_at: new Date(last.created_at).toISOString(), id: String(last.id) }, filters)
        : null,
  };
}

async function listVersions(args) {
  const filters = versionFilters(args);
  const after = decodeCursor(args.cursor, "versions", filters);
  if (after && (typeof after.created_at !== "string" || !/^[1-9][0-9]*$/.test(String(after.id)))) {
    throw badRequest("invalid pagination cursor", "invalid_cursor");
  }
  const options = { limit: args.limit + 1 };
  if (after) options.after = { createdAt: after.created_at, id: after.id };
  if (args.status !== undefined) options.status = args.status;
  const rows = await versions.listVersions(args.slug, options);
  const hasMore = rows.length > args.limit;
  const versionRows = rows.slice(0, args.limit);
  const last = versionRows[versionRows.length - 1];
  return {
    versions: versionRows,
    next_cursor:
      hasMore && last
        ? encodeCursor("versions", { created_at: new Date(last.created_at).toISOString(), id: String(last.id) }, filters)
        : null,
  };
}

function compareIds(a, b) {
  return a !== null && a !== undefined && b !== null && b !== undefined && String(a) === String(b);
}

function deployGuidance(result, page, slug) {
  const urls = pageUrls(slug);
  const versionIsLive = !page.disabled && compareIds(result.version.id, page.published_version_id);
  const pageIsLive = !!page.published_version_id && !page.disabled;
  const liveVersionId = page.published_version_id || null;
  let nextStep;

  if (versionIsLive) {
    nextStep = "This version is live now — share urls.live with the user or client.";
  } else if (page.disabled) {
    nextStep = "The version was saved, but the page is disabled by an admin and is not serving. Ask a human admin to re-enable it; do not share urls.live yet.";
  } else if (result.gated) {
    const prior = pageIsLive
      ? ` The previously published version ${liveVersionId} remains live at urls.live until review completes.`
      : " Nothing is published yet, so urls.live does not serve a page.";
    if (result.version.status === "approved") {
      nextStep = `This exact content already exists as approved version ${result.version.id}, but it is not the published version. A human must publish it from urls.admin.${prior}`;
    } else {
      nextStep = `Approval-gated page: this version is pending human review. Send the user urls.admin; agents cannot approve it.${prior}`;
    }
  } else if (result.version.status === "approved") {
    nextStep = `This exact content already exists as approved version ${result.version.id}. Call rollback_page with that version_id to publish it.`;
  } else {
    nextStep = `Draft saved. Call publish_page with version_id ${result.version.id} to publish it.`;
  }

  return {
    version: result.version,
    deduped: result.deduped,
    published: result.published,
    gated: result.gated,
    live: versionIsLive,
    version_is_live: versionIsLive,
    page_is_live: pageIsLive,
    live_version_id: liveVersionId,
    next_step: nextStep,
    urls,
  };
}

const FILE_PLACEHOLDER_RE = /^(?:\$\(\s*cat(?:\s+[^)]*)?\s*\)|PLACEHOLDER|REPLACE_ME|FILE_CONTENT_PLACEHOLDER)$/i;

// Four tool descriptions have always said "for content over 20,000 UTF-8 bytes,
// use the staged page-upload tools instead" — and nothing enforced it, so a caller
// that ignored the advice sent 43 KB of HTML inline and Pages took it. That is the
// single most expensive thing an agent can do here, and not because of the one
// call: those bytes then sit in the conversation and are re-sent on every
// subsequent step. One observed session paid $100 across four turns for 358k
// tokens of output and 4.2M of prompt.
//
// So the ceiling is real now. Refusing is kinder than accepting: the error names
// the two cheap paths, and an agent that is told why switches, where an agent that
// silently succeeds does the same thing again tomorrow.
const MAX_INLINE_HTML_BYTES = envInt("PAGES_MCP_MAX_INLINE_HTML_BYTES", 20000);

// The managed-data ceiling is a DIFFERENT number for a different reason, and
// saying so out loud is the whole point of it.
//
// HTML is capped low because a cheaper path always exists: the design is either
// already a stored template or belongs in a staged upload. A data payload has no
// such alternative — it is computed from the source on every refresh, it is the
// deliverable, and nothing about it can be deduplicated into a template. So the
// only honest ceiling is the one the transport actually imposes: the dashboard
// app parses request bodies at MAX_HTML_BYTES (default 2mb, server.js), and the
// payload travels as JSON inside that body alongside the rest of the arguments.
// This leaves ~600 KB of headroom under that limit.
//
// It is stated here, in the tool description, and in the refusal because the
// absence of a stated number is itself what broke a production refresh. A daily
// NWM run built a complete, schema-valid 978 KB payload — 846 new rows, nothing
// dropped, schema hash matched — then declined to send it, flagged
// `file_backed_update_tool_unavailable`, and aborted with the page unchanged. It
// had spent six tool searches hunting for an `update_page_data_from_file`
// variant that does not exist. The payload would have been accepted: the same
// page already had 927 KB published, and the body limit is 2 MB. An undocumented
// limit does not make a caller cautious, it makes a caller guess — and a guess
// that costs a day of client-visible data is the expensive kind.
const MAX_INLINE_DATA_BYTES = envInt("PAGES_MCP_MAX_INLINE_DATA_BYTES", 1500000);

function envInt(name, fallback) {
  const value = Number(process.env[name]);
  return Number.isSafeInteger(value) && value > 0 ? value : fallback;
}

// Checks that apply to page HTML however it arrived. Staged bytes stop here:
// a 140 KB file is not a mistake on that path, it is the entire point of it.
function assertMcpHtml(html) {
  const trimmed = typeof html === "string" ? html.trim() : "";
  if (!trimmed) throw badRequest("html is required", "html_required");
  if (FILE_PLACEHOLDER_RE.test(trimmed)) {
    throw badRequest(
      "html is a literal file placeholder, not page content; use the staged page-upload tools for workspace files",
      "html_placeholder"
    );
  }
  return trimmed;
}

// The extra check for html passed INLINE as a tool argument, where every byte is
// output once and prompt on every later step.
function assertInlineHtml(html) {
  const trimmed = assertMcpHtml(html);
  const bytes = Buffer.byteLength(trimmed, "utf8");
  if (bytes > MAX_INLINE_HTML_BYTES) {
    throw badRequest(
      `html is ${bytes} bytes; inline deploys are capped at ${MAX_INLINE_HTML_BYTES}. ` +
        "Every inline byte is output you pay for once and prompt you pay for on every later step. " +
        "If this is another instance of a design Pages already stores, call list_templates and then " +
        "create_page_from_template with just its config (kilobytes). Otherwise write the file to disk and " +
        "use create_upload_ticket, which returns a URL your shell PUTs the file to — the bytes never pass " +
        "through your context at all.",
      "html_too_large_for_inline",
      { bytes, max_bytes: MAX_INLINE_HTML_BYTES }
    );
  }
  return trimmed;
}

function pageUploadCommitKey(args) {
  const options = {
    render_mode: args.render_mode || "themed",
    note: args.note || null,
    publish: args.publish === undefined ? true : args.publish,
    expected_version: args.expected_version || null,
    title: args.title || "",
    require_approval: !!args.require_approval,
    client_id: args.client_id || null,
  };
  return crypto.createHash("sha256").update(JSON.stringify(options)).digest("hex");
}

async function deployPage(args, ctx, requireExisting) {
  assertInlineHtml(args.html);
  const deployArgs = {
    slug: args.slug,
    html: args.html,
    renderMode: args.render_mode,
    note: args.note,
    source: "mcp",
    publish: args.publish === undefined ? true : args.publish,
    expectedVersion: args.expected_version,
  };
  const result = requireExisting
    ? { created: false, ...(await versions.deploy(deployArgs, ctx)) }
    : await versions.createAndDeploy(
        {
          ...deployArgs,
          title: args.title || "",
          clientId: args.client_id || null,
          requireApproval: !!args.require_approval,
        },
        ctx
      );
  const page = (await versions.getPage(args.slug)).page;
  return {
    created: result.created,
    ...deployGuidance(result, page, versions.normalizeSlug(args.slug)),
    // Passed through explicitly: this function shapes its output rather than
    // spreading the deploy result, so a new field is dropped unless named here.
    ...(result.config_schema_generated ? { config_schema_generated: true } : {}),
    preflight: preflight.analyze(args.html, { renderMode: args.render_mode || "themed" }),
  };
}

// registerTemplateUpload — the same verified-bytes seam deploy_page_upload uses,
// pointed at the template tables instead of a page. Verification, chunk
// accounting, ticket single-use, and commit idempotency are untouched.
async function registerTemplateUpload(args, ctx) {
  return pageUploads.deploy(
    args.upload_id,
    ctx,
    templateUploadCommitKey(args),
    async (client, staged) => {
      assertMcpHtml(staged.html);
      const result = await templates.registerWithClient(
        client,
        {
          name: staged.template,
          html: staged.html,
          title: args.title,
          description: args.description,
          note: args.note,
          source: "mcp",
        },
        ctx
      );
      return {
        upload_id: staged.uploadId,
        ...result,
        next_step: templateNextStep(result),
      };
    },
    { expectKind: "template" }
  );
}

function templateUploadCommitKey(args) {
  return crypto
    .createHash("sha256")
    .update(
      JSON.stringify({
        title: args.title || null,
        description: args.description || null,
        note: args.note || null,
      })
    )
    .digest("hex");
}

function templateNextStep(result) {
  const name = result.template.name;
  const revision = result.revision.revision;
  const problems = result.preflight && result.preflight.ok === false;
  if (problems) {
    return (
      `Registered ${name} revision ${revision}, but preflight found errors in the template itself — ` +
      `every page built from it inherits them. Fix the template and register again before creating pages.`
    );
  }
  if (result.deduped) {
    return `These exact bytes are already ${name} revision ${revision}; no new revision was created.`;
  }
  return (
    `${name} is at revision ${revision}. Call create_page_from_template with a complete config to build a page; ` +
    `call get_template to re-read the config and data schemas without pulling the HTML.`
  );
}

// A losing race here is uniquely cheap to recover from and uniquely easy to
// misread as expensive. pageUploads.deploy() writes commit_result and drops the
// chunks only after the callback returns, so a throw rolls the transaction back
// and leaves the staged bytes intact and re-deployable — but the agent sees only
// "stale_version" and re-does the whole job: re-read the page, re-derive the
// HTML, re-upload it. Say so in the error, or the retry costs a full turn
// instead of one call.
async function deployPageUpload(args, ctx) {
  try {
    return await deployPageUploadLocked(args, ctx);
  } catch (err) {
    if (err && err.code === "stale_version" && err.details) {
      err.details = { ...err.details, upload_id: args.upload_id, upload_still_staged: true };
      err.message +=
        ` Upload ${args.upload_id} is untouched and still deployable — retry deploy_page_upload with` +
        " the same upload_id; do not re-upload the file.";
    }
    throw err;
  }
}

async function deployPageUploadLocked(args, ctx) {
  return pageUploads.deploy(args.upload_id, ctx, pageUploadCommitKey(args), async (client, staged) => {
    assertMcpHtml(staged.html);
    const result = await versions.createAndDeployWithClient(
      client,
      {
        slug: staged.slug,
        html: staged.html,
        renderMode: args.render_mode,
        note: args.note,
        source: "mcp",
        publish: args.publish === undefined ? true : args.publish,
        expectedVersion: args.expected_version,
        title: args.title || "",
        clientId: args.client_id || null,
        requireApproval: !!args.require_approval,
      },
      ctx
    );
    const page = (
      await client.query(
        `SELECT slug, disabled, published_version_id
           FROM pages WHERE slug = $1 AND deleted_at IS NULL`,
        [staged.slug]
      )
    ).rows[0];
    return {
      upload_id: staged.uploadId,
      created: result.created,
      ...deployGuidance(result, page, staged.slug),
      preflight: preflight.analyze(staged.html, { renderMode: args.render_mode || "themed" }),
    };
  });
}

async function publishedResult(args, ctx, operation) {
  const slug = versions.normalizeSlug(args.slug);
  const version =
    operation === "publish"
      ? await versions.publish(
          { slug, versionId: args.version_id, expectedVersion: args.expected_version },
          ctx
        )
      : await versions.rollback(
          {
            slug,
            versionId: args.version_id === undefined ? null : args.version_id,
            expectedVersion: args.expected_version,
            note: args.note === undefined ? null : args.note,
          },
          ctx
        );
  const page = (await versions.getPage(slug)).page;
  const versionIsLive = !page.disabled && compareIds(version.id, page.published_version_id);
  return {
    version,
    live: versionIsLive,
    version_is_live: versionIsLive,
    page_is_live: !!page.published_version_id && !page.disabled,
    live_version_id: page.published_version_id || null,
    next_step: versionIsLive
      ? "Live now — share urls.live with the user or client."
      : "The live pointer changed again; reload get_page before sharing a URL.",
    urls: pageUrls(slug),
  };
}

// Patch the live document in place. The base HTML is read, edited and redeployed
// entirely server-side, so a one-line CSS fix costs anchors instead of a whole
// document in each direction.
async function patchPage(args, ctx) {
  const slug = versions.normalizeSlug(args.slug);
  const { page } = await versions.getPage(slug);
  if (!page.published_version_id && args.base_version_id === undefined) {
    throw badRequest(
      `page ${slug} has no published version to patch; deploy one first, or pass base_version_id`,
      "no_published_version"
    );
  }
  const baseId = args.base_version_id === undefined ? page.published_version_id : args.base_version_id;
  const base = await versions.getVersion(slug, baseId);

  const { html, applied } = pagePatch.applyEdits(base.html, args.edits);
  if (html === base.html) {
    throw badRequest(
      "the edits produced no change; check that find and replace actually differ",
      "patch_noop"
    );
  }
  const bytes = Buffer.byteLength(html);
  if (bytes > pageUploads.MAX_UPLOAD_BYTES) {
    throw badRequest(
      `patched document would be ${bytes} bytes, over the ${pageUploads.MAX_UPLOAD_BYTES} limit`,
      "upload_size_invalid"
    );
  }

  // Default the concurrency check to the POINTER we read, not the version we
  // patched. `expected_version` is compared against `published_version_id` (see
  // assertExpectedVersion), so defaulting it to `baseId` meant any explicit
  // `base_version_id` naming a version that is not the live one — a draft, an
  // older revision, exactly the cases the argument exists for — failed
  // `stale_version` every time, against a page nobody else had touched. On a
  // page with nothing published the pointer is null and the check does not
  // apply, which is right: there is no live version to lose.
  //
  // Be honest about what this default buys. patchPage re-reads the page a few
  // lines above, so the default can only catch a publish landing between that
  // read and the row lock — a real window, but a narrow one, and one no
  // black-box test can open deterministically. It is kept because it is free and
  // strictly better than passing nothing; it is not the reason a caller can
  // trust this tool. Pass an explicit `expected_version` from a value you read
  // in an EARLIER turn for that. The tests therefore pin the two things that are
  // observable — a rebase no longer 409s falsely, and an explicitly stale value
  // still does — rather than asserting a default they cannot distinguish.
  const result = await versions.deploy(
    {
      slug,
      html,
      renderMode: args.render_mode || base.render_mode,
      note: args.note,
      source: "mcp",
      publish: args.publish === undefined ? true : args.publish,
      expectedVersion: args.expected_version === undefined ? page.published_version_id : args.expected_version,
    },
    ctx
  );
  const after = (await versions.getPage(slug)).page;
  return {
    ...deployGuidance(result, after, slug),
    patch: {
      base_version_id: String(baseId),
      edits_applied: applied,
      bytes_before: Buffer.byteLength(base.html),
      bytes_after: bytes,
    },
    preflight: preflight.analyze(html, { renderMode: args.render_mode || base.render_mode }),
  };
}

async function findInVersion(args) {
  const slug = versions.normalizeSlug(args.slug);
  let versionId = args.version_id;
  if (versionId === undefined) {
    const { page } = await versions.getPage(slug);
    if (!page.published_version_id) {
      throw badRequest(`page ${slug} has no published version; pass version_id`, "no_published_version");
    }
    versionId = page.published_version_id;
  }
  const version = await versions.getVersion(slug, versionId);
  return {
    version_id: version.id,
    ...pagePatch.findMatches(version.html, args.query, {
      maxMatches: args.max_matches,
      ignoreCase: !!args.ignore_case,
    }),
  };
}

// Preflight a stored version. Note what this deliberately does NOT do: return
// the HTML. Reading a 65 KB dashboard back just to look for a broken button was
// costing agents more context than authoring it did, so the bytes stay here and
// only the findings travel.
async function preflightPage(args) {
  const slug = versions.normalizeSlug(args.slug);
  let versionId = args.version_id;
  if (versionId === undefined) {
    const { page } = await versions.getPage(slug);
    if (!page.published_version_id) {
      throw badRequest(
        `page ${slug} has no published version; pass version_id to preflight a draft`,
        "no_published_version"
      );
    }
    versionId = page.published_version_id;
  }
  const version = await versions.getVersion(slug, versionId);
  return {
    version_id: version.id,
    preflight: preflight.analyze(version.html, { renderMode: version.render_mode || "themed" }),
  };
}

// publishedLiveState — for reads that return the PUBLISHED version: it is live
// exactly when the page is (published pointer set, page not disabled).
function publishedLiveState(page) {
  const pageIsLive = !!page.published_version_id && !page.disabled;
  return {
    version_is_live: pageIsLive,
    page_is_live: pageIsLive,
    live_version_id: page.published_version_id || null,
  };
}

async function getPageData(args) {
  const result = await versions.getPageData(args.slug);
  return {
    page: result.page,
    version: result.version,
    schema: result.schema,
    envelope: result.envelope,
    data_sha256: result.data_sha256,
    schema_sha256: result.schema_sha256,
    template_sha256: result.template_sha256,
    data_profile: result.data_profile,
    freshness: result.freshness,
    ...publishedLiveState(result.page),
    urls: pageUrls(result.page.slug),
  };
}

// A stale refresh and a narrowed payload need DIFFERENT sentences. "Reconcile
// against the source" is right when figures may be wrong; it is the wrong advice
// when the figures are simply the ones already published, because the mistake
// there is not a bad number, it is ANNOUNCING an update that did not happen. A
// real refresh went out as "Version 205 published and live" with unchanged Aug
// 3-Aug 5 coverage and "Data Warnings: None", and the recipient asked why
// yesterday's data was missing. Lead with whichever applies.
function dataUpdateNextStep(nextStep, warnings) {
  if (!warnings.length) return nextStep;
  const isStale = (w) => w.code === "data_unchanged" || w.code === "coverage_did_not_advance";
  const stale = warnings.find(isStale);
  const codes = warnings.map((w) => w.code).join(", ");
  // "You added nothing" is only the headline when there is nothing WORSE to say.
  // A payload that also lost rows, coverage or a dimension value has a real
  // correctness problem, and telling the caller to stop announcing an update
  // would bury it.
  if (stale && warnings.every(isStale)) {
    return (
      `${nextStep} THIS REFRESH ADDED NO NEW DATA (${codes}). ${stale.message} ` +
      `Do not describe this as a data update: say which coverage is live, which day was expected, and that the ` +
      `source did not contain it.`
    );
  }
  return (
    `${nextStep} RECONCILE BEFORE SHARING: ${warnings.length} warning(s) about what this payload contains ` +
    `(${codes}). Read data_warnings and data_profile and check them against the source before telling anyone ` +
    `this page is correct.`
  );
}

// assertInlineData enforces the stated managed-data ceiling. A payload under it
// is sent whole: splitting, sampling or summarizing to "be safe" produces a
// dashboard that is wrong in a way the schema cannot catch, which is strictly
// worse than a refusal.
function assertInlineData(data) {
  if (!data || typeof data !== "object" || Array.isArray(data)) {
    throw badRequest("data must be a JSON object", "data_invalid");
  }
  const bytes = Buffer.byteLength(JSON.stringify(data), "utf8");
  if (bytes > MAX_INLINE_DATA_BYTES) {
    throw badRequest(
      `data is ${bytes} bytes; INLINE managed-data updates are capped at ${MAX_INLINE_DATA_BYTES}. ` +
        "Stage it instead: create_upload_ticket with kind 'data', PUT the JSON file from your shell, then " +
        "update_page_data_upload — the bytes never pass through your context and there is no cap to hit. " +
        "Do NOT split, sample or summarize the payload to fit — a partial payload satisfies the schema " +
        "and silently publishes wrong numbers.",
      "data_too_large_for_inline",
      { bytes, max_bytes: MAX_INLINE_DATA_BYTES }
    );
  }
  return bytes;
}

// dataUpdateResult — the response shape both managed-data writes return. Kept
// as one function so a caller cannot tell the staged route from the inline one
// by what comes back, and so next_step's stale-refresh warning surfacing does
// not have to be reimplemented for the second path.
function dataUpdateResult(result, page, args) {
  const versionIsLive = !page.disabled && compareIds(result.version.id, page.published_version_id);
  const pageIsLive = !!page.published_version_id && !page.disabled;
  let nextStep;
  if (versionIsLive) {
    nextStep = result.deduped
      ? "The exact data and source coverage were already live; no new immutable version was created."
      : "The structured-data version is live now. Layout and schema bytes were preserved.";
  } else if (page.disabled) {
    nextStep = "The data version was saved, but the page is disabled and is not serving. A human admin must re-enable it.";
  } else if (result.gated) {
    nextStep = pageIsLive
      ? `Approval is required. Version ${result.version.id} is pending; the previous live version ${page.published_version_id} remains live.`
      : `Approval is required. Version ${result.version.id} is pending and no page version is currently live.`;
  } else if (args.publish === false) {
    nextStep = `Draft canary ${result.version.id} was saved without moving the live pointer. Inspect it in urls.admin before a publishing call.`;
  } else {
    nextStep = "The pointer changed concurrently after this update; call get_page_data before retrying.";
  }
  return {
    version: result.version,
    envelope: result.envelope,
    data_sha256: result.data_sha256,
    schema_sha256: result.schema_sha256,
    template_sha256: result.template_sha256,
    deduped: result.deduped,
    published: result.published,
    gated: result.gated,
    version_is_live: versionIsLive,
    page_is_live: pageIsLive,
    live_version_id: page.published_version_id || null,
    // A warning nobody reads is not a safeguard. next_step is the one field an
    // agent reliably acts on, so put the count and the codes there rather than
    // hoping data_warnings gets inspected.
    next_step: dataUpdateNextStep(nextStep, result.data_warnings || []),
    urls: pageUrls(page.slug),
    data_profile: result.data_profile,
    data_warnings: result.data_warnings || [],
  };
}

// updatePageDataUpload — the staged counterpart of updatePageData. The payload
// arrived through a ticket, so it never entered the model's context and there is
// no inline ceiling to hit; from the parse onward it is byte-for-byte the same
// write, through versions.updatePageDataWithClient, inside the upload's own
// transaction so a spent upload can never be left with no version behind it.
async function updatePageDataUpload(args, ctx) {
  return pageUploads.deploy(
    args.upload_id,
    ctx,
    dataUploadCommitKey(args),
    async (client, staged) => {
      if (staged.slug !== versions.normalizeSlug(args.slug)) {
        throw conflict(
          `this upload was staged for ${staged.slug}, not ${args.slug}; the target is fixed when the upload starts`,
          "page_upload_target_mismatch",
          { slug: staged.slug }
        );
      }
      // readVerified already proved the bytes are complete, hash-matched and
      // valid UTF-8. What it cannot know is that they are a JSON OBJECT, which
      // is the contract update_page_data has always enforced on its argument.
      let data;
      try {
        data = JSON.parse(staged.html);
      } catch (err) {
        throw badRequest(
          `staged upload is not valid JSON: ${err.message}. Stage the exact file your generator wrote; do not hand-edit it.`,
          "data_upload_not_json"
        );
      }
      if (!data || typeof data !== "object" || Array.isArray(data)) {
        throw badRequest("staged managed data must be a JSON object", "data_invalid");
      }
      const result = await versions.updatePageDataWithClient(
        client,
        {
          slug: staged.slug,
          data,
          sourceAsOf: args.source_as_of,
          expectedVersion: args.expected_version,
          publish: args.publish === undefined ? true : args.publish,
          note: args.note,
          expect: args.expect === undefined ? null : args.expect,
        },
        ctx
      );
      const page = (
        await client.query(
          `SELECT slug, disabled, published_version_id
             FROM pages WHERE slug = $1 AND deleted_at IS NULL`,
          [staged.slug]
        )
      ).rows[0];
      return { upload_id: staged.uploadId, ...dataUpdateResult(result, page, args) };
    },
    { expectKind: "data" }
  );
}

// The commit key is what makes an exact retry idempotent and a DIFFERENT retry
// a conflict rather than a silent second publish. Every field that changes what
// gets written belongs in it.
function dataUploadCommitKey(args) {
  return crypto
    .createHash("sha256")
    .update(
      JSON.stringify({
        slug: args.slug,
        source_as_of: args.source_as_of,
        expected_version: String(args.expected_version),
        publish: args.publish === undefined ? true : args.publish,
        note: args.note || null,
        expect: args.expect === undefined ? null : args.expect,
      })
    )
    .digest("hex");
}

async function updatePageData(args, ctx) {
  assertInlineData(args.data);
  const result = await versions.updatePageData(
    {
      slug: args.slug,
      data: args.data,
      sourceAsOf: args.source_as_of,
      expectedVersion: args.expected_version,
      publish: args.publish === undefined ? true : args.publish,
      note: args.note,
      expect: args.expect === undefined ? null : args.expect,
    },
    ctx
  );
  const page = (await versions.getPage(args.slug)).page;
  return dataUpdateResult(result, page, args);
}

async function listWorkspaces(args) {
  const query = (args.query || "").trim().toLowerCase();
  const filters = { query: query || null };
  const after = decodeCursor(args.cursor, "workspaces", filters);
  if (after && (typeof after.name !== "string" || !/^[1-9][0-9]*$/.test(String(after.id)))) {
    throw badRequest("invalid pagination cursor", "invalid_cursor");
  }
  let rows = await workspaces.list();
  if (query) rows = rows.filter((row) => row.name.toLowerCase().includes(query));
  if (after) {
    // Keep PostgreSQL's exact collation/order authoritative. Re-sorting or
    // tuple-comparing in JavaScript can disagree for Unicode names; locate the
    // cursor row in the already ordered result and continue after it.
    const index = rows.findIndex(
      (row) => row.name.toLowerCase() === after.name && String(row.id) === String(after.id)
    );
    if (index < 0) throw badRequest("invalid or stale pagination cursor", "invalid_cursor");
    rows = rows.slice(index + 1);
  }
  const hasMore = rows.length > args.limit;
  const pageRows = rows.slice(0, args.limit);
  const last = pageRows[pageRows.length - 1];
  return {
    workspaces: pageRows,
    next_cursor:
      hasMore && last
        ? encodeCursor("workspaces", { name: last.name.toLowerCase(), id: String(last.id) }, filters)
        : null,
  };
}

// Preflight findings. Additive and optional on every deploy result: an agent
// that ignores the field behaves exactly as before, and one that reads it
// learns its dashboard is broken BEFORE the client does.
const PreflightFindingSchema = z
  .object({
    code: z.string(),
    message: z.string(),
    fix: z.string().optional(),
    element: z.string().optional(),
    attribute: z.string().optional(),
    identifier: z.string().optional(),
    snippet: z.string().optional(),
    host: z.string().optional(),
    directive: z.string().optional(),
    api: z.string().optional(),
    apis: z.array(z.string()).optional(),
    required_token: z.string().optional(),
    script_index: z.number().int().optional(),
    contract_code: z.string().optional().describe("The managed-data contract error code, on managed_block_invalid."),
  })
  .strict();

const PreflightSchema = z
  .object({
    ok: z.boolean().describe("True when no errors were found. Warnings do not clear it."),
    render_mode: z.string(),
    errors: z.array(PreflightFindingSchema),
    warnings: z.array(PreflightFindingSchema),
    errors_omitted: z.number().int().nonnegative(),
    warnings_omitted: z.number().int().nonnegative(),
    checks: z.array(z.string()),
    summary: z.string(),
  })
  .strict();

const DeployOutputSchema = z
  .object({
    version: VersionSchema,
    deduped: z.boolean(),
    published: z.boolean(),
    gated: z.boolean(),
    live: z.boolean().describe("Compatibility alias for version_is_live."),
    version_is_live: z.boolean(),
    page_is_live: z.boolean(),
    live_version_id: IdOut.nullable(),
    next_step: z.string(),
    urls: UrlsSchema,
    config_schema_generated: z.boolean().optional().describe(
      "Present only when this page shipped a #pages-config block with no #pages-config-schema, so Pages derived one from the values and wrote it into the stored document. It pins types and rejects unknown keys; it does not know your enums, formats or which keys are genuinely optional. Replace it by hand if this design becomes a family."
    ),
    preflight: PreflightSchema.optional().describe(
      "Static check of the deployed document against the content host's CSP and sandbox. Advisory only — the deploy already happened. Errors mean parts of the page will not work in a browser."
    ),
  })
  .strict();

const UploadId = z.string().uuid().describe("Opaque upload handle returned by start_page_upload.");
// What a caller believes its payload contains, computed from the SOURCE in the
// same pass that built it. Shared by both managed-data writes: the inline and
// staged routes must not be able to drift apart on what may be asserted.
const DataExpectSchema = z
  .object({
    row_count: z.record(z.string(), z.number().int().nonnegative()).optional().describe('Keyed by array path, e.g. {"rows": 124}.'),
    totals: z.record(z.string(), z.number()).optional().describe('Keyed by field path, e.g. {"rows.spend": 24541.6}.'),
    date_range: z
      .record(z.string(), z.tuple([z.string(), z.string()]))
      .optional()
      .describe('Keyed by field path, e.g. {"rows.date": ["2026-07-06", "2026-08-05"]}. Exact match.'),
    tolerance: z
      .number()
      .min(0)
      .max(1)
      .optional()
      .describe("Relative slack on totals only. Defaults to 0.0001 (0.01%), enough for float-order drift."),
  })
  .strict()
  .describe(
    "What you believe the payload contains, computed from the SOURCE in the same pass that built it. Verified against the server's own profile BEFORE anything is written; a mismatch fails with data_reconciliation_failed and changes nothing. Paths are the paths data_profile reports."
  );

const UploadStateSchema = z
  .object({
    upload_id: UploadId,
    // An upload targets a page's HTML, a template, or a page's managed DATA.
    // 'page' and 'data' both name a slug; 'template' names a template. The
    // field that does not apply is null. See lib/page-uploads.js state().
    target_kind: z.enum(["page", "template", "data"]),
    slug: z.string().nullable(),
    template: z.string().nullable(),
    content_sha256: Sha256,
    total_bytes: z.number().int().positive(),
    bytes_received: z.number().int().nonnegative(),
    next_sequence: z.number().int().nonnegative(),
    max_chunk_bytes: z.number().int().positive(),
    complete: z.boolean(),
    expires_at: DateTime,
    next_step: z.string(),
  })
  .strict();

const PublishOutputSchema = z
  .object({
    version: VersionSchema,
    live: z.boolean().describe("Compatibility alias for version_is_live."),
    version_is_live: z.boolean(),
    page_is_live: z.boolean(),
    live_version_id: IdOut.nullable(),
    next_step: z.string(),
    urls: UrlsSchema,
  })
  .strict();

const DeployInputShape = {
  slug: Slug,
  // The HTTP boundary enforces MAX_HTML_BYTES on the complete JSON body. Do
  // not advertise a character limit here: UTF-8 width and JSON overhead make
  // a character count a different, misleading contract.
  html: z
    .string()
    .min(1)
    .describe(
      "Inline page HTML or fragment. For a workspace file or content over 20,000 UTF-8 bytes, do not inline it: use start_page_upload, append_page_upload, then deploy_page_upload."
    ),
  render_mode: z.enum(["themed", "raw"]).optional().describe("Defaults to themed."),
  note: z.string().max(500).optional(),
  publish: z.boolean().optional().describe("Publish immediately on open pages; defaults to true."),
  expected_version: IdArg.optional().describe("Optimistic-concurrency check against the current published version."),
};

const UploadDeployInputShape = {
  upload_id: UploadId,
  render_mode: z.enum(["themed", "raw"]).optional().describe("Defaults to themed."),
  note: z.string().max(500).optional(),
  publish: z.boolean().optional().describe("Publish immediately on open pages; defaults to true."),
  expected_version: IdArg.optional().describe("Optimistic-concurrency check against the current published version."),
  title: z.string().max(200).optional().describe(
        "Used only if the page is created. PLAIN TEXT \u2014 Pages escapes it wherever it renders, so pass \"Contoso & Allergex\", never \"Contoso &amp; Allergex\"; a pre-escaped title shows its own entities."
      ),
  require_approval: z.boolean().optional().describe("Used only if the page is created."),
  client_id: z.string().max(200).optional().describe("Used only if the page is created."),
};

// One source binding: which connector serves an input the request names. The
// vocabulary matches the `workflow.sources` shape client bundles already author,
// so a caller does not have to learn a second one. Credential-free by
// construction (names only) and re-screened by updatePrompts.assertCredentialFree.
const SourceBindingSchema = z
  .object({
    source_id: z.string().min(1).max(120).describe("Stable identifier for this input, e.g. ix_ssp or amazon_dsp."),
    mcp_server: z.string().min(1).max(120).describe("MCP server name that serves it, e.g. indexexchange_mcp."),
    account: z.string().min(1).max(120).optional().describe("Logical connector account/variant name; omit for the default seat."),
    required_tools: z
      .array(z.string().min(1).max(200))
      .min(1)
      .max(40)
      .optional()
      .describe("Read-only tool names this source must be retrieved through."),
    path: z
      .string()
      .min(1)
      .max(500)
      .optional()
      .describe("Exact location in the source system, e.g. NWM_keel/Meridian/daily. Beats searching for a filename."),
    partition: z
      .object({
        by: z.literal("date").describe("Only date partitioning is modelled today."),
        format: z.string().min(1).max(40).optional().describe("Partition folder/file format, e.g. YYYY-MM-DD."),
        since: z.string().min(1).max(40).optional().describe("Inclusive lower bound, or source_as_of to continue from the page."),
        until: z.string().min(1).max(40).optional().describe("Inclusive upper bound; omit for the newest available."),
      })
      .strict()
      .optional()
      .describe(
        "Set when this source is a date-partitioned folder (one file per day). The prompt then requires enumerating EVERY partition in range — the default newest-wins pick silently publishes a single day of a multi-week dataset."
      ),
    retrieval_instructions: z
      .string()
      .min(1)
      .max(2000)
      .optional()
      .describe("Deterministic, bounded retrieval detail for this source (window, filters, expected artifacts)."),
  })
  .strict();

const PrepareDashboardUpdateInputSchema = z
  .object({
    slug: Slug,
    instructions: z
      .string()
      .min(1)
      .max(updatePrompts.MAX_INSTRUCTIONS)
      .describe("The user's exact data/source/design request in plain language; describe credential locations, never secret values."),
    recurring: z
      .boolean()
      .optional()
      .describe("False/default: execute once now. True: return reusable text for a user-owned scheduler; Pages does not schedule it."),
    update_type: z
      .enum(["auto", "data", "layout"])
      .optional()
      .describe("Choose data for values only, layout for design/schema/JavaScript, or auto when the request genuinely needs classification."),
    publish: z.boolean().optional().describe("Whether the eventual update should publish; defaults true."),
    sources: z
      .array(SourceBindingSchema)
      .min(1)
      .max(updatePrompts.MAX_SOURCES)
      .optional()
      .describe(
        "Exact source bindings: which connector serves each input, where the data sits, and whether it is date-partitioned. REQUIRED when recurring is true — an unattended run executed weeks later cannot safely re-derive bindings from prose. Optional for a one-time update, but still the difference between a pinned source and a guess. Names only, never secret values."
      ),
  })
  .strict();

const PrepareDashboardUpdateOutputSchema = z
  .object({
    page: PageSchema,
    mode: z.enum(["adaptive", "managed_data", "managed_template", "full_page", "migration_required"]),
    recurring: z.boolean(),
    prompt: z.string(),
    prompt_sha256: Sha256,
    schema_sha256: Sha256.nullable(),
    sources: z.array(SourceBindingSchema).nullable(),
    // What a run of this prompt needs, in a shape a scheduler can validate
    // before accepting the task rather than discovering at dispatch.
    execution_requirements: z
      .object({
        mcp_servers: z.array(z.string()),
        required_tools: z.array(z.string()),
        network: z.boolean(),
        model_required: z.boolean(),
        mode: z.string(),
      })
      .strict(),
    live_version_id: IdOut,
    page_is_live: z.boolean(),
    next_step: z.string(),
  })
  .strict();

// opts carries server-side switches only. They are a SECOND parameter, never
// read off args, so a tool caller cannot set one — `allowUnboundRecurring`
// relaxes a safety gate, and the only caller entitled to it is the legacy
// compatibility alias below.
function prepareDashboardUpdate(args, opts = {}) {
  return updatePrompts.prepare({
    slug: args.slug,
    instructions: args.instructions,
    recurring: args.recurring === true,
    updateType: args.update_type || "auto",
    publish: args.publish === undefined ? true : args.publish,
    sources: args.sources === undefined ? null : args.sources,
    allowUnboundRecurring: opts.allowUnboundRecurring === true,
  });
}

const ConfigurePageRefreshCompatibilityInputSchema = z
  .object({
    slug: Slug,
    instructions: z
      .string()
      .min(1)
      .max(updatePrompts.MAX_INSTRUCTIONS)
      .optional()
      .describe("Preferred: the user's exact plain-language request."),
    recurring: z.boolean().optional(),
    update_type: z.enum(["auto", "data", "layout"]).optional(),
    publish: z.boolean().optional(),
    daily_at_utc: z
      .string()
      .regex(/^(?:[01][0-9]|2[0-3]):[0-5][0-9]$/)
      .optional()
      .describe("Legacy compatibility only; Pages does not install this cadence."),
    workflow: JsonObjectSchema.optional().describe("Legacy credential-free source workflow converted into user-owned prompt text."),
    run_now: z.boolean().optional().describe("Legacy compatibility only; ignored because prompt preparation never executes work."),
  })
  .strict();

async function prepareDashboardUpdateCompatibility(args) {
  if (args.instructions) return prepareDashboardUpdate(args);
  if (!args.workflow) {
    throw badRequest(
      "instructions are required; older clients may instead supply their credential-free workflow",
      "update_instructions_required"
    );
  }
  updatePrompts.assertCredentialFree(args.workflow);
  const serialized = JSON.stringify(args.workflow);
  const cadence = args.daily_at_utc
    ? `The user intends to invoke this single-run workflow daily at ${args.daily_at_utc} UTC in their own scheduler. `
    : "The user intends to invoke this single-run workflow from their own scheduler. ";
  const prepared = await prepareDashboardUpdate({
    slug: args.slug,
    instructions: `${cadence}Use this credential-free source, freshness, reconciliation, and mapping contract exactly: ${serialized}`,
    recurring: true,
    update_type: "data",
    publish: args.publish,
    // The legacy workflow already names a connector per source; lift those into
    // real bindings so an older caller gets the same no-substitutions gate.
    sources: updatePrompts.sourcesFromWorkflow(args.workflow),
  },
  {
    // A legacy workflow whose entries omit mcp_server yields no bindings, and
    // that client has no way to supply them — this alias exists precisely so it
    // keeps working. Exempt it from the recurring-bindings gate rather than
    // turning a compatibility read into a hard failure; its source detail still
    // travels inside the serialized workflow contract above.
    allowUnboundRecurring: true,
  });
  return {
    ...prepared,
    next_step:
      "Pages did not install the legacy cadence and did not honor run_now. Show prompt to the user verbatim for their scheduler; do not claim this update was configured or executed.",
  };
}

const TOOLS = {
  list_pages: {
    title: "List Pages",
    description:
      "List active Pages with workspace, theme, client-access, serving state, and routing URLs. Supports bounded keyset pagination and filters; workspace_id:null selects Ungrouped.\n\nEach row carries freshness: source_as_of, refreshed_at, checked_at, and days_since_* for each. That is how you find dashboards that have quietly stopped updating — Pages states no opinion on what counts as overdue, because it does not know any page's expected cadence.",
    inputSchema: z
      .object({
        query: z.string().min(1).max(200).optional(),
        workspace_id: NullableIdArg.optional(),
        client_id: z.string().nullable().optional(),
        is_live: z.boolean().optional(),
        require_approval: z.boolean().optional(),
        disabled: z.boolean().optional(),
        limit: PageLimit,
        cursor: Cursor.optional(),
      })
      .strict(),
    outputSchema: z
      .object({ pages: z.array(PageSummarySchema), next_cursor: z.string().nullable() })
      .strict(),
    annotations: ReadAnnotations,
    handler: listPages,
  },

  get_page: {
    title: "Get Page",
    description:
      "Get page metadata, workspace/theme, routing URLs, serving state, and published-version metadata. HTML is omitted unless include_html is true.",
    inputSchema: z.object({ slug: Slug, include_html: z.boolean().optional() }).strict(),
    outputSchema: z
      .object({
        page: PageSchema,
        published: z.union([VersionSchema, VersionWithHtmlSchema]).nullable(),
        is_live: z.boolean(),
        urls: UrlsSchema,
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: async (args) => {
      const result = await versions.getPage(args.slug);
      let published = result.published;
      if (published && args.include_html !== true) {
        const { html: _html, ...metadata } = published;
        published = metadata;
      }
      return {
        page: result.page,
        published,
        is_live: !!result.page.published_version_id && !result.page.disabled,
        urls: pageUrls(result.page.slug),
      };
    },
  },

  get_page_data: {
    title: "Get Managed Page Data",
    description:
      "Read the currently published managed-data contract: page/live-version metadata, embedded self-contained JSON Schema, current envelope, deterministic hashes, exact live state, and URLs. Use its live_version_id as update_page_data.expected_version. Fails with page_not_data_managed unless the published HTML has exactly one pages-data-schema block and one pages-data block.\n\nfreshness.source_as_of is the coverage watermark to compare your source against before refreshing: refresh when the source holds a period this page does not already represent, not because a file's timestamp moved.",
    inputSchema: z.object({ slug: Slug }).strict(),
    outputSchema: z
      .object({
        page: PageSchema,
        schema: JsonObjectSchema,
        ...PageDataStateShape,
        data_profile: DataProfileSchema,
        freshness: FreshnessSchema,
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: getPageData,
  },

  // Compatibility guidance for the static Pages allowlists already deployed
  // in Chat and Cutlass. It intentionally exposes no historical schedule or
  // dispatch state; current clients can discover the Pages-only handoff without
  // either repository changing in lockstep.
  get_page_refresh: {
    title: "Get Dashboard Scheduling Guidance",
    description:
      "Compatibility read for clients that still ask about a Pages refresh. Pages no longer stores or dispatches recurring schedules. Read the exact existing page, then call prepare_dashboard_update—or configure_page_refresh on an older static allowlist—with the user's instructions and recurring=true. Show that returned prompt to the user for their own scheduler.",
    inputSchema: z.object({ slug: Slug }).strict(),
    outputSchema: z
      .object({
        page: PageSchema,
        scheduling: z.literal("user_owned"),
        refresh: z.null(),
        runs: z.array(z.never()).max(0),
        next_step: z.string(),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: async (args) => ({
      page: (await versions.getPage(args.slug)).page,
      scheduling: "user_owned",
      refresh: null,
      runs: [],
      next_step:
        "Ask for the complete update/source instructions, then call prepare_dashboard_update. If that tool is hidden by an older static client allowlist, call configure_page_refresh with the same new arguments. Show a recurring prompt to the user; Pages has no schedule to inspect.",
    }),
  },

  // The counterpart to freshness: a refresh that ran correctly and published
  // nothing writes nothing anywhere, so "we check daily and the upstream is
  // dead" reads exactly like "nobody has run this in three weeks".
  record_refresh_check: {
    title: "Record Refresh Check",
    description:
      "Record that a refresh looked at this page and what it concluded, WITHOUT creating a version. Call it when a run ends without publishing — the source had no new coverage, a required tool was unavailable, a gate stopped it. It moves freshness.checked_at only: the published pointer, the data, and every hash are untouched, so the page keeps serving exactly what it was serving.\n\nThis is what separates a dashboard whose upstream has stopped producing from one nobody is running any more. Those look identical otherwise and need different people to act. A run that DID publish does not need this — update_page_data already stamps the page.",
    inputSchema: z
      .object({
        slug: Slug,
        outcome: RefreshCheckOutcome,
        detail: z
          .string()
          .max(500)
          .optional()
          .describe("One line a human can act on, e.g. 'upstream max date still 2026-07-02, unchanged for 6 days'."),
        source_as_of_seen: DateTime.optional().describe(
          "The latest coverage the SOURCE offered, when you could determine it. Compared against the page's own source_as_of, this is what shows an upstream has frozen rather than the job."
        ),
      })
      .strict(),
    outputSchema: z.object({ slug: z.string(), freshness: FreshnessSchema }).strict(),
    // Idempotent: recording the same conclusion twice leaves the same state.
    annotations: { readOnlyHint: false, destructiveHint: false, idempotentHint: true, openWorldHint: false },
    handler: async (args, ctx) => (
      await versions.recordRefreshCheck(
        {
          slug: args.slug,
          outcome: args.outcome,
          detail: args.detail === undefined ? null : args.detail,
          sourceAsOfSeen: args.source_as_of_seen === undefined ? null : args.source_as_of_seen,
        },
        ctx
      )
    ),
  },

  prepare_dashboard_update: {
    title: "Prepare Dashboard Update",
    description:
      "Prepare a safe exact-slug update prompt from a user's plain-language request. Call this whenever a user says 'update <slug> dashboard with ...'. For a one-time request, follow the returned prompt now. With recurring=true, show the returned prompt to the user for their own scheduler; Pages never schedules or dispatches it. update_type=data uses the managed-data contract, layout uses an existing-page staged deployment, and auto supplies bounded routing guidance. This tool is read-only and never changes a page.",
    inputSchema: PrepareDashboardUpdateInputSchema,
    outputSchema: PrepareDashboardUpdateOutputSchema,
    annotations: ReadAnnotations,
    handler: prepareDashboardUpdate,
  },

  find_in_version: {
    title: "Find In Page Version",
    description:
      "Literal (non-regex) search inside a stored version, returning bounded line numbers and short excerpts instead of the document. Use this to locate the exact anchor text for patch_page without reading a whole dashboard back into context. Defaults to the published version. Read-only.",
    inputSchema: z
      .object({
        slug: Slug,
        query: z.string().min(1).max(pagePatch.MAX_FIND_CHARS).describe("Exact substring to look for; not a regular expression."),
        version_id: IdArg.optional().describe("Defaults to the currently published version."),
        max_matches: z.number().int().min(1).max(pagePatch.MAX_MATCHES).optional(),
        ignore_case: z.boolean().optional(),
      })
      .strict(),
    outputSchema: z
      .object({
        version_id: IdOut,
        total_matches: z.number().int().nonnegative(),
        matches_omitted: z.number().int().nonnegative(),
        matches: z.array(
          z.object({ offset: z.number().int().nonnegative(), line: z.number().int().positive(), excerpt: z.string() }).strict()
        ),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: findInVersion,
  },

  patch_page: {
    title: "Patch Page",
    description:
      "Deploy a new version by applying small anchored find/replace edits to the CURRENT published HTML, server-side. Use this for targeted changes — a CSS rule, a handler name, a label — instead of re-uploading the whole document. `find` is a literal string, not a regex, and must match exactly `count` times (default 1) or the whole patch is rejected. Locate anchors with find_in_version. By default it patches the live version and is concurrency-checked against the live pointer, so a patch cannot clobber a deploy that landed between the read and the write. `base_version_id` patches a DIFFERENT version instead — and publishing that republishes its bytes, discarding whatever was deployed since, so pass `expected_version` (or `publish:false`) when you rebase off an older version deliberately.",
    inputSchema: z
      .object({
        slug: Slug,
        edits: z
          .array(
            z
              .object({
                find: z.string().min(1).max(pagePatch.MAX_FIND_CHARS).describe("Exact literal text to replace, whitespace included."),
                replace: z.string().max(pagePatch.MAX_REPLACE_CHARS),
                count: z.number().int().min(1).max(100).optional().describe("Required number of occurrences; defaults to 1."),
              })
              .strict()
          )
          .min(1)
          .max(pagePatch.MAX_EDITS)
          .describe("Applied in order; each sees the result of the previous."),
        base_version_id: IdArg.optional().describe("Patch this version instead of the published one."),
        render_mode: z.enum(["themed", "raw"]).optional().describe("Defaults to the base version's mode."),
        note: z.string().max(500).optional(),
        publish: z.boolean().optional().describe("Publish immediately on open pages; defaults to true."),
        expected_version: IdArg
          .optional()
          .describe("Defaults to the page's current live version, not to base_version_id."),
      })
      .strict(),
    outputSchema: DeployOutputSchema.extend({
      patch: z
        .object({
          base_version_id: IdOut,
          bytes_before: z.number().int().nonnegative(),
          bytes_after: z.number().int().nonnegative(),
          edits_applied: z.array(
            z
              .object({
                index: z.number().int().nonnegative(),
                count: z.number().int().positive(),
                first_line: z.number().int().positive(),
                bytes_delta: z.number().int(),
              })
              .strict()
          ),
        })
        .strict(),
    }).strict(),
    annotations: WriteAnnotations,
    handler: patchPage,
  },

  preflight_page: {
    title: "Preflight Page",
    description:
      "Check a stored version against the exact CSP and sandbox the content host serves it under, WITHOUT pulling the HTML into context. Reports controls that will silently do nothing in a browser: inline on*= handlers shadowed by a built-in DOM member, subresources the CSP blocks, APIs the sandbox ignores, and scripts that do not parse. Defaults to the published version. Read-only.",
    inputSchema: z
      .object({
        slug: Slug,
        version_id: IdArg.optional().describe("Defaults to the currently published version."),
      })
      .strict(),
    outputSchema: z.object({ version_id: IdOut, preflight: PreflightSchema }).strict(),
    annotations: ReadAnnotations,
    handler: preflightPage,
  },

  get_version: {
    title: "Get Page Version",
    description: "Get one version's full metadata and HTML source. Use this for drafts, pending reviews, rollback targets, and source edits.",
    inputSchema: z.object({ slug: Slug, version_id: IdArg }).strict(),
    outputSchema: z.object({ version: VersionWithHtmlSchema }).strict(),
    annotations: ReadAnnotations,
    handler: async (args) => ({ version: await versions.getVersion(args.slug, args.version_id) }),
  },

  list_versions: {
    title: "List Page Versions",
    description:
      "List bounded version history newest-first. is_published marks pointer equality; is_live additionally requires the page to be enabled.",
    inputSchema: z
      .object({
        slug: Slug,
        status: z.enum(["draft", "pending", "approved", "rejected"]).optional(),
        limit: PageLimit,
        cursor: Cursor.optional(),
      })
      .strict(),
    outputSchema: z
      .object({ versions: z.array(VersionListSchema), next_cursor: z.string().nullable() })
      .strict(),
    annotations: ReadAnnotations,
    handler: listVersions,
  },

  list_workspaces: {
    title: "List Workspaces",
    description: "List one-level workspaces with active-page counts. Use returned IDs with set_page_workspace or list_pages filtering.",
    inputSchema: z
      .object({ query: z.string().min(1).max(100).optional(), limit: PageLimit, cursor: Cursor.optional() })
      .strict(),
    outputSchema: z
      .object({ workspaces: z.array(WorkspaceSchema), next_cursor: z.string().nullable() })
      .strict(),
    annotations: ReadAnnotations,
    handler: listWorkspaces,
  },

  create_workspace: {
    title: "Create Workspace",
    description:
      "Create global, reversible organization metadata. Workspace names are case-insensitively unique; live content and URLs are unchanged.",
    inputSchema: z.object({ name: z.string().min(1).max(100) }).strict(),
    outputSchema: z.object({ workspace: WorkspaceSchema }).strict(),
    annotations: AdditiveAnnotations,
    handler: async (args, ctx) => ({ workspace: await workspaces.create({ name: args.name }, ctx) }),
  },

  rename_workspace: {
    title: "Rename Workspace",
    description: "Rename a workspace globally without changing member pages, URLs, versions, or serving state.",
    inputSchema: z.object({ workspace_id: IdArg, name: z.string().min(1).max(100) }).strict(),
    outputSchema: z.object({ workspace: WorkspaceSchema }).strict(),
    annotations: IdempotentWriteAnnotations,
    handler: async (args, ctx) => {
      const renamed = await workspaces.rename({ id: args.workspace_id, name: args.name }, ctx);
      const current = (await workspaces.list()).find((workspace) => compareIds(workspace.id, renamed.id));
      return { workspace: current || { ...renamed, page_count: 0 } };
    },
  },

  set_page_workspace: {
    title: "Set Page Workspace",
    description:
      "Move an active page into a workspace, or pass workspace_id:null for Ungrouped. This is reversible and does not affect serving or URLs.",
    inputSchema: z.object({ slug: Slug, workspace_id: NullableIdArg }).strict(),
    outputSchema: z
      .object({
        page: z
          .object({ slug: z.string(), workspace_id: IdOut.nullable(), workspace_name: z.string().nullable() })
          .strict(),
      })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: async (args, ctx) => ({
      page: await workspaces.assignPage({ slug: args.slug, workspaceId: args.workspace_id }, ctx),
    }),
  },

  list_themes: {
    title: "List Page Themes",
    description: "List the curated themes a human admin can assign. Theme mutation remains human-only.",
    inputSchema: z.object({}).strict(),
    outputSchema: z.object({ themes: z.array(ThemeSchema) }).strict(),
    annotations: ReadAnnotations,
    handler: async () => ({ themes: await versions.listThemes() }),
  },

  create_upload_ticket: {
    title: "Create Page Upload Ticket",
    description:
      "PREFERRED way to publish a file you already have on disk — page HTML, a template, or a managed-data JSON payload. Returns a one-shot URL your shell can PUT the file to directly, so the bytes never pass through your own output: `curl -fsS -X PUT --data-binary @<file> -H \"Authorization: Bearer <ticket>\" <upload_url>`. Supply the exact byte count and lowercase SHA-256 first — the ticket only ever accepts those exact bytes. Then call deploy_page_upload with the upload_id — register_template_upload if you staged a template, update_page_data_upload if you staged kind 'data'. Use append_page_upload instead only if your environment cannot make outbound HTTP requests.",
    inputSchema: z
      .object({
        slug: Slug.optional().describe("The page this upload targets. Supply exactly one of slug or template."),
        template: TemplateName.optional().describe(
          "Register a reusable template instead of deploying a page. Consume with register_template_upload."
        ),
        kind: z
          .enum(["page", "data"])
          .optional()
          .describe(
            "With a slug: 'page' (default) stages HTML for deploy_page_upload; 'data' stages a managed-data JSON " +
              "payload for update_page_data_upload. Fixed here and enforced at deploy, so JSON can never be served " +
              "as a page and a document can never be parsed into a data envelope."
          ),
        total_bytes: z.number().int().min(1).max(pageUploads.MAX_UPLOAD_BYTES),
        content_sha256: Sha256.describe("Lowercase SHA-256 of the exact original UTF-8 file bytes."),
      })
      .strict(),
    outputSchema: UploadStateSchema.extend({
      upload_url: z.string().describe("PUT the raw file bytes here. Absolute — do not rebuild it from a hostname."),
      ticket: z.string().describe("Send as `Authorization: Bearer <ticket>`. Write-only, single upload, expires in minutes."),
      ticket_expires_at: DateTime,
      curl: z.string().describe("A ready-to-run command; substitute your local file path."),
    }).strict(),
    annotations: AdditiveAnnotations,
    handler: async (args, ctx) => {
      const created = await pageUploads.createTicket(
        {
          slug: args.slug,
          template: args.template,
          kind: args.kind,
          totalBytes: args.total_bytes,
          contentSha256: args.content_sha256,
        },
        ctx
      );
      const uploadUrl = uploadTicket.uploadUrl(created.upload_id);
      const consumer = pageUploads.consumerFor(created.target_kind);
      return {
        ...created,
        upload_url: uploadUrl,
        curl: `curl -fsS -X PUT --data-binary @<file> -H "Authorization: Bearer ${created.ticket}" ${uploadUrl}`,
        next_step:
          `Send the file with: curl -fsS -X PUT --data-binary @<file> -H "Authorization: Bearer ${created.ticket}" ${uploadUrl}` +
          ` — then call ${consumer} with upload_id ${created.upload_id}. Do not base64 the file into a tool argument.`,
      };
    },
  },

  start_page_upload: {
    title: "Start Page Content Upload",
    description:
      "FALLBACK path — use create_upload_ticket unless your environment cannot make outbound HTTP requests.\n\nThis is the expensive branch, and it does not look like one from here. It makes YOU emit the whole file as base64, token by token, in tool arguments: a retry re-sends everything, a truncated argument kills the upload, and the sequence, the declared byte count and the canonical encoding are all yours to keep straight across many calls. One observed session started ten uploads for one page, cancelled eight, spent over 10 million tokens, and shipped nothing. create_upload_ticket returns a URL your shell PUTs the file to, and none of that applies.\n\nIf you are here anyway: supply the exact file byte count and lowercase SHA-256, then append the original bytes as ordered base64 chunks. Never send a path, shell expression, placeholder, or the whole file here. When an append fails, RE-SEND it — a failed append does not advance the sequence and nothing already accepted is lost. Cancelling throws away every byte you have paid for.",
    inputSchema: z
      .object({
        slug: Slug.optional().describe("The page this upload targets. Supply exactly one of slug or template."),
        template: TemplateName.optional().describe(
          "Register a reusable template instead of deploying a page. Consume with register_template_upload."
        ),
        kind: z
          .enum(["page", "data"])
          .optional()
          .describe(
            "With a slug: 'page' (default) stages HTML for deploy_page_upload; 'data' stages a managed-data JSON " +
              "payload for update_page_data_upload."
          ),
        total_bytes: z.number().int().min(1).max(pageUploads.MAX_UPLOAD_BYTES),
        content_sha256: Sha256.describe("Lowercase SHA-256 of the exact original UTF-8 file bytes."),
      })
      .strict(),
    outputSchema: UploadStateSchema,
    annotations: AdditiveAnnotations,
    handler: (args, ctx) =>
      pageUploads.start(
        {
          slug: args.slug,
          template: args.template,
          kind: args.kind,
          totalBytes: args.total_bytes,
          contentSha256: args.content_sha256,
        },
        ctx
      ),
  },

  append_page_upload: {
    title: "Append Page Content Upload",
    description:
      "Append one ordered base64 chunk to a staged upload. Decode size must not exceed max_chunk_bytes from start_page_upload. Send the exact next_sequence; replaying the same sequence and the same bytes is a safe no-op.\n\nA FAILED append does not advance the sequence and does not lose anything already accepted, so every error here is resumable: re-send from the expected_sequence the error reports. Do not cancel to recover — that deletes every byte you have already emitted and makes you send the whole document again, which is how a single page can consume a whole turn.\n\nPreserve original byte order; do not minify or alter content between chunks.",
    inputSchema: z
      .object({
        upload_id: UploadId,
        sequence: z.number().int().nonnegative(),
        chunk_base64: z
          .string()
          .min(4)
          .max(pageUploads.MAX_CHUNK_BASE64_CHARS)
          .describe("Canonical base64 for the next raw file chunk; no data-URI prefix or whitespace."),
      })
      .strict(),
    outputSchema: UploadStateSchema.extend({ deduped: z.boolean() }).strict(),
    annotations: IdempotentAdditiveAnnotations,
    handler: (args, ctx) =>
      pageUploads.append(
        { uploadId: args.upload_id, sequence: args.sequence, chunkBase64: args.chunk_base64 },
        ctx
      ),
  },

  cancel_page_upload: {
    title: "Cancel Page Content Upload",
    description:
      "Discard an uncommitted staged upload and free its active-upload slot. This never changes a page or deployed version. Repeating a cancellation is a safe no-op; committed uploads cannot be cancelled.\n\nDo NOT use this to recover from an append error. Sequence and hash failures are resumable — the failed chunk was not accepted and everything before it is still stored — so cancelling only throws away bytes you already paid to emit and forces the whole document again. Cancel when you no longer want to deploy this content at all.",
    inputSchema: z.object({ upload_id: UploadId }).strict(),
    outputSchema: z
      .object({ upload_id: UploadId, cancelled: z.boolean(), next_step: z.string() })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: (args, ctx) => pageUploads.cancel(args.upload_id, ctx),
  },

  list_templates: {
    title: "List Templates",
    description:
      "List reusable page templates: name, title, current revision, schema hashes, and how many pages were built from each. page_count is every live page whose history touches the design; serving_count is those whose PUBLISHED version still comes from it; drifted_count is the difference \u2014 pages a later raw deploy detached, which a design fix will no longer reach. Call list_template_pages to see which. A template is one stored design; a page built from it carries only its own config and data, so building the second dashboard of a family costs kilobytes of JSON instead of the whole document.",
    inputSchema: z.object({}).strict(),
    outputSchema: z
      .object({
        templates: z.array(
          TemplateSchema.extend({
            config_schema_sha256: Sha256.nullable(),
            data_schema_sha256: Sha256.nullable(),
            page_count: z.number().int().nonnegative(),
            serving_count: z.number().int().nonnegative(),
            drifted_count: z.number().int().nonnegative(),
          }).strict()
        ),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: () => templates.list(),
  },

  get_template: {
    title: "Get Template",
    description:
      "Read what a template needs in order to build a page: its config JSON Schema, its data JSON Schema, and the reference config it ships with. Call this INSTEAD of reading a dashboard's HTML — it answers 'what does this design require?' in a couple of KB. The HTML is omitted unless include_html is true, and you almost never need it. Pass revision to inspect an exact past revision.",
    inputSchema: z
      .object({
        template: TemplateName,
        revision: z.number().int().positive().optional().describe("Defaults to the current revision."),
        include_html: z
          .boolean()
          .optional()
          .describe("Defaults to false. Only set this if you must edit the design itself."),
      })
      .strict(),
    outputSchema: z
      .object({
        template: TemplateSchema,
        revision: TemplateRevisionSchema,
        config_schema: JsonObjectSchema,
        data_schema: JsonObjectSchema,
        reference_config: JsonObjectSchema.describe(
          "The config this template shipped with. A STARTING POINT to read, not a default to inherit: create_page_from_template requires a complete config so one client's identity can never leak into another's page."
        ),
        html: z.string().optional(),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: (args) =>
      templates.get(args.template, {
        revision: args.revision === undefined ? null : args.revision,
        includeHtml: args.include_html === true,
      }),
  },

  list_template_revisions: {
    title: "List Template Revisions",
    description:
      "List a template's revisions, newest first, marking the current one. Pages pin the exact revision they were built from, so a new revision never changes a deployed page.",
    inputSchema: z.object({ template: TemplateName }).strict(),
    outputSchema: z
      .object({
        template: z.string(),
        revisions: z.array(TemplateRevisionSchema.extend({ is_current: z.boolean() }).strict()),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: (args) => templates.revisions(args.template),
  },

  create_template_from_page: {
    title: "Make An Existing Page Reusable",
    description:
      "Promote a page you already like into a reusable design, WITHOUT moving its bytes — the design is already on the server, so nothing is re-authored and nothing passes through your output. Use this whenever someone says 'I want more pages like this one'. It requires only that the page already separates its per-instance values into a #pages-config block; if it does not, this fails with page_not_template_managed and you should separate them and redeploy first (a config schema is derived for you, so that costs no schema authoring). Then build the next client's page with create_page_from_template and a few KB of config.",
    inputSchema: z
      .object({
        slug: Slug.describe("The page to promote. Its published version is what gets stored."),
        template: TemplateName.describe("The name to register the design under."),
        empty_data: JsonObjectSchema.describe(
          "What a page built from this shows BEFORE its first refresh — usually empty collections, e.g. {\"rows\":[]}. Validated against the page's own data schema. A template must ship empty so no page inherits this one's numbers."
        ),
        example_from_current_data: z
          .boolean()
          .optional()
          .describe(
            "Copy this page's CURRENT data into the design's preview-only example block, so the library shows the design populated. Off by default because it is a real client's numbers and a template is visible to everyone — prefer a fictional dataset."
          ),
        title: z.string().max(200).optional(),
        description: z.string().max(2000).optional(),
        note: z.string().max(500).optional(),
      })
      .strict(),
    outputSchema: z
      .object({
        created: z.boolean(),
        deduped: z.boolean(),
        template: TemplateSchema,
        revision: TemplateRevisionSchema,
        config_schema: JsonObjectSchema,
        data_schema: JsonObjectSchema,
        reference_config: JsonObjectSchema.describe(
          "The promoted page's own config, now the design's reference config. It is shown to everyone who opens the library and handed to agents by get_template — if it names a real client, replace it."
        ),
        has_sample_data: z.boolean(),
        from_page: z.string(),
        from_version_id: IdOut,
        example_from_current_data: z.boolean(),
        hardcoded_config_values: z
          .array(z.object({ path: z.string(), value: z.string(), occurrences: z.number().int().positive() }).strict())
          .nullable()
          .describe(
            "Config values that are ALSO written into the design outside the config block \u2014 a <title>, a heading, a legend. Moving a value into #pages-config only makes it vary per instance if the design reads it from there, so every page built from this would show the hardcoded copy. Advisory: fix them with patch_page, or ignore any that genuinely belong to the design."
          ),
        preflight: PreflightSchema,
        next_step: z.string(),
      })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: async (args, ctx) => {
      const result = await templates.createFromPage(
        {
          slug: args.slug,
          name: args.template,
          empty_data: args.empty_data,
          example_from_current_data: args.example_from_current_data === true,
          title: args.title,
          description: args.description,
          note: args.note,
        },
        ctx
      );
      return { ...result, next_step: templateNextStep(result) };
    },
  },

  validate_template: {
    title: "Validate A Template Before Registering It",
    description:
      "Dry-run the template contract and report what is wrong, WITHOUT writing anything. Pass upload_id to check bytes you already staged through a ticket — they are verified and read but NOT consumed, so the same upload_id still registers afterwards and you never re-send the file. Pass html instead only for something small. Checks all five blocks, that both schemas are self-contained JSON Schema 2020-12, that the shipped reference config and empty envelope satisfy their own schemas, that an optional #pages-data-example satisfies the data schema, and runs preflight. Call this before register_template_upload on any design you have not registered before.",
    inputSchema: z
      .object({
        upload_id: UploadId.optional().describe(
          "A staged upload started with `template`. Preferred: the bytes never pass through your output, and this does not consume the upload."
        ),
        html: z.string().min(1).optional().describe("Inline alternative to upload_id. Costs the whole document in output tokens."),
        name: TemplateName.optional().describe(
          "The name you intend to register under. Checked too, and reported separately from the contract, so a bad name and a bad design are told apart."
        ),
      })
      .strict(),
    outputSchema: z
      .object({
        upload_id: UploadId.nullable(),
        name: z.string().nullable(),
        name_error: z.object({ code: z.string(), message: z.string() }).strict().nullable(),
        contract_ok: z.boolean(),
        contract_error: z
          .object({ code: z.string(), message: z.string(), details: z.unknown().optional() })
          .loose()
          .nullable(),
        bytes: z.number().int().nonnegative().nullable(),
        config_schema: JsonObjectSchema.nullable(),
        data_schema: JsonObjectSchema.nullable(),
        reference_config: JsonObjectSchema.nullable(),
        data_keys: z.array(z.string()).nullable(),
        ships_empty: z.boolean().nullable().describe(
          "False means #pages-data carries rows, which every page built from this would inherit until its first refresh. Put example rows in #pages-data-example instead."
        ),
        has_sample_data: z.boolean().nullable(),
        sample_data_keys: z.array(z.string()).nullable(),
        hardcoded_config_values: z
          .array(z.object({ path: z.string(), value: z.string(), occurrences: z.number().int().positive() }).strict())
          .nullable()
          .describe(
            "Config values that are ALSO written into the design outside the config block \u2014 a <title>, a heading, a legend. Moving a value into #pages-config only makes it vary per instance if the design reads it from there, so every page built from this would show the hardcoded copy. Advisory: fix them with patch_page, or ignore any that genuinely belong to the design."
          ),
        preflight: PreflightSchema.nullable().describe(
          "Errors here are inherited by every page built from this template."
        ),
        next_step: z.string(),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: async (args, ctx) => {
      const hasUpload = args.upload_id !== undefined;
      const hasHtml = args.html !== undefined;
      if (hasUpload === hasHtml) {
        throw badRequest("pass exactly one of upload_id or html", "template_source_ambiguous");
      }
      let html = args.html;
      let name = args.name;
      if (hasUpload) {
        // peek() verifies the staged bytes exactly as registration will — target
        // kind, completeness, SHA-256, UTF-8 — and leaves the upload intact.
        const staged = await pageUploads.peek(args.upload_id, ctx, { expectKind: "template" });
        html = staged.html;
        // The upload already names the template, so the caller need not repeat it.
        if (name === undefined) name = staged.template;
      }
      const report = templates.validateHtml(html, { name: name === undefined ? null : name });
      const preflightFailed = report.preflight && report.preflight.ok === false;
      return {
        upload_id: hasUpload ? String(args.upload_id) : null,
        ...report,
        next_step: !report.contract_ok
          ? "Fix the blocks named in contract_error, then validate again. Nothing was written."
          : report.name_error
            ? "The design is valid but the name is not; pick a url-safe name and validate again."
            : preflightFailed
              ? "Valid, but preflight found errors that every page built from this would inherit. Fix the design, or register deliberately with allow_preflight_errors."
              : hasUpload
                ? "Valid. Call register_template_upload with this same upload_id."
                : "Valid. Stage the file with create_upload_ticket, then register_template_upload.",
      };
    },
  },

  delete_template: {
    title: "Retire A Template",
    description:
      "Soft-delete a template so it leaves the library and its name becomes reusable — the fix for a mistyped name, and the way to retire a superseded design. Revisions are kept, and pages already built from it KEEP SERVING: a page carries its own materialized HTML and never reads the template at runtime. Refused when pages were built from it unless force is true; those pages then lose their design provenance and can no longer be re-rendered from it. Call list_template_pages first. Confirm with the user before calling.",
    inputSchema: z
      .object({
        template: TemplateName,
        force: z
          .boolean()
          .optional()
          .describe("Required to retire a template that pages were built from. Defaults to false."),
      })
      .strict(),
    outputSchema: z
      .object({
        template: z.string(),
        deleted: z.boolean(),
        pages_built: z.number().int().nonnegative(),
        note: z.string(),
      })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: async (args, ctx) => {
      const result = await templates.remove({ template: args.template, force: args.force === true }, ctx);
      return {
        ...result,
        note:
          result.pages_built > 0
            ? `${result.pages_built} page(s) built from this keep serving, but can no longer be re-rendered from it. The name is free to re-register.`
            : "The name is free to re-register.",
      };
    },
  },

  template_urls: {
    title: "Get Template URLs",
    description:
      "Return the library URL for a template plus a short-TTL signed preview URL that renders the design on the sandboxed content host — populated with the design's example data when it ships one, otherwise its empty state. Use it to show a human what a design looks like before building pages from it. The preview URL expires; mint a fresh one rather than storing it.",
    inputSchema: z
      .object({
        template: TemplateName,
        revision: z.number().int().positive().optional().describe("Defaults to the current revision."),
      })
      .strict(),
    outputSchema: z
      .object({
        template: z.string(),
        revision: z.number().int().positive(),
        library: z.string().url().describe("The template library, on the dashboard host. Requires an admin session."),
        preview: z.string().url().describe("Signed, sandboxed, on the content host. Expires."),
        expires_in_seconds: z.number().int().positive(),
        has_sample_data: z.boolean(),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: async (args) => {
      const target = await templates.getRevisionHtml(
        args.template,
        args.revision === undefined ? null : args.revision
      );
      const token = rawtoken.mint(
        {
          pageId: 0,
          versionId: Number(target.template_version_id),
          purpose: "template",
          renderMode: "themed",
        },
        TEMPLATE_PREVIEW_TTL_SECONDS
      );
      return {
        template: target.template,
        revision: target.revision,
        library: `${DASHBOARD_ORIGIN}/admin/templates`,
        preview: `${CONTENT_ORIGIN}/raw-template/${target.template_version_id}?t=${encodeURIComponent(token)}`,
        expires_in_seconds: TEMPLATE_PREVIEW_TTL_SECONDS,
        has_sample_data: target.has_sample_data,
      };
    },
  },

  list_template_pages: {
    title: "List Pages Built From A Template",
    description:
      "Show every live page whose history touches this template: which revision each one currently serves, which are behind the current revision, and which have DRIFTED off the design entirely (drifted:true \u2014 a later raw deploy_page or patch_page replaced the template-built version, so a design fix no longer reaches them). A drifted page keeps last_revision, which is what rerender_page_from_template needs to pull it back. Read-only. Use it to decide what a design fix should move; Pages never re-renders a page on its own, because a bad revision would otherwise be a fleet-wide incident instead of one page to fix.",
    inputSchema: z.object({ template: TemplateName }).strict(),
    outputSchema: z
      .object({
        template: z.string(),
        current_revision: z.number().int().positive(),
        serving_count: z.number().int().nonnegative(),
        drifted_count: z.number().int().nonnegative(),
        pages: z.array(
          z
            .object({
              slug: z.string(),
              title: z.string(),
              // Nullable on a drifted page: there is no published version, or the
              // one there is has no binding to this template.
              live_version_id: IdOut.nullable(),
              revision: z.number().int().positive().nullable(),
              behind: z.boolean(),
              drifted: z.boolean(),
              last_revision: z.number().int().positive().nullable(),
              page_is_live: z.boolean(),
              config_sha256: Sha256.nullable(),
            })
            .strict()
        ),
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: (args) => templates.listTemplatePages(args.template),
  },

  rerender_page_from_template: {
    title: "Rerender One Page On A Template Revision",
    description:
      "Move ONE page onto a different revision of its template, or pull a DRIFTED page back onto it, keeping its own config and data exactly as they are. A page a raw deploy_page or patch_page detached (list_template_pages: drifted:true) has no binding left to infer the design from, so name the template explicitly and it is re-attached \u2014 which works as long as its published HTML still carries #pages-config and #pages-data, the usual case after patch_page. Both are taken from the page's published HTML and re-validated against the target revision's schemas, so a revision that tightened its contract fails loudly instead of producing a page its own schema rejects. publish defaults to FALSE: the new design lands as an inspectable version so a human can preview it before a client sees it, then publish_page makes it live. One page per call — there is no bulk re-render.",
    inputSchema: z
      .object({
        slug: Slug,
        template: TemplateName.optional().describe("Defaults to the template the page is already built from."),
        revision: z.number().int().positive().optional().describe("Defaults to the template's current revision."),
        publish: z
          .boolean()
          .optional()
          .describe("Defaults to FALSE. Set true only when a design change has already been reviewed."),
        expected_version: IdArg.optional().describe("Optimistic-concurrency check against the current published version."),
        note: z.string().max(500).optional(),
      })
      .strict(),
    outputSchema: DeployOutputSchema.extend({
      template: z.string(),
      from_revision: z
        .number()
        .int()
        .positive()
        .nullable()
        .describe("The revision the page was serving, or null when it had drifted off the design entirely."),
      reattached: z
        .boolean()
        .describe("True when this call pulled a drifted page back onto the template rather than moving revisions."),
      template_revision: z.number().int().positive(),
      template_version_id: IdOut,
      config: JsonObjectSchema,
      config_sha256: Sha256,
      envelope: DataEnvelopeSchema,
      data_sha256: Sha256,
      template_sha256: Sha256,
    }).strict(),
    annotations: WriteAnnotations,
    handler: async (args, ctx) => {
      const result = await templates.rerenderPage(
        {
          slug: args.slug,
          template: args.template === undefined ? null : args.template,
          revision: args.revision === undefined ? null : args.revision,
          expectedVersion: args.expected_version === undefined ? null : args.expected_version,
          publish: args.publish === true,
          note: args.note,
        },
        ctx
      );
      const page = (await versions.getPage(args.slug)).page;
      const { html, ...rest } = result;
      return {
        ...rest,
        ...deployGuidance(result, page, versions.normalizeSlug(args.slug)),
        preflight: preflight.analyze(html, { renderMode: result.version.render_mode }),
      };
    },
  },

  register_template_upload: {
    title: "Register Staged Template",
    description:
      "Register a complete staged upload as a template revision. The upload must have been started with `template` rather than `slug`. The HTML must be a full managed page carrying BOTH block pairs — pages-config-schema/pages-config and pages-data-schema/pages-data — with a reference config and an empty-state data envelope that each satisfy their own schema; that is what lets Pages validate the design instead of trusting it. Registering identical bytes again returns the same revision. This never touches a live page: existing pages stay pinned to the revision they were built from.",
    inputSchema: z
      .object({
        upload_id: UploadId,
        title: z.string().max(200).optional(),
        description: z.string().max(2000).optional(),
        note: z.string().max(500).optional(),
      })
      .strict(),
    outputSchema: z
      .object({
        upload_id: UploadId,
        created: z.boolean(),
        deduped: z.boolean(),
        template: TemplateSchema,
        revision: TemplateRevisionSchema,
        config_schema: JsonObjectSchema,
        data_schema: JsonObjectSchema,
        reference_config: JsonObjectSchema,
        has_sample_data: z.boolean().describe(
          "Whether the design shipped an optional #pages-data-example block. Preview-only: it is deleted from every page built from this template."
        ),
        preflight: PreflightSchema.describe(
          "Static check of the TEMPLATE. Errors here are inherited by every page built from it, so fix them before creating pages."
        ),
        next_step: z.string(),
      })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: registerTemplateUpload,
  },

  deploy_page_upload: {
    title: "Deploy Staged Page Content",
    description:
      "Verify and atomically deploy a complete staged upload, creating the page if needed and publishing by default on open pages. The upload slug is fixed by start_page_upload. Exact retries of this upload_id return the original commit result. Approval, optimistic concurrency, immutable versions, and audit behavior match deploy_page.",
    inputSchema: z.object(UploadDeployInputShape).strict(),
    outputSchema: DeployOutputSchema.extend({ upload_id: UploadId, created: z.boolean() }).strict(),
    annotations: IdempotentWriteAnnotations,
    handler: deployPageUpload,
  },

  // The managed-data half of the staged path. HTML has had one since #14;
  // data — generated on disk by run_python and routinely LARGER than the
  // HTML — could only ever travel inline as a tool argument.
  update_page_data_upload: {
    title: "Update Managed Page Data From Staged Upload",
    description:
      "Publish a managed-data payload you staged with create_upload_ticket (kind 'data') instead of sending it inline. The JSON never passes through your context, so there is no inline size ceiling to hit. Everything else is update_page_data exactly: same schema validation, same monotonic source_as_of, same mandatory expected_version, same dedupe, same data_profile / data_warnings, same expect reconciliation — one write path, not two.\n\nThe staged bytes must parse as a JSON object. An upload staged as 'page' is refused here, and one staged as 'data' is refused by deploy_page_upload.",
    inputSchema: z
      .object({
        upload_id: UploadId,
        slug: Slug.describe("Must match the slug the upload was staged for."),
        source_as_of: DateTime.describe("Latest source coverage represented by the payload; cannot regress or be materially future."),
        expected_version: IdArg.describe("Required live_version_id from get_page_data."),
        publish: z.boolean().optional().describe("Defaults to true. False creates an inspectable canary draft/pending version."),
        note: z.string().max(500).optional(),
        expect: DataExpectSchema.optional(),
      })
      .strict(),
    outputSchema: z
      .object({
        ...PageDataStateShape,
        upload_id: UploadId,
        deduped: z.boolean(),
        published: z.boolean(),
        gated: z.boolean(),
        next_step: z.string(),
        data_profile: DataProfileSchema,
        data_warnings: z.array(DataWarningSchema),
      })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: updatePageDataUpload,
  },

  create_page_from_template: {
    title: "Create Page From Template",
    description:
      "Build a page from a stored template by sending ONLY its config — the design is already on the server, so this costs kilobytes of JSON instead of the whole document. Call get_template first to read the config and data schemas. config must be COMPLETE: it is validated against the template's config schema and is never merged with the template's reference config, so one client's identity cannot leak into another's page. Omit data to deploy the design's awaiting-first-ingest state and fill it later with update_page_data. Fails with page_exists if the slug is taken. An identical retry is a no-op only while that build is still the page's live version (or the page has published nothing yet) — once the page has taken a refresh or a config edit, replaying the create is page_exists too, because deploying it would move the live pointer back to the empty state. Use rollback_page if reverting is what you mean.",
    inputSchema: z
      .object({
        template: TemplateName,
        slug: Slug,
        config: JsonObjectSchema.describe("Complete config satisfying the template's config schema."),
        revision: z
          .number()
          .int()
          .positive()
          .optional()
          .describe("Build from an exact template revision. Defaults to the current one."),
        data: JsonObjectSchema.optional().describe(
          "Optional first payload. When omitted the template's empty-state data is kept and the page shows its awaiting-first-ingest states rather than fabricated zeros."
        ),
        source_as_of: DateTime.optional().describe("Required when data is supplied: the coverage that data represents."),
        render_mode: z.enum(["themed", "raw"]).optional().describe("Defaults to themed."),
        title: z.string().max(200).optional(),
        require_approval: z.boolean().optional(),
        client_id: z.string().max(200).optional(),
        publish: z.boolean().optional().describe("Defaults to true on open pages."),
        note: z.string().max(500).optional(),
      })
      .strict(),
    outputSchema: DeployOutputSchema.extend({
      created: z.boolean(),
      template: z.string(),
      template_revision: z.number().int().positive(),
      template_version_id: IdOut,
      config: JsonObjectSchema,
      config_sha256: Sha256,
      config_schema_sha256: Sha256,
      envelope: DataEnvelopeSchema,
      data_sha256: Sha256,
      schema_sha256: Sha256,
      template_sha256: Sha256,
    }).strict(),
    annotations: IdempotentWriteAnnotations,
    handler: async (args, ctx) => {
      if (args.data !== undefined && args.source_as_of === undefined) {
        throw badRequest(
          "source_as_of is required when data is supplied; it is the coverage that data represents",
          "source_as_of_invalid"
        );
      }
      const result = await templates.createPage(
        {
          template: args.template,
          revision: args.revision === undefined ? null : args.revision,
          slug: args.slug,
          config: args.config,
          data: args.data === undefined ? null : args.data,
          sourceAsOf: args.source_as_of === undefined ? null : args.source_as_of,
          title: args.title || "",
          clientId: args.client_id || null,
          requireApproval: !!args.require_approval,
          renderMode: args.render_mode || "themed",
          publish: args.publish === undefined ? true : args.publish,
          note: args.note,
        },
        ctx
      );
      const page = (await versions.getPage(args.slug)).page;
      const { html, ...rest } = result;
      return {
        ...rest,
        ...deployGuidance(result, page, versions.normalizeSlug(args.slug)),
        created: result.created,
        preflight: preflight.analyze(html, { renderMode: args.render_mode || "themed" }),
      };
    },
  },

  get_page_config: {
    title: "Get Page Config",
    description:
      "Read the deploy-time config of a template-built page: its config JSON Schema, the current config, deterministic hashes, and the live version to pass as update_page_config's expected_version. Separate from get_page_data so changing a campaign's identity never requires reading its rows. Fails with page_not_template_managed on a page that has no pages-config block.",
    inputSchema: z.object({ slug: Slug }).strict(),
    outputSchema: z
      .object({
        page: PageSchema,
        version: VersionSchema,
        config_schema: JsonObjectSchema,
        config: JsonObjectSchema,
        config_sha256: Sha256,
        config_schema_sha256: Sha256,
        template_sha256: Sha256,
        version_is_live: z.boolean(),
        page_is_live: z.boolean(),
        live_version_id: IdOut.nullable(),
        urls: UrlsSchema,
      })
      .strict(),
    annotations: ReadAnnotations,
    handler: async (args) => {
      const result = await versions.getPageConfig(args.slug);
      return {
        ...result,
        ...publishedLiveState(result.page),
        urls: pageUrls(result.page.slug),
      };
    },
  },

  update_page_config: {
    title: "Update Page Config",
    description:
      "Replace the config of a template-built page, validated against its embedded config schema. The data block is left BYTE-FOR-BYTE unchanged, so renaming a campaign or retargeting a KPI cannot disturb, restate, or roll back its numbers, and source coverage is carried across untouched. config must be complete — this replaces, it does not merge. expected_version is mandatory optimistic concurrency; use the live_version_id from get_page_config.",
    inputSchema: z
      .object({
        slug: Slug,
        config: JsonObjectSchema.describe("Complete replacement config satisfying the page's config schema."),
        expected_version: IdArg.describe("Required live_version_id from get_page_config."),
        publish: z.boolean().optional().describe("Defaults to true. False creates an inspectable canary."),
        note: z.string().max(500).optional(),
      })
      .strict(),
    outputSchema: z
      .object({
        version: VersionSchema,
        config: JsonObjectSchema,
        config_schema: JsonObjectSchema,
        config_sha256: Sha256,
        config_schema_sha256: Sha256,
        envelope: DataEnvelopeSchema,
        data_sha256: Sha256,
        template_sha256: Sha256,
        deduped: z.boolean(),
        published: z.boolean(),
        gated: z.boolean(),
        live: z.boolean().describe("Compatibility alias for version_is_live."),
        version_is_live: z.boolean(),
        page_is_live: z.boolean(),
        live_version_id: IdOut.nullable(),
        urls: UrlsSchema,
        next_step: z.string(),
      })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: async (args, ctx) => {
      const result = await versions.updatePageConfig(
        {
          slug: args.slug,
          config: args.config,
          expectedVersion: args.expected_version,
          publish: args.publish === undefined ? true : args.publish,
          note: args.note,
        },
        ctx
      );
      const page = (await versions.getPage(args.slug)).page;
      return {
        ...result,
        ...deployGuidance(result, page, versions.normalizeSlug(args.slug)),
      };
    },
  },

  deploy_page: {
    title: "Deploy Page",
    description:
      "Create a page if needed from small inline HTML and publish by default on open pages. For a workspace file or content over 20,000 UTF-8 bytes, use the staged page-upload tools instead. Approval-gated pages remain pending. Follow page_is_live, version_is_live, and next_step.",
    inputSchema: z
      .object({
        ...DeployInputShape,
        title: z.string().max(200).optional().describe(
        "Used only if the page is created. PLAIN TEXT \u2014 Pages escapes it wherever it renders, so pass \"Contoso & Allergex\", never \"Contoso &amp; Allergex\"; a pre-escaped title shows its own entities."
      ),
        require_approval: z.boolean().optional().describe("Used only if the page is created."),
        client_id: z.string().max(200).optional().describe("Used only if the page is created."),
      })
      .strict(),
    outputSchema: DeployOutputSchema.extend({ created: z.boolean() }).strict(),
    annotations: WriteAnnotations,
    handler: (args, ctx) => deployPage(args, ctx, false),
  },

  update_page: {
    title: "Update Page",
    description:
      "Deploy small inline HTML to an existing page and publish by default on open pages. For a workspace file or content over 20,000 UTF-8 bytes, use the staged page-upload tools instead. Fails if the page is missing. Supports optimistic concurrency.",
    inputSchema: z.object(DeployInputShape).strict(),
    outputSchema: DeployOutputSchema,
    annotations: WriteAnnotations,
    handler: async (args, ctx) => {
      const result = await deployPage(args, ctx, true);
      const { created: _created, ...output } = result;
      return output;
    },
  },

  update_page_data: {
    title: "Update Managed Page Data",
    description:
      "Validate structured data against the published page's embedded JSON Schema, then create an immutable version by replacing only the pages-data script contents. source_as_of is required, monotonic RFC3339 source coverage; refreshed_at is generated by Pages. expected_version is mandatory optimistic concurrency. Same template+data+source dedupes; newer source coverage versions even unchanged metrics. Defaults publish=true; use publish=false for a canary.\n\nThe schema validates SHAPE ONLY \u2014 a payload missing three weeks of days, or a whole deal, satisfies it exactly as well as a correct one. So the response returns data_profile (row counts, date extents, numeric totals, distinct values of low-cardinality keys) and data_warnings (coverage that starts later or ends earlier than what is already published, rows that dropped, dimension values that disappeared). RECONCILE data_profile against your source before you tell anyone the page is right. Better: pass expect with the totals you computed from the source and let Pages refuse the write if the payload disagrees.",
    inputSchema: z
      .object({
        slug: Slug,
        data: JsonObjectSchema.describe(
          "Complete data object that satisfies the page's returned schema; never include credentials. "
            + "SEND IT WHOLE: payloads up to ~1.5 MB are supported and routinely published (live pages carry "
            + "600 KB-1 MB). Do not split it across calls, sample it, or decline to send because it looks "
            + "large — there is no file/path/upload variant of this tool, and a partial payload passes schema "
            + "validation while publishing wrong numbers. Over the cap you get an explicit "
            + "data_too_large_for_inline error rather than a truncation."
        ),
        source_as_of: DateTime.describe("Latest source coverage represented by data; cannot regress or be materially future."),
        expected_version: IdArg.describe("Required live_version_id from get_page_data."),
        publish: z.boolean().optional().describe("Defaults to true. False creates an inspectable canary draft/pending version."),
        note: z.string().max(500).optional(),
        expect: DataExpectSchema.optional(),
      })
      .strict(),
    outputSchema: z
      .object({
        ...PageDataStateShape,
        deduped: z.boolean(),
        published: z.boolean(),
        gated: z.boolean(),
        next_step: z.string(),
        data_profile: DataProfileSchema,
        data_warnings: z.array(DataWarningSchema),
      })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: updatePageData,
  },

  // Transitional alias retained because the deployed Chat/Cutlass allowlists
  // already permit this name. Its old schedule-mutating contract is gone; it is
  // the same read-only prompt preparation as the canonical tool above.
  configure_page_refresh: {
    title: "Prepare Dashboard Update (Compatibility)",
    description:
      "Compatibility alias for prepare_dashboard_update on older static MCP allowlists. On those clients call this whenever a user says 'update <slug> dashboard with ...', passing the exact request as instructions. It is read-only: Pages does not configure, persist, or dispatch a schedule. Legacy daily_at_utc/workflow arguments are safely converted into recurring prompt text so already-deployed clients need no code change; run_now never executes work.",
    inputSchema: ConfigurePageRefreshCompatibilityInputSchema,
    outputSchema: PrepareDashboardUpdateOutputSchema,
    annotations: ReadAnnotations,
    handler: prepareDashboardUpdateCompatibility,
  },

  publish_page: {
    title: "Publish Page Version",
    description: "Publish a draft version on an open page. Agents cannot publish approval-gated or disabled pages.",
    inputSchema: z.object({ slug: Slug, version_id: IdArg, expected_version: IdArg.optional() }).strict(),
    outputSchema: PublishOutputSchema,
    annotations: WriteAnnotations,
    handler: (args, ctx) => publishedResult(args, ctx, "publish"),
  },

  rollback_page: {
    title: "Rollback Page",
    description:
      "Move an open page's published pointer to an approved version. Omit version_id to choose the previous approved version. note is recorded in the audit log \u2014 a rollback republishes bytes that already exist, so the reason is not inferable from any diff.",
    inputSchema: z
      .object({
        slug: Slug,
        version_id: IdArg.optional(),
        expected_version: IdArg.optional(),
        note: z.string().max(500).optional().describe("Why the pointer moved. Recorded in the audit log."),
      })
      .strict(),
    outputSchema: PublishOutputSchema,
    annotations: WriteAnnotations,
    handler: (args, ctx) => publishedResult(args, ctx, "rollback"),
  },

  set_password: {
    title: "Set Client Password",
    description:
      "Set or change a page's client password. Clearing remains human-admin-only. The result says whether the page is actually serving.",
    inputSchema: z.object({ slug: Slug, password: z.string().min(1).max(512) }).strict(),
    outputSchema: z
      .object({
        slug: z.string(),
        has_password: z.boolean(),
        page_is_live: z.boolean(),
        urls: UrlsSchema,
        next_step: z.string(),
      })
      .strict(),
    annotations: WriteAnnotations,
    handler: async (args, ctx) => {
      const result = await versions.setPassword({ slug: args.slug, password: args.password }, ctx);
      const page = (await versions.getPage(args.slug)).page;
      const isLive = !!page.published_version_id && !page.disabled;
      let nextStep;
      if (page.disabled) {
        nextStep = "Password saved, but the page is disabled and not serving. Ask a human admin to re-enable it before sharing urls.live.";
      } else if (!page.published_version_id) {
        nextStep = "Password saved, but no version is published. Publish or complete human approval before sharing urls.live.";
      } else {
        nextStep = "Client access is ready — share urls.live and the password with the client.";
      }
      return { ...result, page_is_live: isLive, urls: pageUrls(page.slug), next_step: nextStep };
    },
  },

  set_title: {
    title: "Set Page Title",
    description: "Change a page's display title without changing its URL, published version, or content.",
    inputSchema: z.object({ slug: Slug, title: z.string().min(1).max(200) }).strict(),
    outputSchema: z.object({ slug: z.string(), title: z.string() }).strict(),
    annotations: IdempotentWriteAnnotations,
    handler: (args, ctx) => versions.setTitle({ slug: args.slug, title: args.title }, ctx),
  },

  delete_page: {
    title: "Soft-delete Page",
    description:
      "Soft-delete an open page so it stops serving and the slug can be reused. Reversible by a human admin. Confirm with the user first.",
    inputSchema: z.object({ slug: Slug }).strict(),
    outputSchema: z
      .object({ slug: z.string(), deleted: z.boolean(), live: z.boolean(), page_is_live: z.boolean(), note: z.string() })
      .strict(),
    annotations: IdempotentWriteAnnotations,
    handler: async (args, ctx) => ({
      ...(await versions.deletePage({ slug: args.slug }, ctx)),
      live: false,
      page_is_live: false,
      note: "Soft-deleted; a human admin can restore it. The slug is free to reuse.",
    }),
  },

  page_urls: {
    title: "Get Page URLs",
    description:
      "Validate that a page exists and return its routing URLs: `live` is the client link to share, " +
      "`admin` and `view` are Elcano-staff-only.",
    inputSchema: z.object({ slug: Slug }).strict(),
    outputSchema: UrlsSchema,
    annotations: ReadAnnotations,
    handler: async (args) => {
      const result = await versions.getPage(args.slug);
      return pageUrls(result.page.slug);
    },
  },
};

module.exports = {
  TOOLS,
  pageUrls,
  encodeCursor,
  decodeCursor,
  assertMcpHtml,
  assertInlineHtml,
  assertInlineData,
  schemas: {
    UrlsSchema,
    PageSchema,
    PageSummarySchema,
    VersionSchema,
    VersionListSchema,
    VersionWithHtmlSchema,
    WorkspaceSchema,
    ThemeSchema,
    JsonObjectSchema,
    DataEnvelopeSchema,
  },
};
