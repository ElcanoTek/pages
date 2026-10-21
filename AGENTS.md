# Notes for coding agents

Short orientation for an AI coding agent working in this repository. Humans
should read [`CONTRIBUTING.md`](CONTRIBUTING.md) instead — it covers the same
ground properly.

## Run and test

```bash
make build                         # compile-check ./... and write ./pages
./pages dev                        # Postgres + migrate + token + server on :3099
./pages dev token                  # reprint the saved agent token

make test                          # go test ./... — run constantly
make test-db                       # throwaway Postgres on :5433 for the database suites
make test-race                     # what CI runs (plus vet, gofmt, golangci-lint)
make lint                          # golangci-lint run ./...
node --test test/frontend.test.js  # front-end unit tests
npm ci && npx playwright test      # browser suite against `pages browser-fixture`
```

`go.mod` pins the toolchain; the `Makefile` sets `GOTOOLCHAIN=auto`, so a
plain `make` fetches it. `pages dev` stands up its own cluster under
`/var/tmp/pages-dev` on Linux and uses your Homebrew Postgres on macOS (or
`PAGES_DEV_DATABASE_URL` anywhere); it finds `initdb` under the usual
versioned directories itself.

Sign in to the local admin UI at
`http://localhost:3099/__dev/login?next=/admin`. That route exists only when
`PAGES_DEV_LOGIN=1`, which only `pages dev` sets, and the server refuses it
under `PAGES_ENV=production`. Never set it in production.

Without Postgres the database suites **skip**, so a green `make test` with no
database proves little. Run `make test-db` first (or point
`PAGES_TEST_DATABASE_URL` at a server whose role may create databases);
`PAGES_TEST_REQUIRE_DB=1` turns a missing database into a failure, as CI does.
`internal/testdb` gives each test its own freshly migrated database.

## Ground rules

- **Branch off `main`; never commit to `main` directly.** Branch names:
  `feat/<slug>`, `fix/<issue>-<slug>`, `docs/<slug>`, `chore/<slug>`.
- **Run the database suites before proposing a change** (`make test-db`, then
  `make test-race`). Without them the tests will not catch breakage in the
  state machine, the host split, or the authorization matrix.
- **Do not weaken the invariants** listed under "Invariants" in
  [`CONTRIBUTING.md`](CONTRIBUTING.md). Several of them are the entire reason
  this system is safe to point at a client, and each is covered by a test that
  exists because it broke silently once.
- **Every first-party source file carries an SPDX header.** New files need one,
  in the file's own comment syntax, after any shebang. In Go it is the first
  two lines, before the package doc comment:
  ```go
  // SPDX-License-Identifier: BUSL-1.1
  // Copyright (c) 2026 ElcanoTek, Inc.
  ```
  Do not add one under `public/assets/flag/` (vendored; it keeps its upstream
  licence headers), or to `deploy/motd` (the login banner, installed verbatim
  as `/etc/motd`, so a header would print at every ssh login). Generated Go
  files such as `internal/preflight/shadowed_names.go` get theirs from the
  generator; regenerate, do not hand-edit.
- **`gofmt`, `go vet` and `golangci-lint` (`.golangci.yml`) must be clean.**
  A `//nolint` needs a specific linter and an explanation.
- **Never edit an applied migration.** Add a new numbered file in `migrations/`
  and classify it in `migrations/compatibility.json`.
- **`public/assets/flag/` is vendored.** Do not hand-edit it.
- **Use invented names in tests, docs and examples** — "Northwind", "Contoso",
  "Fabrikam". Never a real client, customer, brand or person.
- **Never commit a real credential.** `.devdata/` and `.env` are gitignored and
  must stay that way.
- Write commit subjects as a sentence about the change, in the imperative.
  `git log --oneline` is the house style guide.

## Where things are

`cmd/pages` is the one binary; `internal/cli` registers its verbs (`serve`,
`migrate`, `token`, `template`, `doctor`, `update`, `backup`, … — one file per
verb). `internal/server` is the two-host split and route wiring.
`internal/versions` is the version state machine and the centre of the system
— REST (`internal/api`), MCP (`internal/mcp` over `internal/mcpproto`) and the
admin UI (`internal/adminapi`) all route through it. `embed.go` embeds
`public/`, `migrations/`, `templates/` and `docs/` into the binary.
[`docs/SECURITY.md`](docs/SECURITY.md) explains the security model and is
required reading before touching anything under `internal/csp`,
`internal/rawtoken`, `internal/pagecookie`, `internal/tokens` or
`internal/portals`. Operator tooling (bootstrap, update, doctor, backup) lives
under `internal/ops`. The full doc index is in the [README](README.md).

