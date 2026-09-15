// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Exercise the real host dispatcher: testing the limiters on a separate Express
// app misses settings lost when the dispatcher invokes the two inner apps.
const mode = process.argv[2] || "default";
if (mode === "direct") process.env.PAGES_TRUST_PROXY = "false";
else if (mode === "custom") process.env.PAGES_TRUST_PROXY = "127.0.0.1/32,::1/128,198.51.100.0/24";
else delete process.env.PAGES_TRUST_PROXY;
process.env.DASHBOARD_HOST = "dashboard.test";
process.env.CONTENT_HOST = "content.test";
process.env.CONTENT_HOST_ALSO = "content-alias.test";
for (const name of ["RL_API_PER_MIN", "RL_MCP_PER_MIN", "RL_CONTENT_PER_MIN", "RL_PASSWORD_TRIES"]) {
  process.env[name] = "2";
}

const assert = require("node:assert/strict");
const http = require("node:http");
const { once } = require("node:events");
const { app } = require("../server");
const db = require("../lib/db");
const tokens = require("../lib/tokens");

async function listen(server) {
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  return server.address().port;
}

function request(port, localAddress, { method = "GET", path, host = "dashboard.test", headers = {}, body } = {}) {
  return new Promise((resolve, reject) => {
    const payload = body === undefined ? "" : JSON.stringify(body);
    const req = http.request({
      host: "127.0.0.1", port, localAddress, method, path,
      headers: { Host: host, ...headers, ...(payload ? { "Content-Type": "application/json", "Content-Length": Buffer.byteLength(payload) } : {}) },
    }, (res) => {
      let text = "";
      res.setEncoding("utf8");
      res.on("data", (chunk) => { text += chunk; });
      res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, text }));
    });
    req.on("error", reject);
    req.end(payload);
  });
}

(async () => {
  const server = http.createServer(app);
  let proxy;
  try {
    const serverPort = await listen(server);
    // Like the supported one-hop proxy, append the actual connecting address.
    // Earlier forwarded values deliberately survive so hop bounding is tested.
    proxy = http.createServer((req, res) => {
      const forwarded = [req.headers["x-forwarded-for"], req.socket.remoteAddress].filter(Boolean).join(", ");
      const upstream = http.request({
        host: "127.0.0.1", port: serverPort, method: req.method, path: req.url,
        headers: { ...req.headers, "x-forwarded-for": forwarded },
      }, (reply) => {
        res.writeHead(reply.statusCode, reply.headers);
        reply.pipe(res);
      });
      upstream.on("error", (err) => res.destroy(err));
      req.pipe(upstream);
    });
    const port = mode === "direct" ? serverPort : await listen(proxy);
    const surfaces = [
      { name: "content", path: "/raw/northwind", host: "content.test", status: 403, html: true },
      { name: "API", path: "/api/v1/pages", status: 401 },
      { name: "MCP", path: "/mcp", method: "POST", status: 401 },
      { name: "password", path: "/northwind-missing", host: "content.test", method: "POST", status: 404, html: true },
    ];
    for (const surface of surfaces) {
      for (const client of ["127.0.0.2", "127.0.0.3"]) {
        for (let i = 0; i < 2; i++) {
          const reply = await request(port, client, surface);
          assert.equal(reply.status, surface.status, `${mode} ${surface.name}: ${client} gets allowance ${i + 1}`);
        }
        const limited = await request(port, client, surface);
        assert.equal(limited.status, 429, `${mode} ${surface.name}: each client's third request is limited`);
        assert.match(limited.headers["content-type"], surface.html ? /text\/html/ : /application\/json/);
        assert.ok(limited.headers["ratelimit"], "existing standard limit headers remain present");
        if (surface.name === "API") assert.equal(JSON.parse(limited.text).code, "rate_limited");
        if (surface.name === "MCP") assert.equal(JSON.parse(limited.text).error.code, -32000);
        const spoofed = await request(port, client, {
          ...surface, headers: { "X-Forwarded-For": "192.0.2.10, 192.0.2.11" },
        });
        assert.equal(spoofed.status, 429, "earlier forwarded values cannot create another allowance");
      }
      console.log(`✓ ${mode}: ${surface.name} counts two real clients independently`);
    }

    // The zone is determined by Host, regardless of proxy metadata. In
    // particular, proxy attribution must not redirect one host into the other.
    const dashboard = await request(port, "127.0.0.4", {
      path: "/raw/northwind", headers: { "X-Forwarded-Host": "content.test" },
    });
    assert.equal(dashboard.status, 404, "dashboard Host stays in the dashboard zone");
    const content = await request(port, "127.0.0.4", {
      path: "/raw/northwind", host: "CONTENT-ALIAS.TEST:443", headers: { "X-Forwarded-Host": "dashboard.test" },
    });
    assert.equal(content.status, 403, "content Host alias and port still route to content");

    const trust = app.get("trust proxy fn");
    for (const address of ["127.0.0.1", "::1", "::ffff:127.0.0.1"]) {
      assert.equal(trust(address, 0), mode !== "direct", `${address} is a configured immediate proxy`);
      assert.equal(trust(address, 1), false, "a second loopback hop is never trusted");
    }
    assert.equal(trust("198.51.100.9", 0), mode === "custom", "an explicit remote CIDR is supported");
    assert.equal(trust("203.0.113.9", 0), false, "an unconfigured peer is not a proxy");

    // The same attribution reaches the domain audit context, not only a custom
    // limiter key generator that would leave mutations attributed to the proxy.
    const { token } = await tokens.mint({ label: `northwind-proxy-${mode}`, scope: "deploy" });
    const created = await request(port, "127.0.0.5", {
      method: "POST", path: "/api/v1/pages",
      headers: { Authorization: `Bearer ${token}` },
      body: { slug: `northwind-proxy-${mode}`, title: "Northwind" },
    });
    assert.equal(created.status, 201, created.text);
    const pageId = JSON.parse(created.text).page.id;
    const audit = await db.query("SELECT host(ip) AS ip FROM audit_log WHERE page_id=$1 ORDER BY id DESC LIMIT 1", [pageId]);
    assert.equal(audit.rows[0].ip, "127.0.0.5", "mutations record the connecting client");
    console.log(`✓ ${mode}: proxy hops, raw Host dispatch and audit attribution`);
  } catch (err) {
    console.error(err);
    process.exitCode = 1;
  } finally {
    await Promise.all([server, proxy].filter((s) => s && s.listening).map((s) => new Promise((resolve) => s.close(resolve))));
    await db.pool.end();
  }
})();
