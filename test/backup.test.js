// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const backup = require("../scripts/backup");
const ROOT = path.join(__dirname, "..");

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pages-backup-unit-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const write = (relative, content, mode) => {
    const file = path.join(dir, relative);
    fs.mkdirSync(path.dirname(file), { recursive: true });
    fs.writeFileSync(file, content, mode ? { mode } : undefined);
    return file;
  };
  write("database.dump", "Northwind database fixture");
  write("application/server.js", "module.exports = {};\n");
  write("application/package-lock.json", fs.readFileSync(path.join(ROOT, "package-lock.json")));
  write("config/service.env", "NORTHWIND_FIXTURE=retained\n");
  write("assets/northwind.txt", "Northwind persistent bytes\n");
  return { dir, write };
}

function environment(t, values) {
  const previous = Object.fromEntries(Object.keys(values).map((key) => [key, process.env[key]]));
  Object.assign(process.env, values);
  t.after(() => {
    for (const [key, value] of Object.entries(previous)) {
      if (value === undefined) delete process.env[key]; else process.env[key] = value;
    }
  });
}

test("backup inventory checks actual artifact bytes and rejects extra or changed files", async (t) => {
  const { dir, write } = fixture(t);
  const manifest = { format_version: 1, status: "complete", files: await backup.inventory(dir) };
  write("manifest.json", JSON.stringify(manifest));
  assert.deepEqual(await backup.verifyFiles(dir, manifest), manifest.files);
  write("assets/northwind.txt", "Northwind changed bytes\n");
  await assert.rejects(backup.verifyFiles(dir, manifest), /checksum mismatch/);
  write("assets/northwind.txt", "Northwind persistent bytes\n");
  write("assets/contoso.txt", "unexpected additional artifact");
  await assert.rejects(backup.verifyFiles(dir, manifest), /checksum mismatch/);
});

test("backup verification rejects incomplete manifests and missing recovery inputs", async (t) => {
  const { dir } = fixture(t);
  const files = await backup.inventory(dir);
  for (const manifest of [{}, { format_version: 2, status: "complete", files },
    { format_version: 1, status: "partial", files }, { format_version: 1, status: "complete" }]) {
    await assert.rejects(backup.verifyFiles(dir, manifest), /unsupported or incomplete/);
  }
  fs.unlinkSync(path.join(dir, "config/service.env"));
  await assert.rejects(backup.verifyFiles(dir, { format_version: 1, status: "complete", files: await backup.inventory(dir) }),
    /missing required recovery input: config\/service.env/);
});

test("backup verification rejects links instead of following uncaptured files", async (t) => {
  const { dir } = fixture(t);
  fs.symlinkSync(path.join(dir, "application/server.js"), path.join(dir, "assets/linked.js"));
  await assert.rejects(backup.inventory(dir), /unsupported link or special file/);
});

