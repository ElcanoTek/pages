// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const assert = require("node:assert/strict");
const http = require("node:http");
const { once } = require("node:events");
const { spawn } = require("node:child_process");
const { app } = require("../server");
const db = require("../lib/db");
const readiness = require("../lib/readiness");

function get(port, pathname, host = "localhost") {
  return new Promise((resolve, reject) => {
    const req = http.get({ hostname: "127.0.0.1", port, path: pathname, headers: { Host: host } }, (res) => {
      let body = "";
      res.on("data", (chunk) => { body += chunk; });
      res.on("end", () => resolve({ status: res.statusCode, body, headers: res.headers }));
    });
    req.on("error", reject);
    req.setTimeout(5000, () => req.destroy(new Error("readiness did not finish promptly")));
  });
}

(async () => {
  const server = app.listen(0, "127.0.0.1");
  let offline;
  try {
    await once(server, "listening");
    const port = server.address().port;
    for (const host of ["localhost", "content.localhost"]) {
      const ready = await get(port, "/readyz", host);
      assert.equal(ready.status, 200, ready.body);
      assert.deepEqual(JSON.parse(ready.body), { status: "ready" });
      assert.equal(ready.headers["cache-control"], "no-store");
      if (host === "content.localhost") assert.ok(ready.headers["content-security-policy"]);
    }
    const migration = (await db.query("DELETE FROM schema_migrations WHERE filename=(SELECT filename FROM schema_migrations ORDER BY filename DESC LIMIT 1) RETURNING *")).rows[0];
    try {
      assert.equal((await get(port, "/healthz")).status, 200);
      assert.equal((await get(port, "/readyz")).status, 503, "a pending migration prevents readiness");
    } finally { await db.query("INSERT INTO schema_migrations(filename,applied_at) VALUES($1,$2)", [migration.filename, migration.applied_at]); }
    await db.query("ALTER TABLE schema_migrations RENAME TO readiness_migrations_backup");
    try { assert.equal((await get(port, "/readyz")).status, 503, "absent schema prevents readiness"); }
    finally { await db.query("ALTER TABLE readiness_migrations_backup RENAME TO schema_migrations"); }
    assert.equal((await get(port, "/readyz")).status, 200, "readiness recovers after schema repair");

    const env = { ...process.env, DATABASE_URL: "postgres://pages@127.0.0.1:1/pages" };
    delete env.NODE_TEST_CONTEXT;
    offline = spawn(process.execPath, ["-e", "const s=require('./server').app.listen(0,'127.0.0.1',()=>process.send(s.address().port));"], {
      cwd: require("node:path").join(__dirname, ".."), env, stdio: ["ignore", "ignore", "ignore", "ipc"],
    });
    const [offlinePort] = await once(offline, "message");
    const start = Date.now();
    const live = await get(offlinePort, "/healthz");
    const failed = await get(offlinePort, "/readyz");
    assert.equal(live.status, 200);
    assert.equal(failed.status, 503);
    assert.deepEqual(JSON.parse(failed.body), { status: "not_ready" });
    assert.ok(Date.now() - start < 3500, "unavailable database fails promptly");
    console.log("✓ liveness stays available while bounded readiness checks connectivity and complete schema on both hosts");
  } catch (error) { console.error(error); process.exitCode = 1; }
  finally {
    if (offline) { offline.kill("SIGTERM"); await once(offline, "exit"); }
    await new Promise((resolve) => server.close(resolve));
    await Promise.all([db.pool.end(), readiness.close()]);
  }
})();
