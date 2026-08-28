// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { test, expect } = require("@playwright/test");
const { resetFixture, expectNoHorizontalOverflow, expectNoSeriousAxeViolations, expectCollapsedTablesAreLabelled } = require("./helpers");

test.beforeEach(async ({ request }) => resetFixture(request));

test("operations list supports search, workspace filtering, assignment, and accessible names", async ({ page }) => {
  await page.goto("/admin");
  await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();
  // The list opens on a window of 25, not the whole fleet; the count says so.
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(25);
  await expect(page.locator(".workspace-index__count")).toHaveText("Showing 25 of 37 pages");
  await expect(page.getByRole("table")).toHaveAccessibleName(/Pages, serving status/);

  const workspaceManager = page.locator(".workspace-sidebar__head").getByRole("button", { name: "Manage workspaces" });
  const managerIcon = workspaceManager.locator("svg");
  await expect(managerIcon).toHaveCount(1);
  expect(await managerIcon.evaluate((node) => node.namespaceURI)).toBe("http://www.w3.org/2000/svg");
  expect(await managerIcon.locator("use").evaluate((node) => node.namespaceURI)).toBe("http://www.w3.org/2000/svg");
  await expect(managerIcon.locator("use")).toHaveAttribute("href", /core-icons\.svg#folder$/);
  await expect.poll(() => managerIcon.evaluate((node) => node.getBBox().width)).toBeGreaterThan(0);

  const firstRow = page.locator(".operation-table tbody tr").first();
  await expect(firstRow.locator('td[data-label="Live version"]')).toHaveText("Version 2");
  await expect(firstRow.locator('td[data-label="Live version"]')).not.toContainText("#102");

  const search = page.getByRole("searchbox", { name: "Search All pages" });
  await search.fill("annual-performance-with-a-very-long-slug");
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(1);
  await expect(page.getByText("A very long client title", { exact: false })).toBeVisible();
  await search.fill("");

  await page.locator(".workspace-nav").getByRole("button", { name: /Executive reporting/ }).click();
  await expect(page).toHaveURL(/workspace=2/);
  await expect(page.getByRole("heading", { level: 2, name: "Executive reporting" })).toBeVisible();
  await expect(page.getByRole("searchbox", { name: "Search Executive reporting" })).toBeVisible();

  await page.locator(".workspace-nav").getByRole("button", { name: /All pages/ }).click();
  // Reassignment is deliberate now: the row states its workspace and opens a
  // dialog, rather than committing on a <select> change (and on a stray wheel).
  await page.getByRole("button", { name: /^Workspace for Client 01 operations dashboard/ }).click();
  const move = page.getByRole("dialog", { name: /^Move Client 01 operations dashboard/ });
  await expect(move).toBeVisible();
  await move.getByLabel("Workspace").selectOption("2");
  await move.getByRole("button", { name: "Move page" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Moved /client-01" })).toBeVisible();
  await expect(page.getByRole("button", { name: /^Workspace for Client 01 operations dashboard/ }))
    .toHaveAccessibleName(/Executive reporting/);

  await expectNoHorizontalOverflow(page);
  await expectNoSeriousAxeViolations(page, "admin index");
});

// "Last update" is the page ROW's updated_at: it moves when someone renames a
// page and stays put when a daily refresh silently stops. Ten of seventeen live
// managed pages were three days to six weeks stale and this table said nothing.
test("the operations list says how old each dashboard's DATA is", async ({ page }) => {
  await page.goto("/admin");
  const frozen = page.locator(".operation-table tbody tr").first().locator('td[data-label="Last update"]');
  // The whole point: a recently-touched row whose data stopped 46 days ago.
  await expect(frozen).toContainText("Data 46d old");
  // Someone IS looking — so this is a dead upstream, not an abandoned job. The
  // two were indistinguishable before, and they need different people to act.
  await expect(frozen).toContainText("checked today");
  const detail = await frozen.locator("span.table-meta").getAttribute("title");
  expect(detail).toContain("Last outcome: source_not_updated");
  expect(detail).toContain("upstream max date still 2026-07-02");

  // No verdict language anywhere: Pages does not know any page's expected
  // cadence, so it reports the number and leaves the judgement to the reader.
  await expect(frozen).not.toContainText(/overdue|stale/i);

  // A page with no managed data renders no freshness line rather than zeroes.
  const unpublished = page.locator(".operation-table tbody tr", { hasText: "Client 04" }).first();
  await expect(unpublished.locator('td[data-label="Last update"]')).not.toContainText("Data ");

  // A page refreshed today shows its age and nothing about checking, because
  // checked_at equals refreshed_at and saying it twice is noise.
  const current = page.locator(".operation-table tbody tr", { hasText: "Client 01" }).first();
  await expect(current.locator('td[data-label="Last update"]')).toContainText("Data 1d old");
  await expect(current.locator('td[data-label="Last update"]')).not.toContainText("checked");
});

test("workspace manager is compact and creates, renames, and safely removes an empty workspace", async ({ page, request }) => {
  await request.post("/__fixture/empty-workspaces");
  await page.goto("/admin");
  const trigger = page.locator(".workspace-sidebar__head").getByRole("button", { name: "Manage workspaces" });
  await trigger.click();
  const manager = page.getByRole("dialog", { name: "Manage workspaces" });
  await expect(manager).toBeVisible();
  await expect(manager.getByRole("heading", { name: "No workspaces yet" })).toBeVisible();

  const dialogBox = await manager.boundingBox();
  const inputBox = await manager.getByLabel("Workspace name").boundingBox();
  const addBox = await manager.getByRole("button", { name: "Add workspace" }).boundingBox();
  const emptyBox = await manager.locator(".workspace-manager-list .state-panel").boundingBox();
  expect(dialogBox.width).toBeLessThan(800);
  expect(Math.abs(inputBox.y - addBox.y)).toBeLessThan(8);
  expect(emptyBox.y - (addBox.y + addBox.height)).toBeGreaterThan(8);
  await expectNoSeriousAxeViolations(page, "empty workspace manager");

  await manager.getByLabel("Workspace name").fill("Browser QA");
  await manager.getByRole("button", { name: "Add workspace" }).click();
  await expect(manager.getByText("Browser QA", { exact: true })).toBeVisible();
  await expect(page).toHaveURL(/workspace=3/);

  let row = manager.locator(".workspace-manager-row").filter({ hasText: "Browser QA" });
  await row.getByRole("button", { name: "Rename" }).click();
  await manager.getByLabel("New name for Browser QA").fill("Browser review");
  await manager.getByRole("button", { name: "Save" }).click();
  await expect(manager.getByText("Browser review", { exact: true })).toBeVisible();

  row = manager.locator(".workspace-manager-row").filter({ hasText: "Browser review" });
  await row.getByRole("button", { name: "Remove" }).click();
  const confirmation = page.getByRole("dialog", { name: "Remove Browser review?" });
  await expect(confirmation).toContainText("No pages will be deleted");
  await confirmation.getByRole("button", { name: "Remove workspace" }).click();
  await expect(manager.getByText("Browser review", { exact: true })).toHaveCount(0);
  await expect(page).toHaveURL(/workspace=ungrouped/);

  await manager.getByRole("button", { name: "Done" }).click();
  await expect(manager).toBeHidden();
  await expect(trigger).toBeFocused();
});

test("new-page and Cutlass workflows use focused dialogs, Escape, and locked revise slugs", async ({ page }) => {
  await page.goto("/admin");
  const newPage = page.locator(".page-heading__actions").getByRole("button", { name: "New page" });
  await newPage.click();
  const createDialog = page.getByRole("dialog", { name: "New page" });
  await createDialog.getByLabel("Slug").fill("browser/new-page");
  await createDialog.getByLabel("Title (optional)").fill("Browser-created page");
  await createDialog.getByLabel("Workspace").selectOption("1");
  await createDialog.getByRole("button", { name: "Create page" }).click();
  await expect(page).toHaveURL(/\/admin\/browser\/new-page$/);
  await expect(page.getByRole("heading", { level: 1, name: "Browser-created page" })).toBeVisible();

  await page.goto("/admin");
  const compose = page.getByRole("button", { name: "Compose with Cutlass" });
  await compose.click();
  const composeDialog = page.getByRole("dialog", { name: "Compose with Cutlass" });
  await composeDialog.getByLabel("Page brief").fill("Create a compact weekly client summary with three KPIs.");
  await composeDialog.getByLabel("Slug").fill("cutlass-fixture");
  await composeDialog.getByRole("button", { name: "Generate with Cutlass" }).click();
  await expect(composeDialog.getByRole("status")).toContainText("Published /cutlass-fixture");
  await page.keyboard.press("Escape");
  await expect(composeDialog).toBeHidden();
  await expect(compose).toBeFocused();

  const search = page.getByRole("searchbox", { name: "Search All pages" });
  await search.fill("long/client/q2-report");
  await page.getByRole("button", { name: "Revise" }).click();
  const revise = page.getByRole("dialog", { name: "Revise with Cutlass" });
  await expect(revise.getByLabel("Slug")).toHaveValue("long/client/q2-report");
  await expect(revise.getByLabel("Slug")).toHaveAttribute("readonly", "");
});

test("loading, recoverable error, and empty states give clear next steps", async ({ page, request }) => {
  await request.post("/__fixture/delay-next-pages", { data: { milliseconds: 600 } });
  await page.goto("/admin");
  await expect(page.getByText("Loading page operations…")).toBeVisible();
  await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();

  await request.post("/__fixture/fail-next-pages");
  await page.reload();
  // One wording for every screen's load failure now — "Couldn't load <noun>" —
  // and the guidance depends on whether the request reached the server at all.
  await expect(page.getByRole("heading", { name: "Couldn't load client pages" })).toBeVisible();
  await expect(page.getByText(/Try again, or reload if it keeps happening|Check your connection/)).toBeVisible();
  await page.getByRole("button", { name: "Try again" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();

  await request.post("/__fixture/empty");
  await page.reload();
  await expect(page.getByRole("heading", { name: "Nothing in All pages" })).toBeVisible();
  await expect(page.getByRole("button", { name: "New page" }).last()).toBeVisible();
  await expectNoSeriousAxeViolations(page, "empty page index");
});

for (const width of [280, 320, 390, 768, 834, 900, 1024, 1200, 1201, 1440]) {
  test(`index fits its operations list at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/admin");
    await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();
    // Exactly one way to manage workspaces at every width: the sidebar icon above
    // 1200px, the labelled control in the mobile picker at or below it. There used
    // to be a third in the page heading, visible at the same time as the sidebar.
    await expect(page.getByRole("button", { name: "Manage workspaces" })).toHaveCount(1);
    if (width <= 1200) {
      await expect(page.getByLabel("Current workspace")).toBeVisible();
      await page.getByLabel("Current workspace").selectOption("ungrouped");
      await expect(page).toHaveURL(/workspace=ungrouped/);
      if (width <= 640) {
        const pickerGeometry = await page.locator(".workspace-mobile").evaluate((picker) => {
          const selectBox = picker.querySelector("select").getBoundingClientRect();
          const manageBox = picker.querySelector("button").getBoundingClientRect();
          return {
            selectBottom: selectBox.bottom,
            manageBottom: manageBox.bottom,
            manageWidth: manageBox.width,
          };
        });
        expect(Math.abs(pickerGeometry.selectBottom - pickerGeometry.manageBottom), JSON.stringify(pickerGeometry)).toBeLessThan(2);
        expect(pickerGeometry.manageWidth, JSON.stringify(pickerGeometry)).toBeLessThanOrEqual(44);
      }
    }
    await expectNoHorizontalOverflow(page);
    const geometry = await page.locator(".operation-table-wrap").evaluate((wrap) => {
      const row = wrap.querySelector("tbody tr");
      const wrapBox = wrap.getBoundingClientRect();
      const rowBox = row.getBoundingClientRect();
      const pageBox = row.querySelector('td[data-label="Page"]').getBoundingClientRect();
      return {
        viewportWidth: document.documentElement.clientWidth,
        wrapRight: wrapBox.right,
        rowRight: rowBox.right,
        rowHeight: rowBox.height,
        rowWidth: rowBox.width,
        pageWidth: pageBox.width,
      };
    });
    expect(geometry.wrapRight, JSON.stringify(geometry)).toBeLessThanOrEqual(geometry.viewportWidth + 1);
    expect(geometry.rowRight, JSON.stringify(geometry)).toBeLessThanOrEqual(geometry.wrapRight + 1);
    expect(geometry.rowHeight, JSON.stringify(geometry)).toBeLessThan(500);
    if (width <= 768) {
      expect(geometry.pageWidth, JSON.stringify(geometry)).toBeGreaterThan(geometry.rowWidth * 0.9);
    }
  });
}

test("coarse-pointer index controls provide comfortable touch targets", async ({ browser, baseURL }) => {
  const context = await browser.newContext({
    baseURL,
    viewport: { width: 390, height: 844 },
    hasTouch: true,
  });
  const page = await context.newPage();
  try {
    await page.goto("/admin");
    await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();
    expect(await page.evaluate(() => matchMedia("(pointer: coarse)").matches)).toBe(true);
    const undersized = await page.evaluate(() =>
      Array.from(document.querySelectorAll(
        ".page-header .icon-action, .header-signout, .workspace-mobile button, .operation-table .btn, .operation-table select"
      )).flatMap((node) => {
        const rect = node.getBoundingClientRect();
        if (!rect.width || !rect.height || (rect.width >= 44 && rect.height >= 44)) return [];
        return [{
          label: node.getAttribute("aria-label") || node.textContent.trim(),
          width: rect.width,
          height: rect.height,
        }];
      })
    );
    expect(undersized).toEqual([]);
  } finally {
    await context.close();
  }
});

test("every admin screen's document title follows one pattern", async ({ page }) => {
  for (const [path, title] of [
    ["/admin", "Client pages · Pages"],
    ["/admin/templates", "Template library · Pages"],
    ["/admin/portals", "Partner portals · Pages"],
  ]) {
    await page.goto(path);
    await expect(page).toHaveTitle(title);
    // The tab name and the section tab name are the same string, so the two never drift.
    await expect(page.getByRole("navigation", { name: "Pages sections" })
      .getByRole("link", { name: title.replace(" · Pages", "") })).toHaveAttribute("aria-current", "page");
  }
});

for (const width of [1100, 900, 390]) {
  test(`index tables collapse into labelled cards at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto("/admin");
    await expect(page.locator(".operation-table").first()).toBeVisible();
    await expectCollapsedTablesAreLabelled(page, `index at ${width}px`);
    await expectNoHorizontalOverflow(page);
  });
}

test("workspace names survive the sidebar, and an empty one offers a way out", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/admin");
  // "Campaign operations" / "Executive reporting" are ordinary team names; the
  // 14rem column used to render them as "Campaign opera…".
  for (const name of ["Campaign operations", "Executive reporting"]) {
    const label = page.locator(".workspace-nav__label").filter({ hasText: name });
    await expect(label).toHaveText(name);
    const clipped = await label.evaluate((node) => node.scrollWidth > node.clientWidth + 1);
    expect(clipped, `${name} is truncated in the sidebar`).toBe(false);
  }

  // A workspace with no pages told you to move one here and gave you no way to.
  await page.locator(".workspace-nav").getByRole("button", { name: /Campaign operations/ }).click();
  await expect(page.locator(".operation-table tbody tr").first()).toBeVisible();
  await page.route("**/api/v1/admin/pages", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    await route.fulfill({ json: { ...body, pages: body.pages.map((p) => ({ ...p, workspace_id: null, workspace_name: null })) } });
  });
  await page.reload();
  await page.locator(".workspace-nav").getByRole("button", { name: /Campaign operations/ }).click();
  const escape = page.getByRole("button", { name: /Show all \d+ pages/ });
  await expect(escape).toBeVisible();
  await escape.click();
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(25);
});

test("timestamps say how stale a page is, and carry the exact moment they stand for", async ({ page }) => {
  const fresh = new Date(Date.now() - 5 * 60 * 1000).toISOString();
  const old = new Date(Date.now() - 40 * 24 * 60 * 60 * 1000).toISOString();
  await page.route("**/api/v1/admin/pages", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    const pages = body.pages.map((entry, index) => ({ ...entry, updated_at: index === 0 ? fresh : old }));
    await route.fulfill({ json: { ...body, pages } });
  });
  await page.goto("/admin");
  const cells = page.locator(".operation-table tbody tr td[data-label='Last update']");

  // Recent: the reader should not have to do arithmetic on a wall-clock string.
  const recent = cells.first().locator("time").first();
  await expect(recent).toHaveText("5 minutes ago");
  // ...but the exact moment stays available, and machine-readable.
  await expect(recent).toHaveAttribute("datetime", fresh);
  // Locale decides the order and the clock; the format decides the precision. So
  // assert a month, a year and a time — and never seconds — rather than an order.
  const title = await recent.getAttribute("title");
  expect(title).toMatch(/[A-Z][a-z]{2}/);
  expect(title).toMatch(/\d{4}/);
  expect(title).toMatch(/\d{1,2}:\d{2}/);
  expect(title).not.toMatch(/\d{1,2}:\d{2}:\d{2}/);

  // Past a week nobody counts days: show the date instead.
  const stale = await cells.nth(1).locator("time").first().textContent();
  expect(stale).toMatch(/[A-Z][a-z]{2}/);
  expect(stale).toMatch(/\d{4}/);
  expect(stale, "a date, not a time of day").not.toMatch(/\d{1,2}:\d{2}/);

  // Seconds were on every screen and are useful on none.
  const anySeconds = await page.locator(".operation-table time").evaluateAll((nodes) =>
    nodes.some((node) => /\d{1,2}:\d{2}:\d{2}/.test(node.textContent)));
  expect(anySeconds, "no timestamp may render seconds").toBe(false);
});

test("a page that has never been updated says so once, not four different ways", async ({ page }) => {
  await page.route("**/api/v1/admin/pages", async (route) => {
    const response = await route.fetch();
    const body = await response.json();
    await route.fulfill({ json: { ...body, pages: body.pages.map((entry) => ({ ...entry, updated_at: null })) } });
  });
  await page.goto("/admin");
  const cell = page.locator(".operation-table tbody tr td[data-label='Last update']").first();
  await expect(cell).toContainText("Never");
  // No <time> without a moment to point at.
  await expect(cell.locator("time")).toHaveCount(0);
});

test("when a page changed and how old its data is are two facts, not one run-on", async ({ page }) => {
  await page.goto("/admin");
  const cell = page.locator(".operation-table tbody tr td[data-label='Last update']").first();
  const when = cell.locator("time").first();
  const age = cell.locator("span.table-meta").first();
  await expect(when).toBeVisible();
  await expect(age).toBeVisible();

  // They used to render as "7/22/2026, 9:25:00 AMData 46d old" — inline siblings
  // with nothing between them. And within the age itself, "Data 46d old" and
  // "checked today" are two facts too: joined by a middot in a fixed column they
  // left a line ending on its own separator, so each takes a line.
  const [whenBox, ageBox] = await Promise.all([when.boundingBox(), age.boundingBox()]);
  expect(ageBox.y, "the data age must start below the timestamp, not beside it")
    .toBeGreaterThanOrEqual(whenBox.y + whenBox.height - 1);
  expect(await cell.innerText()).toMatch(/\n/);
  const lines = await age.locator(".table-meta__line").allInnerTexts();
  expect(lines, "each fact is its own line").toEqual(["Data 46d old", "checked today"]);
  for (const line of lines) {
    expect(line.trim(), "no line ends on its own separator").not.toMatch(/[·\u00b7]$/);
  }
});

test("the operations list opens on a window and loads the rest on request", async ({ page }) => {
  await page.goto("/admin");
  const rows = page.locator(".operation-table tbody tr");
  const count = page.locator(".workspace-index__count");
  await expect(rows).toHaveCount(25);
  await expect(count).toHaveText("Showing 25 of 37 pages");
  await expect(page.getByText("12 more pages")).toBeVisible();

  const more = page.getByRole("button", { name: "Show 12 more" });
  await more.click();

  await expect(rows).toHaveCount(37);
  await expect(count).toHaveText("37 pages");
  // Nothing left to load, so nothing offers to.
  await expect(page.getByRole("button", { name: /Show \d+ more/ })).toHaveCount(0);
  // Focus is not dropped on the floor when the button it was on disappears — it
  // goes to the first newly revealed row. It used to go to the count, which sits
  // in the toolbar at the top of the screen and took the viewport with it; the
  // test below is the one that measures that.
  await expect(page.locator(".operation-table tbody tr").nth(25).locator(".page-cell__title")).toBeFocused();
});

test("Show more reveals rows without throwing the reader back to the top", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/admin");
  const more = page.locator("#show-more-pages");
  await more.scrollIntoViewIfNeeded();
  const before = await page.evaluate(() => Math.round(window.scrollY));
  expect(before, "the button has to be below the fold for this to mean anything").toBeGreaterThan(400);

  await more.click();
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(37);

  // The press used to hand focus to the live-region count, which sits in the
  // toolbar at the very top — so .focus() scrolled the whole page back there.
  // Not an edge case: the LAST press always removes this button, and 37 pages
  // against a 25-row window means there is only ever one press.
  const after = await page.evaluate(() => ({
    y: Math.round(window.scrollY),
    focused: document.activeElement.className,
    text: document.activeElement.textContent,
  }));
  expect(Math.abs(after.y - before), `the viewport moved ${after.y - before}px`).toBeLessThanOrEqual(4);
  expect(after.focused, "focus must not land on the toolbar count").not.toContain("workspace-index__count");
  // It lands on the first newly revealed row, which renders exactly where the
  // button was standing — so keyboard users are put at the start of what just
  // appeared, and nobody's viewport moves.
  expect(after.focused).toContain("page-cell__title");
  expect(after.text).toBe("Client 25 operations dashboard");
});

