// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Unit tests for lib/agent-guide.js and the get_guide tool: what the guide
// serves, that a caller can reach nothing but the fixed topics, that every
// served document stays tenant-neutral (each Pages deployment serves the same
// text to its own agents), and that the workflow names only tools that exist.
// Run: npm test

const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const guide = require("../lib/agent-guide");
const { TOOLS } = require("../lib/mcp-tools");

const docText = (topic) => fs.readFileSync(path.join(__dirname, "..", "docs", guide.TOPICS[topic]), "utf8");

test("agent-guide: the index lists every topic, workflow first", () => {
  const out = guide.get();
  assert.equal(out.topic, null);
  assert.equal(out.section, null);
  assert.deepEqual(out.entries.map((e) => e.topic), ["workflow", "authoring", "templates", "data_updates", "motion"]);
  for (const topic of guide.TOPIC_NAMES) assert.match(out.content, new RegExp("`" + topic + "`"));
  assert.match(out.content_sha256, /^[0-9a-f]{64}$/);
});

test("agent-guide: every topic splits into uniquely named, bounded sections", () => {
  for (const topic of guide.TOPIC_NAMES) {
    const out = guide.get({ topic });
    assert.ok(out.entries.length > 0, `${topic} has sections`);
    const slugs = out.entries.map((e) => e.section);
    assert.equal(new Set(slugs).size, slugs.length, `${topic} section slugs are unique`);
    for (const entry of out.entries) {
      assert.match(entry.section, guide.SECTION_RE);
      // One read must not flood an agent's context: the largest section
      // shipped today is about 12 KB.
      assert.ok(entry.bytes <= 16 * 1024, `${topic}/${entry.section} is ${entry.bytes} bytes`);
      const section = guide.get({ topic, section: entry.section });
      assert.ok(section.content.startsWith(`## ${entry.title}`), `${topic}/${entry.section} starts at its heading`);
    }
  }
});

