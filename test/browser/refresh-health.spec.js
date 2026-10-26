// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Refresh health on the staff screens. The fixture seeds one page per state,
// each judged by the server's own rules (internal/fixture/health.go):
// client-03 blocked, client-02 overdue, client-05 source stuck, client-01 on
// schedule, and everything else — the detail page included — unscheduled.
// Nineteen of the unscheduled ones are live with data more than a week old (the
// detail page at 46 days, and clients 09 to 35 that are live, at as many days
// as their number), so they carry the grey "No refresh" hint instead.

const { test, expect } = require("@playwright/test");
const { resetFixture, expectNoHorizontalOverflow, expectNoSeriousAxeViolations } = require("./helpers");

test.beforeEach(async ({ request }) => resetFixture(request));

const longTitle = "North America Programmatic Revenue and Performance Review — Quarter Two Executive Detail";
const row = (page, title) => page.locator(".operation-table tbody tr", { hasText: title }).first();
const updated = (page, title) => row(page, title).locator('td[data-label="Last update"]');

test("the index marks the dashboards whose refresh needs a human, with the reason on the row", async ({ page }) => {
  await page.goto("/admin");
  await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();

  const blocked = updated(page, "Client 03 operations dashboard");
  await expect(blocked.locator(".badge")).toHaveText("Blocked");
  await expect(blocked.locator(".badge")).toHaveClass(/badge--error/);
  await expect(blocked.locator(".refresh-health__reason")).toHaveText("Required source ssp_daily could not be authenticated");
  await expect(blocked.locator(".badge")).toHaveAttribute("title", "Required source ssp_daily could not be authenticated");

  const overdue = updated(page, "Client 02 operations dashboard");
  await expect(overdue.locator(".badge")).toHaveText("Overdue");
  await expect(overdue.locator(".badge")).toHaveClass(/badge--warning/);
  await expect(overdue.locator(".refresh-health__reason")).toHaveText("Expected a refresh at Jul 21 09:00 UTC; last activity Jul 20.");

  const stuck = updated(page, "Client 05 operations dashboard");
  await expect(stuck.locator(".badge")).toHaveText("Source stuck");
  await expect(stuck.locator(".badge")).toHaveClass(/badge--warning/);
  await expect(stuck.locator(".refresh-health__reason")).toContainText("Source data hasn't advanced since Jul 17");

  // The existing data-age line is still there, above the chip.
  await expect(stuck).toContainText("Data 5d old");
  // A page on schedule carries no chip at all; a page with no schedule carries
  // no verdict (the first row's grey hint is covered below).
  await expect(updated(page, "Client 01 operations dashboard").locator(".refresh-health")).toHaveCount(0);
  await expect(page.locator(".operation-table tbody tr").first().locator(".badge--warning, .badge--error, .refresh-health__reason")).toHaveCount(0);

  // The summary says how many, before anyone filters.
  await expect(page.locator(".stat", { hasText: "need attention" }).locator("strong")).toHaveText("3");
  await expect(page.getByRole("button", { name: "Needs attention (3)" })).toHaveAttribute("aria-pressed", "false");
  await expectNoSeriousAxeViolations(page, "index with health chips");
});

test("Needs attention filters to those dashboards, worst first, and survives a reload", async ({ page }) => {
  await page.goto("/admin");
  const filter = page.getByRole("button", { name: "Needs attention (3)" });
  await filter.click();
  await expect(filter).toHaveAttribute("aria-pressed", "true");
  await expect(page).toHaveURL(/[?&]attention=1/);
  const titles = page.locator(".operation-table tbody .page-cell__title");
  // Blocked, then overdue, then a stuck source.
  await expect(titles).toHaveText([
    "Client 03 operations dashboard",
    "Client 02 operations dashboard",
    "Client 05 operations dashboard",
  ]);
  await expect(page.locator(".workspace-index__count")).toHaveText("3 of 37 pages");
  await expectNoSeriousAxeViolations(page, "index filtered to attention");

  // Linkable: a digest can send someone straight here.
  await page.reload();
  await expect(page.getByRole("button", { name: "Needs attention (3)" })).toHaveAttribute("aria-pressed", "true");
  await expect(titles).toHaveCount(3);

  // It narrows within a workspace, and its count follows the workspace.
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.locator(".workspace-nav").getByRole("button", { name: /Executive reporting/ }).click();
  await expect(page.getByRole("button", { name: "Needs attention (1)" })).toHaveAttribute("aria-pressed", "true");
  await expect(titles).toHaveText(["Client 02 operations dashboard"]);

  // And off again: every page is back.
  await page.locator(".workspace-nav").getByRole("button", { name: /All pages/ }).click();
  await page.getByRole("button", { name: "Needs attention (3)" }).click();
  await expect(page).not.toHaveURL(/attention=/);
  await expect(page.locator(".workspace-index__count")).toHaveText("Showing 25 of 37 pages");
});

