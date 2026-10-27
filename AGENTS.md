# Notes for coding agents

Short orientation for an AI coding agent working in this repository. Humans
should read [`CONTRIBUTING.md`](CONTRIBUTING.md) instead — it covers the same
ground properly.

<!-- BEGIN:elcano-frontend-standard v2 (shared verbatim by every Elcano repo; scripts/stack-check.sh pins its hash; change it everywhere or nowhere) -->
## Elcano frontend standard (v2)

Every Elcano web product follows one of two tiers. `stack.conf` at the repo
root declares which, and `scripts/stack-check.sh` (run by CI) enforces it.

**The server is Go and serves everything.** The browser talks to one origin:
the Go binary serves the HTML, the static assets and the JSON API, and it owns
sign-in (the `elcano_auth` cookie from the auth service), sessions, CSRF and
security headers. There is no Node, Python or other JavaScript server in
production. Static files ship inside the binary with `go:embed`; Caddy only
terminates TLS and reverse-proxies to Go.

**Tier A — server-rendered (the default).** Go `html/template` pages, plain
CSS that reads Flag tokens, and small plain-JavaScript modules (no framework,
no bundler, no transpiler, no npm runtime dependencies). npm may appear only
as a dev dependency for browser tests (Playwright, axe). Pick Tier A unless the
page is a long-lived, state-heavy application. Components use Flag's CSS
component classes, which mirror the names and states of Flag's shadcn/ui
components so both tiers look and behave alike.

