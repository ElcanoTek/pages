// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// A Bento deck is one .bento.html that is its own viewer and editor. Its runtime
// is deflate-compressed and boots through a blob: module import, which is the
// one thing the content host's CSP used to forbid — so a deck deployed to Pages
// died with "This file could not start" while preflight, which reads plaintext,
// called it clean. These run the real boot mechanism under the real headers.

const { test, expect } = require("@playwright/test");

test("a Bento deck boots under the content host's real CSP", async ({ page }) => {
  const errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  const response = await page.goto("/bento/deck");
  // The header itself, not just its effect: this is the grant the boot depends on.
  expect(response.headers()["content-security-policy"]).toMatch(/script-src 'self' 'unsafe-inline' blob:/);
  expect(response.headers()["content-security-policy"]).toMatch(/media-src data: blob:/);
  await expect(page.locator("#booted")).toHaveText("Synthetic deck booted");
  expect(await page.evaluate(() => window.__bentoBooted)).toBe(true);
  expect(errors, errors.join("\n")).toEqual([]);
  // The sandbox is untouched by the grant: still an opaque origin with no storage.
  const storage = await page.evaluate(() => { try { localStorage.getItem("x"); return "allowed"; } catch (e) { return e.name; } });
  expect(storage).toBe("SecurityError");
  // The one tag Pages adds. The sandbox refuses native file pickers ("Sandboxed
  // documents aren't allowed to show a file picker"), and Bento chooses its Save
  // path by whether the API exists — so with the API present, Save on Chrome did
  // nothing at all. Absent, Bento downloads and says so on the button.
  // Present once in what was SERVED…
  expect(((await response.text()).match(/data-pages-deck-host/g) || []).length).toBe(1);
  // …and gone from the DOM once it has run, because Bento serialises the live
  // DOM on Save and a copy that leaves Pages must be pure Bento.
  await expect(page.locator("script[data-pages-deck-host]")).toHaveCount(0);
  expect(await page.evaluate(() => typeof window.showSaveFilePicker)).toBe("undefined");
});

test("the blob: grant is exactly what makes it boot", async ({ page }) => {
  const errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text()); });
  await page.goto("/bento/deck-without-blob");
  // Bento's own bootstrap reports the failure in the document, which is the only
  // place a reader would ever see it — there is no console in front of a partner.
  await expect(page.getByText(/This file could not start/)).toBeVisible();
  await expect(page.locator("#booted")).toHaveCount(0);
  // blob:null — the origin is opaque, so the URL the import tried is nameable
  // only as null. That is the sandbox working, and the CSP refusing the import.
  expect(errors.some((e) => /blob:null\//.test(e) && /script-src/.test(e)), errors.join("\n")).toBe(true);
});

test("a deck in a portal gets no Page menu drawn over its toolbar", async ({ page }) => {
  await page.goto("/bento/deck-portal");
  await expect(page.locator("#booted")).toHaveText("Synthetic deck booted");
  // A raw dashboard in a portal is handed the built-in switcher; a deck is served
  // byte-for-byte, so neither the control nor the payload it reads is present.
  await expect(page.locator(".pgnav-host")).toHaveCount(0);
  await expect(page.locator("#pages-nav")).toHaveCount(0);
});
