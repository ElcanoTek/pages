// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// A REAL Bento deck under the content host's real raw headers. bento-deck.spec.js
// pins the boot mechanism with a synthetic deck; this runs the same probes
// against the real application, which is not vendored here. Run it when the
// deck runtime is upgraded or the raw headers change:
//
//   PAGES_FIXTURE_BENTO_DECK=/path/to/Deck.bento.html npx playwright test bento-deck-real
//
// The fixture server reads the variable on every request, so start it fresh
// (or set CI=1, which never reuses a running one) if one is already up from
// another run. Without the variable the spec skips. It reports boot, the mode
// that opened (editor or player), what Save does, whether Export PDF reaches
// window.print, whether an edit survives a reload, the edit session's save
// channel, and every console error. It never writes anything outside the
// fixture's in-memory event log.

const { test, expect } = require("@playwright/test");

const DECK = process.env.PAGES_FIXTURE_BENTO_DECK;
test.skip(!DECK, "PAGES_FIXTURE_BENTO_DECK is not set: no real Bento deck to check");
test.describe.configure({ timeout: 120_000 });

const retitle = (value) => {
  const t = [...document.querySelectorAll("input")].find((i) => i.value && document.title.startsWith(i.value));
  if (!t) return false;
  t.value = value === null ? t.value + " (edited)" : value;
  t.dispatchEvent(new Event("input", { bubbles: true }));
  t.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
};

test("a real Bento deck boots, saves and exports under the raw headers", async ({ page }) => {
  const errors = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text().slice(0, 160)); });
  page.on("pageerror", (e) => errors.push(`uncaught: ${String(e.message).slice(0, 160)}`));
  // Nothing is stubbed: the host adaptation Pages injects removes the native
  // file-picker API (the sandbox refuses it anyway), so Save must download.
  await page.addInitScript(() => { window.__print = 0; window.print = () => { window.__print += 1; }; });

  const response = await page.goto("/bento/real", { waitUntil: "load" });
  expect(response.status(), await response.text()).toBe(200);
  await page.waitForTimeout(4000);
  const failed = await page.getByText(/This file could not start/).count();
  const editor = await page.locator('button[title^="Save"]').count();
  const player = await page.locator(".reveal").count();
  console.log(`boot:        ${failed ? "FAILED — " + (await page.locator("body").innerText()).slice(0, 140) : "ok"}`);
  console.log(`mode:        ${editor ? "editor" : player ? "player (readonly deck)" : "unknown"}`);
  expect(failed, "the deck reported that it could not start").toBe(0);
  const picker = await page.evaluate(() => typeof window.showSaveFilePicker);
  console.log(`picker API:  ${picker} (must be undefined — Pages removes it so Bento downloads)`);
  expect(picker).toBe("undefined");

  if (editor) {
    const dl = page.waitForEvent("download", { timeout: 6000 }).catch(() => null);
    await page.locator('button[title^="Save"]').first().click({ timeout: 5000 }).catch(() => {});
    const download = await dl;
    if (download) {
      const stream = await download.createReadStream();
      let saved = "";
      stream.setEncoding("utf8");
      for await (const chunk of stream) saved += chunk;
      const block = (saved.match(/<script\b[^>]*application\/bento\+json[^>]*>([\s\S]*?)<\/script/i) || [])[1] || "";
      console.log(`Save:        downloads ${download.suggestedFilename()} (${saved.length} bytes)`);
      console.log(`  carries:   guard CSP ${/http-equiv="Content-Security-Policy"/.test(saved) ? "yes" : "NO"} · host tag ${/data-pages-deck-host/.test(saved) ? "yes (stripped on upload)" : "no"} · collab keys ${/"collab"\s*:\s*\{/.test(block) ? "YES — preflight will warn; bento_doc.py set drops them" : "no"}`);
    } else {
      console.log("Save:        did nothing observable — a regression: Bento's Save reached for a picker the sandbox refuses");
    }
    expect(download, "Save must download in the sandbox").not.toBeNull();
    await page.keyboard.press("Escape").catch(() => {});
    await page.locator('button[title^="Export PDF"]').first().click({ timeout: 5000 }).catch(() => {});
    await page.waitForTimeout(1200);
    const printed = await page.evaluate(() => window.__print);
    console.log(`Export PDF:  ${printed ? "reaches window.print()" : "did not reach window.print()"}`);
    await page.keyboard.press("Escape").catch(() => {});
    const before = await page.title();
    await page.evaluate(retitle, null);
    await page.waitForTimeout(500);
    page.once("dialog", (d) => d.accept());
    await page.reload({ waitUntil: "load" }).catch(() => {});
    await page.waitForTimeout(3000);
    const survived = (await page.title()) !== before;
    console.log(`persistence: ${survived ? "the edit SURVIVED reload — storage is reachable; check the sandbox" : "an unsaved edit is gone after reload (expected: the sandbox has no storage)"}`);
    expect(survived, "the sandbox must have no storage").toBe(false);
  }
  const relevant = errors.filter((e) => !/bento\.page/.test(e));
  console.log(`console:     ${relevant.length} error(s)${errors.length !== relevant.length ? ` (+${errors.length - relevant.length} blocked update-check fetches to bento.page, expected)` : ""}`);
  relevant.slice(0, 8).forEach((e) => console.log(`  ${e}`));
});

// The edit session against the real app: press Bento's own Save and report
// what reached the save channel. This is the proof the synthetic deck cannot
// give — that the intercept catches the real runtime's save path.
test("a real Bento deck's Save reaches the edit session's channel", async ({ page, request }) => {
  const downloads = [];
  page.on("download", (d) => downloads.push(d.suggestedFilename()));
  await request.post("/__fixture/reset");
  const response = await page.goto("/bento/real-edit", { waitUntil: "load" });
  expect(response.status(), await response.text()).toBe(200);
  await page.waitForTimeout(4000);
  if (!(await page.locator('button[title^="Save"]').count())) test.skip(true, "a readonly (player) deck has no Save to press");
  await page.evaluate(retitle, "Saved through the channel");
  await page.locator('button[title^="Save"]').first().click({ timeout: 5000 }).catch(() => {});
  await page.waitForTimeout(2500);
  const toast = await page.locator("[data-pages-save-toast] p").textContent().catch(() => null);
  const events = (await (await request.get("/__fixture/events")).json()).events;
  const received = (events.find((e) => e.path === "/bento/real-save") || {}).body;
  console.log(`edit session: ${received ? `Save posted ${received.bytes} bytes (deck: ${received.is_deck}, title: ${JSON.stringify(received.title)}) with ${received.authorization} from origin ${received.origin}, cookies: ${received.cookie === null ? "none" : "PRESENT"}, collab keys in body: ${received.collab_keys}` : "Save did NOT reach the channel"}`);
  console.log(`  toast:     ${JSON.stringify(toast)}`);
  console.log(`  download:  ${downloads.length ? downloads.join(", ") + " (should be none when the save succeeded)" : "none (correct)"}`);
  expect(received, "Save did not reach the channel").toBeTruthy();
  expect(received.is_deck).toBe(true);
  expect(received.cookie).toBeNull();
  expect(downloads).toEqual([]);
});