test("a Show more that survives its own press does not drag the reader after it", async ({ page, request }) => {
  // The other branch: with enough pages the button survives its own press. It is
  // unreachable with the default fixture, and it is the branch a real account
  // lives in once it passes fifty pages.
  await request.post("/__fixture/pad-pages", { data: { count: 40 } });
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/admin");
  const more = page.locator("#show-more-pages");
  await more.scrollIntoViewIfNeeded();
  const before = await page.evaluate(() => Math.round(window.scrollY));

  await more.click();
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(50);
  // The button is still there — but 25 rows have been inserted above it, so it is
  // now a screenful further down. Following it threw the reader 1,856px the other
  // way, which is the same bug as the jump to the top, mirrored.
  await expect(more).toHaveCount(1);
  await expect(more).not.toBeFocused();
  const after = await page.evaluate(() => ({
    y: Math.round(window.scrollY),
    focused: document.activeElement.className,
    text: document.activeElement.textContent,
  }));
  expect(Math.abs(after.y - before), `the viewport moved ${after.y - before}px`).toBeLessThanOrEqual(4);
  expect(after.focused).toContain("page-cell__title");
  expect(after.text).toBe("Client 25 operations dashboard");
});

test("searching and switching workspace each start a fresh window", async ({ page }) => {
  await page.goto("/admin");
  await page.getByRole("button", { name: "Show 12 more" }).click();
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(37);

  // A new query is a new list: it must not inherit the previous window.
  await page.getByRole("searchbox", { name: /Search/ }).fill("client");
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(25);
  await page.getByRole("searchbox", { name: /Search/ }).fill("");
  await expect(page.locator(".operation-table tbody tr")).toHaveCount(25);

  await page.locator(".workspace-nav").getByRole("button", { name: /Campaign operations/ }).click();
  await expect(page.locator(".workspace-index__count")).toHaveText("13 pages");
  await expect(page.getByRole("button", { name: /Show \d+ more/ })).toHaveCount(0);
});

