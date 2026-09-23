// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// get_page_data detail='summary' / 'export' through the real MCP route, and the
// /export URLs it mints, fetched the way a host-side download would fetch them.
// The URL is a bearer capability, so most of this file is the ways it must NOT
// work: forged, expired, cross-audience, revoked, un-granted, wrong host.
process.env.DASHBOARD_ORIGIN = "http://localhost";
process.env.CONTENT_ORIGIN = "http://content.localhost";

const assert = require("node:assert/strict");
const crypto = require("node:crypto");
const http = require("node:http");
const db = require("../lib/db");
const tokens = require("../lib/tokens");
const rawtoken = require("../lib/rawtoken");
const dataExport = require("../lib/data-export");
const pageData = require("../lib/page-data");
const { PROTOCOL_VERSION } = require("../lib/mcp");

const slug = "northwind-export";
const sourceAsOf = "2026-08-01T00:00:00.000Z";
let server;
let nextId = 1;

function httpRequest({ method = "GET", path, host = "localhost", headers = {}, body }) {
  return new Promise((resolve, reject) => {
    const req = http.request({
      hostname: "127.0.0.1", port: server.address().port, method, path, agent: false,
      headers: { Host: host, ...headers, ...(body === undefined ? {} : { "Content-Length": Buffer.byteLength(body) }) },
    }, (res) => {
      const chunks = [];
      res.on("data", (chunk) => chunks.push(chunk));
      res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }));
    });
    req.on("error", reject);
    req.end(body);
  });
}

async function tool(token, name, args) {
  const body = JSON.stringify({ jsonrpc: "2.0", id: nextId++, method: "tools/call", params: { name, arguments: args } });
  const res = await httpRequest({
    method: "POST", path: "/mcp", body,
    headers: {
      Accept: "application/json, text/event-stream", Authorization: `Bearer ${token}`,
      "Content-Type": "application/json", "MCP-Protocol-Version": PROTOCOL_VERSION,
    },
  });
  assert.equal(res.status, 200, `${name}: HTTP ${res.status} ${res.body}`);
  const result = JSON.parse(res.body).result;
  assert.notEqual(result.isError, true, `${name}: ${JSON.stringify(result.content)}`);
  return { ...result.structuredContent, wireBytes: Buffer.byteLength(result.content[0].text) };
}

const sha256 = (bytes) => crypto.createHash("sha256").update(bytes).digest("hex");
const pathOf = (url) => new URL(url).pathname;
// Rows carry markup on purpose: an export is JSON served as an attachment, never HTML.
const block = (id, value, type = "application/json") => `<script id="${id}" type="${type}">${pageData.escapedJson(value)}</script>`;
const schema = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object", additionalProperties: false, required: ["dataThrough", "rows"],
  properties: {
    dataThrough: { type: "string" },
    rows: { type: "array", items: { type: "object", required: ["date", "clicks"], properties: { date: { type: "string" }, clicks: { type: "integer" } } } },
  },
};
const dataFor = (days) => ({
  dataThrough: `2026-07-${String(days).padStart(2, "0")}`,
  rows: Array.from({ length: days }, (_, i) => ({ date: `2026-07-${String(i + 1).padStart(2, "0")}`, clicks: 10 * (i + 1), note: "Contoso </script><b>" })),
});
const html = "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind</h1>" +
  block("pages-data-schema", schema, "application/schema+json") +
  block("pages-data", { contract_version: 1, refreshed_at: sourceAsOf, source_as_of: sourceAsOf, data: dataFor(3) }) +
  "</body></html>";

async function refused(url, why) {
  const res = await httpRequest({ path: pathOf(url) });
  assert.equal(res.status, 401, `${why}: HTTP ${res.status}`);
  assert.equal(JSON.parse(res.body).code, "data_export_invalid", why);
  assert.doesNotMatch(res.body.toString(), /Contoso|dataThrough/, `${why}: nothing leaks`);
}

