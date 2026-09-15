// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// Runs only against the private restored cluster, using the captured release.
// It never loads captured credentials, migrates, publishes, or executes page JS.
const path = require("node:path");
const fs = require("node:fs/promises");
const http = require("node:http");
const crypto = require("node:crypto");
const { createReadStream } = require("node:fs");
const [application, assets] = process.argv.slice(2);
const db = require(path.join(application, "lib/db"));
const { app } = require(path.join(application, "server"));
const readiness = require(path.join(application, "lib/readiness"));
const rawtoken = require(path.join(application, "lib/rawtoken"));
const pagecookie = require(path.join(application, "lib/pagecookie"));
let server;

function ensure(condition, message) { if (!condition) throw new Error(message); }
function get(url, { host = "content.localhost", cookie } = {}) {
  return new Promise((resolve, reject) => {
    const req = http.get({ hostname: "127.0.0.1", port: server.address().port, path: url,
      headers: { Host: host, ...(cookie ? { Cookie: cookie } : {}) }, agent: false, timeout: 10000 }, (res) => {
      let body = "";
      res.on("data", (chunk) => { body += chunk; });
      res.on("end", () => resolve({ status: res.statusCode, body, headers: res.headers }));
    });
    req.on("timeout", () => req.destroy(new Error("restored application request timed out")));
    req.on("error", reject);
  });
}

async function checkHashes(table) {
  let last = "0", checked = 0;
  for (;;) {
    const { rows } = await db.query(`SELECT id,html,content_sha256 FROM ${table} WHERE id>$1 ORDER BY id LIMIT 20`, [last]);
    if (!rows.length) return checked;
    for (const row of rows) {
      ensure(crypto.createHash("sha256").update(row.html, "utf8").digest("hex") === row.content_sha256,
        `${table} content hash mismatch at id ${row.id}`);
      checked++;
    }
    last = rows.at(-1).id;
  }
}

async function probe() {
  const badPointers = (await db.query(`SELECT
    (SELECT count(*) FROM pages p JOIN page_versions v ON v.id=p.published_version_id WHERE v.page_id<>p.id) +
    (SELECT count(*) FROM page_templates t JOIN page_template_versions v ON v.id=t.current_version_id WHERE v.template_id<>t.id) +
    (SELECT count(*) FROM preview_links l JOIN page_versions v ON v.id=l.version_id WHERE v.page_id<>l.page_id)
    AS invalid`)).rows[0];
  ensure(Number(badPointers.invalid) === 0, "restored pointers cross page or template ownership");
  const hashes = { versions: await checkHashes("page_versions"), template_versions: await checkHashes("page_template_versions") };
  const assetRows = (await db.query("SELECT sha256,bytes FROM assets ORDER BY sha256")).rows;
  for (const row of assetRows) {
    ensure(/^[a-f0-9]{64}$/.test(row.sha256), "asset has an invalid content-addressed name");
    const file = path.join(assets, row.sha256), stat = await fs.lstat(file);
    ensure(stat.isFile() && stat.size === Number(row.bytes), `asset length mismatch: ${row.sha256}`);
    const hash = crypto.createHash("sha256");
    for await (const chunk of createReadStream(file)) hash.update(chunk);
    ensure(hash.digest("hex") === row.sha256, `asset checksum mismatch: ${row.sha256}`);
  }
  ensure(!(await db.query("SELECT 1 FROM themes t LEFT JOIN assets a ON a.sha256=t.logo_sha256 WHERE t.logo_sha256 IS NOT NULL AND a.sha256 IS NULL LIMIT 1")).rows.length,
    "theme references a missing asset");
  const counts = {};
  for (const { tablename } of (await db.query("SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename")).rows) {
    const quoted = '"' + tablename.replaceAll('"', '""') + '"';
    counts[tablename] = (await db.query(`SELECT count(*)::text AS count FROM public.${quoted}`)).rows[0].count;
  }
  const migrations = (await db.query("SELECT filename FROM schema_migrations ORDER BY filename")).rows.map((row) => row.filename);
  const probes = { pages: 0, versions: 0, templates: 0, portals: 0, portal_pages: 0 };
  await new Promise((resolve, reject) => { server = app.listen(0, "127.0.0.1", resolve); server.once("error", reject); });
  ensure((await get("/readyz", { host: "localhost" })).status === 200, "captured release is not ready against the restored schema; no migrations were applied");
  ensure((await get("/assets/flag/tokens/design-tokens.css")).status === 200, "captured public assets cannot be served");

  const pages = (await db.query("SELECT id,slug,disabled,deleted_at,password_hash,published_version_id FROM pages ORDER BY id LIMIT 5")).rows;
  for (const row of pages) {
    const expected = row.disabled || row.deleted_at || !row.published_version_id ? 404 : row.password_hash ? 200 : 403;
    const cookie = row.password_hash ? `${pagecookie.cookieName(row.id)}=${pagecookie.mintSession(row.id, 60, row.password_hash)}` : undefined;
    const response = await get("/" + row.slug, { cookie });
    ensure(response.status === expected, `restored page ${row.id} returned ${response.status}, expected ${expected}`);
    probes.pages++;
  }
  for (const row of (await db.query(`SELECT v.id,v.page_id,v.render_mode,p.slug,p.disabled,p.deleted_at
    FROM page_versions v JOIN pages p ON p.id=v.page_id ORDER BY v.id LIMIT 5`)).rows) {
    const token = rawtoken.mint({ pageId: row.page_id, versionId: row.id, purpose: "view", renderMode: row.render_mode });
    const response = await get(`/raw/${row.slug}?t=${encodeURIComponent(token)}`);
    const expected = row.disabled || row.deleted_at ? 404 : 200;
    ensure(response.status === expected, `restored version ${row.id} cannot be served (${response.status})`);
    probes.versions++;
  }
  for (const row of (await db.query(`SELECT v.id,t.deleted_at FROM page_template_versions v
    JOIN page_templates t ON t.id=v.template_id ORDER BY v.id LIMIT 5`)).rows) {
    const token = rawtoken.mint({ pageId: 0, versionId: row.id, purpose: "template", renderMode: "themed" });
    const response = await get(`/raw-template/${row.id}?t=${encodeURIComponent(token)}`);
    ensure(response.status === (row.deleted_at ? 404 : 200), `restored template revision ${row.id} cannot be previewed (${response.status})`);
    probes.templates++;
  }
  for (const row of (await db.query("SELECT id,slug,password_hash,deleted_at FROM page_portals ORDER BY id LIMIT 5")).rows) {
    const cookie = `${pagecookie.portalCookieName(row.id)}=${pagecookie.mintPortalSession(row.id, 60, row.password_hash)}`;
    const response = await get(`/portal/${row.slug}`, { cookie });
    ensure(response.status === (row.deleted_at ? 404 : 200), `restored portal ${row.id} cannot be served (${response.status})`);
    probes.portals++;
    if (row.deleted_at) continue;
    const members = await db.getPortalPages(row.id);
    for (const member of members.slice(0, 5)) {
      ensure((await get("/" + member.slug, { cookie })).status === 200, `restored portal ${row.id} cannot serve member ${member.id}`);
      probes.portal_pages++;
    }
  }
  return { counts, migrations, hashes, assets: assetRows.length, probes };
}

probe().then((result) => console.log(JSON.stringify(result))).catch((error) => { console.error(error.message); process.exitCode = 1; })
  .finally(async () => {
    if (server) await new Promise((resolve) => server.close(resolve));
    await Promise.all([db.pool.end(), readiness.close()]);
  });
