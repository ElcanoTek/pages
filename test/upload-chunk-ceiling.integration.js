// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// A deployment that raises PAGE_UPLOAD_MAX_CHUNK_BYTES to the top of its clamp
// (1 MiB) for clients that read workspace_file references host-side. Proven
// through the real MCP route, because three separate things have to agree on
// that number: the advertised chunk_base64 maxLength, the JSON body parser (a
// 1 MiB chunk is ~1.4 MB of base64), and the page_content_upload_chunks CHECK.
// A separate process keeps the raised ceiling out of the other suites, which
// prove the default. Set configuration before any module captures it.
process.env.PAGE_UPLOAD_MAX_CHUNK_BYTES = "1048576";
process.env.DASHBOARD_HOST = "localhost";
process.env.CONTENT_HOST = "content.localhost";
process.env.DASHBOARD_ORIGIN = "http://localhost";
process.env.CONTENT_ORIGIN = "http://content.localhost";

const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const http = require("node:http");
const db = require("../lib/db");
const versions = require("../lib/versions");
const pageUploads = require("../lib/page-uploads");
const { PROTOCOL_VERSION } = require("../lib/mcp");
const { TOOLS } = require("../lib/mcp-tools");

const CEILING = 1024 * 1024;
const slug = "northwind-chunk-ceiling";
const sourceAsOf = "2026-08-01T00:00:00.000Z";
let server;
let token;
let nextId = 1;

function post(message) {
  const body = JSON.stringify(message);
  return new Promise((resolve, reject) => {
    const req = http.request({
      hostname: "127.0.0.1", port: server.address().port, method: "POST", path: "/mcp", agent: false,
      headers: {
        Host: "localhost", Accept: "application/json, text/event-stream",
        Authorization: `Bearer ${token}`, "Content-Type": "application/json",
        "MCP-Protocol-Version": PROTOCOL_VERSION, "Content-Length": Buffer.byteLength(body),
      },
    }, (res) => {
      let response = "";
      res.on("data", (chunk) => { response += chunk; });
      res.on("end", () => {
        try { resolve({ status: res.statusCode, bytes: Buffer.byteLength(body), json: JSON.parse(response) }); }
        catch (error) { reject(new Error(`HTTP ${res.statusCode}: ${response.slice(0, 500)}`)); }
      });
    });
    req.on("error", reject);
    req.end(body);
  });
}

async function callTool(name, args) {
  const sent = await post({ jsonrpc: "2.0", id: nextId++, method: "tools/call", params: { name, arguments: args } });
  assert.equal(sent.status, 200, `${name} HTTP ${sent.status}`);
  return { ...sent.json.result, requestBytes: sent.bytes };
}

async function tool(name, args) {
  const result = await callTool(name, args);
  assert.notEqual(result.isError, true, `${name}: ${JSON.stringify(result.content)}`);
  TOOLS[name].outputSchema.parse(result.structuredContent);
  return { ...result.structuredContent, requestBytes: result.requestBytes };
}

const sha256 = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
const block = (id, value, type = "application/json") => `<script id="${id}" type="${type}">${JSON.stringify(value)}</script>`;
const html = "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind delivery</h1>" +
  block("pages-data-schema", {
    $schema: "https://json-schema.org/draft/2020-12/schema",
    type: "object",
    required: ["rows"],
    additionalProperties: false,
    properties: {
      rows: {
        type: "array",
        items: {
          type: "object",
          required: ["date", "line_item", "impressions", "clicks"],
          additionalProperties: false,
          properties: {
            date: { type: "string" }, line_item: { type: "string" },
            impressions: { type: "integer" }, clicks: { type: "integer" },
          },
        },
      },
    },
  }, "application/schema+json") +
  block("pages-data", { contract_version: 1, refreshed_at: sourceAsOf, source_as_of: sourceAsOf, data: { rows: [] } }) +
  "</body></html>";

// A dashboard payload the size of the largest production data file (~990 KB):
// 21 appends at the 48 KiB default, one at this ceiling.
function largeData() {
  const rows = [];
  for (let i = 0; Buffer.byteLength(JSON.stringify({ rows })) < 989_000; i++) {
    rows.push({
      date: `2026-07-${String(1 + (i % 31)).padStart(2, "0")}`,
      line_item: `Contoso prospecting ${i % 97} / Fabrikam audience ${i % 13}`,
      impressions: 1000 + ((i * 7919) % 90000),
      clicks: (i * 31) % 900,
    });
  }
  return { rows };
}

async function chunkRows(uploadId) {
  return (await db.query(
    "SELECT sequence, octet_length(bytes)::int AS bytes FROM page_content_upload_chunks WHERE upload_id=$1 ORDER BY sequence",
    [uploadId]
  )).rows;
}