async function main() {
  await new Promise((resolve, reject) => {
    server = require("../server").app.listen(0, "127.0.0.1", resolve);
    server.once("error", reject);
  });
  const deployer = await tokens.mint({ label: "export-deployer", scope: "deploy" });
  const refresher = await tokens.mint({ label: "export-refresher", scope: "data_update", allowedSlugs: [slug] });
  const doomed = await tokens.mint({ label: "export-doomed", scope: "deploy" });
  await tool(deployer.token, "deploy_page", { slug, html, render_mode: "raw" });

  // ── summary: the narrow data_update scope can read it, and it is small ────
  const summary = await tool(refresher.token, "get_page_data", { slug, detail: "summary" });
  assert.equal(summary.detail, "summary");
  assert.ok(summary.wireBytes <= 2048, `summary is ${summary.wireBytes} bytes on the wire`);
  assert.equal(Object.hasOwn(summary, "schema"), false);
  assert.equal(Object.hasOwn(summary.envelope, "data"), false);
  assert.deepEqual(summary.coverage_profile.arrays.rows.fields.date, { kind: "date", min: "2026-07-01", max: "2026-07-03", distinct: 3, nulls: 0 });
  const full = await tool(refresher.token, "get_page_data", { slug });
  assert.equal(full.data_sha256, summary.data_sha256);
  console.log(`✓ summary read is ${summary.wireBytes} bytes on the wire and needs only the data_update scope`);

  // ── export: exact bytes, verifiable against the advertised hashes ─────────
  const exported = await tool(refresher.token, "get_page_data", { slug, detail: "export" });
  assert.equal(exported.exports.version_id, summary.live_version_id);
  const files = {};
  for (const part of ["schema", "data", "envelope"]) {
    const res = await httpRequest({ path: pathOf(exported.exports[`${part}_url`]) });
    assert.equal(res.status, 200, `${part}: HTTP ${res.status} ${res.body}`);
    assert.match(res.headers["content-type"], /^application\/json/);
    assert.equal(res.headers["cache-control"], "no-store");
    assert.equal(res.headers["x-content-type-options"], "nosniff");
    assert.match(res.headers["content-disposition"], new RegExp(`^attachment; filename="${slug}\\.v${exported.exports.version_id}\\.${part}\\.json"$`));
    assert.equal(res.headers["set-cookie"], undefined, "an export sets no cookie");
    files[part] = res.body;
  }
  assert.equal(sha256(files.schema), full.schema_sha256, "schema file hashes to schema_sha256");
  assert.equal(sha256(files.data), full.data_sha256, "data file hashes to data_sha256");
  assert.deepEqual(JSON.parse(files.envelope), full.envelope);
  assert.deepEqual(JSON.parse(files.data), full.envelope.data);
  const head = await httpRequest({ method: "HEAD", path: pathOf(exported.exports.data_url) });
  assert.equal(head.status, 200);
  console.log("✓ export URLs serve the schema, data and envelope whose SHA-256 match the advertised hashes");

  // ── what an export URL is not ──────────────────────────────────────────────
  const onContentHost = await httpRequest({ path: pathOf(exported.exports.data_url), host: "content.localhost" });
  assert.notEqual(onContentHost.status, 200, "the content host serves no export route");
  assert.doesNotMatch(onContentHost.body.toString(), /dataThrough/);
  const posted = await httpRequest({ method: "POST", path: pathOf(exported.exports.data_url), body: "{}", headers: { "Content-Type": "application/json" } });
  assert.equal(posted.status, 405);
  const token = pathOf(exported.exports.data_url).split("/")[2];
  const unknown = await httpRequest({ path: `/export/${token}/html.json` });
  assert.equal(unknown.status, 404);

  const [body, sig] = token.split(".");
  await refused(`http://localhost/export/${body}.${sig.slice(0, -2)}${sig.slice(-2) === "AA" ? "BB" : "AA"}/data.json`, "a forged signature");
  const claims = JSON.parse(Buffer.from(body, "base64url"));
  const moved = Buffer.from(JSON.stringify({ ...claims, exp: claims.exp + 86400 })).toString("base64url");
  await refused(`http://localhost/export/${moved}.${sig}/data.json`, "an extended expiry");
  const expired = dataExport.mint({ pageId: claims.pid, versionId: claims.vid, tokenId: refresher.id }, -1);
  await refused(`http://localhost/export/${expired.token}/data.json`, "an expired URL");
  const view = rawtoken.mint({ pageId: Number(claims.pid), versionId: Number(claims.vid), purpose: "view", renderMode: "raw" });
  await refused(`http://localhost/export/${view}/data.json`, "a /raw view token");
  await refused("http://localhost/export/not-a-token/data.json", "garbage");
  console.log("✓ forged, extended, expired, cross-audience and malformed export tokens are refused, and the content host has no export route");

  // ── the minting token is re-checked on every fetch ─────────────────────────
  const doomedUrl = (await tool(doomed.token, "get_page_data", { slug, detail: "export" })).exports.data_url;
  assert.equal((await httpRequest({ path: pathOf(doomedUrl) })).status, 200);
  await tokens.revoke(doomed.id);
  await refused(doomedUrl, "a URL minted by a since-revoked token");
  const refresherUrl = exported.exports.data_url;
  await db.query("DELETE FROM api_token_page_grants WHERE token_id = $1", [refresher.id]);
  await refused(refresherUrl, "a URL whose token no longer holds the page grant");
  console.log("✓ revoking the minting token, or removing its page grant, ends its export URLs");

  // ── one URL, one immutable version ─────────────────────────────────────────
  const pinned = await tool(deployer.token, "get_page_data", { slug, detail: "export" });
  const before = await tool(deployer.token, "get_page_data", { slug, detail: "summary" });
  const updated = await tool(deployer.token, "update_page_data", {
    slug, data: dataFor(4), source_as_of: "2026-08-02T00:00:00.000Z", expected_version: before.live_version_id,
  });
  assert.notEqual(updated.version.id, before.live_version_id);
  const old = await httpRequest({ path: pathOf(pinned.exports.data_url) });
  assert.equal(sha256(old.body), before.data_sha256, "an old URL keeps serving the version it named");
  const fresh = await tool(deployer.token, "get_page_data", { slug, detail: "export" });
  assert.equal(fresh.exports.version_id, updated.version.id);
  assert.equal(sha256((await httpRequest({ path: pathOf(fresh.exports.data_url) })).body), updated.data_sha256);
  await tool(deployer.token, "delete_page", { slug });
  const gone = await httpRequest({ path: pathOf(fresh.exports.data_url) });
  assert.equal(gone.status, 404, "a deleted page exports nothing");
  console.log("✓ an export URL is pinned to one immutable version and dies with its page");
}

main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(async () => {
  if (server) await new Promise((resolve) => server.close(resolve));
  await Promise.all([db.pool.end(), require("../lib/readiness").close()]);
});
