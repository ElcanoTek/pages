"use strict";
// Integration check for the direct-serve client page (PLAN §6b): the content
// host owns the per-page password gate, serves the live page with the sandbox
// CSP, and the dashboard /view brokers Elcano-only pages. Driven by
// run-integration.sh (PG + secrets).

const crypto = require("node:crypto");
// Test keypair for the dashboard /view broker (requireAuth) — set before requiring auth.
const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
process.env.AUTH_SIGNING_PUBKEY = Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url").toString("base64");
process.env.PAGE_COOKIE_SECRET = process.env.PAGE_COOKIE_SECRET || "view-test-secret";

const http = require("node:http");
const assert = require("node:assert/strict");
const { app } = require("../server.js");
const versions = require("../lib/versions");

const PORT = 3102;
const CONTENT = "content.localhost";
const DASH = "localhost";
const HTML = `<!doctype html><html><head><title>Live</title></head><body><h1>Live page</h1><script>chart()</script></body></html>`;

function mintElcano(email) {
  const now = Math.floor(Date.now() / 1000);
  const body = Buffer.from(JSON.stringify({ email, tenant: "", iat: now, exp: now + 3600 })).toString("base64url");
  const sig = crypto.sign(null, Buffer.from(body), privateKey).toString("base64url");
  return `${body}.${sig}`;
}

// HTTP client. opts: { host, cookie, form:{}, redirect:false }
function req(method, path, opts = {}) {
  return new Promise((resolve, reject) => {
    let payload = null;
    const headers = { Host: opts.host || CONTENT };
    if (opts.cookie) headers.Cookie = opts.cookie;
    if (opts.form) {
      payload = new URLSearchParams(opts.form).toString();
      headers["Content-Type"] = "application/x-www-form-urlencoded";
      headers["Content-Length"] = Buffer.byteLength(payload);
    }
    const r = http.request({ host: "127.0.0.1", port: PORT, method, path, headers }, (res) => {
      let b = ""; res.on("data", (d) => (b += d));
      res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, body: b, csp: res.headers["content-security-policy"] || "" }));
    });
    r.on("error", reject);
    if (payload) r.write(payload);
    r.end();
  });
}
// pull the page-session cookie (pgs<id>=...) out of a Set-Cookie header
function sessionCookie(res) {
  const sc = res.headers["set-cookie"] || [];
  const m = sc.map((c) => c.split(";")[0]).find((c) => /^pgs\d+=/.test(c));
  return m || null;
}

(async () => {
  const srv = app.listen(PORT);
  let failed = false;
  try {
    const ctx = { actor: "t@elcanotek.com", actorType: "user", ip: "127.0.0.1" };

    // password-protected page
    await versions.createPage({ slug: "pubpage", title: "Pub" }, ctx);
    await versions.deploy({ slug: "pubpage", html: HTML, publish: true }, ctx);
    await versions.setPassword({ slug: "pubpage", password: "s3cret" }, ctx);
    // Elcano-only page (no password)
    await versions.createPage({ slug: "staffpage", title: "Staff" }, ctx);
    await versions.deploy({ slug: "staffpage", html: HTML, publish: true }, ctx);
    // disabled page
    await versions.createPage({ slug: "downpage", title: "Down" }, ctx);
    await versions.deploy({ slug: "downpage", html: HTML, publish: true }, ctx);
    await versions.setDisabled({ slug: "downpage", disabled: true }, ctx);

    // 1. disabled + unknown → 404.
    assert.equal((await req("GET", "/downpage")).status, 404, "disabled → 404");
    assert.equal((await req("GET", "/no-such-page")).status, 404, "unknown → 404");
    console.log("✓ disabled / unknown → 404");

    // 2. Elcano-only page, no creds → 403 with a staff notice (NOT sandboxed).
    const staff = await req("GET", "/staffpage");
    assert.equal(staff.status, 403);
    assert.match(staff.body, /staff-only/i);
    assert.doesNotMatch(staff.csp, /sandbox/, "gate page is not sandboxed");
    console.log("✓ Elcano-only page → 403 staff notice");

    // 3. password page, no cookie → 401 form (not sandboxed, has a password field).
    const gate = await req("GET", "/pubpage");
    assert.equal(gate.status, 401);
    assert.match(gate.body, /name="password"/, "password form shown");
    assert.doesNotMatch(gate.csp, /sandbox/, "form is not sandboxed (can submit)");
    console.log("✓ password page → 401 form");

    // 4. wrong password → 401 again.
    const wrong = await req("POST", "/pubpage", { form: { password: "nope" } });
    assert.equal(wrong.status, 401);
    assert.match(wrong.body, /Incorrect/);
    console.log("✓ wrong password → 401");

    // 5. correct password → 303 + session cookie; then render with sandbox CSP.
    const ok = await req("POST", "/pubpage", { form: { password: "s3cret" } });
    assert.equal(ok.status, 303, "correct password → 303");
    const cookie = sessionCookie(ok);
    assert.ok(cookie, "session cookie set");
    const live = await req("GET", "/pubpage", { cookie });
    assert.equal(live.status, 200, "authorized render → 200");
    assert.match(live.csp, /sandbox/, "rendered page IS sandboxed");
    assert.match(live.body, /design-tokens\.css" data-flag-injected/, "Flag injected");
    assert.match(live.body, /<script>chart\(\)<\/script>/, "page html served");
    console.log("✓ correct password → cookie → sandboxed live render");

    // 6. dashboard /view broker for the Elcano-only page: SSO → 302 to content host ?t=…
    const elcano = `elcano_auth=${mintElcano("staff@elcanotek.com")}`;
    const brokerNoAuth = await req("GET", "/view/staffpage", { host: DASH });
    assert.equal(brokerNoAuth.status, 302, "no session → redirect to auth login");
    assert.match(brokerNoAuth.headers.location || "", /auth/, "bounces to auth");
    const broker = await req("GET", "/view/staffpage", { host: DASH, cookie: elcano });
    assert.equal(broker.status, 302, "authed broker → 302");
    const loc = broker.headers.location || "";
    assert.match(loc, /\/staffpage\?t=/, "redirects to content host with broker token");
    console.log("✓ dashboard /view broker (302 → content host with token)");

    // 7. follow the broker token on the content host → session cookie → render.
    const t = decodeURIComponent(loc.split("?t=")[1]);
    const exch = await req("GET", `/staffpage?t=${encodeURIComponent(t)}`);
    assert.equal(exch.status, 302, "token exchange → redirect to clean URL");
    const sc2 = sessionCookie(exch);
    assert.ok(sc2, "broker session cookie set");
    const staffLive = await req("GET", "/staffpage", { cookie: sc2 });
    assert.equal(staffLive.status, 200, "staff now sees the page");
    assert.match(staffLive.csp, /sandbox/);
    console.log("✓ broker token → session → staff render");

    console.log("\n✓ view integration passed");
  } catch (err) {
    failed = true;
    console.error("✗", err.stack || err.message);
  } finally {
    srv.close();
    await require("../lib/db").pool.end().catch(() => {});
  }
  process.exit(failed ? 1 : 0);
})();