for (const width of [1024, 1100, 1200]) {
  test(`a ${width}px laptop gets the table, not the phone layout`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/admin");
    await expect(page.locator(".operation-table thead")).toBeVisible();
    const display = await page.locator(".operation-table").evaluate((node) => getComputedStyle(node).display);
    expect(display, "the operations list must stay a table at laptop widths").toBe("table");
    // It scrolls inside its own box rather than taking the page with it, and no
    // column — the workspace control included — is removed to make it fit.
    await expect(page.locator(".operation-table tbody tr").first()
      .locator(".workspace-assignment__button")).toBeVisible();
    await expectNoHorizontalOverflow(page);
  });
}

test("below the collapse the list is still cards", async ({ page }) => {
  await page.setViewportSize({ width: 900, height: 900 });
  await page.goto("/admin");
  const display = await page.locator(".operation-table").evaluate((node) => getComputedStyle(node).display);
  expect(display).toBe("block");
  await expectNoHorizontalOverflow(page);
});

test("a row is a title you can follow and at most one button per thing you might do", async ({ page }) => {
  await page.goto("/admin");
  const row = page.locator(".operation-table tbody tr").first();

  // The title is the link to the review screen; the row no longer needs a button
  // to say so, and with 37 rows a filled primary on each meant none was primary.
  const title = row.locator(".page-cell__title");
  await expect(title).toHaveAttribute("href", "/admin/long/client/q2-report");
  await expect(row.getByRole("button", { name: "Open", exact: true })).toHaveCount(0);
  await expect(row.getByRole("link", { name: "Open as staff" })).toHaveCount(0);

  const filled = await row.locator(".btn-primary").count();
  expect(filled, "no row may carry a filled primary button").toBe(0);

  // The live client page is named for the destination, not for the reader, and
  // opens in a new tab.
  const live = row.getByRole("link", { name: /View live/ });
  await expect(live).toHaveAttribute("href", "/view/long/client/q2-report");
  await expect(live).toHaveAttribute("target", "_blank");
  await expect(live).toHaveAttribute("rel", /noopener/);

  // No live <select> in a row: a stray wheel over a focused one was a data change.
  await expect(page.locator(".operation-table tbody select")).toHaveCount(0);

  // And the actions still fit on one line at 1440px.
  await page.setViewportSize({ width: 1440, height: 900 });
  const actions = row.locator(".row-actions");
  const box = await actions.boundingBox();
  const tallest = await actions.locator(".btn").first().boundingBox();
  expect(box.height, "the actions cluster must not wrap at 1440px").toBeLessThan(tallest.height * 1.6);
});

