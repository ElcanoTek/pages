// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// The dashboard host's security headers. lib/csp.js used to hand these options
// to helmet 8.3.0; it now writes the same set itself. The expectations below
// were recorded from helmet's middleware for Pages' options, so any drift in a
// directive, its order, or a companion header fails here.

const test = require("node:test");
const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const path = require("node:path");

const AUTH = "https://auth.northwind.example";

// lib/csp.js reads DASHBOARD_ORIGIN/CONTENT_ORIGIN at load, so each shape runs
// in its own process.
function headersFor(env) {
  const script = `
    const { shellHeadersMiddleware } = require("./lib/csp");
    const set = {}; const removed = [];
    const res = { setHeader: (k, v) => { set[k] = v; }, removeHeader: (k) => removed.push(k) };
    shellHeadersMiddleware(${JSON.stringify(AUTH)})({}, res, () => {
      process.stdout.write(JSON.stringify({ set, removed }));
    });`;
  const run = spawnSync(process.execPath, ["-e", script], {
    cwd: path.join(__dirname, ".."),
    env: { PATH: process.env.PATH, ...env },
    encoding: "utf8",
  });
  assert.equal(run.status, 0, run.stderr);
  return JSON.parse(run.stdout);
}

const COMMON = {
  "Cross-Origin-Opener-Policy": "same-origin",
  "Cross-Origin-Resource-Policy": "same-origin",
  "Origin-Agent-Cluster": "?1",
  "Referrer-Policy": "no-referrer",
  "X-Content-Type-Options": "nosniff",
  "X-DNS-Prefetch-Control": "off",
  "X-Download-Options": "noopen",
  "X-Frame-Options": "SAMEORIGIN",
  "X-Permitted-Cross-Domain-Policies": "none",
  "X-XSS-Protection": "0",
};

test("shell headers: https dashboard matches helmet 8.3.0 exactly (HSTS, upgrade-insecure-requests)", () => {
  const { set, removed } = headersFor({
    DASHBOARD_ORIGIN: "https://pages.northwind.example",
    CONTENT_ORIGIN: "https://northwind-pages.example",
  });
  assert.deepEqual(set, {
    "Content-Security-Policy":
      "default-src 'self';base-uri 'self';font-src 'self';form-action 'self' https://auth.northwind.example;" +
      "frame-ancestors 'none';img-src 'self' data:;object-src 'none';script-src 'self';script-src-attr 'none';" +
      "style-src 'self' 'unsafe-inline';upgrade-insecure-requests;frame-src https://northwind-pages.example;" +
      "connect-src 'self'",
    ...COMMON,
    "Strict-Transport-Security": "max-age=31536000; includeSubDomains",
  });
  assert.deepEqual(removed, ["X-Powered-By"]);
});

test("shell headers: plain-http dev drops HSTS and upgrade-insecure-requests, as before", () => {
  const { set, removed } = headersFor({
    DASHBOARD_ORIGIN: "http://localhost:3099",
    CONTENT_ORIGIN: "http://content.localhost:3099",
  });
  assert.deepEqual(set, {
    "Content-Security-Policy":
      "default-src 'self';base-uri 'self';font-src 'self';form-action 'self' https://auth.northwind.example;" +
      "frame-ancestors 'none';img-src 'self' data:;object-src 'none';script-src 'self';script-src-attr 'none';" +
      "style-src 'self' 'unsafe-inline';frame-src http://content.localhost:3099;connect-src 'self'",
    ...COMMON,
  });
  assert.deepEqual(removed, ["X-Powered-By"]);
});

test("shell headers: a directive value carrying ';' or ',' is refused, as helmet refused it", () => {
  const run = spawnSync(
    process.execPath,
    ["-e", `require("./lib/csp").shellHeaders("https://a.example; script-src *")`],
    { cwd: path.join(__dirname, ".."), env: { PATH: process.env.PATH }, encoding: "utf8" }
  );
  assert.notEqual(run.status, 0);
  assert.match(run.stderr, /invalid directive value for "form-action"/);
  const comma = spawnSync(
    process.execPath,
    ["-e", `require("./lib/csp")`],
    { cwd: path.join(__dirname, ".."), env: { PATH: process.env.PATH, CONTENT_ORIGIN: "https://a.example,https://b.example" }, encoding: "utf8" }
  );
  assert.equal(comma.status, 0, "loading the module alone builds no header");
  const built = spawnSync(
    process.execPath,
    ["-e", `require("./lib/csp").shellHeaders("https://auth.example")`],
    { cwd: path.join(__dirname, ".."), env: { PATH: process.env.PATH, CONTENT_ORIGIN: "https://a.example,https://b.example" }, encoding: "utf8" }
  );
  assert.match(built.stderr, /invalid directive value for "frame-src"/);
});
