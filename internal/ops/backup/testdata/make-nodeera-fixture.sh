#!/usr/bin/env bash
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
#
# Regenerates testdata/nodeera: a backup taken by the LAST NODE RELEASE's own
# scripts/backup.js (format_version 1, node_version, application tree with
# package-lock.json, no Go binary), so `pages check-integrity` keeps accepting
# the backups operators took before upgrading.
#
#   bash internal/ops/backup/testdata/make-nodeera-fixture.sh
#
# Needs node + npm, PostgreSQL server tools and the test cluster
# (`make test-db`; PAGES_TEST_DATABASE_URL overrides its URL). The Node
# release is `git archive $NODE_REV` (default: the parent of the first Go
# commit), installed with npm ci, migrated with its own lib/migrate.js (001-026),
# seeded, and captured with its own backup.js `create`, which rehearses the
# restore with the Node probe and records that result in manifest.restored.
#
# The one edit afterwards: application/ is trimmed to the recovery inputs the
# JS required (server.js, package-lock.json) plus package.json, and
# manifest.files is recomputed with backup.js's own inventory(), so the fixture
# stays small. Go never runs a captured Node application, so nothing else in
# the tree is read; everything else in the manifest is exactly what
# backup.js wrote.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(git -C "$here" rev-parse --show-toplevel)"
node_rev="${NODE_REV:-$(git -C "$repo" log --diff-filter=A --format=%H -- go.mod | tail -1)^}"
base_url="${PAGES_TEST_DATABASE_URL:-postgres://pages@127.0.0.1:5433/postgres}"
work="$(mktemp -d /tmp/nodeera-XXXXXX)"
trap 'rm -rf "$work"' EXIT
app="$work/app"
mkdir -p "$app"
git -C "$repo" archive "$node_rev" | tar -x -C "$app"
(cd "$app" && npm ci --omit=dev --ignore-scripts --no-audit --no-fund >/dev/null)

dbname="pages_nodeera_fixture_$$"
psql "$base_url" -qc "CREATE DATABASE $dbname"
trap 'psql "$base_url" -qc "DROP DATABASE IF EXISTS $dbname WITH (FORCE)" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
db_url="${base_url%/*}/$dbname"
db_url="${db_url%%\?*}"

cat >"$work/service.env" <<EOF
DATABASE_URL='$db_url'
RAW_TOKEN_SECRET='northwind-nodeera-raw-fixture'
API_TOKEN_PEPPER='northwind-nodeera-pepper-fixture'
PAGE_COOKIE_SECRET='northwind-nodeera-cookie-fixture'
PAGES_DATA_MAX_BYTES='1048576'
EOF
printf "APP_DIR='/opt/pages'\nAPP_USER='pages'\n" >"$work/install.env"
mkdir -p "$app/assets"
printf 'Northwind persistent chart bytes\n' >"$app/assets/pending"
asset_sha="$(sha256sum "$app/assets/pending" | cut -d' ' -f1)"
mv "$app/assets/pending" "$app/assets/$asset_sha"

(
  cd "$app"
  set -a; . "$work/service.env"; set +a
  node lib/migrate.js >/dev/null
  ASSET_SHA="$asset_sha" node -e '
    const crypto = require("node:crypto");
    const db = require("./lib/db");
    const pagecookie = require("./lib/pagecookie");
    const sha = (s) => crypto.createHash("sha256").update(s, "utf8").digest("hex");
    (async () => {
      const html = (t) => `<!doctype html><html><body><h1>${t}</h1></body></html>`;
      const hash = await pagecookie.hashPassword("northwind-portal-fixture");
      await db.query(`INSERT INTO pages (slug, title) VALUES ($$northwind-report$$, $$Northwind$$), ($$fabrikam-disabled$$, $$Fabrikam$$), ($$contoso-locked$$, $$Contoso$$)`);
      await db.query(`UPDATE pages SET password_hash=$1 WHERE slug=$$contoso-locked$$`, [hash]);
      for (const { rows: [p] } of [await db.query(`SELECT id, title FROM pages WHERE slug=$$northwind-report$$`), await db.query(`SELECT id, title FROM pages WHERE slug=$$fabrikam-disabled$$`), await db.query(`SELECT id, title FROM pages WHERE slug=$$contoso-locked$$`)]) {
        for (const n of [1, 2]) {
          const h = html(`${p.title} recovery ${n}`);
          await db.query(`INSERT INTO page_versions (page_id, html, content_sha256, author) VALUES ($1, $2, $3, $$backup-fixture$$)`, [p.id, h, sha(h)]);
        }
      }
      await db.query(`UPDATE pages p SET published_version_id = (SELECT max(v.id) FROM page_versions v WHERE v.page_id = p.id)`);
      await db.query(`UPDATE pages SET disabled = true WHERE slug=$$fabrikam-disabled$$`);
      await db.query(`INSERT INTO assets (sha256, content_type, bytes) VALUES ($1, $$text/plain$$, 33)`, [process.env.ASSET_SHA]);
      await db.query(`UPDATE themes SET logo_sha256 = $1 WHERE name = $$flag$$`, [process.env.ASSET_SHA]);
      const { rows: [portal] } = await db.query(`INSERT INTO page_portals (slug, name, password_hash) VALUES ($$tailspin-portal$$, $$Tailspin$$, $1) RETURNING id`, [hash]);
      await db.query(`INSERT INTO page_portal_members (portal_id, page_id, sort_order) SELECT $1, id, 0 FROM pages WHERE slug=$$contoso-locked$$`, [portal.id]);
      await db.pool.end();
    })().catch((e) => { console.error(e); process.exit(1); });
  '
  # backup.sh, minus the installed-paths/flock plumbing: the service
  # environment is the process environment and the digest is checked.
  PAGES_BACKUP_ENV_DIGEST="$(sha256sum "$work/service.env" | cut -d' ' -f1)" \
    node scripts/backup.js create "$work/out" "$app" "$work/service.env" "$work/install.env" >"$work/created.json"
)

backup_dir="$(node -e 'process.stdout.write(JSON.parse(require("fs").readFileSync(process.argv[1],"utf8")).backup_dir)' "$work/created.json")"
find "$backup_dir/application" -mindepth 1 -maxdepth 1 ! -name server.js ! -name package.json ! -name package-lock.json -exec rm -rf {} +
(cd "$app" && node -e '
  const fs = require("node:fs");
  const { inventory } = require("./scripts/backup.js");
  const dir = process.argv[1], file = dir + "/manifest.json";
  const manifest = JSON.parse(fs.readFileSync(file, "utf8"));
  inventory(dir).then((files) => {
    manifest.files = files;
    fs.writeFileSync(file, JSON.stringify(manifest, null, 2) + "\n");
  });
' "$backup_dir")

rm -rf "$here/nodeera"
cp -R "$backup_dir" "$here/nodeera"
chmod -R u+rwX,go+rX "$here/nodeera"
echo "wrote $here/nodeera"
