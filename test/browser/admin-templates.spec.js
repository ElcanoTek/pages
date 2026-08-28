// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The template library (/admin/templates).
//
// This suite exists because the screen shipped twice with defects that only a
// browser could see: nine invented CSS class names, then a CSS comment whose
// selector text ended the comment early and silently deleted the rule after it.
// The lesson in both cases is that asserting a class attribute proves nothing —
// so these tests assert COMPUTED STYLE, and only on properties the user agent
// does not already supply. (font-family on a <pre> is monospace by default, so
// the earlier version of this file passed while its stylesheet rule was dead.)
//
// It also pins the two responsive behaviours the desktop viewport hides: the
// collapsed table must keep every value labelled, and the detail heading must
// clear the sticky header after Inspect scrolls to it.

const { test, expect } = require("@playwright/test");
const { resetFixture, expectNoHorizontalOverflow, expectNoSeriousAxeViolations, expectCollapsedTablesAreLabelled } = require("./helpers");

const TEMPLATE_HTML = `<!doctype html><html><head><meta charset="utf-8"><title>Fixture template</title></head><body>
<h1 id="hTitle"></h1>
<script type="application/schema+json" id="pages-config-schema">{"type":"object"}</script>
<script type="application/json" id="pages-config">{"campaign":"Reference"}</script>
<script type="application/schema+json" id="pages-data-schema">{"type":"object"}</script>
<script type="application/json" id="pages-data">{"contract_version":1,"data":{"rows":[]}}</script>
</body></html>`;

const NOT_A_TEMPLATE = `<!doctype html><html><head><title>Just a page</title></head><body><p>no managed blocks</p></body></html>`;

const asFile = (name, body) => ({ name, mimeType: "text/html", buffer: Buffer.from(body) });

test.beforeEach(async ({ request }) => resetFixture(request));

test("the library lists templates, and the design system is actually applied", async ({ page }) => {
  await page.goto("/admin/templates");

  await expect(page.getByRole("heading", { level: 1, name: "Template library" })).toBeVisible();
  const rows = page.locator("#template-list tbody tr");
  await expect(rows).toHaveCount(1);
  await expect(rows.first()).toContainText("nwm-campaign-dashboard");
  await expect(rows.first()).toContainText("NWM Campaign Dashboard");
  // A count nobody has to add up by hand.
  await expect(page.locator(".stats")).toContainText("2");
  // replaceChildren() stringifies its arguments, so an absent section used to
  // render the word "null" under the list.
  await expect(page.locator("#app")).not.toContainText("null");

  // A table header styled by .operation-table th is an uppercase micro-label over
  // a rule; an unstyled th is sentence-case with no tracking. This used to check
  // the header's background fill — a fill the header no longer has, because a
  // shaded band inside a bordered card was a third frame on one list.
  const header = await page
    .locator("#template-list thead th")
    .first()
    .evaluate((node) => {
      const style = getComputedStyle(node);
      return { transform: style.textTransform, tracking: style.letterSpacing, rule: style.borderBottomWidth };
    });
  expect(header.transform, "the list table must pick up .operation-table styling").toBe("uppercase");
  expect(header.tracking).not.toBe("normal");
  expect(header.rule, "the header is separated from the rows by a rule, not a fill").not.toBe("0px");

  await expectNoHorizontalOverflow(page);
  await expectNoSeriousAxeViolations(page, "template library index");
});

test("inspecting a template shows its contract, revisions and the pages behind it", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();

  const detail = page.locator("#detail");
  // The detail leads with the human title and keeps the machine name beneath it,
  // the way the page detail does; it used to lead with the slug.
  await expect(detail.locator("#tpl-detail-title")).toHaveText("NWM Campaign Dashboard");
  await expect(detail.locator("code").first()).toHaveText("nwm-campaign-dashboard");
  await expect(detail).toContainText("Revision 2");

  // Pages built from it, with the stale one flagged.
  await expect(detail).toContainText("contoso-allergex-acct00156");
  // One vocabulary across the admin now: sentence case, and "behind" is a warning
  // tone rather than the pending tone it borrowed.
  // One vocabulary across the admin now: sentence case, "Behind" is a warning
  // tone rather than the pending tone it borrowed, and "Current" is neutral —
  // colouring the expected state makes every healthy row shout.
  await expect(detail.getByText("Behind", { exact: true })).toHaveClass(/badge--warning/);
  await expect(detail.getByText("Current", { exact: true }).first()).toHaveClass(/badge--draft/);
  await expect(detail.getByText("Live", { exact: true }).first()).toHaveClass(/badge--live/);

  // Revision history, the current one badged the same way the pages table
  // badges state — not as muted body text.
  await expect(detail).toContainText("chart fix");
  await expect(detail.locator("#tpl-preview-revision-1")).toHaveAttribute("aria-label", "Preview revision 1");

  // Timestamps are formatted for a person, not printed as ISO strings.
  await expect(detail.locator("time").first()).not.toContainText("T09:30:00.000Z");

  await expectNoHorizontalOverflow(page);
  await expectNoSeriousAxeViolations(page, "template detail");
});