test("with nothing late the filter says so and offers the way back", async ({ page, request }) => {
  // Searching inside the filter narrows it like any other view.
  await page.goto("/admin?attention=1");
  await page.getByRole("searchbox").fill("Client 1");
  await expect(page.locator(".operation-table")).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "No matching pages" })).toBeVisible();
  await page.getByRole("searchbox").fill("");
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(3);

  // Clearing a schedule stops the judgement, so these three stop needing anyone.
  for (const slug of ["client-02", "client-03", "client-05"]) {
    const cleared = await request.post(`/api/v1/admin/pages/${slug}/refresh-schedule`, { data: { schedule: null } });
    expect(cleared.ok()).toBeTruthy();
  }
  await page.goto("/admin?attention=1");
  await expect(page.getByRole("heading", { name: "Nothing needs attention" })).toBeVisible();
  const filter = page.getByRole("button", { name: "Needs attention (0)" });
  await expect(filter).toHaveAttribute("aria-pressed", "true");
  await page.getByRole("button", { name: "Show all pages" }).click();
  await expect(filter).toHaveAttribute("aria-pressed", "false");
  await expect(filter).toBeFocused();
  await expect(page.locator(".workspace-index__count")).toHaveText("Showing 25 of 37 pages");
  await expect(page.locator(".operation-table .refresh-health").filter({ has: page.locator(".badge--warning, .badge--error") })).toHaveCount(0);
});

// ── No refresh set: a hint, not a verdict ─────────────────────────────────────

const HINT_46 = "No refresh schedule · data 46 days old — set a schedule or ignore if the campaign has ended";

test("a live page with week-old data and no refresh schedule gets a grey hint, not a verdict", async ({ page }) => {
  await page.goto("/admin");
  // The detail page: live, unscheduled, data 46 days old, checked today.
  const frozen = page.locator(".operation-table tbody tr").first().locator('td[data-label="Last update"]');
  const chip = frozen.locator(".refresh-health .badge");
  await expect(chip).toHaveText("No refresh");
  // Grey — the tone "Not scheduled" uses — never the amber or red of a verdict.
  await expect(chip).toHaveClass(/badge--draft/);
  await expect(chip).not.toHaveClass(/badge--(warning|error)/);
  // The sentence is the hover and the screen-reader text, not a clipped line:
  // the data age is already on the row.
  await expect(chip).toHaveAttribute("title", HINT_46);
  await expect(frozen.locator(".refresh-health .sr-only")).toHaveText(HINT_46);
  await expect(frozen.locator(".refresh-health__reason")).toHaveCount(0);
  await expect(frozen).toContainText("Data 46d old");
  await expect(updated(page, "Client 09 operations dashboard").locator(".badge")).toHaveText("No refresh");
  await expect(updated(page, "Client 09 operations dashboard").locator(".badge")).toHaveAttribute("title", /data 9 days old/);

  // No hint for: a week exactly (client-07), a disabled page (client-11), a
  // page with no managed data (client-12), and a page with a schedule, whose
  // verdict speaks instead (client-05).
  for (const title of ["Client 07", "Client 11", "Client 12"]) {
    await expect(updated(page, `${title} operations dashboard`).locator(".refresh-health")).toHaveCount(0);
  }
  await expect(updated(page, "Client 05 operations dashboard").locator(".badge")).toHaveText("Source stuck");

  // Not counted as needing attention: the tile and that filter are unchanged.
  await expect(page.locator(".stat", { hasText: "need attention" }).locator("strong")).toHaveText("3");
  await expect(page.getByRole("button", { name: "Needs attention (3)" })).toHaveAttribute("aria-pressed", "false");
  await expect(page.getByRole("button", { name: "No refresh set (19)" })).toHaveAttribute("aria-pressed", "false");
  await expectNoSeriousAxeViolations(page, "index with no-refresh hints");
});

