// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Keep this dependency-free and parseable on old Node so install/startup can
// explain an unsupported runtime before loading ESM-only dependencies.
const SUPPORTED = "Node.js 20.19+ (20.x), 22.12+ (22.x), or newer than 22";
function assertSupported(version = process.versions.node) {
  const match = /^(\d+)\.(\d+)\.(\d+)$/.exec(version);
  const major = match && Number(match[1]);
  const minor = match && Number(match[2]);
  if (!match || !((major === 20 && minor >= 19) || (major === 22 && minor >= 12) || major > 22)) {
    throw new Error(`Pages requires ${SUPPORTED}; found Node.js ${version}. Upgrade Node before installing or starting Pages.`);
  }
}
if (require.main === module) {
  try { assertSupported(); }
  catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { assertSupported };