// The .code-block rule shipped dead once: a stray comment terminator ate it,
// and the <pre> fell back to user-agent styling. Assert the two properties the
// UA does NOT provide — a background and a border.
test("the contract renders in a real code block, one view at a time", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();

  const code = page.locator("#detail .code-block");
  await expect(code).toHaveCount(1);
  await expect(code).toContainText("Reference Campaign");

  const box = await code.evaluate((node) => {
    const style = getComputedStyle(node);
    return {
      background: style.backgroundColor,
      borderWidth: style.borderTopWidth,
      padding: style.paddingTop,
      family: style.fontFamily,
    };
  });
  expect(box.background, ".code-block must have a surface of its own").not.toBe("rgba(0, 0, 0, 0)");
  expect(box.borderWidth, ".code-block must be a bordered block").not.toBe("0px");
  expect(box.padding).not.toBe("0px");
  expect(box.family.toLowerCase()).toMatch(/mono|consolas|courier|menlo/);

  // The segmented switch swaps which part of the contract is on screen, so three
  // scroll regions never stack.
  await page.locator("#tpl-view-data_schema").click();
  await expect(page.locator("#detail .code-block")).toHaveCount(1);
  await expect(page.locator("#detail .code-block")).toContainText("rows");
  await expect(page.locator("#tpl-view-data_schema")).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator("#tpl-view-config")).toHaveAttribute("aria-pressed", "false");
});

// Inspect scrolls the detail panel to the top of the viewport, and the app
// header is sticky. Without scroll-margin the template name and its actions land
// behind the header — the operator cannot see which template they opened.
test("after Inspect the heading and its actions clear the sticky header", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  await expect(page.locator("#detail")).toBeVisible();
  await page.waitForFunction(() => {
    const node = document.querySelector("#tpl-detail-title");
    return node && Math.abs(node.getBoundingClientRect().top) < 2000;
  });
  // Let the smooth scroll settle before measuring.
  await page.waitForTimeout(700);

  const geometry = await page.evaluate(() => {
    const header = document.querySelector(".page-header").getBoundingClientRect();
    const title = document.querySelector("#tpl-detail-title").getBoundingClientRect();
    const actions = document.querySelector("#tpl-detail-close").getBoundingClientRect();
    return { headerBottom: header.bottom, titleTop: title.top, actionsTop: actions.top };
  });
  expect(geometry.titleTop, "the template name must not sit behind the header")
    .toBeGreaterThanOrEqual(geometry.headerBottom);
  expect(geometry.actionsTop, "Close must not sit behind the header")
    .toBeGreaterThanOrEqual(geometry.headerBottom);

  // Focus follows the operator to what they opened, rather than falling to <body>.
  const focused = await page.evaluate(() => document.activeElement.tagName + ":" + (document.activeElement.textContent || "").trim().slice(0, 24));
  expect(focused).toContain("NWM Campaign Dashboard");

  // And it can be dismissed again.
  await page.locator("#tpl-detail-close").click();
  await expect(page.locator("#detail")).toHaveCount(0);
});

test("smooth scrolling is skipped when the operator asks for reduced motion", async ({ browser }) => {
  const context = await browser.newContext({ reducedMotion: "reduce", colorScheme: "dark" });
  const page = await context.newPage();
  await page.request.post("/__fixture/reset");
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  await page.waitForSelector("#detail");

  // An animated scroll passes through intermediate offsets. A jump does not.
  const first = await page.evaluate(() => window.scrollY);
  await page.waitForTimeout(150);
  const second = await page.evaluate(() => window.scrollY);
  expect(second, "reduced motion must jump, not animate").toBe(first);
  await context.close();
});