test("moving a page can be abandoned without moving it", async ({ page, request }) => {
  await page.goto("/admin");
  await page.getByRole("button", { name: /^Workspace for Client 01 operations dashboard/ }).click();
  const move = page.getByRole("dialog", { name: /^Move Client 01/ });
  await move.getByLabel("Workspace").selectOption("2");
  await move.getByRole("button", { name: "Cancel" }).click();
  await expect(move).toBeHidden();

  const events = (await (await request.get("/__fixture/events")).json()).events;
  expect(events.filter((event) => event.path.includes("/workspace"))).toHaveLength(0);
  await expect(page.getByRole("button", { name: /^Workspace for Client 01 operations dashboard/ }))
    .toHaveAccessibleName(/Campaign operations/);
});

test("moving a page from the keyboard does not drop focus to the body", async ({ page }) => {
  await page.goto("/admin");
  const control = page.getByRole("button", { name: /^Workspace for Client 01 operations dashboard/ });
  await control.focus();
  await page.keyboard.press("Enter");
  const move = page.getByRole("dialog", { name: /^Move Client 01/ });
  await move.getByLabel("Workspace").selectOption("2");
  await move.getByRole("button", { name: "Move page" }).click();
  await expect(page.getByRole("status").filter({ hasText: "Moved /client-01" })).toBeVisible();

  const landed = await page.evaluate(() => ({ body: document.activeElement === document.body, id: document.activeElement.id }));
  expect(landed.body, `focus fell to <body> (${JSON.stringify(landed)})`).toBe(false);
});

