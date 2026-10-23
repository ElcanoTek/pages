// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Refresh health on the staff screens. The fixture seeds one page per state,
// each judged by the server's own rules (internal/fixture/health.go):
// client-03 blocked, client-02 overdue, client-05 source stuck, client-01 on
// schedule, and everything else — the detail page included — unscheduled.

const { test, expect } = require("@playwright/test");
const { resetFixture, expectNoHorizontalOverflow, expectNoSeriousAxeViolations } = require("./helpers");

test.beforeEach(async ({ request }) => resetFixture(request));

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
  // A page on schedule and a page with no schedule carry no chip at all.
  await expect(updated(page, "Client 01 operations dashboard").locator(".refresh-health")).toHaveCount(0);
  await expect(page.locator(".operation-table tbody tr").first().locator(".refresh-health")).toHaveCount(0);

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
  await expect(page.locator(".operation-table .refresh-health")).toHaveCount(0);
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
