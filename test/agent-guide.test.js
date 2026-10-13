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

// Each deployment serves this text to its own agents, so it must be generic:
// no real organisation's data, endpoints or identifiers, and no internal
// product names presented as if every deployment had them. This repository is
// public, so the checks below are deliberately generic heuristics, not proof.
// The names of real customers belong in a private list, read when
// PAGES_PRIVATE_DENYLIST points at it (one term or /regex/flags per line); the
// list itself must never be committed here, and failures name only the entry
// number so a CI log cannot republish it.

// URLs in served text point at documentation domains or public standards.
const ALLOWED_URL_HOSTS = [/(^|\.)example\.com$/i, /^json-schema\.org$/i];
// Long numeric identifiers are deliberate placeholders, never real records.
const SYNTHETIC_IDS = new Set(["9000001"]);
// ElcanoTek's own domains and internal tools are not part of every deployment.
const INTERNAL_NAMES = [/elcanotek\.com/i, /elcano-pages\.com/i, /\bCutlass\b/, /\bMOC\b/, /\bFleet\b/, /\bVictoria\b/];
const SECRET_PARAMS = new Set([
  "token", "access_token", "refresh_token", "id_token", "key", "api_key", "apikey",
  "secret", "client_secret", "password", "sig", "signature", "auth",
]);

// urlFindings — every URL (any scheme case, or protocol-relative //host.tld)
// that is not a documentation host, or that carries credentials or a
// secret-shaped query parameter. Returns problem descriptions.
function urlFindings(text) {
  const problems = [];
  const urls = [
    ...[...text.matchAll(/\bhttps?:\/\/[^\s`"')<>\]]+/gi)].map((m) => m[0]),
    ...[
      ...text.matchAll(
        /(?<![\w:/])\/\/(?:(?:[a-z0-9-]+\.)+[a-z]{2,}|\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-f:.]+\])(?:[/:?#][^\s`"')<>\]]*)?/gi
      ),
    ].map((m) => `https:${m[0]}`),
  ];
  for (const raw of urls) {
    let url;
    try {
      url = new URL(raw);
    } catch {
      problems.push(`unparseable URL ${raw}`);
      continue;
    }
    if (!ALLOWED_URL_HOSTS.some((re) => re.test(url.hostname))) problems.push(`non-documentation host ${raw}`);
    if (url.username || url.password) problems.push(`credentials in URL ${raw}`);
    if ([...url.searchParams.keys()].some((name) => SECRET_PARAMS.has(name.toLowerCase().replace(/-/g, "_")))) {
      problems.push(`secret-shaped query in ${raw}`);
    }
  }
  for (const [email] of text.matchAll(/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}/g)) {
    if (!/@example\.com$/i.test(email)) problems.push(`non-example email ${email}`);
  }
  return problems;
}

// idFindings — record-shaped identifiers: long numbers that are not listed
// placeholders, account or campaign codes, and storage locations.
function idFindings(text) {
  const problems = [];
  for (const [id] of text.matchAll(/(?<![\w.])\d{6,}(?![\w.])/g)) {
    if (!SYNTHETIC_IDS.has(id)) problems.push(`record-shaped id ${id}`);
  }
  const code = text.match(/\b(?:acct|elc)[-_ ]?\d{3,}\b/i);
  if (code) problems.push(`account or campaign code ${code[0]}`);
  if (/s3:\/\/|\bStorage\//.test(text)) problems.push("storage location");
  return problems;
}

// compilePrivateTerms — one matcher per entry. A literal matches anywhere,
// case-insensitively, when not glued to another letter or digit (so Markdown
// emphasis like _Name_ still matches). A /regex/ entry may use only the i, m,
// s and u flags. A bad entry fails with its number only: the native error
// would quote the private text.
function compilePrivateTerms(lines) {
  return lines
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith("#"))
    .map((line, index) => {
      // A leading slash always means a regex, so a typo cannot silently
      // become a literal that matches nothing.
      const m = line.match(/^\/(.+)\/([A-Za-z]*)$/);
      try {
        if (line.startsWith("/") && !m) throw new Error("malformed regex entry");
        if (m) {
          if (/[^imsu]/.test(m[2])) throw new Error("unsupported flag");
          return new RegExp(m[1], m[2].includes("i") ? m[2] : `${m[2]}i`);
        }
        const escaped = line.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
        return new RegExp(`(?<![A-Za-z0-9])${escaped}(?![A-Za-z0-9])`, "i");
      } catch {
        throw new Error(`private denylist entry #${index + 1} is not a valid term or /regex/flags`);
      }
    });
}

