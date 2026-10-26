// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Front-end unit tests: the pure halves of the admin's browser modules
// (public/shell-assets/*.js), the Flag motion kit, and the admin stylesheet.
// They need nothing but Node itself — no npm dependency and no server code — so
// they keep running whatever the server is written in:
//
//   node --test test/frontend.test.js
//
// Where a browser value must agree with the server's, the browser half is
// pinned here and the server half in Go (internal/fixture/frontend_pin_test.go).

const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

const pageSwitcher = require("../public/shell-assets/page-switcher");
const UI = require("../public/shell-assets/primitives");
const { filterWorkspacePages } = require("../public/shell-assets/welcome");

// ── admin page switcher: pure browser model ─────────────────────────────────

test("page switcher: deterministically orders pages and identifies active neighbors", () => {
  const pages = [
    { slug: "zulu", title: "Zulu", require_approval: true },
    { slug: "team/q2", title: "Alpha", published_version_id: 12, workspace_id: "7", workspace_name: "Acme" },
    { slug: "alpha-disabled", title: "Alpha", disabled: true, published_version_id: 4 },
    { slug: "draft", title: "Draft page" },
  ];
  const nav = pageSwitcher.model(pages, "team/q2");

  assert.deepEqual(nav.items.map((item) => item.slug), ["alpha-disabled", "team/q2", "draft", "zulu"]);
  assert.equal(nav.total, 4);
  assert.equal(nav.current.position, 2);
  assert.equal(nav.current.href, "/admin/team/q2", "nested slashes stay route segments");
  assert.equal(nav.current.workspaceId, "7", "workspace membership remains available to the picker");
  assert.equal(nav.current.workspaceName, "Acme");
  assert.equal(nav.previous.slug, "alpha-disabled");
  assert.equal(nav.next.slug, "draft");
  assert.match(nav.current.optionLabel, /^2\. Alpha — \/team\/q2 · live$/);
  assert.match(nav.items[0].optionLabel, /· disabled$/);
  assert.match(nav.items[3].optionLabel, /· approval$/);
  assert.deepEqual(pages.map((page) => page.slug), ["zulu", "team/q2", "alpha-disabled", "draft"], "does not mutate the API response");
});

test("page switcher: a long list keeps every title, slug, status, and active position", () => {
  const pages = Array.from({ length: 40 }, (_, i) => {
    const n = String(40 - i).padStart(2, "0");
    return {
      slug: `client/${n}`,
      title: `Client ${n} — a deliberately long dashboard title for native select overflow`,
      published_version_id: n === "20" ? 200 : null,
    };
  });
  const nav = pageSwitcher.model(pages, "client/20");

  assert.equal(nav.total, 40);
  assert.equal(nav.items.length, 40, "native select receives the entire scrollable list");
  assert.equal(nav.current.position, 20);
  assert.equal(nav.previous.slug, "client/19");
  assert.equal(nav.next.slug, "client/21");
  assert.ok(nav.items.every((item) => item.optionLabel.includes(`/${item.slug}`)));
  assert.match(nav.current.optionLabel, /deliberately long dashboard title.*· live$/);
});

test("page switcher: nested navigation encodes each slug segment, not the slash", () => {
  assert.equal(pageSwitcher.encodeSlugPath("agency/q2_report"), "agency/q2_report");
  assert.equal(pageSwitcher.adminPath("agency/q2_report"), "/admin/agency/q2_report");
  const empty = pageSwitcher.model(null, "missing");
  assert.equal(empty.total, 0);
  assert.equal(empty.current, null);
  assert.equal(empty.previous, null);
  assert.equal(empty.next, null);
});

// ── shared browser primitives ────────────────────────────────────────────────
// public/shell-assets/primitives.js is the one place the four admin screens may
// format a date, encode a slug, describe a failure, or talk to the admin API.
// The pure half is covered here; the DOM-returning half (field, runAction,
// loadingContent, setBusy) is covered by the Playwright suite on
// purpose — proving it here would mean adding jsdom, and this repo ships no
// build step and no new dependencies.

test("primitives: the module loads outside a browser and exports the shared surface", () => {
  // The UMD wrapper must keep touching `document` only inside function bodies,
  // or requiring it here (and therefore covering any of it) stops working.
  const expected = [
    "el", "icon",
    "bootstrap", "request", "requestScope", "describeError",
    "formatWhen", "timeWhen", "formatCount", "slugPath", "pathSegment",
    "field", "runAction",
    "loadingContent", "errorState", "emptyState",
    "toast", "loadFailed", "makeDialog", "confirmDialog", "credentialDialog", "setBusy", "keepingFocus", "pageHeader", "statTile", "statusChip", "statusDot", "copyText",
    "looksLikeBentoDeck",
  ];
  assert.deepEqual(Object.keys(UI).sort(), expected.slice().sort());
  for (const name of expected) assert.equal(typeof UI[name], "function", `${name} must be callable`);

  // `const { confirm } = UI` shadowed window.confirm in all four screens, so a
  // later `if (confirm("…"))` would have been a truthy Promise and always
  // passed. The export is confirmDialog, with NO alias: a dead `confirm` key
  // would re-admit exactly that.
  assert.equal(UI.confirm, undefined, "no confirm alias may exist");

  // With no bootstrap island (or no document at all) request() must still be
  // able to ask for a CSRF token.
  assert.deepEqual(UI.bootstrap(), {});
});

test("primitives: formatWhen answers \"how stale is this\" and never leaks a raw value", () => {
  const UI = require("../public/shell-assets/primitives.js");
  const now = Date.parse("2026-07-22T12:00:00.000Z");
  const ago = (ms, options = {}) =>
    UI.formatWhen(new Date(now - ms).toISOString(), { now, locale: "en-GB", timeZone: "UTC", ...options });

  // Relative while the arithmetic is the point...
  assert.equal(ago(30 * 1000), "just now");
  assert.equal(ago(5 * 60 * 1000), "5 minutes ago");
  assert.equal(ago(3 * 60 * 60 * 1000), "3 hours ago");
  assert.equal(ago(26 * 60 * 60 * 1000), "yesterday", "numeric:auto must say yesterday, not 1 day ago");
  assert.equal(ago(3 * 24 * 60 * 60 * 1000), "3 days ago");
  // ...absolute once it stops being. Nobody counts past a week.
  assert.equal(ago(12 * 24 * 60 * 60 * 1000), "10 Jul 2026");
  assert.equal(ago(7 * 24 * 60 * 60 * 1000), "15 Jul 2026", "the boundary is absolute, not '7 days ago'");
  // A timestamp in the future is clock skew, not a countdown.
  assert.equal(ago(-60 * 60 * 1000), "22 Jul 2026");

  // One fallback, not four.
  assert.equal(UI.formatWhen(null), "Never");
  assert.equal(UI.formatWhen(undefined), "Never");
  assert.equal(UI.formatWhen(""), "Never");
  assert.equal(UI.formatWhen(NaN), "Never");
  assert.equal(UI.formatWhen("not a date"), "Never", "an unparseable value must not render \"Invalid Date\"");
  assert.equal(UI.formatWhen(null, { fallback: "—" }), "—", "a call site may still override it");

  const iso = "2026-01-02T09:30:00.000Z";
  const options = { locale: "en-GB", timeZone: "UTC", style: "datetime" };
  const rendered = UI.formatWhen(iso, options);
  assert.equal(rendered, "2 Jan 2026, 09:30");
  assert.ok(!/\d\d:\d\d:\d\d/.test(rendered), "seconds are never useful here and are never rendered");
  assert.ok(!rendered.includes("T"), "a raw ISO string must never reach the page");
  assert.equal(UI.formatWhen(new Date(iso), options), rendered, "a Date instance formats identically");
  assert.equal(UI.formatWhen(Date.parse(iso), options), rendered, "an epoch number formats identically");
  assert.equal(UI.formatWhen(iso, { ...options, style: "date" }), "2 Jan 2026");
  // The clock alone, for a line that reports something that just happened and is
  // never re-rendered: "Saved · just now" goes stale where it stands, and the date
  // would spend the line restating today. #156's Settings rows are the caller.
  assert.equal(UI.formatWhen(iso, { ...options, style: "time" }), "09:30");
  assert.equal(UI.formatWhen(null, { ...options, style: "time" }), "Never", "the fallback still holds");
});

