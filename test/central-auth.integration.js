// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Integration check for PAGES_AUTH_MODE=central (lib/central-auth.js): the
// application handoff against a stand-in Auth, the local access list that
// Auth's signed events maintain, the session conventions, and sign-out both
// ways. Driven by run-integration.sh (PG + secrets). The stand-in serves
// /token and /jwks.json the way Auth does; /authorize is never fetched by
// Pages, so the test reads the redirect Pages builds and answers it itself.

const crypto = require("node:crypto");
const http = require("node:http");
const assert = require("node:assert/strict");

const PORT = 3109;
const AUTH_PORT = 3110;
const ISSUER = `http://127.0.0.1:${AUTH_PORT}`;
const CLIENT_SECRET = "s".repeat(40);

const signing = crypto.generateKeyPairSync("ed25519");
const rawPublic = Buffer.from(signing.publicKey.export({ format: "jwk" }).x, "base64url");
// A second Auth key that only /jwks.json publishes: proves rotation needs no
// env edit here.
const rotated = crypto.generateKeyPairSync("ed25519");
const rotatedPublic = Buffer.from(rotated.publicKey.export({ format: "jwk" }).x, "base64url");
const kid = (raw) => crypto.createHash("sha256").update(raw).digest().subarray(0, 16).toString("base64url");

Object.assign(process.env, {
  PAGES_AUTH_MODE: "central",
  AUTH_ISSUER_URL: ISSUER,
  AUTH_ALLOW_INSECURE_HTTP: "1",
  PAGES_AUTH_COOKIE_SECURE: "0",
  PAGES_PUBLIC_URL: "http://localhost",
  AUTH_CLIENT_ID: "pages",
  AUTH_CLIENT_SECRET: CLIENT_SECRET,
  AUTH_SIGNING_PUBKEY: rawPublic.toString("base64"),
  PAGE_COOKIE_SECRET: process.env.PAGE_COOKIE_SECRET || "central-test-secret",
  // Above what the steps below use; step 19 then exhausts it on purpose.
  RL_SIGNIN_PER_MIN: "200",
});

const { app } = require("../server.js");
const db = require("../lib/db");
const centralAuth = require("../lib/central-auth");

const DASH = "localhost";
const CONTENT = "content.localhost";

// ── Stand-in Auth ────────────────────────────────────────────────────────
// Code → claims Pages should receive. The test registers a code before
// sending the browser back to /auth/callback.
const codes = new Map();
let tokenStatus = 200;
const fakeAuth = http.createServer((req, res) => {
  if (req.url === "/jwks.json") {
    res.setHeader("Content-Type", "application/json");
    return res.end(
      JSON.stringify({
        keys: [rawPublic, rotatedPublic].map((k) => ({ kty: "OKP", crv: "Ed25519", x: k.toString("base64url"), kid: kid(k) })),
      })
    );
  }
  if (req.url === "/token" && req.method === "POST") {
    let body = "";
    req.on("data", (d) => (body += d));
    req.on("end", () => {
      const form = new URLSearchParams(body);
      const basic = Buffer.from(String(req.headers.authorization || "").replace(/^Basic /, ""), "base64").toString();
      const entry = codes.get(form.get("code"));
      codes.delete(form.get("code"));
      const challenge = entry && crypto.createHash("sha256").update(form.get("code_verifier") || "").digest("base64url");
      if (
        tokenStatus !== 200 ||
        basic !== `pages:${CLIENT_SECRET}` ||
        !entry ||
        challenge !== entry.challenge ||
        form.get("redirect_uri") !== "http://localhost/auth/callback"
      ) {
        res.statusCode = tokenStatus !== 200 ? tokenStatus : 400;
        return res.end(JSON.stringify({ error: "invalid_grant" }));
      }
      res.setHeader("Content-Type", "application/json");
      res.end(JSON.stringify(entry.claims));
    });
    return;
  }
  res.statusCode = 404;
  res.end();
});

