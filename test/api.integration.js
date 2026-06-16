"use strict";
// Integration check for the Phase-2 REST slice: bearer auth → version state
// machine → pointer-is-truth → the SAME signed-token /raw render. Driven by
// test/run-integration.sh (which provides Postgres + the secrets in env).
//
// Exercises the whole agent loop end to end:
//   create page → deploy+publish → read back live → render at /raw →
//   deploy a 2nd version → history → rollback → dedupe → optimistic-concurrency
//   409 → auth 401 → approval-gate (agent gets `pending`, publish forbidden).

const http = require("node:http");
const assert = require("node:assert/strict");

const { app } = require("../server.js");
const tokens = require("../lib/tokens");
const rawtoken = require("../lib/rawtoken");

const PORT = 3098;
const DASH = "localhost"; // DASHBOARD_HOST in run-integration.sh
const CONTENT = "content.localhost";

const PAGE_HTML = (n) =>
  `<!doctype html><html><head><title>Acme</title></head><body><h1>Report ${n}</h1>` +
  `<canvas id=c></canvas><script>chart(${n})</script></body></html>`;

// minimal JSON-over-HTTP client with optional bearer token
function req(method, pathname, { host = DASH, token, body } = {}) {
  return new Promise((resolve, reject) => {
    const payload = body !== undefined ? JSON.stringify(body) : null;
    const headers = { Host: host };
    if (token) headers["Authorization"] = `Bearer ${token}`;
    if (payload) {
      headers["Content-Type"] = "application/json";
      headers["Content-Length"] = Buffer.byteLength(payload);
    }
    const r = http.request({ host: "127.0.0.1", port: PORT, method, path: pathname, headers }, (res) => {
      let b = "";
      res.on("data", (d) => (b += d));
      res.on("end", () => {
        let json = null;
        try { json = b ? JSON.parse(b) : null; } catch { /* non-JSON (e.g. /raw html) */ }
        resolve({ status: res.statusCode, json, body: b });
      });
    });
    r.on("error", reject);
    if (payload) r.write(payload);
    r.end();
  });
}

