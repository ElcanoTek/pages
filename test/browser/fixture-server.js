// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Deterministic browser fixture: real server-rendered shells and real browser
// assets, backed by an in-memory implementation of the unchanged admin payloads.
// Database/state-machine truth remains covered by test/run-integration.sh.
const fs = require("node:fs");
const path = require("node:path");
const render = require("../../lib/render");
const express = require("express");
const welcomeShell = require("../../lib/welcomeshell");
const adminShell = require("../../lib/adminshell");
const templateShell = require("../../lib/templateshell");
const portalShell = require("../../lib/portalshell");
const errorShell = require("../../lib/errorshell");
const contentview = require("../../lib/contentview");

const PORT = Number(process.env.PLAYWRIGHT_FIXTURE_PORT || 3210);
const app = express();
app.use(express.json({ limit: "3mb" }));
app.use(express.urlencoded({ extended: false }));

const NOW = "2026-07-22T09:30:00.000Z";
const DETAIL_SLUG = "long/client/q2-report";
// An approval-gated page whose only versions are still in the queue: it has
// source, it just has nothing LIVE. The admin must not tell that page's reviewer
// there is no source at all. (client-20 already carries published_version_id
// null + require_approval true from the generated run below.)
const GATED_SLUG = "client-20";
const LONG_TITLE = "North America Programmatic Revenue and Performance Review — Quarter Two Executive Detail";
const VERSION_HTML = "<!doctype html><html><head><title>Fixture page</title></head><body><main><h1>Published fixture source</h1><p>Charts remain intact.</p></main></body></html>";
// Every version carries its OWN source, so a test can tell which one the editor
// seeded itself from. Only `published` ships its html in the detail payload;
// anything else is read through GET /versions/:id, exactly as production does.
const sourceFor = (label) => VERSION_HTML.replace("Published fixture source", `${label} fixture source`);

function iso(minutesAgo) {
  return new Date(Date.parse(NOW) - minutesAgo * 60_000).toISOString();
}

function initialState() {
  const pages = [{
    id: 1,
    slug: DETAIL_SLUG,
    title: LONG_TITLE,
    client_id: null,
    workspace_id: 1,
    workspace_name: "Campaign operations",
    theme_id: null,
    theme_name: "flag",
    require_approval: true,
    disabled: false,
    published_version_id: 102,
    has_password: true,
    is_live: true,
    created_at: iso(5000),
    updated_at: iso(5),
    // Frozen data behind a recently-touched page row: the exact shape the
    // "Last update" column could never show, since updated_at moves when
    // someone renames a page and stays put when a daily refresh stops.
    freshness: {
      source_as_of: iso(46 * 1440),
      refreshed_at: iso(27 * 1440),
      checked_at: iso(2),
      last_check_outcome: "source_not_updated",
      last_check_detail: "upstream max date still 2026-07-02",
      last_check_source_as_of: iso(46 * 1440),
      days_since_source: 46,
      days_since_refresh: 27,
      days_since_check: 0,
    },
  }];
  for (let index = 1; index <= 36; index += 1) {
    const number = String(index).padStart(2, "0");
    const published = index % 4 === 0 ? null : 200 + index;
    pages.push({
      id: index + 1,
      slug: index === 36 ? "nested/team/annual-performance-with-a-very-long-slug" : `client-${number}`,
      title: index === 36 ? "A very long client title that must wrap without widening the page at any viewport" : `Client ${number} operations dashboard`,
      client_id: null,
      workspace_id: index % 3 === 0 ? null : (index % 2 ? 1 : 2),
      workspace_name: index % 3 === 0 ? null : (index % 2 ? "Campaign operations" : "Executive reporting"),
      theme_id: null,
      theme_name: "flag",
      require_approval: index % 5 === 0,
      disabled: index % 11 === 0,
      published_version_id: published,
      has_password: index % 2 === 0,
      is_live: Boolean(published) && index % 11 !== 0,
      created_at: iso(4000 - index),
      updated_at: iso(index * 13),
      // Most rows are managed and current; every fourth is a plain page with no
      // managed data at all, which must render no freshness line rather than a
      // row of zeroes.
      freshness: published
        ? {
            source_as_of: iso(index * 1440),
            refreshed_at: iso(index * 1440),
            checked_at: iso(index * 1440),
            last_check_outcome: null,
            last_check_detail: null,
            last_check_source_as_of: null,
            days_since_source: index,
            days_since_refresh: index,
            days_since_check: index,
          }
        : null,
    });
  }
  return {
    pages,
    workspaces: [
      { id: 1, name: "Campaign operations", created_at: iso(6000), updated_at: iso(6000) },
      { id: 2, name: "Executive reporting", created_at: iso(5900), updated_at: iso(5900) },
    ],
    details: {
      [GATED_SLUG]: {
        versions: [
          { id: 300, page_id: 21, status: "pending", render_mode: "themed", author: "agent@elcanotek.com", source: "mcp", note: "First cut, waiting on review", reviewed_by: null, reviewed_at: null, created_at: iso(15), html: sourceFor("Gated") },
        ],
      },
      [DETAIL_SLUG]: {
        versions: [
          { id: 106, page_id: 1, status: "pending", render_mode: "themed", author: "review-agent-with-a-long-address@elcanotek.com", source: "mcp", note: "Refresh the revenue totals and preserve the attribution chart. This intentionally long note must wrap cleanly on narrow screens.", reviewed_by: null, reviewed_at: null, created_at: iso(2), html: sourceFor("Pending") },
          { id: 105, page_id: 1, status: "pending", render_mode: "themed", author: "analyst@elcanotek.com", source: "api", note: "Client corrections", reviewed_by: null, reviewed_at: null, created_at: iso(10), html: sourceFor("Corrections") },
          { id: 104, page_id: 1, status: "draft", render_mode: "raw", author: "admin@elcanotek.com", source: "admin", note: "Alternative draft", reviewed_by: null, reviewed_at: null, created_at: iso(20), html: sourceFor("Draft") },
          { id: 103, page_id: 1, status: "rejected", render_mode: "themed", author: "agent@elcanotek.com", source: "mcp", note: "Rejected copy", reviewed_by: "reviewer@elcanotek.com", reviewed_at: iso(25), created_at: iso(30), html: sourceFor("Rejected") },
          { id: 102, page_id: 1, status: "approved", render_mode: "themed", author: "publisher@elcanotek.com", source: "api", note: "Current client-approved version", reviewed_by: "reviewer@elcanotek.com", reviewed_at: iso(35), created_at: iso(40), html: VERSION_HTML },
          { id: 101, page_id: 1, status: "approved", render_mode: "themed", author: "publisher@elcanotek.com", source: "api", note: "Prior live version", reviewed_by: "reviewer@elcanotek.com", reviewed_at: iso(50), created_at: iso(60), html: sourceFor("Prior") },
        ],
      },
    },
    events: [],
    failNextPages: false,
    delayNextPages: 0,
    nextWorkspaceId: 3,
    nextPageId: 50,
    nextVersionId: 107,
    templates: initialTemplates(),
  };
}

