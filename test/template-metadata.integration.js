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
  const initial = await templates.register({ name: "metadata-test", html, title: "Northwind", description: "Original" }, actor);
  const count = async () => Number((await db.query("SELECT count(*) FROM audit_log WHERE metadata->>'template' = $1", ["metadata-test"])).rows[0].count);
  const before = await count();
  const title = await templates.register({ name: "metadata-test", html, title: "Contoso" }, actor);
  assert.equal(await count(), before + 1, "a title-only change is a transactionally audited mutation");
  assert.equal(title.deduped, true);
  assert.equal(title.metadata_updated, true);
  assert.equal(title.revision.version_id, initial.revision.version_id);
  const description = await templates.register({ name: "metadata-test", html, description: "Revised" }, actor);
  assert.equal(await count(), before + 2, "a description-only change is audited once");
  const retry = await templates.register({ name: "metadata-test", html, title: "Contoso", description: "Revised" }, actor);
  assert.equal(retry.metadata_updated, false);
  assert.equal(await count(), before + 2);
  assert.equal(retry.template.updated_at, description.template.updated_at, "an identical retry is a genuine no-op");
  const audit = require("../lib/audit");
  const write = audit.write;
  audit.write = async () => { throw new Error("Synthetic audit failure"); };
  try {
    await assert.rejects(templates.register({ name: "metadata-test", html, title: "Rollback" }, actor), /Synthetic audit failure/);
  } finally { audit.write = write; }
  const stored = (await db.query("SELECT title, description FROM page_templates WHERE name = $1", ["metadata-test"])).rows[0];
  assert.deepEqual(stored, { title: "Contoso", description: "Revised" }, "audit failure rolls metadata back");
  console.log("✓ template metadata changes are audited atomically and exact retries are no-ops");
}
main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(() => db.pool.end());
