// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Integration check for the authenticated MCP Streamable HTTP endpoint. This
// intentionally drives the wire protocol rather than calling tool handlers
// directly: transport negotiation, JSON-RPC validation, tool metadata/output,
// auth boundaries, and the shared Pages state machine all stay covered.

// lib/csp.js reads these at module load, so set the exact test origins before
// requiring server.js. URL-returning tools must preserve the scheme and port.
process.env.DASHBOARD_ORIGIN = process.env.DASHBOARD_ORIGIN || "http://localhost:3100";
process.env.CONTENT_ORIGIN = process.env.CONTENT_ORIGIN || "http://content.localhost:3100";

const http = require("node:http");
const crypto = require("node:crypto");
const fs = require("node:fs");
const path = require("node:path");
const assert = require("node:assert/strict");
const { Client } = require("@modelcontextprotocol/sdk/client/index.js");
const { StreamableHTTPClientTransport } = require("@modelcontextprotocol/sdk/client/streamableHttp.js");

const { app } = require("../server.js");
const db = require("../lib/db");
const tokens = require("../lib/tokens");
const versions = require("../lib/versions");
const pageUploads = require("../lib/page-uploads");
const workspaces = require("../lib/workspaces");
const pageDataContract = require("../lib/page-data");
const { PROTOCOL_VERSION } = require("../lib/mcp");

const PORT = 3100;
const DASH = "localhost";
const ACCEPT = "application/json, text/event-stream";
const HTML = (n) =>
  `<!doctype html><html><head><title>MCP</title></head><body><h1>v${n}</h1><canvas id=c></canvas><script>chart(${n})</script></body></html>`;
// A minimal but REAL template: both managed pairs, a reference config and an
// empty-state envelope that each satisfy their own schema, and a render layer
// that reads them the documented way. `n` varies the design so a second
// registration is a genuine revision.
const TEMPLATE_CONFIG_SCHEMA = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  additionalProperties: false,
  required: ["campaign", "channel"],
  properties: {
    campaign: { type: "string", minLength: 1, maxLength: 120 },
    channel: { type: "string", enum: ["display", "video", "olv"] },
  },
};
const TEMPLATE_DATA_SCHEMA = {
  $schema: "https://json-schema.org/draft/2020-12/schema",
  type: "object",
  additionalProperties: false,
  required: ["rows"],
  properties: {
    rows: {
      type: "array",
      maxItems: 500,
      items: {
        type: "object",
        additionalProperties: false,
        required: ["date", "spend"],
        properties: { date: { type: "string" }, spend: { type: "number", minimum: 0 } },
      },
    },
  },
};
const TEMPLATE_HTML = (n) =>
  `<!doctype html><html><head><meta charset="utf-8"><title>Campaign</title></head><body>` +
  `<h1 id="hTitle">Campaign</h1><div id="rev">design v${n}</div><table id="rows"></table>` +
  `<script type="application/schema+json" id="pages-config-schema">${JSON.stringify(TEMPLATE_CONFIG_SCHEMA)}</script>` +
  `<script type="application/json" id="pages-config">${JSON.stringify({ campaign: "Reference", channel: "display" })}</script>` +
  `<script type="application/schema+json" id="pages-data-schema">${JSON.stringify(TEMPLATE_DATA_SCHEMA)}</script>` +
  `<script type="application/json" id="pages-data">${JSON.stringify({
    contract_version: 1,
    refreshed_at: "2026-08-01T00:00:00.000Z",
    source_as_of: "2026-08-01T00:00:00.000Z",
    data: { rows: [] },
  })}</script>` +
  // Preview-only. Deleted from every materialization, which is what the
  // create_page_from_template assertions below check.
  `<script type="application/json" id="pages-data-example">${JSON.stringify({
    rows: [{ date: "2026-07-30", spend: 1111 }, { date: "2026-07-31", spend: 2222 }],
  })}</script>` +
  `<script>
const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);
const DATA = JSON.parse(document.getElementById('pages-data').textContent).data;
document.getElementById('hTitle').textContent = CONFIG.campaign;
document.getElementById('rows').textContent = DATA.rows.length ? String(DATA.rows.length) : 'Awaiting first ingest';
</script></body></html>`;

const EXPECTED_TOOLS = [
  "append_page_upload",
  "cancel_page_upload",
  "configure_page_refresh",
  "create_page_from_template",
  "create_template_from_page",
  "create_upload_ticket",
  "create_workspace",
  "delete_page",
  "delete_template",
  "deploy_page",
  "deploy_page_upload",
  "find_in_version",
  "get_page",
  "get_page_config",
  "get_page_data",
  "get_page_refresh",
  "get_template",
  "get_version",
  "list_pages",
  "list_template_pages",
  "list_template_revisions",
  "list_templates",
  "list_themes",
  "list_versions",
  "list_workspaces",
  "page_urls",
  "patch_page",
  "preflight_page",
  "prepare_dashboard_update",
  "publish_page",
  "record_refresh_check",
  "register_template_upload",
  "rename_workspace",
  "rerender_page_from_template",
  "rollback_page",
  "set_page_workspace",
  "set_password",
  "set_title",
  "start_page_upload",
  "template_urls",
  "update_page",
  "update_page_config",
  "update_page_data",
  "update_page_data_upload",
  "validate_template",
];

let nextId = 1;
let bearerToken = null;

// Generic HTTP helper. Passing token:null explicitly suppresses the default
// bearer token; rawBody bypasses JSON.stringify for parse-error coverage.
function request(method, path = "/mcp", opts = {}) {
  const hasRawBody = Object.prototype.hasOwnProperty.call(opts, "rawBody");
  const payload = hasRawBody
    ? opts.rawBody
    : opts.body === undefined
      ? null
      : JSON.stringify(opts.body);
  const token = Object.prototype.hasOwnProperty.call(opts, "token") ? opts.token : bearerToken;
  const headers = { Host: opts.host || DASH };
  if (opts.accept !== false) headers.Accept = opts.accept || ACCEPT;
  if (token) headers.Authorization = `Bearer ${token}`;
  if (opts.origin !== undefined) headers.Origin = opts.origin;
  if (payload !== null && opts.contentType !== false) {
    headers["Content-Type"] = opts.contentType || "application/json";
    headers["Content-Length"] = Buffer.byteLength(payload);
  }
  Object.assign(headers, opts.headers || {});

  return new Promise((resolve, reject) => {
    const req = http.request(
      { host: "127.0.0.1", port: PORT, method, path, headers },
      (res) => {
        let body = "";
        res.on("data", (chunk) => (body += chunk));
        res.on("end", () => {
          let json = null;
          try {
            json = body ? JSON.parse(body) : null;
          } catch {
            // Empty 202 responses and intentionally malformed non-JSON bodies
            // remain available in `body` for the caller to inspect.
          }
          resolve({ status: res.statusCode, headers: res.headers, body, json });
        });
      }
    );
    req.on("error", reject);
    if (payload !== null) req.write(payload);
    req.end();
  });
}

function rpc(method, params, opts = {}) {
  const message = { jsonrpc: "2.0", method };
  if (params !== undefined) message.params = params;
  if (!opts.notification) {
    message.id = Object.prototype.hasOwnProperty.call(opts, "id") ? opts.id : nextId++;
  }
  const headers = { ...(opts.headers || {}) };
  if (method !== "initialize" && opts.protocolHeader !== false) {
    headers["MCP-Protocol-Version"] = opts.protocolHeader || PROTOCOL_VERSION;
  }
  return request("POST", "/mcp", { ...opts, headers, body: message });
}

function initialize(protocolVersion, opts = {}) {
  return rpc(
    "initialize",
    {
      protocolVersion,
      capabilities: {},
      clientInfo: { name: "pages-integration", title: "Pages Integration", version: "1.0.0" },
    },
    opts
  );
}

function callTool(name, args = {}, opts = {}) {
  return rpc("tools/call", { name, arguments: args }, opts);
}

// Every successful Pages tool deliberately mirrors structuredContent in its
// text fallback so both modern and text-only clients see exactly the same data.
function toolData(response) {
  assert.equal(response.status, 200, `tool HTTP status: ${response.status} ${response.body}`);
  assert.ok(response.json && response.json.result, `expected tool result, got ${response.body}`);
  assert.notEqual(response.json.result.isError, true, `unexpected tool error: ${response.body}`);
  const text = response.json.result.content.find((item) => item.type === "text");
  assert.ok(text, "tool result has a text fallback");
  const parsed = JSON.parse(text.text);
  assert.deepEqual(response.json.result.structuredContent, parsed, "structuredContent exactly mirrors text JSON");
  return parsed;
}

function toolError(response) {
  assert.equal(response.status, 200, `tool error HTTP status: ${response.status} ${response.body}`);
  assert.equal(response.json && response.json.result && response.json.result.isError, true, response.body);
  const text = response.json.result.content.find((item) => item.type === "text");
  assert.ok(text, "tool error has text content");
  let data = null;
  try {
    data = JSON.parse(text.text);
  } catch {
    // SDK argument-validation errors are human-readable text, not a Pages
    // domain-error object.
  }
  if (response.json.result.structuredContent !== undefined && data !== null) {
    assert.deepEqual(response.json.result.structuredContent, data, "structured tool error mirrors its text JSON");
  }
  return { text: text.text, data };
}

async function postEnvelope(body, opts = {}) {
  const headers = { "MCP-Protocol-Version": PROTOCOL_VERSION, ...(opts.headers || {}) };
  return request("POST", "/mcp", { ...opts, headers, body });
}

