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

You need Node 18+ (Node 20 is what CI runs) and PostgreSQL **server** tools —
`initdb` and `pg_ctl`, not just `psql`. Everything else is npm.

```bash
git clone https://github.com/ElcanoTek/pages.git
cd pages
npm install
```

### Running it

Linux — one command stands up a throwaway Postgres cluster, migrates, mints an
agent token, mints a dev SSO keypair, and boots the server in the foreground:

```bash
sudo bash scripts/dev.sh
```

macOS, against a Homebrew `postgresql@16` you already have running:

```bash
bash scripts/dev-macos.sh
```

Both print the URLs and the agent token when they come up. State lives in
`.devdata/` (gitignored) and is shared between the two scripts, so the token
survives restarts and switching entrypoints. `bash scripts/dev.sh token` reprints
the saved token without booting anything.

The dashboard is `http://localhost:3099` and the content host is
`http://content.localhost:3099` — two hostnames, one process. Sign in to the
admin UI through the local dev-login route:

```
http://localhost:3099/__dev/login?next=/admin
```

That route only exists when `PAGES_DEV_LOGIN=1`, which only `dev.sh` and
`dev-macos.sh` set. It must never be set in production.

`scripts/dev.sh` needs root because Postgres refuses to run as root and the
script delegates the cluster to the `postgres` system user via `runuser`. If
`initdb` is not on your `PATH` (Debian and Ubuntu keep it in
`/usr/lib/postgresql/<ver>/bin`), add it before running:

```bash
export PATH="/usr/lib/postgresql/16/bin:$PATH"
```

## Tests and checks

There is no separate linter. CI (`.github/workflows/ci.yml`) runs exactly these,
in this order, and a pull request needs all of them green. CodeQL
(`.github/workflows/codeql.yml`) scans the JavaScript on every push and PR and
weekly; a new alert on your branch is yours to look at.

```bash
npm ci
npm audit --omit=dev --audit-level=high    # production tree, high/critical fail
npm test                                   # unit tests, no database
for f in scripts/*.sh test/*.sh deploy/pages-cli; do bash -n "$f"; done
npm run test:browser                       # Playwright: a11y + admin workflows
bash test/run-integration.sh               # full suite against a throwaway PG
```

What each one is for:

- **`npm test`** — pure unit tests over `lib/` (`test/unit.test.js`) plus the
  theme-token contract (`test/theme.test.js`). Fast, no database, run it
  constantly.
- **`npm run test:browser`** — drives the real admin shell in Chromium against
  `test/browser/fixture-server.js`, a fixture server with no database. Includes
  axe accessibility assertions. Set `PLAYWRIGHT_FIXTURE_PORT` to run two
  checkouts at once; without it, `reuseExistingServer` can point one checkout's
  specs at another's fixture server and give you a green run that proved
  nothing.
- **`bash test/run-integration.sh`** — spins up its own throwaway Postgres
  cluster, migrates, seeds, and runs the REST, MCP, admin, view, rate-limit and
  template-CLI suites end to end. Self-cleaning. This is the one that catches
  real breakage; run it before you open a pull request.

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
  changed anything in `lib/csp.js`, `lib/rawtoken.js`, `lib/pagecookie.js`,
  `lib/portals.js`, `lib/tokens.js` or the sandbox allow-list, say explicitly
  what the security consequence is.
- Squash-merge into `main` once CI is green.

## Invariants: do not break these

These are enforced in code and in tests, and they are the reason the system is
safe to point at a client. If your change needs one of them relaxed, that is
the conversation to have in the issue first.

1. **Untrusted content lives on a separate registrable domain.** The dashboard
   host is the trusted auth zone; the content host is cookieless and serves
   agent HTML under a strict sandbox CSP. Never render agent HTML on the
   dashboard origin. Enforced in `server.js` by the `Host` header.
2. **Edit the stored source, never the rendered DOM.** An iframe is a preview
   only. No `contenteditable` or `outerHTML` round-trip — they corrupt chart
   markup. Editing means patching the source into a new draft and re-rendering.
3. **Pointer-is-truth.** "Live" is `pages.published_version_id`. Do not add a
   `published` status. Publish, approve and rollback are one `UPDATE` under
   `SELECT … FOR UPDATE`.
4. **`page_versions` content is append-only**, enforced by a database trigger.
   Every edit is a new row; only `status` and the review columns mutate.
5. **`/raw` is authorised only by the signed token** (`lib/rawtoken.js`), bound
   to page, version, purpose, render mode and expiry, and verified in constant
   time. No cookies on that host. The sandbox CSP headers go on *every* `/raw`
   response, errors included.
6. **Agents never approve, disable, clear a password, or touch partner portals.**
   Those are admin-cookie-plus-CSRF only. `lib/versions.js` is the single state
   machine; REST, MCP and the admin UI all route through it, and every mutation
   writes an `audit_log` row in the same transaction.
7. **`allow-same-origin` must never join `allow-scripts`** in the content-host
   sandbox. It is a documented sandbox escape.

[`docs/SECURITY.md`](docs/SECURITY.md) explains the reasoning behind each of
these at length. Read it before touching the security surface.

## Where things are

| Path | What lives there |
| --- | --- |
| `server.js` | One process, two vhosts; route wiring and the host split |
| `lib/` | Everything else — state machine, auth, CSP, render, MCP, portals |
| `lib/versions.js` | The version state machine. The centre of the system |
| `migrations/` | Ordered, idempotent SQL. Append a new file; never edit an applied one |
| `public/shell-assets/` | First-party admin UI JavaScript and CSS |
| `public/assets/flag/` | The Flag design system — **vendored, do not hand-edit** |
| `templates/` | Shipped page templates, registered by `pages template sync` |
| `scripts/` | Dev, bootstrap, update, token and template CLIs |
| `deploy/` | systemd unit, Caddy site blocks, the `pages` operator CLI |
| `docs/` | The reference documentation index is in the README |

## Docs and licence headers

- Every first-party source file carries an SPDX header:
  ```js
  // SPDX-License-Identifier: BUSL-1.1
  // Copyright (c) 2026 ElcanoTek, Inc.
  ```
  Use the file's own comment syntax, keep a shebang on line 1 and insert after
  it. New files need one. Do not add one to anything under
  `public/assets/flag/` or to `lib/preflight-shadowed-names.js`, which is
  generated.
- If you change behaviour that `docs/` describes, update the doc in the same
  pull request.
- Do not add badges to the README.
