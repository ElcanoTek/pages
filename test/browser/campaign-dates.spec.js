// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { test, expect } = require("@playwright/test");
const { openCampaign, metricRow } = require("./campaign-dashboard-helpers");

async function openDates(page, config = {}, dates = ["2026-06-05", "2026-06-20"]) {
  await openCampaign(page, { rows: dates.map(date => metricRow(date)),
    config: { flightStart: dates[0], ...config } });
  await page.locator("#dateTrigger").click();
}

async function appliedRange(page) {
  return page.evaluate(() => ({ start: STATE.start, end: STATE.end }));
}

test("invalid typed dates retain their text and leave the applied range unchanged", async ({ page }) => {
  await openDates(page);
  const initial = await appliedRange(page);
  const initialLabel = await page.locator("#dateTriggerLabel").textContent();
  for (const input of ["02/31/2026", "13/05/2026", "00/05/2026", "06/00/2026", "06/05/2026oops", "06x/05/2026", "02/29/2026"]) {
    await page.locator("#draftStart").fill(input);
    await page.locator("#dateApply").click();
    await expect(page.locator("#datePop")).toBeVisible();
    await expect(page.locator("#draftStart")).toHaveValue(input);
    await expect(page.locator("#draftStart")).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#dateError")).toContainText("valid date in MM/DD/YYYY");
    expect(await appliedRange(page)).toEqual(initial);
    await expect(page.locator("#dateTriggerLabel")).toHaveText(initialLabel);
  }
});

test("calendar navigation and editing the other endpoint preserve an invalid typed date", async ({ page }) => {
  await openDates(page);
  await page.locator("#draftStart").fill("06/31/2026");
  await page.getByRole("button", { name: "Forward 1 month", exact: true }).click();
  await expect(page.locator("#draftStart")).toHaveValue("06/31/2026");
  await page.locator("#draftEnd").fill("06/19/2026");
  await page.locator("#dateApply").click();
  await expect(page.locator("#draftStart")).toHaveValue("06/31/2026");
  await expect(page.locator("#draftEnd")).toHaveValue("06/19/2026");
  expect(await appliedRange(page)).toEqual({ start: "2026-06-05", end: "2026-06-20" });
  await page.locator("#dateCancel").click();
  await page.locator("#dateTrigger").click();
  await expect(page.locator("#draftStart")).toHaveValue("06/05/2026");
  await expect(page.locator("#dateError")).toBeHidden();
});

test("typed leap days use calendar rules without normalizing the operator's text", async ({ page }) => {
  await openDates(page, { flightEnd: "2024-03-01" }, ["2024-02-28", "2024-03-01"]);
  await page.locator("#draftStart").fill("02/29/2024");
  await page.locator("#draftEnd").fill("02/29/2024");
  await page.locator("#dateApply").click();
  expect(await appliedRange(page)).toEqual({ start: "2024-02-29", end: "2024-02-29" });
  await expect(page.locator("#dateTriggerLabel")).toHaveText("Feb 29 – Feb 29, 2024");
  expect(await page.evaluate(() => ["02/29/2000", "02/29/1900", "02/29/2100"].map(value => {
    const date = parseMDY(value); return date ? iso(date) : null;
  }))).toEqual(["2000-02-29", null, null]);
});

test("typed dates, calendar days and presets share the inclusive reporting bounds", async ({ page }) => {
  await openDates(page);
  await expect(page.locator('[data-d="2026-06-04"]')).toBeDisabled();
  await expect(page.locator('[data-d="2026-06-21"]')).toBeDisabled();
  await expect(page.locator('[data-d="2026-06-05"]')).toBeEnabled();
  await expect(page.locator('[data-d="2026-06-20"]')).toBeEnabled();
  await expect(page.getByRole("button", { name: "Last month", exact: true })).toBeDisabled();
  for (const [field, value] of [["draftStart", "06/04/2026"], ["draftEnd", "06/21/2026"]]) {
    await page.locator(`#${field}`).fill(value);
    await page.locator("#dateApply").click();
    await expect(page.locator("#datePop")).toBeVisible();
    await expect(page.locator("#dateError")).toContainText("06/05/2026 through 06/20/2026");
    expect(await appliedRange(page)).toEqual({ start: "2026-06-05", end: "2026-06-20" });
    await page.getByRole("button", { name: "Previous 30 days", exact: true }).click();
    await expect(page.locator("#draftStart")).toHaveValue("06/05/2026");
    await expect(page.locator("#draftEnd")).toHaveValue("06/20/2026");
    await expect(page.locator("#dateError")).toBeHidden();
  }
  await page.locator("#draftStart").fill("06/06/2026");
  await page.locator("#draftEnd").fill("06/19/2026");
  await page.locator("#dateApply").click();
  expect(await appliedRange(page)).toEqual({ start: "2026-06-06", end: "2026-06-19" });
  await page.locator("#dateTrigger").click();
  await page.locator('[data-d="2026-06-05"]').click();
  await page.locator('[data-d="2026-06-20"]').click();
  await page.locator("#dateApply").click();
  expect(await appliedRange(page)).toEqual({ start: "2026-06-05", end: "2026-06-20" });
});

test("empty and reversed typed ranges require correction while a calendar day can be applied alone", async ({ page }) => {
  await openDates(page);
  await page.locator("#draftEnd").fill("");
  await page.locator("#dateApply").click();
  await expect(page.locator("#dateError")).toContainText("valid date in MM/DD/YYYY");
  await page.locator("#draftStart").fill("06/19/2026");
  await page.locator("#draftEnd").fill("06/06/2026");
  await page.locator("#dateApply").click();
  await expect(page.locator("#dateError")).toContainText("End date must be on or after start date");
  await expect(page.locator("#draftStart")).toHaveValue("06/19/2026");
  await expect(page.locator("#draftEnd")).toHaveValue("06/06/2026");
  expect(await appliedRange(page)).toEqual({ start: "2026-06-05", end: "2026-06-20" });
  await page.locator('[data-d="2026-06-10"]').click();
  await page.locator("#dateApply").click();
  expect(await appliedRange(page)).toEqual({ start: "2026-06-10", end: "2026-06-10" });
});
