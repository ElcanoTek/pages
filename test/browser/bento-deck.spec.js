// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// A Bento deck is one .bento.html that is its own viewer and editor. Its runtime
// is deflate-compressed and boots through a blob: module import, which is the
// one thing the content host's CSP used to forbid — so a deck deployed to Pages
// died with "This file could not start" while preflight, which reads plaintext,
// called it clean. These run the real boot mechanism under the real headers.

const { test, expect } = require("@playwright/test");
const fs = require("node:fs/promises");

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

// ── the edit session ─────────────────────────────────────────────────────────
// Everything above is what a READER of a deck gets. Staff get an edit session:
// the same deck, served for a signed token, with connect-src opened to Pages'
// own origin and Bento's Save intercepted into a draft version. These pin the
// channel's shape — who may reach what — not only that a save happens.

const ORIGIN_RE = (origin) => new RegExp(`connect-src ${origin.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}(;|$)`);

test("an edit session opens connect-src to Pages alone, and Save posts a draft instead of downloading", async ({ page, request }) => {
  const response = await page.goto("/bento/edit");
  const origin = new URL(page.url()).origin;
  const csp = response.headers()["content-security-policy"];
  expect(csp).toMatch(ORIGIN_RE(origin));
  expect(csp).not.toMatch(/connect-src 'none'/);
  expect(csp).toMatch(/sandbox allow-scripts/, "the sandbox is untouched — the token is the credential, not the origin");
  await expect(page.locator("#booted")).toHaveText("Synthetic deck booted");
  // The deck's own guard is widened the same way and no further.
  const guard = await page.locator('meta[http-equiv="Content-Security-Policy"]').getAttribute("content");
  expect(guard).toContain(`connect-src ${origin}`);
  expect(guard).toContain("default-src 'none'");
  // Pages' tags have run and left the DOM, so what Bento serialises is the deck.
  await expect(page.locator("script[data-pages-deck-host]")).toHaveCount(0);

  const downloads = [];
  page.on("download", (d) => downloads.push(d.suggestedFilename()));
  await page.locator("#save").click();
  await expect(page.locator("[data-pages-save-toast] p")).toContainText("Saved to Pages as a new draft version");
  expect(downloads, "a save that reached Pages does not also download").toEqual([]);

  const events = (await (await request.get("/__fixture/events")).json()).events;
  const save = events.find((event) => event.path === "/bento/save");
  expect(save, "Pages received the save").toBeTruthy();
  // The channel's shape: the token, from an opaque origin, with no cookie, a deck.
  expect(save.body.authorization).toBe("Bearer fixture-edit-token");
  expect(save.body.origin).toBe("null");
  expect(save.body.cookie).toBeNull();
  expect(save.body.is_deck).toBe(true);
  expect(save.body.base_version).toBe("41");
  // What Bento serialises carries the widened guard (prepareDeploy restores it)
  // and none of Pages' tags (they removed themselves).
  expect(save.body.guard_widened).toBe(true);
  expect(save.body.tags_carried).toBe(false);
});

test("a save Pages refuses falls back to the download, and says so", async ({ page }) => {
  await page.goto("/bento/edit?fail=1");
  await expect(page.locator("#booted")).toBeVisible();
  const download = page.waitForEvent("download");
  await page.locator("#save").click();
  // A save is never lost to a failure on our side: the file downloads exactly as
  // it would have without the channel, and the toast says what to do with it.
  expect((await download).suggestedFilename()).toBe("Synthetic.bento.html");
  await expect(page.locator("[data-pages-save-toast] p")).toContainText(/Couldn.t save to Pages/);
  await expect(page.locator("[data-pages-save-toast] p")).toContainText(/Starting a download/);
});

for (const failure of ["response", "network"]) {
  test(`a ${failure} failure downloads the saved bytes after Bento revokes its URL`, async ({ page }) => {
    if (failure === "network") {
      await page.route("**/bento/save", route => route.request().method() === "POST" ? route.abort() : route.continue());
    }
    await page.goto(failure === "response" ? "/bento/edit?fail=1" : "/bento/edit");
    await expect(page.locator("#booted")).toBeVisible();
    const exact = "<!doctype html><html><body>Northwind’s unsaved deck — exact bytes</body></html>";
    await page.evaluate(html => {
      const button = document.createElement("button");
      button.id = "save-revoked"; button.textContent = "Save and release file";
      button.onclick = () => {
        const anchor = document.createElement("a");
        anchor.href = URL.createObjectURL(new Blob([html], { type: "text/html" }));
        anchor.download = "Northwind.bento.html";
        document.body.appendChild(anchor);
        anchor.click();
        URL.revokeObjectURL(anchor.href);
        anchor.href = "#"; anchor.download = "Changed-after-save.html";
        anchor.remove();
      };
      document.body.appendChild(button);
    }, exact);
    const started = page.waitForEvent("download", { timeout: 5000 });
    await page.locator("#save-revoked").click();
    const download = await started;
    expect(download.suggestedFilename()).toBe("Northwind.bento.html");
    expect(await download.failure()).toBeNull();
    expect(await fs.readFile(await download.path(), "utf8")).toBe(exact);
    await expect(page.locator("[data-pages-save-toast] p")).toContainText(/Couldn.t save to Pages/);
  });
}

