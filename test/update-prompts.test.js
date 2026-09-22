// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The EXECUTION REQUIREMENTS contract a prepared managed-data prompt hands a
// scheduler, and the commit tool preparation pins from the live payload size.
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

test("a large recurring page commits by staged upload, with a roster of ten and the contract a scheduler enforces", async () => {
  const data = payloadOfBytes(990_000);
  assert.ok(Buffer.byteLength(JSON.stringify(data)) >= 990_000);
  const prepared = await prepare(data, { recurring: true });
  assert.equal(prepared.mode, "managed_data");
  assert.deepEqual(prepared.execution_requirements, {
    mcp_servers: ["fast_io", "fastio_helpers", "pages"],
    required_tools: [
      "download_url",
      "mcp_fast_io_download",
      "mcp_fastio_helpers_resolve_path",
      "mcp_pages_append_page_upload",
      "mcp_pages_get_page_config",
      "mcp_pages_get_page_data",
      "mcp_pages_preflight_page",
      "mcp_pages_record_refresh_check",
      "mcp_pages_start_page_upload",
      "mcp_pages_update_page_data_upload",
    ],
    roster: "required_tools_only",
    completion: { any_succeeded: ["mcp_pages_record_refresh_check", "mcp_pages_update_page_data_upload"] },
    serialization_key: "pages:northwind/overview",
    network: false,
    model_required: true,
    mode: "managed_data",
  });
  assert.ok(prepared.execution_requirements.required_tools.length <= 10);
  assert.deepEqual(embeddedRequirements(prepared.prompt), prepared.execution_requirements, "the prompt carries the same contract");

  const lines = prepared.prompt.split("\n");
  assert.equal(lines[lines.findIndex((line) => line.startsWith("PUBLISH: ")) + 1], "COMMIT TOOL: mcp_pages_update_page_data_upload");
  assert.match(step(prepared.prompt, 9), /^9\. Stage the complete data file for the COMMIT TOOL: /);
  assert.ok(step(prepared.prompt, 9).includes(fileUploadGuidance("data", "mcp_pages_")), "by-reference upload guidance is kept");
  assert.match(step(prepared.prompt, 10), /^10\. Commit with mcp_pages_update_page_data_upload — the COMMIT TOOL/);
  assert.match(step(prepared.prompt, 10), /declare exactly mcp_pages_update_page_data_upload as the update-branch mutation; never declare or call another commit tool/);
  assert.doesNotMatch(prepared.prompt, /A smaller object may use mcp_pages_update_page_data inline/);
  assert.doesNotMatch(prepared.prompt, /Over 20,000 UTF-8 bytes/, "the run no longer chooses its transport");
  // Every Pages tool the roster offers exists, and every one the prompt names is offered.
  for (const tool of prepared.execution_requirements.required_tools.filter((name) => name.startsWith("mcp_pages_"))) {
    assert.ok(TOOLS[tool.replace(/^mcp_pages_/, "")], `${tool} is a real Pages tool`);
  }
});

test("a small recurring page commits inline and offers no upload tools", async () => {
  const prepared = await prepare(payloadOfBytes(2_000), { recurring: true });
  const requirements = prepared.execution_requirements;
  assert.deepEqual(requirements.required_tools, [
    "download_url",
    "mcp_fast_io_download",
    "mcp_fastio_helpers_resolve_path",
    "mcp_pages_get_page_config",
    "mcp_pages_get_page_data",
    "mcp_pages_preflight_page",
    "mcp_pages_record_refresh_check",
    "mcp_pages_update_page_data",
  ]);
  assert.deepEqual(requirements.completion, { any_succeeded: ["mcp_pages_record_refresh_check", "mcp_pages_update_page_data"] });
  assert.equal(requirements.roster, "required_tools_only");
  assert.equal(requirements.serialization_key, "pages:northwind/overview");
  assert.deepEqual(embeddedRequirements(prepared.prompt), requirements);
  assert.match(prepared.prompt, /\nCOMMIT TOOL: mcp_pages_update_page_data\n/);
  assert.match(step(prepared.prompt, 9), /^9\. Send the complete object inline to the COMMIT TOOL/);
  assert.match(step(prepared.prompt, 9), /select blocked, keep the file, and report that this prompt must be prepared again/);
  assert.match(step(prepared.prompt, 10), /declare exactly mcp_pages_update_page_data as the update-branch mutation/);
  assert.doesNotMatch(prepared.prompt, /start_page_upload|append_page_upload|update_page_data_upload/);
});

test("the commit tool follows the live payload size at the existing 20,000-byte line", () => {
  assert.equal(updatePrompts.INLINE_DATA_MAX_BYTES, 20000);
  assert.equal(updatePrompts.commitToolFor(20000), "mcp_pages_update_page_data");
  assert.equal(updatePrompts.commitToolFor(20001), "mcp_pages_update_page_data_upload");
  assert.equal(updatePrompts.commitToolFor(0), "mcp_pages_update_page_data");
  for (const unknown of [undefined, null, NaN, -1.5]) {
    assert.equal(updatePrompts.commitToolFor(unknown), "mcp_pages_update_page_data_upload", "unknown size stages");
  }
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
  assert.doesNotMatch(oneTime, /COMMIT TOOL/);

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
