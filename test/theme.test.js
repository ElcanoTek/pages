// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// The admin shell is meant to be entirely token-driven: every colour, space,
// radius, shadow and motion value resolves to a vendored Flag design token, or to
// one of a small set of deliberately declared Pages-local custom properties. That
// is what makes the light and dark themes work without a second stylesheet, and
// what stops a screen from drifting into its own palette.
//
// Nothing enforced it. These checks do, because the two ways this decays are both
// silent: a literal colour looks right in whichever theme its author had open, and
// a mistyped token name resolves to nothing at all — `var(--color-text-primry)` is
// not an error, it just renders unstyled.

const test = require("node:test");
const assert = require("node:assert");
const fs = require("node:fs");
const path = require("node:path");

const ROOT = path.join(__dirname, "..");
const SHELL_CSS = path.join(ROOT, "public/shell-assets/shell.css");
const TOKENS_CSS = path.join(ROOT, "public/assets/flag/tokens/design-tokens.css");
const BROWSER_MODULES = [
  "admin.js",
  "welcome.js",
  "portals.js",
  "templates.js",
  "primitives.js",
  "page-switcher.js",
];

const COLOUR_LITERAL = /#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(/g;

function read(file) {
  return fs.readFileSync(file, "utf8");
}

// Line numbers make a failure actionable, so report matches with their location
// rather than just a count.
function findLiterals(source) {
  return source.split("\n").flatMap((line, index) => {
    const matches = line.match(COLOUR_LITERAL);
    return matches ? [{ line: index + 1, text: line.trim().slice(0, 120), matches }] : [];
  });
}

// A hex or rgb() inside a comment is prose (a PR reference like "#125", an
// explanation of a token's value), not a hard-coded colour.
function stripComments(source) {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .split("\n")
    .map((line) => line.replace(/(^|[^:"'`\\])\/\/.*$/, "$1"))
    .join("\n");
}

function declaredCustomProperties(source) {
  return new Set([...source.matchAll(/(--[a-z0-9-]+)\s*:/g)].map((match) => match[1]));
}

function referencedCustomProperties(source) {
  return new Set([...source.matchAll(/var\(\s*(--[a-z0-9-]+)/g)].map((match) => match[1]));
}

test("theme: the admin stylesheet hard-codes no colour", () => {
  const found = findLiterals(stripComments(read(SHELL_CSS)));
  assert.deepStrictEqual(
    found,
    [],
    `shell.css must express colour through Flag tokens, not literals:\n${JSON.stringify(found, null, 2)}`
  );
});

test("theme: every custom property the admin stylesheet reads is actually defined", () => {
  const shell = read(SHELL_CSS);
  const tokens = declaredCustomProperties(read(TOKENS_CSS));
  const locals = declaredCustomProperties(shell);
  const unresolved = [...referencedCustomProperties(shell)]
    .filter((name) => !tokens.has(name) && !locals.has(name))
    .sort();
  assert.deepStrictEqual(
    unresolved,
    [],
    `these var() references resolve to nothing — a mistyped token renders unstyled rather than failing:\n${unresolved.join("\n")}`
  );
});

test("theme: Pages-local custom properties stay few and namespaced", () => {
  const locals = [...declaredCustomProperties(read(SHELL_CSS))].sort();
  const misnamed = locals.filter((name) => !name.startsWith("--pages-"));
  assert.deepStrictEqual(
    misnamed,
    [],
    `a value Pages defines for itself must say so in its name, so it is never mistaken for a Flag token:\n${misnamed.join("\n")}`
  );
  // Not a style rule — a pressure valve. Each of these is a value Flag does not
  // provide; if the list grows, the fix belongs upstream in the tokens, not here.
  assert.ok(
    locals.length <= 8,
    `${locals.length} Pages-local properties (${locals.join(", ")}). Past a handful, these belong in the Flag tokens.`
  );
});

test("theme: the admin's browser modules set no colour of their own", () => {
  const offenders = BROWSER_MODULES.flatMap((name) => {
    const found = findLiterals(stripComments(read(path.join(ROOT, "public/shell-assets", name))));
    return found.map((entry) => ({ file: name, ...entry }));
  });
  assert.deepStrictEqual(
    offenders,
    [],
    `colour belongs in shell.css against a token, never inline in a module:\n${JSON.stringify(offenders, null, 2)}`
  );
});

test("theme: both themes are reachable, so no rule may assume one of them", () => {
  const tokens = read(TOKENS_CSS);
  // The Flag tokens flip on [data-theme="light"] / body.theme-light. A rule in
  // shell.css that hard-codes one theme's value would survive the flip; the
  // literal check above is what prevents that, and this asserts the flip exists
  // at all, so that check keeps meaning something.
  assert.match(tokens, /\[data-theme="light"\]|\.theme-light/, "the Flag tokens no longer define a light theme");
  assert.match(tokens, /:root/, "the Flag tokens no longer define a root palette");
});
