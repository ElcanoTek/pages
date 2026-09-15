// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { test, expect } = require("@playwright/test");
const { openCampaign, exportCampaign, metricRow } = require("./campaign-dashboard-helpers");

// Independent expected arithmetic: spend 190, impressions 4000, clicks 50,
// conversions 15, completed views 2000 and viewable impressions 2200.
const metrics = [
  ["cpm", 47.5, "$47.50", "dspImpressions"],
  ["cpc", 3.8, "$3.80", "clicks"],
  ["cpa", 190 / 15, "$12.67", "conversions"],
  ["cpcv", 0.095, "$0.10", "completedViews"],
  ["ctr", 0.0125, "1.25%", "dspImpressions"],
  ["vcr", 0.5, "50.00%", "dspImpressions"],
  ["viewability", 0.55, "55.00%", "dspImpressions"],
];

for (const [kpi, expected, formatted, denominator] of metrics) {
  test(`${kpi} reconciles cards, tables, charts and CSV using summed metric inputs`, async ({ page }) => {
    await openCampaign(page, { kpi, rows: [metricRow("2026-06-01"), metricRow("2026-06-02", {
      dspSpend: 90, dspImpressions: 3000, clicks: 30, conversions: 5, completedViews: 1200, viewableImpressions: 1500,
    })] });
    await expect(page.locator("#hero .hcard").last().locator(".val")).toHaveText(formatted);
    await expect(page.locator("#tracking tbody tr").first().locator("td").nth(10)).toHaveText(formatted);
    await expect(page.locator("#wowTable tbody tr").first().locator("td").nth(4)).toHaveText(formatted);
    await page.locator("#plot-display .hz").last().dispatchEvent("mousemove", { clientX: 200, clientY: 200 });
    await expect(page.locator("#tip-display .r").filter({ hasText: `Cumulative ${kpi.toUpperCase()}` }).locator("span").last())
      .toHaveText(formatted);
    const csv = await exportCampaign(page);
    const lines = csv.text.split("\n\n")[1].trim().split("\n");
    const index = lines[0].split(",").indexOf("KPI");
    expect(Number(lines[1].split(",")[index])).toBeCloseTo(expected, 10);

    // Missing or zero denominators must never display a fabricated zero KPI.
    for (const value of [0, undefined]) {
      const actual = await page.evaluate(({ denominator, value }) => {
        const row = { ...ROWS[0] };
        if (value === undefined) delete row[denominator]; else row[denominator] = value;
        return { value: kpiValue(CONFIG.channels[0], [row]), formatted: kpiFmt(CONFIG.channels[0], kpiValue(CONFIG.channels[0], [row])) };
      }, { denominator, value });
      expect(actual).toEqual({ value: null, formatted: "N/A" });
    }
  });
}

test("KPI inputs distinguish missing observations from measured zeroes", async ({ page }) => {
  await openCampaign(page, { kpi: "vcr", rows: [metricRow("2026-06-01")] });
  const actual = await page.evaluate(() => {
    const channel = CONFIG.channels[0];
    const complete = ROWS[0];
    const missing = { ...complete }; delete missing.completedViews;
    return {
      missing: kpiValue(channel, [missing]), mixed: kpiValue(channel, [complete, missing]),
      zero: kpiValue(channel, [{ ...complete, completedViews: 0 }]),
      unrelated: kpiValue(channel, [complete, { dealId: complete.dealId, date: complete.date, revenue: 50 }]),
    };
  });
  expect(actual).toEqual({ missing: null, mixed: null, zero: 0, unrelated: 0.8 });
});