async function main() {
  assert.equal(pageUploads.MAX_CHUNK_BYTES, CEILING, "the operator's 1 MiB setting is inside the clamp");
  await new Promise((resolve, reject) => {
    server = require("../server").app.listen(0, "127.0.0.1", resolve);
    server.once("error", reject);
  });
  token = (await require("../lib/tokens").mint({ label: "chunk-ceiling-fixture", scope: "deploy" })).token;

  // What a file-reading client checks its byte range against before calling.
  const listed = await post({ jsonrpc: "2.0", id: nextId++, method: "tools/list" });
  assert.equal(listed.status, 200);
  const chunkSchema = listed.json.result.tools.find((t) => t.name === "append_page_upload").inputSchema.properties.chunk_base64;
  assert.equal(chunkSchema.maxLength, Math.ceil(CEILING / 3) * 4, "advertised maxLength is the configured ceiling as base64");
  console.log(`✓ append_page_upload advertises maxLength ${chunkSchema.maxLength} for a ${CEILING}-byte ceiling`);

  let live = (await tool("deploy_page", { slug, html })).version.id;

  // ── ~990 KB kind='data': start → ONE append → update_page_data_upload ─────
  const data = largeData();
  const file = Buffer.from(JSON.stringify(data));
  assert.ok(file.length > 980_000 && file.length < CEILING, `fixture is ${file.length} bytes`);
  const started = await tool("start_page_upload", {
    slug, kind: "data", total_bytes: file.length, content_sha256: sha256(file),
  });
  assert.equal(started.max_chunk_bytes, CEILING);
  assert.match(started.next_step, /as few appends as max_chunk_bytes \(1048576\) allows: 1 append for this file/);
  const appended = await tool("append_page_upload", {
    upload_id: started.upload_id, sequence: 0, chunk_base64: file.toString("base64"),
  });
  assert.ok(appended.requestBytes > 1_300_000, `the request carried ${appended.requestBytes} bytes`);
  assert.equal(appended.complete, true, "one append completes the upload");
  assert.equal(appended.bytes_received, file.length);
  assert.equal(appended.next_sequence, 1);
  assert.deepEqual(await chunkRows(started.upload_id), [{ sequence: 0, bytes: file.length }], "stored as one chunk row");
  const published = await tool("update_page_data_upload", {
    upload_id: started.upload_id, slug, source_as_of: sourceAsOf, expected_version: live,
  });
  assert.equal(published.version_is_live, true);
  live = published.version.id;
  const stored = await versions.getPageData(slug);
  assert.equal(sha256(Buffer.from(JSON.stringify(stored.envelope.data))), sha256(file), "the whole payload published");
  console.log(`✓ a ${file.length}-byte data file staged in one append (${appended.requestBytes}-byte request) and published`);

  // ── The ceiling is exact: 1 MiB lands, 1 MiB + 1 is refused unchanged ─────
  const page = Buffer.alloc(CEILING + 10, "a");
  const exact = await tool("start_page_upload", { slug, total_bytes: page.length, content_sha256: sha256(page) });
  assert.match(exact.next_step, /2 appends for this file/);
  const first = await tool("append_page_upload", {
    upload_id: exact.upload_id, sequence: 0, chunk_base64: page.subarray(0, CEILING).toString("base64"),
  });
  assert.equal(first.bytes_received, CEILING);
  assert.match(first.next_step, /for the next 10 raw bytes at offset 1048576/);
  const last = await tool("append_page_upload", {
    upload_id: exact.upload_id, sequence: 1, chunk_base64: page.subarray(CEILING).toString("base64"),
  });
  assert.equal(last.complete, true);
  assert.deepEqual(await chunkRows(exact.upload_id), [{ sequence: 0, bytes: CEILING }, { sequence: 1, bytes: 10 }]);
  await tool("cancel_page_upload", { upload_id: exact.upload_id });

  const over = Buffer.alloc(CEILING + 1, "b");
  const refusedUpload = await tool("start_page_upload", { slug, total_bytes: over.length, content_sha256: sha256(over) });
  const refused = await callTool("append_page_upload", {
    upload_id: refusedUpload.upload_id, sequence: 0, chunk_base64: over.toString("base64"),
  });
  assert.equal(refused.isError, true, "one byte over the ceiling is refused");
  assert.deepEqual(await chunkRows(refusedUpload.upload_id), [], "and nothing is stored");
  await tool("cancel_page_upload", { upload_id: refusedUpload.upload_id });
  console.log(`✓ a ${CEILING}-byte chunk is accepted end to end and a ${CEILING + 1}-byte chunk is refused`);

  // ── The DB constraint is the backstop, and it sits at the same number ─────
  const constraint = (await db.query(
    "SELECT pg_get_constraintdef(oid) AS def FROM pg_constraint WHERE conname='page_content_upload_chunks_bytes_check'"
  )).rows[0].def;
  assert.match(constraint, /octet_length\(bytes\) <= 1048576/);
  console.log("✓ page_content_upload_chunks CHECK matches the top of the clamp");
}

main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (server) await new Promise((resolve) => server.close(resolve));
  await Promise.all([db.pool.end(), require("../lib/readiness").close()]);
});
