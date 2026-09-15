// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const db = require("../lib/db");
const tokens = require("../lib/tokens");
const versions = require("../lib/versions");
const templates = require("../lib/templates");
const { TOOLS } = require("../lib/mcp-tools");

const admin = { actor: "replay-admin", actorType: "user", transport: "admin" };
let actor;
async function call(name, args) {
  const tool = TOOLS[name];
  const result = await tool.handler(tool.inputSchema.parse(args), actor);
  return tool.outputSchema.parse(JSON.parse(JSON.stringify(result)));
}
async function stage(target, content) {
  const bytes = Buffer.from(content);
  const upload = await call("start_page_upload", {
    ...target, total_bytes: bytes.length,
    content_sha256: crypto.createHash("sha256").update(bytes).digest("hex"),
  });
  await call("append_page_upload", {
    upload_id: upload.upload_id, sequence: 0, chunk_base64: bytes.toString("base64"),
  });
  return upload.upload_id;
}
const html = (label) => `<!doctype html><html><head><title>Northwind</title></head><body>${label}</body></html>`;
const schema = (properties) => ({ $schema: "https://json-schema.org/draft/2020-12/schema", type: "object", additionalProperties: false, required: Object.keys(properties), properties });
const block = (id, data, type = "application/json") => `<script id="${id}" type="${type}">${JSON.stringify(data)}</script>`;
const managedHtml = html("Northwind").replace("</body>",
  block("pages-config-schema", schema({ title: { type: "string" } }), "application/schema+json") +
  block("pages-config", { title: "Northwind" }) +
  block("pages-data-schema", schema({ count: { type: "number" } }), "application/schema+json") +
  block("pages-data", { contract_version: 1, refreshed_at: "2026-08-01T00:00:00.000Z", source_as_of: "2026-08-01T00:00:00.000Z", data: { count: 0 } }) + "</body>");
