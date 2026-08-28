// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { defineConfig } = require("@playwright/test");

// The fixture port is configurable so several checkouts (git worktrees, two
// terminals) can run the browser suite at once. Without it every run shares
// :3210 and `reuseExistingServer` silently points one checkout's specs at
// another checkout's fixture server — a green run that proved nothing.
const PORT = Number(process.env.PLAYWRIGHT_FIXTURE_PORT || 3210);
const ORIGIN = `http://127.0.0.1:${PORT}`;

module.exports = defineConfig({
  testDir: "./test/browser",
  testMatch: "**/*.spec.js",
  fullyParallel: false,
  workers: 1,
  timeout: 45_000,
  expect: { timeout: 8_000 },
  reporter: process.env.CI ? [["line"], ["html", { open: "never", outputFolder: "playwright-report" }]] : "line",
  use: {
    baseURL: ORIGIN,
    colorScheme: "dark",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  webServer: {
    command: "node test/browser/fixture-server.js",
    url: `${ORIGIN}/healthz`,
    reuseExistingServer: !process.env.CI,
    timeout: 30_000,
    env: { PLAYWRIGHT_FIXTURE_PORT: String(PORT) },
  },
});
