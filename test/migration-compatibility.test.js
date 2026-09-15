// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { checkCompatibility } = require("../scripts/check-migration-compatibility");

test("automatic updates require an established predecessor and reviewed pending migrations", () => {
  const policy = { minimum_previous_schema: "001_base.sql", migrations: { "002_added.sql": "backward-compatible", "003_renamed.sql": "manual" } };
  assert.doesNotThrow(() => checkCompatibility(["001_base.sql"], ["001_base.sql", "002_added.sql"], policy));
  assert.throws(() => checkCompatibility([], ["001_base.sql"], policy), /older installations/);
  assert.throws(() => checkCompatibility(["001_base.sql"], ["001_base.sql", "002_added.sql", "003_renamed.sql"], policy), /manual upgrade.*003_renamed/);
  assert.throws(() => checkCompatibility(["001_base.sql"], ["001_base.sql", "004_unknown.sql"], policy), /manual upgrade.*004_unknown/);
  assert.throws(() => checkCompatibility(["001_base.sql", "005_future.sql"], ["001_base.sql"], policy), /downgrade requires a reviewed manual restore/);
  assert.doesNotThrow(() => checkCompatibility(["001_base.sql", "003_renamed.sql"], ["001_base.sql", "003_renamed.sql"], policy));
});
