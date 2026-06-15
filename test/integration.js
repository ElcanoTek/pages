"use strict";
// Integration check for the Phase 1 core loop: a version row in Postgres →
// signed token → themed render at /raw on the content host. Driven by
// test/run-integration.sh (which provides the DB + seeded version ids via env).
//
//   VID_OMNICOM = id of a themed, live version of page 'omnicom'
//   VID_DOWN    = id of a version whose page is disabled

const http = require("node:http");
const assert = require("node:assert/strict");

const { app } = require("../server.js");
const rawtoken = require("../lib/rawtoken");

const VID_OMNICOM = Number(process.env.VID_OMNICOM);
const VID_DOWN = Number(process.env.VID_DOWN);

function get(pathname, host) {
  return new Promise((resolve) => {
    const req = http.request(
      { host: "127.0.0.1", port: 3097, path: pathname, headers: { Host: host } },
      (r) => {
        let b = "";
        r.on("data", (d) => (b += d));
        r.on("end", () => resolve({ status: r.statusCode, body: b, csp: r.headers["content-security-policy"] || "" }));
      }
    );
    req.end();
  });
}

const CONTENT = "content.localhost";

(async () => {
  const srv = app.listen(3097);
  let failed = false;
  try {
    // 1. Valid token + correct slug → themed render.
    const tok = rawtoken.mint({ pageId: 0, versionId: VID_OMNICOM, purpose: "view", renderMode: "themed" });
    const ok = await get(`/raw/omnicom?t=${encodeURIComponent(tok)}`, CONTENT);
    assert.equal(ok.status, 200, "valid render should be 200");
    assert.match(ok.csp, /sandbox/, "sandbox CSP header present");
    assert.match(ok.body, /design-tokens\.css" data-flag-injected/, "Flag tokens injected");
    assert.match(ok.body, /<canvas id=c><\/canvas><script>chart\(\)<\/script>/, "chart markup preserved verbatim");
    console.log("✓ valid themed render (200, sandboxed, Flag-injected, charts intact)");

    // 2. No token → 403.
    const noTok = await get(`/raw/omnicom`, CONTENT);
    assert.equal(noTok.status, 403, "no token → 403");
    console.log("✓ missing token → 403");

    // 3. Valid token but wrong slug → 404 (token bound to a version, slug must match).
    const wrongSlug = await get(`/raw/notomnicom?t=${encodeURIComponent(tok)}`, CONTENT);
    assert.equal(wrongSlug.status, 404, "wrong slug → 404");
    console.log("✓ token replayed on wrong slug → 404");

    // 4. Disabled page → 404 (takedown switch).
    const downTok = rawtoken.mint({ pageId: 0, versionId: VID_DOWN, purpose: "view", renderMode: "themed" });
    const down = await get(`/raw/down?t=${encodeURIComponent(downTok)}`, CONTENT);
    assert.equal(down.status, 404, "disabled page → 404");
    console.log("✓ disabled page (takedown) → 404");
  } catch (err) {
    failed = true;
    console.error("✗", err.message);
  } finally {
    srv.close();
  }
  process.exit(failed ? 1 : 0);
})();
