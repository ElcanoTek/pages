// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { spawnSync } = require("node:child_process");
const { merge, writeEnv } = require("../scripts/write-env");

test("bootstrap environment: preserve optional/operator values and quote managed values losslessly", () => {
  const original = '# operator settings\nRL_API_PER_MIN="357"\nPG_LOCK_TIMEOUT_MS="1234"\nPAGES_DATA_MAX_BYTES="524288"\nOPERATOR_NOTE="first\nPORT=inside-note\nlast"\nPORT="3002"\n';
  const database = "postgres://pages:synthetic-$`\\\"'@localhost/pages";
  const updated = merge(original, { PORT: "4312", DATABASE_URL: database });
  assert.ok(updated.includes(original.slice(0, original.lastIndexOf('PORT="3002"'))));
  const childEnv = { ...process.env };
  delete childEnv.NODE_TEST_CONTEXT;
  const decoded = spawnSync("bash", ["-c", 'set -a; eval "$1"; node -e "console.log(JSON.stringify(process.env))"', "_", updated], { encoding: "utf8", env: childEnv, timeout: 5000 });
  assert.equal(decoded.status, 0, decoded.stderr);
  const env = JSON.parse(decoded.stdout);
  assert.equal(env.RL_API_PER_MIN, "357");
  assert.equal(env.PG_LOCK_TIMEOUT_MS, "1234");
  assert.equal(env.PAGES_DATA_MAX_BYTES, "524288");
  assert.equal(env.OPERATOR_NOTE, "first\nPORT=inside-note\nlast");
  assert.equal(env.PORT, "4312");
  assert.equal(env.DATABASE_URL, database);
  assert.equal(merge(updated, { PORT: "4312", DATABASE_URL: database }), updated, "reruns do not accumulate entries");
});

test("bootstrap environment: failed generation keeps the original; a valid replacement retains a recoverable copy", () => {
  const dir = fs.mkdtempSync(path.join(require("node:os").tmpdir(), "pages-env-"));
  const file = path.join(dir, "pages");
  try {
    const original = 'PORT="3002"\nRL_CONTENT_PER_MIN="391"\n';
    fs.writeFileSync(file, original, { mode: 0o640 });
    assert.throws(() => writeEnv(file, { PORT: "bad\nvalue" }), /single-line/);
    assert.equal(fs.readFileSync(file, "utf8"), original);
    // A staging write failure occurs before rename and must not truncate the
    // active file; the fixture owns this collision in a temporary directory.
    fs.mkdirSync(`${file}.${process.pid}.tmp`);
    assert.throws(() => writeEnv(file, { PORT: "4312" }));
    assert.equal(fs.readFileSync(file, "utf8"), original);
    fs.rmdirSync(`${file}.${process.pid}.tmp`);
    writeEnv(file, { PORT: "4312" });
    assert.equal(fs.readFileSync(`${file}.previous`, "utf8"), original);
    assert.match(fs.readFileSync(file, "utf8"), /PORT="4312"\nRL_CONTENT_PER_MIN="391"/);
    assert.equal(fs.statSync(file).mode & 0o777, 0o640);
    assert.equal(fs.statSync(`${file}.previous`).mode & 0o777, 0o640);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});