let state = initialState();

function workspaceRows() {
  return state.workspaces.map((workspace) => ({
    ...workspace,
    page_count: state.pages.filter((page) => String(page.workspace_id || "") === String(workspace.id)).length,
  }));
}

function pageRows() {
  return state.pages.map((page) => {
    const versions = state.details[page.slug]?.versions || [];
    const chronological = [...versions].reverse();
    const publishedIndex = chronological.findIndex((version) =>
      String(version.id) === String(page.published_version_id)
    );
    return {
      ...page,
      version_count: versions.length || (page.published_version_id ? 1 : 0),
      published_version_number: page.published_version_id
        ? (publishedIndex >= 0 ? publishedIndex + 1 : 1)
        : null,
    };
  });
}

function findPageFromPath(rawPath) {
  let decoded;
  try { decoded = decodeURIComponent(rawPath); } catch { decoded = rawPath; }
  const page = [...state.pages].sort((left, right) => right.slug.length - left.slug.length)
    .find((candidate) => decoded === candidate.slug || decoded.startsWith(candidate.slug + "/"));
  if (!page) return null;
  return { page, suffix: decoded.slice(page.slug.length) };
}

function detailFor(page) {
  if (!state.details[page.slug]) state.details[page.slug] = { versions: [] };
  const versions = state.details[page.slug].versions;
  const published = versions.find((version) => String(version.id) === String(page.published_version_id)) || null;
  return {
    page,
    versions: versions.map(({ html, ...version }) => version),
    pending: versions.filter((version) => version.status === "pending").map(({ html, ...version }) => version),
    published,
    themes: [
      { id: 1, name: "flag", default_mode: "system" },
      { id: 2, name: "client-brand", default_mode: "light" },
    ],
    csrfOk: true,
  };
}

function record(req, page) {
  state.events.push({ method: req.method, path: req.path, slug: page?.slug || null, body: req.body || {} });
}

// A template the library can list, inspect and preview. Only the fields the
// screen reads — the fixture stands in for the API, not for lib/templates.js.
function initialTemplates() {
  return [
    {
      id: "1", name: "nwm-campaign-dashboard", title: "NWM Campaign Dashboard",
      description: "Per-campaign delivery, revenue, margin and KPI pacing.",
      current_revision: 2, current_version_id: "2", created_at: NOW, updated_at: NOW,
      config_schema_sha256: "a".repeat(64), data_schema_sha256: "b".repeat(64), page_count: 2,
      has_sample_data: true,
    },
  ];
}

