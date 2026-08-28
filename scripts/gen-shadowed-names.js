// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Regenerates lib/preflight-shadowed-names.js from a real browser engine, so the
// list tracks whatever Chromium actually exposes rather than a hand-kept guess.
//
//   node scripts/gen-shadowed-names.js
//
// Needs the Playwright browsers that `npm run test:browser` already installs.

const fs = require("node:fs");
const path = require("node:path");
const { chromium } = require("@playwright/test");

const OUT = path.join(__dirname, "..", "lib", "preflight-shadowed-names.js");

const HEADER = `"use strict";
// lib/preflight-shadowed-names.js — GENERATED DATA. Do not hand-edit.
//
// Every property reachable on the objects an inline \`on*=\` handler puts on its
// scope chain: the element -> its form owner -> the document -> window. A bare
// identifier in such a handler resolves against those FIRST, so a page that
// defines \`function togglePopover()\` and writes \`onclick="togglePopover()"\`
// calls HTMLElement.prototype.togglePopover instead and throws NotSupportedError.
// That exact collision silently killed the date-range picker on a live dashboard.
//
// Regenerate with: node scripts/gen-shadowed-names.js
module.exports = new Set([
`;

async function main() {
  const browser = await chromium.launch();
  const page = await browser.newPage();
  const names = await page.evaluate(() => {
    const set = new Set();
    const protos = [
      EventTarget.prototype, Node.prototype, Element.prototype, HTMLElement.prototype,
      Document.prototype, HTMLDocument.prototype, HTMLFormElement.prototype,
    ];
    const tags = ["a", "button", "input", "select", "textarea", "form", "div", "span", "img",
                  "table", "td", "details", "summary", "label", "option", "canvas", "svg"];
    for (const tag of tags) {
      const el = document.createElement(tag);
      for (let o = Object.getPrototypeOf(el); o && o !== Object.prototype; o = Object.getPrototypeOf(o)) {
        protos.push(o);
      }
    }
    for (const proto of protos) for (const k of Object.getOwnPropertyNames(proto)) set.add(k);
    // `on*` names are the browser's own handler slots, not plausible author globals.
    return [...set]
      .filter((n) => /^[A-Za-z_$][A-Za-z0-9_$]*$/.test(n) && !/^on[a-z]/.test(n) && n !== "constructor")
      .sort();
  });
  await browser.close();

  const lines = [];
  let cur = "  ";
  for (const n of names) {
    const tok = `${JSON.stringify(n)}, `;
    if (cur.length + tok.length > 98) { lines.push(cur.trimEnd()); cur = "  "; }
    cur += tok;
  }
  if (cur.trim()) lines.push(cur.trimEnd().replace(/,$/, ""));

  fs.writeFileSync(OUT, `${HEADER}${lines.join("\n")}\n]);\n`);
  process.stdout.write(`wrote ${names.length} names to ${path.relative(process.cwd(), OUT)}\n`);
}

main().catch((err) => {
  process.stderr.write(`${err && err.stack ? err.stack : err}\n`);
  process.exit(1);
});
