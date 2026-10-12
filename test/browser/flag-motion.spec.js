// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const fs = require("node:fs/promises");
const { test, expect } = require("@playwright/test");

// A page that opts in to the kit exactly as a deployed one does: the two
// same-origin assets, plus whatever managed blocks the case needs.
async function openKitPage(page, { body, envelope, provenance, schema, config, reducedMotion, head = "", htmlAttrs = "" } = {}) {
  if (reducedMotion) await page.emulateMedia({ reducedMotion: "reduce" });
  const block = (type, id, value) =>
    value === undefined ? "" : `<script type="${type}" id="${id}">${JSON.stringify(value).replace(/</g, "\\u003c")}</script>`;
  const html = `<!doctype html><html ${htmlAttrs}><head><meta charset="utf-8">${head}
<link rel="stylesheet" href="/assets/flag/motion/flag-motion.css">
${block("application/schema+json", "pages-data-schema", schema)}
${block("application/json", "pages-data", envelope)}
${block("application/json", "pages-config", config)}
${block("application/json", "pages-provenance", provenance)}
</head><body style="margin:24px;width:900px">${body}
<script src="/assets/flag/motion/flag-motion.js"></script></body></html>`;
  await page.route("**/flag-motion-fixture", route => route.fulfill({ contentType: "text/html", body: html }));
  await page.goto("/flag-motion-fixture");
  await page.waitForFunction(() => document.documentElement.classList.contains("fm-ready"));
}
const hoursAgo = (h) => new Date(Date.now() - h * 3600e3).toISOString();

test("count-up numbers land on their exact formatted values", async ({ page }) => {
  await openKitPage(page, { body: `
    <span id="usd" data-fm-count="1478.05" data-fm-format="usd"></span>
    <span id="pct" data-fm-count="0.1234" data-fm-format="pct1"></span>
    <span id="compact" data-fm-count="1395658" data-fm-format="compact"></span>
    <span id="text" data-fm-count data-fm-format="int">12,345</span>` });
  await expect(page.locator("#usd")).toHaveText("$1,478.05");
  await expect(page.locator("#pct")).toHaveText("12.3%");
  await expect(page.locator("#compact")).toHaveText("1.4M");
  await expect(page.locator("#text")).toHaveText("12,345");
  await expect(page.locator("#usd")).toHaveAttribute("aria-label", "$1,478.05");
});

test("freshness reads the page's own refresh stamps, and says so when there are none", async ({ page }) => {
  await openKitPage(page, {
    body: `<span id="f" data-fm-freshness></span>`,
    envelope: { contract_version: 1, refreshed_at: hoursAgo(3), source_as_of: "2026-10-06T23:59:59Z", data: {} },
  });
  await expect(page.locator("#f")).toHaveText(/Updated 3 h ago\s*· data through Oct 6, 2026/);
  await expect(page.locator("#f")).toHaveClass(/fm-live/);

  await openKitPage(page, {
    body: `<span id="f" data-fm-freshness></span>`,
    envelope: { contract_version: 1, refreshed_at: "1970-01-01T00:00:00Z", source_as_of: "1970-01-01T00:00:00Z", data: {} },
  });
  await expect(page.locator("#f")).toHaveText("Awaiting first data");
});

test("provenance: a number opens what it is, how it is computed and where it came from", async ({ page }) => {
  await openKitPage(page, {
    body: `<p>Spend <b id="spend" data-fm-metric="revenue">$1,478.05</b> · CTR <b id="ctr" data-fm-metric="ctr">0.62%</b>
      · <b id="both" data-fm-metric="revenue" data-fm-count="9876" data-fm-format="usd0"></b></p>`,
    envelope: { contract_version: 1, refreshed_at: hoursAgo(2), source_as_of: "2026-10-06T23:59:59Z", data: {} },
    provenance: {
      metrics: { revenue: { label: "SSP spend", definition: "What buyers spent on the campaign's deals, as the SSPs report it.",
        formula: "Σ Spend ($) over in-scope deals", source: "twc_ssp", filter: "Deal names carrying ELC07361" } },
      sources: { twc_ssp: { label: "TWC SSP history", location: "Fast.io · Storage/twc_ssp_historical_data.csv", cadence: "Daily, 14:30 UTC" } },
    },
    schema: { type: "object", properties: { rows: { type: "array", items: { type: "object", properties: {
      ctr: { type: "number", description: "Clicks divided by impressions, summed before dividing." } } } } } },
  });
  const spend = page.locator("#spend");
  await expect(spend).toHaveAttribute("role", "button");
  await spend.click();
  const pop = page.getByRole("dialog", { name: /SSP spend/ });
  await expect(pop).toBeVisible();
  await expect(pop).toContainText("$1,478.05");
  await expect(pop).toContainText("What buyers spent");
  await expect(pop).toContainText("Σ Spend ($) over in-scope deals");
  await expect(pop).toContainText("TWC SSP history");
  await expect(pop).toContainText("Deal names carrying ELC07361");
  await expect(pop).toContainText("Daily, 14:30 UTC");
  await expect(pop).toContainText("data through Oct 6, 2026");
  await page.keyboard.press("Escape");
  await expect(pop).toHaveCount(0);
  await expect(spend).toBeFocused();

  // A headline number usually counts up AND explains itself.
  await page.keyboard.press("Escape");
  await page.locator("#both").click();
  await expect(page.getByRole("dialog", { name: /SSP spend/ })).toContainText("$9,876");
  await page.keyboard.press("Escape");

  // No provenance entry: the data schema's own description is the definition.
  await page.locator("#ctr").focus();
  await page.keyboard.press("Enter");
  await expect(page.getByRole("dialog")).toContainText("Clicks divided by impressions");
});