function templateDetail(template) {
  return {
    template,
    revision: {
      version_id: template.current_version_id, revision: template.current_revision,
      content_sha256: "f".repeat(64), config_schema_sha256: template.config_schema_sha256,
      data_schema_sha256: template.data_schema_sha256, author: "pages-cli:qa", source: "cli",
      note: "fixture revision", created_at: NOW, has_sample_data: template.has_sample_data,
    },
    config_schema: { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object", required: ["campaign"], properties: { campaign: { type: "string" } } },
    data_schema: { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object", required: ["rows"], properties: { rows: { type: "array" } } },
    reference_config: { campaign: "Reference Campaign", accountCode: "ACCT00000" },
    revisions: [
      { version_id: "2", revision: 2, content_sha256: "f".repeat(64), config_schema_sha256: "a".repeat(64), data_schema_sha256: "b".repeat(64), author: "pages-cli:qa", source: "cli", note: "chart fix", created_at: NOW, has_sample_data: true, is_current: true },
      { version_id: "1", revision: 1, content_sha256: "0".repeat(64), config_schema_sha256: "a".repeat(64), data_schema_sha256: "b".repeat(64), author: "qa-admin@elcanotek.com", source: "admin", note: null, created_at: NOW, has_sample_data: false, is_current: false },
    ],
    pages: [
      { slug: "contoso-allergex-acct00156", title: "Contoso", live_version_id: "10", revision: 2, behind: false, page_is_live: true, config_sha256: "1".repeat(64) },
      { slug: "nwm-vandelay-acct00142", title: "Vandelay", live_version_id: "11", revision: 1, behind: true, page_is_live: true, config_sha256: "2".repeat(64) },
    ],
  };
}

// The format check the screen depends on: a report, never a throw. "not a
// template" is keyed off the fixture HTML lacking the managed blocks, which is
// what the real validator decides too.
function templateReport(body) {
  const html = typeof body.html === "string" ? body.html : "";
  const rawName = typeof body.name === "string" ? body.name.trim() : "";
  const validName = /^[a-z0-9]+([-_][a-z0-9]+)*$/.test(rawName.toLowerCase()) && rawName !== "";
  const looksLikeTemplate = html.includes("pages-config-schema") && html.includes("pages-data-schema");
  return {
    name: validName ? rawName.toLowerCase() : null,
    name_error: rawName === "" || validName ? null : { code: "bad_template_name", message: "template must be url-safe with no slashes (a-z 0-9 - _)" },
    contract_ok: looksLikeTemplate,
    contract_error: looksLikeTemplate ? null : { code: "template_contract_invalid", message: "page is not data-managed; add exactly one #pages-data-schema block and one #pages-data block" },
    config_schema: looksLikeTemplate ? { type: "object" } : null,
    data_schema: looksLikeTemplate ? { type: "object" } : null,
    reference_config: looksLikeTemplate ? { campaign: "Reference" } : null,
    data_keys: looksLikeTemplate ? ["rows"] : null,
    ships_empty: looksLikeTemplate ? true : null,
    has_sample_data: looksLikeTemplate ? html.includes("pages-data-example") : null,
    sample_data_keys: looksLikeTemplate && html.includes("pages-data-example") ? ["rows"] : null,
    bytes: Buffer.byteLength(html, "utf8"),
    preflight: { ok: true, render_mode: "themed", errors: [], warnings: [], errors_omitted: 0, warnings_omitted: 0, checks: [], summary: "Preflight found no problems." },
  };
}

app.get("/healthz", (_req, res) => res.type("text").send("ok"));
app.post("/__fixture/reset", (_req, res) => { state = initialState(); res.json({ ok: true }); });
app.get("/__fixture/events", (_req, res) => res.json({ events: state.events }));
app.post("/__fixture/empty", (_req, res) => { state.pages = []; state.details = {}; res.json({ ok: true }); });
app.post("/__fixture/empty-workspaces", (_req, res) => {
  state.workspaces = [];
  state.pages.forEach((page) => {
    page.workspace_id = null;
    page.workspace_name = null;
  });
  res.json({ ok: true });
});
app.post("/__fixture/empty-templates", (_req, res) => { state.templates = []; res.json({ ok: true }); });
// 37 pages against a 25-row window means "Show more" is pressed exactly once and
// always removes itself, so the branch where the button SURVIVES a press is
// unreachable with the default fixture — and that is the branch a real account
// will live in once it passes fifty pages. This pads the list so both can be
// pinned.
app.post("/__fixture/pad-pages", (req, res) => {
  const count = Math.max(0, Math.min(200, Number(req.body?.count || 40)));
  const base = state.pages.length;
  for (let index = 1; index <= count; index += 1) {
    state.pages.push({
      id: 1000 + base + index,
      slug: `padded-${String(index).padStart(3, "0")}`,
      title: `Padded page ${index}`,
      client_id: null,
      workspace_id: null,
      workspace_name: null,
      theme_id: null,
      theme_name: "flag",
      require_approval: false,
      disabled: false,
      published_version_id: 9000 + index,
      has_password: false,
      is_live: true,
      created_at: iso(9000),
      updated_at: iso(index),
      freshness: null,
    });
  }
  res.json({ ok: true, pages: state.pages.length });
});
app.post("/__fixture/fail-next-pages", (_req, res) => { state.failNextPages = true; res.json({ ok: true }); });
app.post("/__fixture/delay-next-pages", (req, res) => { state.delayNextPages = Number(req.body?.milliseconds || 500); res.json({ ok: true }); });
app.post("/__fixture/delay-portal-detail", (req, res) => {
  portalDetailDelay[String(req.body?.id ?? "")] = Number(req.body?.milliseconds || 500);
  res.json({ ok: true });
});

app.use("/shell-assets/flag", express.static(path.join(__dirname, "..", "..", "public", "assets", "flag"), { index: false }));
// The content host serves the same vendored files from its own mount, and the
// standalone chrome links them from there.
app.use("/assets/flag", express.static(path.join(__dirname, "..", "..", "public", "assets", "flag"), { index: false }));
app.use("/shell-assets", express.static(path.join(__dirname, "..", "..", "public", "shell-assets"), { index: false }));
// Mounted with the SAME headers the content host applies in server.js, so the
// gate pages below are rendered by a real browser against a real asset CSP. With
// a bare mount, a CSP that blocked the Flag stylesheet or the @font-face would
// have passed every test in this suite.
const ASSET_CSP =
  "default-src 'none'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; base-uri 'none'; form-action 'none'";
app.use(
  "/assets",
  express.static(path.join(__dirname, "..", "..", "public", "assets"), {
    index: false,
    setHeaders(res) {
      res.setHeader("Content-Security-Policy", ASSET_CSP);
      res.setHeader("Referrer-Policy", "no-referrer");
      res.setHeader("X-Content-Type-Options", "nosniff");
      res.setHeader("X-Robots-Tag", "noindex, noarchive");
    },
  })
);

app.get("/admin", (_req, res) => res.type("html").send(welcomeShell.render("qa-admin@elcanotek.com", `http://127.0.0.1:${PORT}/live`, { csrf: "fixture-csrf", compose: true })));
// Registered before /admin/{*slug}, exactly as server.js does it — otherwise
// "templates" is read as a page slug and the library never renders.
app.get("/admin/templates", (_req, res) =>
  res.type("html").send(templateShell.render("fixture-csrf", "qa-admin@elcanotek.com", `http://127.0.0.1:${PORT}/live`))
);

app.get("/admin/portals", (_req, res) =>
  res.type("html").send(portalShell.render("fixture-csrf", "qa-admin@elcanotek.com", `http://127.0.0.1:${PORT}/live`))
);

app.get("/admin/{*slug}", (req, res) => {
  const slug = Array.isArray(req.params.slug) ? req.params.slug.join("/") : req.params.slug;
  res.type("html").send(adminShell.render(slug, "fixture-csrf", "qa-admin@elcanotek.com", `http://127.0.0.1:${PORT}/live`));
});

// ── partner portals: the admin JSON surface, in memory ──────────────────────
// Enough state to drive the real screen: one portal whose membership covers every
// warning the UI can show, plus a staff-only page that is NOT yet a member — the
// reclassification notice is the thing most worth seeing in a browser, because it
// is the sentence that stands between an admin and publishing a confidential
// dashboard to a partner.
const fixturePortals = [
  {
    id: 7,
    slug: "nwm",
    name: "Northwind Media Group",
    home_page_id: 71,
    home_page_slug: "nwm-client-overview",
    page_count: 2,
    created_at: iso(4000),
    updated_at: iso(12),
    url: `http://127.0.0.1:${PORT}/live/portal/nwm`,
  },
  // A second portal, so the screen has something to switch BETWEEN: the selected
  // portal is route state and the detail request is racy, and neither can be
  // shown with a list of one.
  {
    id: 9,
    slug: "fabrikam",
    name: "Fabrikam",
    home_page_id: null,
    home_page_slug: null,
    page_count: 1,
    created_at: iso(3000),
    updated_at: iso(30),
    url: `http://127.0.0.1:${PORT}/live/portal/fabrikam`,
  },
];
const nwmMembers = () => [
  // One of each state the screen can report: a design that renders its own menu,
  // one that gets the built-in control (and was staff-only before this portal), and
  // a raw page — which since #125 gets the injected menu like everything else,
  // so the only warning its row earns is the takedown.
  { page_id: 71, slug: "nwm-client-overview", title: "NWM Client Overview", label: "Portfolio overview", display_title: "Portfolio overview", sort_order: 0, added_at: iso(4000), has_password: true, disabled: false, published: true, page_deleted: false, shows_switcher: true, switcher_is_own: true, render_mode: "themed" },
  { page_id: 72, slug: "nwm/contoso-allergex", title: "Contoso Allergex", label: null, display_title: "Contoso Allergex", sort_order: 1, added_at: iso(2000), has_password: false, disabled: false, published: true, page_deleted: false, shows_switcher: true, switcher_is_own: false, render_mode: "themed" },
  { page_id: 73, slug: "nwm-taken-down", title: "Taken down", label: null, display_title: "Taken down", sort_order: 2, added_at: iso(900), has_password: true, disabled: true, published: true, page_deleted: false, shows_switcher: true, switcher_is_own: false, render_mode: "raw" },
];
const fabrikamMembers = () => [
  { page_id: 91, slug: "fabrikam-ssp-weekly", title: "Fabrikam SSP weekly", label: null, display_title: "Fabrikam SSP weekly", sort_order: 0, added_at: iso(3000), has_password: true, disabled: false, published: true, page_deleted: false, shows_switcher: true, switcher_is_own: true, render_mode: "themed" },
];
// Membership is mutable here: /pages/update really applies the sort_order it is
// sent, because the whole point of #173's controls is that the list comes back in
// a different order — a stub that echoed one member proved only that a request
// was made. resetPortalMembers puts the fixtures back so a spec that reorders
// does not decide what the next spec sees.
let membersByPortal = {};
function resetPortalMembers() {
  membersByPortal = { 7: nwmMembers(), 9: fabrikamMembers() };
}
resetPortalMembers();

// The same ORDER BY lib/portals.get uses, so the browser sees the order the real
// screen would get back: curated position first, then a stable tiebreak.
function sortMembers(members) {
  members.sort((a, b) =>
    a.sort_order - b.sort_order ||
    a.display_title.toLowerCase().localeCompare(b.display_title.toLowerCase()) ||
    a.slug.localeCompare(b.slug));
  return members;
}
// One-shot per-portal delay, so a spec can make an EARLIER detail request land
// after a later one — the stale-response race, deterministically.
const portalDetailDelay = {};

// The drift the link audit exists for: the home page links these live pages, but
// nobody added them as members — a partner following the link loses the nav.
// TWO of them, because "Add all N" only exists over more than one row, and one
// of the two has no password of its own, so adding it really does reclassify a
// staff-only page and the screen has something true to confirm afterwards.
const fixtureLinkedPages = [
  { page_id: 81, slug: "nwm-lakeside", title: "Lakeside campaign", has_password: true },
  { page_id: 82, slug: "nwm-mars-petcare", title: "Tailspin Pet Q3", has_password: false },
];

// What lib/portals.linkAudit actually sends: id, slug, title — and only the ones
// that are not already members. has_password is NOT in the payload; whether an
// add reclassified anything is reported by the ADD response, which is the whole
// reason the screen can only say it afterwards. Recomputed per request rather
// than frozen, so a spec can assert that adding a linked page clears its row.
function fixtureLinkAudit(portalId) {
  const members = membersByPortal[String(portalId)] || [];
  return {
    scanned: true,
    missing: fixtureLinkedPages
      .filter((page) => !members.some((member) => String(member.page_id) === String(page.page_id)))
      .map((page) => ({ page_id: page.page_id, slug: page.slug, title: page.title })),
  };
}

// The real add-a-member route takes any page by slug. The link audit's own pages
// exist but are deliberately NOT in state.pages, which the index specs own and
// count — so resolving only against state.pages made "Add to portal" answer 404,
// and a spec asserting only the outbound request body never noticed.
function fixturePageBySlug(slug) {
  const page = state.pages.find((candidate) => candidate.slug === slug);
  if (page) return page;
  const linked = fixtureLinkedPages.find((candidate) => candidate.slug === slug);
  return linked ? { id: linked.page_id, slug: linked.slug, title: linked.title, has_password: linked.has_password } : null;
}

// The portal fixture is module state on a server the whole run shares, and these
// specs create and retire portals. A spec that cares what the list holds resets
// it first rather than depending on what ran before it.
const portalSeed = JSON.parse(JSON.stringify(fixturePortals));
// One endpoint restores BOTH halves of the portal fixtures — the list and each
// portal's membership. They were briefly two handlers on the same route name,
// where Express matched the first and the second was dead code, so the global
// beforeEach that calls this quietly stopped resetting the list.
app.post("/__fixture/reset-portals", (_req, res) => {
  fixturePortals.length = 0;
  JSON.parse(JSON.stringify(portalSeed)).forEach((portal) => fixturePortals.push(portal));
  resetPortalMembers();
  res.json({ ok: true });
});
// One-shot delay on the LIST, so a spec can hold a reload's list response open
// and click a row while it is in flight — the race that used to discard it.
let portalListDelay = 0;
app.post("/__fixture/delay-portal-list", (req, res) => {
  portalListDelay = Number(req.body?.milliseconds || 500);
  res.json({ ok: true });
});
app.get("/api/v1/admin/portals", (_req, res) => {
  const delay = portalListDelay;
  portalListDelay = 0;
  // The body is built when it is SENT, so a delayed list still reports whatever
  // the mutation that triggered the reload did.
  if (delay) return setTimeout(() => res.json({ portals: fixturePortals }), delay);
  return res.json({ portals: fixturePortals });
});
app.get("/api/v1/admin/portals/:id", (req, res) => {
  const id = String(req.params.id);
  const portal = fixturePortals.find((candidate) => String(candidate.id) === id);
  // The real route 404s an id it does not hold (lib/portals.js: portal_not_found).
  // Answering 200-with-the-first-portal instead hid exactly the bug class this
  // screen has — a stale id rendering somebody else's detail.
  if (!portal) return res.status(404).json({ error: "portal not found", code: "portal_not_found" });
  const body = {
    portal,
    members: membersByPortal[id] || [],
    link_audit: id === "7" ? fixtureLinkAudit(id) : { scanned: true, missing: [] },
  };
  const delay = portalDetailDelay[id];
  if (delay) {
    delete portalDetailDelay[id];
    return setTimeout(() => res.json(body), delay);
  }
  return res.json(body);
});
app.post("/api/v1/admin/portals", (req, res) => {
  const { slug, name } = req.body || {};
  if (!slug || !name) return res.status(400).json({ error: "portal name is required", code: "portal_name_required" });
  const portal = {
    id: 8, slug, name, home_page_id: null, home_page_slug: null, page_count: 0,
    created_at: iso(0), updated_at: iso(0), url: `http://127.0.0.1:${PORT}/live/portal/${slug}`,
  };
  fixturePortals.push(portal);
  return res.status(201).json({ portal, password: "pr7k-9mfx-t2qd-w4hn", password_generated: true });
});
app.post("/api/v1/admin/portals/:id/pages", (req, res) => {
  const id = String(req.params.id);
  const body = req.body || {};
  const page = fixturePageBySlug(body.slug);
  if (!page) return res.status(404).json({ error: "page not found", code: "page_not_found" });
  // The added member is KEPT, at the sort_order it was sent, and the list is
  // re-sorted. While this echoed a hard sort_order: 0 and persisted nothing, a
  // spec could only assert the outbound request body — and "appended behind three
  // members" then passed twice in a row for the wrong reason, because the first
  // add never took effect. It also could not read 4 the moment the handler became
  // honest, which is the coupling this removes.
  const members = membersByPortal[id] || (membersByPortal[id] = []);
  const member = {
    page_id: page.id, slug: page.slug, title: page.title, label: body.label || null,
    display_title: body.label || page.title, sort_order: Number(body.sort_order) || 0,
    added_at: iso(0), has_password: !!page.has_password, disabled: false, published: true,
    page_deleted: false, shows_switcher: true, switcher_is_own: false, render_mode: "themed",
  };
  members.push(member);
  sortMembers(members);
  return res.status(201).json({
    member,
    portal_id: Number(id),
    reclassifies_staff_only: !page.has_password,
  });
});
app.post("/api/v1/admin/portals/:id/pages/update", (req, res) => {
  const id = String(req.params.id);
  const body = req.body || {};
  const members = membersByPortal[id] || [];
  const member = members.find((candidate) => String(candidate.page_id) === String(body.page_id));
  if (!member) return res.status(404).json({ error: "not in this portal", code: "portal_page_not_found" });
  // Only sort_order is applied. A label is echoed back but not kept: the spec
  // that presses Enter in the Edit dialog renames Contoso Allergex, and a fixture
  // that remembered it would rename the row for every spec that follows.
  if (Object.prototype.hasOwnProperty.call(body, "sort_order")) member.sort_order = Number(body.sort_order) || 0;
  sortMembers(members);
  return res.json({ member, portal_id: Number(id) });
});
app.post("/api/v1/admin/portals/:id/pages/remove", (_req, res) => res.json({ portal_id: 7, removed: true, home_cleared: false }));
app.post("/api/v1/admin/portals/:id/home", (_req, res) => res.json({ portal: fixturePortals[0] }));
app.post("/api/v1/admin/portals/:id/rename", (req, res) => {
  fixturePortals[0].name = (req.body || {}).name || fixturePortals[0].name;
  return res.json({ portal: fixturePortals[0] });
});
app.post("/api/v1/admin/portals/:id/password", (_req, res) =>
  res.json({ portal: fixturePortals[0], password: "zc4m-8kdq-r3vp-h7ns", password_generated: true })
);
app.post("/api/v1/admin/portals/:id/delete", (req, res) => {
  // Retire the NAMED portal, not the whole table: with a second portal in the
  // fixture, wiping the array made one Retire look like every portal vanishing
  // and left every later detail request answering with undefined.
  const id = String(req.params.id);
  const index = fixturePortals.findIndex((candidate) => String(candidate.id) === id);
  if (index < 0) return res.status(404).json({ error: "portal not found", code: "portal_not_found" });
  const [removed] = fixturePortals.splice(index, 1);
  const members = membersByPortal[id] || [];
  return res.json({ portal: { id: removed.id, slug: removed.slug, name: removed.name, deleted: true, member_count: members.length } });
});

app.get("/preview/:id", (req, res) => res.type("html").send(`<!doctype html><html><head><title>Preview ${req.params.id}</title></head><body><main><h1>Fixture preview #${req.params.id}</h1><p>Sandboxed source preview.</p></main></body></html>`));
app.get("/view/{*slug}", (req, res) => res.type("html").send(`<!doctype html><title>Live fixture</title><h1>Live ${req.params.slug.join("/")}</h1>`));
app.get("/live/{*slug}", (req, res) => res.type("html").send(`<!doctype html><title>Client fixture</title><h1>Client ${req.params.slug.join("/")}</h1>`));

app.get("/gate/password", (_req, res) => res.status(401).type("html").send(contentview.gatePage({ slug: "gate/password", showForm: true })));
app.get("/gate/incorrect", (_req, res) => res.status(401).type("html").send(contentview.gatePage({ slug: "gate/incorrect", message: "Incorrect password.", showForm: true })));
app.get("/gate/staff", (_req, res) => res.status(403).type("html").send(contentview.gatePage({ slug: "gate/staff", showForm: false })));
app.get("/gate/404", (_req, res) => res.status(404).type("html").send(contentview.notFoundPage()));
app.get("/gate/rate-limit", (_req, res) => res.status(429).type("html").send(contentview.rateLimitPage({ slug: "gate/rate-limit", minutes: 15 })));
app.get("/gate/busy", (_req, res) => res.status(429).type("html").send(contentview.busyPage({ slug: "gate/busy" })));
app.get("/gate/error", (_req, res) => res.status(500).type("html").send(contentview.serverErrorPage()));
app.get("/gate/expired", (_req, res) => res.status(403).type("html").send(contentview.expiredLinkPage()));

// The shipped campaign template, rendered through the REAL render path, with and
// without a switcher payload. This is the end of the chain the whole feature is
// for: an injected #pages-nav block becoming a Page menu a partner can click. It
// uses the actual template file, so a design change that breaks the control fails
// here rather than in front of Mandy.
const SHIPPED_TEMPLATE = fs.readFileSync(
  path.join(__dirname, "..", "..", "templates", "nwm-campaign-dashboard", "template.html"),
  "utf8"
);
const FIXTURE_NAV = {
  // url and home are what let the menu reach the portal index and say which page
  // the partner's own index puts first (#162).
  portal: { slug: "nwm", name: "Northwind Media Group", url: "/live/portal/nwm" },
  pages: [
    { slug: "nwm-client-overview", title: "Portfolio overview", url: "/live/nwm-client-overview", current: false, home: true },
    { slug: "nwm/contoso-allergex", title: "Contoso — Allergex always-on", url: "/live/nwm/contoso-allergex", current: true },
    { slug: "nwm-zeta-q3", title: "Zeta Q3", url: "/live/nwm-zeta-q3", current: false },
    // A sibling's title is set by whoever owns THAT page, so one of these is
    // markup. escapedJson keeps it inside the block; textContent is what keeps it
    // out of the DOM.
    { slug: "nwm-hostile", title: "Zeta <img src=x onerror=alert(1)> Q4", url: "/live/nwm-hostile", current: false },
  ],
  truncated: false,
};
// The truncation row only renders when the payload says the list was cut, which
// buildNav does at NAV_MAX_ENTRIES or the byte ceiling — neither reachable with
// four fixture pages.
app.get("/dashboard/with-switcher", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: SHIPPED_TEMPLATE, render_mode: "themed", nav: FIXTURE_NAV })));
app.get("/dashboard/no-switcher", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: SHIPPED_TEMPLATE, render_mode: "themed" })));
// An OLD dashboard: no switcher code of its own, exactly like every page that
// existed before the feature shipped. Pages must supply the control itself, or a
// partner reaching this page has no way onward but the portal index.
const LEGACY_DASHBOARD = `<!doctype html><html><head><title>Legacy dashboard</title>
<style>body{margin:0;font-family:system-ui}main{padding:28px}table{border-collapse:collapse}td,th{border:1px solid #ccd;padding:6px 10px}</style>
</head><body><main><h1>Q3 delivery</h1><table><tr><th>Deal</th><th>Revenue</th></tr>
<tr><td>Awareness · Display</td><td>$412,300</td></tr><tr><td>Consideration · OLV</td><td>$338,150</td></tr></table></main></body></html>`;
// A theme that tries to close the element it is injected into (#189). Themes are
// staff-curated, so this shape only ever arrives here — but "only staff write it"
// is the assumption that stops being true, and a real browser is the only thing
// that can prove the escaping holds.
const HOSTILE_THEME = `:root{--pwn:1}\n.a::after{content:"</style><script>window.__pwned=1</script>"}`;
app.get("/dashboard/hostile-theme", (_req, res) =>
  res.type("html").send(render.renderVersion({
    html: LEGACY_DASHBOARD, render_mode: "themed", override_css: HOSTILE_THEME })));