test("primitives: slugPath keeps nested slugs as path segments", () => {
  assert.equal(UI.slugPath("a/b"), "a/b", "a slash between slug segments is a route separator");
  assert.equal(UI.slugPath("client/q2 report"), "client/q2%20report");
  assert.equal(UI.slugPath("a#b?c"), "a%23b%3Fc");
  assert.equal(UI.slugPath("%"), "%25");
  assert.equal(UI.slugPath("ü"), "%C3%BC");
  assert.equal(UI.slugPath(null), "");
  assert.equal(UI.slugPath(undefined), "");
  assert.equal(UI.slugPath("/lead"), "/lead");
  assert.equal(UI.slugPath("trail/"), "trail/");
});

test("primitives: slugPath agrees with the page switcher's own copy", () => {
  // page-switcher.js loads before primitives on the detail screen and is
  // require()d directly by these tests, so it keeps encodeSlugPath. That is the
  // one deliberate duplicate in the tree; this table is what stops it drifting.
  for (const value of ["a", "a/b", "a b/c", "ä/#?", "", null]) {
    assert.equal(UI.slugPath(value), pageSwitcher.encodeSlugPath(value), `disagreed on ${JSON.stringify(value)}`);
  }
});

test("primitives: pathSegment escapes the slash slugPath preserves", () => {
  // Template names are one segment and are server-validated to contain no
  // slash; pushing one through slugPath would smuggle a path separator.
  assert.equal(UI.pathSegment("a/b"), "a%2Fb");
  assert.equal(UI.pathSegment("nwm-campaign-dashboard"), "nwm-campaign-dashboard");
  assert.equal(UI.pathSegment("a b"), "a%20b");
  assert.equal(UI.pathSegment(null), "");
  assert.notEqual(UI.pathSegment("a/b"), UI.slugPath("a/b"), "the two are not interchangeable");
});

test("primitives: describeError prefers the server's message, then the status", () => {
  assert.equal(UI.describeError(400, { error: "nope" }), "nope");
  assert.equal(UI.describeError(400, { message: "m" }), "m");
  assert.equal(UI.describeError(400, { error: "e", message: "m" }), "e", "error wins over message");
  assert.equal(UI.describeError(400, { error: "   " }), "Request failed (400)", "blank is not a message");
  assert.equal(UI.describeError(400, { error: { nested: true } }), "Request failed (400)", "a non-string falls through");
  assert.equal(UI.describeError(503, null), "Request failed (503)");
  assert.equal(UI.describeError(500, {}), "Request failed (500)");
  // The one intentional error-copy change in #161: templates.js used to toast
  // the first 400 characters of a non-JSON body. That would put an nginx error
  // page in a toast on three more screens; it lives on error.text now.
  assert.equal(
    UI.describeError(502, null, "<html><head><title>502 Bad Gateway</title></head></html>"),
    "Request failed (502)"
  );
});

test("primitives: formatCount groups digits", () => {
  assert.equal(UI.formatCount(1234567, "en-US"), "1,234,567");
  assert.equal(UI.formatCount(0, "en-US"), "0");
  assert.equal(UI.formatCount("42", "en-US"), "42");
});

// request() is exercised against a stubbed global fetch. The Playwright fixture
// server never checks CSRF, so a missing X-CSRF-Token header would keep every
// browser spec green while production returned 403 (lib/csrf.js) — these
// assertions are the only guard on that.
async function withFetchStub(handler, body) {
  const calls = [];
  const original = global.fetch;
  global.fetch = async (url, init) => {
    calls.push({ url, init });
    return handler(url, init);
  };
  try {
    return await body(calls);
  } finally {
    global.fetch = original;
  }
}

const jsonResponse = (status, payload, contentType = "application/json") => ({
  ok: status >= 200 && status < 300,
  status,
  headers: { get: () => contentType },
  text: async () => (typeof payload === "string" ? payload : JSON.stringify(payload)),
});

test("primitives: request infers the method, sends JSON both ways, and merges headers", async () => {
  await withFetchStub(() => jsonResponse(200, { pages: [1] }), async (calls) => {
    const read = await UI.request("/api/v1/admin/pages");
    assert.deepEqual(read, { pages: [1] });
    assert.equal(calls[0].init.method, "GET", "no body means a read");
    assert.equal(calls[0].init.credentials, "same-origin");
    assert.equal(calls[0].init.headers.Accept, "application/json");
    assert.equal(calls[0].init.headers["Content-Type"], undefined, "a GET must not claim a JSON body");
    assert.equal(calls[0].init.headers["X-CSRF-Token"], undefined, "GET is below the CSRF middleware");
    assert.equal(calls[0].init.body, undefined);

    await UI.request("/api/v1/admin/pages", { body: { slug: "a/b", title: "" } });
    assert.equal(calls[1].init.method, "POST", "a body means a write");
    assert.equal(calls[1].init.headers["Content-Type"], "application/json");
    assert.equal(calls[1].init.headers["X-CSRF-Token"], "");
    assert.equal(calls[1].init.body, '{"slug":"a/b","title":""}', "callers pass values; request stringifies");

    await UI.request("/x", { headers: { Accept: "text/plain", "X-Extra": "1" } });
    assert.equal(calls[2].init.headers.Accept, "text/plain", "an explicit header wins");
    assert.equal(calls[2].init.headers["X-Extra"], "1");
  });

  // An empty 200 must resolve to {} rather than null: three of the four screens
  // this replaces relied on `const { x } = await api(...)` not throwing.
  await withFetchStub(() => jsonResponse(200, ""), async () => {
    assert.deepEqual(await UI.request("/x", { body: {} }), {});
  });
  await withFetchStub(() => jsonResponse(200, "<html>not json</html>", "text/html"), async () => {
    assert.deepEqual(await UI.request("/x"), {});
  });
});

test("primitives: request sends CSRF on every mutation, including a bodyless DELETE", async () => {
  await withFetchStub(() => jsonResponse(200, { ok: true }), async (calls) => {
    await UI.request("/api/v1/admin/templates/nwm", { method: "DELETE" });
    assert.equal(calls[0].init.method, "DELETE");
    assert.equal(calls[0].init.headers["X-CSRF-Token"], "", "a DELETE is a mutation");
    assert.equal(calls[0].init.body, undefined, "no body was asked for, so none is sent");
    assert.equal(calls[0].init.headers["Content-Type"], undefined);

    // An explicit method always wins over inference: /enable and /disable are
    // POST-only routes that take no body at all.
    await UI.request("/api/v1/admin/pages/a/enable", { method: "POST" });
    assert.equal(calls[1].init.method, "POST");
    assert.equal(calls[1].init.headers["X-CSRF-Token"], "");

    await UI.request("/x", { body: {}, csrf: "token-from-caller" });
    assert.equal(calls[2].init.headers["X-CSRF-Token"], "token-from-caller");

    await UI.request("/x", { method: "HEAD" });
    assert.equal(calls[3].init.headers["X-CSRF-Token"], undefined, "HEAD is a read");
  });
});

