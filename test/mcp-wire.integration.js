// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Byte-level wire contract for POST /mcp. Every case below is sent over real
// HTTP and its status, response headers and EXACT body are compared with
// test/mcp-wire.golden.json. The golden was recorded against the official MCP
// TypeScript SDK transport (1.32.1) before Pages replaced it with its own
// handler (lib/mcp-protocol.js), so any drift from the SDK's wire behaviour —
// a status, an error code, a message, a key order, a Content-Type — fails here.
//
// Re-record ONLY for a deliberate, reviewed change:
//   MCP_WIRE_CAPTURE=1 node test/mcp-wire.integration.js
// Driven by test/run-integration.sh.

process.env.DASHBOARD_ORIGIN = process.env.DASHBOARD_ORIGIN || "http://localhost:3101";
process.env.CONTENT_ORIGIN = process.env.CONTENT_ORIGIN || "http://content.localhost:3101";

const http = require("node:http");
const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
const assert = require("node:assert/strict");

const { app } = require("../server.js");
const db = require("../lib/db");
const tokens = require("../lib/tokens");
const { version: PKG_VERSION } = require("../package.json");

const PORT = 3101;
const GOLDEN = path.join(__dirname, "mcp-wire.golden.json");
const ACCEPT = "application/json, text/event-stream";
const V = "2025-11-25";
const CLIENT = { name: "wire", version: "1.0.0" };
// Headers whose values legitimately vary per request or per run.
const VOLATILE = new Set(["date", "connection", "keep-alive", "ratelimit", "ratelimit-policy", "transfer-encoding"]);
// The dashboard host's security headers (lib/csp.js) are identical on every
// response; they are recorded once, under SHELL_KEY, and checked on every case.
const SHELL_HEADERS = new Set([
  "content-security-policy",
  "cross-origin-opener-policy",
  "cross-origin-resource-policy",
  "origin-agent-cluster",
  "referrer-policy",
  "strict-transport-security",
  "x-content-type-options",
  "x-dns-prefetch-control",
  "x-download-options",
  "x-frame-options",
  "x-permitted-cross-domain-policies",
  "x-powered-by",
  "x-xss-protection",
]);
const SHELL_KEY = "(dashboard security headers on every response)";

function send({ method = "POST", body, rawBody, headers = {}, token }) {
  const payload = rawBody !== undefined ? rawBody : body === undefined ? null : JSON.stringify(body);
  const h = {
    Host: "localhost",
    Accept: ACCEPT,
    Authorization: `Bearer ${token}`,
    ...(payload !== null ? { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) } : {}),
    ...headers,
  };
  for (const [k, v] of Object.entries(h)) if (v === null) delete h[k];
  return new Promise((resolve, reject) => {
    const req = http.request({ host: "127.0.0.1", port: PORT, method, path: "/mcp", headers: h }, (res) => {
      let data = "";
      res.setEncoding("utf8");
      res.on("data", (c) => (data += c));
      res.on("end", () => {
        // serverInfo.version is the package version; keep a release bump from
        // reading as wire drift.
        const raw = data;
        data = data.split(`"version":"${PKG_VERSION}"`).join('"version":"<package.json version>"');
        const kept = {};
        const shell = {};
        for (const [k, v] of Object.entries(res.headers).sort(([a], [b]) => a.localeCompare(b))) {
          if (SHELL_HEADERS.has(k)) shell[k] = v;
          else if (!VOLATILE.has(k)) kept[k] = v;
        }
        // Presence matters (a 202 has none); the value is the raw body's length.
        if (kept["content-length"] === String(Buffer.byteLength(raw))) kept["content-length"] = "<body length>";
        // Large bodies (the tool catalog) are pinned by digest to keep the
        // golden reviewable; every byte still counts.
        const body = data.length > 4096
          ? `sha256:${crypto.createHash("sha256").update(data).digest("hex")} bytes:${Buffer.byteLength(data)}`
          : data;
        resolve({ status: res.statusCode, headers: kept, body, shell });
      });
    });
    req.on("error", reject);
    if (payload !== null) req.write(payload);
    req.end();
  });
}