test("the same state is named the same way on every screen", async ({ page }) => {
  // A live, password-gated page. The index says it with a dot...
  await page.goto("/admin");
  const row = page.locator(".operation-table tbody tr").first();
  await expect(row.locator(".status")).toHaveText("Live · gated");
  await expect(row.locator(".status")).toHaveClass(/status--live/);

  // ...and the detail says exactly the same words, with the same tone.
  await page.goto("/admin/long/client/q2-report");
  const chip = page.locator(".page-overview__statuses .badge").first();
  await expect(chip).toHaveText("Live · gated");
  await expect(chip).toHaveClass(/badge--live/);
});

test("a status is never printed in the shape the database stores it", async ({ page }) => {
  for (const path of ["/admin", "/admin/templates", "/admin/portals", "/admin/long/client/q2-report"]) {
    await page.goto(path);
    // Wait for the screen itself; the portal list legitimately carries no chips
    // until a partner is selected, so scan whatever each screen does render.
    await expect(page.locator("#app h1, #app h2").first()).toBeVisible();
    const raw = await page.locator(".badge, .status").evaluateAll((nodes) =>
      nodes.map((node) => node.textContent.trim())
        // A raw enum: all lower case, one word, and one of the state names.
        .filter((text) => /^(pending|approved|rejected|draft|live|current|behind|disabled)$/.test(text)));
    expect(raw, `${path} renders a raw status string`).toEqual([]);
  }
});

