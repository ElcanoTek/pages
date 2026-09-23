// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The EXECUTION REQUIREMENTS contract a prepared managed-data prompt hands a
// scheduler.
// No database: prepare() reads the page through versions/templates, stubbed
// here with what they return for a published managed page.

const { test } = require("node:test");
const assert = require("node:assert/strict");
const crypto = require("node:crypto");

process.env.RAW_TOKEN_SECRET = "test-secret-do-not-use-in-prod";
process.env.DASHBOARD_HOST = "pages.elcanotek.com";
process.env.CONTENT_HOST = "elcano-pages.com";

const versions = require("../lib/versions");
const templates = require("../lib/templates");
const updatePrompts = require("../lib/update-prompts");
const { fileUploadGuidance } = require("../lib/upload-guidance");
const { TOOLS } = require("../lib/mcp-tools");

const SLUG = "northwind/overview";
// Bindings shaped like a production SSP refresh: a path resolver and a download.
const SOURCES = [
  { source_id: "ssp_history", mcp_server: "fastio_helpers", path: "Storage/northwind_ssp_history.csv", required_tools: ["mcp_fastio_helpers_resolve_path"] },
  { source_id: "ssp_download", mcp_server: "fast_io", required_tools: ["mcp_fast_io_download", "download_url"] },
];

function payloadOfBytes(bytes) {
  const data = { dataThrough: "2026-09-20", rows: [] };
  let size = Buffer.byteLength(JSON.stringify(data));
  for (let i = 0; size < bytes; i++) {
    const row = [`d${i % 40}`, "2026-09-01", 1000.25, 310.5, 12.75, 150000 + i, 300];
    data.rows.push(row);
    size += Buffer.byteLength(JSON.stringify(row)) + (data.rows.length > 1 ? 1 : 0);
  }
  return data;
}

async function prepare(data, args) {
  const saved = { getPage: versions.getPage, getPageData: versions.getPageData, binding: templates.pageTemplateBinding };
  versions.getPage = async () => ({
    page: { id: "17", slug: SLUG, title: "Northwind", published_version_id: "42", disabled: false, require_approval: false },
    published: { id: "42" },
  });
  versions.getPageData = async () => ({ schema_sha256: "a".repeat(64), envelope: { contract_version: 1, data } });
  templates.pageTemplateBinding = async () => null;
  try {
    const result = await updatePrompts.prepare({ slug: SLUG, instructions: "Refresh from the Northwind export.", sources: SOURCES, ...args });
    // The advertised tool output schema accepts the block exactly as prepared.
    TOOLS.prepare_dashboard_update.outputSchema.shape.execution_requirements.parse(result.execution_requirements);
    return result;
  } finally {
    versions.getPage = saved.getPage;
    versions.getPageData = saved.getPageData;
    templates.pageTemplateBinding = saved.binding;
  }
}

function embeddedRequirements(prompt) {
  const lines = prompt.split("\n");
  return JSON.parse(lines[lines.indexOf("EXECUTION REQUIREMENTS (JSON):") + 1]);
}

const step = (prompt, n) => prompt.split("\n").find((line) => line.startsWith(`${n}. `));

const BOTH_PATHS_REQUIRED_TOOLS = [
  "download_url",
  "mcp_fast_io_download",
  "mcp_fastio_helpers_resolve_path",
  "mcp_pages_append_page_upload",
  "mcp_pages_get_page_config",
  "mcp_pages_get_page_data",
  "mcp_pages_preflight_page",
  "mcp_pages_record_refresh_check",
  "mcp_pages_start_page_upload",
  "mcp_pages_update_page_data",
  "mcp_pages_update_page_data_upload",
];

// fleet's EXECUTION REQUIREMENTS parser (internal/scheduledrun/requirements.go).
const FLEET_REQUIREMENT_NAME = /^[a-zA-Z0-9_.-]{1,200}$/;

