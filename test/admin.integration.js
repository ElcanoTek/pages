// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Integration check for the admin shell's JSON API (/api/v1/admin/*): the
// cookie+CSRF human surface. We mint a REAL Ed25519 elcano_auth cookie (test
// keypair) so the SSO admin gate + CSRF are genuinely exercised end to end:
// list, one-version read, approve/reject, publish/rollback, disable/enable, approval toggle,
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
const workspaces = require("../lib/workspaces");
const portals = require("../lib/portals");
const db = require("../lib/db");
const pageSwitcher = require("../public/shell-assets/page-switcher");

const PORT = 3101;
const DASH = "localhost";
const ORIGIN = "http://localhost"; // matches Host: localhost (no port) → csrf Origin check passes
// The content host is a SEPARATE registrable domain (the trust split): template
// previews render only there, never in the dashboard origin.
const CONTENT_HOST = process.env.CONTENT_HOST || "content.localhost";
const CONTENT_ORIGIN = require("../lib/csp").CONTENT_ORIGIN;
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

// HTTP client. opts: { host, cookie, csrf:true, origin, body, method }
function req(method, path, opts = {}) {
  return new Promise((resolve, reject) => {
    const payload = opts.body !== undefined ? JSON.stringify(opts.body) : null;
    const headers = { Host: opts.host || DASH };
    if (opts.cookie) headers.Cookie = opts.cookie;
    if (opts.csrf) headers["X-CSRF-Token"] = csrfTok;
    if (opts.origin) headers.Origin = opts.origin;
    if (payload) { headers["Content-Type"] = "application/json"; headers["Content-Length"] = Buffer.byteLength(payload); }
    const r = http.request({ host: "127.0.0.1", port: PORT, method, path, headers }, (res) => {
      let b = ""; res.on("data", (d) => (b += d));
      res.on("end", () => { let j = null; try { j = b ? JSON.parse(b) : null; } catch {} resolve({ status: res.statusCode, headers: res.headers, json: j, body: b }); });
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

    // 1b. malformed cookie values (bare '%' throws in decodeURIComponent) must
    // get the normal unauthenticated behavior, never a 500 — for ANY cookie
    // name on the host, and for the auth cookie itself.
    assert.equal((await req("GET", "/api/v1/admin/pages/adm", { cookie: "x=%" })).status, 401, "junk cookie name → 401, not 500");
    assert.equal((await req("GET", "/api/v1/admin/pages/adm", { cookie: "elcano_auth=%" })).status, 401, "malformed auth cookie → 401, not 500");
    assert.equal((await req("GET", "/admin/adm", { cookie: "elcano_auth=%2" })).status, 302, "shell with malformed cookie → login redirect, not 500");
    console.log("✓ malformed cookie → unauthenticated behavior (no 500)");

    // 2. read: page + versions + pending + themes.
    const g = await adminGet("/api/v1/admin/pages/adm");
    assert.equal(g.status, 200);
    assert.equal(g.json.page.published_version_id, v1, "v1 is live");
    assert.equal(g.json.versions.length, 2, "two versions");
    assert.ok(g.json.themes.some((t) => t.name === "flag"), "flag theme listed");
    assert.equal(Object.hasOwn(g.json, "automatic_refresh"), false, "retired Pages-owned schedules are not exposed");
    console.log("✓ admin read (page + versions + themes)");

    // 3. CSRF gates on a mutation.
    assert.equal((await req("POST", "/api/v1/admin/pages/adm/publish", { cookie: adminCookie, origin: ORIGIN, body: { version_id: v2 } })).status, 403, "no CSRF token → 403");
    assert.equal((await req("POST", "/api/v1/admin/pages/adm/publish", { cookie: adminCookie, csrf: true, body: { version_id: v2 } })).status, 403, "no Origin → 403");
    console.log("✓ CSRF gate (missing token / missing Origin → 403)");

    // 3b. Recurring update execution is caller-owned. The retired browser route
    // cannot queue work; prompt preparation lives only on the generic MCP surface.
    assert.equal(
      (await adminPost("/api/v1/admin/pages/adm/refresh/run", {})).status,
      401,
      "without an admin route the request falls through to the bearer API and cannot dispatch"
    );
    console.log("✓ retired admin refresh route cannot dispatch work");

    // 3c. Persistent workspace CRUD + assignment. Existing rows migrate as
    // Ungrouped, every mutation remains admin+CSRF-only, case-insensitive names
    // are unique, and removing a workspace detaches rather than deletes pages.
    const initialIndex = await adminGet("/api/v1/admin/pages");
    assert.deepEqual(initialIndex.json.workspaces, [], "new installation starts with no workspaces");
    const initialAdminPage = initialIndex.json.pages.find((p) => p.slug === "adm");
    assert.equal(initialAdminPage.workspace_id, null, "existing page is Ungrouped");
    assert.equal(initialAdminPage.version_count, 2, "admin index gets this page's version count");
    assert.equal(initialAdminPage.published_version_number, 1, "admin index labels the live pointer with a page-local ordinal");
    assert.equal((await req("GET", "/api/v1/admin/workspaces")).status, 401, "workspace list is admin-only");
    assert.equal(
      (await req("POST", "/api/v1/admin/workspaces", { cookie: adminCookie, origin: ORIGIN, body: { name: "No CSRF" } })).status,
      403,
      "workspace creation requires CSRF"
    );
    assert.equal((await adminPost("/api/v1/admin/workspaces", { name: "  " })).status, 400, "blank name rejected");
    assert.equal((await adminPost("/api/v1/admin/workspaces", { name: "x".repeat(101) })).status, 400, "long name rejected");

    const alphaCreated = await adminPost("/api/v1/admin/workspaces", { name: "Alpha" });
    assert.equal(alphaCreated.status, 201);
    const alpha = alphaCreated.json.workspace;
    assert.equal((await adminPost("/api/v1/admin/workspaces", { name: "aLPHa" })).status, 409, "case-only duplicate rejected");
    const beta = (await adminPost("/api/v1/admin/workspaces", { name: "Beta" })).json.workspace;
    assert.equal((await adminPost("/api/v1/admin/pages/adm/workspace", {})).status, 400, "assignment requires an explicit destination");
    assert.equal((await adminPost("/api/v1/admin/pages/adm/workspace", { workspace_id: 999999 })).status, 404, "unknown workspace rejected");

    const assigned = await adminPost("/api/v1/admin/pages/adm/workspace", { workspace_id: alpha.id });
    assert.equal(assigned.status, 200);
    assert.equal(assigned.json.page.workspace_name, "Alpha");
    let detail = await adminGet("/api/v1/admin/pages/adm");
    assert.equal(detail.json.page.workspace_id, alpha.id, "membership persisted on page detail");
    assert.equal(detail.json.page.workspace_name, "Alpha");
    let groupedIndex = await adminGet("/api/v1/admin/pages");
    assert.equal(groupedIndex.json.pages.find((p) => p.slug === "adm").workspace_name, "Alpha", "membership returned by index API");
    assert.equal(groupedIndex.json.workspaces.find((w) => w.id === alpha.id).page_count, 1, "workspace count returned");

    const renamed = await adminPost(`/api/v1/admin/workspaces/${alpha.id}/rename`, { name: "Client Alpha" });
    assert.equal(renamed.json.workspace.name, "Client Alpha");
    assert.equal((await adminGet("/api/v1/admin/pages/adm")).json.page.workspace_name, "Client Alpha", "renamed workspace joins through page reads");
    const groupedPage = await adminPost("/api/v1/admin/pages", {
      slug: "admgrouped",
      title: "Grouped from creation",
      workspace_id: beta.id,
    });
    assert.equal(groupedPage.status, 201);
    assert.equal(groupedPage.json.page.workspace_id, beta.id, "admin can create directly in a workspace");
    const agentGrouped = await versions.createPage(
      { slug: "agent-grouped", title: "Agent grouped", workspaceId: alpha.id },
      seed
    );
    assert.equal(agentGrouped.workspace_id, alpha.id, "agents may choose reversible workspace organization metadata");
    assert.equal(agentGrouped.workspace_name, "Client Alpha");
    await assert.rejects(
      () => workspaces.remove({ id: alpha.id }, seed),
      (err) => err.status === 403 && err.code === "admin_only",
      "bulk workspace removal remains human-admin-only"
    );

    await adminPost("/api/v1/admin/pages/adm/workspace", { workspace_id: beta.id });
    groupedIndex = await adminGet("/api/v1/admin/pages");
    assert.equal(groupedIndex.json.workspaces.find((w) => w.id === alpha.id).page_count, 1, "moving decrements old workspace without losing the agent-organized page");
    assert.equal(groupedIndex.json.workspaces.find((w) => w.id === beta.id).page_count, 2, "moving increments destination");
    for (const workspace of groupedIndex.json.workspaces) {
      const actual = groupedIndex.json.pages.filter((p) => p.workspace_id === workspace.id).length;
      assert.equal(workspace.page_count, actual, "workspace count derives from the returned page snapshot");
    }
    const beforeRemoval = await adminGet("/api/v1/admin/pages/adm");
    const beforeVersionIds = beforeRemoval.json.versions.map((v) => v.id);
    const removed = await adminPost(`/api/v1/admin/workspaces/${beta.id}/delete`, {});
    assert.equal(removed.status, 200);
    assert.equal(removed.json.workspace.ungrouped_page_count, 2, "delete reports detached active pages");
    assert.equal((await adminPost(`/api/v1/admin/workspaces/${beta.id}/delete`, {})).status, 404, "deleted workspace is gone");

    detail = await adminGet("/api/v1/admin/pages/adm");
    assert.equal(detail.status, 200, "member page survives workspace removal");
    assert.equal(detail.json.page.workspace_id, null, "member moves to Ungrouped");
    assert.equal(detail.json.page.slug, beforeRemoval.json.page.slug, "slug unchanged");
    assert.equal(detail.json.page.published_version_id, beforeRemoval.json.page.published_version_id, "published pointer unchanged");
    assert.equal(detail.json.page.require_approval, beforeRemoval.json.page.require_approval, "approval setting unchanged");
    assert.deepEqual(detail.json.versions.map((v) => v.id), beforeVersionIds, "version history unchanged");
    assert.equal((await adminGet("/api/v1/admin/pages/admgrouped")).json.page.workspace_id, null, "every member safely detached");
    await adminPost(`/api/v1/admin/workspaces/${alpha.id}/delete`, {});
    assert.deepEqual((await adminGet("/api/v1/admin/workspaces")).json.workspaces, [], "workspace CRUD cleanup complete");

    const workspaceAudit = await db.query(
      `SELECT action FROM audit_log
        WHERE actor = $1 AND action IN ('create_workspace', 'rename_workspace', 'delete_workspace', 'set_workspace')`,
      [ADMIN]
    );
    const auditedActions = new Set(workspaceAudit.rows.map((r) => r.action));
    for (const action of ["create_workspace", "rename_workspace", "delete_workspace", "set_workspace"]) {
      assert.ok(auditedActions.has(action), `${action} is audit logged`);
    }
    console.log("✓ workspace CRUD + assignment + migration + safe audited removal");

    // 3d. Partner portals: ONE shared client credential over an admin-curated
    // SET of dashboards. Unlike workspaces this is not organization — membership
    // decides which client's numbers a partner's password opens — so the
    // authority is inverted: humans only, at the route AND at the library. The
    // section ends on the content host, because the point of the whole admin
    // surface is what it changes for a partner.
    await versions.createPage({ slug: "pt-overview", title: "NWM Client Overview" }, seed);
    await versions.deploy({ slug: "pt-overview", html: HTML("pt-1"), publish: true }, seed);
    await versions.createPage({ slug: "pt-campaign", title: "NWM Contoso Allergex" }, seed);
    await versions.deploy({ slug: "pt-campaign", html: HTML("pt-2"), publish: true }, seed);
    await versions.setPassword({ slug: "pt-campaign", password: "existing-client-password" }, seed);

    assert.equal((await req("GET", "/api/v1/admin/portals")).status, 401, "portal reads are admin-only");
    assert.equal((await req("GET", "/api/v1/admin/portals", { cookie: nonAdminCookie })).status, 403, "a signed-in outsider is not an admin");
    assert.equal(
      (await req("POST", "/api/v1/admin/portals", { cookie: adminCookie, origin: ORIGIN, body: { slug: "nocsrf", name: "No CSRF" } })).status,
      403,
      "portal creation requires CSRF"
    );
    // The exhaustive per-verb matrix is a unit test; this is the same guard at
    // the real seam, with a real agent actor context.
    for (const [verb, input] of [
      ["create", { slug: "agent-made", name: "Agent Made" }],
      ["addPage", { id: 1, slug: "pt-overview" }],
      ["setPassword", { id: 1 }],
      ["remove", { id: 1 }],
    ]) {
      await assert.rejects(
        () => portals[verb](input, seed),
        (err) => err.status === 403 && err.code === "portal_admin_only",
        `an agent may never ${verb} a portal — who sees which dashboards is a human decision`
      );
    }

    assert.equal((await adminPost("/api/v1/admin/portals", { slug: "nwm/nested", name: "Nested" })).status, 400, "a portal slug is one url-safe segment");
    assert.equal((await adminPost("/api/v1/admin/portals", { slug: "nwm", name: "  " })).status, 400, "blank name rejected");
    assert.equal(
      (await adminPost("/api/v1/admin/portals", { slug: "nwm", name: "NWM", password: "short-one" })).status,
      400,
      "one weak password would stand in front of every dashboard in the portal"
    );
    const nwmCreated = await adminPost("/api/v1/admin/portals", { slug: "nwm", name: "Northwind Media Group" });
    assert.equal(nwmCreated.status, 201, JSON.stringify(nwmCreated.json));
    const nwm = nwmCreated.json.portal;
    const nwmPassword = nwmCreated.json.password;
    assert.equal(nwmCreated.json.password_generated, true, "Pages generates the credential by default");
    assert.match(nwmPassword, /^[a-hjkmnp-z2-9]{4}(?:-[a-hjkmnp-z2-9]{4}){3}$/, "a transcribable ~79-bit credential");
    assert.doesNotMatch(nwmCreated.body, /scrypt\$/, "no portal response ever carries the stored hash");
    // Both live-unique indexes land on 23505; the codes must tell an admin WHICH
    // collided, or the UI can only say "conflict".
    const dupSlug = await adminPost("/api/v1/admin/portals", { slug: "nwm", name: "Other" });
    assert.equal(dupSlug.status, 409, "duplicate live slug");
    assert.equal(dupSlug.json.code, "portal_exists");
    const dupName = await adminPost("/api/v1/admin/portals", { slug: "nwm-2", name: "northwind media group" });
    assert.equal(dupName.status, 409, "case-only duplicate name");
    assert.equal(dupName.json.code, "portal_name_exists");

    const addOverview = await adminPost(`/api/v1/admin/portals/${nwm.id}/pages`, { slug: "pt-overview", label: "Portfolio overview", sort_order: 0 });
    assert.equal(addOverview.status, 201, JSON.stringify(addOverview.json));
    assert.equal(
      addOverview.json.reclassifies_staff_only,
      true,
      "a staff-only page joining a portal becomes client-readable — reported at the moment of adding"
    );
    // Labelled so that curated order and alphabetical order DISAGREE: "Contoso"
    // sorts before "Portfolio", so an assertion on the returned order can only
    // pass if sort_order is what decides it.
    const addCampaign = await adminPost(`/api/v1/admin/portals/${nwm.id}/pages`, { slug: "pt-campaign", label: "Contoso — Allergex", sort_order: 1 });
    assert.equal(addCampaign.status, 201);
    assert.equal(addCampaign.json.reclassifies_staff_only, false, "a page that already had a client password is not reclassified");
    assert.equal(addCampaign.json.member.display_title, "Contoso — Allergex", "the curated label is the partner-facing title");
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/pages`, { slug: "pt-campaign" })).status, 409, "already a member");
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/pages`, { slug: "pt-nonexistent" })).status, 404, "unknown page");
    assert.equal((await adminPost("/api/v1/admin/portals/999999/pages", { slug: "pt-campaign" })).status, 404, "unknown portal");

    const portalList = await adminGet("/api/v1/admin/portals");
    assert.equal(portalList.json.portals.length, 1, "one live portal");
    assert.equal(portalList.json.portals[0].page_count, 2, "membership count returned for the index");
    const portalDetail = await adminGet(`/api/v1/admin/portals/${nwm.id}`);
    assert.deepEqual(
      portalDetail.json.members.map((m) => m.slug),
      ["pt-overview", "pt-campaign"],
      "curated order wins — alphabetically the campaign page would come first and bury the macro view"
    );
    assert.equal(portalDetail.json.members[0].display_title, "Portfolio overview", "the curated label is what a partner reads, not the agent-settable page title");
    assert.equal(portalDetail.json.members[0].has_password, false);
    assert.equal(portalDetail.json.members[1].has_password, true);

    // Will these dashboards show the Page menu, and whose menu is it? Every themed
    // render gets one — Pages supplies a built-in control unless the design reads
    // #pages-nav itself. So both of these show a menu already, and neither owns it.
    assert.equal(
      portalDetail.json.members.every((m) => m.shows_switcher),
      true,
      "a themed dashboard gets a Page menu whether or not its design was written for one"
    );
    assert.equal(
      portalDetail.json.members.some((m) => m.switcher_is_own),
      false,
      "…and neither of these designs renders its own yet"
    );
    await versions.deploy(
      {
        slug: "pt-overview",
        html: `<!doctype html><html><head><title>Overview</title></head><body><h1>Overview</h1><script>
          const nav = document.getElementById("pages-nav");
          if (nav) draw(JSON.parse(nav.textContent));
        </script></body></html>`,
        publish: true,
      },
      seed
    );
    const switcherAware = (await adminGet(`/api/v1/admin/portals/${nwm.id}`)).json.members;
    assert.equal(switcherAware.find((m) => m.slug === "pt-overview").switcher_is_own, true, "a design that reads the block owns its menu");
    assert.equal(switcherAware.find((m) => m.slug === "pt-campaign").switcher_is_own, false, "…and one that does not gets the built-in control");
    assert.equal(switcherAware.every((m) => m.shows_switcher), true, "either way, both show a menu");

    // The production trap: authoring boilerplate that MENTIONS #pages-nav in a
    // CSS comment without ever reading it (Lakeside/Hy-Vee, 2026-08-20). The old
    // position() scan called this "owns its menu" while the page showed none;
    // the consume-pattern regex must not.
    await versions.deploy(
      {
        slug: "pt-campaign",
        html: `<!doctype html><html><head><title>Contoso</title><style>/* Page switcher (Pages injects #pages-nav on a portal-authorised render; absent otherwise, and this whole control is then never built). */</style></head><body><h1>v3</h1></body></html>`,
        publish: true,
      },
      seed
    );
    const mentionOnly = (await adminGet(`/api/v1/admin/portals/${nwm.id}`)).json.members.find((m) => m.slug === "pt-campaign");
    assert.equal(mentionOnly.switcher_is_own, false, "a comment mention is not ownership — the built-in menu serves");
    assert.equal(mentionOnly.shows_switcher, true, "and the row still reports a menu");

    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/home`, {})).status, 400, "an explicit slug is required (null clears)");
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/home`, { slug: "adm" })).status, 400, "a home page outside the portal is a link to a password wall");
    assert.equal(
      (await adminPost(`/api/v1/admin/portals/${nwm.id}/home`, { slug: "pt-overview" })).json.portal.home_page_slug,
      "pt-overview"
    );

    // Link audit: the drift that keeps recurring is a hub (home) page whose
    // links agents keep current while the human-curated member list falls
    // behind — the partner then follows a link the hub shows and loses the Page
    // menu (Fabrikam/Lakeside, 2026-08-19). The detail read surfaces exactly those
    // links: live pages the home page links to that are not members.
    await versions.createPage({ slug: "pt-lakeside", title: "Lakeside campaign" }, seed);
    await versions.deploy(
      {
        slug: "pt-lakeside",
        html: "<!doctype html><html><head><title>Lakeside</title></head><body>lakeside</body></html>",
        publish: true,
      },
      seed
    );
    await versions.deploy(
      {
        slug: "pt-overview",
        html:
          `<!doctype html><html><head><title>Overview</title></head><body>` +
          `<a href="/pt-lakeside">Lakeside</a>` +
          `<a href="/pt-campaign">Contoso</a>` + // already a member: not drift
          `<a href="/pt-overview">self</a>` + // the home page itself: not drift
          `<a href="https://elsewhere.example.com/pt-lakeside">external, ignored</a>` +
          `</body></html>`,
        publish: true,
      },
      seed
    );
    const audited = (await adminGet(`/api/v1/admin/portals/${nwm.id}`)).json.link_audit;
    assert.equal(audited.scanned, true, "with a published home page the audit runs");
    assert.deepEqual(
      audited.missing.map((p) => p.slug),
      ["pt-lakeside"],
      "exactly the linked live non-member — members and the home page itself are not drift"
    );
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/pages`, { slug: "pt-lakeside" })).status, 201);
    assert.deepEqual(
      (await adminGet(`/api/v1/admin/portals/${nwm.id}`)).json.link_audit.missing,
      [],
      "adding the member clears the audit"
    );
    // Put membership back the way the rest of the flow expects it.
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/remove`, { slug: "pt-lakeside" })).status, 200);

    const relabelled = await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/update`, { slug: "pt-campaign", label: "Contoso — Allergex (Q3)" });
    assert.equal(relabelled.json.member.display_title, "Contoso — Allergex (Q3)");
    assert.equal(relabelled.json.member.sort_order, 1, "an absent field is left alone, not reset");
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/update`, { slug: "pt-campaign" })).status, 400, "nothing to update");
    const clearedLabel = await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/update`, { slug: "pt-campaign", label: null });
    assert.equal(clearedLabel.json.member.label, null);
    assert.equal(clearedLabel.json.member.display_title, "NWM Contoso Allergex", "clearing a label falls back to the page title");

    // Fan-out cap: each extra portal holding a page is one more scrypt candidate
    // on that page's password form once the serve predicate lands.
    for (let i = 2; i <= portals.MAX_PORTALS_PER_PAGE; i++) {
      const extra = (await adminPost("/api/v1/admin/portals", { slug: `fanout-${i}`, name: `Fan-out ${i}` })).json.portal;
      assert.equal((await adminPost(`/api/v1/admin/portals/${extra.id}/pages`, { slug: "pt-campaign" })).status, 201);
    }
    const overCap = (await adminPost("/api/v1/admin/portals", { slug: "fanout-over", name: "Fan-out over" })).json.portal;
    const capped = await adminPost(`/api/v1/admin/portals/${overCap.id}/pages`, { slug: "pt-campaign" });
    assert.equal(capped.status, 409, "portals-per-page is capped where it is created");
    assert.equal(capped.json.code, "portal_fanout_exceeded");

    const memberPageDetail = await adminGet("/api/v1/admin/pages/pt-campaign");
    assert.deepEqual(
      memberPageDetail.json.portals.map((p) => p.slug).sort(),
      ["fanout-2", "fanout-3", "fanout-4", "nwm"],
      "page detail answers 'who can see this page' — a page missing from one audience is invisible portal-first"
    );

    // What membership does on the content host. Adding a staff-only page to a
    // portal is the reclassification this API reports at add time: it stops being
    // a 403 "available to Elcano staff" and starts prompting for the credential
    // that now opens it. The gate still names no portal — that a page belongs to
    // one is implied by the prompt, which one is not.
    const staffOnlyMember = await req("GET", "/pt-overview", { host: CONTENT_HOST });
    assert.equal(staffOnlyMember.status, 401, "the page reported as reclassified is the page whose gate changed");
    assert.match(staffOnlyMember.body, /Portal password/);
    assert.doesNotMatch(staffOnlyMember.body, /Staff-only page/, "a partner is not told they are unentitled to their own dashboard");
    assert.doesNotMatch(staffOnlyMember.body, /Northwind Media Group/, "…and membership does not leak from the gate");
    const passwordedMember = await req("GET", "/pt-campaign", { host: CONTENT_HOST });
    assert.equal(passwordedMember.status, 401, "a member with its own password still gets a password form");
    assert.equal(passwordedMember.headers["set-cookie"], undefined, "membership itself mints no session");

    const removedHome = await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/remove`, { slug: "pt-overview" });
    assert.equal(removedHome.json.removed, true);
    assert.equal(removedHome.json.home_cleared, true, "removing the home page clears it rather than pointing partners at a page the portal no longer opens");
    assert.equal(
      (await adminGet(`/api/v1/admin/portals/${nwm.id}`)).json.portal.home_page_id,
      null,
      "the stored home pointer is cleared, not just reported as cleared"
    );
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/remove`, { slug: "pt-overview" })).status, 404, "not a member any more");

    // A soft-deleted member leaves a membership row whose slug no longer
    // resolves — and whose slug a NEW page may take. It stays visible, and
    // removable by page id, rather than becoming permanent.
    await versions.createPage({ slug: "pt-gone", title: "Retired dashboard" }, seed);
    const addGone = await adminPost(`/api/v1/admin/portals/${nwm.id}/pages`, { slug: "pt-gone" });
    assert.equal(addGone.json.member.display_title, "Retired dashboard", "no label ⇒ fall back to the page title");
    await adminPost("/api/v1/admin/pages/pt-gone/delete", {});
    const orphan = (await adminGet(`/api/v1/admin/portals/${nwm.id}`)).json.members.find((m) => m.slug === "pt-gone");
    assert.equal(orphan.page_deleted, true, "a membership row whose page was deleted stays visible");
    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/remove`, { slug: "pt-gone" })).status, 404, "its slug no longer resolves…");
    assert.equal(
      (await adminPost(`/api/v1/admin/portals/${nwm.id}/pages/remove`, { page_id: orphan.page_id })).json.removed,
      true,
      "…so it is removable by page id"
    );

    assert.equal((await adminPost(`/api/v1/admin/portals/${nwm.id}/rename`, { name: "NWM" })).json.portal.name, "NWM");
    const rotated = await adminPost(`/api/v1/admin/portals/${nwm.id}/password`, {});
    assert.equal(rotated.status, 200);
    assert.notEqual(rotated.json.password, nwmPassword, "rotation mints a new credential — which is how a portal session is revoked");
    const retired = await adminPost(`/api/v1/admin/portals/${nwm.id}/delete`, {});
    assert.equal(retired.json.portal.deleted, true);
    assert.equal(retired.json.portal.member_count, 1, "retiring reports what it takes out of service");
    assert.equal((await adminGet(`/api/v1/admin/portals/${nwm.id}`)).status, 404, "a retired portal is gone from every read");
    // Soft, like a page: the row and its membership survive, so an accidental
    // retire is recoverable rather than a destroyed partner configuration.
    const retiredRow = await db.query(
      `SELECT deleted_at,
              (SELECT count(*)::int FROM page_portal_members m WHERE m.portal_id = page_portals.id) AS members
         FROM page_portals WHERE id = $1`,
      [nwm.id]
    );
    assert.equal(retiredRow.rowCount, 1, "retiring keeps the row — it is a soft delete, not a destroy");
    assert.notEqual(retiredRow.rows[0].deleted_at, null);
    assert.equal(retiredRow.rows[0].members, 1, "…and its membership is kept with it");
    // …and it is out of service, not merely hidden: every mutation goes through
    // the same lock, so none of them can touch it again.
    assert.equal(
      (await adminPost(`/api/v1/admin/portals/${nwm.id}/pages`, { slug: "pt-overview" })).status,
      404,
      "a retired portal accepts no further mutation"
    );
    assert.equal(
      (await adminPost("/api/v1/admin/portals", { slug: "nwm", name: "Northwind Media Group" })).status,
      201,
      "retiring frees the slug and the name for reuse"
    );
    // The retired portal keeps its membership rows (soft delete), so this is what
    // proves they stop counting: a page is no longer exposed by a retired portal,
    // and a NEW portal on the freed slug does not inherit the old one's members.
    assert.deepEqual(
      (await adminGet("/api/v1/admin/pages/pt-campaign")).json.portals.map((p) => p.slug).sort(),
      ["fanout-2", "fanout-3", "fanout-4"],
      "a retired portal exposes nothing, and reusing its slug inherits no membership"
    );

    // The non-empty CHECK is load-bearing, not tidiness: credentialDigest
    // digests `cred:` || (hash || ''), so a portal holding '' would share its
    // digest with every staff-only page and a staff /view cookie would then
    // verify as a portal session.
    await assert.rejects(
      () => db.query(`INSERT INTO page_portals (slug, name, password_hash) VALUES ('empty', 'Empty', '')`),
      /violates check constraint/,
      "a portal can never hold an empty credential"
    );

    const portalActions = await db.query(
      `SELECT DISTINCT action FROM audit_log WHERE actor = $1 AND action LIKE '%portal%' ORDER BY action`,
      [ADMIN]
    );
    assert.deepEqual(
      portalActions.rows.map((r) => r.action),
      [
        "add_portal_page", "create_portal", "delete_portal", "remove_portal_page",
        "rename_portal", "set_portal_home", "set_portal_password", "update_portal_page",
      ],
      "every portal mutation is audited under its own action name"
    );
    const reclassifiedRow = await db.query(
      `SELECT metadata FROM audit_log
        WHERE action = 'add_portal_page' AND metadata->>'page_slug' = 'pt-overview' LIMIT 1`
    );
    assert.equal(
      reclassifiedRow.rows[0].metadata.reclassifies_staff_only,
      true,
      "the reclassification of a staff-only page is stamped into the audit metadata"
    );
    const leaked = await db.query(`SELECT count(*)::int AS n FROM audit_log WHERE metadata::text LIKE $1`, [`%${nwmPassword}%`]);
    assert.equal(leaked.rows[0].n, 0, "a portal credential never reaches the audit log");
    console.log("✓ partner portals (admin-only CRUD, membership, fan-out cap, audited — serving unchanged)");

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

    // 6b. one version's full row, html included. The detail read strips `html`
    // from every entry in `versions` on purpose, so the admin source editor
    // cannot open the version under review without this route — and the route
    // only works because it is declared BEFORE the greedy `/pages/*slug` detail
    // read. Pin the ordering, the per-page scoping, and the auth gate here: the
    // browser suite exercises a fixture reimplementation, not this code.
    const one = await adminGet(`/api/v1/admin/pages/admg/versions/${p1.id}`);
    assert.equal(one.status, 200, "flat slug: one version reads");
    assert.equal(one.json.version.id, p1.id);
    assert.equal(one.json.version.html, HTML(1), "the row carries the exact deployed html");
    assert.equal(
      (await adminGet("/api/v1/admin/pages/admg")).json.versions.every((v) => !Object.hasOwn(v, "html")),
      true,
      "…which the detail read still does not carry, or this route would be pointless"
    );
    // Scoping: a real version id, asked for on the wrong page, is a 404 — never
    // another client's HTML.
    const foreign = await adminGet(`/api/v1/admin/pages/adm/versions/${p1.id}`);
    assert.equal(foreign.status, 404, "a version belonging to another page → 404");
    assert.match(foreign.json.error, /not found/, "and says so rather than leaking the row");
    assert.equal((await adminGet("/api/v1/admin/pages/admg/versions/999999")).status, 404, "unknown version → 404");
    assert.equal((await adminGet("/api/v1/admin/pages/admg/versions/abc")).status, 400, "non-numeric id → 400");
    assert.equal((await adminGet("/api/v1/admin/pages/admg/versions/0")).status, 400, "zero id → 400");
    assert.equal((await req("GET", `/api/v1/admin/pages/admg/versions/${p1.id}`)).status, 401, "no cookie → 401");
    assert.equal(
      (await req("GET", `/api/v1/admin/pages/admg/versions/${p1.id}`, { cookie: nonAdminCookie })).status,
      403,
      "non-admin → 403"
    );
    console.log("✓ admin version read (html, per-page scoping, 400/401/403/404)");

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

    // 8c. admin create + rename + delete/restore (incl. deleting a GATED page,
    // which agents cannot do).
    const created = await adminPost("/api/v1/admin/pages", { slug: "admnew", title: "Admin New" });
    assert.equal(created.status, 201, "admin can create a page");
    assert.equal(created.json.page.slug, "admnew");
    assert.equal((await adminPost("/api/v1/admin/pages/admnew/title", { title: "Renamed" })).json.title, "Renamed");
    assert.equal((await adminPost("/api/v1/admin/pages/admg/delete", {})).json.deleted, true, "admin deletes a gated page");
    assert.equal((await adminGet("/api/v1/admin/pages/admg")).status, 404, "deleted page → 404");
    assert.equal((await adminPost("/api/v1/admin/pages/admg/restore", {})).json.restored, true, "admin restores");
    assert.equal((await adminGet("/api/v1/admin/pages/admg")).status, 200, "restored page visible again");
    // Slug reservation is a creation-time rule, so a soft-deleted row can predate
    // a newly reserved segment. Restoring it is not a read — it would put a live
    // page at an address the router now owns, i.e. a page nobody can open.
    await db.query(`INSERT INTO pages (slug, title, deleted_at) VALUES ('portal/legacy', 'Predates the reservation', now())`);
    const resurrect = await adminPost("/api/v1/admin/pages/portal/legacy/restore", {});
    assert.equal(resurrect.status, 400, "restore must refuse a reserved slug");
    assert.equal(resurrect.json.code, "reserved_slug");
    console.log("✓ admin create + rename + delete + restore");

    // 9. the /admin/:slug HTML shell itself: admin → 200 bootstrap; anon → login redirect.
    const shell = await req("GET", "/admin/adm", { cookie: adminCookie });
    assert.equal(shell.status, 200);
    assert.match(shell.body, /id="pages-bootstrap"/, "shell injects bootstrap");
    assert.match(shell.body, /id="page-switcher"[^>]+aria-busy="true"/, "shell reserves an accessible loading picker");
    assert.match(shell.body, /id="page-switcher-select"[^>]+aria-describedby="page-switcher-count"[^>]+disabled/, "picker starts in a safe loading state");
    assert.match(shell.body, /\/shell-assets\/page-switcher\.js/, "shell loads the picker model before admin.js");
    assert.match(shell.body, /\/shell-assets\/primitives\.js/, "shell loads shared dialog/toast/DOM primitives");
    assert.match(shell.body, /\/shell-assets\/admin\.js/, "shell loads admin.js");
    assert.doesNotMatch(shell.body, /refresh-ui\.js/, "shell no longer loads the retired scheduler UI");
    assert.match(shell.body, /shell-assets\/flag\/tokens\/design-tokens\.css/, "shell links Flag tokens");
    assert.match(shell.body, /shell-assets\/flag\/logos\/elcano-mark-primary\.svg/, "shell uses the approved Elcano mark");
    const landing = await req("GET", "/admin", { cookie: adminCookie });
    assert.equal(landing.status, 200, "workspace index shell renders for admin");
    assert.match(landing.body, /\/shell-assets\/welcome\.js/, "workspace index loads its browser UI");
    assert.match(landing.body, /"csrf":"[^"]+"/, "workspace index receives a CSRF token for organization mutations");
    const welcomeAsset = await req("GET", "/shell-assets/welcome.js");
    assert.equal(welcomeAsset.status, 200);
    assert.match(welcomeAsset.body, /function filterWorkspacePages/, "served index uses the tested grouped view helper");
    assert.doesNotMatch(welcomeAsset.body, /window\.(?:prompt|confirm|alert)/, "index has no native browser dialogs");
    assert.equal((await req("GET", "/shell-assets/refresh-ui.js")).status, 404, "retired refresh model is not served");
    const adminAsset = await req("GET", "/shell-assets/admin.js");
    assert.equal(adminAsset.status, 200);
    assert.doesNotMatch(adminAsset.body, /Run update now|\/refresh\/run|data-refresh-confirm/, "page shell has no retired dispatcher controls");
    assert.doesNotMatch(adminAsset.body, /window\.(?:prompt|confirm|alert)/, "page detail has no native browser dialogs");
    const anon = await req("GET", "/admin/adm");
    assert.equal(anon.status, 302, "anon → redirect to auth login");
    console.log("✓ /admin/:slug shell (200 for admin, 302 for anon)");

    // 9b. The template library SHELL route. This had no coverage and shipped
    //     calling a csrf helper that does not exist, so every request 500ed and
    //     the screen was unreachable in production while its browser tests (which
    //     mount their own route) and its JSON API tests were all green. Assert the
    //     route server.js actually registers.
    const libraryShell = await req("GET", "/admin/templates", { cookie: adminCookie });
    assert.equal(libraryShell.status, 200, "template library shell renders for admin");
    assert.match(libraryShell.body, /\/shell-assets\/templates\.js/, "library loads its browser UI");
    assert.match(libraryShell.body, /"csrf":"[^"]+"/, "library receives a CSRF token for registration");
    assert.equal(
      (await req("GET", "/admin/templates")).status,
      302,
      "anon → redirect to auth login"
    );
    console.log("✓ /admin/templates shell (200 for admin, 302 for anon)");

    // 9c. The partner portal SHELL route. Same lesson as the library above: the
    // browser tests mount their own route, so only this one proves the route
    // server.js actually registers — and it must resolve ahead of /admin/*slug.
    const portalShellRes = await req("GET", "/admin/portals", { cookie: adminCookie });
    assert.equal(portalShellRes.status, 200, "portal screen renders for admin");
    assert.match(portalShellRes.body, /\/shell-assets\/portals\.js/, "portal screen loads its browser UI");
    assert.match(portalShellRes.body, /"csrf":"[^"]+"/, "…and receives a CSRF token for membership mutations");
    assert.match(portalShellRes.body, /context-tab[^>]*href="\/admin\/portals"[^>]*aria-current="page"/, "the section tab marks itself current");
    assert.equal((await req("GET", "/admin/portals")).status, 302, "anon → redirect to auth login");
    const portalAsset = await req("GET", "/shell-assets/portals.js");
    assert.equal(portalAsset.status, 200);
    assert.match(portalAsset.body, /function describeMember/, "the served module contains the tested helper");
    assert.doesNotMatch(portalAsset.body, /window\.(?:prompt|confirm|alert)/, "no native browser dialogs");
    // A page cannot take the address the screen lives at.
    assert.equal((await adminPost("/api/v1/admin/pages", { slug: "portals", title: "Shadow" })).status, 400, "'portals' is a reserved slug");
    console.log("✓ /admin/portals shell (200 for admin, 302 for anon, reserved slug)");

    // 10. nested slugs: shell, admin API, and preview-token all resolve
    //     end to end (the minted /raw URL actually renders on the content host).
    await versions.createPage({ slug: "adm/nest", title: "Nested Admin" }, seed);
    const nv1 = (await versions.deploy({ slug: "adm/nest", html: HTML(5), publish: true }, seed)).version.id;
    const nshell = await req("GET", "/admin/adm/nest", { cookie: adminCookie });
    assert.equal(nshell.status, 200, "/admin/<nested slug> shell resolves");
    assert.match(nshell.body, /id="pages-bootstrap"/, "nested shell bootstraps");
    const nget = await adminGet("/api/v1/admin/pages/adm/nest");
    assert.equal(nget.status, 200, "admin API read on nested slug");
    assert.equal(nget.json.page.published_version_id, nv1);
    const nestedWorkspace = (await adminPost("/api/v1/admin/workspaces", { name: "Nested reports" })).json.workspace;
    const nestedAssignment = await adminPost("/api/v1/admin/pages/adm/nest/workspace", { workspace_id: nestedWorkspace.id });
    assert.equal(nestedAssignment.status, 200, "workspace mutation resolves a nested slug");
    assert.equal((await adminGet("/api/v1/admin/pages/adm/nest")).json.page.workspace_name, "Nested reports");
    const npub = await adminPost("/api/v1/admin/pages/adm/nest/deploy-source", { html: HTML(6) });
    assert.equal(npub.status, 200, "admin mutation on nested slug");
    // The `*slug` splat is greedy, so "/adm/nest/versions/<id>" is exactly the
    // shape the detail route would swallow if the version read were declared
    // after it. A nested slug is the only place that can go wrong.
    const nver = await adminGet(`/api/v1/admin/pages/adm/nest/versions/${nv1}`);
    assert.equal(nver.status, 200, "nested slug: one version reads, not swallowed by the detail route");
    assert.equal(nver.json.version.html, HTML(5), "nested version read carries the deployed html");
    assert.equal((await adminGet(`/api/v1/admin/pages/adm/versions/${nv1}`)).status, 404, "a nested page's version is not readable on its parent");
    const npv = await adminPost("/api/v1/admin/pages/adm/nest/preview-token", { version_id: nv1 });
    assert.equal(npv.status, 200);
    assert.match(npv.json.url, /\/raw\/adm\/nest\?t=/, "preview url embeds the nested slug");
    const npath = npv.json.url.replace(/^https?:\/\/[^/]+/, "");
    const npr = await req("GET", npath, { host: "content.localhost" });
    assert.equal(npr.status, 200, "preview URL renders on the content host");
    await adminPost(`/api/v1/admin/workspaces/${nestedWorkspace.id}/delete`, {});
    console.log("✓ nested slug: /admin shell + admin API + preview-token render");

    // 11. Cross-page switcher: flow the real admin list response through the
    // same pure model used by admin.js. A 35-page run covers the long native
    // select case while also proving active state, deterministic neighbors,
    // status context, and nested-slug-safe direct navigation.
    for (let i = 1; i <= 35; i++) {
      const n = String(i).padStart(2, "0");
      await versions.createPage({ slug: `switch-${n}`, title: `Client ${n}` }, seed);
    }
    const pickerList = await adminGet("/api/v1/admin/pages");
    assert.equal(pickerList.status, 200, "picker list endpoint available to admin");
    const picker = pageSwitcher.model(pickerList.json.pages, "switch-15");
    assert.ok(picker.total >= 39, "long list includes all seeded admin pages");
    assert.equal(picker.current.slug, "switch-15", "active page identified");
    assert.equal(picker.previous.slug, "switch-14", "deterministic previous page");
    assert.equal(picker.next.slug, "switch-16", "deterministic next page");
    assert.equal(picker.next.href, "/admin/switch-16", "selection targets the existing admin route");
    assert.match(picker.current.optionLabel, /Client 15 — \/switch-15 · draft$/, "title, slug, and status are selection context");
    assert.equal(picker.items.find((item) => item.slug === "adm/nest").href, "/admin/adm/nest", "nested selection preserves route segments");
    const pickerAsset = await req("GET", "/shell-assets/page-switcher.js");
    assert.equal(pickerAsset.status, 200, "picker browser helper is served");
    assert.match(pickerAsset.body, /function model\(/, "served helper contains the tested model");
    console.log("✓ admin page switcher (long list + active + navigation + nested slug)");

    // ── template library ───────────────────────────────────────────────────
    // The human surface for stored designs. Two invariants beyond "it works":
    // uploading VALIDATES before it writes, and a design previews only on the
    // CONTENT host — untrusted HTML never renders in the dashboard origin.
    const fs = require("node:fs");
    const path = require("node:path");
    const templateHtml = fs.readFileSync(
      path.join(__dirname, "..", "templates", "nwm-campaign-dashboard", "template.html"),
      "utf8"
    );

    // Admin-only + CSRF, like every other mutation here.
    assert.equal((await req("GET", "/api/v1/admin/templates")).status, 401, "no cookie → 401");
    assert.equal(
      (await req("POST", "/api/v1/admin/templates/validate", { cookie: adminCookie, origin: ORIGIN, body: { html: "x" } })).status,
      403,
      "no CSRF token → 403"
    );

    // A dry run writes NOTHING — that is the whole point of a separate endpoint.
    const before = (await adminGet("/api/v1/admin/templates")).json.templates.length;
    const badCheck = await adminPost("/api/v1/admin/templates/validate", {
      html: "<!doctype html><html><body><p>not a template</p></body></html>",
      name: "Not A Name!",
    });
    assert.equal(badCheck.status, 200, "a dry run REPORTS, it does not fail the request");
    assert.equal(badCheck.json.contract_ok, false);
    assert.equal(badCheck.json.contract_error.code, "template_contract_invalid");
    assert.equal(badCheck.json.name_error.code, "bad_template_name", "the name is checked too");
    assert.equal((await adminGet("/api/v1/admin/templates")).json.templates.length, before, "validate wrote nothing");

    const goodCheck = await adminPost("/api/v1/admin/templates/validate", {
      html: templateHtml,
      name: "admin-nwm",
    });
    assert.equal(goodCheck.json.contract_ok, true, JSON.stringify(goodCheck.json.contract_error));
    assert.equal(goodCheck.json.name, "admin-nwm");
    assert.equal(goodCheck.json.ships_empty, true, "a template must ship its empty state");
    assert.equal(goodCheck.json.preflight.ok, true);
    assert.ok(goodCheck.json.config_schema && goodCheck.json.data_schema && goodCheck.json.reference_config);
    assert.equal((await adminGet("/api/v1/admin/templates")).json.templates.length, before, "still wrote nothing");

    // A malformed template is refused with the reason, not a generic 500.
    const rejected = await adminPost("/api/v1/admin/templates", { html: "<p>nope</p>", name: "admin-broken" });
    assert.equal(rejected.status, 422, JSON.stringify(rejected.json));
    assert.equal(rejected.json.code, "template_contract_invalid");
    assert.equal((await adminGet("/api/v1/admin/templates")).json.templates.length, before);

    // Register for real.
    const registered = await adminPost("/api/v1/admin/templates", {
      html: templateHtml,
      name: "admin-nwm",
      title: "Admin Uploaded NWM",
      description: "Registered through the library.",
    });
    assert.equal(registered.status, 201, JSON.stringify(registered.json));
    assert.equal(registered.json.template.name, "admin-nwm");
    assert.equal(registered.json.revision.source, "admin");
    assert.equal(registered.json.revision.revision, 1);
    // The shipped design carries a preview-only example dataset, which is what
    // lets the library render it populated instead of as a skeleton.
    assert.equal(registered.json.has_sample_data, true, "the NWM template ships example data");
    assert.equal(registered.json.revision.has_sample_data, true);

    // Detail carries what the library screen needs: schemas, reference config,
    // revisions, and which pages were built from it.
    const tplDetail = (await adminGet("/api/v1/admin/templates/admin-nwm")).json;
    assert.equal(tplDetail.template.current_revision, 1);
    assert.equal(tplDetail.revisions.length, 1);
    assert.equal(tplDetail.pages.length, 0);
    assert.ok(Object.keys(tplDetail.reference_config).length > 0);
    assert.equal(tplDetail.html, undefined, "the library reads the contract, not the design bytes");

    // Preview: a signed short-TTL URL on the CONTENT origin.
    const preview = (await adminPost("/api/v1/admin/templates/admin-nwm/preview-token", { render_mode: "themed" })).json;
    assert.ok(preview.url.startsWith(CONTENT_ORIGIN), `preview must be on the content host, got ${preview.url}`);
    assert.match(preview.url, /\/raw-template\/\d+\?t=/);
    assert.equal(preview.revision, 1);

    // ...and it renders there, sandboxed.
    const previewPath = preview.url.slice(CONTENT_ORIGIN.length);
    const rendered = await req("GET", previewPath, { host: CONTENT_HOST });
    assert.equal(rendered.status, 200, rendered.body.slice(0, 200));
    assert.match(rendered.headers["content-security-policy"] || "", /sandbox/, "the preview must be sandboxed");
    assert.ok(rendered.body.includes("pages-config-schema"), "the design was served");

    // A preview is the template MATERIALIZED: the example dataset is poured into
    // #pages-data so the design renders with numbers, and the example block
    // itself is gone, so the previewed bytes are the shape a page would have.
    assert.equal(preview.has_sample_data, true, "the token must say what the preview will show");
    assert.ok(
      !rendered.body.includes("pages-data-example"),
      "the example block must not survive into the rendered preview"
    );
    const previewEnvelope = JSON.parse(
      rendered.body.match(/<script[^>]*id="pages-data"[^>]*>([\s\S]*?)<\/script>/)[1]
    );
    assert.ok(
      previewEnvelope.data.rows.length >= 30,
      `the preview must render populated, got ${previewEnvelope.data.rows.length} rows`
    );
    assert.ok(
      previewEnvelope.data.rows.every((row) => typeof row.dealId === "string"),
      "the example rows must be the design's own shape"
    );

    // The token is bound to this revision, and is not a page token.
    const tokenValue = new URL(preview.url).searchParams.get("t");
    assert.equal((await req("GET", `/raw-template/999999?t=${encodeURIComponent(tokenValue)}`, { host: CONTENT_HOST })).status, 403,
      "a token for one revision must not render another");
    assert.equal((await req("GET", `/raw/adm?t=${encodeURIComponent(tokenValue)}`, { host: CONTENT_HOST })).status, 403,
      "a template token must not be replayable as a page view");
    assert.equal((await req("GET", previewPath.replace(/t=.*/, "t=forged"), { host: CONTENT_HOST })).status, 403);
    console.log("✓ template library (validate-before-write, register, detail, sandboxed content-host preview)");

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