app.get("/dashboard/legacy-with-portal", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: LEGACY_DASHBOARD, render_mode: "themed", nav: FIXTURE_NAV })));
// The truncation row only renders when the payload says the list was cut, which
// buildNav does at NAV_MAX_ENTRIES or the byte ceiling — neither reachable with
// four fixture pages.
app.get("/dashboard/legacy-truncated", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: LEGACY_DASHBOARD, render_mode: "themed", nav: { ...FIXTURE_NAV, truncated: true } })));
app.get("/dashboard/legacy-no-portal", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: LEGACY_DASHBOARD, render_mode: "themed" })));
// A legacy dashboard in a portal that holds only this one page: there is nowhere
// to switch to, so the built-in control must not appear at all.
// A RAW dashboard — its own fonts and colours, no Flag. 18 of 31 live pages are
// like this, so "raw gets no Page menu" meant most of a partner's set was a dead
// end. It must gain the menu and lose none of its own styling.
const RAW_DASHBOARD = `<!doctype html><html><head><title>Bespoke raw</title>
<style>:root{--brand:#7a1f3d}body{margin:0;font-family:Georgia,serif;background:#fffaf5;color:#2b1b22}
h1{font-family:Georgia,serif;color:var(--brand);margin:0 0 18px}main{padding:30px}
.kpi{font-size:40px;font-weight:700;color:var(--brand)}</style>
</head><body><main><h1>Harbor Sun · flight to date</h1><div class="kpi">$918,400</div>
<p>Bespoke layout, deliberately not Flag.</p></main></body></html>`;
app.get("/dashboard/raw-with-portal", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: RAW_DASHBOARD, render_mode: "raw", nav: FIXTURE_NAV })));
app.get("/dashboard/raw-no-portal", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: RAW_DASHBOARD, render_mode: "raw" })));

