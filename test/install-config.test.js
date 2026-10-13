// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const { mkdtempSync, mkdirSync, readFileSync, writeFileSync, rmSync } = require("node:fs");
const path = require("node:path");
const { render } = require("../scripts/render-install");

test("installation: service, proxy, CLI and rerun share custom settings", () => {
  const dir = mkdtempSync(path.join(require("node:os").tmpdir(), "pages-install-"));
  try {
    const values = { APP_DIR: `${dir}/app`, APP_USER: "northwind", INSTALL_SRC_DIR: `${dir}/source`, ENV_FILE: `${dir}/pages.env`, CLI_TARGET: `${dir}/bin/pages`, PORT: "4312", PAGES_INSTALL_CONFIG: `${dir}/install`, DASHBOARD_HOST: "dashboard.test", CONTENT_HOST: "content.test" };
    writeFileSync(values.PAGES_INSTALL_CONFIG, render("config", values));
    writeFileSync(values.ENV_FILE, 'PORT="4517"\n');
    const service = render("service", values);
    assert.match(service, /User=northwind\nGroup=northwind/);
    assert.ok(service.includes(`WorkingDirectory=${values.APP_DIR}`));
    assert.ok(service.includes(`EnvironmentFile=${values.ENV_FILE}`));
    assert.ok(service.includes(`ReadWritePaths=${values.APP_DIR}/assets`));
    assert.equal((render("caddy", values).match(/reverse_proxy 127\.0\.0\.1:4312/g) || []).length, 2);
    assert.ok(render("cli", values).includes(values.PAGES_INSTALL_CONFIG));
    const read = (extra = {}) => spawnSync("bash", ["-c", '. scripts/install-config.sh; printf "%s\\n" "$APP_DIR" "$APP_USER" "$INSTALL_SRC_DIR" "$ENV_FILE" "$CLI_TARGET" "$PORT"'], {
      cwd: path.join(__dirname, ".."), encoding: "utf8", env: { PATH: process.env.PATH, PAGES_INSTALL_CONFIG: values.PAGES_INSTALL_CONFIG, ...extra },
    });
    assert.deepEqual(read().stdout.trim().split("\n"), [values.APP_DIR, "northwind", values.INSTALL_SRC_DIR, values.ENV_FILE, values.CLI_TARGET, "4517"]);
    assert.equal(read({ PAGES_PORT: "4618" }).stdout.trim().split("\n").at(-1), "4618");
    assert.equal(read({ APP_DIR: `${dir}/override` }).stdout.split("\n")[0], `${dir}/override`);

    mkdirSync(`${dir}/bin`);
    mkdirSync(`${values.INSTALL_SRC_DIR}/.git`, { recursive: true });
    mkdirSync(`${values.APP_DIR}/scripts`, { recursive: true });
    writeFileSync(`${values.APP_DIR}/scripts/template.js`, "");
    writeFileSync(values.CLI_TARGET, render("cli", values), { mode: 0o755 });
    // Capture the real CLI's sudo boundary without starting or altering any
    // system service. Config path must survive sudo's environment filtering.
    writeFileSync(`${dir}/bin/sudo`, '#!/usr/bin/env bash\nprintf "%s\\n" "$@"\n', { mode: 0o755 });
    const invoke = (...args) => spawnSync("bash", [values.CLI_TARGET, ...args], {
      encoding: "utf8", env: { PATH: `${dir}/bin:${process.env.PATH}` },
    });
    for (const command of ["update", "rebuild"]) {
      const call = invoke(command);
      assert.equal(call.status, 0, call.stderr);
      for (const value of [`PAGES_INSTALL_CONFIG=${values.PAGES_INSTALL_CONFIG}`, `APP_DIR=${values.APP_DIR}`, "APP_USER=northwind", `PAGES_ENV_FILE=${values.ENV_FILE}`, `${values.INSTALL_SRC_DIR}/scripts/update.sh`]) assert.ok(call.stdout.includes(value));
    }
    const template = invoke("template", "list");
    assert.equal(template.status, 0, template.stderr);
    assert.ok(template.stdout.includes(`northwind\n--\nbash`));
    assert.ok(template.stdout.includes(`${values.ENV_FILE}\n${values.APP_DIR}\nlist`));
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("installation: unsupported path and port values fail before installing", () => {
  const values = { APP_DIR: "/opt/pages", APP_USER: "pages", INSTALL_SRC_DIR: "/opt/pages-src", ENV_FILE: "/etc/default/pages", CLI_TARGET: "/usr/local/bin/pages", PORT: "3002", PAGES_INSTALL_CONFIG: "/etc/default/pages-install" };
  for (const invalid of [{ PORT: "70000" }, { APP_DIR: "/tmp/my app" }, { APP_USER: "bad user" }]) assert.throws(() => render("check", { ...values, ...invalid }));
  assert.match(render("service", values), /WorkingDirectory=\/opt\/pages/);
});

test("installation: a re-run reuses the database password from the prior URL", () => {
  const password = (url) => spawnSync("bash", ["-c", 'set -euo pipefail; . scripts/install-config.sh; database_password "$1"', "_", url], {
    cwd: path.join(__dirname, ".."), encoding: "utf8", env: { PATH: process.env.PATH, PAGES_INSTALL_CONFIG: "/nonexistent", PAGES_ENV_FILE: "/nonexistent" },
  });
  const reused = (url) => {
    const result = password(url);
    assert.equal(result.status, 0, result.stderr);
    return result.stdout.replace(/\n$/, ""); // as bootstrap's $(...) strips it
  };
  const minted = reused("");
  assert.match(minted, /^[0-9a-f]{48}$/);
  assert.notEqual(reused(""), minted);
  assert.equal(reused(`postgres://pages:${minted}@127.0.0.1:5432/pages?sslmode=disable`), minted);
  assert.equal(reused("postgresql://pages:a.b_c~d-e@db.test/pages"), "a.b_c~d-e");
  // An existing URL that cannot be reused verbatim (wrong scheme or role, no
  // password, or text needing URL decoding or SQL quoting) fails with no
  // output, so bootstrap stops before changing the role.
  for (const url of ["mysql://pages:secret@host/pages", "postgres://other:secret@host/pages", "postgres://pages@host/pages",
    "postgres://pages:@host/pages", "postgres://pages:p%40ss@host/pages", "postgres://pages:it's@host/pages"]) {
    const result = password(url);
    assert.equal(result.status, 1, url);
    assert.equal(result.stdout, "", url);
  }
  // Bootstrap must stop on that failure, and must call the helper before the
  // role is created or altered.
  const bootstrap = readFileSync(path.join(__dirname, "../scripts/bootstrap.sh"), "utf8");
  const call = bootstrap.search(/DB_PASS="\$\(database_password "\$\{DATABASE_URL:-\}"\)" \\\n\s+\|\| die /);
  assert.ok(call > 0);
  assert.ok(call < bootstrap.indexOf("ALTER ROLE pages") && call < bootstrap.indexOf("CREATE ROLE pages"));
});
