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

async function state() {
  return (await db.query(`SELECT published_version_id,
    (SELECT count(*) FROM page_versions WHERE page_id=p.id) AS versions,
    (SELECT count(*) FROM audit_log WHERE page_id=p.id) AS audits
    FROM pages p WHERE slug='migration-page'`)).rows[0];
}
async function main() {
  await templates.register({ name: "migration-design", html }, actor);
  const built = await templates.createPage({ template: "migration-design", slug: "migration-page", config, data: { count: 7 }, sourceAsOf: "2026-08-01T00:00:00Z" }, actor);
  const targetConfig = { campaign: "Northwind Spring", region: "North" };
  const targetHtml = html
    .replace(block("pages-config-schema", schema({ campaign: { type: "string" } }), "application/schema+json"), block("pages-config-schema", schema({ campaign: { type: "string" }, region: { type: "string" } }), "application/schema+json"))
    .replace(block("pages-config", config), block("pages-config", targetConfig))
    .replace(block("pages-data-schema", schema({ count: { type: "number" } }), "application/schema+json"), block("pages-data-schema", schema({ total: { type: "number" } }), "application/schema+json"))
    .replace('"data":{"count":0}', '"data":{"total":0}');
  const revision = await templates.register({ name: "migration-design", html: targetHtml }, actor);
  const args = { slug: "migration-page", revision: 2, config: targetConfig, data: { total: 7 }, sourceAsOf: "2026-08-01T00:00:00Z", expectedVersion: built.version.id };
  const before = await state();
  await assert.rejects(templates.rerenderPage({ slug: args.slug, revision: 2 }, actor), (e) => e.code === "config_validation_failed");
  await assert.rejects(versions.updatePageConfig({ slug: args.slug, config: targetConfig, expectedVersion: built.version.id }, actor), (e) => e.code === "config_validation_failed");
  await assert.rejects(templates.rerenderPage({ ...args, data: { total: "bad" } }, actor), (e) => e.code === "data_validation_failed");
  await assert.rejects(templates.rerenderPage({ ...args, expectedVersion: "1" }, actor), (e) => e.code === "stale_version");
  await assert.rejects(templates.rerenderPage({ ...args, expectedVersion: undefined }, actor), (e) => e.code === "expected_version_required");
  assert.deepEqual(await state(), before, "failed migrations do not change history, audit or the live pointer");
  const migrated = await templates.rerenderPage(args, actor);
  assert.equal(migrated.published, false);
  assert.equal(migrated.version.status, "draft");
  assert.equal(migrated.template_version_id, revision.revision.version_id);
  assert.deepEqual(migrated.config, targetConfig);
  assert.deepEqual(migrated.envelope.data, { total: 7 });
  const after = await state();
  assert.equal(after.published_version_id, before.published_version_id);
  assert.equal(Number(after.versions), Number(before.versions) + 1);
  assert.equal(Number(after.audits), Number(before.audits) + 1);
  const retry = await templates.rerenderPage(args, actor);
  assert.equal(retry.deduped, true);
  assert.equal(retry.version.id, migrated.version.id);
  assert.deepEqual(retry.envelope, migrated.envelope, "a retry reports stored freshness, not another generated timestamp");
  assert.equal(retry.html, migrated.html);
  const newer = await templates.rerenderPage({ ...args, data: { total: 9 }, sourceAsOf: "2026-08-03T00:00:00Z" }, actor);
  await assert.rejects(templates.rerenderPage({ ...args, sourceAsOf: "2026-08-02T00:00:00Z" }, actor), (e) => e.code === "source_regression");
  await versions.setApproval({ slug: args.slug, requireApproval: true }, { actor: "migration-reviewer", actorType: "user" });
  const gated = await templates.rerenderPage({ ...args, data: { total: 9 }, sourceAsOf: "2026-08-03T00:00:00Z", publish: true }, actor);
  assert.equal(gated.version.status, "pending");
  assert.equal(gated.published, false);
  assert.equal((await state()).published_version_id, built.version.id);
  // A config-schema migration supplies no new data, so it retains freshness.
  await templates.register({ name: "config-migration-design", html }, actor);
  const configBuilt = await templates.createPage({ template: "config-migration-design", slug: "config-migration-page", config, data: { count: 11 }, sourceAsOf: "2026-08-01T00:00:00Z" }, actor);
  const configTargetHtml = html
    .replace(block("pages-config-schema", schema({ campaign: { type: "string" } }), "application/schema+json"), block("pages-config-schema", schema({ campaign: { type: "string" }, region: { type: "string" } }), "application/schema+json"))
    .replace(block("pages-config", config), block("pages-config", targetConfig));
  await templates.register({ name: "config-migration-design", html: configTargetHtml }, actor);
  const configMigrated = await templates.rerenderPage({ slug: "config-migration-page", config: targetConfig, expectedVersion: configBuilt.version.id }, actor);
  const initialHtml = (await db.query("SELECT html FROM page_versions WHERE id = $1", [configBuilt.version.id])).rows[0].html;
  const pageData = require("../lib/page-data");
  assert.deepEqual(configMigrated.envelope, pageData.parseManaged(initialHtml, pageData.TEMPLATE_SPEC).envelope);
  console.log("✓ target-shaped template migrations are atomic drafts with truthful provenance and freshness");
}
main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(() => db.pool.end());
