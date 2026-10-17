// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Pins scripts/lib/key-only-ssh.sh (bootstrap's key-only SSH step) against
// stubbed sshd/systemctl and a scratch root: it writes the drop-in only when an
// authorized key exists, is idempotent, reverts on a failed `sshd -t`, warns
// when an earlier directive still wins, and never fails the caller.
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const os = require("node:os");
const { spawnSync } = require("node:child_process");
const LIB = path.join(__dirname, "..", "scripts/lib/key-only-ssh.sh");
const CONF = "etc/ssh/sshd_config.d/01-no-password-login.conf";

function fixture() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pages-ssh-"));
  const bin = path.join(dir, "bin"), root = path.join(dir, "root"), log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  fs.mkdirSync(path.join(root, "etc/ssh/sshd_config.d"), { recursive: true });
  fs.mkdirSync(path.join(root, "root/.ssh"), { recursive: true });
  fs.mkdirSync(path.join(root, "home/contoso"), { recursive: true });
  const stub = (name, body) => fs.writeFileSync(path.join(bin, name),
    `#!/usr/bin/env bash\nprintf '${name} %s\\n' "$*" >> ${JSON.stringify(log)}\n${body}\n`, { mode: 0o755 });
  stub("sshd", [
    'case "$1" in',
    '  -t) exit "${STUB_T_RC:-0}" ;;',
    '  -T) echo "authorizedkeysfile .ssh/authorized_keys /etc/ssh/keys/%u"; echo "passwordauthentication ${STUB_PA:-no}"; echo "pubkeyauthentication ${STUB_PUBKEY:-yes}" ;;',
    "esac",
  ].join("\n"));
  stub("systemctl", 'exit "${STUB_RELOAD_RC:-0}"');
  const run = (env = {}) => {
    const r = spawnSync("bash", ["-c", `set -euo pipefail
. "$1"
info() { printf 'INFO %s\\n' "$*"; }
ok()   { printf 'OK %s\\n' "$*"; }
warn() { printf 'WARN %s\\n' "$*"; }
ssh_key_only
printf 'RC=%s\\n' "$?"`, "ssh", LIB], {
      encoding: "utf8",
      env: { PATH: `${bin}:/usr/bin:/bin`, SSH_KEY_ONLY_ROOT: root, ...env },
    });
    const calls = fs.existsSync(log) ? fs.readFileSync(log, "utf8") : "";
    fs.rmSync(log, { force: true });
    return { out: r.stdout + r.stderr, calls };
  };
  const conf = path.join(root, CONF);
  const addKey = () => {
    fs.mkdirSync(path.join(root, "etc/ssh/keys"), { recursive: true });
    fs.writeFileSync(path.join(root, "etc/ssh/keys/contoso"), '# ssh-ed25519 AAAAold retired\nrestrict,from="10.0.0.0/8" ssh-ed25519 AAAAC3Nza contoso@northwind\n');
  };
  return { dir, root, conf, run, addKey };
}

test("no authorized key: warns and leaves sshd alone", () => {
  const f = fixture();
  const { out, calls } = f.run();
  assert.match(out, /WARN no SSH authorized keys found/);
  assert.match(out, /RC=0/);
  assert.equal(fs.existsSync(f.conf), false);
  assert.doesNotMatch(calls, /systemctl|sshd -t/);
});

test("with a key (via AuthorizedKeysFile %u): writes 0600 drop-in, validates, reloads; then idempotent", () => {
  const f = fixture();
  f.addKey();
  let { out, calls } = f.run();
  assert.match(out, /OK SSH is key-only/);
  assert.match(calls, /sshd -t\n/);
  assert.match(calls, /systemctl reload sshd/);
  const body = fs.readFileSync(f.conf, "utf8");
  assert.match(body, /^PasswordAuthentication no$/m);
  assert.match(body, /^KbdInteractiveAuthentication no$/m);
  assert.match(body, /^PermitRootLogin prohibit-password$/m);
  assert.equal(fs.statSync(f.conf).mode & 0o777, 0o600);
  ({ out, calls } = f.run());
  assert.equal(out.trim(), "RC=0");
  assert.doesNotMatch(calls, /systemctl|sshd -t/);
  // Identical file: no write or reload, but an earlier winning directive is still reported.
  ({ out, calls } = f.run({ STUB_PA: "yes" }));
  assert.match(out, /WARN sshd still accepts passwords/);
  assert.doesNotMatch(calls, /systemctl|sshd -t/);
});

test("sshd -t failure restores the previous file and only warns", () => {
  const f = fixture();
  f.addKey();
  fs.writeFileSync(f.conf, "PasswordAuthentication no\n");
  const { out, calls } = f.run({ STUB_T_RC: "1" });
  assert.match(out, /WARN sshd -t rejected/);
  assert.match(out, /RC=0/);
  assert.equal(fs.readFileSync(f.conf, "utf8"), "PasswordAuthentication no\n");
  assert.doesNotMatch(calls, /systemctl/);
});

test("a commented-out key does not count", () => {
  const f = fixture();
  fs.mkdirSync(path.join(f.root, "etc/ssh/keys"), { recursive: true });
  fs.writeFileSync(path.join(f.root, "etc/ssh/keys/contoso"), "# ssh-ed25519 AAAAC3Nza contoso@northwind\n");
  assert.match(f.run().out, /WARN no SSH authorized keys found/);
  assert.equal(fs.existsSync(f.conf), false);
});

test("PubkeyAuthentication no: warns and leaves sshd alone", () => {
  const f = fixture();
  f.addKey();
  const { out, calls } = f.run({ STUB_PUBKEY: "no" });
  assert.match(out, /WARN sshd has PubkeyAuthentication no/);
  assert.equal(fs.existsSync(f.conf), false);
  assert.doesNotMatch(calls, /systemctl|sshd -t/);
});

test("an earlier directive that still allows passwords is reported", () => {
  const f = fixture();
  f.addKey();
  const { out } = f.run({ STUB_PA: "yes" });
  assert.match(out, /WARN sshd still accepts passwords: an earlier directive wins/);
});

test("PAGES_KEEP_SSH_PASSWORDS=1 skips; no sshd_config.d is silent", () => {
  const f = fixture();
  f.addKey();
  let { out } = f.run({ PAGES_KEEP_SSH_PASSWORDS: "1" });
  assert.match(out, /INFO PAGES_KEEP_SSH_PASSWORDS=1/);
  assert.equal(fs.existsSync(f.conf), false);
  fs.rmSync(path.join(f.root, "etc/ssh/sshd_config.d"), { recursive: true });
  ({ out } = f.run());
  assert.equal(out.trim(), "RC=0");
});