(async () => {
  const server = app.listen(PORT);
  let failed = false;
  try {
    const minted = await tokens.mint({ label: "mcp-agent", scope: "deploy" });
    bearerToken = minted.token;

    // 1. Bearer auth is explicit, non-cacheable, and RFC-shaped.
    const noAuth = await initialize("2025-11-25", { token: null });
    assert.equal(noAuth.status, 401, "no token -> 401");
    assert.match(noAuth.headers["www-authenticate"] || "", /^Bearer\b/);
    assert.match(noAuth.headers["cache-control"] || "", /no-store/);
    assert.equal(noAuth.json.error.code, -32001);
    console.log("✓ bearer auth -> 401 + WWW-Authenticate + no-store");

    // 2. Current stable and legacy clients both negotiate truthfully. The
    // initialize payload includes all lifecycle-required client fields.
    assert.equal(PROTOCOL_VERSION, "2025-11-25", "server tracks the latest stable MCP version");
    const init = await initialize(PROTOCOL_VERSION);
    assert.equal(init.status, 200, init.body);
    assert.equal(init.json.result.protocolVersion, "2025-11-25");
    assert.equal(init.json.result.serverInfo.name, "pages");
    assert.equal(init.json.result.serverInfo.title, "Elcano Pages");
    assert.match(init.json.result.instructions || "", /list_workspaces/);
    assert.match(init.json.result.instructions || "", /start_page_upload/);
    assert.match(init.json.result.instructions || "", /never pass a path, \$\(cat/i);
    assert.match(init.json.result.instructions || "", /update <slug> dashboard with/i);
    assert.match(init.json.result.instructions || "", /prepare_dashboard_update/);
    assert.match(init.json.result.instructions || "", /do not execute it now/i);
    assert.equal(init.json.result.capabilities.tools.listChanged, false);

    const legacy = await initialize("2024-11-05");
    assert.equal(legacy.status, 200, legacy.body);
    assert.equal(legacy.json.result.protocolVersion, "2024-11-05");
    const initialized = await rpc("notifications/initialized", {}, { notification: true });
    assert.equal(initialized.status, 202);
    assert.equal(initialized.body, "");
    console.log("✓ stable + legacy initialize negotiation and lifecycle notification");

    // 3. Static tool discovery is complete and richly described for clients.
    const listed = await rpc("tools/list", {});
    assert.equal(listed.status, 200, listed.body);
    const advertised = listed.json.result.tools;
    assert.deepEqual(advertised.map((tool) => tool.name).sort(), EXPECTED_TOOLS);
    assert.equal(advertised.some((tool) => tool.name === "delete_workspace"), false, "bulk delete remains human-only");
    for (const tool of advertised) {
      assert.ok(tool.title && typeof tool.title === "string", `${tool.name} has a title`);
      assert.ok(tool.description && typeof tool.description === "string", `${tool.name} has a description`);
      assert.equal(tool.inputSchema && tool.inputSchema.type, "object", `${tool.name} has an object input schema`);
      assert.equal(tool.outputSchema && tool.outputSchema.type, "object", `${tool.name} has an object output schema`);
      for (const hint of ["readOnlyHint", "destructiveHint", "idempotentHint", "openWorldHint"]) {
        assert.equal(typeof tool.annotations[hint], "boolean", `${tool.name} declares ${hint}`);
      }
    }
    const impossibleToolCursor = await rpc("tools/list", { cursor: "never-issued" });
    assert.equal(impossibleToolCursor.json.error.code, -32602, "static tools/list rejects unissued cursors");
    console.log(`✓ tools/list exposes ${advertised.length} typed, titled, annotated tools`);

    // 4. Streamable HTTP security and media-type boundaries fail closed in
    // JSON-RPC form, before a tool can run.
    const badOrigin = await rpc("ping", {}, { origin: "https://evil.example" });
    assert.equal(badOrigin.status, 403);
    assert.equal(badOrigin.json.error.code, -32000);
    assert.equal((await rpc("ping", {}, { origin: "http://localhost:3100/path" })).status, 403);
    const normalizedOrigin = await rpc("ping", {}, { origin: "HTTP://LOCALHOST:3100" });
    assert.equal(normalizedOrigin.status, 200, "normalized-equivalent allowed origin succeeds");
    assert.equal(normalizedOrigin.headers["access-control-allow-origin"], "http://localhost:3100");
    const badHost = await rpc("ping", {}, { host: "evil.example" });
    assert.equal(badHost.status, 403);
    assert.equal((await rpc("ping", {}, { host: "evil@localhost" })).status, 403, "Host userinfo is rejected");
    const missingAccept = await rpc("ping", {}, { accept: false });
    assert.equal(missingAccept.status, 406, missingAccept.body);
    const wrongType = await request("POST", "/mcp", {
      contentType: "text/plain",
      headers: { "MCP-Protocol-Version": PROTOCOL_VERSION },
      body: { jsonrpc: "2.0", id: nextId++, method: "ping" },
    });
    assert.equal(wrongType.status, 415, wrongType.body);
    const unsupported = await rpc("ping", {}, { protocolHeader: "2099-01-01" });
    assert.equal(unsupported.status, 400, unsupported.body);
    console.log("✓ Origin, Host, Accept, Content-Type, and protocol-version boundaries");

    // 5. Malformed JSON, batches, invalid envelopes, and invalid request IDs
    // receive protocol errors rather than Express HTML or accidental success.
    const malformed = await request("POST", "/mcp", {
      rawBody: "{",
      headers: { "MCP-Protocol-Version": PROTOCOL_VERSION },
    });
    assert.equal(malformed.status, 400);
    assert.equal(malformed.json.error.code, -32700);
    const batch = await postEnvelope([{ jsonrpc: "2.0", id: 1, method: "ping" }]);
    assert.equal(batch.status, 400);
    assert.equal(batch.json.error.code, -32600);
    for (const envelope of [
      { jsonrpc: "2.0", id: null, method: "ping" },
      { jsonrpc: "2.0", id: { bad: true }, method: "ping" },
      { jsonrpc: "1.0", id: 71, method: "ping" },
      { jsonrpc: "2.0", id: 72 },
    ]) {
      const invalid = await postEnvelope(envelope);
      assert.equal(invalid.json.error.code, -32600, `invalid envelope rejected: ${JSON.stringify(envelope)}`);
    }
    console.log("✓ parse, batch, request-envelope, and request-ID validation");

    // 6. A request-only method shaped as a notification gets no result and,
    // critically, cannot mutate Pages state.
    const notificationCall = await callTool(
      "deploy_page",
      { slug: "notification-must-not-run", html: HTML(99) },
      { notification: true }
    );
    assert.equal(notificationCall.status, 202);
    assert.equal(notificationCall.body, "");
    const notificationProbe = toolError(await callTool("get_page", { slug: "notification-must-not-run" }));
    assert.equal(notificationProbe.data.code, "page_not_found");
    console.log("✓ notification-shaped tools/call is acknowledged without mutation");

    // 7. Unknown and prototype-named tools are protocol Invalid Params, never
    // resolved through an object prototype or reported as internal failures.
    for (const name of ["no_such_tool", "constructor", "__proto__"]) {
      const unknown = await callTool(name, {});
      assert.equal(unknown.json.error.code, -32602, `${name} -> -32602`);
    }
    const unknownMethod = await rpc("frobnicate", {});
    assert.equal(unknownMethod.json.error.code, -32601);
    console.log("✓ unknown/prototype tools and unknown methods use protocol errors");

    // 8. GET/DELETE are authenticated but intentionally unsupported for this
    // stateless JSON-response deployment.
    for (const method of ["GET", "DELETE"]) {
      const unauthenticated = await request(method, "/mcp", { token: null });
      assert.equal(unauthenticated.status, 401, `${method} requires auth`);
      const authenticated = await request(method, "/mcp");
      assert.equal(authenticated.status, 405, `${method} -> 405 after auth`);
      assert.equal(authenticated.headers.allow, "POST, OPTIONS");
      assert.equal(authenticated.json.error.code, -32000);
    }
    const preflight = await request("OPTIONS", "/mcp", {
      token: null,
      origin: "http://localhost:3100",
      headers: {
        "Access-Control-Request-Method": "POST",
        "Access-Control-Request-Headers": "authorization,content-type,mcp-protocol-version",
      },
    });
    assert.equal(preflight.status, 204, "allowed CORS preflight does not require bearer auth");
    assert.equal(preflight.headers["access-control-allow-origin"], "http://localhost:3100");
    assert.equal(preflight.headers["access-control-allow-methods"], "POST, OPTIONS");
    console.log("✓ GET/DELETE require auth; POST/OPTIONS boundary advertises CORS consistently");

    // 9. The normal create-and-publish path returns serving truth and exact
    // configured URLs; all success data is dual-encoded identically.
    const deployed = toolData(
      await callTool("deploy_page", {
        slug: "mcpdemo",
        title: "MCP Demo",
        html: HTML(1),
        publish: true,
      })
    );
    assert.equal(deployed.created, true);
    assert.equal(deployed.published, true);
    assert.equal(deployed.version.status, "approved");
    assert.equal(typeof deployed.version.id, "string", "BIGINT ids are canonical decimal strings");
    assert.equal(deployed.version_is_live, true);
    assert.equal(deployed.page_is_live, true);
    assert.equal(deployed.live_version_id, deployed.version.id);
    assert.equal(deployed.urls.admin, "http://localhost:3100/admin/mcpdemo");
    assert.equal(deployed.urls.view, "http://localhost:3100/view/mcpdemo");
    assert.equal(deployed.urls.live, "http://content.localhost:3100/mcpdemo");
    const v1 = deployed.version.id;

    const page = toolData(await callTool("get_page", { slug: "mcpdemo" }));
    assert.equal(page.page.published_version_id, v1);
    assert.equal(page.published.id, v1);
    assert.equal(page.published.html, undefined, "get_page omits HTML by default");
    const pageWithHtml = toolData(await callTool("get_page", { slug: "mcpdemo", include_html: true }));
    assert.match(pageWithHtml.published.html, /v1/);
    const urls = toolData(await callTool("page_urls", { slug: "mcpdemo" }));
    assert.equal(urls.live, deployed.urls.live);
    const themes = toolData(await callTool("list_themes", {}));
    assert.ok(themes.themes.some((theme) => theme.name === "flag"));
    console.log("✓ deploy/get/page_urls/list_themes share canonical state and URLs");

    // Preflight, end to end. The bug this reproduces shipped to a live client
    // dashboard: onclick="togglePopover()" resolves to HTMLElement.togglePopover
    // via the inline-handler scope chain, throws NotSupportedError on click, and
    // the date-range picker silently never opens. Nothing in the authoring loop
    // could execute the page, so five deploys went out with it.
    const brokenHtml =
      '<!doctype html><html><head><meta charset="utf-8"><title>Broken</title></head><body>' +
      '<button id="t" onclick="togglePopover()">Date range</button>' +
      '<img src="https://cdn.example.com/logo.svg">' +
      "<script>function togglePopover(){document.getElementById('t').dataset.open=1;}</script>" +
      "</body></html>";
    const brokenDeploy = toolData(
      await callTool("deploy_page", { slug: "mcp-preflight", title: "Preflight", html: brokenHtml, render_mode: "raw", publish: true })
    );
    assert.equal(brokenDeploy.version_is_live, true, "preflight is advisory — it must never block a deploy");
    assert.equal(brokenDeploy.preflight.ok, false, "deploy results carry the findings without a second call");
    const codes = brokenDeploy.preflight.errors.map((e) => e.code).sort();
    assert.deepEqual(codes, ["inline_handler_shadowed", "remote_subresource_blocked"]);

    const checked = toolData(await callTool("preflight_page", { slug: "mcp-preflight" }));
    assert.equal(checked.version_id, brokenDeploy.version.id, "defaults to the published version");
    assert.equal(checked.preflight.ok, false);
    const shadowed = checked.preflight.errors.find((e) => e.code === "inline_handler_shadowed");
    assert.equal(shadowed.identifier, "togglePopover");
    assert.ok(shadowed.fix, "every finding carries a concrete fix");
    assert.equal(
      JSON.stringify(checked).includes("<!doctype html>"),
      false,
      "preflight reports findings, never the document — reading HTML back is the cost this avoids"
    );

    const fixedDeploy = toolData(
      await callTool("update_page", {
        slug: "mcp-preflight",
        html: brokenHtml.replace(/togglePopover/g, "drTogglePopover").replace(/<img[^>]*>/, ""),
        render_mode: "raw",
        publish: true,
      })
    );
    assert.equal(fixedDeploy.preflight.ok, true, JSON.stringify(fixedDeploy.preflight.errors));
    console.log("✓ preflight catches shadowed inline handlers and blocked subresources without returning HTML");

    // ── patch_page ──────────────────────────────────────────────────────────
    // The real repair, done the cheap way. Locate the anchor, send ~200 bytes of
    // edit, get a new immutable version — instead of reading 65 KB back and
    // re-emitting all of it.
    // Its own page, so the state under test is unambiguous.
    const patchTarget = toolData(
      await callTool("deploy_page", {
        slug: "mcp-patch",
        title: "Patch Target",
        html: brokenHtml,
        render_mode: "raw",
        publish: true,
      })
    );
    assert.equal(patchTarget.preflight.ok, false);

    const located = toolData(await callTool("find_in_version", { slug: "mcp-patch", query: "togglePopover" }));
    assert.equal(located.total_matches, 2, "the onclick attribute and the declaration");
    assert.ok(located.matches[0].line >= 1);
    // The point of search is that its cost is a constant per match, not a
    // fraction of the document — that is what makes it usable on a 300 KB page.
    for (const m of located.matches) assert.ok(m.excerpt.length <= 260, "excerpts stay bounded");

    const patched = toolData(
      await callTool("patch_page", {
        slug: "mcp-patch",
        edits: [{ find: "togglePopover", replace: "drTogglePopover", count: 2 }],
        note: "rename the shadowed inline handler",
      })
    );
    assert.equal(patched.version_is_live, true);
    assert.equal(patched.patch.edits_applied[0].count, 2);
    assert.equal(patched.patch.bytes_after - patched.patch.bytes_before, 2 * "dr".length);
    assert.equal(
      patched.preflight.errors.some((e) => e.code === "inline_handler_shadowed"),
      false,
      "the patch actually cleared the defect preflight reported"
    );
    const patchedHtml = (await db.query("SELECT html FROM page_versions WHERE id = $1", [patched.version.id]))
      .rows[0].html;
    assert.equal(patchedHtml.includes('onclick="drTogglePopover()"'), true);
    assert.equal(/[^r]togglePopover/.test(patchedHtml), false, "no stale references left behind");

    // An anchor that no longer matches is a hard error, never a silent no-op —
    // a patch that quietly did nothing is how a "fixed" dashboard ships unfixed.
    const stale = toolError(
      await callTool("patch_page", {
        slug: "mcp-patch",
        edits: [{ find: "togglePopover(", replace: "x(" }],
      })
    );
    assert.equal(stale.data.code, "patch_anchor_mismatch");
    assert.equal(stale.data.details.actual_count, 0);

    // Optimistic concurrency defaults to the version the patch was computed
    // from, so a patch cannot land on top of a deploy it never saw.
    const conflicting = toolError(
      await callTool("patch_page", {
        slug: "mcp-patch",
        edits: [{ find: "drTogglePopover", replace: "zz", count: 2 }],
        expected_version: "1",
      })
    );
    assert.equal(conflicting.data.code, "stale_version", conflicting.data.code);
    // Every caller of the shared check gets the id that overtook it, so losing a
    // race is a one-call recovery rather than a dead end.
    assert.equal(conflicting.data.details.expected_version, "1");
    assert.equal(conflicting.data.details.retry_with_expected_version, String(patched.version.id));
    assert.match(conflicting.data.error, /confirm version/, conflicting.data.error);

    const unchangedAfterFailures = toolData(await callTool("get_page", { slug: "mcp-patch" }));
    assert.equal(
      unchangedAfterFailures.page.published_version_id,
      patched.version.id,
      "a rejected patch leaves the live pointer exactly where it was"
    );
    // base_version_id names the version to patch FROM. The concurrency check is
    // about the live pointer, so defaulting expected_version to that base made
    // every explicit base_version_id fail stale_version against a page nobody
    // else had touched — the argument was unusable for the cases it exists for.
    const beforeRebase = toolData(await callTool("get_page", { slug: "mcp-patch" }));
    const older = toolData(
      await callTool("list_versions", { slug: "mcp-patch" })
    ).versions.find((v) => String(v.id) !== String(beforeRebase.page.published_version_id));
    assert.ok(older, "fixture: the page has an earlier version to patch from");
    const olderHtml = toolData(await callTool("get_version", { slug: "mcp-patch", version_id: older.id })).version.html;
    const anchor = olderHtml.includes("<main>") ? "<main>" : "<body>";
    const rebased = toolData(
      await callTool("patch_page", {
        slug: "mcp-patch",
        base_version_id: older.id,
        edits: [{ find: anchor, replace: `${anchor}<!-- rebased -->` }],
        publish: false,
      })
    );
    assert.equal(rebased.patch.base_version_id, String(older.id), "it patched the version it was told to");
    assert.equal(rebased.version_is_live, false, "publish:false leaves the pointer alone");
    assert.equal(
      toolData(await callTool("get_page", { slug: "mcp-patch" })).page.published_version_id,
      beforeRebase.page.published_version_id,
      "and the live pointer really did not move"
    );
    // …and with publish:true, which is the shape that actually 409'd before:
    // publishing a rebase republishes that version's bytes, discarding what was
    // deployed since. That is a real consequence, so it is asserted rather than
    // assumed — and the tool description now says so.
    const beforePublishRebase = toolData(await callTool("get_page", { slug: "mcp-patch" }));
    const publishedRebase = toolData(
      await callTool("patch_page", {
        slug: "mcp-patch",
        base_version_id: older.id,
        edits: [{ find: anchor, replace: `${anchor}<!-- rebased-live -->` }],
        publish: true,
      })
    );
    assert.equal(publishedRebase.version_is_live, true, "a published rebase goes live");
    assert.notEqual(
      String(publishedRebase.version.id),
      String(beforePublishRebase.page.published_version_id),
      "and it is a new version, not the one that was live"
    );

    // The default is the POINTER, not "no check at all". An explicitly stale
    // expected_version must still be refused — without this, setting the
    // default to null would pass every test here.
    const stalePatch = toolError(
      await callTool("patch_page", {
        slug: "mcp-patch",
        base_version_id: older.id,
        edits: [{ find: anchor, replace: `${anchor}<!-- nope -->` }],
        expected_version: older.id,
      })
    );
    assert.equal(stalePatch.data.code, "stale_version", "an explicitly stale expected_version is still refused");
    console.log("✓ patch_page can rebase off an explicit base_version_id without a false stale_version");

    console.log("✓ patch_page applies anchored edits server-side, fails closed on stale anchors and versions");

    // Real dashboard files are larger than a reliable model-generated tool
    // argument. Stage exact bytes in bounded calls, verify their hash, and
    // atomically enter the same immutable deploy/publish state machine.
    const largeHtml =
      '<!doctype html><html><head><meta charset="utf-8"><title>Large MCP</title></head><body><main>' +
      Array.from(
        { length: 1800 },
        (_, i) => `<section data-row="${i}">Dashboard row ${i}: complete source data \u2600</section>`
      ).join("") +
      '</main><script>window.dashboardReady=true;</script></body></html>';
    const largeBytes = Buffer.from(largeHtml, "utf8");
    assert.ok(largeBytes.length > 100_000, "fixture is representative of the failed production dashboards");
    const largeSha = crypto.createHash("sha256").update(largeBytes).digest("hex");
    const upload = toolData(
      await callTool("start_page_upload", {
        slug: "mcp-large-upload",
        total_bytes: largeBytes.length,
        content_sha256: largeSha,
      })
    );
    assert.equal(upload.slug, "mcp-large-upload");
    assert.equal(upload.bytes_received, 0);
    assert.equal(upload.next_sequence, 0);
    assert.equal(upload.complete, false);
    assert.equal(upload.max_chunk_bytes, pageUploads.MAX_CHUNK_BYTES);
    assert.match(upload.next_step, /append_page_upload/);

    const incomplete = toolError(
      await callTool("deploy_page_upload", { upload_id: upload.upload_id, render_mode: "raw" })
    );
    assert.equal(incomplete.data.code, "page_upload_incomplete");
    const firstChunk = largeBytes.subarray(0, upload.max_chunk_bytes).toString("base64");
    const outOfOrder = toolError(
      await callTool("append_page_upload", {
        upload_id: upload.upload_id,
        sequence: 1,
        chunk_base64: firstChunk,
      })
    );
    assert.equal(outOfOrder.data.code, "upload_sequence_conflict");
    assert.equal(outOfOrder.data.details.expected_sequence, 0);
    // The eight cancels in the observed 10M-token session were all recovery
    // attempts from exactly this error. Nothing in it said the cheap recovery
    // existed, so the caller reached for the expensive one every time.
    assert.equal(outOfOrder.data.details.resumable, true);
    assert.equal(outOfOrder.data.details.bytes_received, 0);
    assert.equal(outOfOrder.data.details.total_bytes, largeBytes.length);
    assert.match(outOfOrder.data.error, /Do NOT cancel/);
    assert.match(outOfOrder.data.error, /A FAILED append does not advance the sequence/);
    // And the advice is true: the very next call at the expected sequence lands.
    const resumed = toolData(
      await callTool("append_page_upload", { upload_id: upload.upload_id, sequence: 0, chunk_base64: firstChunk })
    );
    assert.equal(resumed.next_sequence, 1, "the conflict cost nothing but the one call");
    assert.equal(resumed.deduped, false);
    // Replaying that same chunk is a no-op, not a second conflict.
    const replayed = toolData(
      await callTool("append_page_upload", { upload_id: upload.upload_id, sequence: 0, chunk_base64: firstChunk })
    );
    assert.equal(replayed.deduped, true);
    assert.equal(replayed.next_sequence, 1);
    // Re-sending an accepted sequence with DIFFERENT bytes is the one genuine
    // conflict, and it is still resumable rather than terminal.
    const contradicted = toolError(
      await callTool("append_page_upload", {
        upload_id: upload.upload_id,
        sequence: 0,
        chunk_base64: Buffer.from("different").toString("base64"),
      })
    );
    assert.equal(contradicted.data.code, "upload_sequence_conflict");
    assert.equal(contradicted.data.details.resumable, true);
    assert.equal(contradicted.data.details.expected_sequence, 1);
    assert.match(contradicted.data.error, /DIFFERENT bytes/);

    const otherDeployToken = await tokens.mint({ label: "mcp-upload-other", scope: "deploy" });
    const crossTokenAppend = toolError(
      await callTool(
        "append_page_upload",
        { upload_id: upload.upload_id, sequence: 0, chunk_base64: firstChunk },
        { token: otherDeployToken.token }
      )
    );
    assert.equal(crossTokenAppend.data.code, "page_upload_not_found", "upload handles are token-bound");

    let finalUpload;
    let sequence = 1; // sequence 0 already landed via the resume above
    for (let offset = upload.max_chunk_bytes; offset < largeBytes.length; offset += upload.max_chunk_bytes) {
      const chunk = largeBytes.subarray(offset, offset + upload.max_chunk_bytes).toString("base64");
      finalUpload = toolData(
        await callTool("append_page_upload", {
          upload_id: upload.upload_id,
          sequence,
          chunk_base64: chunk,
        })
      );
      if (sequence === 1) {
        const replay = toolData(
          await callTool("append_page_upload", {
            upload_id: upload.upload_id,
            sequence,
            chunk_base64: chunk,
          })
        );
        assert.equal(replay.deduped, true, "ambiguous chunk retries are idempotent");
        assert.equal(replay.next_sequence, 2);
      }
      sequence++;
    }
    assert.equal(finalUpload.complete, true);
    assert.equal(finalUpload.bytes_received, largeBytes.length);
    assert.equal(finalUpload.next_sequence, sequence);
    assert.match(finalUpload.next_step, /deploy_page_upload/);
    // The advertised ceiling and the page_content_upload_chunks CHECK are two
    // separate declarations of the same number in two languages. Raising one
    // without the other fails every append with an opaque internal_error, which
    // is exactly how migration 013 got caught. Prove a full-size chunk lands.
    assert.ok(
      upload.max_chunk_bytes >= 32 * 1024,
      `max_chunk_bytes ${upload.max_chunk_bytes} is too small to keep deploys inside one model turn`
    );
    assert.ok(
      largeBytes.length > upload.max_chunk_bytes,
      "the fixture must be large enough to force at least one full-size chunk"
    );
    console.log(`✓ staged upload accepted ${sequence} chunk(s) at ${upload.max_chunk_bytes} bytes`);

    const stagedDeployArgs = {
      upload_id: upload.upload_id,
      title: "Large MCP Dashboard",
      render_mode: "raw",
      publish: true,
      note: "Staged exact-byte integration deploy",
    };
    const staged = toolData(await callTool("deploy_page_upload", stagedDeployArgs));
    assert.equal(staged.upload_id, upload.upload_id);
    assert.equal(staged.created, true);
    assert.equal(staged.version_is_live, true);
    assert.equal(staged.page_is_live, true);
    assert.equal(staged.version.content_sha256, largeSha);
    const stagedReplay = toolData(await callTool("deploy_page_upload", stagedDeployArgs));
    assert.deepEqual(stagedReplay, staged, "committed upload retries return the original result");
    const changedCommit = toolError(
      await callTool("deploy_page_upload", { ...stagedDeployArgs, render_mode: "themed" })
    );
    assert.equal(changedCommit.data.code, "page_upload_commit_conflict", "retry options are bound to the commit");
    const storedLarge = await db.query("SELECT html FROM page_versions WHERE id = $1", [staged.version.id]);
    assert.equal(storedLarge.rows[0].html, largeHtml, "UTF-8 dashboard bytes survive chunking exactly");
    const stagedAudit = await db.query(
      "SELECT count(*)::integer AS count FROM audit_log WHERE action = 'deploy' AND version_id = $1",
      [staged.version.id]
    );
    assert.equal(stagedAudit.rows[0].count, 1, "commit retry does not duplicate the version audit");
    const stagedChunks = await db.query(
      "SELECT count(*)::integer AS count FROM page_content_upload_chunks WHERE upload_id = $1",
      [upload.upload_id]
    );
    assert.equal(stagedChunks.rows[0].count, 0, "committed chunk bytes are released");
    const crossTokenDeploy = toolError(
      await callTool("deploy_page_upload", { upload_id: upload.upload_id }, { token: otherDeployToken.token })
    );
    assert.equal(crossTokenDeploy.data.code, "page_upload_not_found");
    assert.equal(await tokens.revoke(otherDeployToken.id), true);

    // The production failure this recovery path exists for: a host that stages
    // writes for human approval freezes expected_version when the card is
    // staged, so by the time a human clicks Approve another queued write has
    // moved the pointer and the deploy loses a race it never saw. It cost a real
    // session a full turn, because "published version changed" does not say
    // that the 78 KB already uploaded is still sitting there, deployable.
    const raceBytes = Buffer.from(largeHtml.replace("dashboardReady", "raceReady"), "utf8");
    const raceUpload = toolData(
      await callTool("create_upload_ticket", {
        slug: "mcp-large-upload",
        total_bytes: raceBytes.length,
        content_sha256: crypto.createHash("sha256").update(raceBytes).digest("hex"),
      })
    );
    const raceSent = await request("PUT", `/upload/${raceUpload.upload_id}`, {
      rawBody: raceBytes,
      token: raceUpload.ticket,
    });
    assert.equal(raceSent.status, 200, raceSent.body);
    assert.equal(raceSent.json.complete, true);
    // The pointer moves between the read and the deploy, exactly as an earlier
    // approval card landing first would move it.
    const overtaking = toolData(
      await callTool("patch_page", {
        slug: "mcp-large-upload",
        edits: [{ find: "<title>Large MCP</title>", replace: "<title>Overtaken</title>" }],
        note: "the write that wins the race",
      })
    );
    const lostRace = toolError(
      await callTool("deploy_page_upload", {
        upload_id: raceUpload.upload_id,
        render_mode: "raw",
        expected_version: staged.version.id,
      })
    );
    assert.equal(lostRace.data.code, "stale_version");
    assert.equal(lostRace.data.details.expected_version, String(staged.version.id));
    assert.equal(lostRace.data.details.retry_with_expected_version, String(overtaking.version.id));
    assert.equal(lostRace.data.details.upload_id, raceUpload.upload_id);
    assert.equal(lostRace.data.details.upload_still_staged, true);
    assert.match(lostRace.data.error, /still deployable/, lostRace.data.error);
    assert.match(lostRace.data.error, /do not re-upload/, lostRace.data.error);
    // And the advice is true: the same upload_id lands with the corrected id,
    // no second transfer of the bytes.
    const wonOnRetry = toolData(
      await callTool("deploy_page_upload", {
        upload_id: raceUpload.upload_id,
        render_mode: "raw",
        expected_version: overtaking.version.id,
      })
    );
    assert.equal(wonOnRetry.version.content_sha256, crypto.createHash("sha256").update(raceBytes).digest("hex"));
    console.log("✓ a deploy that loses a version race keeps its staged bytes and says how to recover");

    // Upload commit and the ordinary version/audit transaction are one unit:
    // a failure after version creation rolls everything back while retaining
    // the verified upload for a safe retry.
    const atomicHtml = "<!doctype html><html><body>atomic staged content</body></html>";
    const atomicBytes = Buffer.from(atomicHtml);
    const atomicUpload = toolData(
      await callTool("start_page_upload", {
        slug: "mcp-upload-atomic",
        total_bytes: atomicBytes.length,
        content_sha256: crypto.createHash("sha256").update(atomicBytes).digest("hex"),
      })
    );
    toolData(
      await callTool("append_page_upload", {
        upload_id: atomicUpload.upload_id,
        sequence: 0,
        chunk_base64: atomicBytes.toString("base64"),
      })
    );
    const actorCtx = {
      actor: "mcp-agent",
      actorType: "agent",
      tokenId: minted.id,
      ip: "127.0.0.1",
      transport: "mcp",
    };
    await assert.rejects(
      () =>
        pageUploads.deploy(atomicUpload.upload_id, actorCtx, "a".repeat(64), async (client, stagedContent) => {
          await versions.createAndDeployWithClient(
            client,
            {
              slug: stagedContent.slug,
              html: stagedContent.html,
              renderMode: "raw",
              source: "mcp",
              publish: true,
              title: "Must Roll Back",
            },
            actorCtx
          );
          throw new Error("simulated failure after version creation");
        }),
      /simulated failure/
    );
    assert.equal(
      (await db.query("SELECT count(*)::integer AS count FROM pages WHERE slug = 'mcp-upload-atomic'")).rows[0].count,
      0,
      "failed commit cannot strand a page/version"
    );
    const atomicUploadRow = await db.query(
      "SELECT bytes_received, commit_result FROM page_content_uploads WHERE id = $1",
      [atomicUpload.upload_id]
    );
    assert.equal(Number(atomicUploadRow.rows[0].bytes_received), atomicBytes.length);
    assert.equal(atomicUploadRow.rows[0].commit_result, null, "verified upload remains retryable");
    const atomicRetry = toolData(
      await callTool("deploy_page_upload", {
        upload_id: atomicUpload.upload_id,
        title: "Atomic Upload",
        render_mode: "raw",
      })
    );
    assert.equal(atomicRetry.created, true);
    assert.equal(atomicRetry.version_is_live, true);

    const placeholder = toolError(
      await callTool("deploy_page", { slug: "mcp-placeholder", html: "$(cat dashboard.html)" })
    );
    assert.equal(placeholder.data.code, "html_placeholder");
    assert.equal(
      (await db.query("SELECT count(*)::integer AS count FROM pages WHERE slug = 'mcp-placeholder'")).rows[0].count,
      0,
      "literal file placeholders never create pages"
    );
    const intended = Buffer.from("good");
    const badHashUpload = toolData(
      await callTool("start_page_upload", {
        slug: "mcp-upload-bad-hash",
        total_bytes: intended.length,
        content_sha256: crypto.createHash("sha256").update(intended).digest("hex"),
      })
    );
    const badHash = toolError(
      await callTool("append_page_upload", {
        upload_id: badHashUpload.upload_id,
        sequence: 0,
        chunk_base64: Buffer.from("evil").toString("base64"),
      })
    );
    assert.equal(badHash.data.code, "upload_hash_mismatch");
    const badHashRow = await db.query("SELECT bytes_received, next_sequence FROM page_content_uploads WHERE id = $1", [
      badHashUpload.upload_id,
    ]);
    assert.equal(Number(badHashRow.rows[0].bytes_received), 0, "bad final hash rolls back its chunk");
    assert.equal(Number(badHashRow.rows[0].next_sequence), 0);
    const cancelled = toolData(
      await callTool("cancel_page_upload", { upload_id: badHashUpload.upload_id })
    );
    assert.equal(cancelled.cancelled, true);
    const cancelledReplay = toolData(
      await callTool("cancel_page_upload", { upload_id: badHashUpload.upload_id })
    );
    assert.equal(cancelledReplay.cancelled, false, "cancellation retries are harmless");
    // The cancel response no longer reads as an invitation to start another one.
    assert.match(cancelled.next_step, /re-emits the whole document|create_upload_ticket/);

    // A caller stuck in the loop is told so. Cancelling deletes the upload row,
    // so before this there was nothing left to notice the pattern with and the
    // tenth attempt looked exactly like the first.
    const churnBytes = Buffer.from(HTML(60), "utf8");
    const churnStart = () =>
      callTool("start_page_upload", {
        slug: "mcp-upload-churn",
        total_bytes: churnBytes.length,
        content_sha256: crypto.createHash("sha256").update(churnBytes).digest("hex"),
      });
    const churn1 = toolData(await churnStart());
    assert.doesNotMatch(churn1.next_step, /does not converge/, "the first attempt is not scolded");
    toolData(await callTool("cancel_page_upload", { upload_id: churn1.upload_id }));
    const churn2 = toolData(await churnStart());
    toolData(await callTool("cancel_page_upload", { upload_id: churn2.upload_id }));
    const churn3 = toolData(await churnStart());
    assert.match(churn3.next_step, /does not converge/);
    assert.match(churn3.next_step, /create_upload_ticket/);
    assert.match(churn3.next_step, /started 3 chunked uploads for this target and cancelled 2/);
    toolData(await callTool("cancel_page_upload", { upload_id: churn3.upload_id }));

    // Taking the advice is not itself an attempt — a caller who arrives at the
    // ticket must not be scolded for arriving.
    const churnTicket = toolData(
      await callTool("create_upload_ticket", {
        slug: "mcp-upload-churn",
        total_bytes: churnBytes.length,
        content_sha256: crypto.createHash("sha256").update(churnBytes).digest("hex"),
      })
    );
    assert.doesNotMatch(churnTicket.next_step, /does not converge/);
    toolData(await callTool("cancel_page_upload", { upload_id: churnTicket.upload_id }));
    console.log("✓ staged MCP upload preserves large HTML, token binding, retry safety, and atomic deploys");

    // ── Out-of-band upload ticket ───────────────────────────────────────────
    // The whole point: the bytes reach Pages over HTTP instead of through the
    // model's output. A 300 KB dashboard was previously undeployable — 26
    // flawless base64 tool calls, ~430 KB of tokens — and an agent shrank a
    // client deliverable to fit. This path costs a URL and a handle.
    const ticketHtml =
      '<!doctype html><html><head><meta charset="utf-8"><title>Ticketed</title></head><body><main>' +
      Array.from({ length: 2400 }, (_, i) => `<p data-r="${i}">row ${i} \u2600</p>`).join("") +
      "</main></body></html>";
    const ticketBytes = Buffer.from(ticketHtml, "utf8");
    const ticketSha = crypto.createHash("sha256").update(ticketBytes).digest("hex");
    assert.ok(ticketBytes.length > 64 * 1024, "fixture must exceed one chunk");

    const ticketed = toolData(
      await callTool("create_upload_ticket", {
        slug: "mcp-ticket-upload",
        total_bytes: ticketBytes.length,
        content_sha256: ticketSha,
      })
    );
    assert.match(ticketed.ticket, /^pgu_/);
    assert.equal(ticketed.complete, false);
    assert.equal(ticketed.bytes_received, 0);
    assert.match(ticketed.upload_url, /\/upload\//);
    assert.match(ticketed.curl, /--data-binary/);
    const ticketPath = `/upload/${ticketed.upload_id}`;

    // A ticket is not an agent token: it opens exactly one door.
    const ticketNoAuth = await request("PUT", ticketPath, { rawBody: ticketBytes, token: null });
    assert.equal(ticketNoAuth.status, 401);
    assert.equal(ticketNoAuth.json.code, "upload_ticket_missing");

    const agentTokenInsteadOfTicket = await request("PUT", ticketPath, { rawBody: ticketBytes });
    assert.equal(agentTokenInsteadOfTicket.status, 401, "a deploy token is not an upload ticket");

    // Content is pinned at mint time, so a stolen ticket cannot substitute bytes.
    const wrongBytes = await request("PUT", ticketPath, {
      rawBody: Buffer.concat([ticketBytes.subarray(0, ticketBytes.length - 1), Buffer.from("X")]),
      token: ticketed.ticket,
    });
    assert.equal(wrongBytes.status, 409);
    assert.equal(wrongBytes.json.code, "upload_hash_mismatch");

    const shortBytes = await request("PUT", ticketPath, {
      rawBody: ticketBytes.subarray(0, 100),
      token: ticketed.ticket,
    });
    assert.equal(shortBytes.status, 400);
    assert.equal(shortBytes.json.code, "upload_size_mismatch");

    // The ticket is bound to its own upload, not merely to a valid signature.
    const otherTicket = toolData(
      await callTool("create_upload_ticket", {
        slug: "mcp-ticket-other",
        total_bytes: ticketBytes.length,
        content_sha256: ticketSha,
      })
    );
    const crossUpload = await request("PUT", ticketPath, {
      rawBody: ticketBytes,
      token: otherTicket.ticket,
    });
    assert.equal(crossUpload.status, 401, "a ticket for another upload must not work here");
    assert.equal(crossUpload.json.code, "upload_ticket_invalid");
    toolData(await callTool("cancel_page_upload", { upload_id: otherTicket.upload_id }));

    const sent = await request("PUT", ticketPath, { rawBody: ticketBytes, token: ticketed.ticket });
    assert.equal(sent.status, 200, sent.body);
    assert.equal(sent.json.complete, true);
    assert.equal(sent.json.bytes_received, ticketBytes.length);
    assert.equal(sent.json.deduped, false);
    assert.match(sent.json.next_step, /deploy_page_upload/);

    // A retried curl of the identical file is a safe no-op, not an error.
    const resent = await request("PUT", ticketPath, { rawBody: ticketBytes, token: ticketed.ticket });
    assert.equal(resent.status, 200, resent.body);
    assert.equal(resent.json.deduped, true);

    // ...but a spent ticket cannot be turned on different content.
    const spentDifferent = await request("PUT", ticketPath, {
      rawBody: Buffer.from("<!doctype html><p>substituted</p>"),
      token: ticketed.ticket,
    });
    assert.equal(spentDifferent.status, 409);
    assert.equal(spentDifferent.json.code, "upload_ticket_used");

    const ticketDeploy = toolData(
      await callTool("deploy_page_upload", {
        upload_id: ticketed.upload_id,
        title: "Ticketed Dashboard",
        render_mode: "raw",
        publish: true,
      })
    );
    assert.equal(ticketDeploy.created, true);
    assert.equal(ticketDeploy.version_is_live, true);
    assert.equal(ticketDeploy.version.content_sha256, ticketSha);
    const storedTicketed = await db.query("SELECT html FROM page_versions WHERE id = $1", [
      ticketDeploy.version.id,
    ]);
    assert.equal(storedTicketed.rows[0].html, ticketHtml, "out-of-band bytes survive byte-for-byte");
    assert.equal(ticketDeploy.preflight.ok, true, JSON.stringify(ticketDeploy.preflight.errors));

    // Only PUT is offered, and the ticket is dead once the upload is committed.
    const wrongMethod = await request("GET", ticketPath, { token: ticketed.ticket });
    assert.equal(wrongMethod.status, 405);
    const afterCommit = await request("PUT", ticketPath, { rawBody: ticketBytes, token: ticketed.ticket });
    assert.equal(afterCommit.status, 409);
    assert.equal(afterCommit.json.code, "page_upload_committed");
    console.log(
      `✓ upload ticket moved ${ticketBytes.length} bytes out of band (content-pinned, single-use, deploy still needs the agent token)`
    );

    // ── Templates: register once, build many ────────────────────────────────
    // The cost problem this solves: the second dashboard of a family used to
    // cost the whole design again. Registering the design once means a page
    // costs the config that actually differs, and get_template answers "what
    // does this design need?" without pulling the HTML back into context.
    const templateBytes = Buffer.from(TEMPLATE_HTML(1), "utf8");
    const templateSha = crypto.createHash("sha256").update(templateBytes).digest("hex");
    const templateTicket = toolData(
      await callTool("create_upload_ticket", {
        template: "mcp-campaign",
        total_bytes: templateBytes.length,
        content_sha256: templateSha,
      })
    );
    assert.equal(templateTicket.target_kind, "template");
    assert.equal(templateTicket.template, "mcp-campaign");
    assert.equal(templateTicket.slug, null, "a template upload reports no slug");
    const templatePut = await request("PUT", `/upload/${templateTicket.upload_id}`, {
      rawBody: templateBytes,
      token: templateTicket.ticket,
    });
    assert.equal(templatePut.status, 200, templatePut.body);
    assert.match(templatePut.json.next_step, /register_template_upload/);

    // A template upload is not deployable as a page. Without this, a skeleton
    // whose data block is deliberately empty could be published to a client.
    const wrongConsumer = toolError(await callTool("deploy_page_upload", { upload_id: templateTicket.upload_id }));
    assert.equal(wrongConsumer.data.code, "page_upload_target_mismatch");
    assert.match(wrongConsumer.data.error, /staged for a template/i);
    assert.equal(wrongConsumer.data.details.target_kind, "template");

    // A dry run over the STAGED bytes. This is the whole point of the tool: the
    // file was PUT once through the ticket, so checking it must not cost a second
    // upload and must not spend the first one.
    const dryRun = toolData(await callTool("validate_template", { upload_id: templateTicket.upload_id }));
    assert.equal(dryRun.contract_ok, true, JSON.stringify(dryRun.contract_error));
    assert.equal(dryRun.name, "mcp-campaign", "the upload already names the template");
    assert.equal(dryRun.bytes, templateBytes.length);
    assert.equal(dryRun.ships_empty, true);
    assert.equal(dryRun.has_sample_data, true, "the fixture design carries example rows");
    assert.deepEqual(dryRun.sample_data_keys, ["rows"]);
    assert.equal(dryRun.preflight.ok, true, JSON.stringify(dryRun.preflight.errors));
    assert.match(dryRun.next_step, /register_template_upload/);

    // Nothing was written and the upload survives — the same upload_id registers
    // below. A validator that consumed the upload would be useless.
    assert.equal(
      toolData(await callTool("list_templates", {})).templates.some((t) => t.name === "mcp-campaign"),
      false,
      "a dry run must not register anything"
    );

    // It reports a broken design instead of throwing, and refuses an ambiguous
    // call rather than guessing which source was meant.
    const badDry = toolData(await callTool("validate_template", { html: "<p>not a template</p>", name: "nope" }));
    assert.equal(badDry.contract_ok, false);
    assert.equal(badDry.contract_error.code, "template_contract_invalid");
    assert.ok(badDry.preflight, "preflight still runs so both problems surface in one pass");
    assert.equal(
      toolError(await callTool("validate_template", { html: "<p>x</p>", upload_id: templateTicket.upload_id })).data.code,
      "template_source_ambiguous"
    );
    assert.equal(toolError(await callTool("validate_template", {})).data.code, "template_source_ambiguous");

    const registered = toolData(
      await callTool("register_template_upload", {
        upload_id: templateTicket.upload_id,
        title: "MCP Campaign Dashboard",
        description: "Integration fixture for the template contract.",
      })
    );
    assert.equal(registered.created, true);
    assert.equal(registered.deduped, false);
    assert.equal(registered.template.name, "mcp-campaign");
    assert.equal(registered.revision.revision, 1);
    assert.equal(registered.revision.content_sha256, templateSha);
    assert.deepEqual(registered.reference_config, { campaign: "Reference", channel: "display" });
    assert.equal(registered.config_schema.type, "object");
    assert.equal(registered.data_schema.type, "object");
    assert.equal(registered.preflight.ok, true, JSON.stringify(registered.preflight.errors));

    // Exact-retry safety: the same bytes are the same revision, not revision 2.
    const templateRetryBytes = Buffer.from(TEMPLATE_HTML(1), "utf8");
    const retryTicket = toolData(
      await callTool("create_upload_ticket", {
        template: "mcp-campaign",
        total_bytes: templateRetryBytes.length,
        content_sha256: templateSha,
      })
    );
    await request("PUT", `/upload/${retryTicket.upload_id}`, {
      rawBody: templateRetryBytes,
      token: retryTicket.ticket,
    });
    const reRegistered = toolData(await callTool("register_template_upload", { upload_id: retryTicket.upload_id }));
    assert.equal(reRegistered.deduped, true);
    assert.equal(reRegistered.created, false);
    assert.equal(reRegistered.revision.revision, 1, "identical bytes do not invent a revision");

    // get_template is the cheap read: schemas and reference config, no HTML.
    const fetched = toolData(await callTool("get_template", { template: "mcp-campaign" }));
    assert.equal(fetched.template.current_revision, 1);
    assert.equal(fetched.html, undefined, "HTML must be omitted unless asked for");
    assert.deepEqual(fetched.config_schema, registered.config_schema);
    assert.ok(
      JSON.stringify(fetched).length < templateBytes.length,
      "the whole point: reading the contract must cost less than reading the design"
    );
    const withHtml = toolData(await callTool("get_template", { template: "mcp-campaign", include_html: true }));
    assert.equal(withHtml.html, TEMPLATE_HTML(1), "include_html returns the exact registered bytes");

    const listedTemplates = toolData(await callTool("list_templates", {}));
    const mine = listedTemplates.templates.find((entry) => entry.name === "mcp-campaign");
    assert.equal(mine.title, "MCP Campaign Dashboard");
    assert.equal(mine.current_revision, 1);
    assert.equal(mine.page_count, 0, "no pages built from it yet");

    const templateRevisions = toolData(await callTool("list_template_revisions", { template: "mcp-campaign" }));
    assert.equal(templateRevisions.revisions.length, 1);
    assert.equal(templateRevisions.revisions[0].is_current, true);

    // Registration validates the design instead of trusting it: HTML that is
    // missing the config pair is not a template, however well-formed a page.
    const notATemplate = Buffer.from(HTML(9), "utf8");
    const notATemplateTicket = toolData(
      await callTool("create_upload_ticket", {
        template: "mcp-not-a-template",
        total_bytes: notATemplate.length,
        content_sha256: crypto.createHash("sha256").update(notATemplate).digest("hex"),
      })
    );
    await request("PUT", `/upload/${notATemplateTicket.upload_id}`, {
      rawBody: notATemplate,
      token: notATemplateTicket.ticket,
    });
    const rejected = toolError(await callTool("register_template_upload", { upload_id: notATemplateTicket.upload_id }));
    assert.equal(rejected.data.code, "template_contract_invalid");
    const noGhost = toolError(await callTool("get_template", { template: "mcp-not-a-template" }));
    assert.equal(noGhost.data.code, "template_not_found", "a rejected registration leaves nothing behind");

    // Exactly one target, never both and never neither.
    for (const args of [
      { slug: "mcp-both", template: "mcp-both", total_bytes: 10, content_sha256: templateSha },
      { total_bytes: 10, content_sha256: templateSha },
    ]) {
      assert.equal(toolError(await callTool("create_upload_ticket", args)).data.code, "upload_target_required");
    }
    console.log("✓ template registration: contract-validated, retry-safe, cheap to read, and not page-deployable");

    // ── Building pages from one template ────────────────────────────────────
    // Two campaigns, one design. Each create sends only the config that
    // differs — the measurement below is the entire justification for the
    // feature.
    const configA = { campaign: "Acme Spring", channel: "display" };
    const configB = { campaign: "Globex Q3", channel: "video" };
    const builtA = toolData(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-acme",
        config: configA,
        title: "Acme Spring",
        render_mode: "raw",
        publish: true,
      })
    );
    assert.equal(builtA.created, true);
    assert.equal(builtA.version_is_live, true);
    assert.equal(builtA.template, "mcp-campaign");
    assert.equal(builtA.template_revision, 1);
    assert.deepEqual(builtA.config, configA);
    assert.equal(builtA.preflight.ok, true, JSON.stringify(builtA.preflight.errors));
    // Deployed empty: the design's awaiting-first-ingest state, never a zero.
    assert.deepEqual(builtA.envelope.data, { rows: [] });
    assert.equal(
      builtA.envelope.source_as_of,
      "1970-01-01T00:00:00.000Z",
      "a page created without data claims no source coverage, so the first ingest cannot regress"
    );

    // The template's example dataset is PREVIEW-ONLY. It must not survive into a
    // page — neither the block nor its rows — or every page built from this
    // design would ship one fictional campaign's numbers as if they were real.
    for (const needle of ["pages-data-example", "1111", "2222"]) {
      const found = toolData(await callTool("find_in_version", { slug: "mcp-tpl-acme", query: needle }));
      assert.equal(
        found.total_matches,
        0,
        `a page built from the template still contains ${needle}; the example block must be deleted`
      );
    }
    assert.deepEqual(
      toolData(await callTool("get_page_data", { slug: "mcp-tpl-acme" })).envelope.data,
      { rows: [] },
      "the page's own data is still the empty state"
    );
    // And the template itself still reports that it HAS one.
    assert.equal(
      toolData(await callTool("get_template", { template: "mcp-campaign" })).revision.has_sample_data,
      true,
      "the template keeps its example dataset even though pages never receive it"
    );

    const builtB = toolData(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-globex",
        config: configB,
        data: { rows: [{ date: "2026-08-01", spend: 125.5 }] },
        source_as_of: "2026-08-01T12:00:00Z",
        render_mode: "raw",
        publish: true,
      })
    );
    assert.equal(builtB.created, true);
    assert.deepEqual(builtB.envelope.data, { rows: [{ date: "2026-08-01", spend: 125.5 }] });

    // The design is byte-identical between the two pages; only the two managed
    // payloads differ. That is what a template is for.
    const htmlA = (await db.query("SELECT html FROM page_versions WHERE id = $1", [builtA.version.id])).rows[0].html;
    const htmlB = (await db.query("SELECT html FROM page_versions WHERE id = $1", [builtB.version.id])).rows[0].html;
    assert.ok(htmlA.includes('<div id="rev">design v1</div>'), "the stored design is what got rendered");
    assert.equal(
      htmlA.replace(/<script (?:type="application\/json" )?id="pages-(?:config|data)"[^>]*>[^<]*<\/script>/g, "«payload»"),
      htmlB.replace(/<script (?:type="application\/json" )?id="pages-(?:config|data)"[^>]*>[^<]*<\/script>/g, "«payload»"),
      "two pages from one revision differ ONLY in their config and data blocks"
    );
    // Cost, stated plainly: the create arguments versus the design they rendered.
    const createCost = JSON.stringify({ template: "mcp-campaign", slug: "mcp-tpl-acme", config: configA }).length;
    assert.ok(
      createCost * 8 < htmlA.length,
      `building a page must cost far less than its design (${createCost} vs ${htmlA.length} bytes)`
    );
    console.log(
      `✓ two pages from one template revision: ${createCost} bytes of arguments rendered ${htmlA.length} bytes of design`
    );

    // list_templates now attributes both pages to the template.
    const afterBuilds = toolData(await callTool("list_templates", {}));
    assert.equal(afterBuilds.templates.find((entry) => entry.name === "mcp-campaign").page_count, 2);

    // A complete config is required; a partial one is not silently completed
    // from the reference config, which is how one client's identity would
    // otherwise end up on another client's page.
    const partial = toolError(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-partial",
        config: { campaign: "No Channel" },
      })
    );
    assert.equal(partial.data.code, "config_validation_failed");
    assert.equal(toolError(await callTool("get_page", { slug: "mcp-tpl-partial" })).data.code, "page_not_found");

    // An identical retry is a no-op (a turn can die after the commit); anything
    // else refuses rather than silently replacing a live dashboard.
    const retried = toolData(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-acme",
        config: configA,
        title: "Acme Spring",
        render_mode: "raw",
        publish: true,
      })
    );
    assert.equal(retried.deduped, true);
    assert.equal(retried.version.id, builtA.version.id, "an identical retry returns the same immutable version");
    const clobber = toolError(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-acme",
        config: { campaign: "Hostile Takeover", channel: "olv" },
      })
    );
    assert.equal(clobber.data.code, "page_exists");
    assert.match(clobber.data.error, /update_page_config/);

    // ── config and data are genuinely independent ───────────────────────────
    const readConfig = toolData(await callTool("get_page_config", { slug: "mcp-tpl-acme" }));
    assert.deepEqual(readConfig.config, configA);
    assert.equal(readConfig.config_schema.type, "object");
    assert.equal(readConfig.live_version_id, builtA.version.id);

    // First a refresh, so there are real numbers for the config edit to leave alone.
    const dataState = toolData(await callTool("get_page_data", { slug: "mcp-tpl-acme" }));
    const refreshed = toolData(
      await callTool("update_page_data", {
        slug: "mcp-tpl-acme",
        data: { rows: [{ date: "2026-08-02", spend: 42 }] },
        source_as_of: "2026-08-02T00:00:00Z",
        expected_version: dataState.live_version_id,
      })
    );
    assert.equal(refreshed.version_is_live, true, "update_page_data works unchanged on a template-built page");
    const refreshedHtml = (
      await db.query("SELECT html FROM page_versions WHERE id = $1", [refreshed.version.id])
    ).rows[0].html;
    assert.ok(refreshedHtml.includes(JSON.stringify(configA)), "a refresh leaves config bytes alone");

    // Now the config edit. The data block must come out byte-for-byte identical.
    const beforeEdit = toolData(await callTool("get_page_config", { slug: "mcp-tpl-acme" }));
    const renamedConfig = { campaign: "Acme Spring (renamed)", channel: "olv" };
    const configUpdated = toolData(
      await callTool("update_page_config", {
        slug: "mcp-tpl-acme",
        config: renamedConfig,
        expected_version: beforeEdit.live_version_id,
      })
    );
    assert.equal(configUpdated.version_is_live, true);
    assert.deepEqual(configUpdated.config, renamedConfig);
    assert.deepEqual(configUpdated.envelope, refreshed.envelope, "the envelope is carried across untouched");
    assert.equal(configUpdated.data_sha256, refreshed.data_sha256, "numbers cannot move during a config edit");
    assert.notEqual(
      configUpdated.template_sha256,
      refreshed.template_sha256,
      "config is part of the template identity, so a later refresh cannot dedupe back across this edit"
    );
    const editedHtml = (
      await db.query("SELECT html FROM page_versions WHERE id = $1", [configUpdated.version.id])
    ).rows[0].html;
    assert.ok(editedHtml.includes(JSON.stringify(renamedConfig)));
    assert.ok(
      editedHtml.includes(JSON.stringify(refreshed.envelope).replace(/</g, "\\u003c")),
      "the data block survives a config edit byte-for-byte"
    );

    // Provenance survives both operations.
    const provenance = await db.query(
      `SELECT v.template_version_id, v.config_sha256, t.revision
         FROM page_versions v JOIN page_template_versions t ON t.id = v.template_version_id
        WHERE v.id = ANY($1::bigint[]) ORDER BY v.id`,
      [[builtA.version.id, refreshed.version.id, configUpdated.version.id]]
    );
    assert.equal(provenance.rows.length, 3, "create, refresh and config edit all stay attributable");
    assert.ok(provenance.rows.every((row) => Number(row.revision) === 1));

    // expected_version is mandatory, and a stale one is refused.
    const staleConfig = toolError(
      await callTool("update_page_config", {
        slug: "mcp-tpl-acme",
        config: configA,
        expected_version: builtA.version.id,
      })
    );
    assert.equal(staleConfig.data.code, "stale_version");

    // ── the retry that must NOT be a no-op ──────────────────────────────────
    // The identical-retry allowance above ran while the build was still the
    // published version, which is the only moment it is safe. The page has since
    // taken a data refresh and a config edit, so replaying the original create
    // now would dedupe onto the FIRST version and drag the live pointer
    // backward — reverting a client's dashboard to its empty state and
    // discarding every refresh since, while reporting deduped:true and
    // "share urls.live". "A turn can die after the commit" only excuses
    // repeating the version that commit published.
    const beforeReplay = toolData(await callTool("get_page_config", { slug: "mcp-tpl-acme" }));
    assert.equal(
      beforeReplay.live_version_id,
      configUpdated.version.id,
      "fixture: the page has moved past its original build"
    );
    const staleReplay = toolError(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-acme",
        config: configA,
        title: "Acme Spring",
        render_mode: "raw",
        publish: true,
      })
    );
    assert.equal(staleReplay.data.code, "page_exists", "replaying a superseded build is a conflict, not a no-op");
    assert.match(staleReplay.data.error, /rollback_page/, "and names the tool for a deliberate revert");
    const afterReplay = toolData(await callTool("get_page_config", { slug: "mcp-tpl-acme" }));
    assert.equal(afterReplay.live_version_id, configUpdated.version.id, "the live pointer did not move");
    assert.deepEqual(afterReplay.config, renamedConfig, "the config edit survived");
    const liveDataAfterReplay = toolData(await callTool("get_page_data", { slug: "mcp-tpl-acme" }));
    assert.equal(
      liveDataAfterReplay.data_sha256,
      refreshed.data_sha256,
      "and the client's numbers were not reverted to the empty state"
    );
    console.log("✓ replaying a superseded create_page_from_template cannot roll a client dashboard back");

    // A page that was authored directly has no config contract to update.
    assert.equal(
      toolError(await callTool("get_page_config", { slug: "mcpdemo" })).data.code,
      "page_not_template_managed"
    );
    const liveMcpDemo = toolData(await callTool("get_page", { slug: "mcpdemo" }));
    assert.equal(
      toolError(
        await callTool("update_page_config", {
          slug: "mcpdemo",
          config: { campaign: "x", channel: "display" },
          expected_version: liveMcpDemo.published.id,
        })
      ).data.code,
      "page_not_template_managed"
    );
    console.log("✓ config and data are independently updatable: neither write can disturb the other's bytes");

    // ── Propagating a design fix (user-initiated, one page at a time) ───────
    // Revision 2 is the "we fixed the chart" case. Nothing that is already
    // deployed may move until a human says so.
    const revisionTwoBytes = Buffer.from(TEMPLATE_HTML(2), "utf8");
    const revTwoTicket = toolData(
      await callTool("create_upload_ticket", {
        template: "mcp-campaign",
        total_bytes: revisionTwoBytes.length,
        content_sha256: crypto.createHash("sha256").update(revisionTwoBytes).digest("hex"),
      })
    );
    await request("PUT", `/upload/${revTwoTicket.upload_id}`, {
      rawBody: revisionTwoBytes,
      token: revTwoTicket.ticket,
    });
    const revTwo = toolData(
      await callTool("register_template_upload", { upload_id: revTwoTicket.upload_id, note: "chart fix" })
    );
    assert.equal(revTwo.revision.revision, 2);

    // Registering it changed nothing that is serving.
    const stillOnOne = (
      await db.query("SELECT html FROM page_versions WHERE id = (SELECT published_version_id FROM pages WHERE slug = $1)", [
        "mcp-tpl-globex",
      ])
    ).rows[0].html;
    assert.ok(stillOnOne.includes("design v1"), "a new revision must not touch a deployed page");

    const behind = toolData(await callTool("list_template_pages", { template: "mcp-campaign" }));
    assert.equal(behind.current_revision, 2);
    assert.equal(behind.pages.length, 2);
    assert.ok(behind.pages.every((entry) => entry.revision === 1 && entry.behind === true));
    assert.deepEqual(
      behind.pages.map((entry) => entry.slug),
      ["mcp-tpl-acme", "mcp-tpl-globex"]
    );

    // Rerender defaults to a designCanary: the new design is inspectable, the client
    // still sees the old one.
    const designCanary = toolData(await callTool("rerender_page_from_template", { slug: "mcp-tpl-globex" }));
    assert.equal(designCanary.from_revision, 1);
    assert.equal(designCanary.template_revision, 2);
    assert.equal(designCanary.published, false, "publish defaults to false");
    assert.equal(designCanary.version_is_live, false);
    assert.equal(designCanary.page_is_live, true, "…while the previous design keeps serving");
    assert.deepEqual(designCanary.config, configB, "the page keeps its own config");
    assert.deepEqual(designCanary.envelope.data, { rows: [{ date: "2026-08-01", spend: 125.5 }] }, "…and its own data");
    assert.equal(designCanary.preflight.ok, true, JSON.stringify(designCanary.preflight.errors));
    const canaryHtml = (await db.query("SELECT html FROM page_versions WHERE id = $1", [designCanary.version.id])).rows[0].html;
    assert.ok(canaryHtml.includes("design v2"), "the designCanary carries the new design");
    const servingDuringCanary = (
      await db.query("SELECT html FROM page_versions WHERE id = (SELECT published_version_id FROM pages WHERE slug = $1)", [
        "mcp-tpl-globex",
      ])
    ).rows[0].html;
    assert.ok(servingDuringCanary.includes("design v1"), "the client still sees the reviewed design");
    assert.match(designCanary.next_step, /publish_page/);

    // The human publishes. Only now does the design change for a viewer.
    const designPromoted = toolData(
      await callTool("publish_page", { slug: "mcp-tpl-globex", version_id: designCanary.version.id })
    );
    assert.equal(designPromoted.version_is_live, true);
    const afterPromote = toolData(await callTool("list_template_pages", { template: "mcp-campaign" }));
    assert.deepEqual(
      afterPromote.pages.map((entry) => [entry.slug, entry.revision, entry.behind]),
      [
        ["mcp-tpl-acme", 1, true],
        ["mcp-tpl-globex", 2, false],
      ],
      "propagation is per page: the one nobody moved is still on revision 1"
    );

    // The write path used to be the LENIENT one: deploy_page and patch_page
    // published whatever bytes they were handed, and only get_page_data /
    // get_page_config / update_page_data ever parsed the managed blocks. So a
    // patch that broke the JSON inside #pages-data published happily and then
    // dead-ended the whole managed-data toolchain on that page, while the live
    // page threw at JSON.parse and served blank. Refuse the write instead.
    const bricking = toolError(
      await callTool("patch_page", {
        slug: "mcp-tpl-globex",
        edits: [{ find: '"contract_version":1', replace: '"contract_version":1,,' }],
        note: "the edit that used to brick a page",
      })
    );
    assert.equal(bricking.data.code, "data_contract_invalid");
    assert.match(bricking.data.error, /refusing to publish/);
    const notBricked = toolData(await callTool("get_page_data", { slug: "mcp-tpl-globex" }));
    assert.ok(notBricked.envelope.data, "the managed-data toolchain still works on the page");
    // Deploying such a document outright is refused the same way, on the same code.
    const rawBrick = toolError(
      await callTool("deploy_page", {
        slug: "mcp-brick",
        html:
          `<!doctype html><html><body>` +
          `<script type="application/schema+json" id="pages-data-schema">${JSON.stringify(TEMPLATE_DATA_SCHEMA)}</script>` +
          `<script type="application/json" id="pages-data">{ "contract_version": nope }</script>` +
          `</body></html>`,
        render_mode: "raw",
      })
    );
    assert.equal(rawBrick.data.code, "data_contract_invalid");
    assert.equal(
      toolError(await callTool("get_page", { slug: "mcp-brick" })).data.code,
      "page_not_found",
      "a refused deploy creates no page"
    );
    // An ordinary page with no managed blocks is NOT affected — most pages are
    // that, and the refusal must not reach them.
    toolData(
      await callTool("deploy_page", {
        slug: "mcp-plain-ok",
        html: "<!doctype html><html><body><h1>No managed blocks here</h1></body></html>",
        render_mode: "raw",
        publish: true,
      })
    );
    toolData(await callTool("delete_page", { slug: "mcp-plain-ok" }));

    // A page can be knocked off its template and until now that page vanished
    // from every tool: list_template_pages required the published version to be
    // template-bound, while list_templates counted the page anyway. On the live
    // server that read as page_count 5 against 2 listed pages, with the 3 forked
    // pages unreachable — "which pages drifted off this template?" had no answer
    // anywhere in the API. Uses its own page so the assertions below stay
    // independent of what the rest of this suite does to acme and globex.
    const driftConfig = { campaign: "Drift Co", channel: "display" };
    toolData(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-drift",
        config: driftConfig,
        publish: true,
      })
    );
    // The realistic case, and the one the skill warns about: patch_page edits the
    // design in place. The managed blocks survive untouched; only the template
    // binding is lost, because prepareDeploy writes none.
    toolData(
      await callTool("patch_page", {
        slug: "mcp-tpl-drift",
        edits: [{ find: "design v2", replace: "design v2 (hand-edited)" }],
        note: "the edit that forks a page off its design",
        publish: true,
      })
    );
    const drifted = toolData(await callTool("list_template_pages", { template: "mcp-campaign" }));
    assert.equal(drifted.drifted_count, 1);
    assert.equal(drifted.serving_count, 2, "acme and globex are untouched");
    const forked = drifted.pages.find((entry) => entry.slug === "mcp-tpl-drift");
    assert.equal(forked.drifted, true, "a patch over a template-built page is drift, and must be visible");
    assert.equal(forked.revision, null, "it serves no revision of this design any more");
    assert.equal(forked.behind, false, "drifted is not 'behind' — it is off the design entirely");
    assert.equal(forked.last_revision, 2, "…but the revision to pull it back onto is still reported");
    assert.equal(forked.config_sha256, null);
    // list_templates must agree with it rather than quietly counting both as built.
    const counted = toolData(await callTool("list_templates", {})).templates.find((t) => t.name === "mcp-campaign");
    assert.equal(counted.page_count, 3);
    assert.equal(counted.serving_count, 2);
    assert.equal(counted.drifted_count, 1);

    // Reporting drift is only useful if it can be acted on. With no binding left
    // there is nothing to infer the design from, so the template must be named —
    // and then the page is pulled back, carrying the config and data that survived
    // in its published HTML.
    assert.equal(
      toolError(await callTool("rerender_page_from_template", { slug: "mcp-tpl-drift" })).data.code,
      "page_not_template_managed",
      "without a named template there is no design to infer"
    );
    const pulledBack = toolData(
      await callTool("rerender_page_from_template", { slug: "mcp-tpl-drift", template: "mcp-campaign", publish: true })
    );
    assert.equal(pulledBack.reattached, true);
    assert.equal(pulledBack.from_revision, null, "it was serving no revision of any design");
    assert.equal(pulledBack.template_revision, 2);
    assert.deepEqual(pulledBack.config, driftConfig, "the drifted page keeps its own config");
    const healed = toolData(await callTool("list_template_pages", { template: "mcp-campaign" }));
    assert.equal(healed.drifted_count, 0);
    assert.equal(healed.serving_count, 3);

    // A page hand-deployed all the way down to plain HTML has no config or data
    // left to carry over. That is honestly unrecoverable, and the error says so
    // rather than failing on a block-shape complaint.
    toolData(
      await callTool("deploy_page", {
        slug: "mcp-tpl-drift",
        html: "<!doctype html><html><head><title>Forked</title></head><body><h1>Nothing left</h1></body></html>",
        render_mode: "raw",
        publish: true,
      })
    );
    const unrecoverable = toolError(
      await callTool("rerender_page_from_template", { slug: "mcp-tpl-drift", template: "mcp-campaign" })
    );
    assert.equal(unrecoverable.data.code, "page_not_template_managed");
    assert.match(unrecoverable.data.error, /cannot be recovered from what is serving/);
    assert.match(unrecoverable.data.error, /create_page_from_template/);
    toolData(await callTool("delete_page", { slug: "mcp-tpl-drift" }));

    // Rerendering onto the revision a page is already on is a no-op, not a
    // pointless new version.
    assert.equal(
      toolError(await callTool("rerender_page_from_template", { slug: "mcp-tpl-globex" })).data.code,
      "template_revision_unchanged"
    );

    // A revision that tightens its contract must fail loudly, not produce a page
    // whose own schema rejects its config.
    const strictSchema = {
      ...TEMPLATE_CONFIG_SCHEMA,
      required: ["campaign", "channel", "accountCode"],
      properties: { ...TEMPLATE_CONFIG_SCHEMA.properties, accountCode: { type: "string", minLength: 1 } },
    };
    const strictHtml = TEMPLATE_HTML(3).replace(
      JSON.stringify(TEMPLATE_CONFIG_SCHEMA),
      JSON.stringify(strictSchema)
    ).replace(
      JSON.stringify({ campaign: "Reference", channel: "display" }),
      JSON.stringify({ campaign: "Reference", channel: "display", accountCode: "ACCT00000" })
    );
    const strictBytes = Buffer.from(strictHtml, "utf8");
    const strictTicket = toolData(
      await callTool("create_upload_ticket", {
        template: "mcp-campaign",
        total_bytes: strictBytes.length,
        content_sha256: crypto.createHash("sha256").update(strictBytes).digest("hex"),
      })
    );
    await request("PUT", `/upload/${strictTicket.upload_id}`, { rawBody: strictBytes, token: strictTicket.ticket });
    const revThree = toolData(await callTool("register_template_upload", { upload_id: strictTicket.upload_id }));
    assert.equal(revThree.revision.revision, 3);
    const tooStrict = toolError(
      await callTool("rerender_page_from_template", { slug: "mcp-tpl-acme", revision: 3 })
    );
    assert.equal(tooStrict.data.code, "config_validation_failed");
    const acmeUnmoved = toolData(await callTool("get_page_config", { slug: "mcp-tpl-acme" }));
    assert.equal(acmeUnmoved.live_version_id, configUpdated.version.id, "a refused rerender writes nothing");

    // A directly-authored page has no design to move it to.
    assert.equal(
      toolError(await callTool("rerender_page_from_template", { slug: "mcpdemo" })).data.code,
      "page_not_template_managed"
    );
    // prepare_dashboard_update must not tell an agent to rewrite a shared design
    // in one page's HTML — that forks the page off its template silently.
    const templateAdvice = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcp-tpl-globex",
        instructions: "Make the spend chart taller and rename the campaign to Globex Q4.",
      })
    );
    assert.equal(templateAdvice.mode, "managed_template");
    assert.match(templateAdvice.prompt, /update_page_config/);
    assert.match(templateAdvice.prompt, /rerender_page_from_template/);
    assert.match(templateAdvice.prompt, /forks it off/i);
    assert.match(templateAdvice.prompt, /revision 2/);
    // A data request on the same page still routes to the managed-data contract.
    const dataAdvice = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcp-tpl-globex",
        instructions: "Refresh with yesterday's complete spend.",
        update_type: "data",
      })
    );
    assert.equal(dataAdvice.mode, "managed_data");
    console.log("✓ a design fix propagates one page at a time, as a canary, only when a human publishes it");

    // ── template_urls: show a human the design ──────────────────────────────
    // Only the admin API could mint a template preview token, so an agent could
    // build pages from a design it had no way to let anyone look at.
    const tplUrls = toolData(await callTool("template_urls", { template: "mcp-campaign" }));
    assert.equal(tplUrls.template, "mcp-campaign");
    // The invariant is "the current revision", not a number this suite happens to
    // be on by now — so read it rather than hardcoding it.
    assert.equal(
      tplUrls.revision,
      toolData(await callTool("get_template", { template: "mcp-campaign" })).template.current_revision,
      "defaults to the current revision"
    );
    assert.equal(tplUrls.has_sample_data, true);
    assert.ok(tplUrls.library.endsWith("/admin/templates"));
    assert.match(tplUrls.preview, /\/raw-template\/\d+\?t=/, "a signed content-host URL");
    assert.ok(!tplUrls.preview.startsWith(tplUrls.library.replace("/admin/templates", "")),
      "the preview must not be on the dashboard origin");
    assert.equal(tplUrls.expires_in_seconds, 300);
    // An exact revision is addressable, and a missing one is a clean 404-shaped error.
    assert.equal(toolData(await callTool("template_urls", { template: "mcp-campaign", revision: 1 })).revision, 1);
    assert.equal(
      toolError(await callTool("template_urls", { template: "mcp-campaign", revision: 99 })).data.code,
      "template_revision_not_found"
    );
    // The other half of the retry rule: the case the allowance exists FOR still
    // works. On an approval-gated page the build lands pending and the pointer
    // stays NULL, so there is no live version to drag backward and nothing
    // serving to revert — a died-mid-turn retry must still be a no-op. (Placed
    // last because it adds a page to the template, which the counts above pin.)
    const gatedBuild = toolData(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-gated",
        config: { campaign: "Gated Co", channel: "display", accountCode: "ACCT99999" },
        require_approval: true,
      })
    );
    assert.equal(gatedBuild.created, true);
    assert.equal(gatedBuild.version_is_live, false, "a gated build waits for a human");
    assert.equal(gatedBuild.live_version_id, null, "so nothing is published yet");
    const gatedRetry = toolData(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: "mcp-tpl-gated",
        config: { campaign: "Gated Co", channel: "display", accountCode: "ACCT99999" },
        require_approval: true,
      })
    );
    assert.equal(gatedRetry.deduped, true, "the retry allowance still covers a page with nothing published");
    assert.equal(gatedRetry.version.id, gatedBuild.version.id, "and returns the same pending version");

    // …but "nothing published" is not automatically "nothing to lose". An admin
    // restore clears deleted_at and leaves the pointer NULL, so a restored page
    // has a NULL pointer AND a full history. Replaying the original create there
    // would republish the empty state over a page with refreshes behind it — the
    // same revert by a different route — so the allowance is scoped to the page's
    // NEWEST version, which is what a died-mid-turn build always is.
    const adminCtx = { actor: "admin@elcanotek.com", actorType: "user", ip: "127.0.0.1" };
    const restoredSlug = "mcp-tpl-restored";
    const restoredBuild = toolData(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: restoredSlug,
        config: { campaign: "Restored Co", channel: "display", accountCode: "ACCT88888" },
      })
    );
    const restoredRefresh = toolData(
      await callTool("update_page_data", {
        slug: restoredSlug,
        data: { rows: [{ date: "2026-08-03", spend: 77 }] },
        source_as_of: "2026-08-03T00:00:00Z",
        expected_version: restoredBuild.version.id,
      })
    );
    await versions.deletePage({ slug: restoredSlug }, adminCtx);
    await versions.restorePage({ slug: restoredSlug }, adminCtx);
    const restoredRow = (
      await db.query("SELECT published_version_id FROM pages WHERE slug = $1", [restoredSlug])
    ).rows[0];
    assert.equal(restoredRow.published_version_id, null, "fixture: restore leaves the pointer NULL");
    const restoredReplay = toolError(
      await callTool("create_page_from_template", {
        template: "mcp-campaign",
        slug: restoredSlug,
        config: { campaign: "Restored Co", channel: "display", accountCode: "ACCT88888" },
      })
    );
    assert.equal(
      restoredReplay.data.code,
      "page_exists",
      "a restored page has history, so replaying its original build is refused"
    );
    const restoredAfter = (
      await db.query("SELECT published_version_id FROM pages WHERE slug = $1", [restoredSlug])
    ).rows[0];
    assert.equal(restoredAfter.published_version_id, null, "and nothing was republished behind the refresh");
    assert.ok(restoredRefresh.version.id, "fixture sanity: the refresh exists in history");
    console.log("✓ the retry allowance covers a gated build, but not a restored page with history");

    console.log("✓ template_urls mints a sandboxed preview link for a design");

    // ── "I like this page, make more like it" ────────────────────────────────
    // The flow that should have existed first. A page is authored with its
    // per-instance values in #pages-config and NO hand-written config schema —
    // Pages derives one on deploy — and is then promoted in place. No design
    // bytes cross the wire in either direction.
    // The <title> deliberately repeats the config value verbatim, so the
    // promotion's hardcoded-value warning has something real to find.
    const AUTHORED = `<!doctype html><html><head><meta charset="utf-8"><title>Acme Corp</title>
<style>:root{--brand:#333}h1{color:var(--brand)}</style></head><body>
<h1 id="who"></h1><p id="total"></p>
<script type="application/json" id="pages-config">${JSON.stringify({
      client: "Acme Corp",
      brand: { primary: "#0a3d62" },
      kpiTarget: 3.5,
      flightEnd: null,
    })}</script>
<script type="application/schema+json" id="pages-data-schema">${JSON.stringify(TEMPLATE_DATA_SCHEMA)}</script>
<script type="application/json" id="pages-data">${JSON.stringify({
      contract_version: 1,
      refreshed_at: "2026-08-01T00:00:00.000Z",
      source_as_of: "2026-08-01T00:00:00.000Z",
      data: { rows: [{ date: "2026-07-31", spend: 940.25 }] },
    })}</script>
<script>
const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);
const DATA = JSON.parse(document.getElementById('pages-data').textContent).data;
document.documentElement.style.setProperty('--brand', CONFIG.brand.primary);
document.getElementById('who').textContent = CONFIG.client;
document.getElementById('total').textContent = DATA.rows.length ? String(DATA.rows.length) : 'Awaiting first ingest';
</script></body></html>`;

    const authored = toolData(
      await callTool("deploy_page", { slug: "acme-live", html: AUTHORED, title: "Acme", publish: true })
    );
    // The author wrote no JSON Schema for their config; Pages derived one and
    // said so rather than doing it silently.
    assert.equal(authored.config_schema_generated, true, "a config-carrying page gets a derived schema");
    const authoredConfig = toolData(await callTool("get_page_config", { slug: "acme-live" }));
    assert.deepEqual(authoredConfig.config.client, "Acme Corp");
    assert.equal(authoredConfig.config_schema.type, "object");
    // The derived schema is a real contract: it rejects a typo'd key...
    const typo = toolError(
      await callTool("update_page_config", {
        slug: "acme-live",
        config: { clientt: "Acme Corp", brand: { primary: "#0a3d62" }, kpiTarget: 3.5 },
        expected_version: authoredConfig.live_version_id,
      })
    );
    assert.equal(typo.data.code, "config_validation_failed");
    // ...but a null-valued key was inferred OPTIONAL, so omitting it is fine.
    const withoutOptional = toolData(
      await callTool("update_page_config", {
        slug: "acme-live",
        config: { client: "Acme Corp", brand: { primary: "#123456" }, kpiTarget: 3.5 },
        expected_version: authoredConfig.live_version_id,
      })
    );
    assert.ok(withoutOptional.version.id, "a config omitting a null-valued key is accepted");

    // Promote it. Bytes stay server-side.
    const promoted = toolData(
      await callTool("create_template_from_page", {
        slug: "acme-live",
        template: "acme-family",
        empty_data: { rows: [] },
        example_from_current_data: true,
        title: "Acme Family",
      })
    );
    assert.equal(promoted.created, true);
    assert.equal(promoted.from_page, "acme-live");
    assert.equal(promoted.revision.revision, 1);
    assert.equal(promoted.example_from_current_data, true);
    assert.equal(promoted.has_sample_data, true, "the promoted design previews populated");
    assert.equal(promoted.reference_config.client, "Acme Corp", "the page's own config became the reference");
    assert.equal(promoted.preflight.ok, true, JSON.stringify(promoted.preflight.errors));

    // The next client's page: config only. This is the whole point.
    const nextClient = toolData(
      await callTool("create_page_from_template", {
        template: "acme-family",
        slug: "globex-live",
        config: { client: "Globex", brand: { primary: "#b71540" }, kpiTarget: 4 },
        title: "Globex",
        publish: true,
      })
    );
    assert.equal(nextClient.created, true);
    assert.deepEqual(nextClient.envelope.data, { rows: [] }, "it starts empty, not with Acme's numbers");
    // Acme's real data must not have followed it, and neither must the example block.
    for (const needle of ["pages-data-example", "940.25"]) {
      const found = toolData(await callTool("find_in_version", { slug: "globex-live", query: needle }));
      assert.equal(found.total_matches, 0, `Globex's page still contains ${needle}`);
    }

    // The honest limit, asserted rather than hidden: promoting a page does NOT
    // make its hardcoded markup configurable. <title>Acme</title> is part of the
    // design, so Globex's page still carries it — and the promotion SAID so,
    // which is the difference between a known limitation and a client seeing
    // another client's name.
    assert.equal(
      toolData(await callTool("find_in_version", { slug: "globex-live", query: "<title>Acme Corp" })).total_matches,
      1,
      "a literal in the design's markup survives promotion — that is what the warning is for"
    );
    const flagged = promoted.hardcoded_config_values.find((entry) => entry.path === "client");
    assert.ok(flagged, `the promotion must flag client: ${JSON.stringify(promoted.hardcoded_config_values)}`);
    assert.equal(flagged.value, "Acme Corp");
    assert.ok(flagged.occurrences >= 1);
    assert.equal(
      toolData(await callTool("find_in_version", { slug: "globex-live", query: "#b71540" })).total_matches >= 1,
      true,
      "…and it carries its own brand colour"
    );

    // A page that hardcodes its identity is refused with the reason, not promoted
    // into a template whose every instance says the same client. Two distinct
    // refusals, and the code says which: a page with no managed blocks at all is
    // not even data-managed, while a data-managed page simply has nothing that
    // varies per instance.
    const plain = toolError(
      await callTool("create_template_from_page", {
        slug: "mcpdemo",
        template: "not-reusable",
        empty_data: { rows: [] },
      })
    );
    assert.equal(plain.data.code, "page_not_data_managed", "an unmanaged page cannot be promoted");

    const dataOnlyHtml = `<!doctype html><html><head><title>Data only</title></head><body><table id="r"></table>
<script type="application/schema+json" id="pages-data-schema">${JSON.stringify(TEMPLATE_DATA_SCHEMA)}</script>
<script type="application/json" id="pages-data">${JSON.stringify({
      contract_version: 1,
      refreshed_at: "2026-08-01T00:00:00.000Z",
      source_as_of: "2026-08-01T00:00:00.000Z",
      data: { rows: [] },
    })}</script></body></html>`;
    await callTool("deploy_page", { slug: "data-only", html: dataOnlyHtml, title: "Data only", publish: true });
    const noConfig = toolError(
      await callTool("create_template_from_page", {
        slug: "data-only",
        template: "not-reusable",
        empty_data: { rows: [] },
      })
    );
    assert.equal(noConfig.data.code, "page_not_template_managed");
    assert.match(noConfig.data.error, /pages-config/, "the error must name the block to add");
    console.log("✓ a page you already like becomes reusable in place: no design bytes cross the wire either way");

    // ── delete_template: registering is easy, so undoing must be too ─────────
    // Guarded: four pages were built from mcp-campaign (acme, globex, the gated
    // one and the restored one), and confusing "I typed the name wrong" with
    // "retire this design" is the mistake worth preventing.
    const guarded = toolError(await callTool("delete_template", { template: "mcp-campaign" }));
    assert.equal(guarded.data.code, "template_has_pages");
    assert.equal(guarded.data.details.pages, 4);
    assert.match(guarded.data.error, /list_template_pages/);
    assert.equal(
      toolData(await callTool("list_templates", {})).templates.some((t) => t.name === "mcp-campaign"),
      true,
      "a refused delete writes nothing"
    );

    // A typo — nothing built from it — retires without ceremony, and the name
    // comes back, which is the whole reason this exists.
    const typoBytes = Buffer.from(TEMPLATE_HTML(9), "utf8");
    const typoTicket = toolData(
      await callTool("create_upload_ticket", {
        template: "mcp-typoo",
        total_bytes: typoBytes.length,
        content_sha256: crypto.createHash("sha256").update(typoBytes).digest("hex"),
      })
    );
    await request("PUT", `/upload/${typoTicket.upload_id}`, { rawBody: typoBytes, token: typoTicket.ticket });
    await callTool("register_template_upload", { upload_id: typoTicket.upload_id, title: "Typo" });
    const retired = toolData(await callTool("delete_template", { template: "mcp-typoo" }));
    assert.equal(retired.deleted, true);
    assert.equal(retired.pages_built, 0);
    assert.equal(
      toolData(await callTool("list_templates", {})).templates.some((t) => t.name === "mcp-typoo"),
      false,
      "it leaves the library"
    );
    assert.equal(toolError(await callTool("get_template", { template: "mcp-typoo" })).data.code, "template_not_found");

    // The freed name re-registers, rather than colliding with the retired row.
    const reuseTicket = toolData(
      await callTool("create_upload_ticket", {
        template: "mcp-typoo",
        total_bytes: typoBytes.length,
        content_sha256: crypto.createHash("sha256").update(typoBytes).digest("hex"),
      })
    );
    await request("PUT", `/upload/${reuseTicket.upload_id}`, { rawBody: typoBytes, token: reuseTicket.ticket });
    const reused = toolData(await callTool("register_template_upload", { upload_id: reuseTicket.upload_id, title: "Reused" }));
    assert.equal(reused.created, true, "the retired name is available again");
    assert.equal(reused.revision.revision, 1, "and starts a fresh revision series");
    await callTool("delete_template", { template: "mcp-typoo" });

    // force retires a design that pages were built from. They keep serving —
    // each page carries its own materialized HTML and never reads the template.
    const forced = toolData(await callTool("delete_template", { template: "mcp-campaign", force: true }));
    assert.equal(forced.deleted, true);
    assert.equal(forced.pages_built, 4);
    // get_page returns PageSchema, which has no is_live — that field is on the
    // list summary. Serving is published_version_id set and not disabled.
    const stillLive = toolData(await callTool("get_page", { slug: "mcp-tpl-acme" }));
    assert.ok(stillLive.page.published_version_id, "a page outlives the template it was built from");
    assert.equal(stillLive.page.disabled, false);
    assert.ok(stillLive.published, "…and its published version is still readable");
    assert.equal(
      toolError(await callTool("rerender_page_from_template", { slug: "mcp-tpl-acme" })).data.code,
      "template_not_found",
      "…but can no longer be re-rendered from it, which is what force bought"
    );
    console.log("✓ delete_template frees a mistyped name, and guards a design that pages depend on");

    // A real official SDK client must interoperate end to end, including its
    // initialize/initialized lifecycle, generated headers, discovery, and a
    // typed tool call. This catches drift that hand-built HTTP fixtures cannot.
    const sdkClient = new Client(
      { name: "pages-official-sdk-integration", title: "Pages SDK Integration", version: "1.0.0" },
      { capabilities: {} }
    );
    const sdkTransport = new StreamableHTTPClientTransport(new URL(`http://localhost:${PORT}/mcp`), {
      requestInit: { headers: { Authorization: `Bearer ${bearerToken}` } },
    });
    try {
      await sdkClient.connect(sdkTransport);
      assert.equal(sdkTransport.protocolVersion, PROTOCOL_VERSION);
      assert.equal(sdkClient.getServerVersion().name, "pages");
      const sdkTools = await sdkClient.listTools();
      assert.deepEqual(sdkTools.tools.map((tool) => tool.name).sort(), EXPECTED_TOOLS);
      const sdkRead = await sdkClient.callTool({ name: "get_page", arguments: { slug: "mcpdemo" } });
      assert.equal(sdkRead.isError, undefined);
      assert.equal(sdkRead.structuredContent.page.slug, "mcpdemo");
      assert.deepEqual(JSON.parse(sdkRead.content[0].text), sdkRead.structuredContent);
    } finally {
      await sdkClient.close().catch(() => {});
    }
    console.log("✓ official SDK client connect/listTools/callTool interoperability");

    // 10. Pagination is bounded and cursor-based; the next page neither repeats
    // the boundary row nor depends on offset drift.
    const firstPage = toolData(await callTool("list_pages", { limit: 1 }));
    assert.equal(firstPage.pages.length, 1);
    assert.ok(firstPage.next_cursor, "look-ahead emits next_cursor");
    const secondPage = toolData(await callTool("list_pages", { limit: 1, cursor: firstPage.next_cursor }));
    assert.equal(secondPage.pages.length, 1);
    assert.notEqual(secondPage.pages[0].id, firstPage.pages[0].id);
    const staleCursor = toolError(
      await callTool("list_pages", { workspace_id: null, limit: 1, cursor: firstPage.next_cursor })
    );
    assert.equal(staleCursor.data.code, "invalid_cursor", "cursor is bound to its filters");
    console.log("✓ list_pages keyset pagination and filter-bound cursors");

    // 11. Agents can use reversible workspace organization, including explicit
    // Ungrouped selection. Destructive bulk deletion is absent from MCP.
    const alpha = toolData(await callTool("create_workspace", { name: "MCP Alpha" })).workspace;
    const beta = toolData(await callTool("create_workspace", { name: "MCP Beta" })).workspace;
    assert.equal(typeof alpha.id, "string");
    const workspacePage1 = toolData(await callTool("list_workspaces", { limit: 1 }));
    assert.equal(workspacePage1.workspaces.length, 1);
    assert.ok(workspacePage1.next_cursor);
    const workspacePage2 = toolData(
      await callTool("list_workspaces", { limit: 1, cursor: workspacePage1.next_cursor })
    );
    assert.equal(workspacePage2.workspaces.length, 1);
    assert.notEqual(workspacePage2.workspaces[0].id, workspacePage1.workspaces[0].id);

    const renamed = toolData(
      await callTool("rename_workspace", { workspace_id: alpha.id, name: "MCP Client Alpha" })
    ).workspace;
    assert.equal(renamed.name, "MCP Client Alpha");
    const assigned = toolData(
      await callTool("set_page_workspace", { slug: "mcpdemo", workspace_id: alpha.id })
    ).page;
    assert.equal(assigned.workspace_id, alpha.id);
    assert.equal(typeof assigned.workspace_id, "string");
    assert.equal(assigned.workspace_name, "MCP Client Alpha");
    const grouped = toolData(await callTool("list_pages", { workspace_id: alpha.id }));
    assert.deepEqual(grouped.pages.map((item) => item.slug), ["mcpdemo"]);
    const foundWorkspace = toolData(await callTool("list_workspaces", { query: "client alpha" }));
    assert.deepEqual(foundWorkspace.workspaces.map((item) => item.id), [alpha.id]);

    const ungroupedMove = toolData(
      await callTool("set_page_workspace", { slug: "mcpdemo", workspace_id: null })
    ).page;
    assert.equal(ungroupedMove.workspace_id, null);
    assert.equal(ungroupedMove.workspace_name, null);
    const ungrouped = toolData(await callTool("list_pages", { workspace_id: null }));
    assert.ok(ungrouped.pages.some((item) => item.slug === "mcpdemo"));

    // Leave the shared integration database in the state expected by the admin
    // suite. This direct human cleanup also exercises that MCP's omission is an
    // authority boundary, not an inability in the domain layer.
    const human = { actor: "mcp-test-admin", actorType: "user", ip: "127.0.0.1" };
    await workspaces.remove({ id: alpha.id }, human);
    await workspaces.remove({ id: beta.id }, human);
    console.log("✓ workspace create/rename/list/assign/Ungrouped/filter; delete remains absent");

    // 12. Identical content dedupes. A current expected_version succeeds;
    // a stale one returns a structured domain error and leaves the pointer.
    const duplicate = toolData(
      await callTool("deploy_page", { slug: "mcpdemo", html: HTML(1), publish: false })
    );
    assert.equal(duplicate.deduped, true);
    assert.equal(duplicate.version.id, v1);
    assert.equal(duplicate.version_is_live, true);

    const updated = toolData(
      await callTool("update_page", {
        slug: "mcpdemo",
        html: HTML(2),
        publish: true,
        expected_version: v1,
      })
    );
    const v2 = updated.version.id;
    assert.notEqual(v2, v1);
    assert.equal(updated.version_is_live, true);
    const staleAnchor = toolError(
      await callTool("update_page", {
        slug: "mcpdemo",
        html: HTML(3),
        publish: true,
        expected_version: v1,
      })
    );
    assert.equal(staleAnchor.data.code, "stale_version");
    assert.equal((toolData(await callTool("get_page", { slug: "mcpdemo" }))).page.published_version_id, v2);

    const history1 = toolData(await callTool("list_versions", { slug: "mcpdemo", limit: 1 }));
    assert.equal(history1.versions.length, 1);
    assert.ok(history1.next_cursor);
    const history2 = toolData(
      await callTool("list_versions", { slug: "mcpdemo", limit: 1, cursor: history1.next_cursor })
    );
    assert.notEqual(history2.versions[0].id, history1.versions[0].id);
    const fullVersion = toolData(await callTool("get_version", { slug: "mcpdemo", version_id: v2 }));
    assert.match(fullVersion.version.html, /v2/);
    const rolledBack = toolData(
      await callTool("rollback_page", { slug: "mcpdemo", version_id: v1, expected_version: v2 })
    );
    assert.equal(rolledBack.live_version_id, v1);
    console.log("✓ dedupe, optimistic update, version pagination/get, and rollback");

    // 13. On a gate added after an existing live release, the pending version
    // is not mislabeled live and the prior live pointer is reported explicitly.
    const prior = toolData(
      await callTool("deploy_page", { slug: "mcpgated", title: "Gated", html: HTML(10) })
    );
    await db.query("UPDATE pages SET require_approval = true WHERE slug = 'mcpgated'");
    const gated = toolData(
      await callTool("update_page", { slug: "mcpgated", html: HTML(11), publish: true })
    );
    assert.equal(gated.version.status, "pending");
    assert.equal(gated.published, false);
    assert.equal(gated.version_is_live, false);
    assert.equal(gated.page_is_live, true, "the prior published version still serves");
    assert.equal(gated.live_version_id, prior.version.id);
    assert.match(gated.next_step, /previously published version .* remains live/i);
    const gatedPage = toolData(await callTool("get_page", { slug: "mcpgated" }));
    assert.equal(gatedPage.page.published_version_id, prior.version.id);
    const gatedPublish = toolError(
      await callTool("publish_page", { slug: "mcpgated", version_id: gated.version.id })
    );
    assert.equal(gatedPublish.data.code, "approval_required");
    await db.query("UPDATE pages SET require_approval = false WHERE slug = 'mcpgated'");
    const transitionedDraft = toolData(
      await callTool("update_page", { slug: "mcpgated", html: HTML(11), publish: false })
    );
    assert.equal(transitionedDraft.deduped, false, "open page skips an unusable gated-era pending row");
    assert.equal(transitionedDraft.version.status, "draft");
    const transitionedPublish = toolData(
      await callTool("publish_page", {
        slug: "mcpgated",
        version_id: transitionedDraft.version.id,
        expected_version: prior.version.id,
      })
    );
    assert.equal(transitionedPublish.version_is_live, true);
    console.log("✓ approval gate truthfully reports pending version and prior-live serving state");

    // 14. Remaining mutations retain their authority and serving semantics.
    const automatic = toolData(
      await callTool("deploy_page", { slug: "mcpauto", title: "Auto", html: HTML(20) })
    );
    assert.equal(automatic.published, true, "open-page deploy defaults to publish");
    assert.match(automatic.next_step, /live now/i);
    const password = toolData(
      await callTool("set_password", { slug: "mcpauto", password: "integration-password" })
    );
    assert.equal(password.has_password, true);
    assert.equal(password.page_is_live, true);
    const titled = toolData(await callTool("set_title", { slug: "mcpauto", title: "Renamed Auto" }));
    assert.equal(titled.title, "Renamed Auto");

    // Schema failures are tool errors (not -32603 protocol failures), and strict
    // object schemas reject unknown/prototype-named argument keys.
    const badId = toolError(
      await callTool("publish_page", { slug: "mcpauto", version_id: "not-an-id" })
    );
    assert.match(badId.text, /Invalid arguments/i);
    const extra = toolError(await callTool("get_page", { slug: "mcpauto", bogus: 1 }));
    assert.match(extra.text, /Invalid arguments/i);
    const prototypeArg = toolError(
      await callTool("get_page", { slug: "mcpauto", constructor: 1 })
    );
    assert.match(prototypeArg.text, /Invalid arguments/i);
    const clearPassword = toolError(
      await callTool("set_password", { slug: "mcpauto", password: "" })
    );
    assert.match(clearPassword.text, /Invalid arguments/i, "MCP cannot express admin-only password clearing");

    const deleted = toolData(await callTool("delete_page", { slug: "mcpauto" }));
    assert.equal(deleted.deleted, true);
    assert.equal(deleted.page_is_live, false);
    const gone = toolError(await callTool("get_page", { slug: "mcpauto" }));
    assert.equal(gone.data.code, "page_not_found");
    const recreated = toolData(
      await callTool("deploy_page", { slug: "mcpauto", title: "Reborn", html: HTML(21) })
    );
    assert.equal(recreated.created, true, "soft deletion frees the active slug");
    console.log("✓ password/title/delete/recreate and strict tool-argument validation");

    // 15. Admin takedown cannot be bypassed through deploy or delete.
    await db.query("UPDATE pages SET disabled = true WHERE slug = 'mcpauto'");
    const disabledDelete = toolError(await callTool("delete_page", { slug: "mcpauto" }));
    assert.equal(disabledDelete.data.code, "disabled_takedown");
    const disabledDeploy = toolError(
      await callTool("deploy_page", { slug: "mcpauto", html: HTML(22) })
    );
    assert.equal(disabledDeploy.data.code, "disabled_takedown");
    console.log("✓ agent cannot delete or publish an admin-disabled page");

    // 16. Dedupe guidance distinguishes the current live version from an older
    // approved-but-not-live rollback target.
    const dedupeV1 = toolData(
      await callTool("deploy_page", { slug: "mcpdedup", title: "Dedupe", html: HTML(30) })
    );
    const liveDuplicate = toolData(
      await callTool("deploy_page", { slug: "mcpdedup", html: HTML(30), publish: false })
    );
    assert.equal(liveDuplicate.version_is_live, true);
    toolData(await callTool("deploy_page", { slug: "mcpdedup", html: HTML(31) }));
    const priorDuplicate = toolData(
      await callTool("deploy_page", { slug: "mcpdedup", html: HTML(30), publish: false })
    );
    assert.equal(priorDuplicate.version.id, dedupeV1.version.id);
    assert.equal(priorDuplicate.version_is_live, false);
    assert.match(priorDuplicate.next_step, /rollback_page/);
    console.log("✓ dedupe guidance distinguishes live from approved rollback targets");

    // 17. create-or-deploy is atomic and converges under a slug race. A stale
    // creation attempt rolls its inserted page back; simultaneous callers both
    // complete, with exactly one reporting that it created the page.
    const atomicFailure = toolError(
      await callTool("deploy_page", {
        slug: "mcpatomic-fail",
        title: "Must Roll Back",
        html: HTML(40),
        expected_version: v1,
      })
    );
    assert.equal(atomicFailure.data.code, "stale_version");
    const atomicMissing = toolError(await callTool("get_page", { slug: "mcpatomic-fail" }));
    assert.equal(atomicMissing.data.code, "page_not_found", "failed deploy leaves no empty page");

    const raced = await Promise.all([
      callTool("deploy_page", { slug: "mcpatomic-race", title: "Race A", html: HTML(41) }),
      callTool("deploy_page", { slug: "mcpatomic-race", title: "Race B", html: HTML(42) }),
    ]);
    const raceResults = raced.map(toolData);
    assert.equal(raceResults.filter((result) => result.created).length, 1, "one racer creates the page");
    assert.equal(raceResults.every((result) => result.page_is_live), true, "both racers complete normally");
    console.log("✓ create-or-deploy is atomic and converges under concurrent creation");

    // 18. Exercise the successful publish_page branch (gated denial above is
    // intentionally a different result shape) and its advertised output.
    const publishBase = toolData(
      await callTool("deploy_page", { slug: "mcppublish", title: "Publish", html: HTML(50) })
    );
    const publishDraft = toolData(
      await callTool("update_page", { slug: "mcppublish", html: HTML(51), publish: false })
    );
    assert.equal(publishDraft.version.status, "draft");
    const publishedDraft = toolData(
      await callTool("publish_page", {
        slug: "mcppublish",
        version_id: publishDraft.version.id,
        expected_version: publishBase.version.id,
      })
    );
    assert.equal(publishedDraft.version_is_live, true);
    assert.equal(publishedDraft.live_version_id, publishDraft.version.id);
    console.log("✓ publish_page success validates its structured output contract");

    // 19. Managed page data uses the published immutable version as its
    // template. A canary changes only the data-block contents; exact retries
    // dedupe even though Pages would otherwise generate a new refreshed_at.
    const dataSchema = {
      $schema: "https://json-schema.org/draft/2020-12/schema",
      type: "object",
      additionalProperties: false,
      required: ["count", "label"],
      properties: {
        count: { type: "integer", minimum: 0 },
        label: { type: "string" },
      },
    };
    const managedHTML = (sourceAsOf, data = { count: 1, label: "initial" }) =>
      `<!doctype html><html><head><title>Managed</title></head><body>` +
      `<main data-layout="preserve-me"><h1>Managed dashboard</h1></main>` +
      `<script id="pages-data-schema" type="application/schema+json">${JSON.stringify(dataSchema)}</script>` +
      `<script id="pages-data" type="application/json">${JSON.stringify({
        contract_version: 1,
        refreshed_at: new Date(Date.now() - 5 * 60 * 60 * 1000).toISOString(),
        source_as_of: sourceAsOf,
        data,
      })}</script>` +
      `<footer>immutable layout tail</footer></body></html>`;
    const source0 = new Date(Date.now() - 4 * 60 * 60 * 1000).toISOString();
    const source1 = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString();
    const source2 = new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString();
    const source3 = new Date(Date.now() - 60 * 60 * 1000).toISOString();
    const initialHTML = managedHTML(source0);
    const dataBase = toolData(
      await callTool("deploy_page", { slug: "mcpdata", title: "Managed Data", html: initialHTML })
    );
    const dataV0 = dataBase.version.id;
    const initialData = toolData(await callTool("get_page_data", { slug: "mcpdata" }));
    assert.deepEqual(initialData.schema, dataSchema);
    assert.deepEqual(initialData.envelope.data, { count: 1, label: "initial" });
    assert.equal(initialData.live_version_id, dataV0);
    assert.equal(initialData.version_is_live, true);
    assert.match(initialData.data_sha256, /^[0-9a-f]{64}$/);

    const invalidData = toolError(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: { count: -1, label: 4 },
        source_as_of: source1,
        expected_version: dataV0,
      })
    );
    assert.equal(invalidData.data.code, "data_validation_failed");
    assert.ok(invalidData.data.details.validation_errors.length >= 1);

    const injectedData = { count: 2, label: "</script><img src=x onerror=alert(1)>" };
    const canary = toolData(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: injectedData,
        source_as_of: source1,
        expected_version: dataV0,
        publish: false,
        note: "canary",
      })
    );
    assert.equal(canary.published, false);
    assert.equal(canary.version_is_live, false);
    assert.equal(canary.live_version_id, dataV0);
    const canaryVersion = canary.version.id;
    const canaryRead = toolData(await callTool("get_version", { slug: "mcpdata", version_id: canaryVersion }));
    const parsedInitial = pageDataContract.parseManagedHtml(initialHTML);
    const parsedCanary = pageDataContract.parseManagedHtml(canaryRead.version.html);
    assert.equal(
      canaryRead.version.html.slice(0, parsedCanary.dataBlock.contentStart),
      initialHTML.slice(0, parsedInitial.dataBlock.contentStart),
      "all bytes before managed data content are preserved"
    );
    assert.ok(
      canaryRead.version.html.endsWith(initialHTML.slice(parsedInitial.dataBlock.contentEnd)),
      "all bytes after managed data content are preserved"
    );
    assert.deepEqual(parsedCanary.envelope.data, injectedData);
    assert.doesNotMatch(
      canaryRead.version.html.slice(parsedCanary.dataBlock.contentStart, parsedCanary.dataBlock.contentEnd),
      /<\/script/i
    );

    const canaryRetry = toolData(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: injectedData,
        source_as_of: source1,
        expected_version: dataV0,
        publish: false,
      })
    );
    assert.equal(canaryRetry.deduped, true);
    assert.equal(canaryRetry.version.id, canaryVersion);
    assert.equal(canaryRetry.envelope.refreshed_at, canary.envelope.refreshed_at);

    const publishCanary = toolData(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: injectedData,
        source_as_of: source1,
        expected_version: dataV0,
      })
    );
    assert.equal(publishCanary.deduped, true);
    assert.equal(publishCanary.published, true);
    assert.equal(publishCanary.version.id, canaryVersion);
    assert.equal(publishCanary.version_is_live, true);

    const coverageAdvance = toolData(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: injectedData,
        source_as_of: source2,
        expected_version: canaryVersion,
      })
    );
    assert.equal(coverageAdvance.deduped, false, "new source coverage creates a version when metrics are unchanged");
    assert.notEqual(coverageAdvance.version.id, canaryVersion);
    const dataV2 = coverageAdvance.version.id;

    const exactLiveRetry = toolData(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: injectedData,
        source_as_of: source2,
        expected_version: dataV2,
      })
    );
    assert.equal(exactLiveRetry.deduped, true);
    assert.equal(exactLiveRetry.published, false, "an exact live retry does not rewrite the pointer/audit row");
    assert.equal(exactLiveRetry.version_is_live, true);

    const regression = toolError(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: { count: 3, label: "regress" },
        source_as_of: source1,
        expected_version: dataV2,
      })
    );
    assert.equal(regression.data.code, "source_regression");
    assert.equal(regression.data.details.current_source_as_of, source2);
    const future = toolError(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: { count: 3, label: "future" },
        source_as_of: new Date(Date.now() + 60 * 60 * 1000).toISOString(),
        expected_version: dataV2,
      })
    );
    assert.equal(future.data.code, "source_in_future");
    const staleData = toolError(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: { count: 3, label: "stale writer" },
        source_as_of: source3,
        expected_version: canaryVersion,
      })
    );
    assert.equal(staleData.data.code, "stale_version");
    console.log("✓ managed data canary/publish/dedupe/coverage/schema/injection contracts");

    // 19b. Issue #102. A Vandelay dashboard served wrong DSP and SSP numbers to a
    // client and every payload had satisfied its JSON Schema perfectly: the DSP
    // series started Jul 30 instead of Jul 21 because only the newest fast.io
    // daily folder was read (a trailing-7-day export, not a cumulative one), and
    // SSP totals were understated ~7% because rows were filtered to the three
    // configured deals, dropping a fourth that carried all of Jul 6's spend.
    // Shape validation cannot see either. This walks the incident end to end.
    const rowSchema = {
      $schema: "https://json-schema.org/draft/2020-12/schema",
      type: "object",
      additionalProperties: false,
      required: ["rows"],
      properties: {
        dataThrough: { type: ["string", "null"] },
        rows: {
          type: "array",
          items: {
            type: "object",
            additionalProperties: false,
            required: ["date", "dealId", "spend"],
            properties: { date: { type: "string" }, dealId: { type: "string" }, spend: { type: "number" } },
          },
        },
      },
    };
    const campaignRows = () => {
      const out = [];
      for (let day = 6; day <= 36; day++) {
        const date = day <= 31 ? `2026-07-${String(day).padStart(2, "0")}` : `2026-08-${String(day - 31).padStart(2, "0")}`;
        if (day <= 16) out.push({ date, dealId: "1442375", spend: day === 6 ? 502.61 : 118.542 });
        if (day >= 7) {
          out.push({ date, dealId: "allergy", spend: 240.3364 });
          out.push({ date, dealId: "pollen", spend: 284.9543 });
          out.push({ date, dealId: "severe", spend: 276.551 });
        }
      }
      return out;
    };
    const profileHtml =
      `<!doctype html><html><head><title>Campaign</title></head><body><main>Rows</main>` +
      `<script type="application/schema+json" id="pages-data-schema">${JSON.stringify(rowSchema)}</script>` +
      `<script type="application/json" id="pages-data">${JSON.stringify({
        contract_version: 1,
        refreshed_at: source0,
        source_as_of: source0,
        data: { dataThrough: null, rows: [] },
      })}</script></body></html>`;
    const profileBase = toolData(
      await callTool("deploy_page", { slug: "mcp-profile", title: "Profile", html: profileHtml, render_mode: "raw" })
    );

    // The correct refresh: 31 days, 4 deals. The profile is what a human or an
    // agent reconciles against the source export — none of it was available before.
    const goodRefresh = toolData(
      await callTool("update_page_data", {
        slug: "mcp-profile",
        data: { dataThrough: "2026-08-05", rows: campaignRows() },
        source_as_of: source1,
        expected_version: profileBase.version.id,
      })
    );
    assert.equal(goodRefresh.data_profile.arrays.rows.count, 101);
    assert.equal(goodRefresh.data_profile.arrays.rows.fields.date.min, "2026-07-06");
    assert.equal(goodRefresh.data_profile.arrays.rows.fields.date.max, "2026-08-05");
    assert.equal(goodRefresh.data_profile.arrays.rows.fields.spend.sum, 25743.281);
    assert.deepEqual(Object.keys(goodRefresh.data_profile.arrays.rows.fields.dealId.values).sort(), [
      "1442375",
      "allergy",
      "pollen",
      "severe",
    ]);
    // A caller's own numbers come back as the caller's own numbers. Pages
    // canonicalizes BIGINT ids to decimal strings by KEY NAME, and that
    // heuristic used to walk the payload too: a client's `campaign_id: 12345`
    // came back `"12345"` from every read and write. The documented loop is
    // get_page_data -> edit -> update_page_data, so that string went straight
    // back into a schema saying `{"type":"integer"}` and was refused on data
    // Pages had just served. The page HTML always held the number, so the served
    // page and the tool response disagreed about the same field.
    const idSchema = {
      $schema: "https://json-schema.org/draft/2020-12/schema",
      type: "object",
      additionalProperties: false,
      required: ["rows"],
      properties: {
        rows: {
          type: "array",
          items: {
            type: "object",
            additionalProperties: false,
            required: ["campaign_id", "id", "spend"],
            properties: {
              campaign_id: { type: "integer" },
              id: { type: "integer" },
              spend: { type: "number" },
            },
          },
        },
      },
    };
    const idHtml =
      `<!doctype html><html><head><title>Ids</title></head><body><main>Rows</main>` +
      `<script type="application/schema+json" id="pages-data-schema">${JSON.stringify(idSchema)}</script>` +
      `<script type="application/json" id="pages-data">${JSON.stringify({
        contract_version: 1,
        refreshed_at: "2026-08-01T00:00:00.000Z",
        source_as_of: "2026-08-01T00:00:00.000Z",
        data: { rows: [] },
      })}</script></body></html>`;
    const idBase = toolData(
      await callTool("deploy_page", {
        slug: "mcp-numeric-ids",
        title: "Numeric ids",
        html: idHtml,
        render_mode: "raw",
        publish: true,
      })
    );
    const idPayload = { rows: [{ campaign_id: 12345, id: 7, spend: 10.5 }] };
    const idWrite = toolData(
      await callTool("update_page_data", {
        slug: "mcp-numeric-ids",
        data: idPayload,
        source_as_of: "2026-08-05T00:00:00Z",
        expected_version: idBase.version.id,
      })
    );
    assert.deepEqual(idWrite.envelope.data, idPayload, "the write echoes the caller's numbers unchanged");
    const idRead = toolData(await callTool("get_page_data", { slug: "mcp-numeric-ids" }));
    assert.deepEqual(idRead.envelope.data, idPayload, "and so does the read");
    assert.equal(typeof idRead.envelope.data.rows[0].campaign_id, "number");
    assert.equal(typeof idRead.envelope.data.rows[0].id, "number");
    // Pages' OWN ids stay decimal strings — the canonicalization still applies
    // everywhere outside the caller's document.
    assert.equal(typeof idRead.live_version_id, "string");
    // The round trip the incident loop actually performs must be accepted.
    const idRoundTrip = toolData(
      await callTool("update_page_data", {
        slug: "mcp-numeric-ids",
        data: idRead.envelope.data,
        source_as_of: "2026-08-06T00:00:00Z",
        expected_version: idRead.live_version_id,
      })
    );
    assert.equal(idRoundTrip.version_is_live, true, "re-writing what get_page_data returned must validate");
    assert.equal(
      idRoundTrip.data_profile.arrays.rows.fields.campaign_id.kind,
      "number",
      "and the profile still sees an integer column, not a key column"
    );

    // The Vandelay incident's actual write: the FIRST payload was already truncated, so
    // no diff could see it. A first real payload over an empty one is therefore
    // reported as unverified rather than silently accepted.
    assert.deepEqual(
      goodRefresh.data_warnings.map((w) => `${w.code}@${w.path}`),
      ["coverage_unverified@rows"],
      JSON.stringify(goodRefresh.data_warnings)
    );
    assert.match(goodRefresh.next_step, /RECONCILE BEFORE SHARING/);
    assert.equal(goodRefresh.data_profile.scalars.dataThrough, "2026-08-05");

    // The refresh that shipped. Schema-valid, and wrong in two ways at once.
    const badRefresh = toolData(
      await callTool("update_page_data", {
        slug: "mcp-profile",
        data: {
          dataThrough: "2026-08-05",
          rows: campaignRows().filter((row) => row.date >= "2026-07-30" && row.dealId !== "1442375"),
        },
        source_as_of: source2,
        expected_version: goodRefresh.version.id,
      })
    );
    const warned = badRefresh.data_warnings.map((w) => w.code);
    assert.ok(warned.includes("coverage_start_regressed"), warned.join(","));
    assert.ok(warned.includes("dimension_values_missing"), warned.join(","));
    assert.ok(warned.includes("row_count_dropped"), warned.join(","));
    // Warnings, not errors: narrowing is sometimes right, so the write lands.
    assert.equal(badRefresh.version_is_live, true);
    // But it cannot be missed — a warning nobody reads is not a safeguard.
    assert.match(badRefresh.next_step, /RECONCILE BEFORE SHARING/);
    assert.match(badRefresh.next_step, /coverage_start_regressed/);

    // get_page_data profiles the stored payload too, so a page an agent did not
    // just write can still be reconciled without pulling the envelope apart.
    const readBack = toolData(await callTool("get_page_data", { slug: "mcp-profile" }));
    assert.equal(readBack.data_profile.arrays.rows.fields.date.min, "2026-07-30");
    assert.equal(readBack.data_profile.arrays.rows.count, 21);

    // expect is the stronger guarantee: state what the source said and the write
    // is refused if the payload disagrees. This is the ~7% understatement.
    const reconciliationFailed = toolError(
      await callTool("update_page_data", {
        slug: "mcp-profile",
        data: { dataThrough: "2026-08-05", rows: campaignRows().filter((row) => row.dealId !== "1442375") },
        source_as_of: source3,
        expected_version: badRefresh.version.id,
        expect: { row_count: { rows: 101 }, totals: { "rows.spend": 25743.281 } },
      })
    );
    assert.equal(reconciliationFailed.data.code, "data_reconciliation_failed");
    assert.equal(reconciliationFailed.data.details.mismatches.length, 2);
    const unchangedByRefusal = toolData(await callTool("get_page_data", { slug: "mcp-profile" }));
    assert.equal(
      unchangedByRefusal.live_version_id,
      badRefresh.version.id,
      "a refused reconciliation writes nothing at all"
    );

    // And the correct payload, with the same expectations, lands.
    const reconciled = toolData(
      await callTool("update_page_data", {
        slug: "mcp-profile",
        data: { dataThrough: "2026-08-05", rows: campaignRows() },
        source_as_of: source3,
        expected_version: badRefresh.version.id,
        expect: {
          row_count: { rows: 101 },
          totals: { "rows.spend": 25743.281 },
          date_range: { "rows.date": ["2026-07-06", "2026-08-05"] },
        },
      })
    );
    assert.equal(reconciled.version_is_live, true);
    assert.equal(reconciled.data_profile.arrays.rows.count, 101);
    assert.deepEqual(
      reconciled.data_warnings,
      [],
      "declaring the window in expect is stronger than warning about it, so the warning withdraws"
    );

    // A refresh that added nothing. A real one went out as "Version 205 published
    // and live" with "Data Warnings: None" and the same coverage as the version
    // before it; the recipient asked why yesterday's data was missing. A newer
    // source_as_of over byte-identical data is deliberately not a dedupe, so
    // deduped:false, version_is_live:true and a fresh version id all read like new
    // numbers landed.
    const sameRows = { dataThrough: "2026-08-05", rows: campaignRows() };
    const later = new Date(Date.now() - 30 * 60 * 1000).toISOString();
    const noNewData = toolData(
      await callTool("update_page_data", {
        slug: "mcp-profile",
        data: sameRows,
        source_as_of: later,
        expected_version: reconciled.version.id,
      })
    );
    assert.equal(noNewData.deduped, false, "a newer source_as_of over identical data is a real new version");
    assert.equal(noNewData.version_is_live, true);
    assert.deepEqual(
      noNewData.data_warnings.map((w) => w.code),
      ["data_unchanged"],
      JSON.stringify(noNewData.data_warnings)
    );
    // The sentence has to be different from the narrowing case: the mistake here
    // is announcing an update that did not happen, not a number to re-check.
    assert.match(noNewData.next_step, /THIS REFRESH ADDED NO NEW DATA/);
    assert.match(noNewData.next_step, /Do not describe this as a data update/);
    assert.doesNotMatch(noNewData.next_step, /RECONCILE BEFORE SHARING/);

    // Replaying the exact same write IS a dedupe, and a dedupe already reports
    // "already live" in its own fields — so the stale pair is suppressed there,
    // where data_unchanged's "a version was still created" would be untrue.
    const replay = toolData(
      await callTool("update_page_data", {
        slug: "mcp-profile",
        data: sameRows,
        source_as_of: later,
        expected_version: noNewData.version.id,
      })
    );
    assert.equal(replay.deduped, true);
    assert.deepEqual(replay.data_warnings, [], JSON.stringify(replay.data_warnings));
    assert.doesNotMatch(replay.next_step, /ADDED NO NEW DATA/);

    // And a refresh that genuinely extends the window is silent again.
    const extended = toolData(
      await callTool("update_page_data", {
        slug: "mcp-profile",
        data: { dataThrough: "2026-08-06", rows: [...campaignRows(), { date: "2026-08-06", dealId: "allergy", spend: 12.5 }] },
        source_as_of: new Date(Date.now() - 15 * 60 * 1000).toISOString(),
        expected_version: noNewData.version.id,
      })
    );
    assert.deepEqual(extended.data_warnings, [], JSON.stringify(extended.data_warnings));

    toolData(await callTool("delete_page", { slug: "mcp-profile" }));
    console.log("✓ data_profile, regression warnings, expect reconciliation, and stale-refresh detection (#102)");

    // 20. Overlapping writers serialize on the page lock. Exactly one caller
    // can move the pointer from the shared expected version.
    const overlapping = await Promise.all([
      callTool("update_page_data", {
        slug: "mcpdata",
        data: { count: 4, label: "overlap-a" },
        source_as_of: source3,
        expected_version: dataV2,
      }),
      callTool("update_page_data", {
        slug: "mcpdata",
        data: { count: 5, label: "overlap-b" },
        source_as_of: source3,
        expected_version: dataV2,
      }),
    ]);
    const overlapSuccesses = overlapping.filter((response) => !response.json.result.isError);
    const overlapErrors = overlapping.filter((response) => response.json.result.isError);
    assert.equal(overlapSuccesses.length, 1);
    assert.equal(overlapErrors.length, 1);
    assert.equal(toolError(overlapErrors[0]).data.code, "stale_version");
    const overlapWinner = toolData(overlapSuccesses[0]);
    assert.equal(overlapWinner.version_is_live, true);
    console.log("✓ overlapping data writers serialize; stale writer cannot clobber live data");

    // 21. Approval and takedown governance remains owned by the ordinary state
    // machine; unmanaged pages fail with the bounded opt-in error.
    const unmanaged = toolError(await callTool("get_page_data", { slug: "mcpdemo" }));
    assert.equal(unmanaged.data.code, "page_not_data_managed");
    const gatedDataBase = toolData(
      await callTool("deploy_page", {
        slug: "mcpgateddata",
        title: "Gated Managed Data",
        html: managedHTML(source0),
      })
    );
    await db.query("UPDATE pages SET require_approval = true WHERE slug = 'mcpgateddata'");
    const gatedData = toolData(
      await callTool("update_page_data", {
        slug: "mcpgateddata",
        data: { count: 2, label: "pending" },
        source_as_of: source1,
        expected_version: gatedDataBase.version.id,
      })
    );
    assert.equal(gatedData.gated, true);
    assert.equal(gatedData.version.status, "pending");
    assert.equal(gatedData.version_is_live, false);
    assert.equal(gatedData.live_version_id, gatedDataBase.version.id);

    await db.query("UPDATE pages SET disabled = true WHERE slug = 'mcpdata'");
    const disabledData = toolError(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: { count: 6, label: "disabled" },
        source_as_of: new Date(Date.now() - 30 * 60 * 1000).toISOString(),
        expected_version: overlapWinner.version.id,
      })
    );
    assert.equal(disabledData.data.code, "disabled_takedown");
    const disabledRead = toolData(await callTool("get_page_data", { slug: "mcpdata" }));
    assert.equal(disabledRead.page_is_live, false);
    assert.equal(disabledRead.live_version_id, overlapWinner.version.id, "published pointer is retained during takedown");
    await db.query("UPDATE pages SET disabled = false WHERE slug = 'mcpdata'");
    console.log("✓ managed data honors opt-in, approval gate, disabled state, and live-pointer truth");

    // 22. Structured-data identity is immutable and every write is attributed
    // to the exact bearer token in the audit log.
    await assert.rejects(
      () => db.query("UPDATE page_versions SET data_sha256 = repeat('0', 64) WHERE id = $1", [overlapWinner.version.id]),
      /immutable/i
    );
    const dataAudit = await db.query(
      `SELECT action, token_id, metadata
         FROM audit_log
        WHERE version_id = $1 AND action = 'data_update'`,
      [overlapWinner.version.id]
    );
    assert.equal(dataAudit.rowCount, 1);
    assert.equal(String(dataAudit.rows[0].token_id), String(minted.id));
    assert.equal(dataAudit.rows[0].metadata.source_as_of, source3);
    console.log("✓ immutable data metadata and bearer-token audit attribution");

    // 22b. A refresh that ran correctly and published NOTHING is the outcome
    // Pages recorded nowhere, so a page whose upstream has frozen looked exactly
    // like one nobody runs any more. record_refresh_check separates them without
    // creating a version — the client keeps serving the same bytes.
    const beforeCheck = toolData(await callTool("get_page_data", { slug: "mcpdata" }));
    const versionsBeforeCheck = Number(
      (await db.query("SELECT count(*) FROM page_versions WHERE page_id = (SELECT id FROM pages WHERE slug = 'mcpdata')")).rows[0]
        .count
    );
    assert.ok(beforeCheck.freshness, "a managed page reports freshness");
    assert.equal(beforeCheck.freshness.source_as_of, beforeCheck.envelope.source_as_of);
    assert.equal(beforeCheck.freshness.last_check_outcome, null, "nothing has recorded a check yet");
    assert.equal(
      beforeCheck.freshness.checked_at,
      beforeCheck.freshness.refreshed_at,
      "with no recorded check, the last time anyone looked is the last write"
    );

    const recorded = toolData(
      await callTool("record_refresh_check", {
        slug: "mcpdata",
        outcome: "source_not_updated",
        detail: "upstream max date unchanged",
        source_as_of_seen: beforeCheck.envelope.source_as_of,
      })
    );
    assert.equal(recorded.freshness.last_check_outcome, "source_not_updated");
    assert.equal(recorded.freshness.days_since_check, 0);
    assert.equal(recorded.freshness.source_as_of, beforeCheck.freshness.source_as_of, "coverage is untouched");

    const afterCheck = toolData(await callTool("get_page_data", { slug: "mcpdata" }));
    assert.equal(afterCheck.data_sha256, beforeCheck.data_sha256, "no byte of the page changed");
    assert.equal(afterCheck.live_version_id, beforeCheck.live_version_id, "the published pointer did not move");
    assert.equal(afterCheck.freshness.last_check_outcome, "source_not_updated");
    assert.equal(afterCheck.freshness.last_check_detail, "upstream max date unchanged");
    assert.ok(
      Date.parse(afterCheck.freshness.checked_at) >= Date.parse(afterCheck.freshness.refreshed_at),
      "checked_at moved past the last write"
    );
    assert.equal(
      Number(
        (await db.query("SELECT count(*) FROM page_versions WHERE page_id = (SELECT id FROM pages WHERE slug = 'mcpdata')"))
          .rows[0].count
      ),
      versionsBeforeCheck,
      "recording a check creates no version"
    );

    // The list read carries the same block, so one call ranks the estate.
    const listedFreshness = toolData(await callTool("list_pages", { limit: 100 })).pages.find((p) => p.slug === "mcpdata");
    assert.equal(listedFreshness.freshness.last_check_outcome, "source_not_updated");
    assert.equal(listedFreshness.freshness.source_as_of, afterCheck.freshness.source_as_of);

    // Closed vocabulary: an outcome nobody can branch on is refused at the
    // schema, before it can be written. (SDK argument-validation errors arrive
    // as human-readable text, not a Pages domain-error object.)
    const badOutcome = toolError(await callTool("record_refresh_check", { slug: "mcpdata", outcome: "probably_fine" }));
    assert.match(badOutcome.text, /outcome/i);
    assert.equal(
      (await db.query("SELECT last_check_outcome FROM pages WHERE slug = 'mcpdata'")).rows[0].last_check_outcome,
      "source_not_updated",
      "a refused outcome leaves the recorded one alone"
    );
    const checkAudit = await db.query(
      `SELECT action, token_id, metadata FROM audit_log
        WHERE action = 'record_refresh_check' AND page_id = (SELECT id FROM pages WHERE slug = 'mcpdata')`
    );
    assert.equal(checkAudit.rowCount, 1);
    assert.equal(String(checkAudit.rows[0].token_id), String(minted.id));
    assert.equal(checkAudit.rows[0].metadata.outcome, "source_not_updated");
    console.log("✓ a no-op refresh is recordable without a version, and shows up in both reads");

    // 22c. Managed data gets the staged path HTML has had since #14. The
    // payload that motivated it was 978 KB and growing 50 KB in five days, and
    // the run that built it burned six tool searches hunting for a file-backed
    // variant, found none, and aborted with the page unchanged (fleet 3d767956).
    const stagedPayload = { count: 41, label: "staged" };
    const stagedBytes = Buffer.from(JSON.stringify(stagedPayload), "utf8");
    const beforeStaged = toolData(await callTool("get_page_data", { slug: "mcpdata" }));
    const dataTicket = toolData(
      await callTool("create_upload_ticket", {
        slug: "mcpdata",
        kind: "data",
        total_bytes: stagedBytes.length,
        content_sha256: crypto.createHash("sha256").update(stagedBytes).digest("hex"),
      })
    );
    assert.equal(dataTicket.target_kind, "data");
    assert.equal(dataTicket.template, null, "a data upload names a slug, never a template");
    assert.match(dataTicket.next_step, /update_page_data_upload/);
    const dataSent = await request("PUT", `/upload/${dataTicket.upload_id}`, {
      rawBody: stagedBytes,
      token: dataTicket.ticket,
    });
    assert.equal(dataSent.status, 200, dataSent.body);
    assert.equal(dataSent.json.complete, true);

    // A data upload is not page HTML, and the type system says so under the row
    // lock — before any bytes are read.
    const dataAsPage = toolError(
      await callTool("deploy_page_upload", { upload_id: dataTicket.upload_id, expected_version: beforeStaged.live_version_id })
    );
    assert.equal(dataAsPage.data.code, "page_upload_target_mismatch");
    assert.match(dataAsPage.data.error, /update_page_data_upload/);

    const stagedSource = new Date(Date.parse(beforeStaged.envelope.source_as_of) + 1000).toISOString();

    // expect reconciliation runs on this route too, before anything is written
    // — and a refusal must not spend the upload, or the caller pays for the
    // transfer twice to correct one number.
    const misreconciled = toolError(
      await callTool("update_page_data_upload", {
        upload_id: dataTicket.upload_id,
        slug: "mcpdata",
        source_as_of: stagedSource,
        expected_version: beforeStaged.live_version_id,
        expect: { row_count: { rows: 12 } },
      })
    );
    assert.equal(misreconciled.data.code, "data_reconciliation_failed");
    assert.equal(
      toolData(await callTool("get_page_data", { slug: "mcpdata" })).data_sha256,
      beforeStaged.data_sha256,
      "a refused reconciliation changes nothing"
    );

    const stagedResult = toolData(
      await callTool("update_page_data_upload", {
        upload_id: dataTicket.upload_id,
        slug: "mcpdata",
        source_as_of: stagedSource,
        expected_version: beforeStaged.live_version_id,
      })
    );
    assert.equal(stagedResult.version_is_live, true);
    assert.equal(stagedResult.upload_id, dataTicket.upload_id);
    assert.deepEqual(stagedResult.envelope.data, stagedPayload);
    assert.ok(stagedResult.data_profile, "the staged route profiles the payload like the inline one");

    // Same payload inline produces the SAME content hash: one write path, not
    // two. (source_as_of advances, so this is a real second version rather than
    // a dedupe — the point is the bytes agree.)
    const inlineEcho = toolData(
      await callTool("update_page_data", {
        slug: "mcpdata",
        data: stagedPayload,
        source_as_of: new Date(Date.parse(stagedSource) + 1000).toISOString(),
        expected_version: stagedResult.version.id,
      })
    );
    assert.equal(inlineEcho.data_sha256, stagedResult.data_sha256, "staged and inline writes agree byte-for-byte");
    assert.equal(inlineEcho.schema_sha256, stagedResult.schema_sha256);

    // An exact retry of a spent upload returns the original commit result; a
    // retry with different options is a conflict, not a second publish.
    const stagedRetry = toolData(
      await callTool("update_page_data_upload", {
        upload_id: dataTicket.upload_id,
        slug: "mcpdata",
        source_as_of: stagedSource,
        expected_version: beforeStaged.live_version_id,
      })
    );
    assert.equal(stagedRetry.version.id, stagedResult.version.id, "exact retry replays the commit result");
    assert.equal(
      toolError(
        await callTool("update_page_data_upload", {
          upload_id: dataTicket.upload_id,
          slug: "mcpdata",
          source_as_of: stagedSource,
          expected_version: beforeStaged.live_version_id,
          note: "different options",
        })
      ).data.code,
      "page_upload_commit_conflict"
    );

    // The mirror-image guard: page HTML staged as a page cannot be parsed into
    // a data envelope.
    const htmlBytes = Buffer.from(HTML(400), "utf8");
    const htmlTicket = toolData(
      await callTool("create_upload_ticket", {
        slug: "mcpdata",
        total_bytes: htmlBytes.length,
        content_sha256: crypto.createHash("sha256").update(htmlBytes).digest("hex"),
      })
    );
    await request("PUT", `/upload/${htmlTicket.upload_id}`, { rawBody: htmlBytes, token: htmlTicket.ticket });
    const notData = toolError(
      await callTool("update_page_data_upload", {
        upload_id: htmlTicket.upload_id,
        slug: "mcpdata",
        source_as_of: new Date().toISOString(),
        expected_version: inlineEcho.version.id,
      })
    );
    assert.equal(notData.data.code, "page_upload_target_mismatch");

    // JSON that is not an object is refused with the same contract the inline
    // argument has always had, and the upload is not spent by the refusal.
    const arrayBytes = Buffer.from("[1,2,3]", "utf8");
    const arrayTicket = toolData(
      await callTool("create_upload_ticket", {
        slug: "mcpdata",
        kind: "data",
        total_bytes: arrayBytes.length,
        content_sha256: crypto.createHash("sha256").update(arrayBytes).digest("hex"),
      })
    );
    await request("PUT", `/upload/${arrayTicket.upload_id}`, { rawBody: arrayBytes, token: arrayTicket.ticket });
    assert.equal(
      toolError(
        await callTool("update_page_data_upload", {
          upload_id: arrayTicket.upload_id,
          slug: "mcpdata",
          source_as_of: new Date().toISOString(),
          expected_version: inlineEcho.version.id,
        })
      ).data.code,
      "data_invalid"
    );

    // The target is fixed when the upload starts; a mismatched slug is refused
    // rather than silently retargeted.
    const otherSlug = toolError(
      await callTool("update_page_data_upload", {
        upload_id: arrayTicket.upload_id,
        slug: "mcpgateddata",
        source_as_of: new Date().toISOString(),
        expected_version: inlineEcho.version.id,
      })
    );
    assert.equal(otherSlug.data.code, "page_upload_target_mismatch");

    // And the inline refusal now points at the path that exists.
    assert.match(
      toolError(
        await callTool("update_page_data", {
          slug: "mcpdata",
          data: { count: 1, label: "x".repeat(1_600_000) },
          source_as_of: new Date().toISOString(),
          expected_version: inlineEcho.version.id,
        })
      ).data.error,
      /create_upload_ticket with kind 'data'[\s\S]*update_page_data_upload/
    );
    // The chunked path can stage data too, for an environment with no outbound
    // HTTP. Its append UPDATE dropped target_kind from its RETURNING clause, so
    // every chunked template upload — and now every chunked data upload — has
    // been reporting itself as a page and naming the wrong consumer tool.
    const chunkPayload = { count: 7, label: "chunked" };
    const chunkBytes = Buffer.from(JSON.stringify(chunkPayload), "utf8");
    const chunkStart = toolData(
      await callTool("start_page_upload", {
        slug: "mcpdata",
        kind: "data",
        total_bytes: chunkBytes.length,
        content_sha256: crypto.createHash("sha256").update(chunkBytes).digest("hex"),
      })
    );
    assert.equal(chunkStart.target_kind, "data");
    const chunkAppended = toolData(
      await callTool("append_page_upload", {
        upload_id: chunkStart.upload_id,
        sequence: 0,
        chunk_base64: chunkBytes.toString("base64"),
      })
    );
    assert.equal(chunkAppended.target_kind, "data", "an append must not forget what the upload is for");
    assert.match(chunkAppended.next_step, /update_page_data_upload/);
    assert.equal(chunkAppended.complete, true);
    const chunkPublished = toolData(
      await callTool("update_page_data_upload", {
        upload_id: chunkStart.upload_id,
        slug: "mcpdata",
        source_as_of: new Date(Date.parse(stagedSource) + 2000).toISOString(),
        expected_version: inlineEcho.version.id,
      })
    );
    assert.deepEqual(chunkPublished.envelope.data, chunkPayload);
    console.log("✓ managed data has a staged path: same write, no inline ceiling, kinds cannot cross");

    // 23. A broad authoring client can turn the natural request "update <slug>
    // dashboard with ..." into a pinned one-time workflow or reusable scheduler
    // text. Prompt preparation is read-only and never creates a companion page.
    const versionCountBeforePrompts = Number((await db.query("SELECT count(*) FROM page_versions")).rows[0].count);
    const requestText = "Use yesterday's complete reporting export to update count and label.";
    const prepared = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: requestText,
        update_type: "data",
      })
    );
    assert.equal(prepared.mode, "managed_data");
    assert.equal(prepared.recurring, false);
    assert.equal(prepared.live_version_id, chunkPublished.version.id, "pinned to whatever is live now, not to a remembered id");
    assert.equal(prepared.schema_sha256, initialData.schema_sha256);
    assert.match(prepared.prompt, /TARGET SLUG: mcpdata/);
    assert.match(prepared.prompt, /mcp_pages_get_page_data/);
    assert.match(prepared.prompt, /mcp_pages_update_page_data/);
    assert.match(prepared.prompt, /never create another page, companion data page, or replacement slug/i);
    assert.match(prepared.next_step, /current conversation/i);
    assert.equal(prepared.prompt_sha256, crypto.createHash("sha256").update(prepared.prompt).digest("hex"));

    // Declared source bindings must survive the strict input/output schemas and
    // reach the prompt verbatim: naming a source in prose does not tell the
    // executing agent which connector serves it, and an agent that cannot reach
    // one must stop rather than substitute.
    const boundSources = [
      {
        source_id: "reporting",
        mcp_server: "reporting_mcp",
        account: "acme",
        required_tools: ["daily_report"],
        retrieval_instructions: "Complete days only.",
      },
    ];
    const bound = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: requestText,
        update_type: "data",
        sources: boundSources,
      })
    );
    assert.deepEqual(bound.sources, boundSources, "parsed bindings are echoed back to the caller");
    assert.match(bound.prompt, /REQUIRED SOURCE BINDINGS \(exact; no substitutions\)/);
    assert.match(bound.prompt, /- reporting: server reporting_mcp; account acme; tools daily_report — "Complete days only\."/);
    assert.match(bound.prompt, /a source absent from that list is out of scope/i);
    assert.notEqual(bound.prompt_sha256, prepared.prompt_sha256, "bindings change the prepared prompt");
    assert.equal(prepared.sources, null, "an unbound request reports no bindings");

    const credentialInBinding = toolError(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: requestText,
        update_type: "data",
        sources: [{ source_id: "x", mcp_server: "y", retrieval_instructions: "authenticate with Bearer abcdefghijklmnop" }],
      })
    );
    assert.ok(credentialInBinding, "a credential-shaped binding is rejected, not rendered into a prompt");

    const compatibilityGuidance = toolData(await callTool("get_page_refresh", { slug: "mcpdata" }));
    assert.equal(compatibilityGuidance.scheduling, "user_owned");
    assert.match(compatibilityGuidance.next_step, /configure_page_refresh/);
    const compatibilityPrepared = toolData(
      await callTool("configure_page_refresh", {
        slug: "mcpdata",
        instructions: requestText,
        update_type: "data",
      })
    );
    assert.equal(compatibilityPrepared.prompt_sha256, prepared.prompt_sha256, "static-client alias is the canonical read-only operation");
    const legacyCompatibilityPrepared = toolData(
      await callTool("configure_page_refresh", {
        slug: "mcpdata",
        daily_at_utc: "08:00",
        workflow: {
          sources: [{ source_id: "reporting", required_tools: ["daily_report"] }],
          mapping: [{ target_path: "/count", expression: "daily_report.count" }],
        },
        publish: true,
        run_now: true,
      })
    );
    // This workflow's entries carry no mcp_server, so no bindings can be lifted
    // — and an old client has no way to supply them. The compatibility alias
    // must keep working rather than inherit the recurring-bindings gate (#121).
    assert.equal(legacyCompatibilityPrepared.recurring, true);
    assert.match(legacyCompatibilityPrepared.prompt, /daily at 08:00 UTC/i);
    assert.match(legacyCompatibilityPrepared.next_step, /did not honor run_now/i);
    assert.match(legacyCompatibilityPrepared.next_step, /Show prompt to the user verbatim/i);

    // A recurring prompt runs unattended weeks later against a live client page,
    // so it must carry real bindings rather than prose an executing agent has to
    // re-derive (#121).
    const recurringWithoutBindings = toolError(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: requestText,
        recurring: true,
      })
    );
    assert.equal(
      recurringWithoutBindings.data.code,
      "update_sources_required",
      "a recurring update cannot be built from prose alone"
    );

    const recurringPrompt = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: requestText,
        recurring: true,
        sources: [
          {
            source_id: "amazon_dsp",
            mcp_server: "fastio_helpers",
            path: "NWM_keel/Meridian/daily",
            partition: { by: "date", format: "YYYY-MM-DD", since: "source_as_of" },
            required_tools: ["list_partitions"],
          },
        ],
      })
    );
    assert.equal(recurringPrompt.mode, "managed_data");
    assert.equal(recurringPrompt.recurring, true);
    assert.match(recurringPrompt.prompt, /user-owned scheduler/i);
    assert.match(recurringPrompt.next_step, /Show prompt to the user verbatim/i);
    // A partitioned source must render as enumerate-the-range, not newest-wins.
    assert.match(recurringPrompt.prompt, /PARTITIONED by date/);
    assert.match(recurringPrompt.prompt, /never take only the newest/);
    // The scheduler-facing contract: what this run needs, before it is accepted.
    assert.deepEqual(recurringPrompt.execution_requirements.mcp_servers, ["fastio_helpers", "pages"]);
    assert.equal(recurringPrompt.execution_requirements.model_required, true);

    const adaptive = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: "Update the totals and improve the summary card if needed.",
      })
    );
    assert.equal(adaptive.mode, "adaptive");
    assert.match(adaptive.prompt, /Classify USER REQUEST before writing/);

    const migration = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdemo",
        instructions: "Replace revenue with the latest finance export.",
        update_type: "data",
      })
    );
    assert.equal(migration.mode, "migration_required");
    assert.equal(migration.schema_sha256, null);
    assert.match(migration.prompt, /MANAGED-DATA MIGRATION REQUIRED/);
    assert.match(migration.prompt, /Never create a new slug or companion data page/i);

    const layout = toolData(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdemo",
        instructions: "Add a compact date selector above the chart.",
        update_type: "layout",
        publish: false,
      })
    );
    assert.equal(layout.mode, "full_page");
    assert.match(layout.prompt, /PUBLISH: false/);
    assert.match(layout.prompt, /mcp_pages_deploy_page_upload/);

    const forbiddenSecret = toolError(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: "Use token pgs_1234567890abcdefghijklmnop to update it.",
        update_type: "data",
      })
    );
    assert.equal(forbiddenSecret.data.code, "update_credentials_forbidden");
    const badRecurringLayout = toolError(
      await callTool("prepare_dashboard_update", {
        slug: "mcpdata",
        instructions: "Redesign the cards daily.",
        recurring: true,
        update_type: "layout",
      })
    );
    assert.equal(badRecurringLayout.data.code, "recurring_layout_forbidden");
    const missingDashboard = toolError(
      await callTool("prepare_dashboard_update", {
        slug: "does-not-exist",
        instructions: "Update it.",
      })
    );
    assert.equal(missingDashboard.data.code, "page_not_found");
    const versionCountAfterPrompts = Number((await db.query("SELECT count(*) FROM page_versions")).rows[0].count);
    assert.equal(versionCountAfterPrompts, versionCountBeforePrompts, "prompt preparation never mutates page versions");
    assert.equal((await db.query("SELECT count(*)::int AS count FROM page_refreshes")).rows[0].count, 0);
    console.log("✓ exact-slug update requests and static-client aliases produce safe prompts without mutation");

    // 24. The forward migration makes any legacy Pages/MOC state inert while
    // retaining its rows for audit and removing the old runner's slug grant.
    const retiredRunner = await tokens.mint({
      label: "retired-refresh-runner",
      scope: "data_update",
      allowedSlugs: ["mcpdata"],
    });
    const legacyRefresh = await db.query(
      `INSERT INTO page_refreshes
         (page_id, status, daily_at_utc, workflow, prompt, schema_sha256, publish,
          target_node_name, runtime_token_id, next_run_at)
       SELECT id, 'active', '08:00', '{}'::jsonb, 'legacy prompt', $1, true,
              'legacy-runner-*', $2, now()
         FROM pages WHERE slug = 'mcpdata' AND deleted_at IS NULL
       RETURNING id`,
      [initialData.schema_sha256, retiredRunner.id]
    );
    const legacyRefreshId = legacyRefresh.rows[0].id;
    await db.query(
      `INSERT INTO page_refresh_runs
         (refresh_id, kind, scheduled_for, prompt, target_node_name, run_key, status, next_attempt_at)
       VALUES
         ($1, 'manual', now(), 'queued', 'legacy-runner-*', $2, 'queued', now()),
         ($1, 'scheduled', now() + interval '1 minute', 'failed', 'legacy-runner-*', $3, 'error', now()),
         ($1, 'manual', now() + interval '2 minutes', 'sent', 'legacy-runner-*', $4, 'dispatched', now())`,
      [legacyRefreshId, `legacy-${crypto.randomUUID()}`, `legacy-${crypto.randomUUID()}`, `legacy-${crypto.randomUUID()}`]
    );
    const retirementSql = fs.readFileSync(
      path.join(__dirname, "../migrations/010_retire_moc_refresh_dispatch.sql"),
      "utf8"
    );
    await db.query(retirementSql);
    await db.query(retirementSql);
    const retiredDefinition = await db.query("SELECT status, last_error FROM page_refreshes WHERE id = $1", [legacyRefreshId]);
    assert.equal(retiredDefinition.rows[0].status, "paused");
    assert.match(retiredDefinition.rows[0].last_error, /user-owned scheduler/i);
    const retiredRuns = await db.query(
      "SELECT status FROM page_refresh_runs WHERE refresh_id = $1 ORDER BY scheduled_for",
      [legacyRefreshId]
    );
    assert.deepEqual(retiredRuns.rows.map((row) => row.status), ["cancelled", "cancelled", "dispatched"]);
    assert.deepEqual((await tokens.verify(retiredRunner.token)).allowed_slugs, []);
    console.log("✓ legacy Pages/MOC definitions are paused, undispatched runs cancelled, and runner grants revoked");

    // 25. data_update tokens see/call only the data tools and only on exact
    // granted slugs. REST and every unrelated MCP tool fail closed; revocation
    // remains immediate. The deploy token retains the complete legacy surface.
    //
    // record_refresh_check joined the two: the refresh a scoped token runs most
    // often is the one that publishes nothing, and that outcome has to be
    // recordable by the same token that discovered it. It cannot create or move
    // a version and is slug-gated identically, so the scope gains no reach.
    const scoped = await tokens.mint({
      label: "daily-mcpdata",
      scope: "data_update",
      allowedSlugs: ["mcpdata"],
    });
    const tokenRows = await tokens.list();
    const scopedRow = tokenRows.find((row) => String(row.id) === String(scoped.id));
    assert.deepEqual(scopedRow.allowed_slugs, ["mcpdata"]);
    const scopedInit = await initialize(PROTOCOL_VERSION, { token: scoped.token });
    assert.match(scopedInit.json.result.instructions, /restricted to Pages managed-data automation/i);
    const scopedList = await rpc("tools/list", {}, { token: scoped.token });
    const DATA_SCOPE_TOOLS = ["get_page_data", "record_refresh_check", "update_page_data"];
    assert.deepEqual(scopedList.json.result.tools.map((tool) => tool.name).sort(), DATA_SCOPE_TOOLS);
    const scopedRead = toolData(await callTool("get_page_data", { slug: "mcpdata" }, { token: scoped.token }));
    assert.equal(scopedRead.page.slug, "mcpdata");
    const crossPage = toolError(
      await callTool("get_page_data", { slug: "mcpgateddata" }, { token: scoped.token })
    );
    assert.equal(crossPage.data.code, "slug_not_allowed");
    // A grant is per-slug on this tool too, not merely per-scope.
    assert.equal(
      toolError(
        await callTool(
          "record_refresh_check",
          { slug: "mcpgateddata", outcome: "source_not_updated" },
          { token: scoped.token }
        )
      ).data.code,
      "slug_not_allowed"
    );
    toolData(
      await callTool("record_refresh_check", { slug: "mcpdata", outcome: "blocked" }, { token: scoped.token })
    );
    for (const name of EXPECTED_TOOLS.filter((name) => !DATA_SCOPE_TOOLS.includes(name))) {
      const denied = await callTool(name, {}, { token: scoped.token });
      assert.equal(denied.status, 200, `${name} denial stays on the MCP result channel`);
      assert.equal(denied.json.result.isError, true, `${name} denied for data_update token`);
    }
    const scopedRest = await request("GET", "/api/v1/pages/mcpdata", { token: scoped.token });
    assert.equal(scopedRest.status, 403);
    assert.equal(scopedRest.json.code, "token_scope_denied");
    assert.equal(await tokens.revoke(scoped.id), true);
    assert.equal((await rpc("ping", {}, { token: scoped.token })).status, 401);
    const legacyList = await rpc("tools/list", {}, { token: bearerToken });
    assert.deepEqual(legacyList.json.result.tools.map((tool) => tool.name).sort(), EXPECTED_TOOLS);
    console.log("✓ exact-slug data_update authorization, denial matrix, revocation, and legacy compatibility");

    // 26. Grants bind to page IDENTITY, not slug text (issue #50). A grant
    // minted before its page exists binds lazily to the first live holder of
    // the slug; after delete→recreate (a NEW page row) the stale grant must
    // NOT follow the slug; delete→restore (the SAME row) keeps working.
    const rebind = await tokens.mint({
      label: "rebind-watch",
      scope: "data_update",
      allowedSlugs: ["grantrebind"],
    });
    const preCreate = toolError(
      await callTool("get_page_data", { slug: "grantrebind" }, { token: rebind.token })
    );
    assert.equal(
      preCreate.data.code, "page_not_found",
      "unbound grant still surfaces the page lookup result (unchanged legacy behavior)"
    );
    toolData(await callTool("deploy_page", { slug: "grantrebind", title: "Rebind", html: HTML(30) }));
    const boundRead = toolError(
      await callTool("get_page_data", { slug: "grantrebind" }, { token: rebind.token })
    );
    assert.equal(
      boundRead.data.code, "page_not_data_managed",
      "grant lazily bound to the first live page (authorization passed; the plain page is just not data-managed)"
    );
    toolData(await callTool("delete_page", { slug: "grantrebind" }));
    const afterDelete = toolError(
      await callTool("get_page_data", { slug: "grantrebind" }, { token: rebind.token })
    );
    assert.equal(
      afterDelete.data.code, "page_not_found",
      "deleted page keeps the legacy lookup result (no live page to protect)"
    );
    toolData(await callTool("deploy_page", { slug: "grantrebind", title: "Rebind Reborn", html: HTML(31) }));
    const afterRecreate = toolError(
      await callTool("get_page_data", { slug: "grantrebind" }, { token: rebind.token })
    );
    assert.equal(
      afterRecreate.data.code, "slug_not_allowed",
      "delete→recreate must NOT re-arm the stale grant against the new page row"
    );

    const keep = await tokens.mint({
      label: "keep-watch",
      scope: "data_update",
      allowedSlugs: ["grantkeep"],
    });
    toolData(await callTool("deploy_page", { slug: "grantkeep", title: "Keep", html: HTML(32) }));
    const keepBound = toolError(
      await callTool("get_page_data", { slug: "grantkeep" }, { token: keep.token })
    );
    assert.equal(keepBound.data.code, "page_not_data_managed", "grant bound to the live page");
    toolData(await callTool("delete_page", { slug: "grantkeep" }));
    await versions.restorePage({ slug: "grantkeep" }, { actor: "t@elcanotek.com", actorType: "user" });
    const afterRestore = toolError(
      await callTool("get_page_data", { slug: "grantkeep" }, { token: keep.token })
    );
    assert.equal(
      afterRestore.data.code, "page_not_data_managed",
      "delete→restore keeps the same page row, so the bound grant keeps working"
    );
    console.log("✓ grants bind to page identity: lazy first-bind, no re-bind after recreate, restore-safe");

    console.log("\n✓ MCP integration passed");
  } catch (error) {
    failed = true;
    console.error("✗", error.stack || error.message);
  } finally {
    server.close();
    await db.pool.end().catch(() => {});
  }
  process.exit(failed ? 1 : 0);
})();