// A dark bespoke report. The light one above is what let a white pill ship: every
// token fallback in the old sheet fired at once on a raw page, and the fixture
// never had a page dark enough to show it (#163).
const RAW_DARK_DASHBOARD = `<!doctype html><html><head><title>Bespoke dark</title>
<style>body{margin:0;font-family:Georgia,serif;background:#101014;color:#f4f1ea}
h1{margin:0 0 18px;color:#e8c37a}main{padding:30px}</style>
</head><body><main><h1>Night desk · flight to date</h1><p>Deliberately dark.</p></main></body></html>`;
app.get("/dashboard/raw-dark", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: RAW_DARK_DASHBOARD, render_mode: "raw", nav: FIXTURE_NAV })));

// A design that wants the control somewhere else says so with the two custom
// properties, rather than by fighting it with CSS it cannot reach.
const RAW_RELOCATED = RAW_DASHBOARD.replace(":root{--brand:#7a1f3d}", ":root{--brand:#7a1f3d;--pages-nav-top:64px;--pages-nav-right:40px}");
app.get("/dashboard/raw-relocated", (_req, res) =>
  res.type("html").send(render.renderVersion({ html: RAW_RELOCATED, render_mode: "raw", nav: FIXTURE_NAV })));

app.get("/dashboard/legacy-one-page", (_req, res) =>
  res.type("html").send(render.renderVersion({
    html: LEGACY_DASHBOARD,
    render_mode: "themed",
    nav: { ...FIXTURE_NAV, pages: [FIXTURE_NAV.pages[1]] },
  })));

