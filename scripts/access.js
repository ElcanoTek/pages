// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
require("./check-node").assertSupported();
// scripts/access.js — the central-mode access list (PAGES_AUTH_MODE=central).
// Auth keeps it current through signed access events once the Pages app has a
// back-channel URL; this command is for bootstrap and break-glass. Everyone on
// the list is a Pages admin. Requires DATABASE_URL (or PG* env).
//
//   node scripts/access.js grant <email>
//   node scripts/access.js revoke <email>   # also ends that email's sessions
//   node scripts/access.js list

const { store } = require("../lib/central-auth");
const { pool } = require("../lib/db");

async function main() {
  const [cmd, email] = process.argv.slice(2);
  switch (cmd) {
    case "grant": {
      if (!email) throw new Error("usage: access.js grant <email>");
      console.log(`granted ${await store.grantAccess(email)}`);
      break;
    }
    case "revoke": {
      if (!email) throw new Error("usage: access.js revoke <email>");
      const ok = await store.revokeAccess(email);
      console.log(ok ? `revoked ${email} and ended its sessions` : `${email} had no active access`);
      break;
    }
    case "list": {
      const emails = await store.listAccess();
      console.log(emails.length ? emails.join("\n") : "(no one has access)");
      break;
    }
    default:
      console.log("usage: access.js grant <email> | revoke <email> | list");
      process.exitCode = 1;
  }
}

main()
  .then(() => pool.end())
  .catch((err) => {
    console.error(err.message);
    pool.end();
    process.exit(1);
  });
