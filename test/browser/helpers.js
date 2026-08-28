// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { expect } = require("@playwright/test");
const AxeBuilder = require("@axe-core/playwright").default;

async function resetFixture(request) {
  const response = await request.post("/__fixture/reset");
  expect(response.ok()).toBeTruthy();
}

async function expectNoHorizontalOverflow(page) {
  const dimensions = await page.evaluate(() => ({
    viewport: document.documentElement.clientWidth,
    document: document.documentElement.scrollWidth,
    body: document.body.scrollWidth,
  }));
  expect(dimensions.document, JSON.stringify(dimensions)).toBeLessThanOrEqual(dimensions.viewport + 1);
  expect(dimensions.body, JSON.stringify(dimensions)).toBeLessThanOrEqual(dimensions.viewport + 1);
}

// The preview stages embed a CLIENT's document in a sandboxed iframe, and the
// fixture serves it same-origin, so axe descends into it and counts its headings
// and landmarks as if they were ours. That is how the template detail reported a
// heading-order failure on "Pages built from this": the jump it saw was our <h3>
// following the client page's own <h1>. The admin is not responsible for the
// outline of a page it is only displaying, and in production the frame is a
// different origin anyway, so axe would never have seen it.
const PREVIEW_FRAMES = ["#preview-frame", "#tpl-preview-frame"];

function auditor(page) {
  let builder = new AxeBuilder({ page });
  for (const frame of PREVIEW_FRAMES) builder = builder.exclude(frame);
  return builder;
}

async function expectNoSeriousAxeViolations(page, context, options = {}) {
  const result = await auditor(page).analyze();
  const impacts = options.level === "moderate"
    ? ["critical", "serious", "moderate"]
    : ["critical", "serious"];
  const violations = result.violations.filter((violation) => impacts.includes(violation.impact));
  const summary = violations.map((violation) => ({
    id: violation.id,
    impact: violation.impact,
    help: violation.help,
    targets: violation.nodes.map((node) => node.target),
  }));
  expect(violations, `${context}: ${JSON.stringify(summary, null, 2)}`).toEqual([]);
}

// Below 75rem shell.css collapses every .operation-table into cards and renders
// each cell's column name from `td::before { content: attr(data-label) }`. A table
// that omits data-label therefore collapses into a stack of unlabelled values —
// "Northwind Media Group / 2 / 7/22/2026" with nothing saying what 2 counts. This
// asserts it structurally, for every table on the screen, so the next table added
// to the admin cannot ship without labels.
async function expectCollapsedTablesAreLabelled(page, context) {
  const tables = await page.evaluate(() => [...document.querySelectorAll(".operation-table")].map((table, index) => {
    const headers = [...table.querySelectorAll("thead th")].map((th) => th.textContent.trim());
    const unlabelled = [];
    table.querySelectorAll("tbody tr").forEach((row, rowIndex) => {
      [...row.children].forEach((cell, cellIndex) => {
        const label = cell.getAttribute("data-label");
        if (!label || !label.trim()) {
          unlabelled.push({ rowIndex, cellIndex, expected: headers[cellIndex] || "(no header)" });
        }
      });
    });
    return { index, className: table.className, headers, unlabelled, rendered: getComputedStyle(table).display };
  }));
  expect(tables.length, `${context}: expected at least one .operation-table`).toBeGreaterThan(0);
  for (const table of tables) {
    expect(table.unlabelled, `${context}: ${table.className} has cells with no data-label: ${JSON.stringify(table.unlabelled)}`).toEqual([]);
  }
  // A caption must not be left as a table-caption inside the collapsed blocks, or
  // it shrinks to min-content and wraps one word per line.
  const collapsed = tables.some((table) => table.rendered === "block");
  if (!collapsed) return;
  const captions = await page.evaluate(() => [...document.querySelectorAll(".operation-table caption")].map((caption) => ({
    text: caption.textContent.trim().slice(0, 40),
    display: getComputedStyle(caption).display,
    width: caption.getBoundingClientRect().width,
    parentWidth: caption.closest("table").getBoundingClientRect().width,
    srOnly: caption.classList.contains("sr-only"),
  })));
  for (const caption of captions) {
    if (caption.srOnly) continue;
    expect(caption.display, `${context}: caption "${caption.text}" is still ${caption.display}`).toBe("block");
    expect(caption.width, `${context}: caption "${caption.text}" is only ${Math.round(caption.width)}px of ${Math.round(caption.parentWidth)}px`)
      .toBeGreaterThan(caption.parentWidth * 0.5);
  }
}

module.exports = {
  resetFixture,
  expectNoHorizontalOverflow,
  expectNoSeriousAxeViolations,
  expectCollapsedTablesAreLabelled,
};
