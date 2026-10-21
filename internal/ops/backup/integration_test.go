// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ElcanoTek/pages/internal/ops/backupprobe"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/testdb"
)

// TestBackupAgainstRealPostgres exercises capture and rehearsal with the
// real PostgreSQL tools: pg_dump of a migrated, seeded database, a private
// initdb cluster, pg_restore, and the probe's database half. The HTTP half
// needs the server and is covered where the server is.
func TestBackupAgainstRealPostgres(t *testing.T) {
	for _, tool := range []string{"pg_dump", "initdb", "pg_restore", "createdb"} {
		if _, err := PostgresTool(tool); err != nil {
			t.Skip(err)
		}
	}
	d, url := testdb.NewWithURL(t)
	ctx := context.Background()
	euid := os.Geteuid()
	if euid == 0 {
		if _, err := user.Lookup("postgres"); err != nil {
			t.Skip("root run needs a postgres account for the scratch cluster")
		}
	}
	const assetBytes = "Northwind persistent chart bytes\n"
	sum := sha256.Sum256([]byte(assetBytes))
	assetSHA := hex.EncodeToString(sum[:])
	html := "<!doctype html><html><body><h1>Northwind recovery</h1></body></html>"
	hsum := sha256.Sum256([]byte(html))
	for _, q := range []string{
		"INSERT INTO pages (slug, title) VALUES ('northwind-report', 'Northwind'), ('fabrikam-disabled', 'Fabrikam')",
		fmt.Sprintf("INSERT INTO page_versions (page_id, html, content_sha256, author) SELECT id, '%s', '%s', 'backup-fixture' FROM pages", html, hex.EncodeToString(hsum[:])),
		"UPDATE pages p SET published_version_id = v.id FROM page_versions v WHERE v.page_id = p.id",
		"UPDATE pages SET disabled = true WHERE slug = 'fabrikam-disabled'",
		fmt.Sprintf("INSERT INTO assets (sha256, content_type, bytes) VALUES ('%s', 'text/plain', %d)", assetSHA, len(assetBytes)),
		fmt.Sprintf("UPDATE themes SET logo_sha256 = '%s' WHERE name = 'flag'", assetSHA),
	} {
		if _, err := d.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s := installation(t, "DATABASE_URL='"+url+"'\nRAW_TOKEN_SECRET='northwind-backup-raw-fixture'\nAPI_TOKEN_PEPPER='northwind-backup-token-fixture'\nPAGE_COOKIE_SECRET='northwind-backup-cookie-fixture'\nPAGES_AUTH_MODE='central'\nAUTH_CLIENT_SECRET='northwind-central-client-fixture-0000000000'\n")
	writeFile(t, filepath.Join(s.SharedAssets(), assetSHA), assetBytes)
	if euid == 0 {
		// The service account dumps; the fixture uses postgres for it.
		s.AppUser = "postgres"
	}
	probe := func(ctx context.Context, _, assets string, env []string) (json.RawMessage, error) {
		e := envMap(env)
		if e["PAGES_AUTH_MODE"] != "elcano" || e["AUTH_CLIENT_SECRET"] != "" {
			return nil, fmt.Errorf("probe inherited central sign-in")
		}
		pool, err := pgxpool.New(ctx, fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=disable", e["PGHOST"], e["PGPORT"], e["PGUSER"], e["PGDATABASE"]))
		if err != nil {
			return nil, err
		}
		defer pool.Close()
		res, err := backupprobe.Inspect(ctx, pool, assets)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}
	o := Options{Runner: runner.Exec{}, EUID: euid, Probe: probe}
	snapshot := func() string {
		var out strings.Builder
		for _, q := range []string{"SELECT count(*)::text FROM pages", "SELECT string_agg(published_version_id::text, ',' ORDER BY id) FROM pages", "SELECT count(*)::text FROM page_versions"} {
			var v string
			if err := d.QueryRow(ctx, q).Scan(&v); err != nil {
				t.Fatal(err)
			}
			out.WriteString(v + ";")
		}
		return out.String()
	}
	before := snapshot()
	dest := filepath.Join(t.TempDir(), "backups")
	dir, err := Capture(ctx, o, s, dest)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(ctx, o, dir)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Counts map[string]string `json:"counts"`
		Hashes map[string]int    `json:"hashes"`
		Assets int               `json:"assets"`
	}
	_ = json.Unmarshal(res.Restored, &raw)
	if raw.Counts["pages"] != "2" || raw.Counts["page_versions"] != "2" || raw.Counts["assets"] != "1" || raw.Hashes["versions"] != 2 || raw.Assets != 1 {
		t.Fatalf("restored %s", res.Restored)
	}
	if snapshot() != before {
		t.Fatal("capture and rehearsal changed source rows")
	}

	// A version pointer crossing pages fails restore verification, publishes
	// nothing and leaves the source for an explicit repair.
	if _, err := d.Exec(ctx, "UPDATE pages SET published_version_id = (SELECT published_version_id FROM pages WHERE slug='fabrikam-disabled') WHERE slug='northwind-report'"); err != nil {
		t.Fatal(err)
	}
	rejected := filepath.Join(t.TempDir(), "rejected")
	_, err = Capture(ctx, o, s, rejected)
	wantErr(t, err, "restored pointers cross page or template ownership")
	if entries, _ := os.ReadDir(rejected); len(entries) != 0 {
		t.Fatalf("failed verification published %v", entries)
	}
}
