# Security policy

Pages serves password-protected client dashboards and renders untrusted,
agent-authored HTML. Both make it a security-relevant piece of software, and we
would much rather hear about a problem from you than from an incident.

**This file is the reporting policy.** The technical threat model — what the
system enforces, why, and the follow-ups we already know about — lives in
[`docs/SECURITY.md`](docs/SECURITY.md). Read that first if you are trying to
understand the design; read this one if you have something to report.

## Reporting a vulnerability

Email **security@elcanotek.com**.

Please do **not** open a public GitHub issue, pull request, or discussion for a
suspected vulnerability. A public report starts a clock we cannot control.

Include as much of the following as you have:

- What you found, and what an attacker gets out of it.
- The affected component — a file path (`lib/rawtoken.js`, `lib/csp.js`, …), a
  route, or an MCP tool name.
- Reproduction steps against a local `scripts/dev.sh` instance, ideally with the
  commit SHA you tested.
- Whether you believe it is remotely exploitable, and by whom (anonymous
  internet, a password-holding client, a partner-portal holder, a bearer-token
  agent, or a signed-in staff admin).
- Anything you know about exposure in the wild.

If you need to send something sensitive and want encryption, say so in a first
email with no details and we will arrange a channel.

## Response expectations

| Stage | Target |
| --- | --- |
| Acknowledgement that a human has read your report | 3 business days |
| Initial assessment — accepted, needs info, or not a vulnerability | 10 business days |
| Fix or documented mitigation for a confirmed critical/high issue | 30 days |
| Fix or plan for a confirmed medium/low issue | 90 days |

We will tell you which severity we assigned and why. If we disagree with your
assessment, we will say so plainly rather than letting the report go quiet. If
a fix is going to slip past these targets, we will tell you before it does.

We do not run a paid bug bounty. We will credit you in the release notes and
the commit if you want to be credited, and we will respect a request to stay
anonymous.

## Coordinated disclosure

Please give us **90 days** from acknowledgement before publishing, or until a
fix ships, whichever is sooner. If you tell us you intend to publish sooner, we
will work to that date instead of arguing about it. We will not ask you to stay
quiet indefinitely.

## Scope

**In scope** — anything in this repository:

- The two-host trust split, and any way to make agent HTML execute on the
  trusted dashboard origin, or to reach the SSO cookie from the content host.
- Bypassing a page's client password, a partner portal's shared password, or
  the staff-only default — including through `/raw` tokens, the `/view` broker,
  page-session cookies, or portal membership.
- Forging or replaying a `/raw` render token, a page-session cookie, a CSRF
  token, an agent bearer token, or an upload ticket.
- Escaping the content-host sandbox CSP, or getting a token added to the
  sandbox allow-list that should not be there.
- Privilege escalation across the authority split — an agent bearer token
  performing an admin-only action (approve, disable, clear a password, any
  partner-portal mutation), or a non-staff SSO session reaching `/admin`.
- Breaking an append-only or pointer-is-truth invariant: mutating stored version
  content, deleting a version, or moving the live pointer outside the state
  machine.
- SQL injection, authentication bypass, SSRF, or path traversal anywhere in
  `lib/`, `server.js`, `migrations/`, or the MCP tool surface.
- Denial of service that a single unauthenticated request can trigger:
  unbounded CPU in schema/regex validation, unbounded memory in upload or
  render paths, exhausting the database pool.
- Secrets leaking into logs, error pages, audit rows, `pages env` output, or
  MCP tool responses.
- The deploy path in `scripts/` and `deploy/` — privilege escalation from the
  `pages` service user, a world-readable secret, an unsafe systemd or Caddy
  directive.

**Out of scope:**

- Vulnerabilities in third-party dependencies with no exploitable path through
  Pages. Report those upstream; tell us if Pages makes one reachable.
- Findings that require an attacker to already hold a valid staff admin session
  or a valid agent bearer token, where the "impact" is the authority that
  credential legitimately carries.
- Issues that only appear with `PAGES_DEV_LOGIN=1`, `COMPOSE_DRIVER`/`PAGES_COMPOSE`
  enabled, or other documented development-only settings. These are documented
  as never-in-production; a report that one of them is dangerous in production
  is a documentation issue, not a vulnerability.
- Missing hardening headers on a deployment that does not front Pages with the
  supplied Caddy configuration.
- Weak client passwords chosen by an operator, or a client sharing their own
  password.
- Automated scanner output with no demonstrated impact, best-practice
  checklists, and reports about the absence of a feature (rate-limit tuning,
  CAPTCHA, MFA) rather than a defect.
- Social engineering, physical access, and anything requiring compromise of the
  upstream SSO service.
- The known follow-ups already listed under "Known follow-ups" in
  [`docs/SECURITY.md`](docs/SECURITY.md). Telling us you found a *worse*
  consequence of one of those than we documented is very much in scope.

## Testing guidance

Test against your own local instance (`sudo bash scripts/dev.sh`), not against
any deployment you do not own. Do not test against `pages.elcanotek.com` or
`elcano-pages.com`. Never use real client data, and do not attempt to access,
modify or exfiltrate anyone else's data — if you believe you *could*, describe
the path instead of walking it.

Research conducted in good faith under this policy is welcome, and we will not
pursue action over it.

## Supported versions

Pages is pre-1.0 and ships from `main`. Security fixes land on `main`; there are
no maintained release branches. If you run Pages, track `main` and see
[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) for the update path.

Note that this is a source-available project under BUSL-1.1 with no production
use grant — see [`docs/LICENSING.md`](docs/LICENSING.md). A security report is
welcome regardless of whether you hold a commercial licence.
