// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const fs = require("node:fs");
const path = require("node:path");

function checkCompatibility(applied, available, policy) {
  const done = new Set(applied);
  if (!done.has(policy.minimum_previous_schema)) throw new Error(`automatic update requires schema ${policy.minimum_previous_schema}; use the documented manual migration/restore procedure for older installations`);
  const unknownApplied = applied.filter((name) => !available.includes(name));
  if (unknownApplied.length) throw new Error(`database has migrations absent from this release: ${unknownApplied.join(", ")}; downgrade requires a reviewed manual restore`);
  const unclassified = available.filter((name) => !done.has(name) && policy.migrations[name] !== "backward-compatible");
  if (unclassified.length) throw new Error(`pending migrations require a reviewed manual upgrade: ${unclassified.join(", ")}; automatic code rollback is not declared compatible`);
}

if (require.main === module) {
  const db = require("../lib/db");
  (async () => {
    const directory = path.join(__dirname, "..", "migrations");
    const policy = JSON.parse(fs.readFileSync(path.join(directory, "compatibility.json"), "utf8"));
    const applied = (await db.query("SELECT filename FROM schema_migrations")).rows.map((row) => row.filename);
    checkCompatibility(applied, fs.readdirSync(directory).filter((name) => name.endsWith(".sql")), policy);
  })().catch((error) => { console.error(error.message); process.exitCode = 1; }).finally(() => db.pool.end());
}
module.exports = { checkCompatibility };
