// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const { assertSupported } = require("../scripts/check-node");

test("runtime: supported LTS releases and the exact minimum load Pages", () => {
  for (const version of ["22.13.0", "22.22.2", "24.0.0", "24.8.1"]) assert.doesNotThrow(() => assertSupported(version));
  const result = spawnSync(process.execPath, ["-e", "require('./server'); require('./lib/db').pool.end()"], {
    cwd: require("node:path").join(__dirname, ".."), encoding: "utf8", timeout: 15000,
  });
  assert.equal(result.status, 0, result.stderr);
});

test("runtime: unsupported versions fail before application dependencies load", () => {
  for (const version of ["18.20.8", "20.20.0", "22.12.9", "23.1.0", "25.0.0", "22.13.0-rc.1"]) {
    assert.throws(() => assertSupported(version), /Pages requires Node.js 22.13.*Upgrade Node/);
  }
});