test("primitives: a rejected request throws the documented error object", async () => {
  await withFetchStub(() => jsonResponse(409, { error: "stale", code: "conflict", expected: 4 }), async () => {
    const error = await UI.request("/api/v1/admin/pages/a/publish", { body: { version_id: 1 } })
      .then(() => null, (thrown) => thrown);
    assert.ok(error instanceof Error);
    assert.equal(error.message, "stale");
    assert.equal(error.status, 409);
    assert.equal(error.code, "conflict", "the machine key from lib/apierror.js survives for #160");
    assert.deepEqual(error.body, { error: "stale", code: "conflict", expected: 4 });
    assert.equal(error.method, "POST");
    assert.equal(error.url, "/api/v1/admin/pages/a/publish");
  });

  // A proxy's HTML error page must never become the toast text.
  const html = "<html><head><title>500</title></head><body>boom</body></html>";
  await withFetchStub(() => jsonResponse(500, html, "text/html"), async () => {
    const error = await UI.request("/x").then(() => null, (thrown) => thrown);
    assert.equal(error.message, "Request failed (500)");
    assert.equal(error.body, null, "unparseable means no body, not a fake one");
    assert.equal(error.text, html, "the raw text is still available for diagnosis");
    assert.equal(error.code, null);
  });

  await withFetchStub(() => { throw new TypeError("Failed to fetch"); }, async () => {
    const error = await UI.request("/x").then(() => null, (thrown) => thrown);
    assert.equal(error.status, 0);
    assert.equal(error.code, "network");
    assert.equal(error.message, "Failed to fetch");
    assert.ok(error.cause instanceof TypeError);
  });
});

test("primitives: requestScope binds a path prefix and nothing else", async () => {
  await withFetchStub(() => jsonResponse(200, { ok: true }), async (calls) => {
    const api = UI.requestScope("/api/v1/admin");
    await api("/templates");
    await api("/pages/a%2Fb/workspace", { body: { workspace_id: null } });
    const pageApi = UI.requestScope("/api/v1/admin/pages/team/q2");
    await pageApi("");
    assert.deepEqual(calls.map((call) => call.url), [
      "/api/v1/admin/templates",
      "/api/v1/admin/pages/a%2Fb/workspace",
      "/api/v1/admin/pages/team/q2",
    ]);
    assert.equal(calls[0].init.method, "GET");
    assert.equal(calls[1].init.method, "POST");
    assert.equal(calls[2].init.method, "GET", "the detail load is a read of the scope root");
  });
});

test("workspace index: renders All, selected, Ungrouped, and search views", () => {
  const pages = [
    { slug: "alpha", title: "Alpha report", workspace_id: "7", workspace_name: "Acme" },
    { slug: "beta", title: "Beta report", workspace_id: 8, workspace_name: "Bravo" },
    { slug: "loose", title: "Loose report", workspace_id: null, workspace_name: null },
  ];
  assert.deepEqual(filterWorkspacePages(pages, "all").map((p) => p.slug), ["alpha", "beta", "loose"]);
  assert.deepEqual(filterWorkspacePages(pages, "workspace:7").map((p) => p.slug), ["alpha"]);
  assert.deepEqual(filterWorkspacePages(pages, "workspace:8").map((p) => p.slug), ["beta"]);
  assert.deepEqual(filterWorkspacePages(pages, "ungrouped").map((p) => p.slug), ["loose"]);
  assert.deepEqual(filterWorkspacePages(pages, "all", "acme").map((p) => p.slug), ["alpha"]);
  assert.deepEqual(filterWorkspacePages(pages, "ungrouped", "loose").map((p) => p.slug), ["loose"]);
});

// ── refresh health: one vocabulary for the index and the page detail ─────────

const RefreshHealth = require("../public/shell-assets/refresh-health");

test("refresh health: only blocked, overdue and stuck sources need a human, worst first", () => {
  const row = (slug, health) => ({ slug, refresh_health: health ? { health, health_reason: `${slug} reason` } : undefined });
  const pages = [
    row("fine", "ok"), row("late-a", "overdue"), row("none", "unscheduled"), row("stuck", "source_stale"),
    row("legacy", null), row("failed", "blocked"), row("late-b", "overdue"),
  ];
  assert.deepEqual(RefreshHealth.attentionRows(pages).map((p) => p.slug), ["failed", "late-a", "late-b", "stuck"]);
  assert.deepEqual(pages.filter(RefreshHealth.needsAttention).map((p) => p.slug), ["late-a", "stuck", "failed", "late-b"]);
  // The words the screens print, and the tones: red for a failed run, amber for
  // late or stuck, nothing alarming for the rest.
  assert.deepEqual(RefreshHealth.describeHealth(row("x", "blocked")), { kind: "error", label: "Blocked", rank: 0, attention: true });
  assert.equal(RefreshHealth.describeHealth(row("x", "overdue")).kind, "warning");
  assert.equal(RefreshHealth.describeHealth(row("x", "source_stale")).label, "Source stuck");
  assert.equal(RefreshHealth.describeHealth({ health: "ok" }).label, "On schedule");
  assert.equal(RefreshHealth.describeHealth(row("x", "something_new")).label, "Not scheduled");
  assert.equal(RefreshHealth.describeHealth(null), null);
  assert.deepEqual(RefreshHealth.attentionRows(undefined), []);
});

test("refresh health: a live page with old data and no schedule is hinted, never counted as needing attention", () => {
  const row = (slug, health) => ({ slug, refresh_health: health });
  const hinted = (slug, days) => row(slug, { health: "unscheduled", unscheduled_stale: true, days_since_source: days });
  const pages = [
    hinted("old-a", 11), row("plain", { health: "unscheduled", unscheduled_stale: false, days_since_source: 3 }),
    hinted("older", 46), row("late", { health: "overdue", unscheduled_stale: false, days_since_source: 9 }),
    hinted("old-b", 11), row("legacy", undefined), row("nodata", { health: "unscheduled", unscheduled_stale: false, days_since_source: null }),
  ];
  // Oldest data first; the list's own order on a tie.
  assert.deepEqual(RefreshHealth.unscheduledStaleRows(pages).map((p) => p.slug), ["older", "old-a", "old-b"]);
  // The hint is beside the verdict, not in it: still "Not scheduled", and the
  // attention list is only the page with a schedule.
  assert.deepEqual(RefreshHealth.attentionRows(pages).map((p) => p.slug), ["late"]);
  assert.equal(RefreshHealth.needsAttention(hinted("x", 30)), false);
  assert.equal(RefreshHealth.describeHealth(hinted("x", 30)).label, "Not scheduled");
  assert.equal(RefreshHealth.unscheduledStale(hinted("x", 30)), true);
  assert.equal(RefreshHealth.unscheduledStale(pages[1]), false);
  assert.equal(RefreshHealth.unscheduledStale(pages[5]), false);
  // Grey, and the words the row's hover and the CLI both say.
  assert.deepEqual(RefreshHealth.NO_REFRESH, { kind: "neutral", label: "No refresh", filter: "No refresh set" });
  assert.equal(RefreshHealth.unscheduledStaleHint(hinted("x", 11)),
    "No refresh schedule · data 11 days old — set a schedule or ignore if the campaign has ended");
  assert.match(RefreshHealth.unscheduledStaleHint({ health: "unscheduled", unscheduled_stale: true, days_since_source: 1 }), /data 1 day old/);
  assert.match(RefreshHealth.unscheduledStaleHint(row("x", { health: "unscheduled", unscheduled_stale: true })), /data over a week old/);
  assert.equal(RefreshHealth.unscheduledStaleHint(pages[1]), "");
  assert.equal(RefreshHealth.unscheduledStaleHint(null), "");
  assert.deepEqual(RefreshHealth.unscheduledStaleRows(undefined), []);
});

