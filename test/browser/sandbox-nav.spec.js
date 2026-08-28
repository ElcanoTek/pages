// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Can a partner move between their dashboards?
//
// Client pages are served on the content host as TOP-LEVEL documents carrying
// `Content-Security-Policy: sandbox allow-scripts allow-downloads allow-modals`
// as a real response header. `allow-top-navigation`, `allow-popups` and
// `allow-forms` are all deliberately withheld (lib/csp.js), and
// docs/AUTHORING.md's capability table — which tells authors that window.open
// and form submission do not work — says nothing about a plain link.
//
// The partner page switcher rests entirely on two behaviours nobody had
// verified:
//
//   S1  a sandboxed top-level document navigating ITSELF, by anchor and by
//       location.assign (the reference design uses <button onClick>)
//   S2  the target's SameSite=Lax session cookie riding along on that
//       navigation, given the initiator has an opaque origin
//
// If S1 ever regresses, a partner's dropdown silently stops working and the
// only symptom is a click that does nothing — so this stays in the suite as a
// guard rather than being a throwaway script. The fixture routes use the real
// rawHeaders() and the real lib/pagecookie code, so a pass here is a statement
// about Pages and not about a synthetic page.

const { test, expect } = require("@playwright/test");

async function unlockAndOpenSource(page) {
  // Real Set-Cookie from lib/pagecookie, then the sandboxed document.
  await page.goto("/sandbox-nav/unlock");
  await expect(page).toHaveURL(/\/sandbox-nav\/source$/);
  return page;
}

test("a sandboxed top-level page can follow a link to a sibling", async ({ page }) => {
  await unlockAndOpenSource(page);
  await page.locator("#anchor").click();
  await expect(page.locator("#arrived")).toHaveText("arrived");
  await expect(page.locator("#via")).toHaveText("anchor");
});

test("a sandboxed top-level page can navigate itself from script", async ({ page }) => {
  await unlockAndOpenSource(page);
  await page.locator("#assign").click();
  await expect(page.locator("#arrived")).toHaveText("arrived");
  await expect(page.locator("#via")).toHaveText("assign");
});

test("a new tab is still refused, because allow-popups is withheld", async ({ page, context }) => {
  // The other half of the row this spike adds to docs/AUTHORING.md: same-tab
  // navigation works, opening a tab does not. Asserting both is what makes the
  // guidance evidence rather than inference — the table already claims
  // window.open fails, and target="_blank" is the same capability.
  await unlockAndOpenSource(page);
  const before = page.url();
  await page.locator("#blank").click();
  await page.waitForTimeout(500);
  expect(context.pages()).toHaveLength(1);
  expect(page.url()).toBe(before);
});

test("the page session survives that navigation and still verifies", async ({ page }) => {
  // The switcher is useless if every hop lands on a password form: the session
  // cookie is SameSite=Lax and the initiator is an opaque origin, so this is
  // the half of the question that decides whether a partner is prompted again
  // on every dashboard.
  await unlockAndOpenSource(page);
  await page.locator("#anchor").click();
  await expect(page.locator("#cookie-present")).toHaveText("yes");
  await expect(page.locator("#cookie-verified")).toHaveText("yes");
});