const req = (id, method, params) => ({ jsonrpc: "2.0", id, method, ...(params === undefined ? {} : { params }) });
const note = (method, params) => ({ jsonrpc: "2.0", method, ...(params === undefined ? {} : { params }) });
const pv = { "MCP-Protocol-Version": V };
const init = (protocolVersion, extra = {}) =>
  req(1, "initialize", { protocolVersion, capabilities: {}, clientInfo: CLIENT, ...extra });

// [name, request]. Bodies are objects unless `rawBody` is given.
const CASES = [
  // Lifecycle and version negotiation.
  ["initialize latest", { body: init(V) }],
  ["initialize 2025-06-18", { body: init("2025-06-18") }],
  ["initialize 2025-03-26", { body: init("2025-03-26") }],
  ["initialize 2024-11-05", { body: init("2024-11-05") }],
  ["initialize 2024-10-07", { body: init("2024-10-07") }],
  ["initialize unknown version falls back to latest", { body: init("2099-01-01") }],
  ["initialize ignores a bogus protocol header", { body: init(V), headers: { "MCP-Protocol-Version": "1999-01-01" } }],
  ["initialize with client capabilities", { body: init(V, { capabilities: { roots: { listChanged: true }, sampling: {}, elicitation: {} } }) }],
  ["initialize missing capabilities", { body: req(1, "initialize", { protocolVersion: V, clientInfo: CLIENT }) }],
  ["initialize bad clientInfo", { body: req(1, "initialize", { protocolVersion: V, capabilities: {}, clientInfo: { name: 1 } }) }],
  ["initialize bad capability shape", { body: init(V, { capabilities: { roots: 5 } }) }],
  ["initialize string id", { body: { ...init(V), id: "abc" } }],
  ["initialize as a notification", { body: note("initialize", { protocolVersion: V, capabilities: {}, clientInfo: CLIENT }) }],
  ["initialized notification", { body: note("notifications/initialized"), headers: pv }],
  ["initialized notification with params", { body: note("notifications/initialized", {}), headers: pv }],
  ["unknown notification", { body: note("notifications/whatever", { a: 1 }), headers: pv }],
  ["request method sent as a notification", { body: note("tools/list"), headers: pv }],
  ["notification with an unsupported protocol header", { body: note("notifications/initialized"), headers: { "MCP-Protocol-Version": "2099-01-01" } }],
  ["client result response", { body: { jsonrpc: "2.0", id: 9, result: {} }, headers: pv }],
  ["client error response", { body: { jsonrpc: "2.0", id: 9, error: { code: -1, message: "x" } }, headers: pv }],

  // ping and generic request handling.
  ["ping", { body: req(2, "ping"), headers: pv }],
  ["ping without a protocol header", { body: req(2, "ping") }],
  ["ping with empty params", { body: req(2, "ping", {}), headers: pv }],
  ["ping with _meta", { body: req(2, "ping", { _meta: { progressToken: 7 } }), headers: pv }],
  ["ping with bad _meta", { body: req(2, "ping", { _meta: { progressToken: 1.5 } }), headers: pv }],
  ["ping with task augmentation", { body: req(2, "ping", { task: {} }), headers: pv }],
  ["ping with array params", { body: req(2, "ping", [1]), headers: pv }],
  ["ping negative id", { body: req(-4, "ping"), headers: pv }],
  ["ping float id", { body: req(1.5, "ping"), headers: pv }],
  ["ping null id", { body: req(null, "ping"), headers: pv }],
  ["ping with an extra top-level key", { body: { ...req(2, "ping"), extra: true }, headers: pv }],
  ["ping unsupported protocol header", { body: req(2, "ping"), headers: { "MCP-Protocol-Version": "2099-01-01" } }],
  ["ping empty protocol header", { body: req(2, "ping"), headers: { "MCP-Protocol-Version": "" } }],
  ["ping legacy protocol header", { body: req(2, "ping"), headers: { "MCP-Protocol-Version": "2024-11-05" } }],
  ["unknown method", { body: req(3, "frobnicate", {}), headers: pv }],
  ["unknown method without params", { body: req(3, "resources/list"), headers: pv }],
  ["notification-shaped method with an id", { body: req(3, "notifications/initialized"), headers: pv }],
  ["wrong jsonrpc version", { body: { jsonrpc: "1.0", id: 3, method: "ping" }, headers: pv }],
  ["missing method", { body: { jsonrpc: "2.0", id: 3 }, headers: pv }],

  // Tool discovery and calls.
  ["tools/list", { body: req(4, "tools/list"), headers: pv }],
  ["tools/list with _meta", { body: req(4, "tools/list", { _meta: {} }), headers: pv }],
  ["tools/list cursor", { body: req(4, "tools/list", { cursor: "x" }), headers: pv }],
  ["tools/list bad cursor type", { body: req(4, "tools/list", { cursor: 5 }), headers: pv }],
  ["tools/call unknown tool", { body: req(5, "tools/call", { name: "nope", arguments: {} }), headers: pv }],
  ["tools/call prototype tool name", { body: req(5, "tools/call", { name: "__proto__", arguments: {} }), headers: pv }],
  ["tools/call missing name", { body: req(5, "tools/call", { arguments: {} }), headers: pv }],
  ["tools/call array arguments", { body: req(5, "tools/call", { name: "get_page", arguments: [] }), headers: pv }],
  ["tools/call without params", { body: req(5, "tools/call"), headers: pv }],
  ["tools/call invalid arguments", { body: req(5, "tools/call", { name: "get_page", arguments: { slug: "x", bogus: 1 } }), headers: pv }],
  ["tools/call missing required argument", { body: req(5, "tools/call", { name: "get_page", arguments: {} }), headers: pv }],
  ["tools/call omitted arguments", { body: req(5, "tools/call", { name: "get_page" }), headers: pv }],
  ["tools/call nested invalid argument", { body: req(5, "tools/call", { name: "patch_page", arguments: { slug: "x", edits: [{ find: 1 }] } }), headers: pv }],
  ["tools/call domain error", { body: req(5, "tools/call", { name: "get_page", arguments: { slug: "wire-no-such-page" } }), headers: pv }],
  ["tools/call success", { body: req(5, "tools/call", { name: "list_themes", arguments: {} }), headers: pv }],
  ["tools/call task augmentation", { body: req(5, "tools/call", { name: "list_themes", arguments: {}, task: {} }), headers: pv }],
  ["tools/call bad task", { body: req(5, "tools/call", { name: "list_themes", arguments: {}, task: 5 }), headers: pv }],
  ["tools/call as a notification", { body: note("tools/call", { name: "list_themes", arguments: {} }), headers: pv }],
  ["tools/call unsupported protocol header", { body: req(5, "tools/call", { name: "list_themes", arguments: {} }), headers: { "MCP-Protocol-Version": "2099-01-01" } }],

  // Bodies that are not one JSON-RPC message.
  ["malformed JSON", { rawBody: "{", headers: pv }],
  ["empty body", { rawBody: "", headers: pv }],
  ["JSON null", { rawBody: "null", headers: pv }],
  ["JSON string", { rawBody: '"ping"', headers: pv }],
  ["JSON number", { rawBody: "7", headers: pv }],
  ["empty batch", { rawBody: "[]", headers: pv }],
  ["batch of one", { body: [req(6, "ping")], headers: pv }],
  ["batch of notifications", { body: [note("notifications/initialized")], headers: pv }],

  // HTTP boundary.
  ["Content-Type with charset", { body: req(7, "ping"), headers: { ...pv, "Content-Type": "application/json; charset=utf-8" } }],
  ["Content-Type upper case", { body: req(7, "ping"), headers: { ...pv, "Content-Type": "APPLICATION/JSON" } }],
  ["Content-Type text/plain", { body: req(7, "ping"), headers: { ...pv, "Content-Type": "text/plain" } }],
  ["Content-Type missing", { body: req(7, "ping"), headers: { ...pv, "Content-Type": null } }],
  ["Accept JSON only", { body: req(7, "ping"), headers: { ...pv, Accept: "application/json" } }],
  ["Accept wildcard", { body: req(7, "ping"), headers: { ...pv, Accept: "*/*" } }],
  ["Accept upper case", { body: req(7, "ping"), headers: { ...pv, Accept: "APPLICATION/JSON, TEXT/EVENT-STREAM" } }],
  ["Accept with q=0", { body: req(7, "ping"), headers: { ...pv, Accept: "application/json, text/event-stream;q=0" } }],
  ["Accept missing", { body: req(7, "ping"), headers: { ...pv, Accept: null } }],
  ["GET", { method: "GET", headers: { "Content-Type": null } }],
  ["DELETE", { method: "DELETE", headers: { "Content-Type": null } }],
  ["PUT", { method: "PUT", body: req(8, "ping"), headers: pv }],
  ["unauthenticated", { body: req(8, "ping"), headers: { ...pv, Authorization: null } }],

  // A data_update token sees only its scoped tools; anything else is not registered for it.
  ["scoped tools/list", { scoped: true, body: req(9, "tools/list"), headers: pv }],
  ["scoped call to an unregistered tool", { scoped: true, body: req(9, "tools/call", { name: "deploy_page", arguments: {} }), headers: pv }],
  ["scoped call to a registered tool", { scoped: true, body: req(9, "tools/call", { name: "get_page_data", arguments: { slug: "wire-no-such-page" } }), headers: pv }],
  ["scoped initialize", { scoped: true, body: init(V) }],
];