test("the three list screens share one header and one list frame", async ({ page }) => {
  for (const [path, title] of [
    ["/admin", "Client pages"],
    ["/admin/templates", "Template library"],
    ["/admin/portals", "Partner portals"],
  ]) {
    await page.goto(path);
    const heading = page.getByRole("heading", { level: 1, name: title });
    await expect(heading).toBeVisible();

    // Every screen's title is in a <header class="page-heading">, and never inside
    // a bordered panel — partner portals used to put both in a card.
    const shape = await heading.evaluate((node) => {
      const header = node.closest("header.page-heading");
      return { inHeader: Boolean(header), inPanel: Boolean(node.closest(".panel")) };
    });
    expect(shape.inHeader, `${path}: the title must sit in header.page-heading`).toBe(true);
    expect(shape.inPanel, `${path}: the title must not sit inside a panel`).toBe(false);

    // And a table is framed once. The library wrapped .operation-table-wrap —
    // which already draws a border, radius and shadow — inside a .panel, so its
    // list had two. Assert the rendered result rather than the nesting: a wrap
    // inside a panel is fine as long as the panel is the only thing drawing a
    // frame around it.
    const framing = await page.locator(".operation-table-wrap").evaluateAll((wraps) =>
      wraps.map((wrap) => {
        const own = getComputedStyle(wrap);
        return {
          nested: wrap.closest(".panel") !== null,
          border: own.borderTopWidth,
          shadow: own.boxShadow,
        };
      }));
    for (const frame of framing) {
      if (!frame.nested) continue;
      expect(frame.border, `${path}: a nested list draws its own border on top of the panel's`).toBe("0px");
      expect(frame.shadow, `${path}: a nested list draws its own shadow on top of the panel's`).toBe("none");
    }
  }
});

