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

async function main() {
  const a = await templates.register({ name: "provenance-a", html }, actor);
  const b = await templates.register({ name: "provenance-b", html }, actor);
  const built = await templates.createPage({ template: "provenance-a", slug: "provenance-page", config, data: { count: 7 }, sourceAsOf: "2026-08-01T00:00:00Z" }, actor);
  const moved = await templates.rerenderPage({ slug: "provenance-page", template: "provenance-b" }, actor);
  assert.equal(moved.deduped, false, "identical materialized bytes with a different binding require a new immutable version");
  assert.notEqual(moved.version.id, built.version.id);
  const persisted = (await db.query("SELECT template_version_id FROM page_versions WHERE id = $1", [moved.version.id])).rows[0];
  assert.equal(String(persisted.template_version_id), String(b.revision.id));
  assert.equal(moved.template_version_id, String(persisted.template_version_id));
  const retry = await templates.rerenderPage({ slug: "provenance-page", template: "provenance-b" }, actor);
  assert.equal(retry.deduped, true);
  assert.equal(retry.version.id, moved.version.id, "an exact target-binding retry still dedupes");
  const revision = await templates.register({ name: "provenance-a", html: html.replace("Northwind Spring", "Reference only") }, actor);
  const revised = await templates.rerenderPage({ slug: "provenance-page", revision: 2 }, actor);
  assert.equal(revised.deduped, false, "a revision with only reference-config changes still records provenance");
  assert.equal(revised.template_version_id, String(revision.revision.id));
  assert.deepEqual(revised.config, config, "reference config is never implicitly inherited");
  const live = (await db.query("SELECT published_version_id FROM pages WHERE slug = $1", ["provenance-page"])).rows[0];
  assert.equal(String(live.published_version_id), String(built.version.id), "binding changes remain drafts");
  console.log("✓ template identity includes immutable binding and exact retries remain idempotent");
}
main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(() => db.pool.end());
