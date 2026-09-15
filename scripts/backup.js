// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
require("./check-node").assertSupported();

const fs = require("node:fs/promises");
const path = require("node:path");
const crypto = require("node:crypto");
const { spawn } = require("node:child_process");
const { createReadStream, constants } = require("node:fs");
const ROOT = path.resolve(__dirname, "..");
const EXCLUDED = new Set([".git", "node_modules", "assets", ".env", ".devdata", "test-results", "playwright-report"]);
const abort = new AbortController();
const RUNTIME_SETTING = /^(PAGES_DATA_[A-Z_]+|PAGES_MCP_[A-Z_]+|PAGES_SOURCE_FUTURE_TOLERANCE_MS|MAX_HTML_BYTES)$/;
const runtimeSettings = (env) => Object.fromEntries(Object.entries(env).filter(([key, value]) => RUNTIME_SETTING.test(key) && typeof value === "string"));

async function run(command, args, { env = process.env, cleanup = false, outputFile } = {}) {
  if (!cleanup) abort.signal.throwIfAborted();
  const output = outputFile ? await fs.open(outputFile, "wx", 0o600) : null;
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { env, stdio: ["ignore", output ? output.fd : "pipe", "pipe"],
      ...(cleanup ? {} : { signal: abort.signal }) });
    let out = "", err = "", spawnError;
    child.stdout?.on("data", (data) => { out += data; });
    child.stderr.on("data", (data) => { err = (err + data).slice(-8000); });
    // Abort can emit error before the OS process exits. Wait for close before
    // removing any directory that this process may still be writing.
    child.on("error", (error) => { spawnError = error; });
    child.on("close", (code) => {
      if (spawnError) return reject(spawnError);
      if (code === 0) return resolve(out.trim());
      // PostgreSQL diagnostics can echo connection strings. Never print the
      // service environment or credentials in a backup command's error output.
      for (const [key, value] of Object.entries(env)) {
        if (value && /PASSWORD|SECRET|TOKEN|PEPPER|DATABASE_URL|PGDATABASE/i.test(key)) err = err.split(value).join("[redacted]");
      }
      const error = new Error(`${path.basename(command)} failed (${code}): ${err.trim()}`);
      error.exitCode = code;
      reject(error);
    });
  }).finally(async () => { if (output) await output.close(); });
}

async function digest(file) {
  const hash = crypto.createHash("sha256");
  for await (const chunk of createReadStream(file)) hash.update(chunk);
  return hash.digest("hex");
}

async function copyTree(source, target, exclude = new Set()) {
  await fs.mkdir(target, { recursive: true, mode: 0o700 });
  for (const entry of await fs.readdir(source, { withFileTypes: true })) {
    abort.signal.throwIfAborted();
    if (exclude.has(entry.name)) continue;
    const from = path.join(source, entry.name), to = path.join(target, entry.name);
    if (entry.isDirectory()) await copyTree(from, to);
    else if (entry.isFile()) await fs.copyFile(from, to);
    else throw new Error(`unsupported link or special file in capture: ${from}`);
  }
}

async function inventory(directory, prefix = "") {
  const files = [];
  for (const entry of (await fs.readdir(directory, { withFileTypes: true })).sort((a, b) => a.name.localeCompare(b.name))) {
    abort.signal.throwIfAborted();
    if (!prefix && entry.name === "manifest.json") continue;
    const name = prefix + entry.name, file = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...await inventory(file, name + "/"));
    else if (entry.isFile()) files.push({ path: name, bytes: (await fs.stat(file)).size, sha256: await digest(file) });
    else throw new Error(`backup contains an unsupported link or special file: ${name}`);
  }
  return files;
}

async function verifyFiles(directory, manifest) {
  if (manifest.format_version !== 1 || manifest.status !== "complete" || !Array.isArray(manifest.files)) {
    throw new Error("unsupported or incomplete backup manifest");
  }
  for (const name of ["assets", "application", "config"]) {
    if (!(await fs.lstat(path.join(directory, name))).isDirectory()) throw new Error(`backup is missing required directory: ${name}`);
  }
  const actual = await inventory(directory);
  if (JSON.stringify(actual) !== JSON.stringify(manifest.files)) throw new Error("backup file inventory/checksum mismatch; do not restore this artifact");
  for (const file of ["database.dump", "application/server.js", "application/package-lock.json", "config/service.env"]) {
    if (!actual.some((entry) => entry.path === file)) throw new Error(`backup is missing required recovery input: ${file}`);
  }
  return actual;
}