test("refresh health: schedule presets round-trip through the cron the server stores", () => {
  const cases = [
    [{ preset: "none" }, null, "Not scheduled"],
    [{ preset: "daily", time: "14:30" }, "30 14 * * *", "Daily at 14:30 UTC"],
    [{ preset: "daily", time: "09:05" }, "5 9 * * *", "Daily at 09:05 UTC"],
    [{ preset: "weekdays", time: "15:00" }, "0 15 * * 1-5", "Weekdays at 15:00 UTC"],
    [{ preset: "weekly", time: "09:00", day: "1" }, "0 9 * * 1", "Mondays at 09:00 UTC"],
    [{ preset: "weekly", time: "00:00", day: "0" }, "0 0 * * 0", "Sundays at 00:00 UTC"],
    [{ preset: "custom", cron: "  */15   6-18 * * 1-5 " }, "*/15 6-18 * * 1-5", "*/15 6-18 * * 1-5 (cron, UTC)"],
  ];
  for (const [input, cron, words] of cases) {
    assert.deepEqual(RefreshHealth.cronFromPreset(input), { cron }, JSON.stringify(input));
    assert.equal(RefreshHealth.describeSchedule(cron), words);
    if (cron && input.preset !== "custom") {
      const back = RefreshHealth.presetFromCron(cron);
      assert.equal(back.preset, input.preset, cron);
      assert.equal(back.time, input.time, cron);
      if (input.preset === "weekly") assert.equal(back.day, input.day, cron);
    }
  }
  // Sunday written as 7 reads back as the same day.
  assert.equal(RefreshHealth.presetFromCron("0 9 * * 7").day, "0");
  assert.equal(RefreshHealth.describeSchedule("0 9 * * 7"), "Sundays at 09:00 UTC");
  // Anything a preset cannot say is custom, verbatim.
  assert.deepEqual(RefreshHealth.presetFromCron("0 9 1 * *"), { preset: "custom", time: "09:00", day: "1", cron: "0 9 1 * *" });
  assert.equal(RefreshHealth.presetFromCron("75 9 * * *").preset, "custom");
  assert.equal(RefreshHealth.presetFromCron(null).preset, "none");
  // What the form refuses before it asks the server.
  assert.match(RefreshHealth.cronFromPreset({ preset: "daily", time: "" }).error, /HH:MM/);
  assert.match(RefreshHealth.cronFromPreset({ preset: "daily", time: "24:00" }).error, /HH:MM/);
  assert.match(RefreshHealth.cronFromPreset({ preset: "weekly", time: "09:00", day: "9" }).error, /day/);
  assert.match(RefreshHealth.cronFromPreset({ preset: "custom", cron: "  " }).error, /cron/);
  assert.match(RefreshHealth.cronFromPreset({}).error, /schedule/);
});

// ── portal admin screen: what an admin is told about one membership ──────────

const { describeMember, portalIdFromSearch, resolveSelection,
  sortOrderWrites, describeMove, planMove, nextSortOrder, appendPlan, appendAllPlan,
  memberCaption, auditCaption, describeAdded, describePartialAdd,
  MAX_SORT_ORDER: BROWSER_MAX_SORT_ORDER } = require("../public/shell-assets/portals");

test("portal admin: a membership row's warnings say whether a partner can open it at all", () => {
  const live = { has_password: true, disabled: false, published: true, page_deleted: false, shows_switcher: true };
  assert.deepEqual(describeMember(live), { openable: true, warnings: [] }, "a healthy member needs no commentary");

  // The three states that make a member invisible on the partner's index. Each is
  // reported as blocking, because the admin's next action differs per case.
  const cases = [
    [{ ...live, page_deleted: true }, /deleted/i],
    [{ ...live, published: false }, /Nothing published/i],
    [{ ...live, disabled: true }, /Taken down/i],
  ];
  for (const [member, pattern] of cases) {
    const described = describeMember(member);
    assert.equal(described.openable, false, `${pattern} must not count as openable`);
    assert.match(described.warnings[0].text, pattern);
    assert.equal(described.warnings[0].kind, "blocked");
  }

  // A deleted page must not also be nagged about its password or its switcher:
  // there is exactly one thing to do about it, and three lines of advice on a dead
  // row is how a screen teaches people to ignore warnings.
  assert.equal(describeMember({ ...live, page_deleted: true, has_password: false, shows_switcher: false }).warnings.length, 1);

  // The reclassification, restated where an admin reviews an existing portal —
  // not only at the moment of adding.
  const reclassified = describeMember({ ...live, has_password: false });
  assert.equal(reclassified.openable, true, "it opens: the portal is what makes it readable");
  assert.ok(reclassified.warnings.some((w) => /No password of its own/.test(w.text)));

  // The switcher answer. Since #125 EVERY published member shows a Page menu
  // when opened through the portal — themed via the template/built-in control,
  // raw via the injected control — so no membership shape earns a menu warning
  // any more. The old "served raw, no Page menu, redeploy as themed" note was
  // telling admins to redeploy pages that are fine.
  assert.equal(
    describeMember({ ...live, shows_switcher: true, switcher_is_own: false }).warnings.length,
    0,
    "a themed dashboard needs no warning: it gets the built-in menu"
  );
  const rawMember = describeMember({ ...live, render_mode: "raw" });
  assert.equal(rawMember.openable, true, "a raw page still opens fine");
  assert.equal(rawMember.warnings.length, 0, "raw gets the injected menu (#125) — nothing to warn about");
});

// ── portal admin screen: the selected portal as route state ──────────────────

test("portal admin: the URL says which portal is selected, and only a real id counts", () => {
  assert.equal(portalIdFromSearch("?portal=7"), "7");
  assert.equal(portalIdFromSearch("portal=7&workspace=2"), "7");
  // Anything that is not a bare id is somebody else's query string or a
  // hand-edited one: fall back rather than request /portals/<junk>.
  for (const junk of ["", "?", "?portal=", "?portal=abc", "?portal=7a", "?portal=-1", "?portal=1.5", "?workspace=2"]) {
    assert.equal(portalIdFromSearch(junk), null, junk);
  }
  assert.equal(portalIdFromSearch(null), null);
  assert.equal(portalIdFromSearch(undefined), null);
});

test("portal admin: a selection that no longer exists falls back to the first portal", () => {
  const portals = [{ id: 7 }, { id: 9 }];
  // A string from the URL and a number from the API are the same selection.
  assert.equal(resolveSelection(portals, "9"), "9");
  assert.equal(resolveSelection(portals, 9), 9);
  // A retired portal still in a bookmark, or a stale history entry: the screen
  // shows the first portal instead of a list with no detail under it.
  assert.equal(resolveSelection(portals, "404"), 7);
  assert.equal(resolveSelection(portals, null), 7);
  // Nothing to select is a real state — the empty screen renders its own panel.
  assert.equal(resolveSelection([], "7"), null);
  assert.equal(resolveSelection(undefined, "7"), null);
});

