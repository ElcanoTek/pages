# Notes for coding agents

Short orientation for an AI coding agent working in this repository. Humans
should read [`CONTRIBUTING.md`](CONTRIBUTING.md) instead — it covers the same
ground properly.

## Run and test

```bash
npm install
sudo bash scripts/dev.sh          # Linux: Postgres + migrate + token + server on :3099
bash scripts/dev-macos.sh         # macOS, against a Homebrew postgresql@16

npm test                          # unit tests, no database — run constantly
npm run test:browser              # Playwright: admin shell + accessibility
bash test/run-integration.sh      # full suite against a throwaway Postgres
```

Sign in to the local admin UI at
`http://localhost:3099/__dev/login?next=/admin`. That route exists only when
`PAGES_DEV_LOGIN=1`, which only the two dev scripts set. Never in production.

If `initdb` is not on `PATH`, add it first — for example
`export PATH="/usr/lib/postgresql/16/bin:$PATH"`.

## Ground rules

- **Branch off `main`; never commit to `main` directly.** Branch names:
  `feat/<slug>`, `fix/<issue>-<slug>`, `docs/<slug>`, `chore/<slug>`.
- **Run `bash test/run-integration.sh` before proposing a change.** The unit
  tests alone will not catch breakage in the state machine, the host split, or
  the authorization matrix.
- **Do not weaken the invariants** listed under "Invariants" in
  [`CONTRIBUTING.md`](CONTRIBUTING.md). Several of them are the entire reason
  this system is safe to point at a client, and each is covered by a test that
  exists because it broke silently once.
- **Every first-party source file carries an SPDX header.** New files need one,
  in the file's own comment syntax, after any shebang:
  ```js
  // SPDX-License-Identifier: BUSL-1.1
  // Copyright (c) 2026 ElcanoTek, Inc.
  ```
  Do not add one under `public/assets/flag/` or `lib/vendor/` (vendored; they
  keep their upstream licence header) or to `lib/preflight-shadowed-names.js`
  (generated).
- **Never edit an applied migration.** Add a new numbered file in `migrations/`.
- **`public/assets/flag/` is vendored.** Do not hand-edit it.
- **Use invented names in tests, docs and examples** — "Northwind", "Contoso",
  "Fabrikam". Never a real client, customer, brand or person.
- **Never commit a real credential.** `.devdata/` and `.env` are gitignored and
  must stay that way.
- Write commit subjects as a sentence about the change, in the imperative.
  `git log --oneline` is the house style guide.

## Where things are

`server.js` is the two-vhost host split and route wiring. `lib/versions.js` is
the version state machine and the centre of the system — REST, MCP and the
admin UI all route through it. [`docs/SECURITY.md`](docs/SECURITY.md) explains
the security model and is required reading before touching anything under
`lib/csp.js`, `lib/rawtoken.js`, `lib/pagecookie.js`, `lib/tokens.js` or
`lib/portals.js`. The full doc index is in the [README](README.md).

## Dependencies and in-housed code

Prefer the standard library or a small piece of owned code to a new package;
add a dependency only when it clearly earns its place. Dependabot proposes
updates daily, after a cooldown of 7 days (3 for patch releases), with minor
and patch grouped ([`.github/dependabot.yml`](.github/dependabot.yml)); a human
merges one only when the bump earns it — a security fix whose code path we use,
or a specific commit we need. In-housed code is ours and is not tracked by
Dependabot. On an advisory, check reachability first; if the affected code is
unused, delete our use of it instead of upgrading; if it is used, take or adapt
the fix. Code copied from another project keeps its licence text beside it and
an entry in [`NOTICE`](NOTICE).

| What | Lives in | Replaces | Licence |
| --- | --- | --- | --- |
| Stateless MCP server (JSON-RPC 2.0: initialize, ping, tools/list, tools/call) | `lib/mcp-protocol.js`, `lib/mcp.js` | `@modelcontextprotocol/sdk` 1.32.1 McpServer + StreamableHTTPServerTransport | own code |
| MCP envelope schemas and protocol versions | `lib/vendor/mcp-sdk/types.js` | adapted from `@modelcontextprotocol/sdk` 1.32.1 `src/types.ts` | MIT, `lib/vendor/mcp-sdk/LICENSE` |
| Fixed-window rate limiter, draft-7 headers, IPv6 /56 keys | `lib/ratelimit.js` | `express-rate-limit` 8.7.1 | own code |
| Dashboard security headers | `lib/csp.js` (`shellHeaders`) | `helmet` 8.3.0 | own code |
| `MAX_HTML_BYTES` size parser | `lib/data-limits.js` (`parseByteSize`) | `bytes` 3.1.2 | own code |

The MCP SDK stays a **devDependency** as the conformance gate:
`test/mcp-protocol.test.js` answers a corpus side by side with the SDK's
`McpServer`, `test/mcp.integration.js` drives `/mcp` with the SDK's `Client`,
and `test/mcp-wire.integration.js` compares every response byte with
`test/mcp-wire.golden.json`, recorded from the SDK transport. Re-record that
golden (`MCP_WIRE_CAPTURE=1`, inside the integration environment) only for a
deliberate, reviewed wire change. To adopt a new MCP protocol version, re-adapt
`lib/vendor/mcp-sdk/types.js` from the matching SDK release and bump the SDK
devDependency so the side-by-side tests compare against it. There are no data
files to refresh.