app.get("/dashboard/one-page", (_req, res) =>
  res.type("html").send(render.renderVersion({
    html: SHIPPED_TEMPLATE,
    render_mode: "themed",
    nav: { ...FIXTURE_NAV, pages: [FIXTURE_NAV.pages[1]] },
  })));

// The partner portal index, in its three real states. It is the surface a partner
// actually lands on, so it goes through the same scriptless/branded/axe-clean/
// no-overflow bar as every other public gate — and its unlocked state is the one
// page on this host that renders a LIST of links, which is exactly the shape that
// overflows at 320px if nobody checks.
const FIXTURE_PORTAL = { slug: "nwm", name: "Northwind Media Group" };
app.get("/gate/portal", (_req, res) =>
  res.status(401).type("html").send(contentview.portalIndexPage({ portal: FIXTURE_PORTAL, showForm: true })));
app.get("/gate/portal-incorrect", (_req, res) =>
  res.status(401).type("html").send(contentview.portalIndexPage({ portal: FIXTURE_PORTAL, showForm: true, message: "Incorrect password." })));
// The timestamps are computed from a FIXED "now" that is also handed to the
// renderer, so "3 days ago" is the same string on every run and in every
// timezone — a fixture that drifts with the wall clock cannot pin copy.
const PORTAL_NOW = Date.parse("2026-08-27T09:00:00Z");
const daysBefore = (days) => new Date(PORTAL_NOW - days * 86400000).toISOString();
app.get("/gate/portal-open", (_req, res) =>
  res.status(200).type("html").send(contentview.portalIndexPage({
    portal: FIXTURE_PORTAL,
    now: PORTAL_NOW,
    pages: [
      // Each row carries a different freshness shape, because they render
      // differently: a data envelope, a plain publish, and nothing at all.
      { slug: "nwm-client-overview", title: "Portfolio overview", is_home: true, source_as_of: daysBefore(0.25), published_at: daysBefore(0.2) },
      { slug: "nwm/contoso-allergex-always-on", title: "Contoso — Allergex always-on performance, quarter to date", is_home: false, source_as_of: daysBefore(3), published_at: daysBefore(3) },
      { slug: "nwm-zeta-q3", title: "Zeta Q3", is_home: false, source_as_of: null, published_at: daysBefore(19) },
    ],
  })));
app.get("/gate/portal-empty", (_req, res) =>
  res.status(200).type("html").send(contentview.portalIndexPage({ portal: FIXTURE_PORTAL, pages: [] })));
app.get("/dashboard-missing", (_req, res) => res.status(404).type("html").send(errorShell.notFound()));

