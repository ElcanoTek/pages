// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const { spawnSync } = require("node:child_process");
const { mkdtempSync, writeFileSync, rmSync } = require("node:fs");
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
  } finally { rmSync(dir, { recursive: true, force: true }); }
});

test("installation: unsupported path and port values fail before installing", () => {
  const values = { APP_DIR: "/opt/pages", APP_USER: "pages", INSTALL_SRC_DIR: "/opt/pages-src", ENV_FILE: "/etc/default/pages", CLI_TARGET: "/usr/local/bin/pages", PORT: "3002", PAGES_INSTALL_CONFIG: "/etc/default/pages-install" };
  for (const invalid of [{ PORT: "70000" }, { APP_DIR: "/tmp/my app" }, { APP_USER: "bad user" }]) assert.throws(() => render("check", { ...values, ...invalid }));
  assert.match(render("service", values), /WorkingDirectory=\/opt\/pages/);
});
