// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The partner portal screen is the ONLY surface that changes which dashboards a
// client credential opens, so what it says at the moment of a decision matters as
// much as that it works. These run against the real shell, the real browser
// module, and the real Flag assets.

const { test, expect } = require("@playwright/test");
const { resetFixture, expectNoHorizontalOverflow, expectNoSeriousAxeViolations, expectCollapsedTablesAreLabelled } = require("./helpers");

// Creating and retiring portals mutates module state on a fixture server the whole
// run shares, so what the list and the dashboard index hold is otherwise a function
// of test order — these specs ran with whatever page index admin-index.spec.js
// happened to leave behind, including the emptied one.
test.beforeEach(async ({ request }) => {
  await resetFixture(request);
  await request.post("/__fixture/reset-portals");
});

test("the portal list and its detail render, with the partner link to hand out", async ({ page }) => {
  await page.goto("/admin/portals");
  await expect(page.getByRole("heading", { level: 1, name: "Partner portals" })).toBeVisible();
  // The first portal is selected without a click, because a screen that needs one
  // before showing anything reads as empty.
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await expect(page.getByText("/live/portal/nwm")).toBeVisible();
  await expect(page.getByRole("button", { name: "Copy link" }).first()).toBeVisible();
  // Every warning the UI can show about a member is on screen for the fixture set.
  await expect(page.getByText("Taken down — hidden from this partner while it is disabled.")).toBeVisible();
  await expect(page.getByText("No password of its own: this portal is what makes it readable.")).toBeVisible();
  // The old "served raw, no Page menu, redeploy as themed" note is GONE on
  // purpose: since #125 raw renders get the injected menu too, so the warning
  // was advising admins to redeploy pages that are fine.
  await expect(page.getByText("shows no Page menu", { exact: false })).toHaveCount(0);
  // The link audit: live pages the home dashboard links to but nobody added as
  // members — the drift that silently drops the nav behind a hub link.
  await expect(page.getByText("Linked from the home page, but not members")).toBeVisible();
  await expect(page.getByTestId("portal-link-audit-nwm-lakeside")).toContainText("Lakeside campaign");
  await expect(page.getByTestId("portal-link-audit-nwm-lakeside")).toContainText("/nwm-lakeside");
  await expect(page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button", { name: "Add to portal" })).toBeVisible();
  // The reclassification is stated BEFORE any click, the way the file's opening
  // invariant requires: the audit payload has no has_password, so it is said
  // about all of them rather than per row.
  await expect(page.getByTestId("portal-link-audit"))
    .toContainText("becomes readable by everyone holding this portal's password");
  // …and the count says what the partner actually sees, not how many rows exist.
  await expect(page.getByText("3 pages in this order, 2 of them visible to this partner", { exact: false }))
    .toBeVisible();
  await expectNoSeriousAxeViolations(page, "/admin/portals");
});

// ── #155: the audit is a list of dashboards, not a right-aligned drift ──────
// Every missing page used to be an el("div", { class: "row-actions" }) — the
// table CELL cluster, justify-content: flex-end — so the title and its button
// floated to the far right edge of the panel with nothing aligning them to the
// member table directly above. These pin the anatomy that replaced it.
test.describe("the link audit", () => {
  test.beforeEach(async ({ request }) => {
    await request.post("/__fixture/reset-portals");
  });
  test.afterEach(async ({ request }) => {
    await request.post("/__fixture/reset-portals");
  });

  const memberTable = (page) => page.locator(".operation-table").nth(1);
  const auditTable = (page) => page.locator(".operation-table").nth(2);
  const memberOrder = (page) => memberTable(page).locator("tbody td:first-child strong").allTextContents();

  test("the audit reads as a table with the member table's own columns", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    // The same two columns, so the two lists read as one column of dashboards.
    await expect(auditTable(page).getByRole("columnheader")).toHaveText(["Page", "Actions"]);
    // A table's caption is its accessible name, so the audit cannot be confused
    // with the membership table sitting directly above it.
    await expect(auditTable(page).locator("caption")).toContainText("2 linked pages are not in this portal.");
    await expect(auditTable(page).locator("tbody tr")).toHaveCount(2);
  });

  for (const width of [1440, 390]) {
    test(`the audit rows line up with the member table at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1400 });
      await page.goto("/admin/portals");
      await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

      const memberRow = memberTable(page).locator("tbody tr").first();
      const auditRow = page.getByTestId("portal-link-audit-nwm-lakeside");
      // The title, which flex-end used to push to the far right of the panel.
      const memberTitle = await memberRow.locator("td:first-child strong").boundingBox();
      const auditTitle = await auditRow.locator("td:first-child strong").boundingBox();
      expect(
        Math.abs(auditTitle.x - memberTitle.x),
        `the audit title starts at ${auditTitle.x}, the member title at ${memberTitle.x}`
      ).toBeLessThanOrEqual(1);

      // …and the action, which now ends where the member row's actions end
      // instead of at the panel's own edge.
      const memberCell = await memberRow.locator("td").last().boundingBox();
      const auditCell = await auditRow.locator("td").last().boundingBox();
      expect(Math.abs(auditCell.x - memberCell.x), "the Actions columns must share an edge").toBeLessThanOrEqual(1);
      expect(Math.abs(auditCell.width - memberCell.width), "the Actions columns must share a width").toBeLessThanOrEqual(1);
      await expectNoHorizontalOverflow(page);
    });
  }

  test("adding one from the audit says what it did, and clears its row", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    // The staff-only one: adding it is what makes it readable with this portal's
    // password, and the row used to just vanish with nothing said at all.
    await page.getByTestId("portal-link-audit-nwm-mars-petcare")
      .getByRole("button", { name: "Add to portal" }).click();
    await expect(page.getByRole("status").filter({ hasText: "Tailspin Pet Q3 is now readable with this portal's password." }))
      .toBeVisible();
    await expect.poll(() => memberOrder(page))
      .toEqual(["Portfolio overview", "Contoso Allergex", "Taken down", "Tailspin Pet Q3"]);
    // It is a member now, so it is no longer drift.
    await expect(page.getByTestId("portal-link-audit-nwm-mars-petcare")).toHaveCount(0);
    await expect(page.getByTestId("portal-link-audit")).toBeVisible();
  });

  test("a failed add restores its button and says why", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    await page.route("**/portals/7/pages", (route) => route.fulfill({
      status: 500,
      contentType: "application/json",
      body: JSON.stringify({ error: "server exploded", code: "server_error" }),
    }));
    const button = page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button", { name: "Add to portal" });
    await button.click();
    await expect(page.getByRole("alert").filter({ hasText: "server exploded" })).toBeVisible();
    // The old handler disabled event.target by hand and re-enabled it in a catch;
    // runAction restores the label as well, so the row stays operable.
    await expect(button).toBeEnabled();
    await expect(button).toHaveText("Add to portal");
  });

  test("Add all takes the whole audit, after a confirmation that names it", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    // Every sort_order the adds ask for: one appendPlan per page would read the
    // same unchanged member list every time and send 3 twice, which is the shared
    // position #173 exists to remove.
    const sent = [];
    await page.route("**/portals/7/pages", (route) => {
      sent.push(route.request().postDataJSON());
      return route.continue();
    });

    await page.getByTestId("portal-link-audit-add-all").click();
    const confirm = page.getByRole("dialog");
    await expect(confirm).toContainText("Lakeside campaign, Tailspin Pet Q3");
    await expect(confirm).toContainText("becomes readable by everyone holding this portal's password");
    await confirm.getByRole("button", { name: "Add all 2" }).click();

    await expect.poll(() => memberOrder(page))
      .toEqual(["Portfolio overview", "Contoso Allergex", "Taken down", "Lakeside campaign", "Tailspin Pet Q3"]);
    expect(sent.map((body) => body.sort_order)).toEqual([3, 4]);
    // Only the staff-only one is named as reclassified, and it is named: with the
    // rows gone there is nothing left on screen to carry the news.
    await expect(page.getByRole("status").filter({ hasText: "Added 2 pages." })).toBeVisible();
    await expect(page.getByRole("status").filter({ hasText: "Tailspin Pet Q3 is now readable" })).toBeVisible();
    await expect(page.getByRole("status").filter({ hasText: "Lakeside campaign is now readable" })).toHaveCount(0);
    // Nothing left to audit.
    await expect(page.getByTestId("portal-link-audit")).toHaveCount(0);
  });

  test("Add all is not offered over a single row, whose own button is Add all", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    await page.getByTestId("portal-link-audit-nwm-lakeside")
      .getByRole("button", { name: "Add to portal" }).click();
    await expect(page.getByTestId("portal-link-audit-nwm-lakeside")).toHaveCount(0);
    await expect(page.getByTestId("portal-link-audit-add-all")).toHaveCount(0);
    await expect(auditTable(page).locator("caption")).toContainText("1 linked page is not in this portal.");
  });

  test("a part-way failure of Add all leaves the screen showing what the server holds", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    let adds = 0;
    await page.route("**/portals/7/pages", (route) => {
      adds += 1;
      if (adds !== 2) return route.continue();
      return route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ error: "server exploded", code: "server_error" }),
      });
    });

    await page.getByTestId("portal-link-audit-add-all").click();
    await page.getByRole("dialog").getByRole("button", { name: "Add all 2" }).click();
    await expect(page.getByRole("alert").filter({ hasText: "server exploded" })).toBeVisible();
    // The first add really happened. A screen still listing it as drift would
    // send the next click at a page that is already a member.
    await expect.poll(() => memberOrder(page))
      .toEqual(["Portfolio overview", "Contoso Allergex", "Taken down", "Lakeside campaign"]);
    await expect(page.getByTestId("portal-link-audit-nwm-lakeside")).toHaveCount(0);
    await expect(page.getByTestId("portal-link-audit-nwm-mars-petcare")).toBeVisible();
  });

  test("each row's action names the dashboard it would add", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    // A column of identical "Add to portal" buttons tells a screen reader nothing
    // about which one it is standing on.
    await expect(page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button"))
      .toHaveAttribute("aria-label", "Add to portal: Lakeside campaign");
    await expect(page.getByTestId("portal-link-audit-nwm-mars-petcare").getByRole("button"))
      .toHaveAttribute("aria-label", "Add to portal: Tailspin Pet Q3");
    await expectNoSeriousAxeViolations(page, "/admin/portals link audit");
  });

  // ── #209 review ──────────────────────────────────────────────────────────
  test("the caption names the table without repeating the paragraph above it", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    const caption = auditTable(page).locator("caption");
    // Still the table's accessible name — that is its whole job, and it is what a
    // screen reader announces on entry.
    await expect(auditTable(page)).toHaveAccessibleName(/2 linked pages are not in this portal\./);
    // …but not a second visible paragraph. It said the count the two rows already
    // say, and then explained to the NEXT DEVELOPER why the section exists.
    await expect(caption).toHaveClass(/sr-only/);
    await expect(page.getByTestId("portal-link-audit")).not.toContainText("one click shorter");
  });

  test("the reclassification warning is said in the dialog's voice, not in the quietest one", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    const section = page.getByTestId("portal-link-audit");
    const warning = section.locator("p.note--warning");
    await expect(warning)
      .toHaveText("Any of them with no client password of its own becomes readable by everyone holding this portal's password.");
    // Buried in the descriptive paragraph it rendered as plain .note: muted, at
    // caption size, the least emphasised text in the panel — for the one sentence
    // on this screen about who is allowed to read a page. The dialog says the
    // identical thing in .note--warning, and one statement gets one treatment.
    const colourOf = (locator) => locator.evaluate((node) => getComputedStyle(node).color);
    expect(await colourOf(warning)).not.toBe(await colourOf(section.locator("p.note:not(.note--warning)")));
  });

  for (const width of [1440, 390]) {
    test(`both section actions sit on the same edge at ${width}px`, async ({ page }) => {
      await page.setViewportSize({ width, height: 1400 });
      await page.goto("/admin/portals");
      await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
      const addPage = await page.getByRole("button", { name: "Add a page" }).boundingBox();
      const addAll = await page.getByTestId("portal-link-audit-add-all").boundingBox();
      // They never shared a left x — the h3 beside each is a different length —
      // they share the panel's right edge. At 390 the audit's longer heading wraps
      // the pair, and `justify-content: space-between` drops a lone wrapped item
      // to flex-start, so the action left-aligned while its sibling stayed right
      // and the two headings stopped reading the same way.
      expect(
        Math.abs((addAll.x + addAll.width) - (addPage.x + addPage.width)),
        `Add all ends at ${addAll.x + addAll.width}, Add a dashboard at ${addPage.x + addPage.width}`
      ).toBeLessThanOrEqual(1);
      await expectNoHorizontalOverflow(page);
    });
  }

  test("a part-way failure of Add all still confirms the add that landed", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    let adds = 0;
    await page.route("**/portals/7/pages", async (route) => {
      adds += 1;
      if (adds > 1) {
        return route.fulfill({
          status: 500,
          contentType: "application/json",
          body: JSON.stringify({ error: "server exploded", code: "server_error" }),
        });
      }
      // The add really happens; only the flag is forced. The fixture's first
      // linked page has a password of its own, and the case that matters is the
      // one where the add RECLASSIFIES — which only the response can report.
      const response = await route.fetch();
      const body = await response.json();
      return route.fulfill({ response, json: { ...body, reclassifies_staff_only: true } });
    });

    await page.getByTestId("portal-link-audit-add-all").click();
    await page.getByRole("dialog").getByRole("button", { name: "Add all 2" }).click();

    // The first add is committed server-side and its audit row has already been
    // re-read away, so this toast is the ONLY place left that can say a staff-only
    // dashboard just became readable by everyone holding this portal's password.
    // Discarding the collected results left it said nowhere at all.
    const failure = page.getByRole("alert").filter({ hasText: "server exploded" });
    await expect(failure).toContainText("the rest were not added");
    await expect(failure).toContainText("Lakeside campaign is now readable with this portal's password.");
  });

  test("Add all holds the whole section, so no row can be added twice", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    let release;
    const held = new Promise((resolve) => { release = resolve; });
    let adds = 0;
    await page.route("**/portals/7/pages", async (route) => {
      adds += 1;
      if (adds === 1) await held;
      return route.continue();
    });

    await page.getByTestId("portal-link-audit-add-all").click();
    await page.getByRole("dialog").getByRole("button", { name: "Add all 2" }).click();
    // runAction only ever disables the button it was handed. A row acted on here
    // sends a second POST for a page the batch is already adding, which the API
    // refuses as portal_page_exists — aborting the rest of the batch and silently
    // skipping what was left of it.
    await expect(page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button")).toBeDisabled();
    await expect(page.getByTestId("portal-link-audit-nwm-mars-petcare").getByRole("button")).toBeDisabled();
    release();
    await expect(page.getByTestId("portal-link-audit")).toHaveCount(0);
    expect(adds).toBe(2);
  });

  test("a row's add holds the section too, so Add all cannot join it mid-flight", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    let release;
    const held = new Promise((resolve) => { release = resolve; });
    await page.route("**/portals/7/pages", async (route) => {
      await held;
      return route.continue();
    });

    await page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button", { name: "Add to portal" }).click();
    await expect(page.getByTestId("portal-link-audit-add-all")).toBeDisabled();
    await expect(page.getByTestId("portal-link-audit-nwm-mars-petcare").getByRole("button")).toBeDisabled();
    release();
    // …and the lock lifts with the re-read, rather than leaving a dead row behind.
    await expect(page.getByTestId("portal-link-audit-nwm-lakeside")).toHaveCount(0);
    await expect(page.getByTestId("portal-link-audit-nwm-mars-petcare").getByRole("button")).toBeEnabled();
  });

  test("a failed add lifts the lock it took", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    await page.route("**/portals/7/pages", (route) => route.fulfill({
      status: 500,
      contentType: "application/json",
      body: JSON.stringify({ error: "server exploded", code: "server_error" }),
    }));
    await page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button", { name: "Add to portal" }).click();
    await expect(page.getByRole("alert").filter({ hasText: "server exploded" })).toBeVisible();
    // A failure re-renders nothing, so the section-wide disable has to be undone
    // by hand — a row of dead controls is worse than the race it guarded against.
    await expect(page.getByTestId("portal-link-audit-add-all")).toBeEnabled();
    await expect(page.getByTestId("portal-link-audit-nwm-mars-petcare").getByRole("button")).toBeEnabled();
  });

  test("a successful add leaves focus in the page, not on <body>", async ({ page }) => {
    const focusedId = () => page.evaluate(() => (document.activeElement && document.activeElement.id) || document.activeElement.tagName);
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    // setBusy disables the pressed button before the rebuild, so the browser has
    // already blurred it and keepingFocus has no id left to restore — focus fell
    // to <body>, at the top of the document, on a screen only ever driven by hand.
    await page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button", { name: "Add to portal" }).focus();
    await page.keyboard.press("Enter");
    await expect(page.getByTestId("portal-link-audit-nwm-lakeside")).toHaveCount(0);
    await expect.poll(focusedId).toBe("portal-link-audit-heading");

    // Adding the last one deletes the section the user was standing in, so focus
    // lands on the heading of the table the dashboard just joined.
    await page.getByTestId("portal-link-audit-nwm-mars-petcare").getByRole("button", { name: "Add to portal" }).click();
    await expect(page.getByTestId("portal-link-audit")).toHaveCount(0);
    await expect.poll(focusedId).toBe("portal-members-heading");
    await expectNoSeriousAxeViolations(page, "/admin/portals after an add");
  });
});

test("the home dashboard is marked, and cannot be re-set to itself", async ({ page }) => {
  await page.goto("/admin/portals");
  const homeRow = page.getByRole("row").filter({ hasText: "Portfolio overview" });
  await expect(homeRow.getByText("Home")).toBeVisible();
  await expect(homeRow.getByRole("button", { name: "Make home" })).toHaveCount(0);
  // A taken-down dashboard cannot become the landing page a partner is sent to.
  const downRow = page.getByRole("row").filter({ hasText: "Taken down" });
  await expect(downRow.getByRole("button", { name: "Make home" })).toHaveCount(0);
});

test("adding a staff-only page warns BEFORE the click, and confirms after", async ({ page }) => {
  await page.goto("/admin/portals");
  await page.getByRole("button", { name: "Add a page" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();

  // Nothing chosen yet: no warning to react to.
  const notice = page.locator("#portal-reclassify-notice");
  await expect(notice).toBeHidden();

  // A page that already has its own client password is not a reclassification.
  // The list is fetched when the dialog opens (#166), so wait for it to arrive
  // rather than reading the placeholder.
  const picker = dialog.getByLabel("Page", { exact: true });
  // The picker stays ENABLED while it loads (disabled option text is illegible in
  // the light theme), so what says the list has ARRIVED is its status line.
  await expect(dialog.getByTestId("portal-add-page-status")).toContainText("to choose from");
  const options = await picker.locator("option").allTextContents();
  const withPassword = options.find((label) => label.includes("—") && !label.includes("staff-only"));
  await picker.selectOption({ label: withPassword });
  await expect(notice).toBeHidden();

  // One without is: it is staff-only today, and adding it here is what changes that.
  const staffOnly = options.find((label) => label.includes("staff-only"));
  await picker.selectOption({ label: staffOnly });
  await expect(notice).toBeVisible();
  await expect(notice).toContainText("no client password of its own");
  await expect(notice).toContainText("readable by everyone holding this portal's password");

  await dialog.getByRole("button", { name: "Add page", exact: true }).click();
  await expect(page.getByRole("status").filter({ hasText: "now readable with this portal's password" })).toBeVisible();
});

test("a new portal's password is shown once, in a dialog that has to be dismissed", async ({ page }) => {
  await page.goto("/admin/portals");
  await page.getByRole("button", { name: "New portal" }).click();
  const create = page.getByRole("dialog");
  await create.getByLabel("Partner name").fill("Fabrikam — West");
  // The slug is derived from the name, because it lands in a bookmark forever and
  // a typo there is permanent.
  await expect(create.getByLabel("URL slug")).toHaveValue("fabrikam-west");
  await create.getByRole("button", { name: "Create portal" }).click();

  const credential = page.getByRole("dialog").filter({ hasText: "Copy this now" });
  await expect(credential).toBeVisible();
  await expect(credential).toContainText("only time Pages will show this password");
  await expect(credential.getByText("pr7k-9mfx-t2qd-w4hn")).toBeVisible();
  await expect(credential).toContainText("a forwarded password is as good as the holder's");
  // It is modal: the screen behind it cannot be used until it is acknowledged.
  await expect(credential.getByRole("button", { name: "Done" })).toBeVisible();
});

test("retiring and rotating both say what they do to a partner mid-session", async ({ page }) => {
  await page.goto("/admin/portals");
  await page.getByRole("button", { name: "Rotate password" }).click();
  const rotate = page.getByRole("dialog");
  await expect(rotate).toContainText("signed out immediately");
  await rotate.getByRole("button", { name: "Cancel" }).click();

  await page.getByRole("button", { name: "Retire portal" }).click();
  const retire = page.getByRole("dialog");
  await expect(retire).toContainText("link stops working immediately");
  await expect(retire).toContainText("no page is deleted");
});

test("the reclassification notice is styled as a warning, not as dead markup", async ({ page }) => {
  await page.goto("/admin/portals");
  await page.getByRole("button", { name: "Add a page" }).click();
  const notice = page.locator("#portal-reclassify-notice");
  await expect(notice).toHaveClass(/note--warning/);

  // A rename to a selector shell.css never defines looks identical in the DOM
  // and silently unstyled on screen, which is how .template-code shipped dead.
  const warning = await notice.evaluate((node) => getComputedStyle(node).color);
  const plain = await page.evaluate(() => {
    const probe = document.createElement("p");
    probe.className = "note";
    document.body.append(probe);
    const color = getComputedStyle(probe).color;
    probe.remove();
    return color;
  });
  expect(warning, ".note--warning must resolve to a different colour than .note").not.toBe(plain);
});

// ── #166: the three things the dialogs and the selection got wrong ──────────

test("Enter submits a portal dialog, the way it does everywhere else in the admin", async ({ page }) => {
  await page.goto("/admin/portals");
  // Edit membership: type a label, press Enter, and the dialog does what its
  // primary action does. Before #166 the fields sat in a plain div behind a
  // type="button", so this keystroke went nowhere.
  await page.getByRole("row").filter({ hasText: "Contoso Allergex" }).getByRole("button", { name: "Edit" }).click();
  const edit = page.getByRole("dialog");
  await expect(edit).toBeVisible();
  await edit.getByLabel("Label for the partner").fill("Allergex — allergy season");
  await edit.getByLabel("Label for the partner").press("Enter");
  await expect(page.getByRole("status").filter({ hasText: "Membership updated." })).toBeVisible();
  await expect(page.getByRole("dialog")).toHaveCount(0);

  // …and the same in Rename, which is a different dialog through the same
  // formDialog. The value is what it already was, so no later test is disturbed.
  await page.getByRole("button", { name: "Rename" }).click();
  const rename = page.getByRole("dialog");
  await rename.getByLabel("Partner name").fill("Northwind Media Group");
  await rename.getByLabel("Partner name").press("Enter");
  await expect(page.getByRole("status").filter({ hasText: "Portal renamed." })).toBeVisible();
  await expect(page.getByRole("dialog")).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
});

test("a superseded portal response never lands under the row that is selected", async ({ page, request }) => {
  await page.goto("/admin/portals");
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

  // Fabrikam's detail is held back long enough to land AFTER the click that
  // supersedes it: the exact shape that used to show one partner's dashboards,
  // rotate button and retire button under another partner's highlighted row.
  await request.post("/__fixture/delay-portal-detail", { data: { id: 9, milliseconds: 1500 } });
  await page.getByRole("button", { name: "Fabrikam", exact: true }).click();
  await page.getByRole("button", { name: "Northwind Media Group", exact: true }).click();
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

  // Well past the delayed response's arrival.
  await page.waitForTimeout(2500);
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await expect(page.getByText("Fabrikam SSP weekly")).toHaveCount(0);
  await expect(page.getByText("Portfolio overview")).toBeVisible();
  // The highlight and the detail agree, and so does the URL.
  await expect(page.getByRole("row").filter({ hasText: "Northwind Media Group" })).toHaveAttribute("aria-current", "true");
  await expect(page).toHaveURL(/[?&]portal=7\b/);
});

test("a retired portal leaves the list, even if a row is clicked while it reloads", async ({ page, request }) => {
  await page.goto("/admin/portals");
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await expect(page.locator("caption").first()).toHaveText("2 portals");

  // Retire, then click the OTHER portal while the reload's list is still in
  // flight. The click supersedes the detail that reload asked for; it must not
  // also discard the list, which is what left a retired partner listed, counted
  // and clickable under "Portal retired."
  await request.post("/__fixture/delay-portal-list", { data: { milliseconds: 1500 } });
  const listInFlight = page.waitForRequest((candidate) =>
    /\/api\/v1\/admin\/portals(\?|$)/.test(candidate.url()) && candidate.method() === "GET");
  await page.getByRole("button", { name: "Retire portal" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Retire portal" }).click();
  await listInFlight;
  await page.getByRole("button", { name: "Fabrikam", exact: true }).click();
  await expect(page.getByRole("status").filter({ hasText: "Portal retired." })).toBeVisible();

  // Once the held list lands, the retired partner is gone from every place the
  // screen counts it, and the surviving portal is what is rendered.
  await expect(page.locator("caption").first()).toHaveText("1 portal");
  await expect(page.getByRole("row").filter({ hasText: "Northwind Media Group" })).toHaveCount(0);
  await expect(page.getByRole("heading", { level: 2, name: "Fabrikam" })).toBeVisible();
  await expect(page.getByRole("row").filter({ hasText: "/portal/fabrikam" })).toHaveAttribute("aria-current", "true");
});

test("the selected portal survives a reload, and Back returns to the previous one", async ({ page }) => {
  await page.goto("/admin/portals");
  await expect(page).toHaveURL(/[?&]portal=7\b/);

  await page.getByRole("button", { name: "Fabrikam", exact: true }).click();
  await expect(page.getByRole("heading", { level: 2, name: "Fabrikam" })).toBeVisible();
  await expect(page).toHaveURL(/[?&]portal=9\b/);

  // The defect: a reload used to come back on the first portal every time.
  await page.reload();
  await expect(page.getByRole("heading", { level: 2, name: "Fabrikam" })).toBeVisible();
  await expect(page.getByText("Fabrikam SSP weekly")).toBeVisible();

  // Selecting a partner is a navigation, so Back goes back to the one before it.
  await page.goBack();
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await expect(page).toHaveURL(/[?&]portal=7\b/);

  // A link to a portal that no longer exists shows the first one and says so in
  // the URL, rather than a list with no detail under it.
  await page.goto("/admin/portals?portal=404404");
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await expect(page).toHaveURL(/[?&]portal=7\b/);
});

test("Back onto a portal that is gone corrects the address bar, not just the panel", async ({ page }) => {
  await page.goto("/admin/portals");
  await expect(page).toHaveURL(/[?&]portal=7\b/);

  // A history entry naming a portal that no longer exists: retired from another
  // tab, or a stale bookmark. Written directly, because the screen itself never
  // leaves one behind — which is why the fallback only ever ran on arrival.
  await page.evaluate(() => window.history.pushState({}, "", "?portal=999999"));
  await page.getByRole("button", { name: "Fabrikam", exact: true }).click();
  await expect(page).toHaveURL(/[?&]portal=9\b/);

  // Back lands on the dead entry. The panel falls back to the first portal, and
  // the URL has to say the same thing — otherwise the bar names one partner
  // while another is on screen, and a reload from there goes somewhere else again.
  await page.goBack();
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await expect(page).toHaveURL(/[?&]portal=7\b/);
});

test("the add-dashboard picker loads on open and says so when that fails", async ({ page, request }) => {
  await page.goto("/admin/portals");
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

  // The page index is no longer fetched on load, so the next request for it is
  // the dialog's own — armed to fail. It used to be swallowed, leaving a picker
  // offering "Choose a page…" and nothing to choose.
  await request.post("/__fixture/fail-next-pages");
  await page.getByRole("button", { name: "Add a page" }).click();
  const dialog = page.getByRole("dialog");
  const failure = dialog.getByTestId("portal-add-page-error");
  await expect(failure).toBeVisible();
  await expect(failure).toContainText("Couldn't load the page list");
  const picker = dialog.getByLabel("Page", { exact: true });
  await expect(picker).toBeDisabled();
  // The action mirrors the picker, so the dialog is not a dead end offering a
  // solid "Add page" whose only answer is "Choose a page to add." —
  // an instruction there is no way to follow. Cancel and Try again are what is
  // left that does anything.
  const add = dialog.getByRole("button", { name: "Add page", exact: true });
  await expect(add).toBeDisabled();
  await expectNoSeriousAxeViolations(page, "/admin/portals add-dashboard failure");

  // The retry is on the failure itself, so recovering does not mean closing the
  // dialog and losing the label already typed.
  await failure.getByRole("button", { name: "Try again" }).click();
  await expect(picker).toBeEnabled();
  await expect(failure).toBeHidden();
  await expect(add).toBeEnabled();
  await expect(picker.locator("option").first()).toHaveText("Choose a page…");
  // The count is stated in prose, so the arrival of the list is announced and is
  // not carried only by option text nobody was told about.
  await expect(dialog.getByTestId("portal-add-page-status")).toContainText("pages to choose from");

  // Submitting with the placeholder still selected — Enter from another field
  // reaches this even with the action enabled — is refused inline, next to the
  // field, and not sent to the API as a 404.
  await dialog.getByLabel("Label for the partner (optional)").fill("Nothing chosen");
  await dialog.getByLabel("Label for the partner (optional)").press("Enter");
  await expect(dialog.getByText("Choose a page to add.")).toBeVisible();
});

test("the add-page action waits for the list rather than telling the operator to choose from nothing", async ({ page, request }) => {
  await page.goto("/admin/portals");
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

  await request.post("/__fixture/delay-next-pages", { data: { milliseconds: 1500 } });
  await page.getByRole("button", { name: "Add a page" }).click();
  const dialog = page.getByRole("dialog");
  const add = dialog.getByRole("button", { name: "Add page", exact: true });
  const status = dialog.getByTestId("portal-add-page-status");

  // In flight: the state is said in prose next to the picker, the picker reports
  // it to assistive tech, and the action that needs the list is not offered.
  await expect(status).toHaveText("Loading the page list…");
  await expect(dialog.getByLabel("Page", { exact: true })).toHaveAttribute("aria-busy", "true");
  await expect(add).toBeDisabled();

  // Arrived: the count is announced, the action turns on, and nothing about the
  // picker's state is left to disabled option text alone.
  await expect(status).toContainText("pages to choose from", { timeout: 8000 });
  await expect(add).toBeEnabled();
  await expect(dialog.getByLabel("Page", { exact: true })).not.toHaveAttribute("aria-busy", "true");
});

test("with nothing to add, the dialog says which nothing it is and offers no dead action", async ({ page, request }) => {
  // No dashboards at all is not the same sentence as "every dashboard is already
  // in this portal", and saying the second one on a fresh install is a plain lie.
  await request.post("/__fixture/empty");
  await page.goto("/admin/portals");
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await page.getByRole("button", { name: "Add a page" }).click();
  const dialog = page.getByRole("dialog");

  await expect(dialog.getByTestId("portal-add-page-status"))
    .toHaveText("There are no pages in Pages yet. Create one, then add it here.");
  await expect(dialog.getByLabel("Page", { exact: true })).toBeDisabled();
  // The dead end: a solid "Add page" whose only answer was an instruction
  // the operator had no way to follow.
  await expect(dialog.getByRole("button", { name: "Add page", exact: true })).toBeDisabled();
  await expect(dialog.getByRole("button", { name: "Cancel" })).toBeEnabled();
  await expectNoSeriousAxeViolations(page, "/admin/portals add-dashboard with nothing to add");
});

test("a dialog's refusal is on screen at a short viewport, not below the fold", async ({ page }) => {
  // The height that used to clip it: the add-dashboard form is taller than the
  // dialog body from 481px up, and nothing scrolled the message into view. #173
  // took the Order field out, so the form is one field shorter than when this was
  // written — the clipping assertion is what matters and it still holds.
  await page.setViewportSize({ width: 1440, height: 600 });
  await page.goto("/admin/portals");
  await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
  await page.getByRole("button", { name: "Add a page" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByTestId("portal-add-page-status")).toContainText("to choose from");

  // Submit with nothing chosen: the refusal is the dialog's own inline error.
  // Entered from the Label field — the Order field this used to press Enter in is
  // gone (#173), and any field in the form reaches the same one submit path.
  await dialog.getByLabel("Label for the partner (optional)").press("Enter");
  const refusal = dialog.getByText("Choose a page to add.");
  await expect(refusal).toBeVisible();
  const clipped = await refusal.evaluate((node) => {
    const body = node.closest(".ui-dialog__body");
    const box = node.getBoundingClientRect();
    const frame = body.getBoundingClientRect();
    return { above: box.top < frame.top - 1, below: box.bottom > frame.bottom + 1 };
  });
  expect(clipped, "the refusal must be inside the scrollable dialog body").toEqual({ above: false, below: false });
  await expectNoHorizontalOverflow(page);
});

for (const width of [320, 768, 1440]) {
  test(`the portal screen has no overflow at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    await expectNoHorizontalOverflow(page);
  });
}

for (const width of [1100, 900, 390]) {
  test(`portals tables collapse into labelled cards at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto("/admin/portals");
    await expect(page.locator(".operation-table").first()).toBeVisible();
    // The portal list needs a selection before its member table exists.
    await page.getByRole("button", { name: "Northwind Media Group" }).click();
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    await expectCollapsedTablesAreLabelled(page, `portals at ${width}px`);
    await expectNoHorizontalOverflow(page);
  });
}

test("acting on a portal from the keyboard does not drop focus to the body", async ({ page }) => {
  await page.goto("/admin/portals");
  const partner = page.locator(".operation-table tbody button").first();
  await partner.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { level: 2 }).first()).toBeVisible();

  // Every action here rebuilds the whole subtree, on the one screen that is only
  // ever operated by a human.
  const landed = await page.evaluate(() => ({ body: document.activeElement === document.body, id: document.activeElement.id }));
  expect(landed.body, `focus fell to <body> (${JSON.stringify(landed)})`).toBe(false);
});

// ── #173: the order is a position, not a number an admin types ──────────────
// The member table used to show an ORDER column reading 0, 1, 2, and both
// dialogs asked for that integer. Moving a dashboard meant opening a dialog and
// guessing a value, and because sort_order defaults to 0 two rows routinely
// shared it — at which point the list ordered itself by title and nothing typed
// into the field could change it. The fixture's /pages/update really applies
// what it is sent, so these assert the order that comes BACK, not that a request
// was made.

test.describe("membership order", () => {
  // The member order is fixture state now, so a spec that reorders must not
  // decide what the next one sees.
  test.beforeEach(async ({ request }) => {
    await request.post("/__fixture/reset-portals");
  });
  test.afterEach(async ({ request }) => {
    await request.post("/__fixture/reset-portals");
  });

  const memberTable = (page) => page.locator(".operation-table").nth(1);
  const memberOrder = (page) => memberTable(page).locator("tbody td:first-child strong").allTextContents();

  test("no number is shown for the order, and neither dialog asks for one", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    // The whole column is gone: a zero-based integer next to a dashboard title
    // was the defect, not a small piece of it.
    await expect(memberTable(page).getByRole("columnheader")).toHaveText(["Page", "Actions"]);
    // The caption says what the number used to imply, and says it about the
    // PARTNER rather than about this table — which is what the arrows can move.
    await expect(memberTable(page).locator("caption"))
      .toContainText("3 pages in this order, 2 of them visible to this partner.");
    await expect(memberTable(page).locator("caption"))
      .toContainText("The partner's own index always shows the home page first, wherever it sits here.");

    // A number input in either dialog is the shape of the defect, whatever the
    // field is called.
    await page.getByRole("button", { name: "Add a page" }).click();
    const add = page.getByRole("dialog");
    await expect(add.getByLabel("Page", { exact: true })).toBeEnabled();
    await expect(add.getByLabel("Order")).toHaveCount(0);
    await expect(add.locator("input[type=number]")).toHaveCount(0);
    await add.getByRole("button", { name: "Cancel" }).click();

    await page.getByRole("row").filter({ hasText: "Contoso Allergex" }).getByRole("button", { name: "Edit" }).click();
    const edit = page.getByRole("dialog");
    await expect(edit.getByLabel("Label for the partner")).toBeVisible();
    await expect(edit.getByLabel("Order")).toHaveCount(0);
    await expect(edit.locator("input[type=number]")).toHaveCount(0);
    await edit.getByRole("button", { name: "Cancel" }).click();
  });

  test("a page moves up and down from its own row, and the list comes back reordered", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    expect(await memberOrder(page)).toEqual(["Portfolio overview", "Contoso Allergex", "Taken down"]);

    // Each control names its own row: three identical "Move up" buttons in a
    // table tell a screen reader nothing about what they would move.
    await expect(page.getByRole("button", { name: "Move up: Contoso Allergex" })).toBeVisible();
    await expect(page.getByRole("button", { name: "Move down: Contoso Allergex" })).toBeVisible();

    await page.getByRole("button", { name: "Move up: Contoso Allergex" }).click();
    await expect.poll(() => memberOrder(page)).toEqual(["Contoso Allergex", "Portfolio overview", "Taken down"]);
    await expect(page.getByRole("status").filter({ hasText: "Contoso Allergex is now first." })).toBeVisible();

    await page.getByRole("button", { name: "Move down: Contoso Allergex" }).click();
    await expect.poll(() => memberOrder(page)).toEqual(["Portfolio overview", "Contoso Allergex", "Taken down"]);
    await expect(page.getByRole("status").filter({ hasText: "Contoso Allergex moved below Portfolio overview." })).toBeVisible();

    // The ends of the list offer nothing to press, rather than a control that
    // silently does nothing.
    await expect(page.getByTestId("portal-move-up-71")).toBeDisabled();
    await expect(page.getByTestId("portal-move-down-71")).toBeEnabled();
    await expect(page.getByTestId("portal-move-down-73")).toBeDisabled();
    // A portal with a single dashboard has no order to change, so it gets no
    // controls rather than two permanently dead ones — and its caption drops the
    // "in this order" that would be describing a list of one.
    await page.getByRole("button", { name: "Fabrikam", exact: true }).click();
    await expect(page.getByRole("heading", { level: 2, name: "Fabrikam" })).toBeVisible();
    await expect(page.getByTestId("portal-move-up-91")).toHaveCount(0);
    await expect(page.getByTestId("portal-move-down-91")).toHaveCount(0);
    await expect(memberTable(page).locator("caption")).toContainText("1 page, visible to this partner.");
    await expect(memberTable(page).locator("caption")).not.toContainText("in this order");
  });

  test("a keyboard operator can move a row, is told that it moved, and keeps the focus", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    await page.getByTestId("portal-move-up-73").focus();
    await page.keyboard.press("Enter");
    // The announcement is the ONLY confirmation left: with the integer gone,
    // nothing on the row says where it now sits.
    await expect(page.getByRole("status").filter({ hasText: "Taken down moved above Contoso Allergex." })).toBeVisible();
    await expect.poll(() => memberOrder(page)).toEqual(["Portfolio overview", "Taken down", "Contoso Allergex"]);

    // #146, on a control that rebuilds the row it lives in: focus stays on this
    // row, on a control that can still move it.
    const landed = await page.evaluate(() => ({
      body: document.activeElement === document.body,
      id: document.activeElement.id,
    }));
    expect(landed.body, `focus fell to <body> (${JSON.stringify(landed)})`).toBe(false);
    expect(landed.id).toBe("portal-move-up-73");

    // Alt+↓ from anywhere in the row does the same thing, so an operator who has
    // tabbed to Edit does not have to walk back to the arrows.
    await page.getByRole("row").filter({ hasText: "/nwm-taken-down" }).getByRole("button", { name: "Edit" }).focus();
    await page.keyboard.press("Alt+ArrowDown");
    await expect(page.getByRole("status").filter({ hasText: "Taken down is now last." })).toBeVisible();
    await expect.poll(() => memberOrder(page)).toEqual(["Portfolio overview", "Contoso Allergex", "Taken down"]);
    // …and it leaves them where they were. Focus used to be relocated onto an
    // arrow — and, at the end of the list, onto the arrow with the OPPOSITE
    // effect, so the next Enter or Space undid the move just asked for.
    expect(await page.evaluate(() => document.activeElement.id)).toBe("portal-edit-73");
    // The shortcut is declared on the controls rather than only drawn in a title
    // on hover, which the operator using it never sees.
    await expect(page.getByTestId("portal-move-up-73")).toHaveAttribute("aria-keyshortcuts", "Alt+ArrowUp");
    await expect(page.getByTestId("portal-move-down-73")).toHaveAttribute("aria-keyshortcuts", "Alt+ArrowDown");

    // …and the shortcut is refused at the end of the list rather than sending a
    // write that would reorder nothing.
    await page.getByTestId("portal-move-up-73").focus();
    await page.keyboard.press("Alt+ArrowDown");
    await page.waitForTimeout(300);
    expect(await memberOrder(page)).toEqual(["Portfolio overview", "Contoso Allergex", "Taken down"]);

    await expectNoSeriousAxeViolations(page, "/admin/portals reorder controls");
  });

  test("two presses in a row move a dashboard two places", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    // The second press used to be computed against the list captured when the
    // row was built, so a pair of quick presses wrote a stale order. They queue,
    // and each plan is made when its turn comes.
    const up = page.getByTestId("portal-move-up-73");
    await up.click();
    await up.click();
    await expect.poll(() => memberOrder(page)).toEqual(["Taken down", "Portfolio overview", "Contoso Allergex"]);
    await expect(page.getByRole("status").filter({ hasText: "Taken down is now first." })).toBeVisible();
  });

  test("a dashboard added without an Order field is appended, not dropped at 0", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    expect(await memberOrder(page)).toEqual(["Portfolio overview", "Contoso Allergex", "Taken down"]);

    // Three members already, so the fourth goes AFTER them. Sending 0 — which the
    // dialog's default and the link audit both did — is how two rows came to share
    // a position. Asserted on the order that comes BACK rather than on the request
    // body: while the fixture's add handler persisted nothing, asserting the body
    // read 3 twice in a row only because the first add never took effect.
    await page.getByTestId("portal-link-audit-nwm-lakeside").getByRole("button", { name: "Add to portal" }).click();
    await expect.poll(() => memberOrder(page))
      .toEqual(["Portfolio overview", "Contoso Allergex", "Taken down", "Lakeside campaign"]);

    // And the fifth lands after the fourth — the assertion a stub could not make.
    await page.getByRole("button", { name: "Add a page" }).click();
    const dialog = page.getByRole("dialog");
    const picker = dialog.getByLabel("Page", { exact: true });
    await expect(picker).toBeEnabled();
    // By slug, not by the option's label: a page title may contain the same
    // em-dash separator the option text uses.
    const slug = await picker.locator("option").nth(1).getAttribute("value");
    await picker.selectOption(slug);
    await dialog.getByRole("button", { name: "Add page", exact: true }).click();
    await expect.poll(() => memberTable(page).locator("tbody tr").count()).toBe(5);
    await expect(memberTable(page).locator("tbody tr").last()).toContainText(`/${slug}`);
    // …and the four already there keep the order they had.
    expect((await memberOrder(page)).slice(0, 4))
      .toEqual(["Portfolio overview", "Contoso Allergex", "Taken down", "Lakeside campaign"]);
  });

  test("a renumber that fails part-way leaves the screen showing what the server holds", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    expect(await memberOrder(page)).toEqual(["Portfolio overview", "Contoso Allergex", "Taken down"]);

    // A move renumbers by position, so it is more than one write. The second one
    // failing used to leave the screen rendering the order it HOPED for while the
    // server held something else — including, on the list this feature exists to
    // repair, two rows back on one sort_order, invisible to the next press.
    let writes = 0;
    await page.route("**/portals/7/pages/update", (route) => {
      writes += 1;
      if (writes !== 2) return route.continue();
      return route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ error: "server exploded", code: "server_error" }),
      });
    });

    await page.getByRole("button", { name: "Move up: Contoso Allergex" }).click();
    await expect(page.getByRole("alert").filter({ hasText: "server exploded" })).toBeVisible();
    expect(writes, "the plan really was more than one write").toBe(2);

    // The screen re-read: what it shows is what a reload shows, which is what the
    // next press has to plan against.
    const onScreen = await memberOrder(page);
    await page.unroute("**/portals/7/pages/update");
    await page.reload();
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();
    expect(await memberOrder(page), "the failed move left the screen lying about the order").toEqual(onScreen);

    // And the next press repairs it, because a move writes every row that is out
    // of place rather than swapping two values — which is why the honest re-read
    // is enough on its own.
    await page.getByTestId("portal-move-up-73").click();
    await expect.poll(() => memberOrder(page)).toEqual(["Contoso Allergex", "Taken down", "Portfolio overview"]);
  });

  test("a second press in the very same spot moves the next row, not nothing", async ({ page }) => {
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    // By page coordinates on purpose. A locator re-resolves its box between
    // clicks and so cannot see this defect at all: "Make home" comes and goes with
    // the row's state, so in one right-aligned cluster the arrows sat ~97px apart
    // between rows and the second press landed on the cell beside the control.
    // Scrolled first: mouse.click takes VIEWPORT coordinates and the member table
    // sits below the fold at 900px, so an unscrolled box would be clicked at a
    // y the page never renders.
    await page.getByTestId("portal-move-up-73").scrollIntoViewIfNeeded();
    const box = await page.getByTestId("portal-move-up-73").boundingBox();
    const spot = { x: box.x + box.width / 2, y: box.y + box.height / 2 };
    await page.mouse.click(spot.x, spot.y);
    await expect.poll(() => memberOrder(page)).toEqual(["Portfolio overview", "Taken down", "Contoso Allergex"]);

    // Row 3 is now Contoso Allergex, which unlike Taken down carries a "Make home"
    // button — the exact width difference that used to shift the arrows.
    const under = await page.evaluate(({ x, y }) => {
      const node = document.elementFromPoint(x, y);
      const button = node && node.closest("button");
      return button ? button.id : node && node.className;
    }, spot);
    expect(under, "the pointer must still be over a move control").toBe("portal-move-up-72");
    await page.mouse.click(spot.x, spot.y);
    await expect.poll(() => memberOrder(page)).toEqual(["Portfolio overview", "Contoso Allergex", "Taken down"]);
  });

  test("moving the home page down never claims this table is home-first", async ({ page }) => {
    await page.goto("/admin/portals");
    await expect(page.getByRole("heading", { level: 2, name: "Northwind Media Group" })).toBeVisible();

    // Portfolio overview is the home dashboard and starts at the top, which is the
    // only reason the caption's old "— with the home page first" was ever
    // true. The arrows move it, and they report success, so the sentence has to be
    // about the partner's index instead of about this table.
    await page.getByTestId("portal-move-down-71").click();
    await expect.poll(() => memberOrder(page)).toEqual(["Contoso Allergex", "Portfolio overview", "Taken down"]);
    await page.getByTestId("portal-move-down-71").click();
    await expect.poll(() => memberOrder(page)).toEqual(["Contoso Allergex", "Taken down", "Portfolio overview"]);
    await expect(page.getByRole("status").filter({ hasText: "Portfolio overview is now last." })).toBeVisible();

    // The HOME badge is on the LAST row, and the caption above it says nothing to
    // the contrary.
    const rows = memberTable(page).locator("tbody tr");
    await expect(rows.last().locator(".badge")).toHaveText("Home");
    const caption = memberTable(page).locator("caption");
    await expect(caption).not.toContainText("with the home page first");
    await expect(caption)
      .toContainText("The partner's own index always shows the home page first, wherever it sits here.");
  });
});
