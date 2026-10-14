// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Integration check for showing a client password to staff (lib/passwordvault.js):
// the copy is written with the hash, only the admin cookie + CSRF surface reads it,
// every read is audited, a stale or missing copy is reported rather than shown, and
// a password that predates copies is kept the next time a client signs in with it.
// Driven by run-integration.sh (PG + secrets).

const crypto = require("node:crypto");
const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
process.env.AUTH_SIGNING_PUBKEY = Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url").toString("base64");
process.env.PAGE_COOKIE_SECRET = process.env.PAGE_COOKIE_SECRET || "reveal-test-secret";

const http = require("node:http");
const assert = require("node:assert/strict");
const { app } = require("../server.js");
const csrf = require("../lib/csrf");
const db = require("../lib/db");
const versions = require("../lib/versions");
const portals = require("../lib/portals");
const pagecookie = require("../lib/pagecookie");
const passwordvault = require("../lib/passwordvault");
const tokens = require("../lib/tokens");

const PORT = 3107;
const DASH = "localhost";
const CONTENT = "content.localhost";
const ORIGIN = "http://localhost";
const HTML = `<!doctype html><html><head><title>Fabrikam</title></head><body><h1>Fabrikam Q3</h1></body></html>`;

function mintCookie(email) {
  const now = Math.floor(Date.now() / 1000);
  const body = Buffer.from(JSON.stringify({ email, tenant: "", iat: now, exp: now + 3600 })).toString("base64url");
  const sig = crypto.sign(null, Buffer.from(body), privateKey).toString("base64url");
  return `elcano_auth=${body}.${sig}`;
}
const ADMIN = "admin@elcanotek.com";
const adminCookie = mintCookie(ADMIN);
const outsiderCookie = mintCookie("outsider@example.com");

// opts: { host, cookie, csrf, origin, json, form, bearer }
function req(method, path, opts = {}) {
  return new Promise((resolve, reject) => {
    const headers = { Host: opts.host || DASH };
    let payload = null;
    if (opts.cookie) headers.Cookie = opts.cookie;
    if (opts.csrf) headers["X-CSRF-Token"] = csrf.mint(ADMIN);
    if (opts.origin) headers.Origin = opts.origin;
    if (opts.bearer) headers.Authorization = `Bearer ${opts.bearer}`;
    if (opts.form) {
      payload = new URLSearchParams(opts.form).toString();
      headers["Content-Type"] = "application/x-www-form-urlencoded";
    } else if (opts.json !== undefined) {
      payload = JSON.stringify(opts.json);
      headers["Content-Type"] = "application/json";
    }
    if (payload) headers["Content-Length"] = Buffer.byteLength(payload);
    const r = http.request({ host: "127.0.0.1", port: PORT, method, path, headers }, (res) => {
      let b = "";
      res.on("data", (d) => (b += d));
      res.on("end", () => {
        let j = null;
        try { j = b ? JSON.parse(b) : null; } catch {}
        resolve({ status: res.statusCode, headers: res.headers, body: b, json: j });
      });
    });
    r.on("error", reject);
    if (payload) r.write(payload);
    r.end();
  });
}
const adminPost = (path, json = {}) => req("POST", path, { cookie: adminCookie, csrf: true, origin: ORIGIN, json });

async function auditCount(action) {
  const { rows } = await db.query("SELECT count(*)::int AS n FROM audit_log WHERE action = $1", [action]);
  return rows[0].n;
}

