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