test("a recurring prompt lists both commit transports whatever the payload size at preparation", async () => {
  // A recurring prompt runs for months against a payload that grows. Were the
  // transport pinned from today's size, a small page that later passed the
  // inline line would be locked out of the upload tools by a narrowed roster.
  for (const bytes of [2_000, 19_000, 990_000]) {
    const prepared = await prepare(payloadOfBytes(bytes), { recurring: true });
    assert.equal(prepared.mode, "managed_data");
    assert.deepEqual(prepared.execution_requirements, {
      mcp_servers: ["fast_io", "fastio_helpers", "pages"],
      required_tools: BOTH_PATHS_REQUIRED_TOOLS,
      roster: "required_tools_only",
      completion: {
        any_succeeded: ["mcp_pages_record_refresh_check", "mcp_pages_update_page_data", "mcp_pages_update_page_data_upload"],
      },
      serialization_key: "pages:northwind/overview",
      network: false,
      model_required: true,
      mode: "managed_data",
    }, `${bytes} bytes`);
    assert.deepEqual(embeddedRequirements(prepared.prompt), prepared.execution_requirements, "the prompt carries the same contract");
    assert.doesNotMatch(prepared.prompt, /COMMIT TOOL:/, "no transport is pinned at preparation");
    // The run still chooses by the size of the file it built, on every run.
    assert.match(step(prepared.prompt, 9), /^9\. Over 20,000 UTF-8 bytes, stage the complete data file\. /);
    assert.ok(step(prepared.prompt, 9).includes(fileUploadGuidance("data", "mcp_pages_")), "by-reference upload guidance is kept");
    assert.match(prepared.prompt, /\nROSTER: a scheduler may offer this run only required_tools\. Both commit tools are listed: decide the transport in step 9 from the size of the file you built on this run, every run/);
  }
});

test("every requirement is one a strict scheduler accepts, and covers every tool a branch may call", async () => {
  const prepared = await prepare(payloadOfBytes(990_000), { recurring: true });
  const requirements = prepared.execution_requirements;
  for (const name of [...requirements.mcp_servers, ...requirements.required_tools]) {
    assert.match(name, FLEET_REQUIREMENT_NAME, name);
  }
  // Completion names resolve against the narrowed roster, so each must be in it.
  for (const tool of requirements.completion.any_succeeded) assert.ok(requirements.required_tools.includes(tool), tool);
  // Every Pages tool the prompt text tells the run to call is offered. The one
  // exception is create_upload_ticket: the shared upload guidance names it as an
  // alternative for a client with file HTTP, and it must stay OUT of
  // required_tools, because a scheduler that does not expose it (Fleet) would
  // refuse the whole task at dispatch.
  const named = new Set(prepared.prompt.match(/mcp_pages_[a-z_]+/g));
  assert.ok(named.delete("mcp_pages_create_upload_ticket"));
  assert.equal(requirements.required_tools.includes("mcp_pages_create_upload_ticket"), false);
  for (const tool of named) assert.ok(requirements.required_tools.includes(tool), `${tool} is named by the prompt but not in required_tools`);
  // And every Pages tool offered exists.
  for (const tool of requirements.required_tools.filter((name) => name.startsWith("mcp_pages_"))) {
    assert.ok(TOOLS[tool.replace(/^mcp_pages_/, "")], `${tool} is a real Pages tool`);
  }
});

test("a binding whose server or tool a scheduler cannot parse is refused at preparation", async () => {
  // fleet dead-lettered tasks whose prepared block carried "fast_io + fastio_helpers".
  for (const bad of [
    { source_id: "ssp", mcp_server: "fast_io + fastio_helpers" },
    { source_id: "ssp", mcp_server: "fast_io", required_tools: ["mcp_fast_io_download download_url"] },
    { source_id: "ssp", mcp_server: "fast_io", required_tools: ["mcp_fast_io_download,download_url"] },
  ]) {
    assert.throws(() => updatePrompts.normalizeSources([bad]), (err) => err.code === "update_sources_invalid", JSON.stringify(bad));
  }
  assert.deepEqual(updatePrompts.normalizeSources([{ source_id: "ssp", mcp_server: "fast-io.v2", required_tools: ["mcp_fast_io_download"] }]), [
    { source_id: "ssp", mcp_server: "fast-io.v2", required_tools: ["mcp_fast_io_download"] },
  ]);
  // The legacy workflow read never fails; it drops what it cannot hand on.
  assert.deepEqual(
    updatePrompts.sourcesFromWorkflow({
      sources: [
        { source_id: "combined", mcp_server: "fast_io + fastio_helpers" },
        { source_id: "ssp", mcp_server: "fast_io", required_tools: ["mcp_fast_io_download", "two tools"] },
      ],
    }),
    [{ source_id: "ssp", mcp_server: "fast_io", required_tools: ["mcp_fast_io_download"] }]
  );
});