test("agent-guide: a heading inside a code fence is content, not a section break", () => {
  const doc = guide.parseDoc("# T\n\nintro\n\n## One\n\n```md\n## not a heading\n```\n\n## Two\n\ntext\n");
  assert.deepEqual(doc.sections.map((s) => s.slug), ["one", "two"]);
  assert.match(doc.sections[0].text, /## not a heading/);
  assert.equal(doc.intro, "# T\n\nintro");
});

test("agent-guide: unknown sections and a section without a topic are refused", () => {
  assert.throws(
    () => guide.get({ topic: "workflow", section: "no-such-section" }),
    (err) => err.status === 404 && err.code === "guide_section_not_found" && err.details.available.includes("never")
  );
  assert.throws(() => guide.get({ section: "never" }), (err) => err.status === 400 && err.code === "guide_topic_required");
});

test("agent-guide: the tool accepts only fixed topics and slug-shaped sections", () => {
  const input = TOOLS.get_guide.inputSchema;
  assert.equal(input.safeParse({}).success, true);
  assert.equal(input.safeParse({ topic: "workflow", section: "changing-a-live-page" }).success, true);
  for (const bad of [
    { topic: "../../etc/passwd" },
    { topic: "README" },
    { topic: "workflow", section: "../SECURITY" },
    { topic: "workflow", section: "Never" },
    { topic: "workflow", section: "x".repeat(81) },
    { topic: "workflow", extra: true },
  ]) {
    assert.equal(input.safeParse(bad).success, false, JSON.stringify(bad));
  }
});

test("agent-guide: results satisfy the tool's output schema and render as readable text", async () => {
  for (const args of [{}, { topic: "motion" }, { topic: "workflow", section: "never" }]) {
    const out = await TOOLS.get_guide.handler(args);
    assert.equal(TOOLS.get_guide.outputSchema.safeParse(out).success, true, JSON.stringify(args));
    const text = TOOLS.get_guide.renderText(out);
    assert.ok(text.startsWith(out.content), "text leads with the guide itself, not JSON");
    assert.match(text, /get_guide · .* · Pages \d+\.\d+\.\d+ · sha256 [0-9a-f]{12}/);
  }
  const section = await TOOLS.get_guide.handler({ topic: "workflow", section: "never" });
  assert.match(TOOLS.get_guide.renderText(section), /Other sections in workflow: who-owns-what, /);
});

// Each deployment serves this text to its own agents, so it must not carry any
// real customer, partner, mailbox, storage location or production endpoint.
// The product name and its Flag theme are fine; a client's identity is not.
const FORBIDDEN = [
  /\btwc\b/i,
  /weather company/i,
  /merrell/i,
  /raptive/i,
  /reklaim/i,
  /omnicom/i,
  /omcvic/i,
  /rainbarrel/i,
  /diageo/i,
  /\bkeel\b/i,
  /sun bum/i,
  /sc ?johnson/i,
  /energizer/i,
  /\bELC\d{3,}/i,
  /fast[._ ]?io/i,
  /victoria/i,
  /elcanotek\.com/i,
  /elcano-pages\.com/i,
  /elcano[- ]staff/i,
  /elcano share/i,
  /allergex/i,
];

test("agent-guide: every served document is tenant-neutral", () => {
  for (const topic of guide.TOPIC_NAMES) {
    const text = docText(topic);
    for (const re of FORBIDDEN) {
      const hit = text.match(re);
      assert.equal(hit, null, `${guide.TOPICS[topic]} contains ${hit && hit[0]} (${re})`);
    }
  }
});

// Backticked snake_case names in the workflow are either tools (which must
// exist, so a renamed or removed tool cannot leave the guide pointing at
// nothing) or one of these result fields and codes.
const WORKFLOW_FIELDS = new Set([
  "get_guide",
  "mcp_pages_list_templates",
  "hardcoded_config_values",
  "page_not_template_managed",
  "page_is_live",
  "version_is_live",
  "live_version_id",
  "next_step",
  "stale_version",
  "required_tools",
  "source_not_updated",
  "data_profile",
  "data_reconciliation_failed",
  "data_warnings",
  "details.retry_with_expected_version",
  "details.upload_still_staged",
  "urls.live",
]);

test("agent-guide: the workflow names only tools that exist", () => {
  const text = docText("workflow");
  const names = new Set([...text.matchAll(/`([a-z]+(?:[._][a-z]+)+)`/g)].map((m) => m[1]));
  const tools = new Set(Object.keys(TOOLS));
  for (const name of names) {
    assert.ok(tools.has(name) || WORKFLOW_FIELDS.has(name), `unknown name in AGENT_WORKFLOW.md: ${name}`);
  }
  for (const tool of ["list_templates", "create_page_from_template", "prepare_dashboard_update", "update_page_data"]) {
    assert.ok(names.has(tool), `the workflow covers ${tool}`);
  }
});

test("agent-guide: only get_guide renders text; every other tool keeps its JSON text", () => {
  const rendering = Object.entries(TOOLS)
    .filter(([, tool]) => tool.renderText)
    .map(([name]) => name);
  assert.deepEqual(rendering, ["get_guide"]);
});

test("agent-guide: every rendered response is bounded and carries its content's hash", async () => {
  const crypto = require("node:crypto");
  const reads = [{}];
  for (const topic of guide.TOPIC_NAMES) {
    reads.push({ topic });
    for (const entry of guide.get({ topic }).entries) reads.push({ topic, section: entry.section });
  }
  for (const args of reads) {
    const out = await TOOLS.get_guide.handler(args);
    assert.equal(out.content_sha256, crypto.createHash("sha256").update(out.content, "utf8").digest("hex"));
    const rendered = Buffer.byteLength(TOOLS.get_guide.renderText(out), "utf8");
    assert.ok(rendered <= 16 * 1024, `${JSON.stringify(args)} renders ${rendered} bytes`);
  }
});

test("agent-guide: a data_update token cannot read the guide", () => {
  const tokens = require("../lib/tokens");
  assert.equal(tokens.isMcpToolAllowed({ scope: "data_update" }, "get_guide"), false);
  assert.equal(tokens.isMcpToolAllowed({ scope: "deploy" }, "get_guide"), true);
});