for (const failing of [false, true]) {
  test(`repeated ${failing ? "failed" : "successful"} saves release their HTML object URLs`, async ({ page }) => {
    await page.addInitScript(() => {
      window.htmlUrls = { created: [], revoked: [] };
      const create = URL.createObjectURL, revoke = URL.revokeObjectURL;
      URL.createObjectURL = function(blob) {
        const url = create.call(this, blob);
        if (blob.type === "text/html") window.htmlUrls.created.push(url);
        return url;
      };
      URL.revokeObjectURL = function(url) { window.htmlUrls.revoked.push(url); return revoke.call(this, url); };
    });
    await page.goto(failing ? "/bento/edit?fail=1" : "/bento/edit");
    await expect(page.locator("#booted")).toBeVisible();
    for (let i = 0; i < 3; i += 1) {
      const response = page.waitForResponse(r => r.request().method() === "POST" && r.url().includes("/bento/save"));
      await page.locator("#save").click(); await response;
    }
    await expect.poll(() => page.evaluate(() => window.htmlUrls.created.length)).toBe(failing ? 6 : 3);
    await expect.poll(() => page.evaluate(() => window.htmlUrls.created.every(url => window.htmlUrls.revoked.includes(url)))).toBe(true);
  });
}

test("a fallback that cannot start tells the editor to keep its unsaved work open", async ({ page }) => {
  await page.goto("/bento/edit?fail=1");
  await expect(page.locator("#booted")).toBeVisible();
  await page.evaluate(() => {
    const append = document.body.appendChild;
    document.body.appendChild = function(node) {
      if (node.tagName === "A") throw new Error("fixture: download could not start");
      return append.call(this, node);
    };
  });
  await page.locator("#save").click();
  const toast = page.locator("[data-pages-save-toast] p");
  await expect(toast).toContainText("Couldn’t start the download. Keep this editor open and try Save again.");
  await expect(toast).not.toContainText("Starting a download");
});

test("overlapping rejected saves retain each file after the producer revokes both URLs", async ({ page }) => {
  await page.goto("/bento/edit");
  await expect(page.locator("#booted")).toBeVisible();
  let release, arrived;
  const reply = new Promise(resolve => { release = resolve; });
  const started = new Promise(resolve => { arrived = resolve; });
  await page.route("**/bento/save", async route => {
    if (route.request().method() !== "POST") return route.continue();
    arrived(); await reply;
    await route.fulfill({ status: 409, headers: { "Access-Control-Allow-Origin": "null" },
      json: { error: "A newer version exists. Reopen the editor from the admin.", code: "stale_deck_version" } });
  });
  const downloads = [];
  page.on("download", download => downloads.push(download));
  await page.evaluate(() => {
    let count = 0;
    const button = document.createElement("button"); button.id = "save-overlap"; button.textContent = "Save another snapshot";
    button.onclick = () => {
      count += 1;
      const anchor = document.createElement("a");
      anchor.href = URL.createObjectURL(new Blob([`<html><body>Snapshot ${count}</body></html>`], { type: "text/html" }));
      anchor.download = `Northwind-${count}.bento.html`;
      anchor.click(); URL.revokeObjectURL(anchor.href);
    };
    document.body.appendChild(button);
  });
  await page.locator("#save-overlap").click(); await started;
  await page.locator("#save-overlap").click(); release();
  await expect.poll(() => downloads.length).toBe(2);
  const contents = {};
  for (const download of downloads) {
    expect(await download.failure()).toBeNull();
    contents[download.suggestedFilename()] = await fs.readFile(await download.path(), "utf8");
  }
  expect(contents).toEqual({
    "Northwind-1.bento.html": "<html><body>Snapshot 1</body></html>",
    "Northwind-2.bento.html": "<html><body>Snapshot 2</body></html>",
  });
});

test("a viewer's deck has no save channel at all", async ({ page }) => {
  const response = await page.goto("/bento/deck");
  expect(response.headers()["content-security-policy"]).toMatch(/connect-src 'none'/);
  await expect(page.locator("#booted")).toBeVisible();
  // Not even to Pages: without an edit token there is nothing to talk to.
  const outcome = await page.evaluate(async (origin) => {
    try { await fetch(origin + "/bento/save", { method: "POST", body: "x" }); return "connected"; } catch (e) { return e.name; }
  }, new URL(page.url()).origin);
  expect(outcome).toBe("TypeError");
  await expect(page.locator("[data-pages-save-toast]")).toHaveCount(0);
});
