// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Load the real dependency graph and production configuration without binding a
// listener or writing to the database. A syntax-only entrypoint check misses
// errors in imported modules and incompatible locked dependencies.
require("../server");
Promise.all([require("../lib/db").pool.end(), require("../lib/readiness").close()])
  .catch((error) => { console.error(error.message); process.exitCode = 1; });
