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

	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/testdb"
)

// TestRehearsalRunsOnlyTrustedBinaries is the scripted-cluster half: the
// probe runs the installed release's binary (never the backup's copy) while
// it is identical and root's alone, and no child process ever names a file
// from the backup's application once it is not.
func TestRehearsalRunsOnlyTrustedBinaries(t *testing.T) {
	s := installation(t, serviceEnv)
	fake := fakeCluster(t, false, nil)
	o := withReleases(captureOptions(fake), s)
	var probed []string
	inner := o.Probe
	o.Probe = func(ctx context.Context, binary, assets string, env []string) (json.RawMessage, error) {
		probed = append(probed, binary)
		return inner(ctx, binary, assets, env)
	}
	dir, err := Capture(context.Background(), o, s, filepath.Join(t.TempDir(), "backups"))
	if err != nil {
		t.Fatal(err)
	}
	installed, _ := filepath.EvalSymlinks(filepath.Join(s.ReleaseRoot(), "r1", "pages"))
	if res, err := Check(context.Background(), o, dir); err != nil || res.ProbedWith != installed {
		t.Fatalf("%+v %v", res, err)
	}
	if len(probed) != 2 || probed[0] != installed || probed[1] != installed {
		t.Fatalf("probed with %v", probed)
	}
	// The installed release becomes writable by other accounts: it is no
	// longer trusted, and the backup's identical copy is never run instead.
	if err := os.Chmod(installed, 0o666); err != nil {
		t.Fatal(err)
	}
	probed = nil
	// The scripted cluster has no database, so the fallback (inventory,
	// then this release) fails here; it must not have run anything captured.
	_, err = Check(context.Background(), o, dir)
	if err == nil || len(probed) != 0 {
		t.Fatalf("an untrusted match must not probe: %v %v", probed, err)
	}
	if strings.Contains(fake.Log(), filepath.Join(dir, "application")) {
		t.Fatalf("a child named the backup's application:\n%s", fake.Log())
	}
}

// TestCheckIntegrityNeverExecutesCapturedBinary: a backup's files are only
// as trustworthy as its manifest, which is just as writable. A backup whose
// application/pages AND manifest were both rewritten to a malicious script
// verifies its files, yet check-integrity (which runs as root) must never
// execute that script: only a trusted installed binary with the same
// SHA-256 may run, and otherwise this release serves the restored data.
func TestCheckIntegrityNeverExecutesCapturedBinary(t *testing.T) {
	for _, tool := range []string{"pg_dump", "initdb", "pg_restore", "createdb"} {
		if _, err := PostgresTool(tool); err != nil {
			t.Skip(err)
		}
	}
	euid := os.Geteuid()
	if euid == 0 {
		if _, err := user.Lookup("postgres"); err != nil {
			t.Skip("root run needs a postgres account for the scratch cluster")
		}
	}
	d, url := testdb.NewWithURL(t)
	ctx := context.Background()
	html := "<!doctype html><html><body><h1>Contoso recovery</h1></body></html>"
	hsum := sha256.Sum256([]byte(html))
	for _, q := range []string{
		"INSERT INTO pages (slug, title) VALUES ('contoso-report', 'Contoso')",
		fmt.Sprintf("INSERT INTO page_versions (page_id, html, content_sha256, author) SELECT id, '%s', '%s', 'backup-fixture' FROM pages", html, hex.EncodeToString(hsum[:])),
		"UPDATE pages p SET published_version_id = v.id FROM page_versions v WHERE v.page_id = p.id",
	} {
		if _, err := d.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s := installation(t, "DATABASE_URL='"+url+"'\nRAW_TOKEN_SECRET='contoso-backup-raw-fixture'\nAPI_TOKEN_PEPPER='contoso-backup-token-fixture'\nPAGE_COOKIE_SECRET='contoso-backup-cookie-fixture'\n")
	if euid == 0 {
		s.AppUser = "postgres"
	}
	served := 0
	current := inProcessProbe(t, &served)
	var probed []string
	o := Options{Runner: runner.Exec{}, EUID: euid, ReleaseRoot: s.ReleaseRoot(), CurrentProbe: current,
		// The trusted installed release "runs" its probe: this release's,
		// in process (the fixture binary is not a real build).
		Probe: func(ctx context.Context, binary, assets string, env []string) (json.RawMessage, error) {
			probed = append(probed, binary)
			return current(ctx, assets, env)
		}}
	dir, err := Capture(ctx, o, s, filepath.Join(t.TempDir(), "backups"))
	if err != nil {
		t.Fatal(err)
	}
	installed, _ := filepath.EvalSymlinks(filepath.Join(s.ReleaseRoot(), "r1", "pages"))
	if len(probed) != 1 || probed[0] != installed {
		t.Fatalf("capture probed with %v, want the installed release %s", probed, installed)
	}
	res, err := Check(ctx, o, dir)
	if err != nil || res.ProbedWith != installed || res.Notice != "" {
		t.Fatalf("untampered: %+v %v", res, err)
	}

	// Rewrite the captured binary to a script and the manifest to match.
	sentinel := filepath.Join(t.TempDir(), "captured-binary-ran")
	script := "#!/bin/sh\ntouch " + sentinel + "\necho '{}'\n"
	if err := os.WriteFile(filepath.Join(dir, "application", "pages"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "application", "pages"), 0o755); err != nil { // the capture kept 0600
		t.Fatal(err)
	}
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(script))
	for i := range m.Files {
		if m.Files[i].Path == "application/pages" {
			m.Files[i].Bytes, m.Files[i].Sha256 = int64(len(script)), hex.EncodeToString(sum[:])
		}
	}
	b, _ = json.MarshalIndent(m, "", "  ")
	writeFile(t, filepath.Join(dir, "manifest.json"), string(b)+"\n")
	if _, err := VerifyFiles(dir, &m); err != nil {
		t.Fatalf("the tampered backup must still verify by manifest: %v", err)
	}
	// The same script in a release directory other accounts can write is
	// no more trusted than the backup's copy.
	planted := filepath.Join(s.ReleaseRoot(), "r2", "pages")
	writeFile(t, planted, script)
	if err := os.Chmod(planted, 0o777); err != nil {
		t.Fatal(err)
	}
	// The production probe (a real exec) and runner.
	o.Probe = nil
	served = 0
	res, err = Check(ctx, o, dir)
	if _, serr := os.Stat(sentinel); serr == nil {
		t.Fatal("check-integrity executed the backup's binary")
	}
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "verified" || res.ProbedWith != "" || res.Notice != UntrustedBinaryNotice || res.ServedWith == "" || served != 1 {
		t.Fatalf("tampered: %+v served=%d", res, served)
	}
	out, _ := json.Marshal(res)
	if !strings.Contains(string(out), `"notice":"captured binary not executed; served with this release"`) {
		t.Fatalf("report %s", out)
	}
}
