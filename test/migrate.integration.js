// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");
const { Pool } = require("pg");
const db = require("../lib/db");
const { migrate } = require("../lib/migrate");

(async () => {
  const schema = `northwind_migrations_${process.pid}`;
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "pages-migrations-"));
  const pools = [];
  let gate;
  try {
    await db.query(`CREATE SCHEMA ${schema}`);
    for (let i = 0; i < 2; i++) pools.push(new Pool({
      ...(process.env.DATABASE_URL ? { connectionString: process.env.DATABASE_URL } : {}),
      options: `-c search_path=${schema} -c statement_timeout=5000`, max: 1,
    }));
    gate = await db.pool.connect();
    const gateKey = 490000 + process.pid;
    await gate.query("SELECT pg_advisory_lock($1)", [gateKey]);
    await fs.writeFile(path.join(dir, "001_once.sql"), `SELECT pg_advisory_xact_lock(${gateKey}); CREATE TABLE applied_once (id int); INSERT INTO applied_once VALUES (1);`);
    const firstPid = (await pools[0].query("SELECT pg_backend_pid() AS pid")).rows[0].pid;
    const first = migrate({ pool: pools[0], migrationsDir: dir });
    // Wait for the actual first migration to block on our gate, not a timing
    // guess about which concurrent process reached DDL first.
    let waiting = false;
    for (let i = 0; i < 100; i++) {
      const { rows } = await gate.query("SELECT 1 FROM pg_locks WHERE pid=$1 AND locktype='advisory' AND NOT granted", [firstPid]);
      if (rows.length) { waiting = true; break; }
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
    assert.ok(waiting, "first runner reached the pending migration");
    await assert.rejects(migrate({ pool: pools[1], migrationsDir: dir, lockTimeoutMs: 50 }),
      (error) => error.code === "MIGRATION_BUSY" && /another Pages migration/.test(error.message));
    const second = migrate({ pool: pools[1], migrationsDir: dir });
    await gate.query("SELECT pg_advisory_unlock($1)", [gateKey]);
    await Promise.all([first, second]);
    assert.equal((await pools[0].query("SELECT count(*) FROM applied_once")).rows[0].count, "1");
    assert.equal((await pools[0].query("SELECT count(*) FROM schema_migrations")).rows[0].count, "1");
    console.log("✓ simultaneous migrators serialize; a bounded contender reports MIGRATION_BUSY");

    await fs.writeFile(path.join(dir, "002_retry.sql"), "CREATE TABLE retry_probe (id int); SELECT no_such_function();");
    await assert.rejects(migrate({ pool: pools[0], migrationsDir: dir }), /002_retry.sql failed/);
    assert.equal((await pools[0].query("SELECT to_regclass('retry_probe') AS name")).rows[0].name, null);
    // This temporary test migration never committed. Its repaired retry proves
    // rollback and lock release; real applied migration files remain untouched.
    await fs.writeFile(path.join(dir, "002_retry.sql"), "CREATE TABLE retry_probe (id int);");
    await migrate({ pool: pools[1], migrationsDir: dir, lockTimeoutMs: 50 });
    assert.equal((await pools[1].query("SELECT count(*) FROM schema_migrations")).rows[0].count, "2");
    console.log("✓ a failed migration rolls back and releases the lock for another runner");
  } catch (error) {
    console.error(error);
    process.exitCode = 1;
  } finally {
    if (gate) { await gate.query("SELECT pg_advisory_unlock_all()"); gate.release(); }
    await Promise.all(pools.map((pool) => pool.end()));
    await db.query(`DROP SCHEMA IF EXISTS ${schema} CASCADE`);
    await db.pool.end();
    await fs.rm(dir, { recursive: true, force: true });
  }
})();