test("every list screen shows the same kind of summary above its list", async ({ page }) => {
  for (const path of ["/admin", "/admin/templates", "/admin/portals"]) {
    await page.goto(path);
    const stats = page.locator(".page-heading .stats .stat");
    await expect(stats.first()).toBeVisible();
    // A count and a word for it, in that order, on all three.
    const shape = await stats.first().evaluate((node) => ({
      value: node.querySelector("strong")?.textContent,
      label: node.querySelector("span")?.textContent,
    }));
    expect(shape.value, `${path} stat has no number`).toMatch(/^\d+$/);
    expect(shape.label, `${path} stat has no label`).toBeTruthy();
  }
});

test("every screen reports a load failure the same way, and offers the same way out", async ({ page, request }) => {
  for (const [path, noun] of [
    ["/admin", "client pages"],
    ["/admin/templates", "the template library"],
    ["/admin/portals", "partner portals"],
  ]) {
    // The template library and partner portals used to report the failure and stop:
    // no retry at all, and portals said "Could not" where everything else said
    // "Couldn't".
    await page.route("**/api/v1/admin/**", (route) => route.fulfill({ status: 503, json: { error: "the database is asleep" } }));
    await page.goto(path);
    await expect(page.getByRole("heading", { name: `Couldn't load ${noun}` })).toBeVisible();
    await expect(page.getByText("the database is asleep")).toBeVisible();
    const retry = page.getByRole("button", { name: "Try again" });
    await expect(retry, `${path} must offer a way out`).toBeVisible();

    // And the way out works.
    await page.unroute("**/api/v1/admin/**");
    await retry.click();
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.getByRole("heading", { name: `Couldn't load ${noun}` })).toHaveCount(0);
  }
});

test("the admin screens hold at axe's moderate level, not just serious", async ({ page }) => {
  // Landmark, heading-order and region findings never show up in a screenshot and
  // never fail a functional test, which is how a duplicate <main> and an <h2>
  // under an <h3> both survived. The content-host gates keep the serious bar for
  // now; they are a separate batch.
  for (const [path, ready] of [
    ["/admin", ".operation-table"],
    ["/admin/templates", "#template-list"],
    ["/admin/portals", ".operation-table"],
    ["/admin/long/client/q2-report", ".version-option"],
  ]) {
    await page.goto(path);
    await expect(page.locator(ready).first()).toBeVisible();
    await expectNoSeriousAxeViolations(page, `${path} at moderate`, { level: "moderate" });
  }
});

test("the template detail holds at moderate once it is open", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  await expect(page.locator("#detail")).toBeVisible();
  await expectNoSeriousAxeViolations(page, "template detail at moderate", { level: "moderate" });
});

// The index is the screen everyone opens first, and it read as unfinished: three
// mismatched boxes for three counts, a bordered card round the workspace rail
// with a second border round the selected item inside it, two page headers, and
// a table whose own column headings wrapped. These pin the decisions, not the
// pixels — each one is a rule about what may look like what.

