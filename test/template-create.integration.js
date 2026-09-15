// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Drive the actual transaction/unique-index race behind template creation.
// Both transactions must read an absent slug before either may insert it.
// The barrier delays real query results; it never invents database state.

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

async function raceMissingSlug(slug, calls) {
  const original = db.withTransaction;
  const arrivals = new Set();
  let release;
  let fail;
  const ready = new Promise((resolve, reject) => { release = resolve; fail = reject; });
  const timeout = setTimeout(() => fail(new Error("both creates did not reach the missing-slug barrier")), 4000);
  db.withTransaction = (fn) => original((client) => fn({
    async query(sql, params) {
      const result = await client.query(sql, params);
      if (params?.[0] === slug && /FROM pages WHERE slug = \$1 AND deleted_at IS NULL FOR UPDATE/.test(sql)
          && !arrivals.has(client.processID)) {
        assert.equal(result.rows.length, 0, "the real database reported an absent slug");
        arrivals.add(client.processID);
        if (arrivals.size === 2) release();
        await ready;
      }
      return result;
    },
  }));
  try {
    const results = await Promise.allSettled(calls.map((call) => call()));
    assert.equal(arrivals.size, 2, "two independent database connections reached the barrier");
    return results;
  } finally {
    clearTimeout(timeout);
    release();
    db.withTransaction = original;
  }
}

async function state(slug) {
  return (await db.query(
    `SELECT p.id, p.published_version_id,
            (SELECT count(*)::integer FROM page_versions WHERE page_id = p.id) AS versions,
            (SELECT count(*)::integer FROM audit_log WHERE page_id = p.id) AS audits
       FROM pages p WHERE p.slug = $1 AND p.deleted_at IS NULL`,
    [slug]
  )).rows[0];
}

async function main() {
  await templates.register({ name: "create-race-template", html }, actor);

  for (const mode of ["published", "draft", "gated"]) {
    const options = { publish: mode !== "draft", requireApproval: mode === "gated" };
    const slug = `template-create-conflict-${mode}`;
    const args = { template: "create-race-template", slug, config, ...options };
    const results = await raceMissingSlug(slug, [
      () => templates.createPage(args, actor),
      () => templates.createPage({ ...args, config: { campaign: "Contoso Summer" } }, actor),
    ]);
    const successes = results.filter((result) => result.status === "fulfilled");
    const failures = results.filter((result) => result.status === "rejected");
    assert.equal(successes.length, 1, `${mode}: only one different build may succeed`);
    assert.equal(failures.length, 1);
    assert.equal(failures[0].reason.code, "page_exists");
    assert.equal(failures[0].reason.status, 409);
    assert.equal(successes[0].value.created, true);
    const stored = await state(slug);
    assert.equal(stored.versions, 1, "a rejected race leaves no second version");
    assert.equal(stored.audits, mode === "published" ? 3 : 2, "only the winner writes audit rows");
    assert.equal(stored.published_version_id, mode === "published" ? successes[0].value.version.id : null);
    console.log(`✓ concurrent different ${mode} template creates return one winner and page_exists`);

    const retrySlug = `template-create-retry-${mode}`;
    const retryArgs = { ...args, slug: retrySlug };
    const retries = await raceMissingSlug(retrySlug, [
      () => templates.createPage({ ...retryArgs, now: Date.parse("2026-08-01T00:00:00Z") }, actor),
      () => templates.createPage({ ...retryArgs, now: Date.parse("2026-08-02T00:00:00Z") }, actor),
    ]);
    assert.ok(retries.every((result) => result.status === "fulfilled"), JSON.stringify(retries));
    const [first, second] = retries.map((result) => result.value);
    assert.equal(first.version.id, second.version.id, "timestamps do not turn an exact retry into a new build");
    assert.equal([first, second].filter((result) => result.created).length, 1);
    assert.equal([first, second].filter((result) => result.deduped).length, 1);
    const retryState = await state(retrySlug);
    assert.equal(retryState.versions, 1);
    assert.equal(retryState.audits, mode === "published" ? 3 : 2, "an exact retry writes no extra audit rows");
    assert.equal(retryState.published_version_id, mode === "published" ? first.version.id : null);

    if (mode === "published") {
      const refreshed = await versions.updatePageData({
        slug: retrySlug,
        data: { count: 42 },
        sourceAsOf: "2026-08-03T00:00:00Z",
        expectedVersion: first.version.id,
      }, actor);
      const beforeReplay = await state(retrySlug);
      await assert.rejects(() => templates.createPage(retryArgs, actor), { code: "page_exists" });
      assert.deepEqual(await state(retrySlug), beforeReplay, "a historical retry cannot undo a refresh or write an audit row");
      assert.equal(beforeReplay.published_version_id, refreshed.version.id);
    }
    console.log(`✓ concurrent identical ${mode} template creates reuse one version and preserve publication gates`);
  }

  const slug = "template-create-ordinary-upsert";
  const writes = await raceMissingSlug(slug, [1, 2].map((number) => () => versions.createAndDeploy({
    slug,
    html: `<!doctype html><html><body>Northwind ${number}</body></html>`,
    publish: true,
  }, actor)));
  assert.ok(writes.every((result) => result.status === "fulfilled"), JSON.stringify(writes));
  assert.equal(writes.filter((result) => result.value.created).length, 1);
  assert.equal((await state(slug)).versions, 2, "ordinary deploy_page remains create-or-update");
  console.log("✓ ordinary concurrent deploys retain their create-or-update contract");
}

main().catch((error) => {
  console.error(error);
  process.exitCode = 1;
}).finally(() => db.pool.end());
