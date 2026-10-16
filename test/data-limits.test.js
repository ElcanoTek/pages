// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const { execFileSync } = require("node:child_process");

process.env.PAGES_DATA_MAX_BYTES = "2048";
process.env.PAGES_MCP_MAX_INLINE_DATA_BYTES = "1024";
process.env.MAX_HTML_BYTES = "4kb";
const limits = require("../lib/data-limits");
const { assertInlineData, TOOLS } = require("../lib/mcp-tools");

test("data limits distinguish payload, stored envelope and transport budgets", () => {
  assert.deepEqual(limits.DATA_LIMITS, {
    max_payload_bytes: 2048,
    max_envelope_bytes: 2048,
    max_inline_bytes: 1024,
    max_staged_bytes: 2097152,
    max_request_bytes: 4096,
    min_envelope_overhead_bytes: limits.MIN_ENVELOPE_OVERHEAD_BYTES,
  });
  for (const boundary of ["payload", "envelope"]) {
    for (const bytes of [2047, 2048]) assert.doesNotThrow(() => limits.assertDomainSize(bytes, boundary));
    assert.throws(() => limits.assertDomainSize(2049, boundary), (err) => {
      assert.equal(err.code, "data_validation_failed");
      assert.equal(err.details.size_limit, boundary);
      assert.equal(err.details.changing_transport_can_help, false);
      assert.equal(err.details.bytes, 2049);
      assert.equal(err.details.max_bytes, 2048);
      return true;
    });
  }
});

test("HTML escaping and UTF-8 count against the stored envelope independently", () => {
  const data = { label: "Northwind <&>\u2028\u2029é" };
  const measured = limits.measureData(data);
  assert.equal(measured.payload_bytes, Buffer.byteLength(JSON.stringify(data)));
  assert.equal(measured.escaped_payload_bytes, measured.payload_bytes + 21);
  assert.doesNotThrow(() => assertInlineData(data));
  assert.throws(() => assertInlineData({ label: "<".repeat(350) }), (err) =>
    err.code === "data_validation_failed" && err.details.size_limit === "envelope");
});

test("only a transport-only refusal recommends staging", () => {
  assert.equal(assertInlineData({ x: "x".repeat(1016) }), 1024);
  assert.throws(() => assertInlineData({ x: "x".repeat(1017) }), (err) =>
    err.code === "data_too_large_for_inline" && err.details.changing_transport_can_help === true);
  assert.throws(() => assertInlineData({ x: "x".repeat(2041) }), (err) =>
    err.code === "data_validation_failed" && err.details.changing_transport_can_help === false);
});

test("inline preflight includes the actual normalized source timestamp", () => {
  const data = { x: "x".repeat(2048 - limits.MIN_ENVELOPE_OVERHEAD_BYTES - 8) };
  assert.throws(() => assertInlineData(data, "2000-01-01T00:00:00Z"), (err) =>
    err.code === "data_too_large_for_inline");
  assert.throws(() => assertInlineData(data, "0000-01-01T00:00:00+01:00"), (err) =>
    err.code === "data_validation_failed" && err.details.size_limit === "envelope" && err.details.bytes === 2051);
});

test("advisory measurements reject impossible uploads without rejecting pretty printing", () => {
  assert.doesNotThrow(() => limits.assertStagedDataSize("data", undefined));
  assert.doesNotThrow(() => limits.assertStagedDataSize("data", { payload_bytes: 2, escaped_payload_bytes: 2 }));
  assert.throws(() => limits.assertStagedDataSize("page", { payload_bytes: 2, escaped_payload_bytes: 2 }), /only.*data/i);
  for (const size of [
    { payload_bytes: -1, escaped_payload_bytes: 2 },
    { payload_bytes: 2, escaped_payload_bytes: 1 },
    { payload_bytes: 2.5, escaped_payload_bytes: 3 },
    { payload_bytes: 2, escaped_payload_bytes: Number.MAX_SAFE_INTEGER + 1 },
  ]) assert.throws(() => limits.assertStagedDataSize("data", size), /data_size/);
  assert.throws(() => limits.assertStagedDataSize("data", { payload_bytes: 2049, escaped_payload_bytes: 2049 }), (err) =>
    err.details.size_limit === "payload");
  assert.throws(() => limits.assertStagedDataSize("data", { payload_bytes: 350, escaped_payload_bytes: 2100 }), (err) =>
    err.details.size_limit === "envelope");
});

