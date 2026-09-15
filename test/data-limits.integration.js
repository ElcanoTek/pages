// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// A separate process keeps deliberately small capacity settings out of the
// other integration suites. Set them before any module captures configuration.
process.env.PAGES_DATA_MAX_BYTES = "2048";
process.env.PAGES_MCP_MAX_INLINE_DATA_BYTES = "1024";
process.env.MAX_HTML_BYTES = "4kb";

const assert = require("node:assert/strict");
const db = require("../lib/db");
const versions = require("../lib/versions");
const { TOOLS } = require("../lib/mcp-tools");

const slug = "data-size-contract";
const sourceAsOf = "2026-08-01T00:00:00.000Z";
const envelope = (data) => ({
  contract_version: 1,
  refreshed_at: sourceAsOf,
  source_as_of: sourceAsOf,
  data,
});
// Independent serialization in the caller demonstrates the documented local
// measurements, including the extra UTF-8 bytes of script-safe JSON escaping.
const escaped = (data) => JSON.stringify(data).replace(/[<>&\u2028\u2029]/g, (character) =>
  `\\u${character.charCodeAt(0).toString(16).padStart(4, "0")}`);
const utf8 = (text) => Buffer.byteLength(text, "utf8");
const measure = (data) => ({
  payload_bytes: utf8(JSON.stringify(data)),
  escaped_payload_bytes: utf8(escaped(data)),
});
const envelopeOverhead = utf8(escaped(envelope({}))) - utf8(escaped({}));
const limits = {
  max_payload_bytes: 2048,
  max_envelope_bytes: 2048,
  max_inline_bytes: 1024,
  max_staged_bytes: 2097152,
  max_request_bytes: 4096,
  min_envelope_overhead_bytes: envelopeOverhead,
};
const payloadOfBytes = (bytes) => {
  const value = { value: "" };
  value.value = "a".repeat(bytes - utf8(JSON.stringify(value)));
  assert.equal(utf8(JSON.stringify(value)), bytes);
  return value;
};
const block = (id, value, type = "application/json") =>
  `<script id="${id}" type="${type}">${escaped(value)}</script>`;
const html = "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind</h1>" +
  block("pages-data-schema", {
    $schema: "https://json-schema.org/draft/2020-12/schema",
    type: "object",
    required: ["value"],
    additionalProperties: false,
    properties: { value: { type: "string" } },
  }, "application/schema+json") +
  block("pages-data", envelope({ value: "initial" })) + "</body></html>";

async function snapshot() {
  return (await db.query(`SELECT
    (SELECT count(*)::int FROM page_content_uploads) AS uploads,
    (SELECT count(*)::int FROM page_versions) AS versions,
    (SELECT count(*)::int FROM audit_log) AS audit,
    (SELECT coalesce(sum(starts),0)::int FROM page_upload_attempts) AS starts,
    (SELECT published_version_id::text FROM pages WHERE slug=$1) AS live`, [slug])).rows[0];
}

function domainError(boundary, bytes) {
  return (error) => {
    assert.equal(error.status, 400);
    assert.equal(error.code, "data_validation_failed");
    assert.equal(error.details.size_limit, boundary);
    assert.equal(error.details.bytes, bytes);
    assert.equal(error.details.max_bytes, 2048);
    assert.equal(error.details.changing_transport_can_help, false);
    assert.deepEqual(error.details.limits, limits);
    assert.match(error.message, /limit|exceed|large/i);
    return true;
  };
}