// ── portal admin screen: membership order without a number (#173) ───────────

test("portal admin: a move renumbers by position, so a shared sort_order cannot stick", () => {
  const list = [
    { page_id: 71, display_title: "Portfolio overview", sort_order: 0 },
    { page_id: 72, display_title: "Contoso Fabrikam", sort_order: 1 },
    { page_id: 73, display_title: "Taken down", sort_order: 2 },
  ];

  const up = planMove(list, 72, "up");
  assert.deepEqual(up.members.map((member) => member.page_id), [72, 71, 73]);
  assert.equal(up.index, 0);
  // Only the two rows that actually moved are written, and each is numbered by
  // where it now sits.
  assert.deepEqual(up.writes, [{ page_id: 72, sort_order: 0 }, { page_id: 71, sort_order: 1 }]);
  // planMove must not mutate the list the screen is still rendering.
  assert.deepEqual(list.map((member) => member.page_id), [71, 72, 73]);

  const down = planMove(list, 72, "down");
  assert.deepEqual(down.members.map((member) => member.page_id), [71, 73, 72]);
  assert.deepEqual(down.writes, [{ page_id: 73, sort_order: 1 }, { page_id: 72, sort_order: 2 }]);

  // The defect the numeric field left behind: three rows sharing 0, ordered by
  // title, where no value an admin could type moved anything. A move renumbers
  // the whole list, so the first press is also the repair.
  const tied = [
    { page_id: 1, display_title: "Alpha", sort_order: 0 },
    { page_id: 2, display_title: "Beta", sort_order: 0 },
    { page_id: 3, display_title: "Gamma", sort_order: 0 },
  ];
  const repaired = planMove(tied, 3, "up");
  assert.deepEqual(repaired.members.map((member) => member.page_id), [1, 3, 2]);
  assert.deepEqual(repaired.writes, [{ page_id: 3, sort_order: 1 }, { page_id: 2, sort_order: 2 }]);
  assert.equal(repaired.announcement, "Gamma moved above Beta.");
  // …and a list already numbered by its own positions needs no writes at all.
  assert.deepEqual(sortOrderWrites(list), []);
  assert.deepEqual(sortOrderWrites([]), []);
  assert.deepEqual(sortOrderWrites(undefined), []);

  // A press that cannot go anywhere sends nothing: the ends of the list, and a
  // row a concurrent change has already taken out from under the click.
  assert.equal(planMove(list, 71, "up"), null);
  assert.equal(planMove(list, 73, "down"), null);
  assert.equal(planMove(list, 999, "up"), null);
  assert.equal(planMove(undefined, 71, "up"), null);
  // The id may arrive as a string from a dataset and as a number from the API.
  assert.equal(planMove(list, "72", "up").index, 0);
});

test("portal admin: a move announces itself, because nothing on the row shows the order", () => {
  const list = [
    { page_id: 71, display_title: "Portfolio overview" },
    { page_id: 72, display_title: "Contoso Fabrikam" },
    { page_id: 73, display_title: "Taken down" },
  ];
  // The ends are absolute: "first"/"last" is what an admin was checking for.
  assert.equal(describeMove(list, 0, "up"), "Portfolio overview is now first.");
  assert.equal(describeMove(list, 2, "down"), "Taken down is now last.");
  // In the middle there is no number to quote, so it names the neighbour the
  // press moved it past — which is what the person was aiming at.
  assert.equal(describeMove(list, 1, "up"), "Contoso Fabrikam moved above Taken down.");
  assert.equal(describeMove(list, 1, "down"), "Contoso Fabrikam moved below Portfolio overview.");
  // A one-member portal has no ends to reach, and must not claim a move.
  assert.equal(describeMove([{ page_id: 91, display_title: "Fabrikam SSP weekly" }], 0, "up"),
    "Fabrikam SSP weekly is the only page in this portal.");
  assert.equal(describeMove(list, 9, "up"), "", "a row that is no longer there says nothing");
  assert.equal(describeMove(undefined, 0, "up"), "");
});

test("portal admin: a dashboard added with no Order field lands last", () => {
  assert.equal(nextSortOrder([{ sort_order: 0 }, { sort_order: 1 }, { sort_order: 2 }]), 3);
  // Gaps and ties are both real: the value has to clear everything already there.
  assert.equal(nextSortOrder([{ sort_order: 0 }, { sort_order: 0 }]), 1);
  assert.equal(nextSortOrder([{ sort_order: 40 }, { sort_order: 2 }]), 41);
  assert.equal(nextSortOrder([]), 0);
  assert.equal(nextSortOrder(undefined), 0);
  // Above the API's ceiling there is no free number to append at, and null says
  // so. Clamping to 9999 — which this used to do, and this test used to pin —
  // appended straight into a tie with the row already holding 9999, which is the
  // shared sort_order the whole feature exists to remove.
  assert.equal(nextSortOrder([{ sort_order: 9999 }]), null);
  assert.equal(nextSortOrder([{ sort_order: 9998 }]), 9999, "the ceiling itself is still reachable");
  // The API normalizes a null/absent sort_order to 0 (lib/portals.normalizeSortOrder),
  // so a row carrying one already occupies position 0 and appending must clear it.
  // Anything that is not a number at all is ignored rather than poisoning the max.
  assert.equal(nextSortOrder([{ sort_order: null }, { sort_order: "x" }]), 1);
  assert.equal(nextSortOrder([{ sort_order: "x" }]), 0);
});

test("portal admin: appending at the ceiling renumbers instead of tying", () => {
  // The ordinary case costs no extra write at all.
  assert.deepEqual(appendPlan([{ page_id: 71, sort_order: 0 }, { page_id: 72, sort_order: 1 }]),
    { writes: [], sort_order: 2 });
  assert.deepEqual(appendPlan([]), { writes: [], sort_order: 0 });
  assert.deepEqual(appendPlan(undefined), { writes: [], sort_order: 0 });

  // At the ceiling the list is renumbered by position first — the same
  // renumbering a move does — so the new row has a free number to land on and no
  // two rows share one.
  assert.deepEqual(appendPlan([{ page_id: 71, sort_order: 9999 }]),
    { writes: [{ page_id: 71, sort_order: 0 }], sort_order: 1 });
  assert.deepEqual(appendPlan([{ page_id: 71, sort_order: 3 }, { page_id: 72, sort_order: 9999 }]),
    { writes: [{ page_id: 71, sort_order: 0 }, { page_id: 72, sort_order: 1 }], sort_order: 2 });
  // Whatever the plan is, the appended value never collides with a renumbered one
  // and never exceeds what the write will take.
  for (const list of [
    [{ page_id: 71, sort_order: 9999 }],
    [{ page_id: 71, sort_order: 0 }, { page_id: 72, sort_order: 9999 }],
    [{ page_id: 71, sort_order: 9999 }, { page_id: 72, sort_order: 9999 }],
  ]) {
    const plan = appendPlan(list);
    // Every position the existing list ENDS UP holding, not only the ones that
    // needed a write: sortOrderWrites emits the smallest set that renumbers by
    // position, so a row already sitting on its own index is absent from
    // plan.writes while still occupying that index. Deriving `taken` from the
    // writes alone left those positions out of the uniqueness check this loop
    // claims to make — for [{71,0},{72,9999}] it only ever checked position 1
    // (#209 review).
    const taken = plan.writes.length ? list.map((_, index) => index)
      : list.map((member) => member.sort_order);
    assert.equal(taken.includes(plan.sort_order), false, `appended into a tie: ${JSON.stringify(plan)}`);
    assert.ok(plan.sort_order <= BROWSER_MAX_SORT_ORDER);
  }
});

