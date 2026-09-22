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

// The fixture reexecutes the real pages_doctor function with a shimmed PATH
// (the update.test.js pattern): systemctl/curl/git/openssl/stat/readlink are
// fake binaries driven by files under the fixture dir, so a run of the doctor
// can be asserted end to end without a box, root, systemd or a database.
function fixture(options) {
  const { args, envFile = "present", statPerms, runningDir, active = 1 } = options;
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
  if (envFile === "present" || envFile === "shared-domain") {
    fs.writeFileSync(envFilePath, [
      'PORT="4312"',
      'DASHBOARD_HOST="pages.contoso.example"',
      'CONTENT_HOST="contoso-pages.example"',
      `AUTH_SIGNING_PUBKEY="${Buffer.alloc(32, 97).toString("base64")}"`,
      `PAGE_COOKIE_SECRET="${"0123456789abcdef".repeat(4)}"`,
      `RAW_TOKEN_SECRET="${"abcdef0123456789".repeat(4)}"`,
      `API_TOKEN_PEPPER="${"aaaaaaaabbbbbbbb".repeat(4)}"`,
      'DATABASE_URL="postgres://pages:contoso@127.0.0.1:5432/pages?sslmode=disable"',
      "",
    ].join("\n"), { mode: 0o640 });
  }
  if (envFile === "shared-domain") {
    fs.writeFileSync(envFilePath, fs.readFileSync(envFilePath, "utf8").replace("contoso-pages.example", "cdn.pages.contoso.example"));
  }
  fs.writeFileSync(osReleasePath, 'ID=fedora\nVERSION_ID="42"\nSUPPORT_END="2030-12-31"\n');
  const write = (name, text) => { fs.writeFileSync(path.join(bin, name), `#!/usr/bin/env bash\nset -eu\n${text}\n`, { mode: 0o755 }); };
  write("node", 'if [[ "${1:-}" == "-v" ]]; then echo v22.12.0; else exit 0; fi');
  write("npm", "exit 0");
  write("psql", "echo 1");
  write("dnf", "exit 0");
  write("rpm", 'printf "%s\\n" "6.9.0-100.fc40.x86_64"');
  write("uname", '[[ "${1:-}" == "-r" ]] && { printf "%s\\n" "6.9.0-100.fc40.x86_64"; exit 0; }; command -p uname "$@"');
  write("git", 'case "$*" in *--abbrev-ref*) echo main ;; *rev-list\\ --count*) echo 0 ;; *status\\ --porcelain*) : ;; *) exit 0 ;; esac');
  write("curl", '[[ "$*" == *"127.0.0.1:4312/readyz"* ]]');
  write("caddy", 'case "$1" in version) echo "v2.8.4" ;; *) exit 0 ;; esac');
  write("openssl", 'case "$*" in *s_client*) cat >/dev/null 2>&1 || true; printf "%s\\n" "-----BEGIN CERTIFICATE-----" "Y29udG9zbwo=" "-----END CERTIFICATE-----" ;; *x509*) echo "notAfter=Jan 15 12:00:00 2027 GMT" ;; *) exit 0 ;; esac');
  write("stat", `case "$*" in *"${envFilePath}"*) printf "%s\\n" "\${DOCTOR_TEST_STAT_PERMS:-640 root:\${DOCTOR_TEST_APP_USER}}" ;; *) command -p stat "$@" ;; esac`);
  write("readlink", 'case "${@: -1}" in /proc/*/cwd) printf "%s\\n" "$DOCTOR_TEST_RUNNING_DIR" ;; *) command -p readlink "$@" ;; esac');
  write("chown", 'printf "chown %s\\n" "$*" >> "$DOCTOR_TEST_LOG"; exit 0');
  write("chmod", 'printf "chmod %s\\n" "$*" >> "$DOCTOR_TEST_LOG"; command -p chmod "$@"');
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
  };
  delete env.NODE_TEST_CONTEXT;
  delete env.PAGES_PORT;
  const result = spawnSync("bash", ["-c", '. "$1"; shift; pages_doctor "$@"', "_", path.join(root, "scripts/doctor.sh"), ...args],
    { env, encoding: "utf8", timeout: 30000 });
  const output = `${result.stdout}${result.stderr}`;
  const actions = fs.existsSync(log) ? fs.readFileSync(log, "utf8") : "";
  return { dir, result, output, actions };
}

test("doctor --check on a healthy box reports healthy, changes nothing", () => {
  const { dir, result, output, actions } = fixture({ args: ["--check"] });
  try {
    assert.equal(result.status, 0, output);
    assert.match(output, /box healthy/);
    assert.match(output, /\/readyz/);
    assert.equal(actions, "");
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("doctor without --check repairs env perms, a stopped service and a stale release", () => {
  const stopped = fixture({ args: [], statPerms: "600 contoso:contoso", runningDir: "previous", active: 0 });
  try {
    assert.equal(stopped.result.status, 0, stopped.output);
    assert.match(stopped.output, /box repaired/);
    assert.match(stopped.actions, /start pages\.service/);
    assert.match(stopped.actions, /restart pages\.service/);
    assert.match(stopped.actions, /chown /);
    assert.match(stopped.actions, /chmod /);
  } finally { fs.rmSync(stopped.dir, { recursive: true, force: true }); }
});

test("doctor --check flags a missing env file and a broken trust split", () => {
  const missing = fixture({ args: ["--check"], envFile: "missing" });
  try {
    assert.equal(missing.result.status, 1, missing.output);
    assert.match(missing.output, /missing — run scripts\/bootstrap\.sh/);
  } finally { fs.rmSync(missing.dir, { recursive: true, force: true }); }
  const split = fixture({ args: ["--check"], envFile: "shared-domain" });
  try {
    assert.equal(split.result.status, 1, split.output);
    assert.match(split.output, /shares a registrable domain/);
  } finally { fs.rmSync(split.dir, { recursive: true, force: true }); }
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
  } finally { fs.rmSync(dry.dir, { recursive: true, force: true }); }
});