function isolatedEnv(socket, runtime = {}) {
  // No archived environment is sourced during rehearsal. Clear every libpq
  // override, including service files and options, before targeting the cluster.
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("PG") && key !== "DATABASE_URL" && !RUNTIME_SETTING.test(key)));
  return { ...env, ...runtimeSettings(runtime), PGHOST: socket, PGPORT: "5432", PGUSER: "pages_backup", PGDATABASE: "pages",
    NODE_ENV: "production", RAW_TOKEN_SECRET: crypto.randomUUID(), API_TOKEN_PEPPER: crypto.randomUUID(),
    PAGE_COOKIE_SECRET: crypto.randomUUID(), RL_CONTENT_PER_MIN: "1000", PAGES_DEV_LOGIN: "0", AUTH_SIGNING_PUBKEY: "",
    DASHBOARD_HOST: "localhost", CONTENT_HOST: "content.localhost",
    DASHBOARD_ORIGIN: "http://localhost", CONTENT_ORIGIN: "http://content.localhost" };
}

async function postgresTool(name) {
  const candidates = (process.env.PATH || "").split(path.delimiter).filter(Boolean).map((directory) => path.resolve(directory, name));
  for (const base of ["/usr/lib/postgresql", "/opt/homebrew/opt", "/usr/local/opt"]) {
    let entries;
    try { entries = await fs.readdir(base); } catch { continue; }
    for (const entry of entries.sort((a, b) => b.localeCompare(a, undefined, { numeric: true }))) {
      if (base.endsWith("/opt") && !entry.startsWith("postgresql")) continue;
      candidates.push(path.join(base, entry, "bin", name));
    }
  }
  for (const tool of candidates) {
    try { await fs.access(tool, constants.X_OK); return tool; } catch { /* next installed version */ }
  }
  throw new Error(`${name} is required; install matching PostgreSQL server tools for restore verification`);
}

async function rehearse(directory, runtime = {}) {
  if (await digest(path.join(directory, "application/package-lock.json")) !== await digest(path.join(ROOT, "package-lock.json"))) {
    throw new Error("backup dependencies differ from this installation; run check-integrity using the matching release after npm ci");
  }
  const initdb = await postgresTool("initdb"), pgctl = path.join(path.dirname(initdb), "pg_ctl");
  const pgrestore = await postgresTool("pg_restore");
  const scratch = await fs.mkdtemp("/tmp/pgb-");
  const data = path.join(scratch, "db"), socket = path.join(scratch, "socket"), app = path.join(scratch, "app");
  let startAttempted = false;
  const env = isolatedEnv(socket, runtime);
  const asPg = (tool, args, options = {}) => process.getuid?.() === 0
    ? run("runuser", ["-u", "postgres", "--", tool, ...args], { ...options, env }) : run(tool, args, { ...options, env });
  try {
    await fs.mkdir(socket, { mode: 0o700 });
    if (process.getuid?.() === 0) await run("chown", ["-R", "postgres:postgres", scratch]);
    await asPg(initdb, ["-D", data, "--auth=trust", "--username=pages_backup", "--encoding=UTF8", "--no-locale"]);
    startAttempted = true;
    await asPg(pgctl, ["-D", data, "-l", path.join(scratch, "postgres.log"), "-w", "-t", "60", "-o",
      `-p 5432 -k ${socket} -c listen_addresses='' -c unix_socket_permissions=0700`, "start"]);
    await run(await postgresTool("createdb"), ["pages"], { env: { ...env, PGDATABASE: "postgres" } });
    await run(pgrestore, ["--exit-on-error", "--single-transaction", "--no-owner", "--no-acl", "--dbname=pages", path.join(directory, "database.dump")], { env });
    await copyTree(path.join(directory, "application"), app);
    await fs.symlink(await fs.realpath(path.join(ROOT, "node_modules")), path.join(app, "node_modules"));
    const output = await run(process.execPath, [path.join(ROOT, "scripts/backup-probe.js"), app, path.join(directory, "assets")], { env });
    return JSON.parse(output);
  } finally {
    // A failed stop deliberately retains the scratch directory for recovery;
    // never remove storage underneath a still-running PostgreSQL instance.
    if (startAttempted) {
      try { await asPg(pgctl, ["-D", data, "-w", "-t", "60", "-m", "immediate", "stop"], { cleanup: true }); }
      catch {
        let stopped = false;
        try { await asPg(pgctl, ["-D", data, "status"], { cleanup: true }); }
        catch (error) { stopped = error.exitCode === 3; }
        if (!stopped) throw new Error(`cannot confirm scratch PostgreSQL stopped; retained ${scratch} for operator cleanup`);
      }
    }
    await fs.rm(scratch, { recursive: true, force: true });
  }
}