function readPrivateTerms() {
  const file = process.env.PAGES_PRIVATE_DENYLIST;
  if (!file) return null;
  let text;
  try {
    text = fs.readFileSync(file, "utf8");
  } catch {
    throw new Error("PAGES_PRIVATE_DENYLIST is set but the file cannot be read");
  }
  return compilePrivateTerms(text.split("\n"));
}

function privateFindings(text, terms) {
  return terms.flatMap((re, i) => (re.test(text) ? [`private denylist entry #${i + 1}`] : []));
}

function servedDocs() {
  return guide.TOPIC_NAMES.map((topic) => ({ file: guide.TOPICS[topic], text: docText(topic) }));
}

test("agent-guide: served URLs point only at documentation domains", () => {
  for (const { file, text } of servedDocs()) assert.deepEqual(urlFindings(text), [], file);
});

test("agent-guide: served examples carry no record-shaped identifiers", () => {
  for (const { file, text } of servedDocs()) assert.deepEqual(idFindings(text), [], file);
});

test("agent-guide: served text names no internal domain or tool", () => {
  for (const { file, text } of servedDocs()) {
    for (const re of INTERNAL_NAMES) {
      const hit = text.match(re);
      assert.equal(hit, null, `${file} contains ${hit && hit[0]}`);
    }
  }
});

test("agent-guide: the checks fire on invented leaks and their usual disguises", () => {
  // Every fixture is invented; none names a real organisation.
  for (const leak of [
    "see https://dashboards.invented-corp.test/page",
    "see HTTPS://dashboards.invented-corp.test/page",
    "see //dashboards.invented-corp.test/page",
    "see https://demo:invented-secret@example.com/page",
    "see https://example.com/page?token=invented-secret",
    "see https://example.com/page?%74oken=invented-secret",
    "see https://example.com/page?access_token=invented-secret",
    "see //192.0.2.1/page",
    "see //[2001:db8::1]/page",
    "mail ops@invented-corp.test",
  ]) {
    assert.notDeepEqual(urlFindings(leak), [], leak);
  }
  for (const fine of ["see https://content.example.com/page", "see https://JSON-SCHEMA.org/draft", "mail OPS@EXAMPLE.COM", "a protocol-relative (`//host/path`) URL"]) {
    assert.deepEqual(urlFindings(fine), [], fine);
  }
  for (const leak of ['{"dealId":"4815162"}', "filter: ACCT00123", "filter: elc-0042", "s3://invented-bucket/x", "Storage/invented.csv"]) {
    assert.notDeepEqual(idFindings(leak), [], leak);
  }
  assert.deepEqual(idFindings('{"dealId":"9000001","bytes":"1.048576"}'), []);
});

test("agent-guide: the private denylist matches disguised names and never echoes an entry", () => {
  const terms = compilePrivateTerms(["# comment", "Invented Holdings", "/invent(?:ed)? ?co\\b/", "Invented Co."]);
  for (const text of ["by _Invented Holdings_.", "INVENTED HOLDINGS", "**Invented Holdings**", "from inventedco", "an Invented Co. page"]) {
    assert.notDeepEqual(privateFindings(text, terms), [], text);
  }
  assert.deepEqual(privateFindings("Uninvented Holdingsworth", compilePrivateTerms(["Invented Holdings"])), []);
  for (const bad of ["/InventedSecretName[/i", "/InventedSecretName/y", "/InventedSecretName/g", "/InventedSecretName/I", "/InventedSecretName"]) {
    assert.throws(
      () => compilePrivateTerms(["fine", bad]),
      (err) => err.message === "private denylist entry #2 is not a valid term or /regex/flags" && !err.message.includes("InventedSecretName")
    );
  }
});

test("agent-guide: no term from the private denylist appears in served text", (t) => {
  const terms = readPrivateTerms();
  if (!terms) return t.skip("PAGES_PRIVATE_DENYLIST is not set (it is kept outside this public repository)");
  assert.ok(terms.length > 0, "the private denylist is empty");
  for (const { file, text } of servedDocs()) assert.deepEqual(privateFindings(text, terms), [], file);
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
