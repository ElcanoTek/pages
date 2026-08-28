// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Integration check for the direct-serve client page (PLAN §6b): the content
// host owns the per-page password gate, serves the live page with the sandbox
// CSP, and the dashboard /view brokers Elcano-only pages. Driven by
// run-integration.sh (PG + secrets).

const crypto = require("node:crypto");
// Test keypair for the dashboard /view broker (requireAdmin) — set before requiring auth.
const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
process.env.AUTH_SIGNING_PUBKEY = Buffer.from(publicKey.export({ format: "jwk" }).x, "base64url").toString("base64");
process.env.PAGE_COOKIE_SECRET = process.env.PAGE_COOKIE_SECRET || "view-test-secret";

const http = require("node:http");
const assert = require("node:assert/strict");
const { app } = require("../server.js");
const db = require("../lib/db");
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

// HTTP client. opts: { host, cookie, form:{}, headers:{} }
function req(method, path, opts = {}) {
  return new Promise((resolve, reject) => {
    let payload = null;
    const headers = { Host: opts.host || CONTENT };
    if (opts.headers) Object.assign(headers, opts.headers);
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
    const disabled404 = await req("GET", "/downpage");
    const unknown404 = await req("GET", "/no-such-page");
    assert.equal(disabled404.status, 404, "disabled → 404");
    assert.equal(unknown404.status, 404, "unknown → 404");
    assert.match(unknown404.body, /elcano-mark-primary\.svg/, "404 uses approved Elcano branding");
    assert.match(unknown404.body, /Check the link with the person who shared it/, "404 gives corrective guidance");
    assert.match(unknown404.csp, /sandbox/, "content-host 404 retains raw-zone sandboxing");
    console.log("✓ disabled / unknown → 404");

    // 2. Elcano-only page, no creds → 403 with a staff notice (NOT sandboxed).
    const staff = await req("GET", "/staffpage");
    assert.equal(staff.status, 403);
    // The heading names what the READER can do, because the person who lands here
    // is the partner who followed a link shared before the page was passworded.
    assert.match(staff.body, /hasn(&#39;|')t been shared yet/i);
    assert.match(staff.body, /ask them to share it again/i, "…and tells them who to ask");
    // The staff route is still there, as the secondary line it always should have been.
    assert.match(staff.body, /open it from the Pages dashboard/i, "staff gate keeps the correct route");
    assert.doesNotMatch(staff.body, /<script/i, "staff gate remains scriptless");
    assert.doesNotMatch(staff.csp, /sandbox/, "gate page is not sandboxed");
    console.log("✓ Elcano-only page → 403 staff notice");

    // 3. password page, no cookie → 401 form (not sandboxed, has a password field).
    const gate = await req("GET", "/pubpage");
    assert.equal(gate.status, 401);
    assert.match(gate.body, /name="password"/, "password form shown");
    assert.match(gate.body, /<label for="page-password">Page password<\/label>/, "password has an explicit label");
    assert.match(gate.body, /elcano-mark-primary\.svg/, "password gate uses approved Elcano branding");
    assert.doesNotMatch(gate.body, /<script/i, "password gate remains scriptless");
    assert.doesNotMatch(gate.csp, /sandbox/, "form is not sandboxed (can submit)");
    console.log("✓ password page → 401 form");

    // 3b. malformed cookie (decodeURIComponent would throw) → still the normal
    // gate, not a 500. Any cookie name on the domain used to poison the parse.
    const junkCookie = await req("GET", "/pubpage", { cookie: "x=%; pgs1=%2" });
    assert.equal(junkCookie.status, 401, "malformed cookie → password gate, not 500");
    assert.match(junkCookie.body, /name="password"/, "form still shown");
    const junkView = await req("GET", "/view/staffpage", { host: DASH, cookie: "elcano_auth=%" });
    assert.equal(junkView.status, 302, "/view with malformed auth cookie → login redirect, not 500");
    console.log("✓ malformed cookie → gate/redirect (no 500)");

    // 4. wrong password → 401 again.
    const wrong = await req("POST", "/pubpage", { form: { password: "nope" } });
    assert.equal(wrong.status, 401);
    assert.match(wrong.body, /Incorrect/);
    assert.match(wrong.body, /role="alert"/, "incorrect password is announced");
    assert.match(wrong.body, /Check the password and try again/, "incorrect password gives corrective guidance");
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

    // 6b. The broker is the one credential that reads a page without its client
    // password, so it is Elcano-staff-only. A valid SSO session from outside the
    // admin domain is a real principal (the same auth service serves more than
    // Elcano staff) and must be refused here, not brokered.
    const outsider = `elcano_auth=${mintElcano("someone@example.com")}`;
    const brokerOutsider = await req("GET", "/view/staffpage", { host: DASH, cookie: outsider });
    assert.equal(brokerOutsider.status, 403, "non-staff SSO session → 403, not a broker token");
    assert.equal(brokerOutsider.headers.location, undefined, "no redirect, so no broker token reaches the outsider");
    // …and the same on a password-protected page, where brokering would skip the
    // password entirely.
    const brokerOutsiderPw = await req("GET", "/view/pubpage", { host: DASH, cookie: outsider });
    assert.equal(brokerOutsiderPw.status, 403, "non-staff SSO must not bypass the client password");
    console.log("✓ /view broker is staff-only (outsider SSO → 403, no token minted)");

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

    // 7a. An /admin preview token is a RENDER credential, never a page
    // credential. The shell mints purpose:"view" for ANY version — and when
    // nothing is pending it previews the PUBLISHED one, so merely opening
    // /admin/<slug> mints such a token. Redeemed at the direct-serve URL it
    // would become an hour-long session for the whole live page, bypassing the
    // client password. Both shapes are checked, and the PUBLISHED one carries
    // the weight: under a version-only binding the draft case still fails
    // correctly while the published case passes, which is exactly how the
    // first attempt at this fix looked complete while leaving the hole open.
    // Do not collapse this loop.
    const rawtoken = require("../lib/rawtoken");
    const { page: pubPage } = await versions.getPage("pubpage");
    const draft = await versions.deploy(
      { slug: "pubpage", html: `${HTML}<!-- draft -->`, publish: false },
      ctx
    );
    const { page: pubPageAfter } = await versions.getPage("pubpage");
    assert.equal(
      Number(pubPageAfter.published_version_id),
      Number(pubPage.published_version_id),
      "publish:false must not have moved the pointer"
    );
    assert.notEqual(
      Number(draft.version.id),
      Number(pubPageAfter.published_version_id),
      "fixture must be an unpublished draft for this to mean anything"
    );
    // Exactly what lib/adminapi.js preview-token mints, for each version.
    const previewTokenFor = (versionId) =>
      rawtoken.mint({ pageId: pubPage.id, versionId, purpose: "view", renderMode: "themed" }, 300);

    for (const [label, versionId] of [
      ["draft", draft.version.id],
      ["published", pubPageAfter.published_version_id],
    ]) {
      const token = previewTokenFor(versionId);
      const replay = await req("GET", `/pubpage?t=${encodeURIComponent(token)}`);
      assert.equal(replay.status, 401, `${label}-version preview token must not unlock the live page`);
      assert.equal(sessionCookie(replay), null, `${label}-version preview token must not mint a page session`);
      assert.match(replay.body, /name="password"/, "the client password gate still stands");
      // The same token still does its real job on /raw, so this is a binding
      // fix, not a capability removal.
      const preview = await req("GET", `/raw/pubpage?t=${encodeURIComponent(token)}`);
      assert.equal(preview.status, 200, `${label}-version preview token still renders at /raw`);
    }
    assert.match(
      (await req("GET", `/raw/pubpage?t=${encodeURIComponent(previewTokenFor(draft.version.id))}`)).body,
      /<!-- draft -->/,
      "and renders the exact version it was minted for"
    );
    console.log("✓ /admin preview tokens (draft AND published) are not live-page credentials");

    // 7b. The session token's other bindings: it is bound to one page, it
    // expires, and it is not a render credential.
    const sessionFor = (pageId, versionId, ttl = 120) =>
      rawtoken.mint({ pageId, versionId, purpose: "session", renderMode: "themed" }, ttl);
    const { page: staffPage } = await versions.getPage("staffpage");
    // Deliberately hold the VERSION correct and vary only the page id, so the
    // page-id check is the only thing that can reject this. Minting with the
    // other page's version too would be rejected by the version equality alone,
    // and the page-id check could then be deleted with the suite still green.
    const wrongPage = await req(
      "GET",
      `/pubpage?t=${encodeURIComponent(sessionFor(staffPage.id, pubPageAfter.published_version_id))}`
    );
    assert.equal(wrongPage.status, 401, "a session token naming another page must not unlock this one");
    assert.equal(sessionCookie(wrongPage), null, "and must not mint a page session");
    const expired = await req(
      "GET",
      `/pubpage?t=${encodeURIComponent(sessionFor(pubPage.id, pubPageAfter.published_version_id, -1))}`
    );
    assert.equal(expired.status, 401, "an expired session token must not unlock the page");
    assert.equal(sessionCookie(expired), null, "and must not mint a page session");
    const sessionAtRaw = await req(
      "GET",
      `/raw/pubpage?t=${encodeURIComponent(sessionFor(pubPage.id, pubPageAfter.published_version_id))}`
    );
    assert.equal(sessionAtRaw.status, 403, "/raw allow-lists purpose 'view' — a session token is refused");
    console.log("✓ session token is bound to one page, expires, and is not a /raw render credential");

    // 7b. rotating the page password invalidates existing sessions: the old
    // cookie no longer authorizes; the NEW password unlocks; the old one 401s.
    await versions.setPassword({ slug: "pubpage", password: "n3w-secret" }, ctx);
    const staleSession = await req("GET", "/pubpage", { cookie });
    assert.equal(staleSession.status, 401, "old session cookie → gate after rotation");
    assert.match(staleSession.body, /name="password"/, "password form shown again");
    const oldPw = await req("POST", "/pubpage", { form: { password: "s3cret" } });
    assert.equal(oldPw.status, 401, "old password no longer unlocks");
    const newPw = await req("POST", "/pubpage", { form: { password: "n3w-secret" } });
    assert.equal(newPw.status, 303, "new password unlocks");
    const freshCookie = sessionCookie(newPw);
    assert.ok(freshCookie, "fresh session cookie set");
    assert.equal((await req("GET", "/pubpage", { cookie: freshCookie })).status, 200, "fresh session renders");
    console.log("✓ password rotation invalidates old sessions (fresh unlock required)");

    // 8. nested slug: /view broker + token exchange + direct serve all resolve.
    await versions.createPage({ slug: "staff/nest", title: "Nested Staff" }, ctx);
    await versions.deploy({ slug: "staff/nest", html: HTML, publish: true }, ctx);
    const nb = await req("GET", "/view/staff/nest", { host: DASH, cookie: elcano });
    assert.equal(nb.status, 302, "/view resolves for a nested slug");
    const nloc = nb.headers.location || "";
    assert.match(nloc, /\/staff\/nest\?t=/, "broker redirect carries the nested path");
    const nt = decodeURIComponent(nloc.split("?t=")[1]);
    const nexch = await req("GET", `/staff/nest?t=${encodeURIComponent(nt)}`);
    assert.equal(nexch.status, 302, "nested token exchange → redirect");
    const nsc = sessionCookie(nexch);
    assert.ok(nsc, "nested broker session cookie set");
    const nlive = await req("GET", "/staff/nest", { cookie: nsc });
    assert.equal(nlive.status, 200, "nested staff page renders");
    assert.match(nlive.csp, /sandbox/);
    console.log("✓ nested slug: /view broker → session → render");

    // 8b. The content zone's header contract holds on EVERY response shape this
    // host can emit, not just the happy ones. Each of these paths previously
    // answered with some or all of the hardening headers missing: the terminal
    // 404 (a method neither wildcard claims) sent bare text/plain, a thrown
    // handler fell through to Express's default error document, and /assets
    // carried only nosniff. A rule with an unlisted exception is not a rule.
    const CONTENT_HEADERS = ["referrer-policy", "x-content-type-options", "x-robots-tag"];
    const headerCases = [
      { label: "terminal 404 (non-GET/POST method)", res: await req("PUT", "/pubpage"), status: 404, sandbox: true },
      { label: "routed 404 (unknown slug)", res: await req("GET", "/no-such-page-here"), status: 404, sandbox: true },
      { label: "/raw with no token", res: await req("GET", "/raw/pubpage"), status: 403, sandbox: true },
      { label: "/raw-template with no token", res: await req("GET", "/raw-template/1"), status: 403, sandbox: true },
      // Gate pages are deliberately NOT sandboxed — the password form has to be
      // able to submit — but they carry the rest of the contract.
      { label: "password gate", res: await req("GET", "/pubpage"), status: 401, sandbox: false },
      { label: "staff-only gate", res: await req("GET", "/staffpage"), status: 403, sandbox: false },
    ];
    for (const { label, res, status, sandbox } of headerCases) {
      assert.equal(res.status, status, `${label}: expected ${status}`);
      for (const h of CONTENT_HEADERS) {
        assert.ok(res.headers[h], `${label}: missing ${h}`);
      }
      assert.equal(res.headers["cache-control"], "no-store", `${label}: must not be cacheable`);
      assert.ok(res.csp, `${label}: missing Content-Security-Policy`);
      assert.equal(/sandbox/.test(res.csp), sandbox, `${label}: sandbox expectation`);
      assert.match(res.headers["content-type"] || "", /text\/html/, `${label}: branded HTML, not text or JSON`);
    }
    // Shapes no route of ours produces, and which is exactly why they were bare:
    // /healthz, and the 301 express.static emits for a directory (its setHeaders
    // hook runs only on the file path). Both are covered by the zone floor now.
    for (const [label, path_] of [
      ["/healthz", "/healthz"],
      ["express.static directory redirect", "/assets/flag"],
    ]) {
      const r = await req("GET", path_);
      for (const h of CONTENT_HEADERS) assert.ok(r.headers[h], `${label}: missing ${h}`);
      assert.ok(r.headers["content-security-policy"], `${label}: missing CSP`);
      assert.equal(r.headers["x-powered-by"], undefined, `${label}: must not advertise the framework`);
    }

    // Static assets: assert the VALUES, not just presence. A CSP header that
    // merely exists is satisfied by `default-src *; script-src 'unsafe-eval'`,
    // and a presence check would ship that green.
    const asset = await req("GET", "/assets/flag/tokens/design-tokens.css");
    assert.equal(asset.status, 200, "flag tokens still serve");
    for (const h of CONTENT_HEADERS) assert.ok(asset.headers[h], `/assets missing ${h}`);
    assert.match(asset.headers["content-security-policy"] || "", /^default-src 'none'/, "/assets CSP starts closed");
    assert.doesNotMatch(asset.headers["content-security-policy"] || "", /script-src|unsafe-eval|\*/, "/assets CSP grants no script and no wildcard");
    // Caching is deliberately cacheable-but-revalidate, and pinned by VALUE.
    // Two ways to get this wrong, both of which have happened here: leaving it to
    // express.static (which defers to the zone floor's no-store, so every client
    // re-downloads the fonts on every load), or marking it immutable (which pins
    // every client to whatever tokens it first loaded, because sync-flag.sh
    // overwrites these paths in place).
    assert.equal(asset.headers["cache-control"], "public, max-age=0", "/assets must revalidate — not no-store, not immutable");
    console.log("✓ content-zone headers on every response shape (404s, 403s, gates, assets, /healthz, redirects)");

    // A client-error forwarded by express.static must keep its status. Before the
    // error handler preserved it, an unsatisfiable Range came back 500 — which
    // invites a retry loop for a request that can never succeed.
    const badRange = await req("GET", "/assets/flag/tokens/design-tokens.css", {
      headers: { Range: "bytes=99999999-" },
    });
    assert.equal(badRange.status, 416, "an unsatisfiable Range keeps its 416");
    assert.ok(badRange.headers["x-robots-tag"], "and still carries the zone contract");
    const badPrecondition = await req("GET", "/assets/flag/tokens/design-tokens.css", {
      headers: { "If-Match": '"not-the-etag"' },
    });
    assert.equal(badPrecondition.status, 412, "a failed precondition keeps its 412");
    console.log("✓ a forwarded client error keeps its status (416/412, not 500)");

    // 9. Per-page progressive backoff (lib/passwordgate.js, issue #51): failed
    // unlocks share ONE counter per page across all source IPs — fresh
    // addresses (X-Forwarded-For, trusted first hop) don't reset the budget.
    // Only failures are delayed; a success is fast and resets the page, so
    // legitimate viewers are never locked out.
    const passwordgate = require("../lib/passwordgate");
    await versions.createPage({ slug: "brakepage", title: "Brake" }, ctx);
    await versions.deploy({ slug: "brakepage", html: HTML, publish: true }, ctx);
    await versions.setPassword({ slug: "brakepage", password: "br4ke" }, ctx);

    const attempts = [];
    for (const [i, expectedDelay] of [passwordgate.BASE_DELAY_MS, 1000, 2000].entries()) {
      const t0 = Date.now();
      const r = await req("POST", "/brakepage", {
        form: { password: "bad" },
        headers: { "X-Forwarded-For": `10.9.${i}.1` }, // a fresh source IP each time
      });
      const elapsed = Date.now() - t0;
      attempts.push(elapsed);
      assert.equal(r.status, 401, `failure ${i + 1} → 401`);
      assert.ok(
        elapsed >= expectedDelay - 100,
        `failure ${i + 1} from a NEW IP still waited ~${expectedDelay}ms (got ${elapsed}ms) — the brake is per-page, not per-IP`
      );
    }

    // A legitimate success mid-attack is not penalized (no sleep on success)
    // and resets the page's counter.
    const okT0 = Date.now();
    const okBrake = await req("POST", "/brakepage", {
      form: { password: "br4ke" },
      headers: { "X-Forwarded-For": "10.9.9.9" },
    });
    const okElapsed = Date.now() - okT0;
    assert.equal(okBrake.status, 303, "correct password still unlocks");
    assert.ok(
      okElapsed < 3000,
      `success was fast (${okElapsed}ms) despite the escalated counter — viewers are never locked out`
    );
    const t1 = Date.now();
    const afterClear = await req("POST", "/brakepage", {
      form: { password: "bad" },
      headers: { "X-Forwarded-For": "10.9.10.1" },
    });
    const clearElapsed = Date.now() - t1;
    assert.equal(afterClear.status, 401);
    assert.ok(
      clearElapsed < 2000,
      `counter reset after success — next typo waits ~${passwordgate.BASE_DELAY_MS}ms, not the escalated ceiling (got ${clearElapsed}ms)`
    );

    // The reset is a QUIET window, not a fixed window from the first typo.
    // Continuous failures must keep escalating even after window_start ages;
    // conversely, 15 minutes since the most recent failure resets immediately.
    const { rows: brakeRows } = await db.query(
      `SELECT id FROM pages WHERE slug = 'brakepage' AND deleted_at IS NULL`
    );
    const brakeId = brakeRows[0].id;
    await db.query(
      `UPDATE page_password_failures
          SET window_start = now() - interval '1 hour',
              last_fail_at = now()
        WHERE page_id = $1`,
      [brakeId]
    );
    assert.equal(
      await passwordgate.recordFailure(brakeId),
      1000,
      "an old window_start does not reset a counter that has not been quiet"
    );
    await db.query(
      `UPDATE page_password_failures
          SET window_start = now(),
              last_fail_at = now() - interval '16 minutes'
        WHERE page_id = $1`,
      [brakeId]
    );
    assert.equal(
      await passwordgate.recordFailure(brakeId),
      passwordgate.BASE_DELAY_MS,
      "15 minutes since the last failure resets the backoff"
    );
    await passwordgate.clearFailures(brakeId);
    console.log("✓ per-page password backoff: shared across IPs, quiet reset, success reset, no viewer lockout");

    // 9b. The partner portal index: /portal/<slug> on the content host. One
    // credential, then the list of dashboards it opens. This is the surface that
    // always works — it is our own chrome, not a template rendering a block — so
    // it is what makes "send Mandy one link" true.
    const portals = require("../lib/portals");
    const adminCtx = { actor: "admin@elcanotek.com", actorType: "user", ip: "127.0.0.1" };
    const created = await portals.create({ slug: "vw-nwm", name: "Weather Co (view suite)" }, adminCtx);
    const portalId = created.portal.id;
    const portalPassword = created.password;

    // Which portals exist is not public: unknown, and retired, are both 404.
    assert.equal((await req("GET", "/portal/nope")).status, 404, "unknown portal → 404");
    assert.equal((await req("GET", "/portal/vw-nwm/extra")).status, 404, "a portal slug is one segment");
    // `/portal` alone is not a portal URL, and `portal` is a reserved page slug,
    // so it can only ever be a 404 — never a listing of portals.
    assert.equal((await req("GET", "/portal")).status, 404, "there is no portal index of portals");

    const locked = await req("GET", "/portal/vw-nwm");
    assert.equal(locked.status, 401, "no session → the portal password form");
    assert.match(locked.body, /Weather Co \(view suite\)/, "the form names the portal, so the partner knows they are in the right place");
    assert.match(locked.body, /Portal password/);
    assert.doesNotMatch(locked.body, /<script/i, "the portal index is scriptless");
    assert.doesNotMatch(locked.csp, /sandbox/, "NOT sandboxed: its links must behave like ordinary links");
    assert.match(locked.csp, /form-action 'self'/, "…but it may only post back to us");
    assert.equal(locked.headers["cache-control"], "no-store");
    assert.equal(locked.headers["set-cookie"], undefined, "a locked index hands out nothing");

    // Wrong password: 401, no cookie, and the PORTAL's shared counter is charged.
    const wrongPortalTry = await req("POST", "/portal/vw-nwm", { form: { password: "not-it" } });
    assert.equal(wrongPortalTry.status, 401);
    assert.match(wrongPortalTry.body, /Incorrect password/);
    assert.equal(wrongPortalTry.headers["set-cookie"], undefined, "a failed attempt mints nothing");
    const charged = await db.query(`SELECT fail_count FROM portal_password_failures WHERE portal_id = $1`, [portalId]);
    assert.equal(charged.rowCount, 1, "a failed portal attempt charges the portal's own shared counter");
    assert.equal(charged.rows[0].fail_count, 1, "…once, keyed by portal rather than by the URL it arrived at");

    // Right password: a pgp<id> cookie, and the counter resets.
    const opened = await req("POST", "/portal/vw-nwm", { form: { password: portalPassword } });
    assert.equal(opened.status, 303, "303 so the browser GETs the index");
    assert.equal(opened.headers.location, "/portal/vw-nwm");
    const portalCookie = (opened.headers["set-cookie"] || []).map((c) => c.split(";")[0]).find((c) => new RegExp(`^pgp${portalId}=`).test(c));
    assert.ok(portalCookie, "a portal session cookie is set");
    const cookieAttrs = (opened.headers["set-cookie"] || []).find((c) => c.startsWith(`pgp${portalId}=`));
    assert.match(cookieAttrs, /Path=\/;/, "Path=/ — the cookie must ride requests for member pages at any slug");
    assert.match(cookieAttrs, /HttpOnly/);
    assert.match(cookieAttrs, /SameSite=Lax/);
    assert.equal(
      (await db.query(`SELECT 1 FROM portal_password_failures WHERE portal_id = $1`, [portalId])).rowCount,
      0,
      "a successful unlock resets the portal's counter"
    );

    // The unlocked index lists exactly the dashboards that will actually open.
    await versions.createPage({ slug: "vw-overview", title: "Portfolio overview" }, adminCtx);
    await versions.deploy({ slug: "vw-overview", html: HTML, publish: true }, adminCtx);
    await versions.createPage({ slug: "vw-contoso", title: "Contoso Allergex" }, adminCtx);
    await versions.deploy({ slug: "vw-contoso", html: HTML, publish: true }, adminCtx);
    // …and three that must NOT be listed, because each would answer 404: a page
    // with nothing published, one taken down, and one soft-deleted. A dead link in
    // a partner's list is worse than an omission — they cannot tell it from a
    // permissions problem.
    await versions.createPage({ slug: "vw-draft", title: "Never published" }, adminCtx);
    await versions.createPage({ slug: "vw-down", title: "Taken down" }, adminCtx);
    await versions.deploy({ slug: "vw-down", html: HTML, publish: true }, adminCtx);
    await versions.setDisabled({ slug: "vw-down", disabled: true }, adminCtx);
    await versions.createPage({ slug: "vw-gone", title: "Deleted" }, adminCtx);
    await versions.deploy({ slug: "vw-gone", html: HTML, publish: true }, adminCtx);
    for (const slug of ["vw-overview", "vw-contoso", "vw-draft", "vw-down", "vw-gone"]) {
      await portals.addPage({ id: portalId, slug, sortOrder: slug === "vw-contoso" ? 1 : 5 }, adminCtx);
    }
    await versions.deletePage({ slug: "vw-gone" }, adminCtx);
    await portals.setHome({ id: portalId, slug: "vw-overview" }, adminCtx);

    const listing = await req("GET", "/portal/vw-nwm", { cookie: portalCookie });
    assert.equal(listing.status, 200, "a valid portal session lists the dashboards");
    assert.match(listing.body, /2 dashboards available to you/, "only the openable ones are counted");
    assert.match(listing.body, /href="\/vw-overview"/, "root-absolute hrefs: slugs nest, and base-uri is 'none'");
    assert.match(listing.body, /href="\/vw-contoso"/);
    for (const hidden of ["vw-draft", "vw-down", "vw-gone"]) {
      assert.doesNotMatch(listing.body, new RegExp(`href="/${hidden}"`), `${hidden} would 404, so it is not listed`);
    }
    // The home page sorts first even though its sort_order is higher — that is
    // what home_page_id is for.
    assert.ok(
      listing.body.indexOf('href="/vw-overview"') < listing.body.indexOf('href="/vw-contoso"'),
      "the macro view is not buried among the campaign dashboards"
    );
    assert.match(listing.body, /<h2 class="portal-section">Start here<\/h2>/, "and it is set apart, rather than tagged with a word its own title already says");
    // Freshness (#176): the row states when it was last current, with a machine
    // instant beside the phrase. Nothing here has a data envelope, so both rows
    // say "Updated", which is the weaker of the two claims and the correct one.
    assert.match(listing.body, /Updated <\/time>|Updated [^<]+<\/time>/, "each dashboard says when it was last current");
    assert.match(listing.body, /<time datetime="\d{4}-\d{2}-\d{2}T/, "…as a real timestamp, not only prose");
    assert.doesNotMatch(listing.body, /Data as of/, "a page with no data envelope must not claim its data is fresh");
    assert.doesNotMatch(listing.body, /<script/i, "still scriptless once unlocked");
    assert.equal(listing.headers["set-cookie"], undefined, "reading the index re-mints nothing");

    // The seizure scenario, which is the whole reason this route is matched before
    // the page lookup: a page sitting at the portal's exact address must never be
    // served there. `portal` is a reserved slug segment now, so this row can only
    // be built the way a pre-reservation row would have existed — directly.
    await db.query(`INSERT INTO pages (slug, title) VALUES ('portal/vw-nwm', 'Seizure attempt')`);
    await versions.deploy({ slug: "portal/vw-nwm", html: HTML, publish: true }, adminCtx);
    const contested = await req("GET", "/portal/vw-nwm");
    assert.equal(contested.status, 401, "the portal owns this address");
    assert.match(contested.body, /Portal password/, "…and answers with the portal gate, not the page's");
    assert.doesNotMatch(contested.body, /Staff-only/, "the page that claims the address is never served there");

    // Live membership, not a snapshot in the session: removing a dashboard is
    // effective on the partner's very next load, with no re-login.
    await portals.removePage({ id: portalId, slug: "vw-contoso" }, adminCtx);
    const afterRemoval = await req("GET", "/portal/vw-nwm", { cookie: portalCookie });
    assert.doesNotMatch(afterRemoval.body, /href="\/vw-contoso"/, "membership is read per request");
    assert.match(afterRemoval.body, /1 dashboard available to you/);

    // Sign out (#176). Thirty days on a laptop a partner shares with a colleague
    // was unendable short of clearing cookies by hand.
    const overviewId = (await db.query(`SELECT id FROM pages WHERE slug = 'vw-overview'`)).rows[0].id;
    const signedOut = await req("POST", "/portal/vw-nwm/lock", { cookie: portalCookie, form: {} });
    assert.equal(signedOut.status, 303, "303 back to the index, so the reader SEES the form return");
    assert.equal(signedOut.headers.location, "/portal/vw-nwm");
    const cleared = signedOut.headers["set-cookie"] || [];
    const clearedPortal = cleared.find((c) => c.startsWith(`pgp${portalId}=;`));
    assert.ok(clearedPortal, "the portal cookie is cleared");
    assert.match(clearedPortal, /Max-Age=0/);
    // A browser only drops a cookie when Path matches the one it was minted with.
    // Get this wrong and the page says "signed out" while the session survives —
    // the exact failure the button exists to prevent.
    assert.match(clearedPortal, /Path=\/;/, "…on the same Path it was minted with");
    assert.match(clearedPortal, /HttpOnly/);
    assert.match(clearedPortal, /SameSite=Lax/);
    assert.ok(
      cleared.some((c) => c.startsWith(`pgs${overviewId}=;`)),
      "a page the partner unlocked with its own password is cleared too — otherwise a dashboard stays open on the shared machine"
    );
    // Honest about what it is NOT: this clears the browser's copy, it does not
    // revoke the token. Replaying a stolen cookie still works; rotating the
    // password (below) is what revokes.
    assert.equal(
      (await req("GET", "/portal/vw-nwm", { cookie: portalCookie })).status,
      200,
      "signing out clears this device, it does not revoke the token server-side"
    );
    assert.equal((await req("POST", "/portal/nope/lock", { form: {} })).status, 404, "signing out of a portal that does not exist is a 404, like reading it");

    // A portal with nothing openable says so rather than rendering an empty list.
    const empty = await portals.create({ slug: "vw-empty", name: "Nothing Yet" }, adminCtx);
    const emptyCookie = `pgp${empty.portal.id}=${require("../lib/pagecookie").mintPortalSession(
      empty.portal.id, 600, (await db.getPublicPortal("vw-empty")).password_hash
    )}`;
    const emptyIndex = await req("GET", "/portal/vw-empty", { cookie: emptyCookie });
    assert.equal(emptyIndex.status, 200);
    assert.match(emptyIndex.body, /No dashboards are available in this portal yet/);
    assert.match(emptyIndex.body, /Nothing is wrong with your password/, "an empty portal is not a credential failure");

    // A page-session cookie renamed to the portal cookie is refused: the two
    // token types are domain-separated, not merely differently named.
    const pageStyle = `pgp${portalId}=${require("../lib/pagecookie").mintSession(portalId, 600, (await db.getPublicPortal("vw-nwm")).password_hash)}`;
    assert.equal((await req("GET", "/portal/vw-nwm", { cookie: pageStyle })).status, 401, "a pgs body in a pgp cookie is not a portal session");

    // Rotation is revocation: the live cookie stops working immediately.
    await portals.setPassword({ id: portalId, password: "a-new-portal-secret-x" }, adminCtx);
    assert.equal((await req("GET", "/portal/vw-nwm", { cookie: portalCookie })).status, 401, "rotating the password invalidates every live session");
    // Retiring the portal takes the whole address out of service.
    await portals.remove({ id: portalId }, adminCtx);
    // 404 even though a live, published page still sits at this exact slug: the
    // address belongs to the route, retired portal or not.
    assert.equal((await req("GET", "/portal/vw-nwm")).status, 404, "a retired portal is a 404, like any unknown address");
    console.log("✓ partner portal index: one credential, live membership, no dead links, rotation and retirement revoke");

    // 9c. The serve predicate: a portal session opens the portal's member pages.
    // This is the security-critical half of the feature, so each rule gets its
    // own assertion rather than one happy path.
    const sp = await portals.create({ slug: "sp-portal", name: "Serve Predicate Co" }, adminCtx);
    const spId = sp.portal.id;
    const spPassword = sp.password;
    const other = await portals.create({ slug: "sp-other", name: "Some Other Partner" }, adminCtx);

    // Two members: one with a client password of its own, one staff-only (the
    // reclassification an admin performs by adding it to a portal).
    await versions.createPage({ slug: "sp-paid", title: "Has its own password" }, adminCtx);
    await versions.deploy({ slug: "sp-paid", html: HTML, publish: true }, adminCtx);
    await versions.setPassword({ slug: "sp-paid", password: "page-own-secret" }, adminCtx);
    await versions.createPage({ slug: "sp-staff", title: "Staff-only until now" }, adminCtx);
    await versions.deploy({ slug: "sp-staff", html: HTML, publish: true }, adminCtx);
    // …and one page in NO portal, to prove nothing about the old behaviour moved.
    await versions.createPage({ slug: "sp-outside", title: "In no portal" }, adminCtx);
    await versions.deploy({ slug: "sp-outside", html: HTML, publish: true }, adminCtx);
    for (const slug of ["sp-paid", "sp-staff"]) await portals.addPage({ id: spId, slug }, adminCtx);

    const spCookie = `pgp${spId}=${require("../lib/pagecookie").mintPortalSession(
      spId, 600, (await db.getPublicPortal("sp-portal")).password_hash
    )}`;
    const otherCookie = `pgp${other.portal.id}=${require("../lib/pagecookie").mintPortalSession(
      other.portal.id, 600, (await db.getPublicPortal("sp-other")).password_hash
    )}`;

    // A member opens, including the one that has no password of its own — which
    // is the reclassification made visible: it was a 403 staff-only page before
    // a human put it in this portal.
    for (const slug of ["sp-paid", "sp-staff"]) {
      const opened2 = await req("GET", `/${slug}`, { cookie: spCookie });
      assert.equal(opened2.status, 200, `${slug} opens on a portal session`);
      assert.match(opened2.csp, /sandbox/, "the rendered page is still sandboxed agent HTML");
      assert.match(opened2.body, /<script>chart\(\)<\/script>/, "…and it is the real published page");
      // The rule the whole revocation story rests on.
      assert.equal(opened2.headers["set-cookie"], undefined, "a portal-authorised render mints NO cookie, ever");
    }

    // A non-member is not opened by a portal session, and neither is a page in
    // some OTHER portal's set.
    assert.equal((await req("GET", "/sp-outside", { cookie: spCookie })).status, 403, "a portal session opens only its own members");
    assert.equal((await req("GET", "/sp-staff", { cookie: otherCookie })).status, 401, "another portal's session does not open this one's pages");
    assert.equal((await req("GET", "/staffpage", { cookie: spCookie })).status, 403, "a page in no portal is unaffected");

    // A page-session token renamed into the portal cookie is refused here too.
    const forged = `pgp${spId}=${require("../lib/pagecookie").mintSession(spId, 600, (await db.getPublicPortal("sp-portal")).password_hash)}`;
    assert.equal((await req("GET", "/sp-staff", { cookie: forged })).status, 401, "a pgs body in a pgp cookie authorises nothing");

    // Cold, with no session: a member with its own password still gets its form,
    // and a member without one gets a prompt rather than the staff-only notice —
    // which would tell a partner they are not entitled to a page they are. Neither
    // names a portal.
    const coldPaid = await req("GET", "/sp-paid");
    assert.equal(coldPaid.status, 401);
    assert.match(coldPaid.body, /or your portal password/, "a bookmarked partner is told their portal password works here");
    const coldStaff = await req("GET", "/sp-staff");
    assert.equal(coldStaff.status, 401, "a reclassified member prompts instead of refusing");
    assert.match(coldStaff.body, /Portal password/);
    for (const body of [coldPaid.body, coldStaff.body]) {
      assert.doesNotMatch(body, /Serve Predicate Co|sp-portal/, "the gate never names the portal: membership does not leak");
    }
    assert.match((await req("GET", "/staffpage")).body, /hasn(&#39;|')t been shared yet/, "a non-member staff page keeps the plain notice");

    // The member page's own form accepts the PORTAL password and mints a portal
    // session — never a page session, which would outlive the membership.
    const viaPortalPw = await req("POST", "/sp-staff", { form: { password: spPassword } });
    assert.equal(viaPortalPw.status, 303);
    const minted = (viaPortalPw.headers["set-cookie"] || []).map((c) => c.split(";")[0]);
    assert.ok(minted.some((c) => c.startsWith(`pgp${spId}=`)), "a portal password mints a portal session");
    assert.ok(!minted.some((c) => /^pgs\d+=/.test(c)), "…and never a page session");
    assert.equal((await req("GET", "/sp-staff", { cookie: minted[0] })).status, 200, "the minted session opens the page");
    // The page's own password still works on a page that has one.
    const viaPagePw = await req("POST", "/sp-paid", { form: { password: "page-own-secret" } });
    assert.equal(viaPagePw.status, 303);
    assert.ok((viaPagePw.headers["set-cookie"] || []).some((c) => /^pgs\d+=/.test(c)), "a page password still mints a page session");

    // A failed attempt at a MEMBER PAGE's form charges the portal's counter too.
    // Without this, an attacker gets one budget per door against a secret worth
    // the same at all of them.
    await db.query(`DELETE FROM portal_password_failures WHERE portal_id = $1`, [spId]);
    await db.query(`DELETE FROM page_password_failures WHERE page_id = (SELECT id FROM pages WHERE slug='sp-paid')`);
    assert.equal((await req("POST", "/sp-paid", { form: { password: "wrong-everywhere" } })).status, 401);
    const portalCharged = await db.query(`SELECT fail_count FROM portal_password_failures WHERE portal_id = $1`, [spId]);
    assert.equal(portalCharged.rowCount, 1, "the portal counter is charged from a member page's form");
    const pageCharged = await db.query(
      `SELECT fail_count FROM page_password_failures WHERE page_id = (SELECT id FROM pages WHERE slug='sp-paid')`
    );
    assert.equal(pageCharged.rowCount, 1, "and the page's own counter, since both were tested");

    // Takedown and lifecycle beat a portal session — nothing below the 404 rules
    // changed, so a portal credential cannot reach a page they already closed.
    await versions.setDisabled({ slug: "sp-paid", disabled: true }, adminCtx);
    assert.equal((await req("GET", "/sp-paid", { cookie: spCookie })).status, 404, "disabled still 404s, portal session or not");
    await versions.setDisabled({ slug: "sp-paid", disabled: false }, adminCtx);
    await versions.createPage({ slug: "sp-nopub", title: "Nothing published" }, adminCtx);
    await portals.addPage({ id: spId, slug: "sp-nopub" }, adminCtx);
    assert.equal((await req("GET", "/sp-nopub", { cookie: spCookie })).status, 404, "unpublished still 404s");

    // delete → recreate on the same slug is NOT in the portal: membership binds to
    // the page id, so a new page inheriting a freed slug inherits no access.
    await versions.deletePage({ slug: "sp-staff" }, adminCtx);
    assert.equal((await req("GET", "/sp-staff", { cookie: spCookie })).status, 404, "a soft-deleted member 404s");
    await versions.createPage({ slug: "sp-staff", title: "A different page, same slug" }, adminCtx);
    await versions.deploy({ slug: "sp-staff", html: HTML, publish: true }, adminCtx);
    const recreated = await req("GET", "/sp-staff", { cookie: spCookie });
    assert.equal(recreated.status, 403, "the recreated page is a stranger to the portal");
    assert.match(recreated.body, /hasn(&#39;|')t been shared yet/, "…and gets the plain notice, not a portal prompt");

    // Membership removal is effective on the next request — no re-login, no wait.
    assert.equal((await req("GET", "/sp-paid", { cookie: spCookie })).status, 200, "sanity: still a member");
    await portals.removePage({ id: spId, slug: "sp-paid" }, adminCtx);
    assert.equal((await req("GET", "/sp-paid", { cookie: spCookie })).status, 401, "removed from the portal → closed immediately");

    // Rotating the portal password revokes every live portal session for it.
    await portals.addPage({ id: spId, slug: "sp-paid" }, adminCtx);
    assert.equal((await req("GET", "/sp-paid", { cookie: spCookie })).status, 200, "sanity: re-added");
    await portals.setPassword({ id: spId, password: "rotated-portal-secret" }, adminCtx);
    assert.equal((await req("GET", "/sp-paid", { cookie: spCookie })).status, 401, "rotation revokes the session at the member page too");

    // Retiring the portal closes its member pages, not just its index — and this
    // is a DIFFERENT mechanism from rotation, so it needs a session that is
    // otherwise perfectly valid to prove it.
    const rotatedCookie = `pgp${spId}=${require("../lib/pagecookie").mintPortalSession(
      spId, 600, (await db.getPublicPortal("sp-portal")).password_hash
    )}`;
    assert.equal((await req("GET", "/sp-paid", { cookie: rotatedCookie })).status, 200, "sanity: a session on the new credential opens it");
    await portals.remove({ id: spId }, adminCtx);
    assert.equal(
      (await req("GET", "/sp-paid", { cookie: rotatedCookie })).status,
      401,
      "retiring the portal takes its members out of reach, with the same live session"
    );

    // A membership lookup that fails must DENY, not 500: this query can only ever
    // grant access, so falling back to "no portals" is safe by construction, while
    // a throw would take the gate out for every page on the host.
    const realGetPortalsForPage = db.getPortalsForPage;
    db.getPortalsForPage = async () => {
      throw new Error("forced failure: membership lookup");
    };
    try {
      const degraded = await req("GET", "/sp-paid");
      assert.equal(degraded.status, 401, "the page still answers with its own gate, not a 500");
      assert.doesNotMatch(degraded.body, /couldn.t be loaded/, "no error interstitial for a non-member page");
      const stillClosed = await req("GET", "/sp-paid", { cookie: spCookie });
      assert.equal(stillClosed.status, 401, "…and it fails CLOSED: no portal can authorise while membership is unknown");
    } finally {
      db.getPortalsForPage = realGetPortalsForPage;
    }
    console.log("✓ portal serve predicate: members open, strangers do not, no cookie minted, revocation is immediate");

    // 9d. The page switcher payload. Injected into <head> on every themed render
    // that a portal authorised, carrying the CURRENT membership of that portal —
    // which is what lets a partner move between dashboards without going back to
    // the index, and without anything being redeployed when the set changes.
    const navPortal = await portals.create({ slug: "nav-one", name: "Nav Portal One" }, adminCtx);
    const navOther = await portals.create({ slug: "nav-two", name: "Nav Portal Two" }, adminCtx);
    await versions.createPage({ slug: "nav-shared", title: "Co-branded dashboard" }, adminCtx);
    await versions.deploy({ slug: "nav-shared", html: HTML, publish: true }, adminCtx);
    await versions.createPage({ slug: "nav-sibling", title: "Sibling in portal one" }, adminCtx);
    await versions.deploy({ slug: "nav-sibling", html: HTML, publish: true }, adminCtx);
    await versions.createPage({ slug: "nav-secret", title: "Only portal two sees this" }, adminCtx);
    await versions.deploy({ slug: "nav-secret", html: HTML, publish: true }, adminCtx);
    await portals.addPage({ id: navPortal.portal.id, slug: "nav-shared" }, adminCtx);
    await portals.addPage({ id: navPortal.portal.id, slug: "nav-sibling" }, adminCtx);
    await portals.addPage({ id: navOther.portal.id, slug: "nav-shared" }, adminCtx);
    await portals.addPage({ id: navOther.portal.id, slug: "nav-secret" }, adminCtx);

    const mintPortal = (id, hash) => `pgp${id}=${require("../lib/pagecookie").mintPortalSession(id, 600, hash)}`;
    const oneCookie = mintPortal(navPortal.portal.id, (await db.getPublicPortal("nav-one")).password_hash);
    const twoCookie = mintPortal(navOther.portal.id, (await db.getPublicPortal("nav-two")).password_hash);
    const navOf = (body) => {
      const m = body.match(/<script type="application\/json" id="pages-nav"[^>]*>([\s\S]*?)<\/script>/);
      return m ? JSON.parse(m[1]) : null;
    };

    const viaOne = await req("GET", "/nav-shared", { cookie: oneCookie });
    assert.equal(viaOne.status, 200);
    const navOne = navOf(viaOne.body);
    assert.ok(navOne, "a portal-authorised themed render carries the switcher payload");
    assert.equal(navOne.portal.slug, "nav-one");
    assert.deepEqual(navOne.pages.map((p) => p.slug).sort(), ["nav-shared", "nav-sibling"]);
    assert.equal(navOne.pages.find((p) => p.current).slug, "nav-shared", "the page being viewed is marked");
    assert.match(navOne.pages[0].url, /^https?:\/\/[^/]+\//, "hrefs are ready-made and absolute");

    // THE property this whole design turns on: the list is the AUTHORISING
    // portal's, never the union of the portals containing the page. The same page
    // shows two different lists to two partners, and neither learns of the other.
    const viaTwo = await req("GET", "/nav-shared", { cookie: twoCookie });
    const navTwo = navOf(viaTwo.body);
    assert.equal(navTwo.portal.slug, "nav-two");
    assert.deepEqual(navTwo.pages.map((p) => p.slug).sort(), ["nav-secret", "nav-shared"]);
    assert.doesNotMatch(viaOne.body, /nav-secret/, "portal one is never told what portal two contains");
    assert.doesNotMatch(viaTwo.body, /nav-sibling/, "and the reverse");

    // Membership changes reach the partner with nothing redeployed. There is
    // deliberately no bulk re-render primitive; this is why the list is injected
    // per request instead of baked in at deploy time.
    await portals.removePage({ id: navPortal.portal.id, slug: "nav-sibling" }, adminCtx);
    assert.deepEqual(
      navOf((await req("GET", "/nav-shared", { cookie: oneCookie })).body).pages.map((p) => p.slug),
      ["nav-shared"],
      "the switcher reflects membership on the next request, with no redeploy"
    );

    // No portal, no payload: an ordinary page-session render is what it was
    // before this feature existed.
    await versions.createPage({ slug: "nav-plain", title: "In no portal at all" }, adminCtx);
    await versions.deploy({ slug: "nav-plain", html: HTML, publish: true }, adminCtx);
    await versions.setPassword({ slug: "nav-plain", password: "nav-plain-page-pw" }, adminCtx);
    const plainCookie = sessionCookie(await req("POST", "/nav-plain", { form: { password: "nav-plain-page-pw" } }));
    const pageSessionRender = await req("GET", "/nav-plain", { cookie: plainCookie });
    assert.equal(pageSessionRender.status, 200);
    assert.equal(navOf(pageSessionRender.body), null, "a page-session render carries no switcher");

    // …but a partner who holds BOTH a page session and a portal cookie still gets
    // their switcher, rather than falling into the fast path and losing it.
    await versions.setPassword({ slug: "nav-shared", password: "nav-shared-page-pw" }, adminCtx);
    const bothCookies = `${sessionCookie(await req("POST", "/nav-shared", { form: { password: "nav-shared-page-pw" } }))}; ${oneCookie}`;
    const withBoth = await req("GET", "/nav-shared", { cookie: bothCookies });
    assert.equal(withBoth.status, 200);
    assert.ok(navOf(withBoth.body), "holding a page session as well must not cost a partner their switcher");

    // A document that ships its own #pages-nav never gets to keep it: the id is
    // Pages', normalised out at deploy time, so exactly one answer is served.
    const impostor = `<!doctype html><html><head><title>Impostor</title></head><body><script type="application/json" id="pages-nav">{"portal":{"slug":"forged"},"pages":[{"slug":"forged","title":"Forged","url":"https://x/forged"}]}</script></body></html>`;
    await versions.createPage({ slug: "nav-impostor", title: "Ships its own block" }, adminCtx);
    await versions.deploy({ slug: "nav-impostor", html: impostor, publish: true }, adminCtx);
    await portals.addPage({ id: navPortal.portal.id, slug: "nav-impostor" }, adminCtx);
    const served = await req("GET", "/nav-impostor", { cookie: oneCookie });
    assert.equal(served.status, 200);
    assert.doesNotMatch(served.body, /forged/, "the stored copy was stripped at deploy time");
    assert.equal((served.body.match(/id="pages-nav"/g) || []).length, 1, "exactly one #pages-nav is served");
    assert.equal(navOf(served.body).portal.slug, "nav-one", "…and it is ours");

    // The highest-value defensive requirement in the feature: a membership query
    // that fails must not turn a live client dashboard into a 500. The page
    // renders identically, minus the switcher.
    const realGetPortalPages = db.getPortalPages;
    db.getPortalPages = async () => {
      throw new Error("forced failure: switcher membership");
    };
    try {
      const degradedRender = await req("GET", "/nav-shared", { cookie: oneCookie });
      assert.equal(degradedRender.status, 200, "the dashboard still renders");
      assert.match(degradedRender.body, /<script>chart\(\)<\/script>/, "…with its content intact");
      assert.equal(navOf(degradedRender.body), null, "…just without a switcher");
    } finally {
      db.getPortalPages = realGetPortalPages;
    }
    console.log("✓ page switcher: scoped to the authorising portal, live, injected, and unable to break a render");

    // 10. A THROWN handler. Runs last because it stubs the page lookup: with no
    // error handler on this host the rejection fell through to Express's
    // default one, which answers a public client URL with an unbranded error
    // document, none of the zone's headers, and — outside
    // NODE_ENV=production — the stack trace.
    const realGetPublicPage = db.getPublicPage;
    db.getPublicPage = async () => {
      throw new Error("forced failure: pg connection lost");
    };
    try {
      const boom = await req("GET", "/pubpage");
      assert.equal(boom.status, 500, "a thrown handler → 500");
      assert.match(boom.headers["content-type"] || "", /text\/html/, "500 is branded HTML");
      assert.match(boom.csp, /sandbox/, "500 still carries the content-zone sandbox CSP");
      assert.equal(boom.headers["cache-control"], "no-store", "500 is not cacheable");
      assert.ok(boom.headers["x-robots-tag"], "500 carries the zone header contract");
      assert.match(boom.body, /This page couldn(&#39;|')t be loaded/, "500 says whose fault it is");
      assert.doesNotMatch(boom.body, /forced failure|at Object|\.js:\d+/, "500 leaks no internals");
      assert.doesNotMatch(boom.body, /<script/i, "500 chrome stays scriptless");
    } finally {
      db.getPublicPage = realGetPublicPage;
    }
    console.log("✓ a thrown handler → branded, hardened, non-leaking 500");

    console.log("\n✓ view integration passed");
  } catch (err) {
    failed = true;
    console.error("✗", err.stack || err.message);
  } finally {
    srv.close();
    await db.pool.end().catch(() => {});
  }
  process.exit(failed ? 1 : 0);
})();