**Tier B — interactive app.** React + TypeScript built by Vite into a static
bundle that the Go server embeds and serves (same origin, so no CORS). No
server-side rendering, no Next.js, no runtime server code in TypeScript; data
comes from the Go JSON API. Base UI components come from Flag's shadcn/ui
registry (React + Radix + Tailwind, themed by Flag tokens; registry URL in
Flag's README): `npx shadcn add` copies them into the app, where they live and
are reviewed like our own code. Build a component by hand only when the
registry lacks it, then upstream it to Flag. Keep other runtime npm
dependencies few and justified in AGENTS.md. Use Tier B only when Tier A would
mean re-implementing a client-side state framework by hand (streaming chat,
multi-step builders).

**Both tiers:**

- Design comes from Flag (`ElcanoTek/flag`). Vendored Flag files are listed in
  `flag.lock` with their hashes; `scripts/flag-sync.sh check` fails CI on any
  hand edit. Update with `scripts/flag-sync.sh sync`, never by editing in place.
  Product styling lives outside the vendored directories and reads tokens.
- No third-party CDNs: every script, stylesheet and font is served by our own
  binary. Fonts are Flag's Nebula Sans and Hack only.
- A strict Content-Security-Policy (no `unsafe-inline` scripts, no remote
  script hosts), `X-Content-Type-Options: nosniff`, and `frame-ancestors`.
- Hashed or versioned asset URLs with long-lived caching; HTML is `no-cache`.
- Accessibility is tested (Playwright + axe where a browser suite exists).
- Exceptions (a non-Go backend, an extra runtime dependency) are written down
  in this repo's AGENTS.md with the reason, not left implicit.
<!-- END:elcano-frontend-standard -->


### How Pages meets it

Pages is **Tier A** (`stack.conf`: `tier=A`, `npm=.`, `backend=go`). There is
no frontend build step:

- `web=` lists everything that serves markup, CSS or JS, so the no-CDN scan
  covers it: `public/`, `templates/`, and the Go packages that write HTML
  (`internal/shell` for the admin shell and gate pages, `internal/render` for
  the tags injected into themed pages, `internal/contentview` and
  `internal/httpx` for the content host's own documents and redirect bodies).
  A new Go package that emits HTML joins that list.
- The admin shell's HTML comes from Go (`internal/shell`); its CSS and plain
  browser JS are `public/shell-assets/*`, edited directly and embedded by
  `embed.go`. `node --test test/frontend.test.js` covers their pure halves.
- `public/assets/flag/` is the vendored Flag tree, served on the content host
  at `/assets/flag/` and on the dashboard at `/shell-assets/flag/`.
- `templates/` are the shipped customer-page templates. They are in `web=`
  because they load nothing remote either: client pages may not (see
  [`docs/AUTHORING.md`](docs/AUTHORING.md)), and the content-host CSP would
  block it. Pages that agents deploy are customer content, stored in the
  database, and outside the standard's no-CDN rule; the content-host sandbox
  CSP governs them.
- npm (`package.json`) holds devDependencies only: Playwright and axe.
- **Asset URLs are versioned** (`internal/assetver`). Every asset is embedded,
  and one content hash of `public/` is the version, carried as a path segment:
  `/shell-assets/_v/<version>/…` on the dashboard, `/assets/_v/<version>/…` on
  the content host. The shells, the gate pages and the Flag tags injected into
  themed pages all link that form, and it is served
  `Cache-Control: public, max-age=31536000, immutable` when the version is
  current. Because it is a path segment, relative URLs inherit it: the font
  files that vendored `fonts.css` names, and the sprite that `primitives.js`
  resolves against its own URL. Any other version (an open tab from the
  previous release) gets the current bytes, revalidating. Link new assets
  through `assetver.Shell()` / `assetver.ContentFlag()`, never a bare path.
  HTML is `no-store`: `httpx.SendHTML` sets it on every page (the dashboard
  shells carry the signed-in email and a CSRF token), and an asset error
  such as a 416 is `no-store` too, never the asset's immutable policy.
  `FLAG_ASSETS_BASE` may only be a same-origin path matching
  `^/[A-Za-z0-9._~/-]*$` without `//` or `..` (startup refuses anything else,
  and the renderer escapes it anyway; the content CSP is `'self'` and stays
  that way).
- **Scoped exception: the stable `/assets/flag/…` paths stay revalidating**
  (`public, no-cache` plus an ETag, so a cached copy costs a 304). Client
  pages hard-code them (the documented `/assets/flag/motion/…` kit, the token
  sheet in `raw` pages), and Pages cannot rewrite the stored source
  (invariant 2), so those URLs cannot carry a version; making them immutable
  would pin every client to the first release it saw. `/favicon.ico`
  redirects there for the same reason: a cached 301 must not name a version.
  Nothing user-published is served under `/assets`: it is the embedded
  `public/assets` tree only, and page content and uploads live in the
  database.
- Components (buttons, fields, toasts, dialogs) are product CSS in
  `public/shell-assets/shell.css` that reads Flag tokens and follows Flag's
  `COMPONENTS.md`. They predate Flag's `components/components.css`, which is
  not vendored yet: when a shell primitive is next substantially changed,
  vendor that sheet with `scripts/flag-sync.sh add` and move the primitive onto
  its classes rather than restyling it here.

## Run and test

```bash
make build                         # compile-check ./... and write ./pages
./pages dev                        # Postgres + migrate + token + server on :3099
./pages dev token                  # reprint the saved agent token

make test                          # go test ./... — run constantly
make test-db                       # throwaway Postgres on :5433 for the database suites
make test-race                     # what CI runs (plus vet, gofmt, golangci-lint)
make lint                          # golangci-lint run ./...
make stack-check                   # the frontend standard, offline (CI runs it first)
node --test test/frontend.test.js  # front-end unit tests
npm ci && npx playwright test      # browser suite against `pages browser-fixture`
```

`make ci` is stack-check + vet + lint + `test-race`. CI pins golangci-lint
v2.15.0; a golangci-lint built with a Go older than `go.mod`'s cannot load
this module, so install it with the module's toolchain
(`GOTOOLCHAIN=go1.27.3 go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0`).
If the Chromium on your machine is not the build this Playwright pins, point
`PLAYWRIGHT_CHROMIUM_EXECUTABLE` at it instead of downloading another.

In a Claude Code cloud session, `.claude/hooks/session-start.sh` does all of
this at start-up: `go mod download`, golangci-lint at CI's version, `npm
install`, `PLAYWRIGHT_CHROMIUM_EXECUTABLE` for the preinstalled Chromium (never
run `playwright install` there), and a test Postgres via
`make test-db TEST_PG_DIR=/var/tmp/pages-testpg`. Point the database suites at
it with `PAGES_TEST_DATABASE_URL=postgres://pages@127.0.0.1:5433/postgres?sslmode=disable`
(the default) and `PAGES_TEST_REQUIRE_DB=1`.

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
  licence headers; the first-party `motion/` kit there does carry one). Do not
  touch the header of `scripts/flag-sync.sh` or `scripts/stack-check.sh`: they
  are vendored MIT copies of Flag's `tools/product/` (their own
  `SPDX-License-Identifier: MIT` header names the canonical copy), kept
  byte-identical across Elcano repos. Nor add one to the other shared files
  (`.github/workflows/flag-drift.yml`, the standard block above), to
  `flag.lock` (generated: `flag-sync.sh` rewrites it whole, header included),
  or to `deploy/motd` (the login banner, installed verbatim
  as `/etc/motd`, so a header would print at every ssh login). Generated Go
  files such as `internal/preflight/shadowed_names.go` get theirs from the
  generator; regenerate, do not hand-edit.