test("line chart: a path per series, direct end labels, a crosshair tooltip and a data table", async ({ page }) => {
  const labels = ["2026-10-01", "2026-10-02", "2026-10-03", "2026-10-04"];
  await openKitPage(page, { body: `<div id="c" data-fm-chart='${JSON.stringify({
    type: "line", title: "Daily spend", labels, format: "usd",
    series: [{ name: "SSP", values: [100, 140, 120, 180] }, { name: "DSP", values: [90, 110, null, 150] }],
  })}'></div>` });
  const chart = page.locator("#c");
  await expect(chart.locator(".fm-chart-title")).toHaveText("Daily spend");
  await expect(chart.locator("path.fm-line")).toHaveCount(2);
  await expect(chart.locator(".fm-legend span")).toHaveText(["SSP", "DSP"]);
  await expect(chart.locator("text.fm-direct")).toHaveText(["$180.00", "$150.00"]);
  const box = await chart.locator("rect.fm-hit").boundingBox();
  await page.mouse.move(box.x + box.width - 2, box.y + box.height / 2);
  const tip = chart.locator(".fm-tip");
  await expect(tip).toHaveClass(/fm-on/);
  await expect(tip).toContainText("Oct 4, 2026");
  await expect(tip).toContainText("$180.00");
  await expect(tip).toContainText("$150.00");
  await chart.locator(".fm-table summary").click();
  await expect(chart.locator(".fm-table tbody tr")).toHaveCount(4);
  await expect(chart.locator(".fm-table tbody tr").nth(2)).toContainText("N/A");
});

test("bar, ranked-bar and donut charts render; a donut folds past six slices into Other", async ({ page }) => {
  const items = ["Index", "Magnite", "PubMatic", "OpenX", "Sovrn", "TripleLift", "Xandr", "Equativ"]
    .map((label, i) => ({ label, value: 800 - i * 90 }));
  await openKitPage(page, { body: `
    <div id="bar" data-fm-chart='${JSON.stringify({ type: "bar", labels: ["Mon", "Tue", "Wed"], series: [{ name: "Spend", values: [3, 9, 5] }], format: "int" })}'></div>
    <div id="hbar" data-fm-chart='${JSON.stringify({ type: "hbar", items, format: "usd0", limit: 5 })}'></div>
    <div id="donut" data-fm-chart='${JSON.stringify({ type: "donut", items, format: "usd0", centerLabel: "Spend" })}'></div>` });
  await expect(page.locator("#bar path.fm-bar")).toHaveCount(3);
  await expect(page.locator("#bar text.fm-direct")).toHaveText("9");
  await expect(page.locator("#hbar path.fm-hbar")).toHaveCount(5);
  await expect(page.locator("#hbar text.fm-bar-value").first()).toHaveText("$800");
  await expect(page.locator("#hbar svg")).toContainText("+ 3 more");
  await expect(page.locator("#donut circle.fm-seg")).toHaveCount(6);
  await expect(page.locator("#donut .fm-legend > span").last()).toContainText("Other");
  await expect(page.locator("#donut text.fm-donut-caption")).toHaveText("Spend");
});

test("a chart bound to data: renders the page's managed data", async ({ page }) => {
  await openKitPage(page, {
    body: `<div id="c" data-fm-chart="data:charts.trend"></div>`,
    envelope: { contract_version: 1, refreshed_at: hoursAgo(1), source_as_of: "2026-10-06T23:59:59Z", data: { charts: { trend: {
      type: "area", labels: ["2026-10-05", "2026-10-06"], series: [{ name: "Spend", values: [10, 20] }], format: "usd0", source: "TWC SSP history" } } } },
  });
  await expect(page.locator("#c path.fm-area")).toHaveCount(1);
  await expect(page.locator("#c .fm-chart-foot")).toHaveText("Source: TWC SSP history · data through Oct 6, 2026");
});

test("reduced motion: reveals, counts and charts settle at once", async ({ page }) => {
  await openKitPage(page, { reducedMotion: true, body: `
    <div id="r" data-fm-reveal>hello</div><span id="n" data-fm-count="42" data-fm-format="int"></span>
    <div id="c" data-fm-chart='${JSON.stringify({ type: "bar", labels: ["a"], series: [{ name: "x", values: [1] }] })}'></div>` });
  await expect(page.locator("#r")).toHaveCSS("opacity", "1");
  await expect(page.locator("#n")).toHaveText("42");
  await expect(page.locator("#c")).not.toHaveClass(/fm-anim/);
});