test("restore rehearsal replaces database targets and credentials while preserving explicit data limits", (t) => {
  environment(t, { DATABASE_URL: "postgres://northwind:fixture@192.0.2.1/live", PGHOST: "/northwind-live",
    PGPORT: "5497", PGDATABASE: "northwind_live", PGUSER: "northwind_live", PGPASSWORD: "northwind-fixture",
    PGSERVICE: "northwind-service", PGSERVICEFILE: "/northwind-service.conf", PGPASSFILE: "/northwind-pass",
    PGOPTIONS: "-c default_transaction_read_only=off", PAGES_DATA_MAX_BYTES: "999999",
    MAX_HTML_BYTES: "999mb", RAW_TOKEN_SECRET: "original-northwind-fixture", API_TOKEN_PEPPER: "original-northwind-fixture",
    PAGE_COOKIE_SECRET: "original-northwind-fixture", PAGES_DEV_LOGIN: "1" });
  const env = backup.isolatedEnv("/tmp/northwind-private", { PAGES_DATA_MAX_BYTES: "2048", MAX_HTML_BYTES: "3mb",
    DATABASE_URL: "postgres://ignored.invalid/live", PGHOST: "/ignored-live", PAGE_COOKIE_SECRET: "ignored-secret" });
  assert.deepEqual(Object.fromEntries(Object.entries(env).filter(([key]) => key.startsWith("PG"))), {
    PGHOST: "/tmp/northwind-private", PGPORT: "5432", PGUSER: "pages_backup", PGDATABASE: "pages",
  });
  assert.equal(env.DATABASE_URL, undefined);
  assert.equal(env.PAGES_DATA_MAX_BYTES, "2048");
  assert.equal(env.MAX_HTML_BYTES, "3mb");
  assert.equal(env.PAGES_DEV_LOGIN, "0");
  for (const key of ["RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "PAGE_COOKIE_SECRET"]) {
    assert.ok(env[key]);
    assert.notEqual(env[key], process.env[key]);
    assert.notEqual(env[key], "ignored-secret");
  }
});

for (const retainsRunningCluster of [false, true]) {
  test(`failed PostgreSQL start ${retainsRunningCluster ? "retains storage when shutdown cannot be confirmed" : "stops the owned cluster before removing storage"}`, async (t) => {
    const { dir, write } = fixture(t);
    const bin = path.join(dir, "bin"), eventsFile = path.join(dir, "events.jsonl");
    const executable = (name, code) => write(`bin/${name}`, `#!${process.execPath}\n${code}\n`, 0o755);
    const record = `const fs = require('node:fs'); const path = require('node:path'); const args = process.argv.slice(2);
      const data = args[args.indexOf('-D') + 1];
      fs.appendFileSync(process.env.BACKUP_UNIT_EVENTS, JSON.stringify({command:path.basename(process.argv[1]),args,data,
        pg:{host:process.env.PGHOST,port:process.env.PGPORT,database:process.env.PGDATABASE},
        url:process.env.DATABASE_URL}) + '\\n');`;
    executable("initdb", `${record}\nfs.mkdirSync(data, {recursive:true});`);
    executable("pg_ctl", `${record}\nconst verb = args.at(-1);
      if(verb === 'start') { fs.writeFileSync(path.join(data,'fixture-running'), 'owned fixture cluster'); process.exit(19); }
      if(verb === 'stop') { if(process.env.BACKUP_UNIT_RETAIN === '1') process.exit(20); fs.unlinkSync(path.join(data,'fixture-running')); }
      if(verb === 'status') process.exit(fs.existsSync(path.join(data,'fixture-running')) ? 0 : 3);`);
    executable("pg_restore", "process.exit(99);");
    executable("chown", "process.exit(0);");
    // Simulate root delegation without requiring a postgres system account.
    executable("runuser", `const {spawnSync}=require('node:child_process'); const args=process.argv.slice(2);
      const command=args.slice(args.indexOf('--')+1); const result=spawnSync(command[0],command.slice(1),{stdio:'inherit',env:process.env});
      process.exit(result.status === null ? 98 : result.status);`);
    environment(t, { PATH: `${bin}:${process.env.PATH}`, BACKUP_UNIT_EVENTS: eventsFile,
      BACKUP_UNIT_RETAIN: retainsRunningCluster ? "1" : "0", PGHOST: "/northwind-live", PGPORT: "5497",
      PGDATABASE: "northwind_live", DATABASE_URL: "postgres://ignored.invalid/live" });
    let scratch;
    try {
      await assert.rejects(backup.rehearse(dir), retainsRunningCluster ? /cannot confirm scratch PostgreSQL stopped; retained/ : /failed \(19\)/);
      const events = fs.readFileSync(eventsFile, "utf8").trim().split("\n").map((line) => JSON.parse(line));
      const data = events[0].data;
      scratch = path.dirname(data);
      assert.deepEqual(events.map((event) => event.command === "pg_ctl" ? event.args.at(-1) : event.command),
        retainsRunningCluster ? ["initdb", "start", "stop", "status"] : ["initdb", "start", "stop"]);
      for (const event of events) {
        assert.equal(event.data, data, "only the owned scratch cluster is controlled");
        assert.deepEqual(event.pg, { host: path.join(scratch, "socket"), port: "5432", database: "pages" });
        assert.equal(event.url, undefined, "no scratch process inherits the source connection URL");
      }
      assert.equal(fs.existsSync(scratch), retainsRunningCluster);
      if (retainsRunningCluster) assert.equal(fs.readFileSync(path.join(data, "fixture-running"), "utf8"), "owned fixture cluster");
    } finally {
      if (scratch) fs.rmSync(scratch, { recursive: true, force: true }); // fake server only
    }
  });
}

// The service env holds DATABASE_URL, which libpq never reads from PGDATABASE,
// so capture parses it with the application's own parser and hands pg_dump the
// parts as PG* variables, never argv.
test("backup capture connects exactly where the application's DATABASE_URL points", () => {
  const pg = (url, extra = {}) => Object.fromEntries(Object.entries(backup.dumpEnvironment({ DATABASE_URL: url, ...extra }))
    .filter(([key]) => key.startsWith("PG")));
  // The shape bootstrap writes.
  assert.deepEqual(pg("postgres://pages:0f3a@127.0.0.1:5432/pages?sslmode=disable"),
    { PGHOST: "127.0.0.1", PGPORT: "5432", PGUSER: "pages", PGPASSWORD: "0f3a", PGDATABASE: "pages", PGSSLMODE: "disable" });
  // The URL is the only source: inherited libpq overrides cannot redirect it.
  assert.deepEqual(pg("postgres://pages:p@db.test/pages", { PGHOSTADDR: "192.0.2.9", PGSERVICE: "northwind", PGPASSFILE: "/x" }),
    { PGHOST: "db.test", PGUSER: "pages", PGPASSWORD: "p", PGDATABASE: "pages", PGSSLMODE: "disable" });
  assert.equal(backup.dumpEnvironment({ DATABASE_URL: "postgres://p:q@h/d", KEEP: "kept" }).KEEP, "kept");
  assert.equal(backup.dumpEnvironment({ DATABASE_URL: "postgres://p:q@h/d" }).DATABASE_URL, undefined);
  // Decoding and precedence follow the driver: encoded socket host, hostless
  // ?host=, query values over userinfo, bare IPv6, its localhost default.
  assert.deepEqual(pg("postgresql://pages:fix%40ture@%2Ftmp%2Fpg-sock/north%20wind"),
    { PGHOST: "/tmp/pg-sock", PGUSER: "pages", PGPASSWORD: "fix@ture", PGDATABASE: "north wind", PGSSLMODE: "disable" });
  assert.equal(pg("postgres://pages:p@/pages?host=%2Ftmp%2Fpg-sock&port=5497").PGHOST, "/tmp/pg-sock");
  assert.deepEqual(pg("postgres://old:old@localhost:5432/pages?user=new&password=new&port=5497"),
    { PGHOST: "localhost", PGPORT: "5497", PGUSER: "new", PGPASSWORD: "new", PGDATABASE: "pages", PGSSLMODE: "disable" });
  assert.equal(pg("postgres://p:q@[::1]:5433/d").PGHOST, "::1");
  assert.equal(pg("postgres://p:q@/d").PGHOST, "localhost");
  // TLS: off unless asked, full verification when asked (as node-postgres).
  assert.deepEqual([pg("postgres://p:q@h/d?sslmode=verify-full").PGSSLMODE, pg("postgres://p:q@h/d?sslmode=verify-full").PGSSLROOTCERT],
    ["verify-full", "system"]);
  // Without a URL the PG* settings are the source, untouched.
  assert.deepEqual(backup.dumpEnvironment({ PGDATABASE: "northwind", PGHOST: "/s" }), { PGDATABASE: "northwind", PGHOST: "/s" });
  // Refused, never silently dropped or weakened, and never quoting the URL.
  for (const [url, why] of [
    ["not a url", /is not a postgres:\/\/ connection URL/],
    ["mysql://pages:hunter-fixture@host/pages", /is not a postgres:\/\/ connection URL/],
    ["postgres://pages:hunter-fixture@host/pages?sslrootcert=/etc/northwind.crt", /option "sslrootcert" is not supported/],
    ["postgres://pages:hunter-fixture@host/pages?options=-c%20x", /option "options" is not supported/],
    ["postgres://pages:hunter-fixture@host/pages?sslmode=require", /sslmode must be disable or verify-full/],
    ["postgres://pages:hunter-fixture@host/pages?sslmode=prefer", /sslmode must be disable or verify-full/],
  ]) {
    assert.throws(() => backup.dumpEnvironment({ DATABASE_URL: url }), (error) => {
      assert.match(error.message, why);
      assert.doesNotMatch(error.message, /hunter-fixture|\/etc\/northwind|mysql:/);
      return true;
    }, url);
  }
});

// PostgreSQL cut the URL it took as a database name to 63 bytes, so the echo
// slipped past exact-value redaction. Lines that show a connection string, or
// the start of a secret, are dropped whole; exact secrets are replaced.
test("backup errors never echo a connection string or part of a secret", () => {
  const minted = "0123456789abcdef".repeat(3); // 48 hex, the shape bootstrap mints
  const url = `postgres://pages:${minted}@127.0.0.1:5432/pages?sslmode=disable`;
  const env = { DATABASE_URL: url, PGPASSWORD: minted, RAW_TOKEN_SECRET: "northwind-raw-fixture-value" };
  const leaks = (text) => /0123456789ab|postgres(ql)?:\/\/|northwind-raw/i.test(text);
  for (const line of [
    `FATAL:  database "${url.slice(0, 63)}" does not exist`,
    `FATAL:  database "${`postgres://pages:alpha'${"x".repeat(70)}@h/p`.slice(0, 63)}" does not exist`,
    `error at "PostgreSQL://pages:${minted.slice(0, 20)}"`,
    `password authentication failed; tried ${minted.slice(0, 30)}`,
  ]) {
    const out = backup.redact(line, env);
    assert.equal(out, "[line removed: it may contain a credential]", line);
  }
  assert.equal(backup.redact("token northwind-raw-fixture-value refused\nplain context", env), "token [redacted] refused\nplain context");

  // Split across chunks: the URL arrives in pieces and is still removed.
  const split = backup.stderrCollector(env);
  for (const piece of ["pg_dump: error: FATAL: database \"postg", "res://pages:0123", "456789ab\" does not exist\n", "pg_dump: hint\n"]) split.push(piece);
  assert.equal(split.text(), "[line removed: it may contain a credential]\npg_dump: hint");

  // The tail is cut AFTER redaction, so a cut cannot strip the identifying prefix.
  const tail = backup.stderrCollector(env, 200);
  tail.push(`${"noise ".repeat(40)}\nFATAL: database "${url.slice(0, 63)}"\n${"z".repeat(150)}\n`);
  assert.ok(!leaks(tail.text()), tail.text());

  // An oversized line is dropped whole, including the rest arriving later.
  const big = backup.stderrCollector(env, 100);
  big.push("a".repeat(150));
  big.push(`${minted.slice(12)} tail of the oversized line\nnext line\n`);
  assert.equal(big.text(), "[line removed: it may contain a credential]\nnext line");
  // A final unterminated line is still redacted.
  const last = backup.stderrCollector(env);
  last.push(`FATAL: "${url}`);
  assert.equal(last.text(), "[line removed: it may contain a credential]");
});

// Every client box signs in centrally, and those settings name the live host,
// which the localhost probe would fail. Rehearsal pins the self-contained mode.
test("restore rehearsal never carries central sign-in settings", (t) => {
  environment(t, { PAGES_AUTH_MODE: "central", PAGES_PUBLIC_URL: "https://pages.northwind.example",
    AUTH_ISSUER_URL: "https://auth.northwind.example", AUTH_CLIENT_ID: "pages",
    AUTH_CLIENT_SECRET: "northwind-central-client-fixture-0000000000", AUTH_SIGNING_PUBKEY: "northwind-key-fixture",
    PAGES_AUTH_COOKIE_SECURE: "1", AUTH_COOKIE_NAME: "northwind_auth" });
  const env = backup.isolatedEnv("/tmp/northwind-private");
  assert.equal(env.PAGES_AUTH_MODE, "elcano");
  assert.equal(env.AUTH_SIGNING_PUBKEY, "", "admin requests fail closed");
  for (const key of ["PAGES_PUBLIC_URL", "AUTH_ISSUER_URL", "AUTH_CLIENT_ID", "AUTH_CLIENT_SECRET", "PAGES_AUTH_COOKIE_SECURE", "AUTH_COOKIE_NAME"]) {
    assert.equal(env[key], undefined, key);
  }
});