(async () => {
  const srv = app.listen(PORT);
  let failed = false;
  try {
    const staff = { actor: ADMIN, actorType: "user", ip: "127.0.0.1" };
    const agent = { actor: "victoria@agent", actorType: "agent", ip: "127.0.0.1" };

    // ── 1. the vault itself ──
    const hash = await pagecookie.hashPassword("contoso-q3-access");
    const sealed = passwordvault.seal("contoso-q3-access", hash);
    assert.equal(passwordvault.open(sealed, hash), "contoso-q3-access", "a copy opens with the hash it was sealed for");
    assert.equal(passwordvault.open(sealed, await pagecookie.hashPassword("contoso-q3-access")), null,
      "the same password under a new hash (fresh salt) does not open an old copy");
    assert.doesNotMatch(sealed, /contoso-q3-access/, "the copy is not the plaintext");
    console.log("✓ vault: bound to the hash it was sealed with");

    // ── 2. a page password set by an agent is showable by staff ──
    await versions.createPage({ slug: "rv-fabrikam", title: "Fabrikam" }, agent);
    await versions.deploy({ slug: "rv-fabrikam", html: HTML, publish: true }, agent);
    await versions.setPassword({ slug: "rv-fabrikam", password: "fabrikam-client-pass" }, agent);

    const before = await auditCount("reveal_password");
    const shown = await adminPost("/api/v1/admin/pages/rv-fabrikam/reveal-password");
    assert.equal(shown.status, 200, shown.body);
    assert.deepEqual(shown.json, { slug: "rv-fabrikam", has_password: true, available: true, password: "fabrikam-client-pass" });
    assert.equal(shown.headers["cache-control"], "no-store", "a credential must not sit in a cache");
    assert.equal(await auditCount("reveal_password"), before + 1, "every reveal is audited");
    const { rows: [auditRow] } = await db.query(
      "SELECT actor, actor_type, metadata FROM audit_log WHERE action = 'reveal_password' ORDER BY id DESC LIMIT 1"
    );
    assert.equal(auditRow.actor, ADMIN, "the audit row names who looked");
    assert.equal(auditRow.actor_type, "user");
    assert.doesNotMatch(JSON.stringify(auditRow.metadata), /fabrikam-client-pass/, "the audit row never carries the password");
    console.log("✓ staff can show an agent-set page password; the reveal is audited");

    // ── 3. who may NOT read it ──
    assert.equal((await req("POST", "/api/v1/admin/pages/rv-fabrikam/reveal-password", { json: {} })).status, 401, "no session");
    assert.equal(
      (await req("POST", "/api/v1/admin/pages/rv-fabrikam/reveal-password", { cookie: outsiderCookie, json: {} })).status,
      403,
      "a signed-in outsider is not staff"
    );
    assert.equal(
      (await req("POST", "/api/v1/admin/pages/rv-fabrikam/reveal-password", { cookie: adminCookie, origin: ORIGIN, json: {} })).status,
      403,
      "CSRF is required"
    );
    assert.equal(
      (await req("GET", "/api/v1/admin/pages/rv-fabrikam/reveal-password", { cookie: adminCookie })).status === 200,
      false,
      "there is no GET form of the reveal"
    );
    const minted = await tokens.mint({ label: "reveal-agent", scope: "deploy" });
    for (const path of ["/api/v1/pages/rv-fabrikam/reveal-password", "/api/v1/admin/pages/rv-fabrikam/reveal-password"]) {
      const viaBearer = await req("POST", path, { bearer: minted.token, json: {} });
      assert.notEqual(viaBearer.status, 200, `an agent token never reads a password (${path})`);
      assert.doesNotMatch(viaBearer.body, /fabrikam-client-pass/);
    }
    await assert.rejects(
      () => versions.revealPassword({ slug: "rv-fabrikam" }, agent),
      (err) => err.status === 403 && err.code === "admin_only",
      "the state machine refuses an agent even without HTTP in front of it"
    );
    console.log("✓ no session, outsiders, missing CSRF and agent tokens cannot read it");

    // ── 4. no other read carries the password or its copy ──
    const detail = await req("GET", "/api/v1/admin/pages/rv-fabrikam", { cookie: adminCookie });
    const agentRead = await req("GET", "/api/v1/pages/rv-fabrikam", { bearer: minted.token });
    for (const [label, res] of [["admin detail", detail], ["agent read", agentRead]]) {
      assert.equal(res.status, 200, `${label}: ${res.body}`);
      assert.doesNotMatch(res.body, /fabrikam-client-pass|password_ciphertext|v1\.[A-Za-z0-9_-]{16}\./, `${label} carries no password`);
    }
    console.log("✓ page reads never include the password or its encrypted copy");

    // ── 5. a password from before copies were kept appears after a client signs in ──
    await db.query("UPDATE pages SET password_ciphertext = NULL WHERE slug = 'rv-fabrikam'");
    const missing = await adminPost("/api/v1/admin/pages/rv-fabrikam/reveal-password");
    assert.equal(missing.json.available, false, "no copy yet is reported, not guessed");
    assert.equal(missing.json.password, null);

    const wrong = await req("POST", "/rv-fabrikam", { host: CONTENT, form: { password: "not-the-password" } });
    assert.equal(wrong.status, 401);
    assert.equal((await adminPost("/api/v1/admin/pages/rv-fabrikam/reveal-password")).json.available, false,
      "a wrong guess never becomes the stored copy");

    const signIn = await req("POST", "/rv-fabrikam", { host: CONTENT, form: { password: "fabrikam-client-pass" } });
    assert.equal(signIn.status, 303, "the client still signs in");
    const recovered = await adminPost("/api/v1/admin/pages/rv-fabrikam/reveal-password");
    assert.equal(recovered.json.available, true);
    assert.equal(recovered.json.password, "fabrikam-client-pass", "the copy is kept from the sign-in that verified");
    console.log("✓ an older password becomes showable after a client signs in with it");

    // ── 6. a copy for a different hash is reported unavailable, never shown ──
    const otherHash = await pagecookie.hashPassword("an-old-password");
    await db.query("UPDATE pages SET password_ciphertext = $1 WHERE slug = 'rv-fabrikam'", [
      passwordvault.seal("an-old-password", otherHash),
    ]);
    const stale = await adminPost("/api/v1/admin/pages/rv-fabrikam/reveal-password");
    assert.equal(stale.json.available, false, "a stale copy is not shown as the current password");
    await req("POST", "/rv-fabrikam", { host: CONTENT, form: { password: "fabrikam-client-pass" } });
    assert.equal((await adminPost("/api/v1/admin/pages/rv-fabrikam/reveal-password")).json.password, "fabrikam-client-pass",
      "the next sign-in replaces a copy that no longer opens");
    console.log("✓ a copy sealed for another hash is unavailable until the next sign-in");

    // ── 7. clearing the password clears the copy ──
    await adminPost("/api/v1/admin/pages/rv-fabrikam/password", { password: "" });
    const cleared = await adminPost("/api/v1/admin/pages/rv-fabrikam/reveal-password");
    assert.deepEqual(cleared.json, { slug: "rv-fabrikam", has_password: false, available: false, password: null });
    const { rows: [afterClear] } = await db.query("SELECT password_ciphertext FROM pages WHERE slug = 'rv-fabrikam'");
    assert.equal(afterClear.password_ciphertext, null, "no copy outlives the password");
    console.log("✓ clearing a password removes its copy");

    // ── 8. portals: create, show, rotate, sign-in capture ──
    const created = await adminPost("/api/v1/admin/portals", { slug: "rv-northwind", name: "Northwind Reveal" });
    assert.equal(created.status, 201, created.body);
    const portalId = created.json.portal.id;
    const portalBefore = await auditCount("reveal_portal_password");
    const portalShown = await adminPost(`/api/v1/admin/portals/${portalId}/reveal-password`);
    assert.equal(portalShown.status, 200, portalShown.body);
    assert.deepEqual(portalShown.json, { id: portalId, available: true, password: created.json.password });
    assert.equal(portalShown.headers["cache-control"], "no-store");
    assert.equal(await auditCount("reveal_portal_password"), portalBefore + 1, "portal reveals are audited");

    const rotated = await adminPost(`/api/v1/admin/portals/${portalId}/password`, {});
    assert.equal((await adminPost(`/api/v1/admin/portals/${portalId}/reveal-password`)).json.password, rotated.json.password,
      "after a rotation the new password is the one shown");

    await db.query("UPDATE page_portals SET password_ciphertext = NULL WHERE id = $1", [portalId]);
    assert.equal((await adminPost(`/api/v1/admin/portals/${portalId}/reveal-password`)).json.available, false);
    const portalSignIn = await req("POST", "/portal/rv-northwind", { host: CONTENT, form: { password: rotated.json.password } });
    assert.equal(portalSignIn.status, 303, "the partner still signs in");
    assert.equal((await adminPost(`/api/v1/admin/portals/${portalId}/reveal-password`)).json.password, rotated.json.password,
      "a portal password is kept from the partner's next sign-in");

    await assert.rejects(
      () => portals.revealPassword({ id: portalId }, agent),
      (err) => err.status === 403 && err.code === "portal_admin_only",
      "an agent never reads a portal password"
    );
    assert.equal((await req("POST", `/api/v1/admin/portals/${portalId}/reveal-password`, { bearer: minted.token, json: {} })).status, 401);
    const portalDetail = await req("GET", `/api/v1/admin/portals/${portalId}`, { cookie: adminCookie });
    assert.doesNotMatch(portalDetail.body, new RegExp(`${rotated.json.password}|password_ciphertext`), "portal reads carry no password");
    console.log("✓ portal passwords: shown to staff, follow rotation, kept on sign-in, never to agents");

    // ── 9. unknown targets ──
    assert.equal((await adminPost("/api/v1/admin/pages/rv-nope/reveal-password")).status, 404);
    assert.equal((await adminPost("/api/v1/admin/portals/999999/reveal-password")).status, 404);
    console.log("✓ unknown page or portal → 404");
  } catch (err) {
    failed = true;
    console.error("✗ password reveal integration failed:", err);
  } finally {
    srv.close();
    await db.pool.end().catch(() => {});
    process.exit(failed ? 1 : 0);
  }
})();
