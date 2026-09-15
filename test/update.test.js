// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { spawnSync } = require("node:child_process");
const root = path.join(__dirname, "..");

function fixture(failure) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pages-update-"));
  const app = path.join(dir, "app"), source = path.join(dir, "source"), bin = path.join(dir, "bin");
  const write = (name, text, mode) => { fs.mkdirSync(path.dirname(name), { recursive: true }); fs.writeFileSync(name, text, mode ? { mode } : undefined); };
  fs.mkdirSync(path.join(source, ".git"), { recursive: true });
  fs.mkdirSync(bin);
  for (const name of ["scripts/preflight-install.js", "scripts/check-migration-compatibility.js", "scripts/render-install.js", "deploy/pages.service", "deploy/pages-cli", "migrations/compatibility.json"]) write(path.join(source, name), fs.readFileSync(path.join(root, name)));
  write(path.join(source, "scripts/check-node.js"), '"use strict";\n');
  write(path.join(source, "server.js"), failure === "module" ? "require('./lib/missing-dependency');" : "module.exports = {};\n");
  write(path.join(source, "lib/readiness.js"), "exports.close=()=>Promise.resolve();\n");
  write(path.join(source, "lib/db.js"), 'exports.pool={end:()=>Promise.resolve()}; exports.query=async()=>({rows:[{filename:"022_page_upload_attempts.sql"}]});\n');
  write(path.join(source, "lib/migrate.js"), 'require("node:fs").appendFileSync(process.env.UPDATE_TEST_LOG,"migrated\\n");\n');
  if (failure === "compatibility") write(path.join(source, "migrations/999_unknown.sql"), "SELECT 1;");
  write(path.join(source, "release-marker"), "candidate");
  write(path.join(app, "release-marker"), "original");
  write(path.join(app, "assets/chart.txt"), "Northwind asset bytes");
  write(path.join(app, ".env"), "LOCAL_SETTING=preserved\n");
  write(path.join(dir, "pages.env"), "PORT=4312\nOPERATOR_SETTING=preserved\n");
  write(path.join(dir, "pages.service"), "original service\n");
  write(path.join(dir, "pages-cli"), "original CLI\n", 0o755);
  const executable = (name, text) => write(path.join(bin, name), '#!/usr/bin/env bash\nset -eu\n' + text + '\n', 0o755);
  executable("runuser", 'shift 2; [[ "$1" == -- ]] && shift; exec "$@"');
  executable("chown", "exit 0");
  executable("npm", '[[ "$UPDATE_TEST_FAILURE" != npm ]]');
  executable("git", "echo abcdef0");
  executable("sleep", "exit 0");
  executable("rsync", '[[ "$UPDATE_TEST_FAILURE" != copy ]] || exit 1; exec /usr/bin/rsync "$@"');
  executable("mv", 'if [[ "$UPDATE_TEST_FAILURE" == activate && "${@: -1}" == "$APP_DIR" ]] && [[ "$(readlink "${@: -2:1}" || true)" == *"/abcdef0-"* ]]; then exit 1; fi; exec /usr/bin/mv "$@"');
  executable("systemctl", 'marker="$(cat "$APP_DIR/release-marker" 2>/dev/null || true)"; printf "%s %s\\n" "$1" "$marker" >> "$UPDATE_TEST_LOG"; [[ "$1" != start || "$UPDATE_TEST_FAILURE" != start || "$marker" != candidate ]]');
  executable("curl", '[[ "$*" == *":4312/readyz"* ]] || exit 4; [[ "$UPDATE_TEST_FAILURE" != readiness || "$(cat "$APP_DIR/release-marker")" != candidate ]]');
  const env = { ...process.env, PATH: `${bin}:${process.env.PATH}`, APP_DIR: app, APP_USER: os.userInfo().username,
    PAGES_SRC_DIR: source, PAGES_ENV_FILE: path.join(dir, "pages.env"), PAGES_CLI_TARGET: path.join(dir, "pages-cli"),
    PAGES_INSTALL_CONFIG: path.join(dir, "install"), PAGES_SERVICE_FILE: path.join(dir, "pages.service"),
    PAGES_UPDATE_NO_PULL: "1", PAGES_UPDATE_YES: "1", PAGES_SKIP_TEMPLATE_SYNC: "1",
    UPDATE_TEST_FAILURE: failure, UPDATE_TEST_LOG: path.join(dir, "actions.log"),
  };
  delete env.NODE_TEST_CONTEXT;
  delete env.PAGES_PORT;
  const result = spawnSync("bash", ["-c", '. "$1"; pages_update', "_", path.join(root, "scripts/update.sh")], { env, encoding: "utf8", timeout: 20000 });
  return { dir, app, result };
}

for (const failure of ["copy", "npm", "module", "compatibility", "activate", "start", "readiness", "none"]) {
  test(`release update: ${failure === "none" ? "success retains predecessor" : failure + " failure keeps original service recoverable"}`, () => {
    const { dir, app, result } = fixture(failure);
    try {
      assert.equal(result.status, failure === "none" ? 0 : 1, result.stdout + result.stderr);
      assert.equal(fs.readFileSync(path.join(app, "release-marker"), "utf8"), failure === "none" ? "candidate" : "original");
      assert.equal(fs.readFileSync(path.join(app, "assets/chart.txt"), "utf8"), "Northwind asset bytes");
      assert.equal(fs.readFileSync(path.join(app, ".env"), "utf8"), "LOCAL_SETTING=preserved\n");
      assert.equal(fs.readFileSync(path.join(dir, "pages.env"), "utf8"), "PORT=4312\nOPERATOR_SETTING=preserved\n");
      if (failure !== "none") {
        assert.equal(fs.readFileSync(path.join(dir, "pages.service"), "utf8"), "original service\n");
        assert.equal(fs.readFileSync(path.join(dir, "pages-cli"), "utf8"), "original CLI\n");
      }
      if (["activate", "start", "readiness"].includes(failure)) {
        assert.match(result.stderr, /previous release is ready again/);
        assert.match(fs.readFileSync(path.join(dir, "actions.log"), "utf8"), /start original/);
      }
      if (failure === "none") {
        assert.ok(fs.lstatSync(app).isSymbolicLink());
        assert.equal(fs.readFileSync(path.join(app + ".releases", "previous", "release-marker"), "utf8"), "original");
      }
    } finally { fs.rmSync(dir, { recursive: true, force: true }); }
  });
}