// ── sandbox navigation spike (test/browser/sandbox-nav.spec.js) ─────────────
// The partner page switcher rests on one unverified browser behaviour: can a
// TOP-LEVEL document served with `Content-Security-Policy: sandbox
// allow-scripts …` navigate ITSELF, and does the target's SameSite=Lax cookie
// ride along when the initiator is an opaque origin? These routes use the REAL
// rawHeaders() and the REAL pagecookie code so the answer is about Pages, not
// about a synthetic fixture.
const { rawHeaders } = require("../../lib/csp");
const pagecookie = require("../../lib/pagecookie");
const auth = require("../../lib/auth");

const SANDBOX_PAGE_ID = 4242;

// Hands out a real page-session cookie, then bounces to the sandboxed source.
// Secure is off because the fixture is http://127.0.0.1.
app.get("/sandbox-nav/unlock", (_req, res) => {
  res.setHeader("Set-Cookie", pagecookie.sessionCookieHeader(SANDBOX_PAGE_ID, { secure: false, passwordHash: null }));
  res.redirect(302, "/sandbox-nav/source");
});

// The sandboxed document a partner is looking at. Anchor and button cover the
// two ways a switcher could navigate: a plain link, and location.assign from a
// handler (which is what the reference design's <button onClick> would compile
// to).
app.get("/sandbox-nav/source", (_req, res) => {
  res.set(rawHeaders()).type("html").send(
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sandbox source</title></head><body>
<h1>Sandbox source</h1>
<a id="anchor" href="/sandbox-nav/target?via=anchor">Anchor to sibling</a>
<a id="blank" href="/sandbox-nav/target?via=blank" target="_blank" rel="noopener">Anchor in a new tab</a>
<button id="assign" type="button">Assign to sibling</button>
<script>
  document.getElementById("assign").addEventListener("click", function () {
    location.assign("/sandbox-nav/target?via=assign");
  });
</script>
</body></html>`
  );
});

// The sibling dashboard. Reports, in the DOM, whether the real page-session
// cookie survived the navigation and still verifies.
app.get("/sandbox-nav/target", (req, res) => {
  const cookies = auth.parseCookies(req.headers.cookie);
  const token = cookies[pagecookie.cookieName(SANDBOX_PAGE_ID)];
  const verified = pagecookie.verifySession(token, SANDBOX_PAGE_ID, null);
  res.set(rawHeaders()).type("html").send(
    `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Sandbox target</title></head><body>
<h1 id="arrived">arrived</h1>
<p id="via">${String(req.query.via || "").replace(/[^a-z]/g, "")}</p>
<p id="cookie-present">${token ? "yes" : "no"}</p>
<p id="cookie-verified">${verified ? "yes" : "no"}</p>
</body></html>`
  );
});

app.use("/api/v1/admin", (req, res, next) => {
  const route = req.path;
  if (req.method === "GET" && route === "/pages") {
    if (state.failNextPages) {
      state.failNextPages = false;
      return res.status(503).json({ error: "fixture page list unavailable" });
    }
    if (state.delayNextPages) {
      const delay = state.delayNextPages;
      state.delayNextPages = 0;
      return setTimeout(() => res.json({ pages: pageRows(), workspaces: workspaceRows() }), delay);
    }
    return res.json({ pages: pageRows(), workspaces: workspaceRows() });
  }
  if (req.method === "GET" && route === "/workspaces") return res.json({ workspaces: workspaceRows() });

  // ── template library ──────────────────────────────────────────────────────
  if (req.method === "GET" && route === "/templates") return res.json({ templates: state.templates });
  if (req.method === "GET" && route.startsWith("/templates/")) {
    const name = decodeURIComponent(route.slice("/templates/".length));
    const found = state.templates.find((t) => t.name === name);
    if (!found) return res.status(404).json({ error: `template not found: ${name}`, code: "template_not_found" });
    return res.json(templateDetail(found));
  }
  if (req.method === "POST" && route === "/templates/validate") {
    return res.json(templateReport(req.body || {}));
  }
  if (req.method === "POST" && route === "/templates") {
    const report = templateReport(req.body || {});
    if (!report.contract_ok) {
      return res.status(422).json({ error: report.contract_error.message, code: report.contract_error.code });
    }
    const existing = state.templates.find((t) => t.name === report.name);
    if (existing) {
      existing.current_revision += 1;
      return res.json({ created: false, deduped: false, template: existing, revision: { revision: existing.current_revision } });
    }
    const created = {
      id: String(state.templates.length + 1),
      name: report.name,
      title: (req.body || {}).title || "",
      description: (req.body || {}).description || "",
      current_revision: 1,
      current_version_id: String(state.templates.length + 1),
      created_at: NOW,
      updated_at: NOW,
      config_schema_sha256: "c".repeat(64),
      data_schema_sha256: "d".repeat(64),
      page_count: 0,
    };
    state.templates.push(created);
    return res.status(201).json({ created: true, deduped: false, template: created, revision: { revision: 1 } });
  }
  if (req.method === "DELETE" && /^\/templates\/[^/]+$/.test(route)) {
    const name = decodeURIComponent(route.split("/")[2]);
    const template = state.templates.find((candidate) => candidate.name === name);
    if (!template) return res.status(404).json({ error: "template not found" });
    const built = templateDetail(template).pages.length;
    if (built > 0 && req.query.force !== "true") {
      return res.status(409).json({ error: `${built} pages were built from ${name}; pass force`, code: "template_has_pages" });
    }
    state.templates = state.templates.filter((candidate) => candidate.name !== name);
    return res.json({ template: name, deleted: true, pages_built: built });
  }

  if (req.method === "POST" && /^\/templates\/[^/]+\/preview-token$/.test(route)) {
    const name = decodeURIComponent(route.split("/")[2]);
    const template = state.templates.find((candidate) => candidate.name === name);
    if (!template) return res.status(404).json({ error: "template not found" });
    // Echo back the revision that was asked for, and that revision's own
    // example-data state — the shell labels the frame from both, so a fixture
    // that always answered "revision 1, has data" would hide a mislabelled frame.
    const asked = (req.body || {}).revision;
    const revision = asked === undefined || asked === null ? template.current_revision : Number(asked);
    const known = templateDetail(template).revisions.find((entry) => entry.revision === revision);
    return res.json({
      template: name,
      revision,
      render_mode: (req.body || {}).render_mode === "raw" ? "raw" : "themed",
      content_sha256: "e".repeat(64),
      has_sample_data: known ? Boolean(known.has_sample_data) : false,
      url: `http://127.0.0.1:${PORT}/preview/tpl-${encodeURIComponent(name)}-r${revision}`,
      expires_in_seconds: 300,
    });
  }

  if (req.method === "POST" && route === "/pages") {
    const body = req.body || {};
    const slug = String(body.slug || "").trim().toLowerCase();
    if (!slug) return res.status(400).json({ error: "slug is required" });
    if (state.pages.some((page) => page.slug === slug)) return res.status(409).json({ error: "slug already exists" });
    const workspace = state.workspaces.find((item) => String(item.id) === String(body.workspace_id));
    const page = {
      id: state.nextPageId++, slug, title: body.title || slug, client_id: null,
      workspace_id: workspace?.id || null, workspace_name: workspace?.name || null,
      theme_id: null, theme_name: "flag", require_approval: false, disabled: false,
      published_version_id: null, has_password: false, is_live: false,
      created_at: NOW, updated_at: NOW,
    };
    state.pages.unshift(page);
    state.details[slug] = { versions: [] };
    record(req, page);
    return res.status(201).json({ page });
  }

  if (req.method === "POST" && route === "/workspaces") {
    const workspace = { id: state.nextWorkspaceId++, name: String(req.body.name || "").trim(), created_at: NOW, updated_at: NOW };
    state.workspaces.push(workspace);
    state.events.push({ method: req.method, path: route, body: req.body || {} });
    return res.status(201).json({ workspace });
  }
  const renameWorkspace = route.match(/^\/workspaces\/(\d+)\/rename$/);
  if (req.method === "POST" && renameWorkspace) {
    const workspace = state.workspaces.find((item) => String(item.id) === renameWorkspace[1]);
    if (!workspace) return res.status(404).json({ error: "workspace not found" });
    workspace.name = String(req.body.name || "").trim();
    state.pages.filter((page) => String(page.workspace_id) === String(workspace.id)).forEach((page) => { page.workspace_name = workspace.name; });
    state.events.push({ method: req.method, path: route, body: req.body || {} });
    return res.json({ workspace });
  }
  const deleteWorkspace = route.match(/^\/workspaces\/(\d+)\/delete$/);
  if (req.method === "POST" && deleteWorkspace) {
    const index = state.workspaces.findIndex((item) => String(item.id) === deleteWorkspace[1]);
    if (index < 0) return res.status(404).json({ error: "workspace not found" });
    const [workspace] = state.workspaces.splice(index, 1);
    state.pages.filter((page) => String(page.workspace_id) === String(workspace.id)).forEach((page) => { page.workspace_id = null; page.workspace_name = null; });
    state.events.push({ method: req.method, path: route, body: req.body || {} });
    return res.json({ workspace });
  }

  if (req.method === "POST" && route === "/compose") {
    state.events.push({ method: req.method, path: route, body: req.body || {} });
    return res.json({ jobId: "fixture-job" });
  }
  if (req.method === "GET" && route === "/compose/fixture-job") {
    return res.json({ status: "done", slug: "cutlass-fixture", title: "Cutlass fixture", log: "audit complete\npublished" });
  }

  if (!route.startsWith("/pages/")) return next();
  const found = findPageFromPath(route.slice("/pages/".length));
  if (!found) return res.status(404).json({ error: "page not found" });
  const { page, suffix } = found;
  const details = state.details[page.slug] || (state.details[page.slug] = { versions: [] });
  const versions = details.versions;
  if (req.method === "GET" && suffix === "") return res.json(detailFor(page));
  // One version's full row, html included — what the source editor reads to open
  // a version that is not the live one.
  const versionRead = suffix.match(/^\/versions\/(\d+)$/);
  if (req.method === "GET" && versionRead) {
    const version = versions.find((item) => String(item.id) === versionRead[1]);
    if (!version) return res.status(404).json({ error: "version not found" });
    return res.json({ version });
  }
  if (req.method !== "POST") return res.status(405).json({ error: "method not allowed" });
  record(req, page);

  if (suffix === "/preview-token") return res.json({ url: `http://127.0.0.1:${PORT}/preview/${req.body.version_id}` });
  const versionAction = suffix.match(/^\/versions\/(\d+)\/(approve|reject)$/);
  if (versionAction) {
    const version = versions.find((item) => String(item.id) === versionAction[1]);
    if (!version) return res.status(404).json({ error: "version not found" });
    if (versionAction[2] === "approve") {
      version.status = "approved";
      version.reviewed_by = "qa-admin@elcanotek.com";
      version.reviewed_at = NOW;
      page.published_version_id = version.id;
      page.is_live = !page.disabled;
      if (!version.html) version.html = VERSION_HTML.replace("Published", `Approved ${version.id}`);
    } else {
      version.status = "rejected";
      version.reviewed_by = "qa-admin@elcanotek.com";
      version.reviewed_at = NOW;
    }
    return res.json({ version });
  }
  if (suffix === "/publish" || suffix === "/rollback") {
    const version = versions.find((item) => String(item.id) === String(req.body.version_id));
    if (!version) return res.status(404).json({ error: "version not found" });
    version.status = "approved";
    if (!version.html) version.html = VERSION_HTML.replace("Published", `Version ${version.id}`);
    page.published_version_id = version.id;
    page.is_live = !page.disabled;
    return res.json({ version });
  }
  if (suffix === "/workspace") {
    const workspace = state.workspaces.find((item) => String(item.id) === String(req.body.workspace_id));
    page.workspace_id = workspace?.id || null;
    page.workspace_name = workspace?.name || null;
    return res.json({ page });
  }
  if (suffix === "/title") { page.title = String(req.body.title || ""); return res.json(page); }
  if (suffix === "/theme") { page.theme_id = req.body.theme === "client-brand" ? 2 : null; page.theme_name = req.body.theme; return res.json(page); }
  if (suffix === "/approval") { page.require_approval = Boolean(req.body.require_approval); return res.json(page); }
  if (suffix === "/disable") { page.disabled = true; page.is_live = false; return res.json(page); }
  if (suffix === "/enable") { page.disabled = false; page.is_live = Boolean(page.published_version_id); return res.json(page); }
  if (suffix === "/password") { page.has_password = Boolean(req.body.password); return res.json(page); }
  if (suffix === "/delete") {
    state.pages = state.pages.filter((item) => item !== page);
    return res.json({ deleted: true });
  }
  if (suffix === "/deploy-source") {
    const version = {
      id: state.nextVersionId++, page_id: page.id,
      status: page.require_approval ? "pending" : "draft",
      render_mode: req.body.render_mode === "raw" ? "raw" : "themed",
      author: "qa-admin@elcanotek.com", source: "admin", note: req.body.note || null,
      reviewed_by: null, reviewed_at: null, created_at: NOW, html: req.body.html,
    };
    versions.unshift(version);
    return res.json({ version, deduped: false, published: false, gated: page.require_approval });
  }
  return res.status(404).json({ error: "fixture action not found" });
});

app.use((_req, res) => res.status(404).type("html").send(errorShell.notFound()));

app.listen(PORT, "127.0.0.1", () => process.stdout.write(`fixture server on ${PORT}\n`));
