# Contributing to Pages

Thanks for looking. Pages is source-available rather than open source, and
outside contributions are welcome under the terms below.

Before you start on anything substantial, open an issue. Pages has a small
number of load-bearing invariants (see [Invariants](#invariants-do-not-break-these))
and a patch that crosses one of them will be hard to accept no matter how well
written it is. A short conversation first is cheaper for both of us.

## Licensing of contributions

**Contributions are accepted under the Business Source License 1.1**, the same
licence as the rest of the repository — see [`LICENSE`](LICENSE) and
[`docs/LICENSING.md`](docs/LICENSING.md).

By opening a pull request you confirm that:

- You wrote the contribution, or you have the right to submit it.
- You license it to ElcanoTek, Inc. under BUSL-1.1 with the same parameters as
  this repository, including the MIT Change Licence and the rolling two-year
  Change Date.
- You grant ElcanoTek the right to relicense your contribution as part of a
  commercial licence for Pages. This is what lets ElcanoTek sell production
  rights to the whole work, and it is the practical reason the project can be
  published at all.

There is no separate CLA to sign. If any of the above does not work for you,
say so in the issue before writing code.

## Reporting a security issue

Do not open a public issue or pull request. Email **security@elcanotek.com** —
see [`SECURITY.md`](SECURITY.md).

## Getting set up

You need Go and PostgreSQL **server** tools — `initdb` and `pg_ctl`, not just
`psql`. `go.mod` pins the toolchain (Go 1.27) and the `Makefile` sets
`GOTOOLCHAIN=auto`, so any Go from 1.21 on fetches the pinned release on first
build. Node is needed only for the front-end and browser tests.

```bash
git clone https://github.com/ElcanoTek/pages.git
cd pages
make build          # compile-check ./... and write ./pages
```

### Running it

One command stands up a throwaway Postgres cluster, migrates, mints an agent
token, mints a dev SSO keypair, and boots the server in the foreground:

```bash
./pages dev
```

On Linux the cluster lives under `/var/tmp/pages-dev` (`PAGES_DEV_DIR`), trust
auth on a Unix socket on port 5499. Run as a normal user it is yours; run as
root it belongs to the `postgres` account via `runuser`, because Postgres
refuses to run as root. `initdb` is looked up on `PATH` and under
`/usr/lib/postgresql/*/bin` (Debian, Ubuntu) and `/usr/pgsql-*/bin`.

On macOS it uses the Homebrew `postgresql@16` you already have running
(`127.0.0.1:5432`, your own user; `PGHOST`, `PGPORT`, `PGUSER` and `PGDB`
override that), creating the `pages` database if it is missing. On either
platform `PAGES_DEV_DATABASE_URL` points it at a server of your choice instead.

It prints the URLs and the agent token when it comes up. State lives in
`.devdata/` (gitignored), so the token survives restarts.
`./pages dev token` reprints the saved token without booting anything.

The dashboard is `http://localhost:3099` and the content host is
`http://content.localhost:3099` — two hostnames, one process. Sign in to the
admin UI through the local dev-login route:

```
http://localhost:3099/__dev/login?next=/admin
```

That route only exists when `PAGES_DEV_LOGIN=1`, which only `pages dev` sets,
and never under `PAGES_ENV=production`. It must never be set in production.

## Tests and checks

CI (`.github/workflows/ci.yml`) runs four jobs and a pull request needs all of
them green, through the single `CI gate` check. CodeQL
(`.github/workflows/codeql.yml`) scans the Go and the JavaScript on every push
and PR and weekly; a new alert on your branch is yours to look at.

```bash
# Frontend standard job (offline; see the standard block in AGENTS.md)
make stack-check

# Go job (against a PostgreSQL 16 service, PAGES_TEST_REQUIRE_DB=1)
make build
go vet ./...
test -z "$(gofmt -l .)"
golangci-lint run ./...                     # v2.15.0, config in .golangci.yml
go test -race -count=1 ./...
go test -count=1 -run 'LatencyBackstop' ./...   # wall-clock backstops, skipped under -race
GOTOOLCHAIN="$(go env GOVERSION)" go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# Browser job
npm ci && npm audit --audit-level=high      # test tooling only
node --test test/frontend.test.js
npx playwright test

# Shell job
for f in install.sh scripts/*.sh scripts/lib/*.sh .claude/hooks/*.sh; do bash -n "$f"; done
shellcheck -S warning install.sh scripts/*.sh scripts/lib/*.sh .claude/hooks/*.sh
```

Locally, `make ci` is stack-check + vet + lint + `test-race`. What each one is for:

- **`make stack-check`** — `scripts/stack-check.sh`, shared by every Elcano
  repo: the frontend standard block in AGENTS.md is intact, the vendored Flag
  files match `flag.lock` (`scripts/flag-sync.sh check`), no served file under
  the `web=` directories of `stack.conf` (`public/`, `templates/` and the Go
  packages that write HTML) loads a script, stylesheet or font from another
  host, and npm holds test tooling only. A weekly `Flag drift` workflow
  separately reports when Flag upstream has moved past `flag.lock`.

- **`go test ./...`** — the whole server: unit tests beside each package, the
  HTTP surfaces driven end to end through `httptest`, and the database suites
  (state machine, REST, MCP, admin, view, rate limits, templates, migrations,
  backup rehearsal). Database tests use `internal/testdb`, which gives each test
  a freshly migrated database on `PAGES_TEST_DATABASE_URL` (default
  `postgres://pages@127.0.0.1:5433/postgres`, the cluster `make test-db`
  starts; `make test-db-stop` stops it). **Without a server they skip**, so run
  `make test-db` first — this is the run that catches real breakage. Set
  `PAGES_TEST_REQUIRE_DB=1` to make a missing database fail instead, as CI does.
- **`node --test test/frontend.test.js`** — the pure halves of the admin's
  browser modules, the Flag motion kit and the admin stylesheet. Node only, no
  npm install.
- **`npx playwright test`** — drives the real admin shell in Chromium against
  `pages browser-fixture`, a hidden verb serving the real shells and embedded
  assets over an in-memory admin API (`internal/fixture`), with no database.
  Playwright builds the binary first. Includes axe accessibility assertions.
  Set `PLAYWRIGHT_FIXTURE_PORT` to run two checkouts at once; without it,
  `reuseExistingServer` can point one checkout's specs at another's fixture
  server and give you a green run that proved nothing.

A behaviour change needs a test. The suites above are the specification for
most of this codebase — several of them exist because a subtle invariant broke
silently once already, and the comments say which.

## Branches, commits and pull requests

- Branch off `main`. Do not commit to `main` directly.
- Name the branch by intent: `feat/<short-slug>`, `fix/<issue>-<short-slug>`,
  `docs/<short-slug>`, `chore/<short-slug>`. Referencing the issue number in a
  `fix/` branch is the convention here.
- **Write commit subjects as a sentence about the change, in the imperative,
  from the reader's point of view.** The history is the changelog. Look at
  `git log --oneline` before writing one: subjects here read like *"Stop the
  Page menu leaking into the dashboard it lands in"*, not *"fix menu"* or
  *"update portals.js"*.
- Keep a pull request to one concern. A refactor and a behaviour change in one
  diff is two pull requests.
- Explain **why** in the pull request body, and say which suites you ran. If you
  changed anything in `internal/csp`, `internal/rawtoken`,
  `internal/pagecookie`, `internal/portals`, `internal/tokens` or the sandbox
  allow-list, say explicitly what the security consequence is.
- Squash-merge into `main` once CI is green.

## Invariants: do not break these

These are enforced in code and in tests, and they are the reason the system is
safe to point at a client. If your change needs one of them relaxed, that is
the conversation to have in the issue first.

1. **Untrusted content lives on a separate registrable domain.** The dashboard
   host is the trusted auth zone; the content host is cookieless and serves
   agent HTML under a strict sandbox CSP. Never render agent HTML on the
   dashboard origin. Enforced in `internal/server` by the `Host` header.
2. **Edit the stored source, never the rendered DOM.** An iframe is a preview
   only. No `contenteditable` or `outerHTML` round-trip — they corrupt chart
   markup. Editing means patching the source into a new draft and re-rendering.
3. **Pointer-is-truth.** "Live" is `pages.published_version_id`. Do not add a
   `published` status. Publish, approve and rollback are one `UPDATE` under
   `SELECT … FOR UPDATE`.
4. **`page_versions` content is append-only**, enforced by a database trigger.
   Every edit is a new row; only `status` and the review columns mutate, plus
   `coverage_gaps`, a cache derived from the row's immutable data (migration
   029) that a read may fill once.
5. **`/raw` is authorised only by the signed token** (`internal/rawtoken`), bound
   to page, version, purpose, render mode and expiry, and verified in constant
   time. No cookies on that host. The sandbox CSP headers go on *every* `/raw`
   response, errors included.
6. **Agents never approve, disable, clear a password, or touch partner portals.**
   Those are admin-cookie-plus-CSRF only. `internal/versions` is the single state
   machine; REST, MCP and the admin UI all route through it, and every mutation
   writes an `audit_log` row in the same transaction.
7. **`allow-same-origin` must never join `allow-scripts`** in the content-host
   sandbox. It is a documented sandbox escape.

[`docs/SECURITY.md`](docs/SECURITY.md) explains the reasoning behind each of
these at length. Read it before touching the security surface.

## Where things are

| Path | What lives there |
| --- | --- |
| `cmd/pages` | The one binary's `main`: dispatches to a verb |
| `internal/cli` | One file per verb — `serve`, `migrate`, `token`, `template`, `doctor`, `update`, `backup`, `dev`, … |
| `internal/server` | One process, two hosts; route wiring and the host split |
| `internal/versions` | The version state machine. The centre of the system |
| `internal/api`, `internal/adminapi`, `internal/mcp`, `internal/mcpproto` | REST, the admin JSON API, the MCP tool catalog and the MCP protocol core |
| `internal/contentview`, `internal/render`, `internal/shell` | The content host, render-time theming, and the HTML shells |
| `internal/csp`, `internal/rawtoken`, `internal/pagecookie`, `internal/tokens`, `internal/auth` | The security surface — read docs/SECURITY.md first |
| `internal/pagedata`, `internal/templates`, `internal/uploads`, `internal/preflight` | Managed data, templates, staged uploads, deploy-time preflight |
| `internal/ops` | Operator tooling behind bootstrap, update, doctor and backup |
| `internal/testdb` | Per-test throwaway databases for the database suites |
| `embed.go` | Embeds `public/`, `migrations/`, `templates/` and `docs/` into the binary |
| `migrations/` | Ordered SQL. Append a new file; never edit an applied one |
| `public/shell-assets/` | First-party admin UI JavaScript and CSS (plain browser JS, no build) |
| `public/assets/flag/` | The Flag design system — **vendored and pinned by `flag.lock`; update with `scripts/flag-sync.sh sync`, never by hand**. `motion/` is the first-party Flag Motion kit, not Flag |
| `templates/` | Shipped page templates, registered by `pages template sync` |
| `scripts/` | The pre-binary install bridge (`bootstrap.sh`), the test cluster, the licence-date helper, and the shared `flag-sync.sh` / `stack-check.sh` |
| `deploy/` | systemd unit, Caddy site blocks, the login banner |
| `test/` | Front-end unit tests, the Playwright browser suite, the MCP wire golden |
| `docs/` | The reference documentation index is in the README |

## Docs and licence headers

- Every first-party source file carries an SPDX header. In Go it is the
  file's first two lines, before the package doc comment:
  ```go
  // SPDX-License-Identifier: BUSL-1.1
  // Copyright (c) 2026 ElcanoTek, Inc.
  ```
  Use the file's own comment syntax elsewhere (`#` in shell and SQL's `--`),
  keep a shebang on line 1 and insert after it. New files need one. Do not add
  one to the vendored Flag files under `public/assets/flag/` (the first-party
  `motion/` kit there has one), to `.github/workflows/flag-drift.yml` (shared
  verbatim across Elcano repos) or to the generated `flag.lock`.
  `scripts/flag-sync.sh` and `scripts/stack-check.sh` are vendored MIT copies
  of Flag's `tools/product/`, kept byte-identical, with their own MIT header —
  never edit it; generated Go files (such as
  `internal/preflight/shadowed_names.go`) get theirs from their generator.
- Go code is `gofmt`ed and passes `go vet` and `golangci-lint`. Errors are
  returned, not panicked; `ctx context.Context` comes first; exported names
  have doc comments. Keep the *why* in comments — they are the project's
  memory.
- If you change behaviour that `docs/` describes, update the doc in the same
  pull request.
- Do not add badges to the README.
- Classify new migrations in `migrations/compatibility.json`: use
  `backward-compatible` only when the predecessor can keep running and be
  restored after the migration. Other/unknown migrations require a planned
  manual upgrade; automatic code rollback never reverses database changes.
