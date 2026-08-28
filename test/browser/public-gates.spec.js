// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { test, expect } = require("@playwright/test");
const { expectNoHorizontalOverflow, expectNoSeriousAxeViolations } = require("./helpers");

const cases = [
  { path: "/gate/password", status: 401, heading: "This page is protected" },
  { path: "/gate/incorrect", status: 401, heading: "This page is protected" },
  { path: "/gate/staff", status: 403, heading: "This page hasn't been shared yet" },
  { path: "/gate/404", status: 404, heading: "Page not found" },
  { path: "/gate/rate-limit", status: 429, heading: "Too many password attempts" },
  { path: "/gate/busy", status: 429, heading: "This page is being loaded too often" },
  { path: "/gate/error", status: 500, heading: "This page couldn't be loaded" },
  { path: "/gate/expired", status: 403, heading: "This preview link is no longer valid" },
  { path: "/gate/portal", status: 401, heading: "Northwind Media Group" },
  { path: "/gate/portal-incorrect", status: 401, heading: "Northwind Media Group" },
  { path: "/gate/portal-open", status: 200, heading: "Northwind Media Group" },
  { path: "/gate/portal-empty", status: 200, heading: "Northwind Media Group" },
  { path: "/dashboard-missing", status: 404, heading: "Page not found" },
];

for (const entry of cases) {
  test(`${entry.path} is scriptless, branded, and accessible`, async ({ page }) => {
    const response = await page.goto(entry.path);
    expect(response.status()).toBe(entry.status);
    await expect(page.getByRole("heading", { level: 1, name: entry.heading })).toBeVisible();
    await expect(page.locator("script")).toHaveCount(0);
    await expect(page.locator('img[src*="elcano-mark-primary.svg"]')).toBeVisible();
    await expectNoHorizontalOverflow(page);
    await expectNoSeriousAxeViolations(page, entry.path);
  });
}

test("password error is announced and the corrective field remains labeled", async ({ page }) => {
  await page.goto("/gate/incorrect");
  await expect(page.getByRole("alert")).toContainText("Incorrect password");
  await expect(page.getByRole("alert")).toContainText("Check the password and try again");
  await expect(page.getByLabel("Page password")).toBeFocused();
  await expect(page.getByRole("button", { name: "View page" })).toBeVisible();
});

