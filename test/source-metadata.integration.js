// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const assert = require("node:assert/strict");
const db = require("../lib/db");
const templates = require("../lib/templates");
const versions = require("../lib/versions");

const actor = { actor: "template-create-test", actorType: "agent", transport: "mcp" };
const config = { campaign: "Northwind Spring" };
const schema = (properties) => ({
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  additionalProperties: false,
  required: Object.keys(properties),
  properties,
});
const block = (id, value, type = "application/json") =>
  `<script id="${id}" type="${type}">${JSON.stringify(value)}</script>`;
const html = "<!doctype html><html><head><title>Northwind</title></head><body><h1>Campaign</h1>" +
  block("pages-config-schema", schema({ campaign: { type: "string" } }), "application/schema+json") +
  block("pages-config", config) +
  block("pages-data-schema", schema({ count: { type: "number" } }), "application/schema+json") +
  block("pages-data", {
    contract_version: 1,
    refreshed_at: "2026-08-01T00:00:00.000Z",
    source_as_of: "2026-08-01T00:00:00.000Z",
    data: { count: 0 },
  }) + "</body></html>";

const pageData = require("../lib/page-data");
const { TOOLS } = require("../lib/mcp-tools");
async function indexed(id) {
  const row = (await db.query("SELECT * FROM page_versions WHERE id = $1", [id])).rows[0];
  const parsed = pageData.parseManagedHtml(row.html);
  assert.equal(row.data_sha256, parsed.data_sha256);
  assert.equal(row.data_template_sha256, parsed.template_sha256);
  assert.equal(row.refreshed_at.toISOString(), new Date(parsed.envelope.refreshed_at).toISOString());
  assert.equal(row.source_as_of.toISOString(), new Date(parsed.envelope.source_as_of).toISOString());
  assert.equal(row.template_version_id, null);
  return parsed;
}
async function main() {
  const token = await require("../lib/tokens").mint({ label: "source-metadata-fixture", scope: "deploy" });
  const ctx = { ...actor, tokenId: token.id };
  const tool = async (name, args) => {
    const definition = TOOLS[name];
    const result = JSON.parse(JSON.stringify(await definition.handler(definition.inputSchema.parse(args), ctx)));
    definition.outputSchema.parse(result);
    return result;
  };
  const inline = await tool("deploy_page", { slug: "source-metadata-page", html });
  await indexed(inline.version.id);
  const patched = await tool("patch_page", { slug: "source-metadata-page", edits: [{ find: "<h1>Campaign</h1>", replace: "<h1>Revised campaign</h1>" }] });
  await indexed(patched.version.id);
  const bytes = Buffer.from(html.replaceAll("2026-08-01", "2026-08-03"));
  const upload = await tool("start_page_upload", { slug: "source-metadata-page", total_bytes: bytes.length, content_sha256: versions.sha256(bytes) });
  await tool("append_page_upload", { upload_id: upload.upload_id, sequence: 0, chunk_base64: bytes.toString("base64") });
  const staged = await tool("deploy_page_upload", { upload_id: upload.upload_id, publish: false });
  await indexed(staged.version.id);
  await assert.rejects(versions.updatePageData({ slug: "source-metadata-page", data: { count: 2 }, sourceAsOf: "2026-08-02T00:00:00Z", expectedVersion: patched.version.id }, ctx), (e) => e.code === "source_regression");
  const listed = (await versions.listPages()).find((row) => row.slug === "source-metadata-page");
  assert.deepEqual(listed.freshness, (await versions.getPageData("source-metadata-page")).freshness);

  const timestampOnly = html.replace('"refreshed_at":"2026-08-01', '"refreshed_at":"2026-08-02');
  const changed = await tool("deploy_page", { slug: "source-byte-dedupe", html });
  const changedTime = await tool("deploy_page", { slug: "source-byte-dedupe", html: timestampOnly });
  assert.notEqual(changedTime.version.id, changed.version.id, "full-source writes retain byte identity even when only an envelope timestamp changes");
  assert.equal((await tool("deploy_page", { slug: "source-byte-dedupe", html: timestampOnly })).version.id, changedTime.version.id);
  const managedRetry = await versions.updatePageData({ slug: "source-byte-dedupe", data: { count: 0 }, sourceAsOf: "2026-08-01T00:00:00Z", expectedVersion: changedTime.version.id }, ctx);
  assert.equal(managedRetry.version.id, changedTime.version.id, "managed retries can see indexed full-source versions");

  // Simulate pre-fix immutable rows without mutating any stored version.
  const legacyPage = await versions.createPage({ slug: "legacy-source-metadata" }, ctx);
  const legacy = (await db.query(`INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author,source)
    VALUES($1,$2,$3,'approved','themed','source-fixture','mcp') RETURNING id`, [legacyPage.id, html, versions.sha256(html)])).rows[0];
  await db.query("UPDATE pages SET published_version_id=$1 WHERE id=$2", [legacy.id, legacyPage.id]);
  const legacyListed = (await versions.listPages()).find((row) => row.slug === "legacy-source-metadata");
  assert.deepEqual(legacyListed.freshness, (await versions.getPageData("legacy-source-metadata")).freshness, "legacy full-source pages are readable without rewriting immutable rows");
  const checked = await versions.recordRefreshCheck({ slug: "legacy-source-metadata", outcome: "source_not_updated", sourceAsOfSeen: "2026-08-01T00:00:00Z" }, ctx);
  assert.equal(checked.freshness.source_as_of, "2026-08-01T00:00:00.000Z");
  await db.query(`INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author,source)
    VALUES($1,$2,$3,'draft','themed','source-fixture','mcp')`, [legacyPage.id, bytes.toString(), versions.sha256(bytes)]);
  await assert.rejects(versions.updatePageData({ slug: "legacy-source-metadata", data: { count: 2 }, sourceAsOf: "2026-08-02T00:00:00Z", expectedVersion: legacy.id }, ctx), (e) => e.code === "source_regression");
  assert.equal((await db.query("SELECT data_sha256 FROM page_versions WHERE id=$1", [legacy.id])).rows[0].data_sha256, null);
  const plain = await tool("deploy_page", { slug: "plain-source-metadata", html: "<!doctype html><html><body><h1>Plain</h1></body></html>" });
  assert.equal((await db.query("SELECT data_sha256 FROM page_versions WHERE id=$1", [plain.version.id])).rows[0].data_sha256, null);
  assert.equal((await versions.listPages()).find((row) => row.slug === "plain-source-metadata").freshness, null);
  await templates.register({ name: "source-detachment-design", html }, ctx);
  const bound = await templates.createPage({ template: "source-detachment-design", slug: "source-detachment-page", config, data: { count: 0 }, sourceAsOf: "2026-08-01T00:00:00Z" }, ctx);
  const boundHtml = (await db.query("SELECT html FROM page_versions WHERE id=$1", [bound.version.id])).rows[0].html;
  const detached = await versions.deploy({ slug: "source-detachment-page", html: boundHtml, publish: false }, ctx);
  assert.notEqual(detached.version.id, bound.version.id, "indexed full source must not dedupe into an old template binding");
  await indexed(detached.version.id);
  console.log("✓ inline, staged, patched and legacy full-source pages report indexed, truthful metadata");
}
main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(() => db.pool.end());