## Dependencies

Prefer the standard library or a small piece of owned code to a new module;
add a dependency only when it clearly earns its place. Dependabot proposes
updates daily, after a cooldown of 7 days (3 for patch releases), with minor
and patch grouped ([`.github/dependabot.yml`](.github/dependabot.yml)); a human
merges one only when the bump earns it — a security fix whose code path we use,
or a specific commit we need. The `go` directive in `go.mod` is moved by hand
(Dependabot does not), and CI runs `govulncheck`. On an advisory, check
reachability first; if the affected code is unused, delete our use of it
instead of upgrading; if it is used, take or adapt the fix. Code copied from
another project keeps its licence text beside it and an entry in
[`NOTICE`](NOTICE).

The Go modules the server uses (`go.mod`):

| Module | Used by | For |
| --- | --- | --- |
| `github.com/jackc/pgx/v5` | `internal/db` | PostgreSQL driver and pool (pulls in `pgpassfile`, `pgservicefile`, `puddle`) |
| `golang.org/x/net/html` | `internal/preflight`, `internal/pagedata` | HTML5 tokenizing of deployed documents and managed blocks |
| `github.com/tdewolff/parse/v2` | `internal/preflight` | Parse-only syntax check of inline classic scripts and modules; bounded recursion |
| `github.com/santhosh-tekuri/jsonschema/v6` | `internal/pagedata` | JSON Schema 2020-12 validation of managed config and data |
| `golang.org/x/crypto` | `internal/pagecookie` | scrypt password hashing |
| `golang.org/x/text` | `internal/auth/central`, `internal/contentview` | NFC email normalization; ISO-8859-1 form bodies |

What a framework would otherwise supply is owned code:

| What | Lives in | Licence |
| --- | --- | --- |
| Stateless MCP server (JSON-RPC 2.0: initialize, ping, tools/list, tools/call) and tool input parsing | `internal/mcpproto` | own code |
| MCP envelope schemas and protocol versions | `internal/mcpproto/envelope.go`, adapted from `@modelcontextprotocol/sdk` 1.32.1 `src/types.ts` | MIT, `internal/mcpproto/LICENSE.mcp-sdk` |
| JS-exact JSON parse/stringify (key order, number formatting, escaping) | `internal/jsonv`, `internal/jsstr` | own code |
| Express-compatible responses (weak ETags, 304s, JSON content type), routing and body limits | `internal/httpx`, `internal/route`, `internal/reqbody` | own code |
| Fixed-window rate limiter, draft-7 headers, IPv6 /56 keys | `internal/ratelimit` | own code |
| Dashboard and content security headers | `internal/csp` | own code |
| `MAX_HTML_BYTES` / `PAGES_UPLOAD_MAX_BYTES` size parser | `internal/datalimits` (`ParseByteSize`) | own code |

npm holds **test tooling only** — Playwright and axe for the browser suite.

The MCP wire is held to the official SDK by recordings, not by a live SDK:
`internal/mcpproto` replays `internal/mcp/testdata/mcp-wire.golden.json` (every response byte
from the SDK transport) and the corpora in `internal/mcpproto/testdata`
(`protocol-corpus.json`, `envelope-verdicts.json`), recorded once from the
SDK while the Node release was still in the tree. They are fixed now: change
them only for a deliberate, reviewed wire change. To adopt a new MCP protocol
version, re-adapt `envelope.go` and the supported-version list from the
matching SDK release and record the new behaviour against that SDK (the old
recorder scripts are in git history under `internal/mcpproto/testdata/record`).

Golden files recorded from the Node release pin its old limits with
`internal/nodedefaults.Setenv(t)`. When a test needs a limit, derive it from
the `datalimits` / `uploads` / `pagedata` functions, never a literal.

Go comments that cite `lib/*.js`, `server.js`, `scripts/*.js` or `test/*.js`
name the Node release this server was ported from, which is no longer in the
tree. They record provenance (what wire format or behaviour a package keeps);
the Go package is the source of truth. `git log --diff-filter=D -- lib/` finds
the commit that removed them, and `git show <commit>^:<path>` reads one.
