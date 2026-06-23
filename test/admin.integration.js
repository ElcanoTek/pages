"use strict";
// Integration check for the admin shell's JSON API (/api/v1/admin/*): the
// cookie+CSRF human surface. We mint a REAL Ed25519 elcano_auth cookie (test
// keypair) so the SSO admin gate + CSRF are genuinely exercised end to end:
// list, approve/reject, publish/rollback, disable/enable, approval toggle,
// preview-token, plus the auth/CSRF rejections. Driven by run-integration.sh.

const crypto = require("node:crypto");

// Test keypair → set the verify pubkey BEFORE requiring auth (it reads at load).
const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
process.env.AUTH_SIGNING_PUBKEY = Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url").toString("base64");
process.env.PAGE_COOKIE_SECRET = process.env.PAGE_COOKIE_SECRET || "admin-test-csrf-secret";

const http = require("node:http");
const assert = require("node:assert/strict");
const { app } = require("../server.js");
const csrf = require("../lib/csrf");
const versions = require("../lib/versions");

const PORT = 3101;
const DASH = "localhost";
const ORIGIN = "http://localhost"; // matches Host: localhost (no port) → csrf Origin check passes
const HTML = (n) => `<!doctype html><html><head><title>Adm</title></head><body><h1>v${n}</h1></body></html>`;

function mintCookie(email, ttl = 3600) {
  const now = Math.floor(Date.now() / 1000);
  const body = Buffer.from(JSON.stringify({ email, tenant: "", iat: now, exp: now + ttl })).toString("base64url");
  const sig = crypto.sign(null, Buffer.from(body), privateKey).toString("base64url");
  return `${body}.${sig}`;
}
const ADMIN = "admin@elcanotek.com";
const adminCookie = `elcano_auth=${mintCookie(ADMIN)}`;
const nonAdminCookie = `elcano_auth=${mintCookie("outsider@example.com")}`;
const csrfTok = csrf.mint(ADMIN);

// HTTP client. opts: { cookie, csrf:true, origin, body, method }
function req(method, path, opts = {}) {
  return new Promise((resolve, reject) => {
    const payload = opts.body !== undefined ? JSON.stringify(opts.body) : null;
    const headers = { Host: DASH };
    if (opts.cookie) headers.Cookie = opts.cookie;
    if (opts.csrf) headers["X-CSRF-Token"] = csrfTok;
    if (opts.origin) headers.Origin = opts.origin;
    if (payload) { headers["Content-Type"] = "application/json"; headers["Content-Length"] = Buffer.byteLength(payload); }
    const r = http.request({ host: "127.0.0.1", port: PORT, method, path, headers }, (res) => {
      let b = ""; res.on("data", (d) => (b += d));
      res.on("end", () => { let j = null; try { j = b ? JSON.parse(b) : null; } catch {} resolve({ status: res.statusCode, json: j, body: b }); });
    });
    r.on("error", reject);
    if (payload) r.write(payload);
    r.end();
  });
}
const adminGet = (p) => req("GET", p, { cookie: adminCookie });
const adminPost = (p, body) => req("POST", p, { cookie: adminCookie, csrf: true, origin: ORIGIN, body });