function signed(typ, claims, key = signing.privateKey, keyRaw = rawPublic) {
  const header = Buffer.from(JSON.stringify({ alg: "EdDSA", typ, kid: kid(keyRaw) })).toString("base64url");
  const body = Buffer.from(JSON.stringify(claims)).toString("base64url");
  const sig = crypto.sign(null, Buffer.from(`${header}.${body}`), key).toString("base64url");
  return `${header}.${body}.${sig}`;
}

const nowS = () => Math.floor(Date.now() / 1000);
function logoutToken(sub, extra = {}) {
  return signed("logout+jwt", {
    iss: ISSUER, aud: "pages", sub, jti: crypto.randomUUID(), iat: nowS(), exp: nowS() + 120,
    events: { "http://schemas.openid.net/event/backchannel-logout": {} }, ...extra,
  });
}
function accessToken(sub, email, action, version, opts = {}) {
  return signed(
    "access+jwt",
    {
      iss: ISSUER, aud: "pages", sub, email, jti: crypto.randomUUID(), iat: nowS(), exp: nowS() + 120,
      events: { "urn:elcanotek:event:application-access": { action, version } },
    },
    opts.key,
    opts.keyRaw
  );
}

// ── HTTP helpers ─────────────────────────────────────────────────────────
function req(method, path, opts = {}) {
  return new Promise((resolve, reject) => {
    const headers = { Host: opts.host || DASH, ...(opts.headers || {}) };
    if (opts.cookie) headers.Cookie = opts.cookie;
    let payload = null;
    if (opts.form) {
      payload = new URLSearchParams(opts.form).toString();
      headers["Content-Type"] = "application/x-www-form-urlencoded";
      headers["Content-Length"] = Buffer.byteLength(payload);
    }
    const r = http.request({ host: "127.0.0.1", port: PORT, method, path, headers }, (res) => {
      let b = "";
      res.on("data", (d) => (b += d));
      res.on("end", () => resolve({ status: res.statusCode, headers: res.headers, body: b }));
    });
    r.on("error", reject);
    if (payload) r.write(payload);
    r.end();
  });
}

function cookieFrom(res, name) {
  const found = (res.headers["set-cookie"] || []).map((c) => c.split(";")[0]).find((c) => c.startsWith(`${name}=`));
  return found && found !== `${name}=` ? found : null;
}

const backchannel = (form) => req("POST", "/auth/backchannel-logout", { form });

// signIn — the whole browser round trip. Returns the callback response and
// the session cookie (null when sign-in was refused).
async function signIn(sub, email, { next = "/admin", claims = {} } = {}) {
  const start = await req("GET", `/auth/login?next=${encodeURIComponent(next)}`);
  assert.equal(start.status, 303, "login starts with a redirect to Auth");
  const authorize = new URL(start.headers.location);
  const loginCookie = cookieFrom(start, "pages_login");
  assert.ok(loginCookie, "login transaction cookie set");
  const code = crypto.randomBytes(16).toString("hex");
  codes.set(code, {
    challenge: authorize.searchParams.get("code_challenge"),
    claims: { iss: ISSUER, aud: "pages", sub, email, nonce: authorize.searchParams.get("nonce"), exp: nowS() + 300, ...claims },
  });
  const state = authorize.searchParams.get("state");
  const callback = await req("GET", `/auth/callback?code=${code}&state=${encodeURIComponent(state)}&iss=${encodeURIComponent(ISSUER)}`, {
    cookie: loginCookie,
  });
  return { callback, session: cookieFrom(callback, "pages_session"), authorize, loginCookie, state };
}

