// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Exercise the operator workflow against its own database: the general suite
// intentionally contains legacy placeholder hashes, which are not valid backups.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const crypto = require("node:crypto");
const { spawnSync } = require("node:child_process");

const ROOT = path.join(__dirname, "..");
const ASSET_BYTES = "Northwind persistent chart bytes\n";
const ASSET_SHA = crypto.createHash("sha256").update(ASSET_BYTES).digest("hex");

async function seed() {
  const db = require("../lib/db");
  const versions = require("../lib/versions");
  const templates = require("../lib/templates");
  const { hashPassword } = require("../lib/pagecookie");
  const actor = { actor: "backup-fixture", actorType: "user", transport: "admin" };
  const schema = (properties) => ({
    $schema: "https://json-schema.org/draft/2020-12/schema", type: "object",
    additionalProperties: false, required: Object.keys(properties), properties,
  });
  const block = (id, value, type = "application/json") =>
    `<script id="${id}" type="${type}">${JSON.stringify(value)}</script>`;
  const html = "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind recovery</h1>" +
    block("pages-config-schema", schema({ campaign: { type: "string" } }), "application/schema+json") +
    block("pages-config", { campaign: "Northwind" }) +
    block("pages-data-schema", schema({ count: { type: "number" } }), "application/schema+json") +
    block("pages-data", { contract_version: 1, refreshed_at: "2026-08-01T00:00:00.000Z",
      source_as_of: "2026-08-01T00:00:00.000Z", data: { count: 1 } }) + "</body></html>";
  try {
    const registered = await templates.register({ name: "northwind-layout", html }, actor);
    const built = await templates.createPage({ template: "northwind-layout", slug: "northwind-report",
      config: { campaign: "Northwind current" }, data: { count: 7 }, sourceAsOf: "2026-08-01T00:00:00Z" }, actor);
    await versions.setPassword({ slug: "northwind-report", password: "northwind-fixture-password" }, actor);
    await templates.register({ name: "northwind-layout", html: html.replace("Northwind recovery", "Northwind next layout") }, actor);
    await versions.deploy({ slug: "northwind-report", html: built.html.replace("Northwind recovery", "Northwind unpublished draft"), publish: false }, actor);
    const live = (await db.query("SELECT published_version_id FROM pages WHERE slug='northwind-report'")).rows[0];
    assert.equal(String(live.published_version_id), String(built.version.id));
    const pinned = (await db.query("SELECT template_version_id FROM page_versions WHERE id=$1", [built.version.id])).rows[0];
    assert.equal(String(pinned.template_version_id), String(registered.revision.version_id));
    await templates.register({ name: "contoso-retired", html }, actor);
    await db.query("UPDATE page_templates SET deleted_at=now() WHERE name='contoso-retired'");
    await versions.createPage({ slug: "fabrikam-disabled", title: "Fabrikam retained page" }, actor);
    await versions.deploy({ slug: "fabrikam-disabled", html: "<!doctype html><html><body>Fabrikam retained page</body></html>", publish: true }, actor);
    await versions.setDisabled({ slug: "fabrikam-disabled", disabled: true }, actor);
    const credential = await hashPassword("northwind-portal-fixture-password");
    const portal = (await db.query(`INSERT INTO page_portals(slug,name,password_hash,home_page_id)
      SELECT 'northwind-partners','Northwind partners',$1,id FROM pages WHERE slug='fabrikam-disabled' RETURNING id`, [credential])).rows[0];
    await db.query(`INSERT INTO page_portal_members(portal_id,page_id,label,sort_order)
      SELECT $1,id,'Northwind report',3 FROM pages WHERE slug='northwind-report'`, [portal.id]);
    // A home page absent from membership is a valid, advisory fallback.
    await db.query("INSERT INTO page_portals(slug,name,password_hash,deleted_at) VALUES ('contoso-retired','Contoso retired',$1,now())", [credential]);
    await db.query("INSERT INTO assets(sha256,content_type,bytes) VALUES ($1,'text/plain',$2)", [ASSET_SHA, Buffer.byteLength(ASSET_BYTES)]);
    await db.query("UPDATE themes SET logo_sha256=$1 WHERE name='flag'", [ASSET_SHA]);
  } finally { await db.pool.end(); }
}