test("portal admin: adding several at once gives each its own position", () => {
  // The defect this exists for: appendPlan called once per page reads the same
  // state.detail.members every time — the screen does not reload between the
  // writes — so all N would be sent the SAME sort_order, and a shared position is
  // exactly what #173 removed. One plan, N consecutive numbers.
  assert.deepEqual(appendAllPlan([{ page_id: 71, sort_order: 0 }, { page_id: 72, sort_order: 1 }], 2),
    { writes: [], sort_orders: [2, 3] });
  assert.deepEqual(appendAllPlan([], 3), { writes: [], sort_orders: [0, 1, 2] });
  assert.deepEqual(appendAllPlan(undefined, 1), { writes: [], sort_orders: [0] });
  // Nothing to add asks for nothing, rather than for one row at the end.
  assert.deepEqual(appendAllPlan([{ page_id: 71, sort_order: 0 }], 0), { writes: [], sort_orders: [] });

  // No room above what is already there for ALL of them, so the list is
  // renumbered by position first — what appendPlan does for one — and they land
  // after it.
  assert.deepEqual(appendAllPlan([{ page_id: 71, sort_order: 9998 }], 3),
    { writes: [{ page_id: 71, sort_order: 0 }], sort_orders: [1, 2, 3] });
  // A list already numbered by position needs no writes even at the ceiling.
  assert.deepEqual(appendAllPlan([{ page_id: 71, sort_order: 9999 }], 1),
    { writes: [{ page_id: 71, sort_order: 0 }], sort_orders: [1] });

  // Whatever the plan, no proposed number repeats, none collides with a
  // renumbered row, and none exceeds what the write will take.
  for (const [list, count] of [
    [[], 4],
    [[{ page_id: 71, sort_order: 9999 }], 2],
    [[{ page_id: 71, sort_order: 0 }, { page_id: 72, sort_order: 9999 }], 3],
    [[{ page_id: 71, sort_order: 9999 }, { page_id: 72, sort_order: 9999 }], 1],
  ]) {
    const plan = appendAllPlan(list, count);
    assert.equal(plan.sort_orders.length, count);
    // Every position the existing list ENDS UP holding, not only the ones that
    // needed a write: sortOrderWrites emits the smallest set that renumbers by
    // position, so a row already sitting on its own index is absent from
    // plan.writes while still occupying that index. Deriving `taken` from the
    // writes alone left those positions out of the uniqueness check this loop
    // claims to make — for [{71,0},{72,9999}] it only ever checked position 1
    // (#209 review).
    const taken = plan.writes.length ? list.map((_, index) => index)
      : list.map((member) => member.sort_order);
    const all = [...taken, ...plan.sort_orders];
    assert.equal(new Set(all).size, all.length, `two rows would share a position: ${JSON.stringify(plan)}`);
    for (const order of plan.sort_orders) assert.ok(order >= 0 && order <= BROWSER_MAX_SORT_ORDER);
  }
});

test("portal admin: the link audit's caption names the table and its one action", () => {
  // A table's caption is its accessible name, and this table sits directly under
  // the membership table with the same two column headers — so the caption is the
  // only thing telling a screen reader which of the two it has landed on.
  assert.equal(auditCaption(1), "1 linked page is not in this portal.");
  assert.equal(auditCaption(3), "3 linked pages are not in this portal.");
  // It used to carry a second sentence — "Adding one is the same decision as Add
  // a dashboard, one click shorter" — which is the rationale for building the
  // screen, addressed to the next developer, and a caption is what a screen
  // reader announces on ENTRY. One sentence, naming the table (#209 review).
  for (const count of [1, 2, 7]) {
    assert.equal(auditCaption(count).split(".").filter(Boolean).length, 1,
      `the caption is the table's name, not prose: ${auditCaption(count)}`);
    assert.equal(/Add a page|one click/.test(auditCaption(count)), false);
  }
});

test("portal admin: an add made from the audit says what it did, by name", () => {
  // The row used to just vanish — no toast at all on the one click here that can
  // make a staff-only dashboard readable by everyone holding the password.
  assert.equal(describeAdded([{ reclassifies_staff_only: false, member: { display_title: "Woodgrove campaign" } }]),
    "Page added.");
  assert.equal(describeAdded([{ reclassifies_staff_only: true, member: { display_title: "Woodgrove campaign" } }]),
    "Page added. Woodgrove campaign is now readable with this portal's password.");

  // "Add all N" can reclassify some of the N and not the others, and the rows
  // that would have carried the news are gone by the time it arrives — so the
  // ones that changed are named, and only those.
  const mixed = describeAdded([
    { reclassifies_staff_only: false, member: { display_title: "Woodgrove campaign" } },
    { reclassifies_staff_only: true, member: { display_title: "Tailspin Pet Q3" } },
  ]);
  assert.equal(mixed, "Added 2 pages. Tailspin Pet Q3 is now readable with this portal's password.");
  assert.equal(/Woodgrove/.test(mixed), false, "a page that was already client-readable must not be reported as changed");

  const both = describeAdded([
    { reclassifies_staff_only: true, member: { display_title: "A" } },
    { reclassifies_staff_only: true, member: { display_title: "B" } },
    { reclassifies_staff_only: true, member: { display_title: "C" } },
  ]);
  assert.equal(both, "Added 3 pages. A, B and C are now readable with this portal's password.");
  assert.equal(describeAdded([{ reclassifies_staff_only: false }, { reclassifies_staff_only: false }]),
    "Added 2 pages.");
  // A response shape that surprises us must still produce a sentence, not a crash
  // or the word "undefined" in a live region.
  assert.equal(describeAdded([{ reclassifies_staff_only: true }]),
    "Page added. A page is now readable with this portal's password.");
  assert.equal(describeAdded([null, { reclassifies_staff_only: false }]), "Page added.");
  // Nothing landed is nothing to say, not "Added 0 dashboards." — describePartialAdd
  // leans on this to tell a total failure from a half one.
  assert.equal(describeAdded([]), "");
  assert.equal(describeAdded(null), "");
});

test("portal admin: a half-done Add all reports what it left behind", () => {
  // The adds are sequential and each is committed on its own, so a failure on
  // page 2 leaves page 1 a real member — and the re-read that follows has already
  // deleted the audit row that would have carried the news. Reporting only the
  // failure dropped the one sentence this screen exists to say out loud: a
  // staff-only dashboard just became readable by everyone holding the portal's
  // password (#209 review).
  assert.equal(
    describePartialAdd("server exploded", [
      { reclassifies_staff_only: true, member: { display_title: "Tailspin Pet Q3" } },
    ]),
    "server exploded — the rest were not added. Page added."
      + " Tailspin Pet Q3 is now readable with this portal's password.");
  // A batch that only added something already client-readable still says it stopped.
  assert.equal(
    describePartialAdd("server exploded", [{ reclassifies_staff_only: false }]),
    "server exploded — the rest were not added. Page added.");
  // Failing on the FIRST write changed nothing, so the failure speaks alone —
  // no phantom "the rest were not added" over an untouched portal.
  assert.equal(describePartialAdd("server exploded", []), "server exploded");
  assert.equal(describePartialAdd("server exploded", undefined), "server exploded");
});