// Golden hashes of the pre-#102 prompt text (generated from the unchanged main
// branch), with the requirements line and the shared upload guidance masked so
// only this module's own wording is pinned. A one-time prompt is supervised and
// keeps choosing its transport; only its requirements block changed.
const ONE_TIME_GOLDEN = "0b70280b3a8c4c35d0eb1a52f4e875fbadc0feea573c46a7d5c03ad78aeb97eb";
const ADAPTIVE_GOLDEN = "a59fd538d4c3bd7f5e3ef26adfb32e1cf93b8d641d39751dae57f3f6cdd36111";

function masked(prompt) {
  const lines = prompt.split("\n");
  const at = lines.indexOf("EXECUTION REQUIREMENTS (JSON):");
  if (at >= 0) lines.splice(at + 1, 1, "<REQUIREMENTS>");
  return lines.join("\n").split(fileUploadGuidance("data", "mcp_pages_")).join("<UPLOAD GUIDANCE>");
}
const sha256 = (text) => crypto.createHash("sha256").update(text).digest("hex");

test("one-time managed and adaptive prompts are unchanged apart from their requirements block", async () => {
  const sources = updatePrompts.normalizeSources(SOURCES);
  const common = { slug: SLUG, instructions: "Refresh from the Northwind export.", schemaSha256: "a".repeat(64), sources };
  const oneTime = updatePrompts.managedPrompt({ ...common, publish: true, recurring: false });
  assert.equal(sha256(masked(oneTime)), ONE_TIME_GOLDEN);
  assert.equal(sha256(masked(updatePrompts.adaptivePrompt({ ...common, liveVersionId: "42", publish: false }))), ADAPTIVE_GOLDEN);
  assert.doesNotMatch(oneTime, /COMMIT TOOL|ROSTER:/);

  // Its block gains the new keys but keeps both commit paths, because the text
  // still lets a supervised run pick inline or staged by size.
  const requirements = embeddedRequirements(oneTime);
  for (const tool of ["mcp_pages_update_page_data", "mcp_pages_update_page_data_upload", "mcp_pages_start_page_upload", "mcp_pages_append_page_upload"]) {
    assert.ok(requirements.required_tools.includes(tool), tool);
  }
  assert.deepEqual(requirements.completion.any_succeeded, ["mcp_pages_record_refresh_check", "mcp_pages_update_page_data", "mcp_pages_update_page_data_upload"]);
  assert.equal(requirements.serialization_key, "pages:northwind/overview");

  // Preparing a one-time data update pins nothing, whatever the payload size.
  const prepared = await prepare(payloadOfBytes(990_000), { recurring: false, updateType: "data" });
  assert.deepEqual(prepared.execution_requirements, requirements);
  assert.doesNotMatch(prepared.prompt, /COMMIT TOOL/);
});

test("modes other than managed data carry no roster, completion or serialization key", () => {
  for (const mode of ["full_page", "adaptive", "managed_template", "migration_required"]) {
    const requirements = updatePrompts.executionRequirements(null, mode, { slug: SLUG });
    for (const key of ["roster", "completion", "serialization_key"]) assert.equal(Object.hasOwn(requirements, key), false, `${mode} ${key}`);
    assert.deepEqual(requirements.required_tools, []);
  }
});