function run(command, args, env, timeout = 180000) {
  const result = spawnSync(command, args, { cwd: ROOT, env, encoding: "utf8", timeout });
  result.out = `${result.stdout || ""}${result.stderr || ""}`;
  return result;
}
function good(result) {
  assert.equal(result.error, undefined, result.out);
  assert.equal(result.status, 0, result.out);
  return result.stdout.trim();
}
const quoteShell = (value) => `'${String(value).replaceAll("'", "'\\''")}'`;
const quoteIdentifier = (value) => `"${value.replaceAll('"', '""')}"`;

function databaseSnapshot(env) {
  const query = (sql) => good(run("psql", ["-X", "-At", "-v", "ON_ERROR_STOP=1", "-c", sql], env));
  const tables = query("SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename").split("\n");
  const sequences = query("SELECT sequencename FROM pg_sequences WHERE schemaname='public' ORDER BY sequencename").split("\n");
  return JSON.stringify({
    tables: tables.map((name) => [name, query(`SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb) FROM public.${quoteIdentifier(name)} t`)]),
    sequences: sequences.filter(Boolean).map((name) => [name, query(`SELECT last_value,is_called FROM public.${quoteIdentifier(name)}`)]),
  });
}

async function main() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "pages-backup-test-"));
  const database = `pages_backup_fixture_${process.pid}`;
  const env = { ...process.env, PGDATABASE: database };
  delete env.DATABASE_URL;
  delete env.NODE_TEST_CONTEXT;
  const outerEnv = { ...env, PGDATABASE: "postgres" };
  const app = path.join(dir, "installed"), sharedAssets = path.join(dir, "persistent-assets");
  const destination = path.join(dir, "backups"), failedDestination = path.join(dir, "failed-backups");
  try {
    good(run("createdb", [database], outerEnv));
    good(run(process.execPath, [path.join(ROOT, "lib/migrate.js")], env));
    good(run(process.execPath, [__filename, "--seed"], env));
    fs.cpSync(ROOT, app, { recursive: true, filter: (source) => {
      const relative = path.relative(ROOT, source);
      return ![".git", "node_modules", "assets", ".env", ".devdata", "test-results", "playwright-report"].includes(relative.split(path.sep)[0]);
    } });
    fs.symlinkSync(path.join(ROOT, "node_modules"), path.join(app, "node_modules"), "dir");
    fs.mkdirSync(sharedAssets);
    fs.writeFileSync(path.join(sharedAssets, "northwind-chart.txt"), ASSET_BYTES);
    fs.writeFileSync(path.join(sharedAssets, ASSET_SHA), ASSET_BYTES);
    fs.symlinkSync(sharedAssets, path.join(app, "assets"), "dir");
    const localEnv = path.join(dir, "local.env");
    fs.writeFileSync(localEnv, "NORTHWIND_LOCAL_SETTING=retained\n");
    fs.symlinkSync(localEnv, path.join(app, ".env"));
    const serviceEnv = path.join(dir, "service.env"), installEnv = path.join(dir, "install.env");
    const serviceValues = {
      PGHOST: env.PGHOST, PGPORT: env.PGPORT, PGUSER: env.PGUSER, PGDATABASE: database,
      RAW_TOKEN_SECRET: "northwind-backup-raw-fixture", API_TOKEN_PEPPER: "northwind-backup-token-fixture",
      PAGE_COOKIE_SECRET: "northwind-backup-cookie-fixture", DASHBOARD_HOST: "localhost", CONTENT_HOST: "content.localhost",
    };
    fs.writeFileSync(serviceEnv, Object.entries(serviceValues).filter(([, value]) => value !== undefined)
      .map(([key, value]) => `${key}=${quoteShell(value)}`).join("\n") + "\n", { mode: 0o600 });
    fs.writeFileSync(installEnv, `PAGES_APP_DIR=${quoteShell(app)}\nPAGES_APP_USER=${quoteShell(os.userInfo().username)}\nPAGES_ENV_FILE=${quoteShell(serviceEnv)}\nPAGES_PORT=4317\n`);
    Object.assign(env, { APP_DIR: app, APP_USER: os.userInfo().username, PAGES_APP_DIR: app,
      PAGES_ENV_FILE: serviceEnv, PAGES_INSTALL_CONFIG: installEnv, PAGES_SRC_DIR: ROOT });
    const before = databaseSnapshot(env);
    const created = JSON.parse(good(run("bash", [path.join(ROOT, "scripts/backup.sh"), destination], env)));
    const backup = created.backup_dir;
    assert.equal(path.dirname(backup), destination);
    const manifest = JSON.parse(fs.readFileSync(path.join(backup, "manifest.json"), "utf8"));
    assert.equal(manifest.status, "complete");
    assert.equal(manifest.format_version, 1);
    for (const [archived, original] of [["config/service.env", serviceEnv], ["config/install.env", installEnv],
      ["config/local.env", localEnv], ["assets/northwind-chart.txt", path.join(sharedAssets, "northwind-chart.txt")],
      ["application/package-lock.json", path.join(ROOT, "package-lock.json")]]) {
      assert.deepEqual(fs.readFileSync(path.join(backup, archived)), fs.readFileSync(original), `${archived} preserves actual bytes`);
    }
    assert.equal(fs.lstatSync(path.join(backup, "assets")).isSymbolicLink(), false, "archive assets directory contents");
    assert.equal(fs.existsSync(path.join(backup, "application/node_modules")), false);
    assert.equal(fs.existsSync(path.join(backup, "application/.env")), false);
    const verified = JSON.parse(good(run(process.execPath, [path.join(ROOT, "scripts/backup.js"), "check", backup], env)));
    assert.equal(verified.status, "verified");
    for (const [table, count] of Object.entries({ pages: "2", page_versions: "3", page_templates: "2",
      page_template_versions: "3", page_portals: "2", page_portal_members: "1", assets: "1" })) {
      assert.equal(verified.restored.counts[table], count, `${table} survives the isolated restore`);
    }
    assert.deepEqual(verified.restored.hashes, { versions: 3, template_versions: 3 });
    assert.equal(verified.restored.assets, 1, "referenced asset bytes and hash survive the restore");
    assert.deepEqual(verified.restored.probes, { pages: 2, versions: 3, templates: 3, portals: 2, portal_pages: 1 });
    assert.deepEqual(verified.restored, manifest.restored, "new rehearsal reproduces capture-time verification");
    assert.equal(databaseSnapshot(env), before, "capture and rehearsal preserve all source rows, pointers and sequences");

    const bin = path.join(dir, "bin");
    fs.mkdirSync(bin);
    fs.writeFileSync(path.join(bin, "pg_dump"), "#!/bin/sh\nprintf 'injected dump failure\\n' >&2\nexit 27\n", { mode: 0o755 });
    fs.mkdirSync(failedDestination);
    const failed = run("bash", [path.join(ROOT, "scripts/backup.sh"), failedDestination], { ...env, PATH: `${bin}:${env.PATH}` });
    assert.notEqual(failed.status, 0, "failed dump must fail the backup command");
    assert.equal(failed.error, undefined, failed.out);
    assert.deepEqual(fs.readdirSync(failedDestination), [], "a partial failure publishes no completed or partial directory");

    fs.appendFileSync(path.join(backup, "assets/northwind-chart.txt"), "corrupted\n");
    const corrupt = run(process.execPath, [path.join(ROOT, "scripts/backup.js"), "check", backup], env);
    assert.notEqual(corrupt.status, 0, "changed artifact bytes must fail verification");
    assert.equal(corrupt.error, undefined, corrupt.out);
    assert.equal(databaseSnapshot(env), before, "failed backups and verification preserve live content");
    console.log("✓ backup restores pages, immutable versions, pinned templates and portals without changing source state");
    console.log("✓ symlinked persistent inputs survive capture; failures and changed artifact bytes never verify");
  } finally {
    run("dropdb", ["--if-exists", database], outerEnv);
    fs.rmSync(dir, { recursive: true, force: true });
  }
}

(process.argv[2] === "--seed" ? seed() : main()).catch((error) => { console.error(error); process.exitCode = 1; });
