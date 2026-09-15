// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/migrate.js — apply migrations/*.sql in filename order, once each, inside
// a transaction. Tracked in schema_migrations. Idempotent: re-running applies
// only new files. Run by bootstrap/update and `node lib/migrate.js`.

const fs = require("node:fs");
const path = require("node:path");
const { performance } = require("node:perf_hooks");
const { pool } = require("./db");

const MIGRATIONS_DIR = path.join(__dirname, "..", "migrations");
// Two int32 keys name Pages' migration lock independently of schema/search_path.
const LOCK = [0x50414745, 0x4d494752];
const configuredTimeout = Number(process.env.PAGES_MIGRATION_LOCK_TIMEOUT_MS ?? 30000);
const LOCK_TIMEOUT_MS = Number.isFinite(configuredTimeout) && configuredTimeout >= 0 ? configuredTimeout : 30000;

async function migrate({ pool: connectionPool = pool, migrationsDir = MIGRATIONS_DIR, lockTimeoutMs = LOCK_TIMEOUT_MS } = {}) {
  const client = await connectionPool.connect();
  let locked = false;
  let discard = false;
  try {
    const deadline = performance.now() + lockTimeoutMs;
    do {
      locked = (await client.query("SELECT pg_try_advisory_lock($1, $2) AS locked", LOCK)).rows[0].locked;
      if (locked) break;
      const remaining = deadline - performance.now();
      if (remaining <= 0) {
        const error = new Error(`another Pages migration is running; waited ${lockTimeoutMs}ms — retry after it finishes`);
        error.code = "MIGRATION_BUSY";
        throw error;
      }
      await new Promise((resolve) => setTimeout(resolve, Math.min(100, remaining)));
    } while (!locked);
    // The session lock spans tracking-table creation, the initial read and all
    // per-file commits. Acquiring it after reading pending files races too.
    await client.query(`
      CREATE TABLE IF NOT EXISTS schema_migrations (
        filename   TEXT PRIMARY KEY,
        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
      )`);
    const applied = new Set(
      (await client.query("SELECT filename FROM schema_migrations")).rows.map((r) => r.filename)
    );
    const files = fs
      .readdirSync(migrationsDir)
      .filter((f) => f.endsWith(".sql"))
      .sort();

    let count = 0;
    for (const file of files) {
      if (applied.has(file)) continue;
      const sql = fs.readFileSync(path.join(migrationsDir, file), "utf8");
      await client.query("BEGIN");
      try {
        await client.query(sql);
        await client.query("INSERT INTO schema_migrations (filename) VALUES ($1)", [file]);
        await client.query("COMMIT");
        console.log(`migrated: ${file}`);
        count++;
      } catch (err) {
        await client.query("ROLLBACK");
        // Carry DETAIL/HINT through. A migration that asserts a precondition
        // (`RAISE EXCEPTION … USING HINT = …`) puts the remedy there by
        // convention, and err.message alone would drop exactly the sentence the
        // operator needs.
        const extra = [err.detail, err.hint].filter(Boolean).join(" ");
        throw new Error(`migration ${file} failed: ${err.message}${extra ? ` — ${extra}` : ""}`);
      }
    }
    console.log(count ? `applied ${count} migration(s)` : "schema up to date");
  } finally {
    if (locked) {
      try { await client.query("SELECT pg_advisory_unlock($1, $2)", LOCK); }
      catch { discard = true; } // never return a possibly locked session to the pool
    }
    client.release(discard);
  }
}

if (require.main === module) {
  migrate()
    .then(() => pool.end())
    .catch((err) => {
      console.error(err.code === "MIGRATION_BUSY" ? `MIGRATION_BUSY: ${err.message}` : err.message);
      process.exit(1);
    });
}

module.exports = { migrate };