test("No refresh set filters to those pages, oldest data first, beside Needs attention", async ({ page, request }) => {
  await page.goto("/admin");
  const unscheduled = page.getByRole("button", { name: /^No refresh set/ });
  const attention = page.getByRole("button", { name: /^Needs attention/ });
  await unscheduled.click();
  await expect(unscheduled).toHaveAttribute("aria-pressed", "true");
  await expect(page).toHaveURL(/[?&]unscheduled=1/);
  const titles = page.locator(".operation-table tbody .page-cell__title");
  await expect(titles).toHaveCount(19);
  await expect(titles.nth(0)).toHaveText(longTitle);
  await expect(titles.nth(1)).toHaveText("Client 35 operations dashboard");
  await expect(titles.nth(18)).toHaveText("Client 09 operations dashboard");
  await expect(page.locator(".workspace-index__count")).toHaveText("19 of 37 pages");
  await expect(page.locator(".operation-table .badge--warning, .operation-table .badge--error")).toHaveCount(0);
  await expectNoSeriousAxeViolations(page, "index filtered to no refresh set");

  // Linkable, like ?attention=1.
  await page.reload();
  await expect(page.getByRole("button", { name: "No refresh set (19)" })).toHaveAttribute("aria-pressed", "true");
  await expect(titles).toHaveCount(19);

  // One filter at a time: the two lists never share a page.
  await attention.click();
  await expect(attention).toHaveAttribute("aria-pressed", "true");
  await expect(unscheduled).toHaveAttribute("aria-pressed", "false");
  await expect(page).toHaveURL(/[?&]attention=1/);
  await expect(page).not.toHaveURL(/unscheduled=/);
  await expect(titles).toHaveCount(3);
  await unscheduled.click();
  await expect(page).not.toHaveURL(/attention=/);
  await expect(titles).toHaveCount(19);

  // Its count follows the workspace.
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.locator(".workspace-nav").getByRole("button", { name: /Executive reporting/ }).click();
  await expect(page.getByRole("button", { name: "No refresh set (4)" })).toHaveAttribute("aria-pressed", "true");
  await expect(titles).toHaveText([
    "Client 34 operations dashboard",
    "Client 26 operations dashboard",
    "Client 14 operations dashboard",
    "Client 10 operations dashboard",
  ]);
  await page.locator(".workspace-nav").getByRole("button", { name: /All pages/ }).click();

  // Setting a schedule moves a page from the hint to the verdict (client-09's
  // last refresh was nine days ago, so a daily schedule finds it overdue);
  // taking a page down removes its hint.
  expect((await request.post("/api/v1/admin/pages/client-09/refresh-schedule", { data: { schedule: "0 9 * * *" } })).ok()).toBeTruthy();
  expect((await request.post("/api/v1/admin/pages/client-10/disable", { data: {} })).ok()).toBeTruthy();
  await page.goto("/admin?unscheduled=1");
  await expect(page.getByRole("button", { name: "No refresh set (17)" })).toHaveAttribute("aria-pressed", "true");
  await expect(page.getByRole("button", { name: "Needs attention (4)" })).toBeVisible();
  await expect(titles.filter({ hasText: /^Client (09|10) operations dashboard$/ })).toHaveCount(0);

  // And off again: every page is back.
  await page.getByRole("button", { name: "No refresh set (17)" }).click();
  await expect(page).not.toHaveURL(/unscheduled=/);
  await expect(page.locator(".workspace-index__count")).toHaveText("Showing 25 of 37 pages");
});

