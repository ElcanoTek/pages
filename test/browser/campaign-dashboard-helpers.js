// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const fs = require("node:fs/promises");
const path = require("node:path");

async function openCampaign(page, { kpi = "cpm", rows, unallocated = [], config = {} }) {
  let html = await fs.readFile(path.join(__dirname, "../../templates/nwm-campaign-dashboard/template.html"), "utf8");
  const reference = JSON.parse(html.match(/id="pages-config">([\s\S]*?)<\/script>/)[1]);
  const cost = ["cpm", "cpc", "cpa", "cpcv"].includes(kpi);
  const settings = {
    ...reference, campaign: "Northwind campaign", client: "Northwind", accountCode: "TEST001",
    flightStart: "2026-06-01", flightEnd: "2026-06-30",
    channels: [{ id: "display", name: "Display", kpi, kpiLabel: kpi.toUpperCase(), target: cost ? 3 : 0.5,
      yellow: cost ? 5 : 0.4, lowerIsBetter: cost, unit: cost ? "$" : "%", decimals: 2 }],
    deals: [{ id: "northwind-a", channel: "display", short: "Alpha", full: "Northwind Alpha", code: "A" }],
    revshareMap: { A: { pct: 50, type: "margin" } }, ...config,
  };
  const data = { dataThrough: rows.length ? rows.map(row => row.date).sort().at(-1) : null,
    lastRefreshed: "Jun 30, 2026", unmapped: { sspRows: 0, dspRows: 0 }, rows, unallocated };
  const encode = value => JSON.stringify(value).replace(/</g, "\\u003c");
  html = html.replace(/(<script[^>]+id="pages-config">)[\s\S]*?(<\/script>)/, (_, start, end) => start + encode(settings) + end);
  html = html.replace(/(<script[^>]+id="pages-data">)[\s\S]*?(<\/script>)/, (_, start, end) => start + encode({ contract_version: 1,
    source_as_of: "2026-06-30T00:00:00Z", refreshed_at: "2026-06-30T00:00:00Z", data }) + end);
  await page.route("**/campaign-dashboard-fixture", route => route.fulfill({ contentType: "text/html", body: html }));
  await page.goto("/campaign-dashboard-fixture");
}

async function exportCampaign(page, channel = "display") {
  const pending = page.waitForEvent("download");
  await page.evaluate(which => exportCsv(which), channel);
  const download = await pending;
  return { filename: download.suggestedFilename(), text: await fs.readFile(await download.path(), "utf8") };
}

function metricRow(date, overrides = {}) {
  return { dealId: "northwind-a", date, revenue: 200, grossMargin: 80, platformFee: 20, sspImpressions: 1000,
    dspSpend: 100, dspImpressions: 1000, clicks: 20, conversions: 10, completedViews: 800, viewableImpressions: 700,
    ...overrides };
}

module.exports = { openCampaign, exportCampaign, metricRow };