(async () => {
  const srv = app.listen(PORT);
  let failed = false;
  try {
    const seed = { actor: "seed@agent", actorType: "agent", ip: "127.0.0.1" };

    // ── open page with a live v1 and a draft v2 ──
    await versions.createPage({ slug: "adm", title: "Admin Demo" }, seed);
    const v1 = (await versions.deploy({ slug: "adm", html: HTML(1), publish: true }, seed)).version.id;
    const v2 = (await versions.deploy({ slug: "adm", html: HTML(2) }, seed)).version.id;

    // 1. auth gate.
    assert.equal((await req("GET", "/api/v1/admin/pages/adm")).status, 401, "no cookie → 401");
    assert.equal((await req("GET", "/api/v1/admin/pages/adm", { cookie: nonAdminCookie })).status, 403, "non-admin → 403");
    console.log("✓ admin auth gate (401 / 403)");

    // 2. read: page + versions + pending + themes.
    const g = await adminGet("/api/v1/admin/pages/adm");
    assert.equal(g.status, 200);
    assert.equal(g.json.page.published_version_id, v1, "v1 is live");
    assert.equal(g.json.versions.length, 2, "two versions");
    assert.ok(g.json.themes.some((t) => t.name === "flag"), "flag theme listed");
    console.log("✓ admin read (page + versions + themes)");

    // 3. CSRF gates on a mutation.
    assert.equal((await req("POST", "/api/v1/admin/pages/adm/publish", { cookie: adminCookie, origin: ORIGIN, body: { version_id: v2 } })).status, 403, "no CSRF token → 403");
    assert.equal((await req("POST", "/api/v1/admin/pages/adm/publish", { cookie: adminCookie, csrf: true, body: { version_id: v2 } })).status, 403, "no Origin → 403");
    console.log("✓ CSRF gate (missing token / missing Origin → 403)");

    // 4. publish the draft (v2 goes live), then rollback to v1.
    assert.equal((await adminPost("/api/v1/admin/pages/adm/publish", { version_id: v2, expected_version: v1 })).status, 200);
    assert.equal((await adminGet("/api/v1/admin/pages/adm")).json.page.published_version_id, v2, "v2 live after publish");
    assert.equal((await adminPost("/api/v1/admin/pages/adm/rollback", { version_id: v1, expected_version: v2 })).status, 200);
    assert.equal((await adminGet("/api/v1/admin/pages/adm")).json.page.published_version_id, v1, "rolled back to v1");
    console.log("✓ admin publish + rollback");

    // 5. optimistic concurrency surfaces as 409.
    const stale = await adminPost("/api/v1/admin/pages/adm/rollback", { version_id: v2, expected_version: 999999 });
    assert.equal(stale.status, 409, "stale expected_version → 409");
    console.log("✓ admin 409 on stale expected_version");

    // 6. approval gate + approve/reject queue.
    await versions.createPage({ slug: "admg", title: "Gated", requireApproval: true }, seed);
    const p1 = (await versions.deploy({ slug: "admg", html: HTML(1), publish: true }, seed)).version; // → pending
    const p2 = (await versions.deploy({ slug: "admg", html: HTML(2) }, seed)).version; // → pending
    assert.equal(p1.status, "pending");
    const gq = await adminGet("/api/v1/admin/pages/admg");
    assert.equal(gq.json.pending.length, 2, "two pending in queue");
    assert.equal((await adminPost(`/api/v1/admin/pages/admg/versions/${p1.id}/approve`, {})).status, 200);
    assert.equal((await adminGet("/api/v1/admin/pages/admg")).json.page.published_version_id, p1.id, "approve published p1");
    assert.equal((await adminPost(`/api/v1/admin/pages/admg/versions/${p2.id}/reject`, { note: "no" })).status, 200);
    assert.equal((await adminGet("/api/v1/admin/pages/admg")).json.versions.find((v) => v.id === p2.id).status, "rejected", "p2 rejected");
    console.log("✓ approve (→ live) + reject (→ terminal)");

    // 7. approval toggle + disable/enable.
    assert.equal((await adminPost("/api/v1/admin/pages/adm/approval", { require_approval: true })).json.require_approval, true);
    assert.equal((await adminPost("/api/v1/admin/pages/adm/approval", { require_approval: false })).json.require_approval, false);
    assert.equal((await adminPost("/api/v1/admin/pages/adm/disable", {})).json.disabled, true);
    assert.equal((await adminGet("/api/v1/admin/pages/adm")).json.page.disabled, true, "page disabled");
    assert.equal((await adminPost("/api/v1/admin/pages/adm/enable", {})).json.disabled, false);
    console.log("✓ approval toggle + disable/enable");

    // 8. preview-token → a content-host /raw URL for the chosen version.
    const pv = await adminPost("/api/v1/admin/pages/adm/preview-token", { version_id: v2 });
    assert.equal(pv.status, 200);
    assert.match(pv.json.url, /\/raw\/adm\?t=/, "preview url points at /raw");
    console.log("✓ preview-token mints a /raw URL");

    // 8b. source editor: deploy-source (admin cookie + CSRF). Saves edited HTML
    // as a new draft (source:"admin"), never auto-publishing. Respects dedupe
    // and the approval gate. PLAN §8 — edits stored source, lossless.
    assert.equal(
      (await req("POST", "/api/v1/admin/pages/adm/deploy-source", { cookie: adminCookie, csrf: true, origin: ORIGIN, body: {} })).status,
      400, "empty body → 400 (html required)"
    );
    assert.equal(
      (await req("POST", "/api/v1/admin/pages/adm/deploy-source", { cookie: adminCookie, csrf: true, body: { html: HTML(3) } })).status,
      403, "no Origin → 403 (CSRF gate still applies)"
    );
    const edited = await adminPost("/api/v1/admin/pages/adm/deploy-source", { html: HTML(3), note: "typo fix" });
    assert.equal(edited.status, 200, "deploy-source ok");
    assert.equal(edited.json.version.status, "draft", "saved as draft (not published)");
    assert.equal(edited.json.version.source, "admin", "source tagged admin");
    assert.equal(edited.json.published, false, "not published");
    assert.equal((await adminGet("/api/v1/admin/pages/adm")).json.page.published_version_id, v1, "live pointer unchanged by edit");
    // dedupe: same HTML again → deduped:true, same version id, no new row.
    const dup = await adminPost("/api/v1/admin/pages/adm/deploy-source", { html: HTML(3) });
    assert.equal(dup.json.deduped, true, "identical re-save deduped");
    assert.equal(dup.json.version.id, edited.json.version.id, "dedupe returns the same version id");
    // the saved draft is publishable via the normal publish endpoint.
    assert.equal((await adminPost("/api/v1/admin/pages/adm/publish", { version_id: edited.json.version.id, expected_version: v1 })).status, 200, "edited draft publishable");
    assert.equal((await adminGet("/api/v1/admin/pages/adm")).json.page.published_version_id, edited.json.version.id, "edited version now live");
    // on an approval-gated page the edit lands pending, not draft.
    const gatedEdit = await adminPost("/api/v1/admin/pages/admg/deploy-source", { html: HTML(9) });
    assert.equal(gatedEdit.json.version.status, "pending", "gated page edit → pending");
    assert.equal(gatedEdit.json.gated, true, "gated flag set");
    console.log("✓ source editor (deploy-source: draft + dedupe + publishable + gated)");

    // 9. the /admin/:slug HTML shell itself: admin → 200 bootstrap; anon → login redirect.
    const shell = await req("GET", "/admin/adm", { cookie: adminCookie });
    assert.equal(shell.status, 200);
    assert.match(shell.body, /id="pages-bootstrap"/, "shell injects bootstrap");
    assert.match(shell.body, /\/shell-assets\/admin\.js/, "shell loads admin.js");
    assert.match(shell.body, /shell-assets\/flag\/tokens\/design-tokens\.css/, "shell links Flag tokens");
    const anon = await req("GET", "/admin/adm");
    assert.equal(anon.status, 302, "anon → redirect to auth login");
    console.log("✓ /admin/:slug shell (200 for admin, 302 for anon)");

    console.log("\n✓ admin integration passed");
  } catch (err) {
    failed = true;
    console.error("✗", err.stack || err.message);
  } finally {
    srv.close();
    await require("../lib/db").pool.end().catch(() => {});
  }
  process.exit(failed ? 1 : 0);
})();