test("with no page left to hint, the filter is not offered, and a stale link says so", async ({ page }) => {
  // Every live page has a schedule or recent data.
  await page.route("**/api/v1/admin/pages", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    await route.fulfill({ json: { ...body, pages: body.pages.map((entry) => ({ ...entry, refresh_health: { ...entry.refresh_health, unscheduled_stale: false } })) } });
  });
  await page.goto("/admin");
  await expect(page.getByRole("button", { name: /^Needs attention/ })).toBeVisible();
  await expect(page.getByRole("button", { name: /^No refresh set/ })).toHaveCount(0);
  await expect(page.locator(".operation-table .refresh-health .badge--draft")).toHaveCount(0);

  await page.goto("/admin?unscheduled=1");
  await expect(page.getByRole("heading", { name: "Every live page has a refresh" })).toBeVisible();
  const filter = page.getByRole("button", { name: "No refresh set (0)" });
  await expect(filter).toHaveAttribute("aria-pressed", "true");
  await expectNoSeriousAxeViolations(page, "no refresh set, empty");
  await page.getByRole("button", { name: "Show all pages" }).click();
  await expect(filter).toHaveAttribute("aria-pressed", "false");
  await expect(filter).toBeFocused();
  await expect(page).not.toHaveURL(/unscheduled=/);
  await expect(page.locator(".workspace-index__count")).toHaveText("Showing 25 of 37 pages");
});

