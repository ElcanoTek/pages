// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { spawnSync } = require("node:child_process");
const root = path.join(__dirname, "..");

// CodeQL: these scripts are handed to bash by absolute path. Validate the
// path against the same character class render-install.js enforces so the
// spawned shell command stays controlled (the values come from __dirname,
// but the check is what makes that provable).
function controlledScript(relative) {
  const resolved = path.join(root, relative);
  if (!/^\/[A-Za-z0-9_./-]+$/.test(resolved)) {
    throw new Error(`refusing uncontrolled script path: ${resolved}`);
  }
  return resolved;
}
const DOCTOR_SCRIPT = controlledScript("scripts/doctor.sh");
const CLI_SCRIPT = controlledScript("deploy/pages-cli");

// Escape helpers for the few places a value is embedded into generated shell
// code or a regular expression. Centralized because hand-rolled escaping
// repeatedly gets one case wrong (CodeQL js/incomplete-sanitization): in a
// DOUBLE-quoted shell string the backslash must be escaped BEFORE the quote —
// otherwise the quote-escape introduces a backslash that the first pass
// missed. Single-quoting sidesteps the ordering trap entirely, which is why
// shq is preferred wherever the syntax allows it.
//
// shq: lossless single-quoting for arbitrary values (`'\''` idiom).
function shq(value) {
  return `'${String(value).replace(/'/g, `'\\''`)}'`;
}
// dq: escape a value for a DOUBLE-quoted shell string — backslashes first.
function dq(value) {
  return String(value).replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}
// regexEscape: every regex metacharacter, backslash included — `\\` in the
// character class means a single pass handles backslashes correctly.
function regexEscape(value) {
  return String(value).replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// Static-looking fixture secrets. The leakage test asserts none of these
// values appears anywhere in doctor's output (key NAMES are fine — only
// values must never be printed). "Northwind"-style invented data, not real
// credentials.
const SECRETS = {
  pubkey: Buffer.alloc(32, 97).toString("base64"),
  pageCookie: "0123456789abcdef".repeat(4),
  rawToken: "abcdef0123456789".repeat(4),
  pepper: "aaaaaaaabbbbbbbb".repeat(4),
  dbPassword: "db-northwind-9f2c",
};
SECRETS.dbUrl = `postgres://pages:${SECRETS.dbPassword}@127.0.0.1:5432/pages?sslmode=disable`;

// The fixture certificate expires 45 days out, relative to the test run —
// a fixed date would start failing both TLS probes on that date.
const CERT_END = (() => {
  const d = new Date(Date.now() + 45 * 86400000);
  const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  const pad = (n) => String(n).padStart(2, "0");
  return `${months[d.getUTCMonth()]} ${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())} ${d.getUTCFullYear()} GMT`;
})();

// The fixture reexecutes the real pages_doctor function with a shimmed PATH
// (the update.test.js pattern): systemctl/curl/git/openssl/stat/readlink are
// fake binaries driven by files under the fixture dir, so a run of the doctor
// can be asserted end to end without a box, root, systemd or a database.
function fixture(options) {
  const {
    args, envFile = "present", statPerms, runningDir, active = 1,
    gitBehind = "0", gitRemoteSha = "abc1230", gitLsRemoteFail = false, readyzFails = 0,
    dbPassword = SECRETS.dbPassword,
  } = options;
  const dbUrl = `postgres://pages:${dbPassword}@127.0.0.1:5432/pages?sslmode=disable`;
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pages-doctor-"));
  const bin = path.join(dir, "bin");
  const state = path.join(dir, "state");
  const log = path.join(dir, "actions.log");
  const app = path.join(dir, "app");
  const release = path.join(dir, "release-current");
  const previous = path.join(dir, "release-previous");
  const source = path.join(dir, "source");
  const envFilePath = path.join(dir, "pages.env");
  const osReleasePath = path.join(dir, "os-release");
  const username = os.userInfo().username;
  fs.mkdirSync(bin);
  fs.mkdirSync(state);
  fs.mkdirSync(release);
  fs.mkdirSync(previous);
  fs.mkdirSync(path.join(source, ".git"), { recursive: true });
  fs.writeFileSync(path.join(state, "active"), String(active));
  fs.writeFileSync(path.join(release, "server.js"), "// contoso fixture release\n");
  fs.symlinkSync(release, app);
  fs.mkdirSync(path.join(source, "scripts"));
  fs.copyFileSync(path.join(root, "scripts/check-node.js"), path.join(source, "scripts/check-node.js"));
  fs.copyFileSync(DOCTOR_SCRIPT, path.join(source, "scripts/doctor.sh"));

  const hosts = {
    present: ["pages.contoso.example", "pages.northwind.example"],
    "shared-domain": ["pages.contoso.example", "cdn.pages.contoso.example"],
    // Different subdomains under the SAME co.uk domain: the naive
    // first-label comparison misses this pair, the PSL-aware one must not.
    "etld-same": ["pages.contoso.co.uk", "cdn.contoso.co.uk"],
    "bad-pubkey": ["pages.contoso.example", "pages.northwind.example"],
  }[envFile] || ["pages.contoso.example", "pages.northwind.example"];
  const pubkey = envFile === "bad-pubkey" ? `${SECRETS.pubkey}!` : SECRETS.pubkey;
  if (envFile !== "missing") {
    // Every interpolated value goes through dq: a backslash or quote in a
    // value must not corrupt the env file or change what env_get reads back.
    fs.writeFileSync(envFilePath, [
      'PORT="4312"',
      `DASHBOARD_HOST="${dq(hosts[0])}"`,
      `CONTENT_HOST="${dq(hosts[1])}"`,
      `AUTH_SIGNING_PUBKEY="${dq(pubkey)}"`,
      `PAGE_COOKIE_SECRET="${dq(SECRETS.pageCookie)}"`,
      `RAW_TOKEN_SECRET="${dq(SECRETS.rawToken)}"`,
      `API_TOKEN_PEPPER="${dq(SECRETS.pepper)}"`,
      `DATABASE_URL="${dq(dbUrl)}"`,
      // Sourcing canary: if anything ever evaluates this file as shell code
      // (the install-config sourcing bug), this line runs. It is not a
      // KEY= line, so the non-evaluating parser must ignore it.
      `touch ${shq(path.join(state, "pwned"))}`,
      "",
    ].join("\n"), { mode: 0o640 });
  }
  fs.writeFileSync(osReleasePath, 'ID=fedora\nVERSION_ID="42"\nSUPPORT_END="2030-12-31"\n');
  const write = (name, text) => { fs.writeFileSync(path.join(bin, name), `#!/usr/bin/env bash\nset -eu\n${text}\n`, { mode: 0o755 }); };
  write("node", 'if [[ "${1:-}" == "-v" ]]; then echo v22.12.0; else exit 0; fi');
  write("npm", "exit 0");
  write("psql", 'printf "psql %s\\n" "$*" >> "$DOCTOR_TEST_LOG"; echo 1');
  write("dnf", "exit 0");
  write("rpm", 'printf "%s\\n" "6.9.0-100.fc40.x86_64"');
  write("uname", 'if [[ "${1:-}" == "-r" ]]; then printf "%s\\n" "6.9.0-100.fc40.x86_64"; exit 0; fi; command -p uname "$@"');
  // fetch stays shammed (and logged) so any regression back to a mutating
  // freshness check is caught by asserting its absence from the log.
  write("git", [
    'case "$*" in',
    '  *fetch*)',
    '    printf "git-fetch\\n" >> "$DOCTOR_TEST_LOG"',
    '    if [[ "${DOCTOR_TEST_GIT_FETCH_FAIL:-0}" == 1 ]]; then exit 1; fi',
    "    exit 0 ;;",
    '  *ls-remote*)',
    '    printf "git-ls-remote\\n" >> "$DOCTOR_TEST_LOG"',
    '    if [[ "${DOCTOR_TEST_GIT_LSREMOTE_FAIL:-0}" == 1 ]]; then exit 1; fi',
    '    printf "%s\\trefs/heads/main\\n" "${DOCTOR_TEST_GIT_REMOTE_SHA:-abc1230}" ;;',
    "  *--abbrev-ref*) echo main ;;",
    '  *rev-list\\ --count*) printf "%s\\n" "${DOCTOR_TEST_GIT_BEHIND:-0}" ;;',
    '  *rev-parse*) printf "%s\\n" "${DOCTOR_TEST_GIT_HEAD_SHA:-abc1230}" ;;',
    "  *status\\ --porcelain*) : ;;",
    "  *) exit 0 ;;",
    "esac",
  ].join("\n"));
  write("curl", [
    'if [[ "$*" == *"127.0.0.1:4312/readyz"* ]]; then',
    '  n="$(cat "$DOCTOR_TEST_STATE/readyz_calls" 2>/dev/null || echo 0)"',
    '  echo $((n + 1)) > "$DOCTOR_TEST_STATE/readyz_calls"',
    '  if (( n >= ${DOCTOR_TEST_READYZ_FAILS:-0} )); then exit 0; fi',
    "  exit 1",
    "fi",
    "exit 1",
  ].join("\n"));
  write("caddy", 'case "$1" in version) echo "v2.8.4" ;; *) exit 0 ;; esac');
  write("openssl", 'case "$*" in *s_client*) cat >/dev/null 2>&1 || true; printf "%s\\n" "-----BEGIN CERTIFICATE-----" "Y29udG9zbwo=" "-----END CERTIFICATE-----" ;; *x509*) printf "notAfter=%s\\n" "$DOCTOR_TEST_CERT_END" ;; *) exit 0 ;; esac');
  write("stat", `case "$*" in *${shq(envFilePath)}*) printf "%s\\n" "\${DOCTOR_TEST_STAT_PERMS:-640 root:\${DOCTOR_TEST_APP_USER}}" ;; *) command -p stat "$@" ;; esac`);
  write("readlink", 'case "${@: -1}" in /proc/*/cwd) printf "%s\\n" "$DOCTOR_TEST_RUNNING_DIR" ;; *) command -p readlink "$@" ;; esac');
  write("chown", 'printf "chown %s\\n" "$*" >> "$DOCTOR_TEST_LOG"; exit 0');
  write("chmod", 'printf "chmod %s\\n" "$*" >> "$DOCTOR_TEST_LOG"; command -p chmod "$@"');
  write("sudo", 'printf "sudo %s\\n" "$*" >> "$DOCTOR_TEST_LOG"; if [[ "${1:-}" == "-u" ]]; then shift 2; if [[ "${1:-}" == "--" ]]; then shift; fi; fi; exec "$@"');
  write("systemctl", [
    'case "$1" in',
    "  is-active)",
    '    case "${@: -1}" in',
    "      postgresql.service|caddy.service) exit 0 ;;",
    '      *) [[ "$(cat "$DOCTOR_TEST_STATE/active" 2>/dev/null)" == 1 ]] ;;',
    "    esac ;;",
    "  is-enabled) exit 0 ;;",
    '  start)   printf "start %s\\n"   "${@: -1}" >> "$DOCTOR_TEST_LOG"; echo 1 > "$DOCTOR_TEST_STATE/active" ;;',
    '  restart) printf "restart %s\\n" "${@: -1}" >> "$DOCTOR_TEST_LOG"; echo 1 > "$DOCTOR_TEST_STATE/active" ;;',
    "  show) echo 4242 ;;",
    "  *) exit 0 ;;",
    "esac",
  ].join("\n"));
  const env = {
    ...process.env,
    PATH: `${bin}:${process.env.PATH}`,
    APP_DIR: app,
    APP_USER: username,
    PAGES_SRC_DIR: source,
    PAGES_ENV_FILE: envFilePath,
    PAGES_CLI_TARGET: path.join(dir, "pages-cli"),
    PAGES_INSTALL_CONFIG: path.join(dir, "install"),
    PAGES_OS_RELEASE: osReleasePath,
    DOCTOR_TEST_STATE: state,
    DOCTOR_TEST_LOG: log,
    DOCTOR_TEST_APP_USER: username,
    DOCTOR_TEST_STAT_PERMS: statPerms || "",
    DOCTOR_TEST_RUNNING_DIR: runningDir === "previous" ? previous : release,
    DOCTOR_TEST_GIT_BEHIND: gitBehind,
    DOCTOR_TEST_GIT_HEAD_SHA: "abc1230",
    DOCTOR_TEST_GIT_REMOTE_SHA: gitRemoteSha,
    DOCTOR_TEST_GIT_LSREMOTE_FAIL: gitLsRemoteFail ? "1" : "0",
    DOCTOR_TEST_GIT_FETCH_FAIL: "0",
    DOCTOR_TEST_CERT_END: CERT_END,
    DOCTOR_TEST_READYZ_FAILS: String(readyzFails),
  };
  delete env.NODE_TEST_CONTEXT;
  delete env.PAGES_PORT;
  const result = spawnSync("bash", ["-c", '. "$1"; shift; pages_doctor "$@"', "_", DOCTOR_SCRIPT, ...args],
    { env, encoding: "utf8", timeout: 60000 });
  const output = `${result.stdout}${result.stderr}`;
  const actions = fs.existsSync(log) ? fs.readFileSync(log, "utf8") : "";
  const pwned = fs.existsSync(path.join(state, "pwned"));
  return { dir, env, result, output, actions, pwned, state };
}

test("doctor --check on a healthy box reports healthy, changes nothing", () => {
  const { dir, result, output, actions, pwned } = fixture({ args: ["--check"] });
  try {
    assert.equal(result.status, 0, output);
    assert.match(output, /box healthy/);
    assert.match(output, /\/readyz/);
    assert.match(output, /current with origin\/main/);
    assert.match(output, /registrable domain \(northwind\.example\) differs from the dashboard's \(contoso\.example\)/);
    // freshness came from ls-remote, never a mutating fetch
    assert.match(actions, /git-ls-remote\n/);
    assert.ok(!actions.includes("git-fetch"), actions);
    // read-only: no repair actions of any kind, and no file was sourced
    assert.ok(!/(start|restart|chown|chmod) /.test(actions), actions);
    assert.equal(pwned, false, "doctor executed content of the env file");
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("doctor without --check repairs env perms, a stopped service and a stale release — and never sources anything", () => {
  const stopped = fixture({ args: [], statPerms: "600 contoso:contoso", runningDir: "previous", active: 0 });
  try {
    assert.equal(stopped.result.status, 0, stopped.output);
    assert.match(stopped.output, /box repaired/);
    assert.match(stopped.actions, /start pages\.service/);
    assert.match(stopped.actions, /restart pages\.service/);
    assert.match(stopped.actions, /chown /);
    assert.match(stopped.actions, /chmod /);
    // root repair mode must still never evaluate file content as shell code
    assert.equal(stopped.pwned, false, "doctor executed content of the env file");
  } finally { fs.rmSync(stopped.dir, { recursive: true, force: true }); }
});

test("doctor --check flags a missing env file, a shared domain, and a shared eTLD+1 across subdomains", () => {
  const missing = fixture({ args: ["--check"], envFile: "missing" });
  try {
    assert.equal(missing.result.status, 1, missing.output);
    assert.match(missing.output, /missing — run scripts\/bootstrap\.sh/);
  } finally { fs.rmSync(missing.dir, { recursive: true, force: true }); }
  const split = fixture({ args: ["--check"], envFile: "shared-domain" });
  try {
    assert.equal(split.result.status, 1, split.output);
    assert.match(split.output, /shares the registrable domain contoso\.example/);
  } finally { fs.rmSync(split.dir, { recursive: true, force: true }); }
  // pages.contoso.co.uk vs contoso-pages.co.uk: the naive first-label
  // comparison passes this pair; the PSL-aware one must fail it.
  const etld = fixture({ args: ["--check"], envFile: "etld-same" });
  try {
    assert.equal(etld.result.status, 1, etld.output);
    assert.match(etld.output, /shares the registrable domain contoso\.co\.uk/);
  } finally { fs.rmSync(etld.dir, { recursive: true, force: true }); }
});

test("doctor rejects a pubkey whose base64 decodes only partially", () => {
  // GNU base64 -d prints the decoded prefix before erroring on the trailing
  // '!', so a byte-count-only check would accept this value.
  const bad = fixture({ args: ["--check"], envFile: "bad-pubkey" });
  try {
    assert.equal(bad.result.status, 1, bad.output);
    assert.match(bad.output, /not a base64 32-byte Ed25519 key/);
  } finally { fs.rmSync(bad.dir, { recursive: true, force: true }); }
});

test("doctor passes the configured DATABASE_URL to psql positionally", () => {
  const { dir, result, output, actions } = fixture({ args: ["--check"] });
  try {
    assert.equal(result.status, 0, output);
    assert.match(actions, new RegExp(`psql ${regexEscape(SECRETS.dbUrl)} -tAc SELECT 1`));
    assert.ok(!actions.includes("DATABASE_URL="), "probe used the env-var form psql ignores");
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("backslashes and quotes in a value survive the fixture's escaping end to end", () => {
  // Contains a double quote, a single quote, and a backslash in one value —
  // the combination hand-escaping gets wrong (backslashes must be escaped
  // before quotes; regexes must escape the backslash itself).
  const nasty = 'p"\'\\w0rd';
  // shq is lossless through a real shell.
  const sh = spawnSync("bash", ["-c", `printf %s ${shq(nasty)}`], { encoding: "utf8" });
  assert.equal(sh.stdout, nasty, `shq round-trip failed: ${JSON.stringify(sh.stdout)}`);
  // dq is lossless through a real double-quoted shell string.
  const dqs = spawnSync("bash", ["-c", `printf %s "${dq(nasty)}"`], { encoding: "utf8" });
  assert.equal(dqs.stdout, nasty, `dq round-trip failed: ${JSON.stringify(dqs.stdout)}`);
  // regexEscape matches the literal value embedded in surrounding text.
  assert.ok(new RegExp(regexEscape(nasty)).test(`psql postgres://pages:${nasty}@host/db -tAc SELECT 1`));

  // End to end through the fixture: the env file carries the dq-escaped
  // value; the production-style parser returns it without unescaping, and
  // that exact string is what doctor must hand to psql.
  const { dir, result, output, actions } = fixture({ args: ["--check"], dbPassword: nasty });
  try {
    assert.equal(result.status, 0, output);
    const asParserReturns = `postgres://pages:${dq(nasty)}@127.0.0.1:5432/pages?sslmode=disable`;
    assert.match(actions, new RegExp(`psql ${regexEscape(asParserReturns)} -tAc SELECT 1`));
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("doctor waits for readiness after repair-starting a stopped service", () => {
  // Service starts but /readyz needs 3 polls; without restarted=1 the
  // readiness loop gets one try and the run fails right after repairing.
  const slow = fixture({ args: [], active: 0, readyzFails: 3 });
  try {
    assert.equal(slow.result.status, 0, slow.output);
    assert.match(slow.output, /box repaired/);
    assert.match(slow.output, /\/readyz/);
    const calls = fs.readFileSync(path.join(slow.state, "readyz_calls"), "utf8").trim();
    assert.ok(Number(calls) > 1, `expected several readiness polls, got ${calls}`);
  } finally { fs.rmSync(slow.dir, { recursive: true, force: true }); }
});

test("doctor compares against origin with ls-remote — behind warns, failure never reads as current", () => {
  const behind = fixture({ args: ["--check"], gitRemoteSha: "def4567", gitBehind: "3" });
  try {
    assert.equal(behind.result.status, 0, behind.output); // behind is an advisory
    assert.match(behind.actions, /git-ls-remote\n/);
    assert.ok(!behind.actions.includes("git-fetch"), behind.actions);
    assert.match(behind.output, /3 commit\(s\) behind origin\/main — run: sudo pages update/);
    assert.ok(!behind.output.includes("current with origin/main"));
  } finally { fs.rmSync(behind.dir, { recursive: true, force: true }); }
  const unreachable = fixture({ args: ["--check"], gitRemoteSha: "def4567", gitLsRemoteFail: true });
  try {
    assert.equal(unreachable.result.status, 0, unreachable.output); // honest advisory
    assert.match(unreachable.output, /could not reach origin/);
    assert.ok(!unreachable.output.includes("behind origin/main"), unreachable.output);
    assert.ok(!unreachable.output.includes("current with origin/main"), unreachable.output);
  } finally { fs.rmSync(unreachable.dir, { recursive: true, force: true }); }
});

test("pages doctor --check dispatches as the service user, not root", () => {
  const { dir, env, output } = fixture({ args: ["--check"] });
  try {
    const result = spawnSync("bash", [CLI_SCRIPT, "doctor", "--check"], { env, encoding: "utf8", timeout: 60000 });
    const combined = `${result.stdout}${result.stderr}`;
    assert.equal(result.status, 0, combined);
    assert.match(combined, /box healthy/);
    const log = fs.readFileSync(env.DOCTOR_TEST_LOG, "utf8");
    assert.ok(
      log.split("\n").some((line) => line.startsWith(`sudo -u ${env.APP_USER} -- `)),
      `no service-user dispatch in:\n${log}`,
    );
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("doctor handles a fully-seeded secrets env file without crashing or leaking values", () => {
  // The pages analog of the explorer static-credentials crash: the secret-
  // bearing path must run clean, report normally, and never print a value.
  const { dir, result, output } = fixture({ args: ["--check"] });
  try {
    assert.equal(result.status, 0, output);
    for (const [name, value] of Object.entries(SECRETS)) {
      assert.ok(!output.includes(value), `doctor output leaked ${name}`);
    }
    // the expected standing advisory on this fixture (release feed unreachable)
    assert.match(output, /could not read the Fedora release feed/);
    // key NAMES are reported, values are not
    assert.match(output, /PAGE_COOKIE_SECRET set/);
    assert.match(output, /API_TOKEN_PEPPER set/);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("doctor --strict turns advisories into a failure; --dry-run changes nothing", () => {
  // The fixture's curl shim denies the Fedora release feed, so a healthy
  // --check still carries one advisory; --strict must fail on it.
  const strict = fixture({ args: ["--check", "--strict"] });
  try {
    assert.equal(strict.result.status, 1, strict.output);
    assert.match(strict.output, /PROBLEM\(S\)/);
  } finally { fs.rmSync(strict.dir, { recursive: true, force: true }); }
  const dry = fixture({ args: ["--dry-run"] });
  try {
    assert.equal(dry.result.status, 0, dry.output);
    assert.match(dry.output, /\[dry-run\]/);
    assert.equal(dry.actions, "");
    assert.equal(dry.pwned, false, "even --dry-run must not evaluate file content");
  } finally { fs.rmSync(dry.dir, { recursive: true, force: true }); }
});