(async () => {
  const server = app.listen(PORT);
  let failed = false;
  try {
    const { token } = await tokens.mint({ label: "mcp-wire", scope: "deploy" });
    const scoped = await tokens.mint({ label: "mcp-wire-data", scope: "data_update", allowedSlugs: ["wire-no-such-page"] });
    const observed = {};
    let shell = null;
    for (const [name, spec] of CASES) {
      assert.equal(Object.prototype.hasOwnProperty.call(observed, name), false, `duplicate case ${name}`);
      const { shell: caseShell, ...result } = await send({ ...spec, token: spec.scoped ? scoped.token : token });
      if (shell === null) shell = caseShell;
      assert.deepEqual(caseShell, shell, `security headers differ on "${name}"`);
      observed[name] = result;
    }
    observed[SHELL_KEY] = shell;
    if (process.env.MCP_WIRE_CAPTURE === "1") {
      fs.writeFileSync(GOLDEN, JSON.stringify(observed, null, 1) + "\n");
      console.log(`✓ recorded ${CASES.length} wire cases to ${path.basename(GOLDEN)}`);
    } else {
      const golden = JSON.parse(fs.readFileSync(GOLDEN, "utf8"));
      assert.deepEqual(Object.keys(observed), Object.keys(golden), "the case list matches the golden");
      for (const name of Object.keys(golden)) {
        assert.deepEqual(observed[name], golden[name], `wire case "${name}" drifted`);
      }
      console.log(`✓ ${CASES.length} MCP wire cases match the recorded SDK behaviour byte for byte`);
    }
  } catch (error) {
    failed = true;
    console.error("✗", error.stack || error.message);
  } finally {
    server.close();
    await db.pool.end().catch(() => {});
  }
  process.exit(failed ? 1 : 0);
})();
