// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/backupprobe"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/server"
)

// testdata/nodeera was written by the last Node release's own
// scripts/backup.js (see testdata/make-nodeera-fixture.sh): three pages, six
// versions, a theme logo asset and a password portal, migrated to 026 by the
// Node migrator, with the Node probe's result recorded in manifest.restored.
const nodeEraFixture = "testdata/nodeera"

// copyBackup copies the fixture to a private directory a test may tamper with.
func copyBackup(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pages-backup-nodeera")
	if err := copyTree(context.Background(), nodeEraFixture, dir, nil); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readManifest(t *testing.T, dir string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeManifest(t *testing.T, dir string, m map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "manifest.json"), string(b)+"\n")
}

func nodeEraManifest(t *testing.T, dir string) *NodeEraManifest {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m NodeEraManifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return &m
}

func TestNodeEraManifestIsRecognised(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(nodeEraFixture, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if legacy, err := IsNodeEra(b); err != nil || !legacy {
		t.Fatalf("fixture: legacy=%v err=%v", legacy, err)
	}
	if legacy, err := IsNodeEra([]byte(`{"format_version":1,"status":"complete","pages_version":"1.0.0","go_version":"go1.27.2","files":[]}`)); err != nil || legacy {
		t.Fatalf("Go manifest: legacy=%v err=%v", legacy, err)
	}
	_, err = IsNodeEra([]byte(`{"format_version":1,"node_version":"v22.22.0","go_version":"go1.27.2"}`))
	wantErr(t, err, "unsupported or incomplete backup manifest")
	_, err = IsNodeEra([]byte(`[`))
	wantErr(t, err, "manifest.json")
}

func TestNodeEraFilesVerifyLikeBackupJS(t *testing.T) {
	if _, err := VerifyNodeEraFiles(nodeEraFixture, nodeEraManifest(t, nodeEraFixture)); err != nil {
		t.Fatal(err)
	}
	// The Go format's verification refuses it: no binary.
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(nodeEraFixture, "manifest.json"))
	_ = json.Unmarshal(b, &m)
	_, err := VerifyFiles(nodeEraFixture, &m)
	wantErr(t, err, "missing required recovery input: application/pages")

	cases := []struct {
		name, want string
		tamper     func(t *testing.T, dir string)
	}{
		{"changed application byte", "inventory/checksum mismatch", func(t *testing.T, dir string) {
			f := filepath.Join(dir, "application/server.js")
			b, _ := os.ReadFile(f)
			b[len(b)/2] ^= 1
			writeFile(t, f, string(b))
		}},
		{"changed dump", "inventory/checksum mismatch", func(t *testing.T, dir string) {
			f, err := os.OpenFile(filepath.Join(dir, "database.dump"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("x")
			f.Close()
		}},
		{"extra asset", "inventory/checksum mismatch", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "assets/fabrikam.txt"), "unexpected\n")
		}},
		{"repeated entry", "inventory/checksum mismatch", func(t *testing.T, dir string) {
			m := readManifest(t, dir)
			files := m["files"].([]any)
			m["files"] = append(files[:len(files)-1:len(files)-1], files[0])
			writeManifest(t, dir, m)
		}},
		{"lock file absent from files and manifest", "missing required recovery input: application/package-lock.json", func(t *testing.T, dir string) {
			_ = os.Remove(filepath.Join(dir, "application/package-lock.json"))
			m := readManifest(t, dir)
			var kept []any
			for _, f := range m["files"].([]any) {
				if f.(map[string]any)["path"] != "application/package-lock.json" {
					kept = append(kept, f)
				}
			}
			m["files"] = kept
			writeManifest(t, dir, m)
		}},
		{"optional configuration disagrees", "optional configuration disagrees with its files: config/local.env", func(t *testing.T, dir string) {
			m := readManifest(t, dir)
			m["optional_configuration"].(map[string]any)["local.env"] = true
			writeManifest(t, dir, m)
		}},
		{"incomplete", "unsupported or incomplete backup manifest", func(t *testing.T, dir string) {
			m := readManifest(t, dir)
			m["status"] = "partial"
			writeManifest(t, dir, m)
		}},
		{"missing config section", "missing required directory: config", func(t *testing.T, dir string) {
			_ = os.RemoveAll(filepath.Join(dir, "config"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := copyBackup(t)
			c.tamper(t, dir)
			_, err := VerifyNodeEraFiles(dir, nodeEraManifest(t, dir))
			wantErr(t, err, c.want)
			// Check refuses before any rehearsal starts.
			_, err = Check(context.Background(), Options{Runner: &runner.Fake{}, Tool: func(string) (string, error) {
				return "", errors.New("rehearsal must not start")
			}}, dir)
			wantErr(t, err, c.want)
		})
	}
}

// inProcessProbe is CurrentProbe without a child process: the isolated
// environment becomes this test process's (server.Run reads it), and the
// probe serves the restored data with this release's server in process.
func inProcessProbe(t *testing.T, served *int) func(ctx context.Context, assets string, env []string) (json.RawMessage, error) {
	return func(ctx context.Context, assets string, env []string) (json.RawMessage, error) {
		e := envMap(env)
		if e["PAGES_AUTH_MODE"] != "elcano" || e["RAW_TOKEN_SECRET"] == "" || strings.Contains(e["RAW_TOKEN_SECRET"], "northwind") {
			return nil, errors.New("probe did not get the isolated environment")
		}
		for k, v := range e {
			t.Setenv(k, v)
		}
		t.Setenv("DATABASE_URL", "")
		saved := backupprobe.Hooks.Serve
		backupprobe.Hooks.Serve = func(ctx context.Context, ln net.Listener) error {
			*served++
			return server.Run(ctx, ln, server.RunOptions{})
		}
		defer func() { backupprobe.Hooks.Serve = saved }()
		res, err := backupprobe.Run(ctx, assets)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}
}

// TestCheckIntegrityAcceptsNodeEraBackup restores the Node-era fixture into a
// real private cluster, runs the database half on the snapshot as captured,
// migrates the scratch copy and serves it with this release's server.
func TestCheckIntegrityAcceptsNodeEraBackup(t *testing.T) {
	for _, tool := range []string{"initdb", "pg_restore", "createdb"} {
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
	ctx := context.Background()
	served := 0
	o := Options{Runner: runner.Exec{}, EUID: euid, CurrentProbe: inProcessProbe(t, &served),
		Probe: func(context.Context, string, string, []string) (json.RawMessage, error) {
			return nil, errors.New("a Node-era backup must not run the captured application")
		}}
	dir := copyBackup(t)
	res, err := Check(ctx, o, dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "verified" || res.Format != "node" || res.Notice != NodeEraNotice || res.ServedWith == "" || served != 1 {
		t.Fatalf("result %+v served=%d", res, served)
	}
	var want bytes.Buffer
	_ = json.Compact(&want, nodeEraManifest(t, dir).Restored)
	if !bytes.Equal(want.Bytes(), res.Restored) {
		t.Fatalf("restored %s\nwant %s", res.Restored, want.Bytes())
	}
	var r struct {
		Migrations []string       `json:"migrations"`
		Probes     map[string]int `json:"probes"`
	}
	_ = json.Unmarshal(res.Restored, &r)
	// The inventory is the snapshot's (Node schema), the probes are served.
	if r.Migrations[len(r.Migrations)-1] != "026_password_reveal.sql" || r.Probes["pages"] != 3 || r.Probes["portal_pages"] != 1 {
		t.Fatalf("restored %s", res.Restored)
	}
	out, _ := json.Marshal(res)
	if !strings.Contains(string(out), `"notice":"legacy Node-era backup: application verified by manifest only; served with this release"`) {
		t.Fatalf("report %s", out)
	}

	// A manifest whose recorded rehearsal no longer matches what the dump
	// restores to fails (the manifest is not in its own inventory).
	tampered := copyBackup(t)
	m := readManifest(t, tampered)
	m["restored"].(map[string]any)["counts"].(map[string]any)["pages"] = "4"
	writeManifest(t, tampered, m)
	_, err = Check(ctx, o, tampered)
	wantErr(t, err, "restored database inventory differs from the backup manifest")

	// An HTTP half that fails fails the check.
	o.CurrentProbe = func(context.Context, string, []string) (json.RawMessage, error) {
		return nil, errors.New("restored page 1 returned 500, expected 403")
	}
	_, err = Check(ctx, o, copyBackup(t))
	wantErr(t, err, "restored page 1 returned 500")
}