async function snapshot(uploadId) {
  return (await db.query(`SELECT
    (SELECT count(*) FROM page_versions)::integer AS versions,
    (SELECT count(*) FROM page_template_versions)::integer AS revisions,
    (SELECT count(*) FROM audit_log)::integer AS audits,
    (SELECT row_to_json(u) FROM page_content_uploads u WHERE id = $1) AS upload`, [uploadId])).rows[0];
}
async function replay(name, args, original, state) {
  const before = await snapshot(args.upload_id);
  const result = await call(name, args);
  assert.deepEqual(await snapshot(args.upload_id), before, "replay cannot change a version, audit, upload receipt or expiry");
  for (const key of Object.keys(original)) {
    if (!["live", "version_is_live", "page_is_live", "live_version_id", "next_step", "replayed"].includes(key)) {
      assert.deepEqual(result[key], original[key], `${key} remains the original immutable commit receipt`);
    }
  }
  for (const [key, value] of Object.entries(state)) assert.equal(result[key], value, key);
  assert.equal(result.replayed, true);
  return result;
}
async function main() {
  const minted = await tokens.mint({ label: "upload-replay-agent", scope: "deploy" });
  actor = { actor: "upload-replay-agent", actorType: "agent", tokenId: minted.id, transport: "mcp" };

  const slug = "replay-page";
  const args = { upload_id: await stage({ slug }, html("original")), render_mode: "raw" };
  const first = await call("deploy_page_upload", args);
  const newer = await call("deploy_page", { slug, html: html("newer"), render_mode: "raw", publish: true });
  let result = await replay("deploy_page_upload", args, first, {
    live: false, version_is_live: false, page_is_live: true, live_version_id: newer.version.id,
  });
  assert.match(result.next_step, /did not republish/);
  assert.doesNotMatch(result.next_step, /share urls.live|Call rollback_page|Call publish_page/);
  await versions.rollback({ slug, versionId: first.version.id }, actor);
  result = await replay("deploy_page_upload", args, first, {
    live: true, version_is_live: true, page_is_live: true, live_version_id: first.version.id,
  });
  assert.match(result.next_step, /share urls.live/);
  await versions.setDisabled({ slug, disabled: true }, admin);
  result = await replay("deploy_page_upload", args, first, {
    live: false, version_is_live: false, page_is_live: false, live_version_id: first.version.id,
  });
  assert.match(result.next_step, /disabled/);
  await versions.setDisabled({ slug, disabled: false }, admin);
  await versions.deletePage({ slug }, actor);
  await call("deploy_page", { slug, html: html("reused slug"), render_mode: "raw", publish: true });
  result = await replay("deploy_page_upload", args, first, {
    live: false, version_is_live: false, page_is_live: false, live_version_id: null,
  });
  assert.match(result.next_step, /deleted/);
  await assert.rejects(call("deploy_page_upload", { ...args, publish: false }), { code: "page_upload_commit_conflict" });
  console.log("✓ page replay follows publication, rollback, disable and deletion without reusing a replacement page");

  const dataSlug = "replay-data";
  await templates.register({ name: "replay-data-design", html: managedHtml }, actor);
  const base = await call("create_page_from_template", { template: "replay-data-design", slug: dataSlug, config: { title: "Northwind" }, render_mode: "raw", publish: true });
  const dataArgs = { upload_id: await stage({ slug: dataSlug, kind: "data" }, JSON.stringify({ count: 1 })), slug: dataSlug, source_as_of: "2026-08-02T00:00:00.000Z", expected_version: base.version.id };
  const data = await call("update_page_data_upload", dataArgs);
  await templates.remove({ template: "replay-data-design", force: true }, actor);
  await replay("update_page_data_upload", dataArgs, data, { version_is_live: true, page_is_live: true, live_version_id: data.version.id });
  const newerData = await call("update_page_data", { slug: dataSlug, data: { count: 2 }, source_as_of: "2026-08-03T00:00:00.000Z", expected_version: data.version.id });
  result = await replay("update_page_data_upload", dataArgs, data, { version_is_live: false, page_is_live: true, live_version_id: newerData.version.id });
  assert.match(result.next_step, /get_page_data/);
  await versions.rollback({ slug: dataSlug, versionId: base.version.id }, actor);
  await replay("update_page_data_upload", dataArgs, data, { version_is_live: false, page_is_live: true, live_version_id: base.version.id });
  await versions.setDisabled({ slug: dataSlug, disabled: true }, admin);
  await replay("update_page_data_upload", dataArgs, data, { version_is_live: false, page_is_live: false, live_version_id: base.version.id });
  await versions.deletePage({ slug: dataSlug }, admin);
  result = await replay("update_page_data_upload", dataArgs, data, { version_is_live: false, page_is_live: false, live_version_id: null });
  assert.match(result.next_step, /deleted/);
  await assert.rejects(call("update_page_data_upload", { ...dataArgs, source_as_of: "2026-08-03T00:00:00.000Z" }), { code: "page_upload_commit_conflict" });
  console.log("✓ data replay preserves the committed envelope and profile while refreshing current serving state");

  const template = "replay-template";
  const templateArgs = { upload_id: await stage({ template }, managedHtml) };
  const registered = await call("register_template_upload", templateArgs);
  await templates.register({ name: template, html: managedHtml.replace("<title>Northwind</title>", "<title>Contoso</title>") }, actor);
  result = await replay("register_template_upload", templateArgs, registered, {});
  assert.match(result.next_step, /current revision is 2/);
  await templates.remove({ template }, actor);
  await templates.register({ name: template, html: managedHtml }, actor);
  result = await replay("register_template_upload", templateArgs, registered, {});
  assert.match(result.next_step, /retired/);
  assert.doesNotMatch(result.next_step, /Call create_page_from_template/);
  await assert.rejects(call("register_template_upload", { ...templateArgs, title: "Changed" }), { code: "page_upload_commit_conflict" });
  console.log("✓ template replay never re-registers a retired template or confuses a reused name with its original registration");
}
main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(() => db.pool.end());