// Below 60rem .operation-table collapses to cards and hides the header row. Every
// value then has to carry its own label, or the list becomes bare numbers. The
// default 1280px viewport is well above the breakpoint, which is how this shipped
// broken. (The collapse used to start at 75rem; #150 moved it down so a laptop
// gets the table, so these widths sit below 60rem — 1100px no longer collapses.)
for (const width of [900, 390]) {
  test(`every value stays labelled when the tables collapse at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await page.goto("/admin/templates");

    const labels = await page.$$eval("#template-list tbody td", (cells) =>
      cells.map((cell) => ({
        label: cell.getAttribute("data-label"),
        rendered: getComputedStyle(cell, "::before").content,
      }))
    );
    expect(labels.length).toBeGreaterThan(0);
    for (const cell of labels) {
      expect(cell.label, "every collapsed cell needs a data-label").toBeTruthy();
      expect(cell.rendered, `the label for ${cell.label} must render`).toContain(cell.label);
    }

    // The bespoke named-area arrangement belongs to the admin index alone; a
    // template table placed into it scatters its cells.
    const areas = await page.locator("#template-list tbody tr").evaluate((row) => getComputedStyle(row).gridTemplateAreas);
    expect(areas).toBe("none");

    await page.getByRole("button", { name: "Inspect" }).click();
    await expect(page.locator("#detail")).toBeVisible();
    const detailLabels = await page.$$eval("#detail tbody td", (cells) => cells.map((cell) => cell.getAttribute("data-label")));
    expect(detailLabels.length).toBeGreaterThan(0);
    expect(detailLabels.every(Boolean), "detail tables need labels too").toBe(true);

    await expectNoHorizontalOverflow(page);
    await expectNoSeriousAxeViolations(page, `template library at ${width}px`);
  });
}

test("an empty library says what to do about it", async ({ page, request }) => {
  await request.post("/__fixture/empty-templates");
  await page.goto("/admin/templates");

  const empty = page.locator(".state-panel");
  await expect(empty).toContainText("No templates yet");
  await expect(empty).toContainText("pages template sync");
  await expect(empty.getByRole("button", { name: "Add a template" })).toBeVisible();
  await expect(page.locator("#app")).not.toContainText("null");
  await expectNoSeriousAxeViolations(page, "template library empty");
});

test("adding a template checks the format before anything is written", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.locator("#tpl-add").click();

  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();
  const register = dialog.locator("#tpl-register");
  await expect(register).toBeDisabled();

  // Picking a file checks it immediately — no separate step to forget.
  await dialog.locator("#tpl-file").setInputFiles(asFile("not-a-template.html", NOT_A_TEMPLATE));
  await expect(dialog.locator("#tpl-verdict")).toContainText("template_contract_invalid");
  await expect(register).toBeDisabled();

  // A valid file arms Register.
  await dialog.locator("#tpl-file").setInputFiles(asFile("fixture-upload.html", TEMPLATE_HTML));
  await expect(dialog.locator("#tpl-verdict")).toContainText("Valid");
  await expect(dialog.locator("#tpl-name")).toHaveValue("fixture-upload");
  await expect(register).toBeEnabled();

  // A verdict belongs to the bytes it was computed from: swapping the file must
  // disarm Register rather than leave a stale pass in place.
  await dialog.locator("#tpl-file").setInputFiles(asFile("not-a-template.html", NOT_A_TEMPLATE));
  await expect(dialog.locator("#tpl-verdict")).toContainText("template_contract_invalid");
  await expect(register).toBeDisabled();

  // Same for the name: changing it invalidates the pass until it is re-checked.
  await dialog.locator("#tpl-file").setInputFiles(asFile("fixture-upload.html", TEMPLATE_HTML));
  await expect(register).toBeEnabled();
  await dialog.locator("#tpl-name").fill("Not A Name!");
  await expect(register).toBeDisabled();
  await dialog.locator("#tpl-name").blur();
  await expect(dialog.locator("#tpl-verdict")).toContainText("url-safe");
  await expect(register).toBeDisabled();

  // Nothing has been written at any point.
  await expect(page.locator("#template-list tbody tr")).toHaveCount(1);

  await dialog.locator("#tpl-name").fill("fixture-upload");
  await dialog.locator("#tpl-name").blur();
  await expect(register).toBeEnabled();
  await expectNoSeriousAxeViolations(page, "add a template dialog");

  await register.click();
  await expect(dialog).toBeHidden();
  await expect(page.locator("#template-list tbody tr")).toHaveCount(2);
  await expect(page.locator("#template-list")).toContainText("fixture-upload");
  // It opens what was just registered, so the operator can check it.
  await expect(page.locator("#detail")).toContainText("fixture-upload");
});

// The section bar used to hold an inert <span> styled with the page-switcher's
// counter class plus one link, and its label was repeated by an overline over a
// differently-worded h1 — which read as two stacked headers. It is now a real
// two-way nav with the current section marked, and each screen has one heading.
test("the two sections are one nav, and each screen has a single heading", async ({ page }) => {
  await page.goto("/admin/templates");

  const nav = page.getByRole("navigation", { name: "Pages sections" });
  await expect(nav.getByRole("link", { name: "Template library" })).toHaveAttribute("aria-current", "page");
  await expect(nav.getByRole("link", { name: "Client pages" })).not.toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("heading", { level: 1 })).toHaveCount(1);

  // Marking the current tab must not shift the row.
  const heights = await nav.locator(".context-tab").evaluateAll((tabs) =>
    tabs.map((tab) => Math.round(tab.getBoundingClientRect().height))
  );
  expect(new Set(heights).size, `tabs must be the same height: ${heights}`).toBe(1);

  // It navigates both ways — the library used to be reachable only from the index.
  await nav.getByRole("link", { name: "Client pages" }).click();
  await expect(page).toHaveURL(/\/admin$/);
  const indexNav = page.getByRole("navigation", { name: "Pages sections" });
  await expect(indexNav.getByRole("link", { name: "Client pages" })).toHaveAttribute("aria-current", "page");
  await expect(page.getByRole("heading", { level: 1, name: "Client pages" })).toBeVisible();
  await expect(page.getByRole("heading", { level: 1 })).toHaveCount(1);
  await expect(page.locator(".page-heading .overline")).toHaveCount(0);

  await indexNav.getByRole("link", { name: "Template library" }).click();
  await expect(page).toHaveURL(/\/admin\/templates$/);
  await expect(page.getByRole("heading", { level: 1, name: "Template library" })).toBeVisible();
});

// The library exists to answer "is this the right design?", so it renders the
// design in place — the way every other preview surface in this product already
// does. Opening a new tab for it was the odd one out.
test("inspecting renders the design inline, sandboxed on the content host", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();

  const frame = page.locator("#tpl-preview-frame");
  await expect(frame).toBeVisible();
  // Sandboxed, no referrer, and pointed at the content host rather than here.
  await expect(frame).toHaveAttribute("sandbox", "allow-scripts");
  await expect(frame).toHaveAttribute("referrerpolicy", "no-referrer");
  await expect.poll(() => frame.getAttribute("src")).toContain("/preview/tpl-nwm-campaign-dashboard");

  // The design actually rendered in there.
  await expect(page.frameLocator("#tpl-preview-frame").locator("h1")).toContainText("Fixture preview");

  // And the status says WHAT was rendered: a template ships an empty
  // #pages-data, so "here is the design" is only honest with example data.
  await expect(page.locator("#tpl-preview-status")).toContainText("example data");

  // The design's bytes never entered the trusted document.
  await expect(page.locator("#app")).not.toContainText("pages-config-schema");
});

test("previewing from the revisions table leaves that button's own label alone", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  const trigger = page.locator("#tpl-preview-revision-1");
  await expect(trigger).toHaveText("Preview");

  await trigger.click();
  await expect(page.locator("#tpl-preview-status")).toContainText("Revision 1");
  // The busy restore used to pass the label "Reload" — the panel button's word,
  // not this one's — so previewing an old revision permanently renamed the
  // button that did it.
  await expect(trigger).toHaveText("Preview");
  await expect(trigger).toHaveAttribute("aria-label", "Preview revision 1");
});

test("switching the contract view keeps the loaded design in place", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  const frame = page.locator("#tpl-preview-frame");
  await expect(frame).toBeVisible();
  const before = await frame.getAttribute("src");

  // A full re-render would rebuild the iframe and throw away a loaded design
  // plus its signed token, just because someone read a schema.
  await page.locator("#tpl-view-data_schema").click();
  await expect(page.locator("#detail .code-block")).toContainText("rows");
  await expect(page.locator("#tpl-view-data_schema")).toHaveAttribute("aria-pressed", "true");
  await expect(frame).toBeVisible();
  expect(await frame.getAttribute("src"), "the frame must not have been rebuilt").toBe(before);
});

test("previewing an older revision reframes it and says which one", async ({ page }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  await expect(page.locator("#tpl-preview-status")).toContainText("Revision 2");

  await page.locator("#tpl-preview-revision-1").click();
  await expect(page.locator("#tpl-preview-status")).toContainText("Revision 1");
  // Revision 1 of the fixture carries no example dataset, so the label has to
  // stop claiming one.
  await expect(page.locator("#tpl-preview-status")).toContainText("empty state");
});

test("open full size leaves the document rather than rendering here", async ({ page, context }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  await expect(page.locator("#tpl-preview-frame")).toBeVisible();

  const popupPromise = context.waitForEvent("page");
  await page.locator("#tpl-preview-tab").click();
  const popup = await popupPromise;
  await expect.poll(() => popup.url()).toContain("/preview/tpl-nwm-campaign-dashboard");
  await popup.close();
});

// Registering got easy, so getting it wrong had to become undoable. A mistyped
// template name used to need SQL, and the library now shows it — with a rendered
// preview — to everyone.
test("a template can be retired from the library, and the name comes back", async ({ page, request }) => {
  await page.goto("/admin/templates");
  await page.getByRole("button", { name: "Inspect" }).click();
  await expect(page.locator("#detail")).toBeVisible();

  // The fixture template has two pages built from it, so the confirmation has to
  // say so rather than asking a bare "are you sure?".
  await page.locator("#tpl-detail-retire").click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toContainText("2 pages were built from this");
  await expect(dialog).toContainText("keep serving");

  // Backing out writes nothing.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(page.locator("#template-list tbody tr")).toHaveCount(1);

  // Hold the list reload open, so the order of "you retired it" and "here is the
  // list without it" is observable. The confirmation of a destructive action
  // belongs to the DELETE, not to the round trip after it.
  let release = () => {};
  const held = new Promise((resolve) => { release = resolve; });
  await page.route("**/api/v1/admin/templates", async (route) => {
    if (route.request().method() !== "GET") return route.continue();
    await held;
    return route.continue();
  });

  const retire = page.locator("#tpl-detail-retire");
  await retire.click();
  await page.getByRole("dialog").getByRole("button", { name: "Retire anyway" }).click();
  await expect(page.getByText("nwm-campaign-dashboard retired", { exact: false })).toBeVisible();
  // Still the old list on screen, and the button that started it is still busy.
  await expect(page.locator("#template-list tbody tr")).toHaveCount(1);
  await expect(retire).toBeDisabled();
  release();

  // It leaves the library, the detail closes with it, and the screen falls back
  // to the empty state rather than a broken selection.
  await expect(page.locator("#detail")).toHaveCount(0);
  await expect(page.locator(".state-panel")).toContainText("No templates yet");
  await expect(page.locator("#app")).not.toContainText("null");
  await expectNoSeriousAxeViolations(page, "template library after retiring");

  // And the name is reusable — the whole reason this exists.
  const check = await request.post("/api/v1/admin/templates/validate", {
    headers: { "X-CSRF-Token": "fixture-csrf" },
    data: { html: TEMPLATE_HTML, name: "nwm-campaign-dashboard" },
  });
  expect(check.ok()).toBeTruthy();
  expect((await check.json()).name).toBe("nwm-campaign-dashboard");
});

for (const width of [1100, 900, 390]) {
  test(`templates tables collapse into labelled cards at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 1000 });
    await page.goto("/admin/templates");
    await expect(page.locator(".operation-table").first()).toBeVisible();
    await expectCollapsedTablesAreLabelled(page, `templates at ${width}px`);
    await expectNoHorizontalOverflow(page);
  });
}
