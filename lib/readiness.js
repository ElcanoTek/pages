// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const fs = require("node:fs");
const path = require("node:path");
const { Pool } = require("pg");
const { connectionConfig } = require("./db");

const migrations = fs.readdirSync(path.join(__dirname, "..", "migrations")).filter((name) => name.endsWith(".sql")).sort();
// Keep the probe out of the application's pool so exhausted application
// connections cannot queue unbounded health checks. One probe is shared by
// simultaneous callers and the idle connection is closed promptly.
const pool = new Pool({ ...connectionConfig, max: 1, connectionTimeoutMillis: 1000, idleTimeoutMillis: 1000,
  options: `${connectionConfig.options} -c statement_timeout=1000 -c lock_timeout=1000`,
});
pool.on("error", () => {}); // readiness replies remain generic
let pending;

async function probe() {
  let client;
  try {
    client = await pool.connect();
    // Parsing this zero-row read checks the current rendering tables/columns
    // and read permissions without scanning page data.
    await client.query("SELECT p.slug, p.published_version_id, v.html, t.default_mode FROM pages p LEFT JOIN page_versions v ON v.id=p.published_version_id LEFT JOIN themes t ON t.id=p.theme_id WHERE false");
    const { rows } = await client.query("SELECT filename FROM schema_migrations WHERE filename=ANY($1::text[])", [migrations]);
    return rows.length === migrations.length;
  } catch { return false; }
  finally { if (client) client.release(); }
}

function check() {
  if (!pending) pending = probe().finally(() => { pending = null; });
  return pending;
}

async function handler(_req, res) {
  const ready = await check();
  res.set("Cache-Control", "no-store").status(ready ? 200 : 503).json({ status: ready ? "ready" : "not_ready" });
}

module.exports = { check, handler, close: () => pool.end() };
