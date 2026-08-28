// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
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

    // 11. rename + soft-delete + slug reuse.
    const rn = await req("POST", "/api/v1/pages/acme/title", { token, body: { title: "Acme Renamed" } });
    assert.equal(rn.status, 200);
    assert.equal(rn.json.title, "Acme Renamed");
    const del = await req("DELETE", "/api/v1/pages/acme", { token });
    assert.equal(del.status, 200);
    assert.equal(del.json.deleted, true);
    const goneAcme = await req("GET", "/api/v1/pages/acme", { token });
    assert.equal(goneAcme.status, 404, "deleted page → 404");
    const reAcme = await req("POST", "/api/v1/pages", { token, body: { slug: "acme", title: "Acme 2" } });
    assert.equal(reAcme.status, 201, "slug freed by soft-delete → recreate works");
    console.log("✓ rename + soft-delete + slug reuse");

    // 12. getPage now carries routing urls (parity with MCP).
    const withUrls = await req("GET", "/api/v1/pages/acme", { token });
    assert.match(withUrls.json.urls.live, /\/acme$/, "getPage returns urls");
    console.log("✓ getPage returns routing urls");

    // 13. password: agent may SET, but CLEARING is admin-only (403 admin_only).
    const setPw = await req("POST", "/api/v1/pages/acme/password", { token, body: { password: "letmein" } });
    assert.equal(setPw.status, 200);
    assert.equal(setPw.json.has_password, true);
    const clearPw = await req("POST", "/api/v1/pages/acme/password", { token, body: { password: "" } });
    assert.equal(clearPw.status, 403, "agent clearing password → 403");
    assert.equal(clearPw.json.code, "admin_only");
    console.log("✓ password (set agent-ok, clear admin-only)");

    // 14. agents may NOT delete an approval-gated page (403 approval_required).
    const delGated = await req("DELETE", "/api/v1/pages/gated", { token });
    assert.equal(delGated.status, 403, "agent delete on gated page → 403");
    assert.equal(delGated.json.code, "approval_required");
    console.log("✓ gated-page delete forbidden for agents");

    // 15. agents may NOT delete a disabled (taken-down) page — this is what stops
    //     the delete→recreate slug-reuse bypass of an admin takedown.
    await require("../lib/db").query("UPDATE pages SET disabled = true WHERE slug = 'acme'");
    const delDisabled = await req("DELETE", "/api/v1/pages/acme", { token });
    assert.equal(delDisabled.status, 403, "agent delete on disabled page → 403");
    assert.equal(delDisabled.json.code, "disabled_takedown");
    console.log("✓ disabled-page delete forbidden for agents");

    // 16. nested slugs are first-class (PLAN §13): every REST route + /raw
    //     resolves with the raw (unencoded) nested path.
    const nc = await req("POST", "/api/v1/pages", { token, body: { slug: "nested/q2", title: "Nested" } });
    assert.equal(nc.status, 201, "create nested-slug page → 201");
    const nd = await req("POST", "/api/v1/pages/nested/q2/versions", { token, body: { html: PAGE_HTML(7), publish: true } });
    assert.equal(nd.status, 201, "deploy to nested slug → 201");
    assert.equal(nd.json.published, true);
    const nv = nd.json.version.id;
    const ng = await req("GET", "/api/v1/pages/nested/q2", { token });
    assert.equal(ng.status, 200, "GET nested slug resolves without URL-encoding");
    assert.equal(ng.json.page.slug, "nested/q2");
    assert.match(ng.json.urls.admin, /\/admin\/nested\/q2$/, "urls.admin uses the raw nested path");
    assert.match(ng.json.urls.live, /\/nested\/q2$/, "urls.live uses the raw nested path");
    const nEnc = await req("GET", "/api/v1/pages/nested%2Fq2", { token });
    assert.equal(nEnc.status, 200, "URL-encoded slash still resolves too");
    const nl = await req("GET", "/api/v1/pages/nested/q2/versions", { token });
    assert.equal(nl.status, 200, "list versions on nested slug");
    assert.equal(nl.json.versions.length, 1);
    const n1 = await req("GET", `/api/v1/pages/nested/q2/versions/${nv}`, { token });
    assert.equal(n1.status, 200, "get version on nested slug");
    assert.equal(n1.json.version.id, nv);
    const nTok = rawtoken.mint({ pageId: ng.json.page.id, versionId: nv, purpose: "view", renderMode: "themed" });
    const nRaw = await req("GET", `/raw/nested/q2?t=${encodeURIComponent(nTok)}`, { host: CONTENT });
    assert.equal(nRaw.status, 200, "/raw resolves for nested slug");
    assert.match(nRaw.body, /Report 7/, "nested page renders");
    console.log("✓ nested slug: REST routes + /raw resolve unencoded");

    // 17. reserved slugs (route collisions) are rejected at creation.
    for (const bad of ["welcome", "raw/q2", "acme/versions"]) {
      const rr = await req("POST", "/api/v1/pages", { token, body: { slug: bad } });
      assert.equal(rr.status, 400, `reserved slug '${bad}' → 400`);
      assert.equal(rr.json.code, "reserved_slug");
    }
    console.log("✓ reserved slugs rejected at creation");

    // 18. dedupe scoping: same bytes + same mode still dedupes (idempotent),
    //     but a render_mode change is a NEW row with the REQUESTED mode, and
    //     rejected content re-enters as a fresh publishable row (PLAN §5's
    //     "clone into a new draft" — no byte-mutation workaround needed).
    await req("POST", "/api/v1/pages", { token, body: { slug: "dedup", title: "Dedup" } });
    const w1 = await req("POST", "/api/v1/pages/dedup/versions", { token, body: { html: PAGE_HTML(42) } });
    assert.equal(w1.status, 201);
    const w2 = await req("POST", "/api/v1/pages/dedup/versions", { token, body: { html: PAGE_HTML(42) } });
    assert.equal(w2.status, 200, "identical bytes + mode → deduped 200");
    assert.equal(w2.json.deduped, true);
    assert.equal(w2.json.version.id, w1.json.version.id);
    const w3 = await req("POST", "/api/v1/pages/dedup/versions", { token, body: { html: PAGE_HTML(42), render_mode: "raw" } });
    assert.equal(w3.status, 201, "render_mode change on identical bytes → NEW version");
    assert.equal(w3.json.deduped, false);
    assert.equal(w3.json.version.render_mode, "raw", "returned version carries the requested mode");
    assert.notEqual(w3.json.version.id, w1.json.version.id);
    // reject the themed draft (admin-only → direct state-machine call), then
    // identical re-deploy must NOT hit the rejected dead end.
    const adminCtx = { actor: "admin@elcanotek.com", actorType: "user", ip: "127.0.0.1" };
    await require("../lib/versions").reject({ slug: "dedup", versionId: w1.json.version.id }, adminCtx);
    const w4 = await req("POST", "/api/v1/pages/dedup/versions", { token, body: { html: PAGE_HTML(42), publish: true } });
    assert.equal(w4.status, 201, "identical bytes after reject → NEW version, not the rejected row");
    assert.equal(w4.json.deduped, false);
    assert.notEqual(w4.json.version.id, w1.json.version.id);
    assert.equal(w4.json.published, true, "the re-entered content is publishable through the normal flow");
    assert.equal(w4.json.live, true, "REST deploy reports the truthful live flag");
    console.log("✓ dedupe scoped: mode honored, rejected content re-enterable, idempotent otherwise");

    // 19. bounded lock waits (PG_LOCK_TIMEOUT_MS=2000 in run-integration.sh):
    //     a wedged transaction holding one page's row lock makes concurrent
    //     mutations on THAT page fail fast with a clean 503 — while mutations
    //     on other pages keep succeeding (no pool-wide convoy).
    const dbmod = require("../lib/db");
    const wedge = await dbmod.pool.connect();
    try {
      await wedge.query("BEGIN");
      await wedge.query("SELECT id FROM pages WHERE slug = 'dedup' FOR UPDATE");
      const t0 = Date.now();
      const blocked = await req("POST", "/api/v1/pages/dedup/title", { token, body: { title: "Blocked" } });
      const waited = Date.now() - t0;
      assert.equal(blocked.status, 503, "lock wait is bounded → 503, not a hang");
      assert.equal(blocked.json.code, "db_lock_timeout");
      assert.ok(waited >= 1000 && waited < 10000, `failed within the bound (waited ${waited}ms)`);
      const other = await req("POST", "/api/v1/pages/nested/q2/title", { token, body: { title: "Unaffected" } });
      assert.equal(other.status, 200, "mutations on other pages keep succeeding");
    } finally {
      await wedge.query("ROLLBACK").catch(() => {});
      wedge.release();
    }
    console.log("✓ bounded lock waits: wedged page → fast 503, other pages fine");

    // 20. expected_version must hold on EVERY deploy, not only the ones that
    //     move the pointer. docs/API.md and docs/SECURITY.md both state it
    //     unconditionally, and it was enforced only inside movePointer — so the
    //     two paths that do not move the pointer, publish:false and an
    //     approval-gated page, dropped the caller's check silently.
    //
    //     The gated page is the worse half: it is the case where a human is
    //     trusting the review queue. Agent A reads live v1 and computes a
    //     change; a human approves pending v2; agent A's write lands as pending
    //     v3 built from v1 — no 409 — and approving v3 reverts v2.
    const ctx = { actor: "t@elcanotek.com", actorType: "user", ip: "127.0.0.1" };
    const versionsLib = require("../lib/versions");

    await req("POST", "/api/v1/pages", { token, body: { slug: "stalegate", title: "Stale Gate" } });
    const sg1 = await req("POST", "/api/v1/pages/stalegate/versions", {
      token,
      body: { html: "<h1>one</h1>", publish: true },
    });
    assert.equal(sg1.status, 201);
    const staleId = sg1.json.version.id;
    // Another writer moves the pointer.
    await req("POST", "/api/v1/pages/stalegate/versions", {
      token,
      body: { html: "<h1>two</h1>", publish: true },
    });

    // (a) publish:false — REST's default. The caller pins the version it read.
    const draftStale = await req("POST", "/api/v1/pages/stalegate/versions", {
      token,
      body: { html: "<h1>three</h1>", publish: false, expected_version: staleId },
    });
    assert.equal(draftStale.status, 409, "a publish:false deploy must honour expected_version");
    assert.equal(draftStale.json.code, "stale_version");

    // (b) approval-gated — publish is ignored entirely on this path.
    await versionsLib.setApproval({ slug: "stalegate", requireApproval: true }, ctx);
    const gatedStale = await req("POST", "/api/v1/pages/stalegate/versions", {
      token,
      body: { html: "<h1>four</h1>", publish: true, expected_version: staleId },
    });
    assert.equal(gatedStale.status, 409, "a gated deploy must honour expected_version too");
    assert.equal(gatedStale.json.code, "stale_version");
    // REST serializes {error, code} only (lib/api.js) — the structured details
    // are an MCP affordance — but the message still names the recovery.
    assert.match(gatedStale.json.error, /retry once with expected_version/, "and says what to retry with");

    // …and a CURRENT expected_version still works on both paths, so this is a
    // concurrency check and not a blanket refusal.
    const live = await req("GET", "/api/v1/pages/stalegate", { token });
    const fresh = await req("POST", "/api/v1/pages/stalegate/versions", {
      token,
      body: { html: "<h1>five</h1>", expected_version: live.json.page.published_version_id },
    });
    assert.equal(fresh.status, 201, "a current expected_version is accepted on a gated page");
    assert.equal(fresh.json.version.status, "pending");
    await versionsLib.setApproval({ slug: "stalegate", requireApproval: false }, ctx);
    console.log("✓ expected_version holds on publish:false and approval-gated deploys");

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
