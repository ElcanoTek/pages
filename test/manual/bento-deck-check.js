// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// bento-deck-check — serve a REAL Bento deck under the content host's real
// rawHeaders() and report what a reader would get. The browser suite covers the
// boot mechanism with a synthetic deck; this is for the 689KB application itself,
// which is not vendored here. Run it when Bento is re-vendored in fleet, or when
// lib/csp.js changes.
//
//   node test/manual/bento-deck-check.js path/to/Deck.bento.html
//
// Reports: boot, which mode opened (editor or player), what Save does, whether
// Export PDF reaches window.print, whether an edit survives a reload, and every
// console error. It never writes anything.

const fs = require("node:fs");
const path = require("node:path");
const express = require("express");
const { chromium } = require("playwright");
const { rawHeaders } = require("../../lib/csp");
const render = require("../../lib/render");

const deckPath = process.argv[2];
if (!deckPath) {
  console.error("usage: node test/manual/bento-deck-check.js <deck.bento.html>");
  process.exit(2);
}
const html = fs.readFileSync(path.resolve(deckPath), "utf8");
if (!render.isBentoDeck(html)) {
  console.error("not a Bento deck: no <script type=\"application/bento+json\"> block");
  process.exit(2);
}

const app = express();
app.get("/raw/deck", (_req, res) => res.set(rawHeaders()).type("html").send(render.renderVersion({ render_mode: "raw", html })));
const server = app.listen(0, async () => {
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await chromium.launch();
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, acceptDownloads: true });
  const page = await context.newPage();
  const errors = [];
  const downloads = [];
  page.on("console", (m) => { if (m.type() === "error") errors.push(m.text().slice(0, 160)); });
  page.on("pageerror", (e) => errors.push(`uncaught: ${String(e.message).slice(0, 160)}`));
  page.on("download", (d) => downloads.push(d.suggestedFilename()));
  // Nothing is stubbed: the host adaptation Pages injects removes the native
  // file-picker API (the sandbox refuses it anyway), so Save must download.
  await page.addInitScript(() => { window.__print = 0; window.print = () => { window.__print += 1; }; });

  await page.goto(`${origin}/raw/deck`, { waitUntil: "load" });
  await page.waitForTimeout(4000);
  const failed = await page.getByText(/This file could not start/).count();
  const editor = await page.locator('button[title^="Save"]').count();
  const player = await page.locator(".reveal").count();
  console.log(`boot:        ${failed ? "FAILED — " + (await page.locator("body").innerText()).slice(0, 140) : "ok"}`);
  console.log(`mode:        ${editor ? "editor" : player ? "player (readonly deck)" : "unknown"}`);

  console.log(`picker API:  ${await page.evaluate(() => typeof window.showSaveFilePicker)} (must be undefined — Pages removes it so Bento downloads)`);
  if (editor) {
    const dl = page.waitForEvent("download", { timeout: 6000 }).catch(() => null);
    await page.locator('button[title^="Save"]').first().click({ timeout: 5000 }).catch(() => {});
    const download = await dl;
    if (download) {
      const saved = await download.createReadStream().then((stream) => new Promise((resolve) => { let text = ""; stream.setEncoding("utf8"); stream.on("data", (c) => (text += c)); stream.on("end", () => resolve(text)); }));
      const block = (saved.match(/<script\b[^>]*application\/bento\+json[^>]*>([\s\S]*?)<\/script/i) || [])[1] || "";
      console.log(`Save:        downloads ${download.suggestedFilename()} (${saved.length} bytes)`);
      console.log(`  carries:   guard CSP ${/http-equiv="Content-Security-Policy"/.test(saved) ? "yes" : "NO"} · host tag ${/data-pages-deck-host/.test(saved) ? "yes (stripped on upload)" : "no"} · collab keys ${/"collab"\s*:\s*\{/.test(block) ? "YES — preflight will warn; bento_doc.py set drops them" : "no"}`);
    } else {
      console.log("Save:        did nothing observable — a regression: Bento's Save reached for a picker the sandbox refuses");
    }
    await page.keyboard.press("Escape").catch(() => {});
    await page.locator('button[title^="Export PDF"]').first().click({ timeout: 5000 }).catch(() => {});
    await page.waitForTimeout(1200);
    console.log(`Export PDF:  ${(await page.evaluate(() => window.__print)) ? "reaches window.print()" : "did not reach window.print()"}`);
    await page.keyboard.press("Escape").catch(() => {});
    const before = await page.title();
    await page.evaluate(() => { const t = [...document.querySelectorAll("input")].find((i) => i.value && document.title.startsWith(i.value)); if (t) { t.value = t.value + " (edited)"; t.dispatchEvent(new Event("input", { bubbles: true })); t.dispatchEvent(new Event("change", { bubbles: true })); } });
    await page.waitForTimeout(500);
    page.once("dialog", (d) => d.accept());
    await page.reload({ waitUntil: "load" }).catch(() => {});
    await page.waitForTimeout(3000);
    console.log(`persistence: ${(await page.title()) === before ? "an unsaved edit is gone after reload (expected: the sandbox has no storage)" : "the edit SURVIVED reload — storage is reachable; check the sandbox"}`);
  }
  const relevant = errors.filter((e) => !/bento\.page/.test(e));
  console.log(`console:     ${relevant.length} error(s)${errors.length !== relevant.length ? ` (+${errors.length - relevant.length} blocked update-check fetches to bento.page, expected)` : ""}`);
  relevant.slice(0, 8).forEach((e) => console.log(`  ${e}`));
  await browser.close();
  server.close();
});
