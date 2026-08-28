// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// scripts/token.js — mint / list / revoke agent API tokens for local dev
// (PLAN.md §9 `pages token …`). The production `pages` CLI shells out to the
// same lib. Requires DATABASE_URL (or PG* env) and API_TOKEN_PEPPER to match
// the running server's.
//
//   node scripts/token.js add <label> [scope] [slug ...]
//     scope: deploy (default) | admin | data_update (may start with no grants)
//   node scripts/token.js list
//   node scripts/token.js revoke <id>
//
// The raw token is printed ONCE by `add` — copy it; it is not recoverable.

const tokens = require("../lib/tokens");
const { pool } = require("../lib/db");

async function main() {
  const [cmd, ...args] = process.argv.slice(2);
  switch (cmd) {
    case "add": {
      const label = args[0];
      const scope = args[1] || "deploy";
      const allowedSlugs = args.slice(2);
      if (!label) throw new Error("usage: token.js add <label> [scope] [slug ...]");
      const t = await tokens.mint({ label, scope, allowedSlugs });
      console.log(`created token #${t.id}  label=${t.label}  scope=${t.scope}  prefix=${t.prefix}`);
      if (t.allowed_slugs.length) console.log(`allowed slugs: ${t.allowed_slugs.join(", ")}`);
      console.log(`\n  ${t.token}\n`);
      console.log("^ copy this now — it will not be shown again.");
      break;
    }
    case "list": {
      const rows = await tokens.list();
      if (!rows.length) {
        console.log("(no tokens)");
        break;
      }
      for (const r of rows) {
        const state = r.revoked_at ? "REVOKED" : "active";
        const used = r.last_used_at ? `used ${r.last_used_at.toISOString()}` : "never used";
        const grants = r.allowed_slugs.length ? `  slugs=${r.allowed_slugs.join(",")}` : "";
        console.log(`#${r.id}  ${r.prefix}…  ${r.label}  [${r.scope}]  ${state}  ${used}${grants}`);
      }
      break;
    }
    case "revoke": {
      const id = Number(args[0]);
      if (!Number.isInteger(id)) throw new Error("usage: token.js revoke <id>");
      const ok = await tokens.revoke(id);
      console.log(ok ? `revoked token #${id}` : `no active token #${id}`);
      break;
    }
    default:
      console.log("usage: token.js add <label> [scope] [slug ...] | list | revoke <id>");
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