test("tool descriptions advertise resolved limits without unlimited upload claims", () => {
  const description = TOOLS.update_page_data.inputSchema.shape.data.description;
  assert.match(description, /2048/);
  assert.match(description, /1024/);
  assert.match(description, /4096/);
  assert.doesNotMatch(description, /1\.5 MB|no cap to hit/);
  for (const name of ["create_upload_ticket", "start_page_upload"]) {
    assert.ok(TOOLS[name].inputSchema.shape.data_size);
    assert.match(TOOLS[name].description, /2048/);
  }
});

test("startup overrides use one HTTP parser and retain validated defaults", () => {
  const script = "process.stdout.write(JSON.stringify(require('./lib/data-limits').DATA_LIMITS))";
  const env = { ...process.env, PAGES_DATA_MAX_BYTES: "invalid", PAGES_MCP_MAX_INLINE_DATA_BYTES: "0", MAX_HTML_BYTES: "1.5mb" };
  delete env.NODE_TEST_CONTEXT;
  const resolved = JSON.parse(execFileSync(process.execPath, ["-e", script], { cwd: require("node:path").resolve(__dirname, ".."), env }));
  assert.equal(resolved.max_payload_bytes, 1048576);
  assert.equal(resolved.max_inline_bytes, 1500000);
  assert.equal(resolved.max_request_bytes, 1572864);
});

test("byte-size settings parse b/kb/mb/gb (1024-based) and refuse anything else", () => {
  const { parseByteSize } = limits;
  for (const [text, bytes] of [
    ["999", 999],
    ["999b", 999],
    ["4kb", 4096],
    ["4KB", 4096],
    ["2mb", 2 * 1024 * 1024],
    ["2 Mb", 2 * 1024 * 1024],
    [" 3mb ", 3 * 1024 * 1024],
    ["1.5mb", 1572864],
    ["0.5kb", 512],
    ["1.0001kb", 1024],
    ["1gb", 1024 ** 3],
  ]) {
    assert.equal(parseByteSize(text, "MAX_HTML_BYTES"), bytes, text);
  }
  // Each of these the old `bytes` parser read as something (often 1 or 2
  // bytes) or silently replaced with the default.
  for (const text of ["", "0", "0.1b", "garbage", "2mbx", "2MB2", "1e6", "-1mb", "+2mb", "1tb", "2 m b", ".5mb", "1.", "0x10", "Infinity", "99999999999gb"]) {
    assert.throws(
      () => parseByteSize(text, "MAX_HTML_BYTES"),
      (err) => {
        assert.match(err.message, /^MAX_HTML_BYTES=".*" is not a positive byte size; use a number of bytes or a value such as 512kb, 2mb or 1gb$/);
        return true;
      },
      JSON.stringify(text)
    );
  }
});

test("an unreadable MAX_HTML_BYTES stops startup with the setting named", () => {
  const { spawnSync } = require("node:child_process");
  const env = { ...process.env, MAX_HTML_BYTES: "2 megabytes" };
  delete env.NODE_TEST_CONTEXT;
  const run = spawnSync(process.execPath, ["-e", "require('./lib/data-limits')"], {
    cwd: require("node:path").resolve(__dirname, ".."),
    env,
    encoding: "utf8",
  });
  assert.notEqual(run.status, 0);
  assert.match(run.stderr, /MAX_HTML_BYTES="2 megabytes" is not a positive byte size/);
  env.MAX_HTML_BYTES = "";
  const unset = spawnSync(process.execPath, ["-e", "process.stdout.write(String(require('./lib/data-limits').MAX_REQUEST_BYTES))"], {
    cwd: require("node:path").resolve(__dirname, ".."),
    env,
    encoding: "utf8",
  });
  assert.equal(unset.stdout, String(2 * 1024 * 1024), "empty means unset: the 2mb default");
});