test("portal admin: the browser's sort_order ceiling is the API's own", () => {
  // The only machine-checked guard on these two numbers agreeing used to be the
  // native max="9999" on the Order input #173 deleted, so this is what is left to
  // catch a drift between lib/portals.js and the browser module that has to
  // append without proposing a value the write would reject.
  // The server half of this pin is Go: internal/fixture's
  // TestBrowserMaxSortOrderMatchesServer reads this module's literal and checks
  // it against the API's ceiling. This side pins the number the browser plans
  // with, so a change to either file fails one of the two.
  assert.equal(BROWSER_MAX_SORT_ORDER, 9999);
});

test("portal admin: the member caption speaks about the partner's view, not this table", () => {
  // The clause it replaced read "— with the home page first", sitting
  // directly above a table that shows the home row wherever the arrows put it.
  // The claim is unconditional and it is about the PARTNER, because
  // db.getPortalPages orders by home_page_id BEFORE sort_order: a home row third
  // here is still first there. The wording must not drift back to a claim about
  // this table's own order.
  const withHome = memberCaption({ count: 3, openable: 2, hasHome: true });
  assert.match(withHome, /^3 pages in this order, 2 of them visible to this partner\./);
  assert.match(withHome, /The partner's own index always shows the home page first, wherever it sits here\./);
  assert.equal(/home page first\./.test(withHome), false, "no claim that THIS table is home-first");
  assert.equal(/with the home page first/.test(withHome), false);
  // Nothing about a home dashboard when the portal has none.
  const noHome = memberCaption({ count: 3, openable: 3, hasHome: false });
  assert.equal(/home dashboard/.test(noHome), false);
  // The shortcut is named where a keyboard operator can actually read it, rather
  // than only in a `title` that renders on hover.
  assert.match(withHome, /Alt\+↑ and Alt\+↓ move a row from anywhere in it\./);

  // One dashboard has no order to speak of and no row to move.
  const one = memberCaption({ count: 1, openable: 1, hasHome: true });
  assert.match(one, /^1 page, visible to this partner\./);
  assert.equal(/in this order/.test(one), false);
  assert.equal(/Alt\+/.test(one), false);
  assert.match(memberCaption({ count: 1, openable: 0, hasHome: false }), /^1 page, not visible to this partner\./);

  // Every wording still ends with what a change here actually does.
  for (const caption of [withHome, noHome, one]) {
    assert.match(caption, /The list is read live: changes here reach them on their next page load, with nothing redeployed\.$/);
  }
});

test("flag motion: number formats are exact and never print a fabricated zero", () => {
  const motion = require("../public/assets/flag/motion/flag-motion.js");
  const f = motion.format;
  assert.equal(f(1234.5, "usd"), "$1,234.50");
  assert.equal(f(1234.5, "usd0"), "$1,235");
  assert.equal(f(1234567, "usdc"), "$1.2M");
  assert.equal(f(0.1234, "pct"), "12.3%");
  assert.equal(f(0.1234, "pct2"), "12.34%");
  assert.equal(f(3.2, "x"), "3.20x");
  assert.equal(f(1425000, "compact"), "1.4M");
  assert.equal(f(1234.4, "int"), "1,234");
  for (const missing of [null, undefined, NaN, Infinity, "12"]) assert.equal(f(missing, "usd"), "N/A");
  assert.equal(motion.formatLabel("2026-10-06"), "Oct 6");
  assert.equal(motion.formatLabel("2026-10-06", "long"), "Oct 6, 2026");
  assert.deepEqual(motion.niceTicks(0, 1450, 4).ticks, [0, 500, 1000, 1500]);
  assert.equal(motion.relativeAge(3 * 3600e3), "3 h ago");
  // Refresh stamps: the epoch placeholder a page carries before its first ingest is "no data".
  assert.equal(motion.freshness({ refreshedAt: "1970-01-01T00:00:00Z", sourceAsOf: "1970-01-01T00:00:00Z" }).text, "Awaiting first data");
  const now = Date.parse("2026-10-12T12:00:00Z");
  const fresh = motion.freshness({ refreshedAt: "2026-10-12T09:00:00Z", sourceAsOf: "2026-10-11T23:59:59Z" }, now);
  assert.equal(fresh.text, "Updated 3 h ago · data through Oct 11, 2026");
  assert.equal(fresh.state, "live");
  assert.equal(motion.freshness({ refreshedAt: "2026-10-08T09:00:00Z" }, now).state, "stale");
  // End of day in New York is 03:59:59Z the next day: still "through Oct 8".
  assert.equal(motion.freshness({ refreshedAt: "2026-10-12T09:00:00Z", sourceAsOf: "2026-10-09T03:59:59Z" }, now).through, "Oct 8, 2026");
  // A page that states its covered day wins.
  assert.equal(motion.freshness({ refreshedAt: "2026-10-12T09:00:00Z", sourceAsOf: "2026-10-09T12:00:00Z", data: { dataThrough: "2026-10-07" } }, now).through, "Oct 7, 2026");
});

// ── the admin stylesheet ─────────────────────────────────────────────────────
// Two defects have shipped in shell.css that no API test could see, both of the
// same shape: a rule that does not exist, so the element silently falls back to
// user-agent styling. First a batch of invented class names; then a comment whose
// body contained a selector ending in a star-slash, which closed the comment
// early and made the parser swallow the rule that followed it. These tests are
// the cheap guard: the stylesheet must parse to sane selectors, and the classes
// the template library depends on must actually be defined.

function stripCssComments(css) {
  return css.replace(/\/\*[\s\S]*?\*\//g, "");
}

function cssSelectors(css) {
  const selectors = [];
  // Good enough for a flat stylesheet with one level of @media nesting: take the
  // text before each "{" and drop at-rule preludes.
  for (const match of stripCssComments(css).matchAll(/([^{}]+)\{/g)) {
    const prelude = match[1].trim();
    if (!prelude || prelude.startsWith("@")) continue;
    selectors.push(prelude);
  }
  return selectors;
}

const SHELL_CSS = fs.readFileSync(path.join(__dirname, "..", "public", "shell-assets", "shell.css"), "utf8");

test("shell.css: no comment leaks selector text into the stylesheet", () => {
  // A selector can never contain a slash. One appearing here means a comment
  // terminated early and the parser is now reading prose as CSS — which silently
  // discards the following rule.
  const leaked = cssSelectors(SHELL_CSS).filter((selector) => selector.includes("/"));
  assert.deepEqual(leaked, [], `selector text containing "/" means a comment ended early: ${leaked.join(" | ")}`);

  // Same failure seen from the other side: stripping comments must leave none of
  // their terminators behind.
  assert.ok(!stripCssComments(SHELL_CSS).includes("*/"), "an unbalanced comment terminator remains after stripping");
});

test("shell.css: every class the template library renders is defined", () => {
  const defined = new Set();
  for (const selector of cssSelectors(SHELL_CSS)) {
    for (const match of selector.matchAll(/\.([A-Za-z0-9_-]+)/g)) defined.add(match[1]);
  }
  // Read from public/shell-assets/templates.js — the screen renders nothing else.
  const used = [
    "page-heading", "page-heading__row", "page-heading__copy", "page-heading__intro", "page-heading__actions",
    "stats", "stat", "panel", "section-block", "section-heading", "section-heading--row",
    "operation-table", "operation-table-wrap", "page-cell", "table-meta", "row-actions",
    "template-table__template", "template-table__num", "template-table__hash",
    "template-table__when", "template-table__actions",
    "row--selected", "template-detail", "template-kv", "code-block", "template-contract",
    "segmented", "segmented__button", "badge", "badge--live", "badge--pending",
    "btn", "btn-sm", "btn-primary", "form-stack", "field", "field-label", "field-help",
    "note", "note--warning",
    "cluster", "muted", "sr-only", "state-panel",
  ];
  const missing = used.filter((name) => !defined.has(name));
  assert.deepEqual(missing, [], `templates.js renders classes shell.css never defines: ${missing.join(", ")}`);
});

test("shell.css: every class the portals screen renders is defined", () => {
  const defined = new Set();
  for (const selector of cssSelectors(SHELL_CSS)) {
    for (const match of selector.matchAll(/\.([A-Za-z0-9_-]+)/g)) defined.add(match[1]);
  }
  // Read from public/shell-assets/portals.js. It used to borrow the template
  // library's .template-code/.template-row--selected and the form primitives'
  // .field-error/.field-help for things that are neither a template nor a form
  // field, so a rename on either screen could silently unstyle this one.
  const used = [
    "code-block", "row--selected", "note", "note--warning",
    "field", "field-label", "field-help", "form-stack", "cluster",
    "operation-table", "operation-table-wrap", "table-meta", "row-actions",
    "section-block", "section-heading", "section-heading--row", "panel",
    "badge", "badge--live", "btn", "btn-sm", "btn-primary", "btn-danger",
    "state-panel",
    // The move-up/move-down controls (#173) borrow the dialog close button's
    // square icon control and the shared icon sizing rather than adding a class
    // of their own. The two that ARE new are what keeps their x from changing
    // between rows, so a rename that unstyled them would put the pointer bug back.
    "icon-action", "icon-inline",
    "row-actions--split", "row-actions__group", "operation-table__member-actions",
    "operation-table__page",
  ];
  const missing = used.filter((name) => !defined.has(name));
  assert.deepEqual(missing, [], `portals.js renders classes shell.css never defines: ${missing.join(", ")}`);
});

test("shell.css: every class the refresh-health views render is defined", () => {
  const defined = new Set();
  for (const selector of cssSelectors(SHELL_CSS)) {
    for (const match of selector.matchAll(/\.([A-Za-z0-9_-]+)/g)) defined.add(match[1]);
  }
  // Read from welcome.js (the index chip and filter) and admin.js (the banner
  // and the Refresh schedule section).
  const used = [
    "index-toolbar__tools", "attention-filter", "unscheduled-filter", "refresh-health", "refresh-health__reason", "sr-only",
    "refresh-banner", "refresh-banner--error", "refresh-banner--warning", "refresh-banner__reason", "refresh-banner__link",
    "refresh-health__summary", "refresh-facts", "template-kv", "access-grid", "subpanel", "subpanel__head",
    "form-stack", "form-grid", "field", "field-error", "form-status", "cluster",
  ];
  const missing = used.filter((name) => !defined.has(name));
  assert.deepEqual(missing, [], `refresh health renders classes shell.css never defines: ${missing.join(", ")}`);
});

test("shell-assets: only primitives.js and page-switcher.js may fetch, encode, or format", () => {
  // The issue's own done-when condition, as a test. Once a screen can reach for
  // fetch() or toLocaleString() itself, the fifth variant of the behaviour is
  // one session away — which is how four incompatible fetch wrappers and four
  // date fallbacks got here in the first place.
  const screens = ["welcome.js", "admin.js", "templates.js", "portals.js", "refresh-health.js"];
  const banned = [
    [/\bfetch\(/, "fetch( — use UI.request or UI.requestScope"],
    [/toLocale/, "toLocale… — use UI.formatWhen or UI.formatCount"],
    [/encodeURIComponent/, "encodeURIComponent — use UI.slugPath or UI.pathSegment"],
  ];
  for (const name of screens) {
    const source = fs.readFileSync(path.join(__dirname, "..", "public", "shell-assets", name), "utf8");
    for (const [pattern, why] of banned) {
      assert.equal(pattern.test(source), false, `${name} must not contain ${why}`);
    }
    // A bare `confirm` in the destructure shadows window.confirm, so a later
    // `if (confirm("…"))` would test a Promise and always pass.
    const destructure = source.match(/const \{[^}]*\} = UI;/g) || [];
    for (const line of destructure) {
      assert.equal(/\bconfirm\b(?!Dialog)/.test(line), false, `${name} destructures a bare confirm: ${line}`);
    }
  }
});

test("shell.css: the collapsed table arrangement stays scoped to the admin index", () => {
  // The named grid areas exist only for the index's own six data-labels. Applying
  // them to any other operation-table scatters its cells into implicit tracks.
  const blocks = stripCssComments(SHELL_CSS).matchAll(/([^{}]+)\{([^{}]*)\}/g);
  for (const [, prelude, body] of blocks) {
    if (!body.includes("grid-template-areas")) continue;
    if (!prelude.includes("operation-table")) continue;
    assert.ok(
      prelude.includes("operation-table--index"),
      `grid-template-areas on "${prelude.trim()}" must be scoped to .operation-table--index`
    );
  }
});

// A Bento deck, as test/unit.test.js builds one for the server's recogniser.
function bentoDeckHtml() {
  return `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><title>Deck</title>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline' blob:; connect-src 'none'">
<script id="fleet-offline-deck">(function(){try{localStorage.setItem("bento-offline","on")}catch(e){}})();</script>
<script type="application/bento+json" id="bento-doc">{"format":"bento/slides","version":1,"slides":[]}</script>
<script id="bento-rt" type="bento/deflate-b64">c8vPz1EoLSjOSczJyU9V0FIoLSktKSpJUUhJVEgQMAAA</script>
</head><body><div id="bento-splash"></div><script>(async()=>{var js="";var url=URL.createObjectURL(new Blob([js],{type:"text/javascript"}));await import(url)})()</script></body></html>`;
}

test("bento: the admin's file check and the server's recogniser agree", () => {
  // The admin picks Raw for a deck before the server would refuse Themed. Two
  // recognisers, one in the browser and one in lib/render.js, must never answer
  // differently, or a file the editor calls a deck is refused by the save.
  const samples = [
    bentoDeckHtml(),
    bentoDeckHtml().replace('"application/bento+json"', "'application/bento+json'"),
    bentoDeckHtml().replace('type="application/bento+json" id="bento-doc"', 'id="bento-doc" type=application/bento+json'),
    "<p>a page that mentions application/bento+json in prose</p>",
    '<script type="application/json" id="bento-doc">{}</script>',
    "<!doctype html><html><body><h1>Plain</h1></body></html>",
    "",
  ];
  // What the server's recogniser answers for each sample, in order. The Go side
  // (internal/fixture TestBentoRecogniserTable) checks render.IsBentoDeck
  // against this same table, so the browser and the server cannot drift apart
  // without one of the two failing.
  const server = [true, true, true, false, false, false, false];
  samples.forEach((sample, index) => {
    assert.equal(UI.looksLikeBentoDeck(sample), server[index], sample.slice(0, 70));
  });
});