async function create(destination, application, environment, installConfig) {
  const startedAt = new Date().toISOString();
  application = await fs.realpath(application);
  const assets = await fs.realpath(path.join(application, "assets"));
  await fs.mkdir(destination, { recursive: true, mode: 0o700 });
  destination = await fs.realpath(destination);
  for (const source of [application, assets]) {
    if (destination === source || destination.startsWith(source + path.sep)) throw new Error("backup destination must be outside application and asset directories");
  }
  const temporary = await fs.mkdtemp(path.join(destination, ".pages-partial-"));
  let published;
  try {
    await fs.mkdir(path.join(temporary, "config"), { mode: 0o700 });
    await fs.copyFile(environment, path.join(temporary, "config/service.env"));
    if (process.env.PAGES_BACKUP_ENV_DIGEST && await digest(path.join(temporary, "config/service.env")) !== process.env.PAGES_BACKUP_ENV_DIGEST) {
      throw new Error("service environment changed while loading configuration; retry after configuration changes finish");
    }
    const optional = {}, configurationSources = [["service.env", environment]];
    for (const [name, source] of [["install.env", installConfig], ["local.env", path.join(application, ".env")]]) {
      try {
        await fs.copyFile(source, path.join(temporary, "config", name));
        optional[name] = true;
        configurationSources.push([name, source]);
      } catch (error) { if (error.code !== "ENOENT") throw error; optional[name] = false; }
    }
    const dumpEnv = { ...process.env };
    if (dumpEnv.DATABASE_URL) dumpEnv.PGDATABASE = dumpEnv.DATABASE_URL;
    const pgdump = await postgresTool("pg_dump");
    const dumpArgs = ["--format=custom", "--no-owner", "--no-acl"];
    const outputFile = path.join(temporary, "database.dump");
    // Preserve the service account's libpq defaults/peer authentication while
    // writing through an already-open private descriptor owned by the operator.
    if (process.getuid?.() === 0 && process.env.PAGES_BACKUP_APP_USER) {
      await run("runuser", ["-u", process.env.PAGES_BACKUP_APP_USER, "--", pgdump, ...dumpArgs], { env: dumpEnv, outputFile });
    } else await run(pgdump, dumpArgs, { env: dumpEnv, outputFile });
    await copyTree(assets, path.join(temporary, "assets"));
    await copyTree(application, path.join(temporary, "application"), EXCLUDED);
    // Inventory comes from the restored dump, never from a later live read.
    const runtime = runtimeSettings(process.env);
    const restored = await rehearse(temporary, runtime);
    for (const [name, source] of configurationSources) {
      if (await digest(source) !== await digest(path.join(temporary, "config", name))) {
        throw new Error(`configuration changed during capture: ${name}; retry after configuration changes finish`);
      }
    }
    const manifest = { format_version: 1, status: "complete", started_at: startedAt, completed_at: new Date().toISOString(),
      runtime_configuration: runtime, node_version: process.version, pg_dump_version: await run(pgdump, ["--version"]), optional_configuration: optional,
      application_source: "captured installed files; source checkout HEAD is not used", restored,
      files: await inventory(temporary) };
    await fs.writeFile(path.join(temporary, "manifest.json"), JSON.stringify(manifest, null, 2) + "\n", { mode: 0o600 });
    const complete = path.join(destination, `pages-backup-${startedAt.replace(/[:.]/g, "-")}-${crypto.randomUUID()}`);
    // Flush recovery inputs and directory entries before publishing completion.
    for (const file of [...manifest.files.map((entry) => entry.path), "manifest.json"]) {
      const handle = await fs.open(path.join(temporary, file), "r");
      try { await handle.sync(); } finally { await handle.close(); }
    }
    const syncDirectories = async (directory) => {
      for (const entry of await fs.readdir(directory, { withFileTypes: true })) {
        if (entry.isDirectory()) await syncDirectories(path.join(directory, entry.name));
      }
      const handle = await fs.open(directory, "r");
      try { await handle.sync(); } finally { await handle.close(); }
    };
    await syncDirectories(temporary);
    await fs.rename(temporary, complete);
    published = complete;
    const parent = await fs.open(destination, "r");
    try { await parent.sync(); } finally { await parent.close(); }
    return { backup_dir: complete };
  } catch (error) {
    await fs.rm(published || temporary, { recursive: true, force: true });
    throw error;
  }
}

async function check(directory) {
  directory = await fs.realpath(directory);
  const manifest = JSON.parse(await fs.readFile(path.join(directory, "manifest.json"), "utf8"));
  await verifyFiles(directory, manifest);
  const restored = await rehearse(directory, manifest.runtime_configuration);
  if (JSON.stringify(restored) !== JSON.stringify(manifest.restored)) throw new Error("restored database inventory differs from the backup manifest");
  return { status: "verified", backup_dir: directory, restored };
}

if (require.main === module) {
  process.umask(0o077);
  process.once("SIGINT", () => abort.abort());
  process.once("SIGTERM", () => abort.abort());
  const [command, ...args] = process.argv.slice(2);
  const operation = command === "create" && args.length === 4 ? () => create(...args)
    : command === "check" && args.length === 1 ? () => check(args[0])
      : () => { throw new Error("usage: pages backup [destination] | pages check-integrity <backup-directory>"); };
  Promise.resolve().then(operation).then((result) => console.log(JSON.stringify(result)))
    .catch((error) => { console.error(`backup: ${error.message}`); process.exitCode = 1; });
}

module.exports = { create, check, verifyFiles, inventory, isolatedEnv, rehearse };
