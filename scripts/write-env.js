// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
const fs = require("node:fs");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const MANAGED = ["PORT", "DASHBOARD_HOST", "CONTENT_HOST", "DASHBOARD_ORIGIN", "CONTENT_ORIGIN", "AUTH_SIGNING_PUBKEY", "AUTH_COOKIE_NAME", "AUTH_LOGIN_URL", "ADMIN_EMAIL_DOMAIN", "PAGE_COOKIE_SECRET", "RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "DATABASE_URL"];

function quote(value) {
  const text = String(value);
  if (/[\0\r\n]/.test(text)) throw new Error("bootstrap-managed environment values must be single-line text");
  return '"' + text.replace(/[\\"$`]/g, "\\$&") + '"';
}

// Keep unknown assignments/comments byte-for-byte, including quoted multiline
// values. Only records beginning with an explicitly managed assignment change.
function records(text) {
  const output = [];
  let start = 0, quoted = "", escaped = false, comment = false;
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (escaped) { escaped = false; continue; }
    if (!comment && c === "\\" && quoted !== "'") { escaped = true; continue; }
    if (!comment && (c === "'" || c === '"')) {
      if (!quoted) quoted = c; else if (quoted === c) quoted = "";
    }
    if (!quoted && c === "#") comment = true;
    if (c === "\n" && !quoted) { output.push(text.slice(start, i + 1)); start = i + 1; comment = false; }
  }
  if (start < text.length) output.push(text.slice(start));
  return output;
}

function merge(original, updates) {
  const pending = new Set(Object.keys(updates));
  for (const key of pending) if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(key)) throw new Error("invalid environment key");
  let result = records(original).map((record) => {
    const match = /^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=/.exec(record);
    if (!match || !Object.hasOwn(updates, match[1])) return record;
    pending.delete(match[1]);
    return `${match[1]}=${quote(updates[match[1]])}\n`;
  }).join("");
  if (result && !result.endsWith("\n")) result += "\n";
  for (const key of pending) result += `${key}=${quote(updates[key])}\n`;
  const check = spawnSync("bash", ["-n"], { input: result, encoding: "utf8" });
  if (check.status !== 0) throw new Error("generated environment is not valid shell syntax; original file retained");
  return result;
}

function writeEnv(filename, updates) {
  const exists = fs.existsSync(filename);
  const original = exists ? fs.readFileSync(filename, "utf8") : "";
  const stat = exists ? fs.statSync(filename) : null;
  const next = merge(original, updates); // finish validation before any mutation
  const pending = [];
  function stage(target, value) {
    const temporary = `${target}.${process.pid}.tmp`;
    pending.push(temporary);
    const fd = fs.openSync(temporary, "wx", 0o600);
    try {
      fs.writeFileSync(fd, value);
      if (stat) fs.fchownSync(fd, stat.uid, stat.gid);
      fs.fchmodSync(fd, stat ? stat.mode & 0o777 : 0o640);
      fs.fsyncSync(fd);
    } finally { fs.closeSync(fd); }
    return temporary;
  }
  try {
    const candidate = stage(filename, next);
    if (exists) fs.renameSync(stage(`${filename}.previous`, original), `${filename}.previous`);
    fs.renameSync(candidate, filename);
    const directory = fs.openSync(path.dirname(filename), "r");
    try { fs.fsyncSync(directory); } finally { fs.closeSync(directory); }
  } finally {
    for (const file of pending) { try { fs.unlinkSync(file); } catch (error) { if (error.code !== "ENOENT") throw error; } }
  }
}

if (require.main === module) {
  try {
    const updates = Object.fromEntries(MANAGED.map((key) => {
      if (process.env[key] === undefined) throw new Error(`missing bootstrap value: ${key}`);
      return [key, process.env[key]];
    }));
    writeEnv(process.argv[2], updates);
  } catch (error) { console.error(error.message); process.exitCode = 1; }
}
module.exports = { merge, writeEnv };
