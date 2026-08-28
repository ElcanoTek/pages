// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// The file-backed template path, against a real database: the one registration
// route that involves no agent and no model output. `pages template sync` is what
// a deploy runs, so its guarantees are load-bearing —
//
//   * unchanged bytes are a NO-OP (a deploy re-runs it every time),
//   * an edited design becomes revision N+1 and moves nothing deployed,
//   * a broken template is reported and SKIPPED rather than aborting the rest,
//   * an explicit path + name works, so a client bundle can supply a template.
//
// Driven as a subprocess, because the exit code and the operator-facing output are
// part of the contract update.sh depends on.

const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const ROOT = path.join(__dirname, "..");
const CLI = path.join(ROOT, "scripts", "template.js");

function run(...args) {
  const result = spawnSync(process.execPath, [CLI, ...args], {
    cwd: ROOT,
    encoding: "utf8",
    env: process.env,
  });
  return { status: result.status, out: `${result.stdout}${result.stderr}` };
}

const CONFIG_SCHEMA = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  additionalProperties: false,
  required: ["campaign"],
  properties: { campaign: { type: "string", minLength: 1 } },
};
const DATA_SCHEMA = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  additionalProperties: false,
  required: ["rows"],
  properties: { rows: { type: "array", items: { type: "object", additionalProperties: false } } },
};

function templateHtml(marker) {
  const envelope = {
    contract_version: 1,
    refreshed_at: "2026-08-01T00:00:00.000Z",
    source_as_of: "2026-08-01T00:00:00.000Z",
    data: { rows: [] },
  };
  return (
    '<!doctype html><html><head><meta charset="utf-8"><title>CLI</title></head><body>' +
    `<h1 id="t"></h1><div id="rev">${marker}</div>` +
    `<script type="application/schema+json" id="pages-config-schema">${JSON.stringify(CONFIG_SCHEMA)}</script>` +
    `<script type="application/json" id="pages-config">${JSON.stringify({ campaign: "Reference" })}</script>` +
    `<script type="application/schema+json" id="pages-data-schema">${JSON.stringify(DATA_SCHEMA)}</script>` +
    `<script type="application/json" id="pages-data">${JSON.stringify(envelope)}</script>` +
    "<script>\n" +
    "const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);\n" +
    "const DATA = JSON.parse(document.getElementById('pages-data').textContent).data;\n" +
    "document.getElementById('t').textContent = CONFIG.campaign;\n" +
    "</script></body></html>"
  );
}

(async () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "pages-tplcli-"));
  const dir = path.join(root, "cli-smoke");
  fs.mkdirSync(dir);
  const htmlPath = path.join(dir, "template.html");
  fs.writeFileSync(htmlPath, templateHtml("DESIGN_V1"));
  fs.writeFileSync(path.join(dir, "template.json"), JSON.stringify({ title: "CLI Smoke", description: "Fixture." }));

  const created = run("sync", root);
  assert.equal(created.status, 0, created.out);
  assert.match(created.out, /cli-smoke: created \(revision 1/, created.out);

  // A deploy re-runs sync every time; unchanged bytes must not invent revisions.
  const again = run("sync", root);
  assert.equal(again.status, 0, again.out);
  assert.match(again.out, /cli-smoke: unchanged \(revision 1/, again.out);

  // An edited design is a new revision.
  fs.writeFileSync(htmlPath, templateHtml("DESIGN_V2"));
  const bumped = run("sync", root);
  assert.equal(bumped.status, 0, bumped.out);
  assert.match(bumped.out, /cli-smoke: new revision \(revision 2/, bumped.out);

  const listed = run("list");
  assert.equal(listed.status, 0, listed.out);
  assert.match(listed.out, /cli-smoke\s+revision 2\s+0 page\(s\)\s+— CLI Smoke/, listed.out);

  // show pins the exact revision, and omits HTML by default.
  const shown = run("show", "cli-smoke");
  assert.equal(shown.status, 0, shown.out);
  const parsed = JSON.parse(shown.out);
  assert.equal(parsed.template.current_revision, 2);
  assert.equal(parsed.html, undefined, "show must not dump the design");
  assert.deepEqual(parsed.reference_config, { campaign: "Reference" });

  // A broken template must not take the good ones down with it — a deploy that
  // half-registers is worse than one that names the bad file.
  const broken = path.join(root, "broken");
  fs.mkdirSync(broken);
  fs.writeFileSync(path.join(broken, "template.html"), "<!doctype html><html><body>no blocks</body></html>");
  const mixed = run("sync", root);
  assert.equal(mixed.status, 1, "a failed template must fail the command");
  assert.match(mixed.out, /broken: FAILED/, mixed.out);
  assert.match(mixed.out, /cli-smoke: unchanged \(revision 2/, "the healthy template still synced");
  assert.match(mixed.out, /synced 1\/2/, mixed.out);

  // An explicit path + name: how a template from a client bundle is registered.
  const byPath = run("register", htmlPath, "--name", "from-a-bundle", "--title", "Bundle Supplied");
  assert.equal(byPath.status, 0, byPath.out);
  assert.match(byPath.out, /from-a-bundle: created \(revision 1/, byPath.out);

  // A CLI-registered template must be READABLE OVER MCP. This is the gap that
  // shipped: registration attributes itself as source "cli", and the MCP output
  // schema allowed only api|mcp|admin — so get_template and
  // list_template_revisions failed output validation with -32602 for every
  // template this repo ships, while list_templates (no source field) kept
  // working. The MCP suite could not catch it because it only ever registers
  // over MCP. Validate the handlers' real return values against the real output
  // schemas.
  const templates = require("../lib/templates");
  const { TOOLS } = require("../lib/mcp-tools");

  const fetched = await templates.get("cli-smoke");
  assert.equal(fetched.revision.source, "cli", "a file-backed registration attributes itself as cli");
  const getParsed = TOOLS.get_template.outputSchema.safeParse(fetched);
  assert.ok(getParsed.success, `get_template output invalid: ${JSON.stringify(getParsed.error?.issues)}`);

  const revisions = await templates.revisions("cli-smoke");
  const listParsed = TOOLS.list_template_revisions.outputSchema.safeParse(revisions);
  assert.ok(listParsed.success, `list_template_revisions output invalid: ${JSON.stringify(listParsed.error?.issues)}`);

  const listed2 = TOOLS.list_templates.outputSchema.safeParse(await templates.list());
  assert.ok(listed2.success, `list_templates output invalid: ${JSON.stringify(listed2.error?.issues)}`);

  fs.rmSync(root, { recursive: true, force: true });
  console.log("✓ template CLI: sync creates/dedupes/revisions, reports a bad template, registers by path");
})().catch((error) => {
  console.error(error);
  process.exit(1);
});