test("a count and the table it summarises are not framed the same way", async ({ page }) => {
  await page.goto("/admin");
  const frames = await page.evaluate(() => {
    const read = (selector) => {
      const style = getComputedStyle(document.querySelector(selector));
      return { shadow: style.boxShadow, radius: style.borderTopLeftRadius, top: style.borderTopWidth };
    };
    return { stat: read(".stat"), rail: read(".workspace-sidebar"), table: read(".operation-table-wrap") };
  });
  // The list is the object on this page. It gets the frame.
  expect(frames.table.shadow).not.toBe("none");
  expect(frames.table.radius).not.toBe("0px");
  // A count is a line of text. A filter rail is part of the page. Neither is an
  // object, and giving all three the same border, radius and shadow is what made
  // nothing on the screen read as primary.
  for (const [name, frame] of [["stat", frames.stat], ["rail", frames.rail]]) {
    expect(frame.shadow, `${name} draws a card shadow`).toBe("none");
    expect(frame.radius, `${name} draws a card radius`).toBe("0px");
    expect(frame.top, `${name} draws a card border`).toBe("0px");
  }
});

test("the selected workspace is marked, not boxed", async ({ page }) => {
  await page.goto("/admin");
  const selected = page.locator('.workspace-nav__item[aria-pressed="true"]');
  const marking = await selected.evaluate((node) => {
    const style = getComputedStyle(node);
    const bar = getComputedStyle(node, "::before");
    return {
      border: style.borderTopWidth,
      fill: style.backgroundColor,
      barWidth: bar.width,
      barColour: bar.backgroundColor,
    };
  });
  // A border round the selected item made it a second box inside the rail's box.
  expect(marking.border, "selection must not draw a border").toBe("0px");
  // It is still unmistakably selected: an accent bar and a fill, not fill alone.
  expect(marking.fill).not.toBe("rgba(0, 0, 0, 0)");
  expect(marking.barWidth).toBe("2px");
  expect(marking.barColour).not.toBe("rgba(0, 0, 0, 0)");
});

test("no column heading wraps, and no row action falls to a second line", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto("/admin");
  // A column narrower than its own heading breaks it over two lines and sets a
  // ragged top edge across the whole header row — "LIVE VERSION" did, in 6.5rem.
  // Cell HEIGHT cannot answer this: every th in a row stretches to the tallest, so
  // comparing them is vacuous. A Range over the text returns one rect per rendered
  // line, which is the actual question.
  const heads = await page.locator(".operation-table thead th").evaluateAll((cells) =>
    cells.map((cell) => {
      const range = document.createRange();
      range.selectNodeContents(cell);
      return { label: cell.textContent, lines: range.getClientRects().length };
    })
  );
  for (const head of heads) {
    expect(head.lines, `"${head.label}" wraps over ${head.lines} lines`).toBe(1);
  }
  // And what the fixed columns take, the title column loses — so the actions must
  // fit their own column, or every row grows a line to hold them.
  const actions = await page.locator(".operation-table tbody .row-actions").first().evaluate((node) => {
    const child = node.firstElementChild.getBoundingClientRect();
    return { row: Math.round(node.getBoundingClientRect().height), item: Math.round(child.height) };
  });
  expect(actions.row, "the row actions wrapped onto a second line").toBeLessThanOrEqual(actions.item + 2);
});

test("every screen stacks its sections at the same rhythm", async ({ page }) => {
  // The index alone never carried the section-stack class, so its heading ran
  // straight into the layout below with no gap — which is why its summary line
  // sat on top of the workspace rail.
  const gaps = [];
  for (const path of ["/admin", "/admin/templates", "/admin/portals", "/admin/client-01"]) {
    await page.goto(path);
    await expect(page.locator("#app")).toBeVisible();
    gaps.push([path, await page.locator("#app").evaluate((node) => getComputedStyle(node).rowGap)]);
  }
  for (const [path, gap] of gaps) {
    expect(gap, `${path} has no section gap: ${JSON.stringify(gaps)}`).not.toBe("normal");
    expect(gap, `${path} has no section gap`).not.toBe("0px");
  }
  expect(new Set(gaps.map(([, gap]) => gap)).size, `screens disagree: ${JSON.stringify(gaps)}`).toBe(1);
});

test("the list toolbar is one line, not a second page header", async ({ page }) => {
  await page.goto("/admin");
  // An overline, an h2 and a caption stacked in a column is a whole second page
  // header under the real one, and the reader met two titles for one screen.
  await expect(page.locator(".index-toolbar__copy .overline")).toHaveCount(0);
  const line = await page.evaluate(() => {
    const scope = document.querySelector(".index-toolbar__copy h2").getBoundingClientRect();
    const count = document.querySelector(".workspace-index__count").getBoundingClientRect();
    return { scope: Math.round(scope.bottom), count: Math.round(count.bottom) };
  });
  expect(Math.abs(line.scope - line.count), "the scope and its count are one fact, on one line").toBeLessThanOrEqual(4);
});