(async () => {
  const srv = app.listen(PORT);
  let failed = false;
  try {
    const { token } = await tokens.mint({ label: "test-agent", scope: "deploy" });

    // 0. No token → 401.
    const noAuth = await req("POST", "/api/v1/pages", { body: { slug: "x" } });
    assert.equal(noAuth.status, 401, "missing bearer → 401");
    console.log("✓ unauthenticated request → 401");

    // 1. Create an open page.
    const create = await req("POST", "/api/v1/pages", { token, body: { slug: "acme", title: "Acme" } });
    assert.equal(create.status, 201, "create page → 201");
    assert.equal(create.json.page.slug, "acme");
    assert.equal(create.json.page.published_version_id, null, "new page has no live version");
    console.log("✓ create page");

    // 2. Deploy + publish in one call (open-page fast path).
    const d1 = await req("POST", "/api/v1/pages/acme/versions", { token, body: { html: PAGE_HTML(1), publish: true } });
    assert.equal(d1.status, 201, "deploy → 201");
    assert.equal(d1.json.published, true, "publish:true on open page → live");
    assert.equal(d1.json.version.status, "approved", "published version is approved");
    const v1 = d1.json.version.id;
    console.log("✓ deploy + publish (v1 live)");

    // 3. Read back: published pointer == v1.
    const g1 = await req("GET", "/api/v1/pages/acme", { token });
    assert.equal(g1.json.page.published_version_id, v1, "pointer is v1");
    assert.equal(g1.json.published.id, v1);
    console.log("✓ pointer-is-truth reflects v1");

    // 3b. Slug is normalized: a mixed-case lookup resolves to the same page.
    const mixed = await req("GET", "/api/v1/pages/ACME", { token });
    assert.equal(mixed.status, 200, "mixed-case slug resolves");
    assert.equal(mixed.json.page.id, g1.json.page.id, "same page regardless of case");
    console.log("✓ slug normalization (case-insensitive lookup)");

    // 4. Full render loop: mint a /raw view token for the live version, fetch it
    //    on the cookieless content host → themed render with charts intact.
    const viewTok = rawtoken.mint({ pageId: g1.json.page.id, versionId: v1, purpose: "view", renderMode: "themed" });
    const raw = await req("GET", `/raw/acme?t=${encodeURIComponent(viewTok)}`, { host: CONTENT });
    assert.equal(raw.status, 200, "/raw render → 200");
    assert.match(raw.body, /design-tokens\.css" data-flag-injected/, "Flag injected");
    assert.match(raw.body, /<canvas id=c><\/canvas><script>chart\(1\)<\/script>/, "charts verbatim");
    console.log("✓ live version renders at /raw (Flag-injected, charts intact)");

    // 5. Deploy a second version and publish → pointer moves to v2.
    const d2 = await req("POST", "/api/v1/pages/acme/versions", { token, body: { html: PAGE_HTML(2), publish: true } });
    assert.equal(d2.status, 201);
    const v2 = d2.json.version.id;
    assert.notEqual(v2, v1, "v2 is a new row");
    const g2 = await req("GET", "/api/v1/pages/acme", { token });
    assert.equal(g2.json.page.published_version_id, v2, "pointer moved to v2");
    console.log("✓ deploy + publish v2 moves the pointer");

    // 6. History: newest first, two versions.
    const hist = await req("GET", "/api/v1/pages/acme/versions", { token });
    assert.equal(hist.json.versions.length, 2, "two versions in history");
    assert.equal(hist.json.versions[0].id, v2, "history newest-first");
    console.log("✓ version history");

    // 7. Rollback to v1 (no status change, pointer only).
    const rb = await req("POST", "/api/v1/pages/acme/rollback", { token, body: { version_id: v1 } });
    assert.equal(rb.status, 200);
    const g3 = await req("GET", "/api/v1/pages/acme", { token });
    assert.equal(g3.json.page.published_version_id, v1, "rolled back to v1");
    console.log("✓ rollback moves the pointer back to v1");

    // 8. Dedupe: re-deploying v1's exact HTML returns the existing row, 200.
    const dup = await req("POST", "/api/v1/pages/acme/versions", { token, body: { html: PAGE_HTML(1) } });
    assert.equal(dup.status, 200, "identical re-deploy → 200 (not 201)");
    assert.equal(dup.json.deduped, true);
    assert.equal(dup.json.version.id, v1, "dedupe returns the same version id");
    console.log("✓ content dedupe (same sha → existing row)");

    // 9. Optimistic concurrency: stale expected_version → 409.
    const stale = await req("POST", "/api/v1/pages/acme/rollback", { token, body: { version_id: v2, expected_version: 999999 } });
    assert.equal(stale.status, 409, "stale expected_version → 409");
    assert.equal(stale.json.code, "stale_version");
    console.log("✓ optimistic-concurrency 409 on stale expected_version");

    // 10. Approval gate: agent deploy+publish yields a PENDING version (not live),
    //     and an explicit publish is forbidden for agents.
    await req("POST", "/api/v1/pages", { token, body: { slug: "gated", title: "Gated", require_approval: true } });
    const gd = await req("POST", "/api/v1/pages/gated/versions", { token, body: { html: PAGE_HTML(9), publish: true } });
    assert.equal(gd.status, 201);
    assert.equal(gd.json.gated, true, "page reports gated");
    assert.equal(gd.json.published, false, "publish ignored on gated page");
    assert.equal(gd.json.version.status, "pending", "agent deploy lands as pending");
    const gp = await req("POST", "/api/v1/pages/gated/publish", { token, body: { version_id: gd.json.version.id } });
    assert.equal(gp.status, 403, "agent publish on gated page → 403");
    assert.equal(gp.json.code, "approval_required");
    const gg = await req("GET", "/api/v1/pages/gated", { token });
    assert.equal(gg.json.page.published_version_id, null, "gated page stays unpublished");
    console.log("✓ approval gate: agent gets pending, publish forbidden");

    console.log("\n✓ API integration passed");
  } catch (err) {
    failed = true;
    console.error("✗", err.stack || err.message);
  } finally {
    srv.close();
    await require("../lib/db").pool.end().catch(() => {});
  }
  process.exit(failed ? 1 : 0);
})();
