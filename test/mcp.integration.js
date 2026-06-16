"use strict";
// Integration check for /mcp (MCP-over-HTTP). Proves the JSON-RPC surface wraps
// the SAME state machine as REST: initialize/tools-list, deploy+publish via a
// tool, dedupe, rollback, the approval gate, and JSON-RPC error shapes. Driven
// by test/run-integration.sh (Postgres + secrets in env).

const http = require("node:http");
const assert = require("node:assert/strict");

const { app } = require("../server.js");
const tokens = require("../lib/tokens");
const { PROTOCOL_VERSION } = require("../lib/mcp");

const PORT = 3100;
const DASH = "localhost";
const HTML = (n) =>
  `<!doctype html><html><head><title>MCP</title></head><body><h1>v${n}</h1><canvas id=c></canvas><script>chart(${n})</script></body></html>`;

let nextId = 1;

// POST a JSON-RPC message to /mcp. Returns { status, json }.
function rpc(method, params, { token, notification = false } = {}) {
  const msg = { jsonrpc: "2.0", method, ...(params ? { params } : {}) };
  if (!notification) msg.id = nextId++;
  const payload = JSON.stringify(msg);
  const headers = { Host: DASH, "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) };
  if (token) headers["Authorization"] = `Bearer ${token}`;
  return new Promise((resolve, reject) => {
    const r = http.request({ host: "127.0.0.1", port: PORT, method: "POST", path: "/mcp", headers }, (res) => {
      let b = "";
      res.on("data", (d) => (b += d));
      res.on("end", () => {
        let json = null;
        try { json = b ? JSON.parse(b) : null; } catch { /* 202 empty */ }
        resolve({ status: res.statusCode, json });
      });
    });
    r.on("error", reject);
    r.write(payload);
    r.end();
  });
}

// Unwrap a tools/call result's text payload (our tools return JSON as text).
function toolData(resp) {
  assert.ok(resp.json && resp.json.result, `expected a result, got ${JSON.stringify(resp.json)}`);
  return JSON.parse(resp.json.result.content[0].text);
}

(async () => {
  const srv = app.listen(PORT);
  let failed = false;
  try {
    const { token } = await tokens.mint({ label: "mcp-agent", scope: "deploy" });

    // 0. No token → 401 (server is "disabled" without a valid token).
    const noAuth = await rpc("initialize", { protocolVersion: PROTOCOL_VERSION });
    assert.equal(noAuth.status, 401, "no token → 401");
    console.log("✓ unauthenticated → 401");

    // 1. initialize handshake.
    const init = await rpc("initialize", { protocolVersion: PROTOCOL_VERSION, capabilities: {} }, { token });
    assert.equal(init.status, 200);
    assert.equal(init.json.result.protocolVersion, PROTOCOL_VERSION, "protocol version echoed");
    assert.equal(init.json.result.serverInfo.name, "pages");
    console.log("✓ initialize handshake");

    // 1b. the initialized notification gets no response body (202).
    const note = await rpc("notifications/initialized", {}, { token, notification: true });
    assert.equal(note.status, 202, "notification → 202 no body");
    console.log("✓ initialized notification → 202");

    // 2. tools/list advertises the agent tool set.
    const list = await rpc("tools/list", {}, { token });
    const names = list.json.result.tools.map((t) => t.name);
    for (const want of ["list_pages", "get_page", "deploy_page", "publish_page", "rollback_page", "list_versions"]) {
      assert.ok(names.includes(want), `tools/list missing ${want}`);
    }
    // every tool advertises an inputSchema object
    assert.ok(list.json.result.tools.every((t) => t.inputSchema && t.inputSchema.type === "object"));
    console.log(`✓ tools/list (${names.length} tools)`);

    // 3. deploy_page create+publish in one call (the "make me a dashboard" path).
    const dep = await rpc("tools/call", { name: "deploy_page", arguments: { slug: "mcpdemo", title: "MCP Demo", html: HTML(1), publish: true } }, { token });
    const d = toolData(dep);
    assert.equal(d.created, true, "page created");
    assert.equal(d.published, true, "published live on open page");
    assert.equal(d.version.status, "approved");
    assert.ok(d.urls && d.urls.admin.includes("/admin/mcpdemo"), "returns routing urls");
    const v1 = d.version.id;
    console.log("✓ deploy_page (create + publish)");

    // 4. get_page reflects the live pointer.
    const gp = toolData(await rpc("tools/call", { name: "get_page", arguments: { slug: "mcpdemo" } }, { token }));
    assert.equal(gp.page.published_version_id, v1, "pointer is v1");
    assert.equal(gp.published.id, v1);
    console.log("✓ get_page reflects pointer-is-truth");

    // 5. list_pages includes it.
    const lp = toolData(await rpc("tools/call", { name: "list_pages", arguments: {} }, { token }));
    assert.ok(lp.pages.some((p) => p.slug === "mcpdemo"), "list_pages includes mcpdemo");
    console.log("✓ list_pages");

    // 6. dedupe: identical html → existing version.
    const dup = toolData(await rpc("tools/call", { name: "deploy_page", arguments: { slug: "mcpdemo", html: HTML(1) } }, { token }));
    assert.equal(dup.deduped, true);
    assert.equal(dup.version.id, v1);
    console.log("✓ deploy_page dedupe");

    // 7. update_page v2 + publish, then rollback to v1.
    const up = toolData(await rpc("tools/call", { name: "update_page", arguments: { slug: "mcpdemo", html: HTML(2), publish: true } }, { token }));
    const v2 = up.version.id;
    assert.notEqual(v2, v1);
    const rb = toolData(await rpc("tools/call", { name: "rollback_page", arguments: { slug: "mcpdemo", version_id: v1 } }, { token }));
    assert.equal(rb.version.id, v1);
    const after = toolData(await rpc("tools/call", { name: "get_page", arguments: { slug: "mcpdemo" } }, { token }));
    assert.equal(after.page.published_version_id, v1, "rolled back to v1");
    console.log("✓ update_page + rollback_page");

    // 8. unknown tool → JSON-RPC -32602 (protocol error, not a result).
    const unk = await rpc("tools/call", { name: "nope", arguments: {} }, { token });
    assert.equal(unk.json.error.code, -32602, "unknown tool → -32602");
    console.log("✓ unknown tool → JSON-RPC error");

    // 9. unknown method → -32601.
    const um = await rpc("frobnicate", {}, { token });
    assert.equal(um.json.error.code, -32601, "unknown method → -32601");
    console.log("✓ unknown method → JSON-RPC error");

    // 10. approval gate: deploy on a gated page → pending (not live); publish → isError.
    await rpc("tools/call", { name: "deploy_page", arguments: { slug: "mcpgated", title: "Gated", html: HTML(1) } }, { token });
    // make it gated via SQL (no agent tool toggles approval — that's admin-only)
    await require("../lib/db").query("UPDATE pages SET require_approval = true WHERE slug = 'mcpgated'");
    const gd = toolData(await rpc("tools/call", { name: "deploy_page", arguments: { slug: "mcpgated", html: HTML(5), publish: true } }, { token }));
    assert.equal(gd.gated, true);
    assert.equal(gd.published, false, "gated deploy not published");
    assert.equal(gd.version.status, "pending");
    const gpub = await rpc("tools/call", { name: "publish_page", arguments: { slug: "mcpgated", version_id: gd.version.id } }, { token });
    assert.equal(gpub.json.result.isError, true, "agent publish on gated page → isError");
    assert.match(gpub.json.result.content[0].text, /approval_required/);
    console.log("✓ approval gate (pending + publish isError)");

    console.log("\n✓ MCP integration passed");
  } catch (err) {
    failed = true;
    console.error("✗", err.stack || err.message);
  } finally {
    srv.close();
    await require("../lib/db").pool.end().catch(() => {});
  }
  process.exit(failed ? 1 : 0);
})();