async function main() {
  const token = await require("../lib/tokens").mint({ label: "data-limit-fixture", scope: "deploy" });
  const ctx = { actor: "data-limit-fixture", actorType: "agent", transport: "mcp", tokenId: token.id };
  const tool = async (name, args) => {
    const definition = TOOLS[name];
    const result = JSON.parse(JSON.stringify(await definition.handler(definition.inputSchema.parse(args), ctx)));
    definition.outputSchema.parse(result);
    return result;
  };
  let liveVersion = (await tool("deploy_page", { slug, html })).version.id;
  const update = (data) => tool("update_page_data", {
    slug, data, source_as_of: sourceAsOf, expected_version: liveVersion,
  });
  const startArgs = (text, dataSize) => ({
    slug, kind: "data", total_bytes: utf8(text), content_sha256: versions.sha256(Buffer.from(text)),
    ...(dataSize === undefined ? {} : { data_size: dataSize }),
  });
  const stage = async (text, dataSize) => {
    const upload = await tool("start_page_upload", startArgs(text, dataSize));
    assert.deepEqual(upload.data_limits, limits);
    const appended = await tool("append_page_upload", {
      upload_id: upload.upload_id, sequence: 0, chunk_base64: Buffer.from(text).toString("base64"),
    });
    assert.equal(appended.complete, true);
    assert.deepEqual(appended.data_limits, limits);
    return upload;
  };
  const consume = (upload) => tool("update_page_data_upload", {
    upload_id: upload.upload_id, slug, source_as_of: sourceAsOf, expected_version: liveVersion,
  });

  const ticketText = JSON.stringify({ value: "ticket capacity discovery" });
  const ticket = await tool("create_upload_ticket", startArgs(ticketText, measure(JSON.parse(ticketText))));
  assert.deepEqual(ticket.data_limits, limits);
  await tool("cancel_page_upload", { upload_id: ticket.upload_id });

  const tooMuchPayload = payloadOfBytes(2049);
  const tooMuchEnvelope = payloadOfBytes(2048 - envelopeOverhead + 1);
  const escapedOverflow = { value: "<>&\u2028\u2029é".repeat(90) };
  assert.ok(measure(escapedOverflow).payload_bytes < 2048);
  assert.ok(utf8(escaped(envelope(escapedOverflow))) > 2048);
  const domainCases = [
    { data: tooMuchPayload, boundary: "payload", bytes: 2049 },
    { data: tooMuchEnvelope, boundary: "envelope", bytes: 2049 },
    { data: escapedOverflow, boundary: "envelope", bytes: utf8(escaped(envelope(escapedOverflow))) },
  ];
  for (const { data, boundary, bytes } of domainCases) {
    const before = await snapshot();
    await assert.rejects(update(data), domainError(boundary, bytes));
    for (const name of ["start_page_upload", "create_upload_ticket"]) {
      await assert.rejects(tool(name, startArgs(JSON.stringify(data), measure(data))), domainError(boundary, bytes));
    }
    assert.deepEqual(await snapshot(), before, "known-impossible data must not reserve uploads, versions or audit rows");
  }

  // When only the inline transport is too small, the same full payload remains
  // publishable through staging. Test its exact threshold independently.
  const exactInline = await update(payloadOfBytes(1024));
  liveVersion = exactInline.version.id;
  const oneOverInline = payloadOfBytes(1025);
  const beforeInlineFailure = await snapshot();
  await assert.rejects(update(oneOverInline), (error) => {
    assert.equal(error.code, "data_too_large_for_inline");
    assert.equal(error.details.changing_transport_can_help, true);
    assert.deepEqual(error.details.limits, limits);
    return true;
  });
  assert.deepEqual(await snapshot(), beforeInlineFailure);
  liveVersion = (await consume(await stage(JSON.stringify(oneOverInline), measure(oneOverInline)))).version.id;

  // File whitespace is a transport cost, not part of the compact data or its
  // stored envelope. Its raw bytes exceed the domain cap and must still work.
  const prettyData = { value: "Northwind complete data" };
  const prettyText = " ".repeat(2100) + JSON.stringify(prettyData, null, 2) + "\n";
  assert.ok(utf8(prettyText) > limits.max_payload_bytes);
  liveVersion = (await consume(await stage(prettyText, measure(prettyData)))).version.id;
  const prettyStored = (await versions.getPageData(slug)).envelope.data;
  assert.deepEqual(prettyStored, prettyData);

  // The materialized envelope's exact maximum is valid through staging. One
  // byte over was refused above, including when the inline cap also fails.
  for (const envelopeBytes of [2047, 2048]) {
    const data = payloadOfBytes(envelopeBytes - envelopeOverhead);
    assert.equal(utf8(escaped(envelope(data))), envelopeBytes);
    const saved = await consume(await stage(JSON.stringify(data), measure(data)));
    liveVersion = saved.version.id;
    const stored = (await db.query("SELECT html FROM page_versions WHERE id=$1", [liveVersion])).rows[0].html;
    const managed = require("../lib/page-data").parseManagedHtml(stored);
    assert.equal(utf8(escaped(managed.envelope)), envelopeBytes);
    assert.deepEqual(managed.envelope.data, data);
  }

  // Omitted or understated local measurements never bypass the authoritative
  // consume-time validation, nor do failed commits spend the upload handle.
  for (const { data, boundary, bytes } of domainCases) {
    for (const claims of [undefined, { payload_bytes: 2, escaped_payload_bytes: 2 }]) {
      const upload = await stage(JSON.stringify(data), claims);
      const before = await snapshot();
      await assert.rejects(consume(upload), domainError(boundary, bytes));
      assert.deepEqual(await snapshot(), before, "failed materialization is atomic");
      const staged = (await db.query("SELECT committed_at,commit_result,bytes_received FROM page_content_uploads WHERE id=$1", [upload.upload_id])).rows[0];
      assert.equal(staged.committed_at, null);
      assert.equal(staged.commit_result, null);
      assert.equal(Number(staged.bytes_received), utf8(JSON.stringify(data)));
      await tool("cancel_page_upload", { upload_id: upload.upload_id });
    }
  }
  console.log("✓ managed-data limits distinguish payload, escaped envelope and transport, with atomic rejection");
}

main().catch((error) => { console.error(error); process.exitCode = 1; }).finally(() => db.pool.end());