for (const width of [320, 390, 768, 1440]) {
  test(`public gates have no overflow at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 });
    await page.goto("/gate/incorrect");
    await expectNoHorizontalOverflow(page);
    // The unlocked portal index is the only public page that renders a list of
    // links, one of which carries a long real-world title and a nested slug.
    await page.goto("/gate/portal-open");
    await expectNoHorizontalOverflow(page);
  });
}

test("the unlocked portal index is a keyboard-navigable list of real links", async ({ page }) => {
  await page.goto("/gate/portal-open");
  const links = page.getByRole("listitem").getByRole("link");
  await expect(links).toHaveCount(3);
  // Real anchors with root-absolute hrefs — not script-driven buttons. Slugs
  // nest, so a relative href would resolve against the wrong directory, and
  // `base-uri 'none'` means a <base> tag cannot fix it.
  await expect(links.first()).toHaveAttribute("href", "/nwm-client-overview");
  await expect(links.nth(1)).toHaveAttribute("href", "/nwm/contoso-allergex-always-on");
  // The home page is first, and it is now marked by the section it sits under
  // rather than by a word repeated inside its own link.
  await expect(page.getByRole("heading", { level: 2, name: "Start here" })).toBeVisible();
  await expect(links.first()).toHaveAccessibleName(/^Portfolio overview\b/);
  await page.keyboard.press("Tab");
  await expect(links.first()).toBeFocused();
});

test("an empty portal explains itself instead of blaming the password", async ({ page }) => {
  await page.goto("/gate/portal-empty");
  await expect(page.getByText("No dashboards are available in this portal yet")).toBeVisible();
  await expect(page.getByText("Nothing is wrong with your password")).toBeVisible();
  await expect(page.getByRole("link")).toHaveCount(0);
});

test("the portal password error is announced and the field stays labeled", async ({ page }) => {
  await page.goto("/gate/portal-incorrect");
  await expect(page.getByRole("alert")).toContainText("Incorrect password");
  await expect(page.getByLabel("Portal password")).toBeFocused();
  await expect(page.getByRole("button", { name: "Open portal" })).toBeVisible();
});

test("the gate gives a password manager something to key on", async ({ page }) => {
  await page.goto("/gate/password");
  // Every page and portal lives on one registrable domain, so with a lone
  // password field a manager keys them all to the host: a partner with two pages
  // is offered the wrong saved password, and Chrome often will not offer to save
  // at all. A hidden username field is what makes them separate credentials.
  const account = page.locator('input[autocomplete="username"]');
  await expect(account).toHaveAttribute("value", "gate/password");
  await expect(account).toHaveAttribute("readonly", "");
  // It is a hint for the manager, not a field: out of the tab order, hidden from
  // assistive technology, and occupying no space a reader could click.
  await expect(account).toHaveAttribute("tabindex", "-1");
  await expect(account).toHaveAttribute("aria-hidden", "true");
  const box = await account.boundingBox();
  expect(box.width, "the account hint must not take space").toBeLessThanOrEqual(1);
});

test("a wrong password says the next attempt will be slower", async ({ page }) => {
  await page.goto("/gate/incorrect");
  // The backoff sleeps before answering and the form is scriptless, so there is no
  // pending state to show. The attempt that causes the wait has to announce it.
  await expect(page.getByRole("alert")).toContainText("adds a short delay before the next one");
});

test("a rate-limited reader is told the real wait and given a way back", async ({ page }) => {
  await page.goto("/gate/rate-limit");
  // It said "a few minutes" for a fifteen-minute window, and answered a POST with
  // no link — so the URL bar held the POST target and a reload asked the browser
  // to resubmit the form.
  await expect(page.getByText(/Wait about \d+ minutes/)).toBeVisible();
  await expect(page.getByRole("link", { name: /Return to the page/ })).toBeVisible();
});

// #176 — the index is the one page where a partner sees their whole set, and for
// daily-refreshed dashboards "which of these moved this morning" is the entire
// question. It used to answer three same-weight titles and nothing else.

test("every dashboard on the index says when it was last current", async ({ page }) => {
  await page.goto("/gate/portal-open");
  const stamps = page.locator(".portal-list time");
  await expect(stamps).toHaveCount(3);
  // A machine-readable instant, not just prose: the phrase is relative and the
  // attribute is what survives being read by anything other than a human.
  for (let i = 0; i < 3; i += 1) {
    await expect(stamps.nth(i)).toHaveAttribute("datetime", /^\d{4}-\d{2}-\d{2}T/);
  }
  // The two claims are different claims and must not be phrased alike: a page
  // with a data envelope says when the DATA is from; one without says only when
  // we last published. Conflating them is the false reassurance this fixes.
  await expect(stamps.nth(0)).toHaveText("Data as of 6 hours ago");
  await expect(stamps.nth(1)).toHaveText("Data as of 3 days ago");
  await expect(stamps.nth(2)).toHaveText("Updated 8 Aug 2026");
});

test("the index groups the overview away from the rest", async ({ page }) => {
  await page.goto("/gate/portal-open");
  const headings = page.getByRole("heading");
  // h1 then h2s, in that order — the hierarchy is real, not just heavier type.
  await expect(headings).toHaveCount(3);
  await expect(headings.nth(0)).toHaveText("Northwind Media Group");
  await expect(headings.nth(1)).toHaveText("Start here");
  await expect(headings.nth(2)).toHaveText("All dashboards");
  const lists = page.locator(".portal-list");
  await expect(lists).toHaveCount(2);
  await expect(lists.nth(0).getByRole("link")).toHaveCount(1);
  await expect(lists.nth(1).getByRole("link")).toHaveCount(2);
});

test("a partner can end the session on a shared machine", async ({ page }) => {
  await page.goto("/gate/portal-open");
  // The old copy said "for as long as this portal session lasts" — true of every
  // session ever. The cookie is thirty days and the page now says so.
  await expect(page.getByText("for 30 days on this device")).toBeVisible();
  const signOut = page.getByRole("button", { name: "Sign out" });
  await expect(signOut).toBeVisible();
  // A real form, because this host runs no script: a button wired by JS would be
  // dead here and the reader would have no way to tell.
  const form = page.locator("form.portal-signout");
  await expect(form).toHaveAttribute("method", /post/i);
  await expect(form).toHaveAttribute("action", "/portal/nwm/lock");
});

test("the empty portal can still be signed out of", async ({ page }) => {
  await page.goto("/gate/portal-empty");
  await expect(page.getByRole("button", { name: "Sign out" })).toBeVisible();
});