const explainerSpec = {
  title: "Merrell in 4 seconds", brand: "Elcano · TWC", filename: "merrell-recap", source: "TWC SSP history",
  scenes: [
    { type: "title", duration: 900, kicker: "Campaign recap", title: "Merrell — first week" },
    { type: "kpis", duration: 900, title: "Delivery", items: [{ label: "Spend", value: 1478.05, format: "usd" }, { label: "Impressions", value: 1395658, format: "compact", delta: 0.12 }] },
    { type: "bars", duration: 900, title: "Top deals", items: [{ label: "Index — Crisp Fall Air", value: 620 }, { label: "Magnite — Cardigan", value: 410 }], format: "usd0" },
    { type: "line", duration: 900, title: "Daily spend", labels: ["2026-10-01", "2026-10-02", "2026-10-03"], series: [{ name: "Spend", values: [120, 260, 310] }], format: "usd0" },
  ],
};

test("explainer: a timeline of scenes that scrubs, plays to the end and reads as text", async ({ page }) => {
  await openKitPage(page, { body: `<div id="x" data-fm-explainer='${JSON.stringify(explainerSpec)}'></div>` });
  const x = page.locator("#x");
  await expect(x.locator("canvas")).toHaveAttribute("width", "1920");
  await expect(x.locator(".fm-ex-chip")).toHaveText(["Merrell — first week", "Delivery", "Top deals", "Daily spend"]);
  await expect(x.locator("ol.fm-sr li").nth(1)).toContainText("Spend: $1,478.05");
  await expect(x.locator("ol.fm-sr li").nth(1)).toContainText("Impressions: 1.4M (+12.0%)");
  const state = () => page.evaluate(() => FlagMotion.registry.explainers[0].state());
  expect((await state()).duration).toBe(3600);
  expect((await state()).time).toBeGreaterThan(0); // the poster: first scene settled, not a blank frame
  await page.evaluate(() => { const p = FlagMotion.registry.explainers[0]; p.pause(); p.seek(2000); });
  expect(await state()).toMatchObject({ scene: 2, playing: false });
  await expect(x.locator(".fm-ex-chip").nth(2)).toHaveAttribute("aria-current", "true");
  await expect(x.locator(".fm-ex-time")).toHaveText("0:02 / 0:04");
  // The canvas is painted: not one flat colour.
  const distinct = await page.evaluate(() => {
    const c = document.querySelector("#x canvas"), d = c.getContext("2d").getImageData(0, 0, c.width, c.height).data;
    const seen = new Set();
    for (let i = 0; i < d.length; i += 4 * 997) seen.add(d[i] + "," + d[i + 1] + "," + d[i + 2]);
    return seen.size;
  });
  expect(distinct).toBeGreaterThan(20);
  await page.evaluate(() => { const p = FlagMotion.registry.explainers[0]; p.seek(3000); p.play(); });
  await expect.poll(state, { timeout: 5000 }).toMatchObject({ playing: false, time: 3600 });
});

test("explainer: Download video records the timeline into a file", async ({ page }) => {
  await openKitPage(page, { body: `<div id="x" data-fm-explainer='${JSON.stringify({ ...explainerSpec, scenes: explainerSpec.scenes.slice(0, 2), autoplay: false })}'></div>` });
  const button = page.getByRole("button", { name: "Download video" });
  await expect(button).toBeVisible();
  const pending = page.waitForEvent("download", { timeout: 15000 });
  await button.click();
  const download = await pending;
  expect(download.suggestedFilename()).toMatch(/^merrell-recap\.(mp4|webm)$/);
  const bytes = (await fs.stat(await download.path())).size;
  expect(bytes).toBeGreaterThan(1000);
  await expect(page.locator(".fm-ex-status")).toContainText("Saved merrell-recap.");
});

test("the kit follows the page's real background, not only the theme flag", async ({ page }) => {
  // A custom light design under Flag's dark theme: dark-scheme text would vanish.
  await openKitPage(page, { htmlAttrs: 'data-theme="dark"', head: "<style>body{background:#ffffff}</style>",
    body: `<span id="f" data-fm-freshness></span>` });
  await expect(page.locator("html")).toHaveAttribute("data-fm-scheme", "light");
  const ink = () => page.locator("#f .fm-fresh-label").evaluate(el => getComputedStyle(el).color);
  expect(await ink()).toBe("rgb(20, 24, 36)");
  // The page turns dark (a theme toggle): the kit follows.
  await page.evaluate(() => { document.body.style.background = "#1a0b1e"; });
  await expect(page.locator("html")).toHaveAttribute("data-fm-scheme", "dark");
  await expect.poll(ink).toBe("rgb(255, 255, 255)");
});
