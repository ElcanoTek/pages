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
    // A container whose preinstalled Chromium is not the build this Playwright
    // release pins can point at it instead of downloading another one.
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE }
      : {},
  },
  // The fixture is the Go binary's hidden `browser-fixture` verb: the real
  // shells, gate pages and embedded assets over an in-memory admin API
  // (internal/fixture). It is built first, outside Playwright's outputDir (which
  // a run empties), so the server Playwright waits on is the compiled binary and
  // not a `go run` wrapper. The timeout covers a cold build cache; a warm one
  // starts in about a second.
  webServer: {
    command: "go build -o node_modules/.cache/pages-browser-fixture/pages ./cmd/pages && exec node_modules/.cache/pages-browser-fixture/pages browser-fixture",
    url: `${ORIGIN}/healthz`,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
    env: { PLAYWRIGHT_FIXTURE_PORT: String(PORT) },
  },
});