- **`gofmt`, `go vet` and `golangci-lint` (`.golangci.yml`) must be clean.**
  A `//nolint` needs a specific linter and an explanation.
- **Never edit an applied migration.** Add a new numbered file in `migrations/`
  and classify it in `migrations/compatibility.json`.
- **`public/assets/flag/` is vendored from Flag.** Do not hand-edit it; see
  "Vendored Flag" below.
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

## Vendored Flag

Flag (`ElcanoTek/flag`, MIT) is the design system. `flag.lock` lists every
vendored file, the flag commit it came from and its sha256:
`public/assets/flag/{fonts,icons,theme,tokens}/` are locked whole (any
unlocked file in them fails the check), `logos/` file by file (only the two
Elcano marks Pages uses), and Flag's MIT licence text travels with the copies
as `public/assets/flag/FLAG-LICENSE` (NOTICE points at it). `flag-sync.sh`
refuses a Flag source with uncommitted changes, so the lock's commit always
reproduces its hashes. To update, with a flag checkout beside this one:

```bash
scripts/flag-sync.sh status ../flag   # what differs (make flag-status FLAG_DIR=../flag)
scripts/flag-sync.sh sync ../flag     # re-copy every locked file, rewrite flag.lock
scripts/flag-sync.sh add ../flag logos/<file>.svg public/assets/flag/logos/<file>.svg
```

then commit the files and `flag.lock` together, and run the browser suite: the
admin shell uses sprite ids from `icons/core-icons.svg` and the token names in
`tokens/design-tokens.css`. Never fix Flag here: a missing token, icon or a
wrong comment in a vendored file is a change to `ElcanoTek/flag`, synced back.
Product styling lives in `public/shell-assets/shell.css` and reads tokens.

The gates: `scripts/stack-check.sh` (`make stack-check`, the first CI job)
runs `scripts/flag-sync.sh check` offline on every PR, and
`TestEmbeddedFlagMatchesLock` (`embed_test.go`) checks what the binary embeds
against the lock. The weekly `Flag drift` workflow
(`.github/workflows/flag-drift.yml`) runs `status` against flag `main` and
goes red when upstream has moved; answer it with a sync PR.

**`public/assets/flag/motion/` is not Flag.** The Flag Motion kit
(`flag-motion.{css,js}`, [`docs/MOTION.md`](docs/MOTION.md)) is first-party
Pages code (BUSL, SPDX headers) that sits under `/assets/flag/motion/` because
deployed client pages load it from that URL. It is deliberately outside
`flag.lock`; edit it like any other Pages source, with
`test/frontend.test.js` and `test/browser/flag-motion.spec.js`.

## Dependencies

Prefer the standard library or a small piece of owned code to a new module;
add a dependency only when it clearly earns its place. Dependabot proposes
updates daily, after a cooldown of 7 days (3 for patch releases), with minor
and patch grouped ([`.github/dependabot.yml`](.github/dependabot.yml)); a human
merges one only when the bump earns it — a security fix whose code path we use,
a security scanner update (such as CodeQL), or a specific commit we need. The `go` directive in `go.mod` is moved by hand
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