(async () => {
  await new Promise((r) => fakeAuth.listen(AUTH_PORT, "127.0.0.1", r));
  const srv = app.listen(PORT);
  let failed = false;
  try {
    // 1. Anonymous admin request → local sign-in, not Elcano's auth.
    const anon = await req("GET", "/admin/templates");
    assert.equal(anon.status, 302);
    assert.equal(anon.headers.location, "/auth/login?next=%2Fadmin%2Ftemplates");
    const anonApi = await req("GET", "/api/v1/admin/pages");
    assert.equal(anonApi.status, 401, "admin JSON API without a session is 401");
    console.log("✓ anonymous /admin → /auth/login; admin API → 401");

    // 2. /auth/login builds the exact authorize request.
    const { authorize } = await signIn("sub-nobody", "nobody@northwind.example");
    assert.equal(authorize.origin + authorize.pathname, `${ISSUER}/authorize`);
    const q = authorize.searchParams;
    assert.equal(q.get("response_type"), "code");
    assert.equal(q.get("client_id"), "pages");
    assert.equal(q.get("redirect_uri"), "http://localhost/auth/callback");
    assert.equal(q.get("scope"), "openid email");
    assert.equal(q.get("code_challenge_method"), "S256");
    assert.ok(q.get("state").length >= 40 && q.get("nonce").length >= 40);
    console.log("✓ authorize request: code + S256 PKCE + state + nonce + exact redirect_uri");

    // 3. A valid Auth identity that is not on the access list is refused.
    const denied = await signIn("sub-ana", "ana@northwind.example");
    assert.equal(denied.callback.status, 403);
    assert.match(denied.callback.body, /does not have access to Pages/);
    assert.equal(denied.session, null, "no session for an account without access");
    const authDenied = await req("GET", "/auth/callback?error=access_denied&state=x");
    assert.equal(authDenied.status, 403, "Auth's access_denied shows the same page");
    console.log("✓ identity without access → 403 no-access page, no session");

    // 4. Auth's signed grant event puts the account on the list.
    const grant = await backchannel({ access_token: accessToken("sub-ana", "Ana@Northwind.example", "grant", 1) });
    assert.equal(grant.status, 204);
    const stale = await backchannel({ access_token: accessToken("sub-ana", "ana@northwind.example", "revoke", 1) });
    assert.equal(stale.status, 204, "an already-applied version is acknowledged");
    assert.deepEqual(await centralAuth.store.listAccess(), ["ana@northwind.example"], "same version ignored; email normalized");
    console.log("✓ access grant event applied; same-version replay ignored");

    // 5. Sign-in now works and lands on the requested page.
    const ok = await signIn("sub-ana", "ana@northwind.example", { next: "/admin/templates?x=1" });
    assert.equal(ok.callback.status, 303);
    assert.equal(ok.callback.headers.location, "/admin/templates?x=1");
    assert.ok(ok.session, "session cookie issued");
    const setCookies = ok.callback.headers["set-cookie"].join("\n");
    assert.match(setCookies, /pages_session=[^;]+; Path=\/; HttpOnly; SameSite=Lax/);
    assert.match(setCookies, /pages_login=; Path=\/; HttpOnly; SameSite=Lax; Max-Age=0/, "transaction cookie cleared");
    assert.equal(ok.callback.headers["referrer-policy"], "no-referrer");
    const admin = await req("GET", "/admin", { cookie: ok.session });
    assert.equal(admin.status, 200, "signed-in member reaches /admin");
    const me = JSON.parse((await req("GET", "/api/me", { cookie: ok.session })).body);
    assert.deepEqual(me, { email: "ana@northwind.example", tenant: "", admin: true });
    console.log("✓ granted account signs in, lands on next, is an admin");

    // 6. The session is stored only as a hash, with the convention's limits.
    const token = ok.session.split("=")[1];
    const { rows } = await db.query(`SELECT * FROM central_sessions WHERE email = 'ana@northwind.example'`);
    assert.equal(rows.length, 1);
    assert.equal(rows[0].token_hash, crypto.createHash("sha256").update(token).digest("hex"));
    assert.ok(!JSON.stringify(rows).includes(token), "raw token never stored");
    assert.equal(Number(rows[0].absolute_expires_at) - Number(rows[0].created_at), 24 * 3600);
    assert.equal(Number(rows[0].idle_expires_at) - Number(rows[0].created_at), 12 * 3600);
    console.log("✓ session stored as SHA-256, 24 h absolute / 12 h idle");

    // 7. Already signed in: /auth/login goes straight to next.
    const again = await req("GET", "/auth/login?next=%2Fadmin%2Fportals", { cookie: ok.session });
    assert.equal(again.status, 303);
    assert.equal(again.headers.location, "/admin/portals");
    const dotted = await req("GET", "/auth/login?next=%2Fa%2F..%2F%2Fevil.example%2Fx", { cookie: ok.session });
    assert.equal(dotted.headers.location, "/admin", "normalized next cannot become //evil.example");
    console.log("✓ /auth/login with a live session skips Auth, still refuses off-site next");

    // 8. Callback hardening: state mismatch, replayed transaction, open redirect.
    const s1 = await req("GET", "/auth/login?next=%2Fadmin");
    const wrongState = await req("GET", "/auth/callback?code=abc&state=nope", { cookie: cookieFrom(s1, "pages_login") });
    assert.equal(wrongState.status, 400);
    const noTx = await req("GET", `/auth/callback?code=abc&state=${new URL(s1.headers.location).searchParams.get("state")}`);
    assert.equal(noTx.status, 400, "callback without the transaction cookie is refused");
    const evil = await signIn("sub-ana", "ana@northwind.example", { next: "//evil.example/x" });
    assert.equal(evil.callback.headers.location, "/admin", "protocol-relative next falls back to /admin");
    const evil2 = await signIn("sub-ana", "ana@northwind.example", { next: "/\\evil.example" });
    assert.equal(evil2.callback.headers.location, "/admin", "backslash next falls back to /admin");
    const s2 = await req("GET", "/auth/login?next=%2Fadmin");
    const s2State = new URL(s2.headers.location).searchParams.get("state");
    const otherIss = await req("GET", `/auth/callback?code=abc&state=${s2State}&iss=${encodeURIComponent("https://evil.example")}`, {
      cookie: cookieFrom(s2, "pages_login"),
    });
    assert.equal(otherIss.status, 400, "authorization response from another issuer is refused");
    console.log("✓ wrong state / missing transaction / other issuer → 400; off-site next refused");

    // 9. Token-endpoint answers Pages must not trust.
    const badNonce = await signIn("sub-ana", "ana@northwind.example", { claims: { nonce: "forged" } });
    assert.equal(badNonce.callback.status, 502);
    assert.equal(badNonce.session, null);
    const badAud = await signIn("sub-ana", "ana@northwind.example", { claims: { aud: "explorer" } });
    assert.equal(badAud.session, null, "a code minted for another application is refused");
    const badIss = await signIn("sub-ana", "ana@northwind.example", { claims: { iss: "https://other.example" } });
    assert.equal(badIss.session, null);
    tokenStatus = 500;
    const outage = await signIn("sub-ana", "ana@northwind.example");
    assert.equal(outage.callback.status, 502);
    tokenStatus = 200;
    console.log("✓ nonce/aud/iss mismatch and Auth outage → no session");

    // 10. The content host never serves the sign-in surface.
    const contentLogin = await req("GET", "/auth/login", { host: CONTENT });
    assert.notEqual(contentLogin.status, 303);
    assert.equal(cookieFrom(contentLogin, "pages_login"), null);
    console.log("✓ content host has no /auth routes");

    // 11. Back-channel logout ends every session of the subject, idempotently.
    const second = await signIn("sub-ana", "ana@northwind.example");
    const lt = logoutToken("sub-ana");
    assert.equal((await backchannel({ logout_token: lt })).status, 204);
    assert.equal((await backchannel({ logout_token: lt })).status, 204, "replay acknowledged");
    for (const cookie of [ok.session, second.session]) {
      const r = await req("GET", "/admin", { cookie });
      assert.equal(r.status, 302, "revoked session is signed out");
    }
    console.log("✓ back-channel logout revokes all of the subject's sessions; replay is a no-op");

    // 11b. Logout that lands between /token and the session insert wins: a
    //      sign-in whose exchange started before it is refused.
    const startedBefore = Date.now() - 5000;
    await centralAuth.store.consumeLogoutEvent({ eventId: crypto.randomUUID(), subject: "sub-ana", issuer: ISSUER, issuedAt: nowS() });
    await assert.rejects(
      centralAuth.store.createSession("sub-ana", "ana@northwind.example", { exchangeStartedMs: startedBefore }),
      centralAuth.AuthTransactionError
    );
    console.log("✓ a sign-in racing a back-channel logout does not survive it");

    // 12. Back-channel input validation.
    assert.equal((await backchannel({})).status, 400, "no token");
    assert.equal((await backchannel({ logout_token: logoutToken("sub-x"), extra: "1" })).status, 400, "extra field");
    assert.equal((await backchannel({ logout_token: logoutToken("sub-x"), access_token: "" })).status, 400, "blank second field");
    assert.equal((await backchannel({ logout_token: lt, access_token: "x" })).status, 400, "both tokens");
    assert.equal((await backchannel({ logout_token: "a.b.c" })).status, 400, "garbage");
    const other = crypto.generateKeyPairSync("ed25519");
    const otherRaw = Buffer.from(other.publicKey.export({ format: "jwk" }).x, "base64url");
    assert.equal(
      (await backchannel({ logout_token: signed("logout+jwt", { iss: ISSUER, aud: "pages", sub: "sub-ana", jti: "j1", iat: nowS(), exp: nowS() + 60, events: { "http://schemas.openid.net/event/backchannel-logout": {} } }, other.privateKey, otherRaw) })).status,
      400,
      "unknown signing key"
    );
    assert.equal((await backchannel({ logout_token: logoutToken("sub-ana", { aud: "explorer" }) })).status, 400, "wrong audience");
    assert.equal((await backchannel({ logout_token: logoutToken("sub-ana", { exp: nowS() - 600 }) })).status, 400, "expired");
    assert.equal((await backchannel({ logout_token: logoutToken("sub-ana", { nonce: "n" }) })).status, 400, "nonce present");
    assert.equal(
      (await backchannel({ logout_token: signed("access+jwt", { iss: ISSUER, aud: "pages", sub: "sub-ana", jti: "j2", iat: nowS(), exp: nowS() + 60, events: { "http://schemas.openid.net/event/backchannel-logout": {} } }) })).status,
      400,
      "access+jwt typ is not a logout token"
    );
    console.log("✓ back-channel refuses missing/both/unsigned/foreign/expired/nonce/wrong-typ tokens");

    // 13. A key only Auth's JWKS publishes verifies (rotation is one-sided).
    const rotatedGrant = accessToken("sub-ben", "ben@northwind.example", "grant", 1, { key: rotated.privateKey, keyRaw: rotatedPublic });
    assert.equal((await backchannel({ access_token: rotatedGrant })).status, 204);
    assert.ok((await centralAuth.store.listAccess()).includes("ben@northwind.example"));
    console.log("✓ token signed by a JWKS-only key verifies");

    // 14. Revoke event ends sessions and refuses the next sign-in.
    const third = await signIn("sub-ana", "ana@northwind.example");
    assert.ok(third.session);
    assert.equal((await backchannel({ access_token: accessToken("sub-ana", "ana@northwind.example", "revoke", 2) })).status, 204);
    const live = await db.query(`SELECT 1 FROM central_sessions WHERE email = 'ana@northwind.example' AND revoked_at IS NULL`);
    assert.equal(live.rows.length, 0, "the revoke itself marks the rows (before any request), not only the access entry");
    assert.equal((await req("GET", "/admin", { cookie: third.session })).status, 302, "revoked access ends the session");
    assert.equal((await signIn("sub-ana", "ana@northwind.example")).callback.status, 403, "and refuses a new one");
    console.log("✓ access revoke ends sessions and blocks sign-in");

    // 15. A changed email moves access to the new address.
    assert.equal((await backchannel({ access_token: accessToken("sub-ben", "ben.new@northwind.example", "grant", 2) })).status, 204);
    const access = await centralAuth.store.listAccess();
    assert.ok(access.includes("ben.new@northwind.example") && !access.includes("ben@northwind.example"));
    console.log("✓ email change on a grant disables the old address");

    // 16. Idle and absolute expiry are enforced server-side; touch is throttled.
    await centralAuth.store.grantAccess("cy@northwind.example");
    const cy = await signIn("sub-cy", "cy@northwind.example");
    const cyHash = crypto.createHash("sha256").update(cy.session.split("=")[1]).digest("hex");
    const before = (await db.query(`SELECT last_seen_at FROM central_sessions WHERE token_hash = $1`, [cyHash])).rows[0].last_seen_at;
    await req("GET", "/admin", { cookie: cy.session });
    const after = (await db.query(`SELECT last_seen_at FROM central_sessions WHERE token_hash = $1`, [cyHash])).rows[0].last_seen_at;
    assert.equal(String(after), String(before), "a request inside the touch interval does not write");
    await db.query(`UPDATE central_sessions SET last_seen_at = last_seen_at - 120 WHERE token_hash = $1`, [cyHash]);
    await req("GET", "/admin", { cookie: cy.session });
    const touched = (await db.query(`SELECT last_seen_at, idle_expires_at, absolute_expires_at FROM central_sessions WHERE token_hash = $1`, [cyHash])).rows[0];
    assert.ok(Number(touched.last_seen_at) > Number(before) - 120, "touched after the interval");
    await db.query(`UPDATE central_sessions SET idle_expires_at = $2 WHERE token_hash = $1`, [cyHash, nowS() - 1]);
    assert.equal((await req("GET", "/admin", { cookie: cy.session })).status, 302, "idle-expired session is signed out");
    const cy2 = await signIn("sub-cy", "cy@northwind.example");
    const cy2Hash = crypto.createHash("sha256").update(cy2.session.split("=")[1]).digest("hex");
    await db.query(`UPDATE central_sessions SET absolute_expires_at = $2, idle_expires_at = $2 WHERE token_hash = $1`, [cy2Hash, nowS() - 1]);
    assert.equal((await req("GET", "/admin", { cookie: cy2.session })).status, 302, "absolute-expired session is signed out");
    // Concurrent requests at the touch boundary write once and never move
    // the clock backwards.
    const cy1b = await signIn("sub-cy", "cy@northwind.example");
    const cy1bHash = crypto.createHash("sha256").update(cy1b.session.split("=")[1]).digest("hex");
    await db.query(`UPDATE central_sessions SET last_seen_at = last_seen_at - 120 WHERE token_hash = $1`, [cy1bHash]);
    const burst = await Promise.all(Array.from({ length: 6 }, () => req("GET", "/api/me", { cookie: cy1b.session })));
    assert.ok(burst.every((r) => r.status === 200));
    const seen = Number((await db.query(`SELECT last_seen_at FROM central_sessions WHERE token_hash = $1`, [cy1bHash])).rows[0].last_seen_at);
    assert.ok(seen >= nowS() - 2, "touched to now, not left stale or moved back");
    // Two requests that both read the stale row: hold the row lock so both
    // UPDATEs queue, newer first. The older one must not move the clock back
    // (the SQL re-check, not only the JS pre-check, has to hold).
    const t0 = nowS() + 1000;
    const cyToken = cy1b.session.split("=")[1];
    await db.query(`UPDATE central_sessions SET last_seen_at = $2, absolute_expires_at = $3, idle_expires_at = $3 WHERE token_hash = $1`, [cy1bHash, t0 - 200, t0 + 5000]);
    const holder = await db.pool.connect();
    const waiting = async (n) => {
      for (let i = 0; i < 200; i++) {
        const { rows } = await db.query(`SELECT count(*)::int AS n FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE 'UPDATE central_sessions SET last_seen_at%'`);
        if (rows[0].n >= n) return;
        await new Promise((r) => setTimeout(r, 25));
      }
      throw new Error("touch UPDATEs never queued");
    };
    try {
      await holder.query("BEGIN");
      await holder.query(`SELECT 1 FROM central_sessions WHERE token_hash = $1 FOR UPDATE`, [cy1bHash]);
      const newer = centralAuth.store.identity(cyToken, t0);
      await waiting(1);
      const older = centralAuth.store.identity(cyToken, t0 - 100);
      await waiting(2);
      await holder.query("COMMIT");
      assert.ok((await newer) && (await older));
    } finally {
      holder.release();
    }
    const kept = Number((await db.query(`SELECT last_seen_at FROM central_sessions WHERE token_hash = $1`, [cy1bHash])).rows[0].last_seen_at);
    assert.equal(kept, t0, "an older concurrent touch does not overwrite a newer one");
    console.log("✓ idle/absolute expiry enforced; touch at most once a minute, monotonic under a burst");

    // 17. CLI revoke ends sessions in the same transaction.
    const cy3 = await signIn("sub-cy", "cy@northwind.example");
    assert.equal(await centralAuth.store.revokeAccess("CY@northwind.example"), true);
    assert.equal((await req("GET", "/admin", { cookie: cy3.session })).status, 302);
    console.log("✓ `pages access revoke` ends that email's sessions");

    // 18. Sign-out: local session ended, browser handed to Auth's RP logout;
    //     a cross-site navigation cannot sign anyone out.
    await centralAuth.store.grantAccess("cy@northwind.example");
    const cy4 = await signIn("sub-cy", "cy@northwind.example");
    const crossSite = await req("GET", "/logout", { cookie: cy4.session, headers: { "Sec-Fetch-Site": "cross-site" } });
    assert.equal(crossSite.status, 303);
    assert.equal(crossSite.headers.location, "/admin");
    assert.equal((await req("GET", "/admin", { cookie: cy4.session })).status, 200, "cross-site logout ignored");
    const foreign = await req("POST", "/logout", { cookie: cy4.session, headers: { Origin: "https://evil.example" } });
    assert.equal(foreign.headers.location, "/admin", "foreign Origin ignored");
    const fromContent = await req("GET", "/logout", { host: CONTENT, cookie: cy4.session });
    assert.ok(!String(fromContent.headers.location || "").includes("/logout?client_id"), "content host has no /logout handoff");
    assert.equal((await req("GET", "/admin", { cookie: cy4.session })).status, 200, "session still live");
    const out = await req("POST", "/logout", { cookie: cy4.session, headers: { Origin: "http://localhost" } });
    assert.equal(out.status, 303);
    assert.equal(out.headers.location, `${ISSUER}/logout?client_id=pages`);
    assert.match(out.headers["set-cookie"].join("\n"), /pages_session=; Path=\/; HttpOnly; SameSite=Lax; Max-Age=0/);
    assert.match(out.headers["set-cookie"].join("\n"), /pages_login=; Path=\/; HttpOnly; SameSite=Lax; Max-Age=0/, "pending login cleared too");
    assert.ok(!/Domain=/i.test(out.headers["set-cookie"].join("\n")), "host-only cookies");
    assert.equal((await req("GET", "/admin", { cookie: cy4.session })).status, 302, "session revoked server-side");
    const signedOut = await req("GET", "/signed-out");
    assert.equal(signedOut.status, 200);
    assert.match(signedOut.body, /signed out of Pages/);
    console.log("✓ /logout revokes, clears, and hands off to Auth; cross-site ignored; /signed-out public");

    // 19. The sign-in routes are rate-limited per client, with a page a
    //     person can read. Last, because it spends this client's budget.
    let limited = null;
    for (let i = 0; i < 250 && !limited; i++) {
      const r = await req("GET", "/auth/login");
      if (r.status === 429) limited = r;
    }
    assert.ok(limited, "sign-in limiter engages");
    assert.match(limited.headers["content-type"], /text\/html/);
    assert.equal(limited.headers["retry-after"], "60");
    assert.match(limited.body, /Too many sign-in attempts/);
    assert.equal((await req("GET", "/logout")).status, 429, "/logout shares the limit");
    assert.equal((await req("GET", "/auth/callback?code=c&state=s")).status, 429, "/auth/callback shares the limit");
    console.log("✓ /auth/login, /auth/callback and /logout are rate-limited with an HTML 429");

    console.log("✓ central auth integration passed");
  } catch (err) {
    failed = true;
    console.error("✗", err && (err.stack || err.message));
  } finally {
    srv.close();
    fakeAuth.close();
    await db.pool.end();
    process.exit(failed ? 1 : 0);
  }
})();
