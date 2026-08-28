// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Integration check for the /api/v1 rate-limit ordering: the limiter must run
// BEFORE the 2 MB body parsers so an unauthenticated client cannot make the
// server parse max-size bodies before being rejected. Driven by
// test/run-integration.sh; sets its own tiny limit before loading the app.

process.env.RL_API_PER_MIN = "3"; // must be set before lib/ratelimit.js loads
process.env.RL_CONTENT_PER_MIN = "2"; // ditto, for the content-host read limiter

const http = require("node:http");
const assert = require("node:assert/strict");

const { app } = require("../server.js");

const PORT = 3097;
const DASH = "localhost";
const CONTENT = "content.localhost";

function req(method, pathname, { rawBody, contentType, host } = {}) {
  return new Promise((resolve, reject) => {
    const headers = { Host: host || DASH };
    if (rawBody !== undefined) {
      headers["Content-Type"] = contentType || "application/json";
      headers["Content-Length"] = Buffer.byteLength(rawBody);
    }
    const r = http.request({ host: "127.0.0.1", port: PORT, method, path: pathname, headers }, (res) => {
      let b = "";
      res.on("data", (d) => (b += d));
      res.on("end", () => {
        let json = null;
        try { json = b ? JSON.parse(b) : null } catch { /* non-JSON */ }
        resolve({ status: res.statusCode, json, body: b, headers: res.headers });
      });
    });
    r.on("error", reject);
    if (rawBody !== undefined) r.write(rawBody);
    r.end();
  });
}

(async () => {
  const srv = app.listen(PORT);
  let failed = false;
  try {
    // 1. Under the limit the body parser is still reached: a malformed JSON
    //    body gets the parser's 400, not a limiter response. (count: 1)
    const malformed = await req("POST", "/api/v1/pages", { rawBody: "{not json" });
    assert.equal(malformed.status, 400, "under-limit malformed JSON → 400 parse error");
    console.log("✓ under-limit malformed body still reaches the parser (400)");

    // 2-3. Two unauthenticated API calls consume the remaining budget.
    for (const i of [2, 3]) {
      const r = await req("GET", "/api/v1/pages");
      assert.equal(r.status, 401, `request ${i} under limit → 401`);
    }
    console.log("✓ requests under the limit pass through to auth (401)");

    // 4. Over the limit: the identical 429 JSON shape as before the reorder.
    const limited = await req("GET", "/api/v1/pages");
    assert.equal(limited.status, 429, "over-limit request → 429");
    assert.deepEqual(
      limited.json,
      { error: "rate limit exceeded — slow down", code: "rate_limited" },
      "429 keeps the documented JSON shape"
    );
    console.log("✓ over-limit request → 429 { error, code: rate_limited }");

    // 5. The limiter now precedes the parser: an over-limit request carrying a
    //    malformed max-size-style body is rejected with the SAME 429 JSON, not
    //    the parser's 400 — proving no parsing work happens pre-limit.
    const limitedBody = await req("POST", "/api/v1/pages", { rawBody: "{not json" });
    assert.equal(limitedBody.status, 429, "over-limit malformed body → 429 (not 400)");
    assert.equal(limitedBody.json && limitedBody.json.code, "rate_limited");
    console.log("✓ over-limit malformed body → 429 before any parsing");

    // 6. The CONTENT host's read limiter answers a person, not an API client.
    //    Every request it rejects is a browser or an iframe loading a client's
    //    dashboard, so a raw JSON error body was the wrong answer — and it
    //    carried none of the content zone's headers either. The /api/v1 shape
    //    above is unchanged; only this one is HTML.
    for (const i of [1, 2]) {
      const r = await req("GET", "/some-client-page", { host: CONTENT });
      assert.ok(r.status !== 429, `content request ${i} is under the limit`);
    }
    const busy = await req("GET", "/some-client-page", { host: CONTENT });
    assert.equal(busy.status, 429, "over-limit content read → 429");
    assert.match(busy.headers["content-type"] || "", /text\/html/, "content 429 is branded HTML, not JSON");
    assert.equal(busy.json, null, "content 429 is not a JSON body");
    assert.match(busy.body, /This page is being loaded too often/, "content 429 explains itself to a viewer");
    assert.doesNotMatch(busy.body, /<script/i, "content 429 chrome stays scriptless");
    assert.ok(busy.headers["content-security-policy"], "content 429 carries a CSP");
    assert.equal(busy.headers["cache-control"], "no-store", "content 429 is not cacheable");
    assert.ok(busy.headers["x-robots-tag"], "content 429 carries the zone header contract");
    console.log("✓ content-host 429 is a branded, hardened page (API 429 stays JSON)");

    console.log("✓ ratelimit ordering integration passed");
  } catch (err) {
    failed = true;
    console.error("✗ ratelimit ordering integration FAILED:", err.message);
  } finally {
    srv.close();
  }
  process.exit(failed ? 1 : 0);
})();