test("the no-refresh hint and its filter fit a phone", async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 900 });
  await page.goto("/admin?unscheduled=1");
  const cell = page.locator(".operation-table tbody tr").first().locator('td[data-label="Last update"]');
  await expect(cell.locator(".badge")).toHaveText("No refresh");
  await expect(cell.locator(".badge")).toBeVisible();
  await expect(page.getByRole("button", { name: "No refresh set (19)" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Needs attention (3)" })).toBeVisible();
  await expectNoHorizontalOverflow(page);
  await expectNoSeriousAxeViolations(page, "no refresh set at phone width");
});

test("the index keeps the chip and drops the sentence where a card has no room for it", async ({ page }) => {
  await page.setViewportSize({ width: 320, height: 900 });
  await page.goto("/admin?attention=1");
  const cell = updated(page, "Client 03 operations dashboard");
  await expect(cell.locator(".badge")).toHaveText("Blocked");
  await expect(cell.locator(".refresh-health__reason")).toBeHidden();
  await expect(page.getByRole("button", { name: "Needs attention (3)" })).toBeVisible();
  await expectNoHorizontalOverflow(page);
});

test("the review screen leads with a slim banner when the page's refresh needs a human", async ({ page }) => {
  await page.goto("/admin/client-02");
  const banner = page.locator("#refresh-banner");
  await expect(banner).toBeVisible();
  await expect(banner.locator(".badge")).toHaveText("Overdue");
  await expect(banner).toContainText("Expected a refresh at Jul 21 09:00 UTC; last activity Jul 20.");
  await expect(banner).toHaveClass(/refresh-banner--warning/);
  // The banner is part of the page header, under the identity, not a pill
  // beside the serving state.
  await expect(page.locator(".page-overview__statuses #refresh-banner")).toHaveCount(0);
  await banner.getByRole("link", { name: "Refresh schedule" }).click();
  await expect(page).toHaveURL(/#refresh-title$/);
  await expectNoSeriousAxeViolations(page, "detail with a refresh banner");

  await page.goto("/admin/client-03");
  await expect(page.locator("#refresh-banner")).toHaveClass(/refresh-banner--error/);
  await expect(page.locator("#refresh-banner")).toContainText("Required source ssp_daily could not be authenticated");

  // On schedule, and unscheduled: no banner.
  await page.goto("/admin/client-01");
  await expect(page.getByRole("heading", { level: 2, name: "Refresh schedule" })).toBeVisible();
  await expect(page.locator("#refresh-banner")).toHaveCount(0);
  await expect(page.locator("#refresh-status .badge")).toHaveText("On schedule");
  await expect(page.locator("#refresh-status")).toContainText("Daily at 09:00 UTC");
  await expect(page.locator("#refresh-status")).toContainText("desk@northwind.example");
  await page.goto("/admin/long/client/q2-report");
  await expect(page.getByRole("heading", { level: 2, name: "Refresh schedule" })).toBeVisible();
  await expect(page.locator("#refresh-banner")).toHaveCount(0);
  await expect(page.locator("#refresh-status .badge")).toHaveText("Not scheduled");
});

test("staff set, correct and clear a page's refresh schedule from its review screen", async ({ page, request }) => {
  await page.goto("/admin/client-04");
  const section = page.locator("section", { has: page.getByRole("heading", { level: 2, name: "Refresh schedule" }) });
  await expect(section.locator("#refresh-status .badge")).toHaveText("Not scheduled");
  await expect(page.locator("#refresh-banner")).toHaveCount(0);

  // A preset reads as words and saves as a UTC cron. Only its inputs show.
  const preset = section.getByLabel("Refreshes");
  await expect(preset).toHaveValue("none");
  await expect(section.getByLabel("Time (UTC)")).toBeHidden();
  await preset.selectOption("weekdays");
  await expect(section.getByLabel("Time (UTC)")).toBeVisible();
  await expect(section.getByLabel("Day")).toBeHidden();
  await section.getByLabel("Time (UTC)").fill("15:00");
  await section.getByLabel("Owner").fill("Desk@Northwind.example");
  await section.getByRole("button", { name: "Save schedule" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Refresh schedule saved" })).toBeVisible();

  const events = (await (await request.get("/__fixture/events")).json()).events.filter((event) => event.path.endsWith("/refresh-schedule"));
  expect(events.map((event) => event.body)).toEqual([{ schedule: "0 15 * * 1-5", owner: "Desk@Northwind.example" }]);
  // The server's verdict comes straight back: a page nobody has ever
  // refreshed, created days ago, is overdue the moment it has a schedule.
  await expect(section.locator("#refresh-status .badge")).toHaveText("Overdue");
  await expect(section.locator("#refresh-status")).toContainText("Weekdays at 15:00 UTC");
  await expect(section.locator("#refresh-status")).toContainText("desk@northwind.example");
  await expect(page.locator("#refresh-banner")).toContainText("no refresh or check has been recorded yet");
  await expect(section.getByLabel("Refreshes")).toHaveValue("weekdays");
  await expect(section.getByLabel("Time (UTC)")).toHaveValue("15:00");

  // A custom cron starts from the preset it replaces, and the server's own
  // words come back when it is not one Pages accepts.
  await section.getByLabel("Refreshes").selectOption("custom");
  await expect(section.getByLabel("Cron (UTC)")).toHaveValue("0 15 * * 1-5");
  await section.getByLabel("Cron (UTC)").fill("0 15 * * MON-FRI");
  await section.getByRole("button", { name: "Save schedule" }).click();
  await expect(section.getByRole("alert")).toContainText("names such as MON or JAN are not accepted");
  await expect(section.locator("#refresh-status")).toContainText("Weekdays at 15:00 UTC");

  // Weekly, on a named day.
  await section.getByLabel("Refreshes").selectOption("weekly");
  await section.getByLabel("Day").selectOption("Monday");
  await section.getByLabel("Time (UTC)").fill("09:30");
  await section.getByRole("button", { name: "Save schedule" }).click();
  await expect(section.locator("#refresh-status")).toContainText("Mondays at 09:30 UTC");
  await expectNoSeriousAxeViolations(page, "detail refresh schedule editor");

  // No schedule: no judgement, and no banner. The owner stays.
  await section.getByLabel("Refreshes").selectOption("none");
  await section.getByRole("button", { name: "Save schedule" }).click();
  await expect(section.locator("#refresh-status .badge")).toHaveText("Not scheduled");
  await expect(page.locator("#refresh-banner")).toHaveCount(0);
  await expect(section.locator("#refresh-status")).toContainText("desk@northwind.example");
  const last = (await (await request.get("/__fixture/events")).json()).events.filter((event) => event.path.endsWith("/refresh-schedule")).pop();
  expect(last.body).toEqual({ schedule: null, owner: "desk@northwind.example" });
});