test("channel-only conversions reconcile totals and CPA without leaking into deal rows", async ({ page }) => {
  await openCampaign(page, { kpi: "cpa", rows: [
    metricRow("2026-06-01"), metricRow("2026-06-02", { dspSpend: 50, conversions: 5 }),
    metricRow("2026-06-01", { dealId: "northwind-b", dspSpend: 80, conversions: 8 }),
  ], unallocated: [
    { channel: "display", date: "2026-06-01", conversions: 2 },
    { channel: "display", date: "2026-06-02", conversions: 3 },
    { channel: "display", date: "2026-05-31", conversions: 999 },
  ], config: { deals: [
    { id: "northwind-a", channel: "display", short: "Alpha", full: "Northwind Alpha", code: "A" },
    { id: "northwind-b", channel: "display", short: "Beta", full: "Northwind Beta", code: "A" },
  ] } });
  // Allocated = 23, channel-only = 5, spend = 230, total CPA = 230/28.
  const table = page.locator("#tracking tbody");
  await expect(table.locator("tr").first().locator("td").nth(12)).toHaveText("15");
  await expect(table.locator("tr").nth(1).locator("td").nth(12)).toHaveText("8");
  await expect(table.locator("tr.total td").nth(12)).toHaveText("28");
  await expect(table.locator("tr.total td").nth(10)).toHaveText("$8.21");
  await expect(page.locator("#hero .hcard").last().locator(".val")).toHaveText("$8.21");
  await expect(page.locator("#tracking")).toContainText("5 purchases in the selected date range. Included once");
  const weekly = await exportCampaign(page, "wow");
  expect(weekly.text.split("\n\n")[1]).toContain("230,3000,60,28");
  const deals = (await exportCampaign(page)).text.split("\n\n")[1].trim().split("\n");
  expect(deals.map(line => line.split(",")[16])).toEqual(["Purchases", "15", "8", "5", "28"]);
  expect(Number(deals.at(-1).split(",")[14])).toBeCloseTo(230 / 28, 10);

  await page.evaluate(() => { STATE.start = STATE.end = "2026-06-02"; renderAll(); });
  await expect(table.locator("tr.total td").nth(12)).toHaveText("8");
  await expect(table.locator("tr.total td").nth(10)).toHaveText("$6.25");
  await expect(page.locator("#tracking")).toContainText("3 purchases in the selected date range");
  await page.evaluate(() => { STATE.deals = ["northwind-a"]; renderAll(); });
  await expect(table.locator("tr.total td").nth(12)).toHaveText("5");
  await expect(table.locator("tr.total td").nth(10)).toHaveText("$10.00");
  await expect(page.locator("#tracking")).toContainText("Excluded from these totals because the deal filter selects only part of this channel");
  expect((await exportCampaign(page)).text).not.toContain("Channel-only conversions");
});

test("campaign labels remain text and the channel export button keeps its binding", async ({ page }) => {
  await openCampaign(page, { rows: [metricRow("2026-06-01")], config: { channels: [
    { id: "display", name: "Northwind <Q3> & partners", kpi: "cpm", kpiLabel: "CPM", target: 3, yellow: 4,
      lowerIsBetter: true, unit: "$", decimals: 2 },
  ] } });
  await expect(page.locator("#hero .hcard").last().locator(".lab")).toHaveText("Northwind <Q3> & partners CPM");
  const pending = page.waitForEvent("download");
  await page.locator("#tracking").getByRole("button", { name: "Export Excel" }).click();
  expect((await pending).suggestedFilename()).toBe("TEST001_display_deal_2026-06-01_to_2026-06-01.csv");
});

test("daily CSV groups the visible dates and honors channel and deal scope", async ({ page }) => {
  await openCampaign(page, { kpi: "cpa", rows: [
    metricRow("2026-06-01"), metricRow("2026-06-02", { dspSpend: 50, conversions: 5 }),
    metricRow("2026-06-01", { dealId: "northwind-b", dspSpend: 40, conversions: 4 }),
    metricRow("2026-06-02", { dealId: "northwind-b", dspSpend: 30, conversions: 3 }),
  ], unallocated: [{ channel: "display", date: "2026-06-02", conversions: 2 }], config: { deals: [
    { id: "northwind-a", channel: "display", short: "Alpha", full: "Northwind Alpha", code: "A" },
    { id: "northwind-b", channel: "display", short: "Beta", full: "Northwind Beta", code: "A" },
  ] } });
  await page.evaluate(() => { STATE.dim = "daily"; renderAll(); });
  const daily = await exportCampaign(page);
  expect(daily.filename).toBe("TEST001_display_daily_2026-06-01_to_2026-06-02.csv");
  expect(daily.text).toContain("View,Daily");
  const lines = daily.text.split("\n\n")[1].trim().split("\n").map(line => line.split(","));
  expect(lines[0][0]).toBe("Date");
  expect(lines[0]).not.toContain("Deal ID");
  expect(lines.slice(1).map(row => [row[0], Number(row[2]), Number(row[10])]))
    .toEqual([["2026-06-01", 140, 14], ["2026-06-02", 80, 10], ["Total - Display", 220, 24]]);
  expect(Number(lines[2][8])).toBe(8);
  const table = page.locator("#tracking tbody tr");
  await expect(table.nth(1).locator("td").nth(9)).toHaveText("$8.00");
  await expect(table.nth(1).locator("td").nth(11)).toHaveText("10");
  await expect(table.last().locator("td").nth(11)).toHaveText("24");

  await page.evaluate(() => { STATE.start = "2026-06-02"; STATE.deals = ["northwind-a"]; renderAll(); });
  const filtered = (await exportCampaign(page)).text.split("\n\n")[1].trim().split("\n").map(line => line.split(","));
  expect(filtered.slice(1).map(row => [row[0], Number(row[2]), Number(row[10])]))
    .toEqual([["2026-06-02", 50, 5], ["Total - Display", 50, 5]]);
  await expect(table.first().locator("td").nth(11)).toHaveText("5");
  await page.evaluate(() => { STATE.dim = "deal"; renderAll(); });
  expect((await exportCampaign(page)).text.split("\n\n")[1]).toMatch(/^Deal Name,Deal ID/);
  expect((await exportCampaign(page, "wow")).text.split("\n\n")[1]).toMatch(/^Week,Partial/);
});
