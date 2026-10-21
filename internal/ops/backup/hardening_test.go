// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/testdb"
)

// TestRehearsalRestoresAsNonSuperuser: check-integrity restores a dump
// anyone who can write the backup can craft, as root. A dump whose CHECK
// constraint runs COPY ... TO PROGRAM during the restore must never run
// that program: the scratch cluster restores (and probes) as an ordinary
// role, so the restore fails instead. A clean dump restores and is probed
// by a role with no superuser, role/database creation or server-program
// privileges.
func TestRehearsalRestoresAsNonSuperuser(t *testing.T) {
	for _, tool := range []string{"pg_dump", "initdb", "pg_restore", "createdb", "createuser"} {
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
	d, dbURL := testdb.NewWithURL(t)
	ctx := context.Background()
	o := Options{Runner: runner.Exec{}, EUID: euid}
	o.defaults()

	// The scratch cluster's OS account must be able to create the sentinel
	// if the program ever runs, or the test would prove nothing.
	shared, err := os.MkdirTemp("", "northwind-sentinel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shared) })
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(shared, "restore-ran-a-program")

	clean := backupDir(t, dumpOf(t, dbURL))
	var privileged string
	restored, err := rehearse(ctx, o, clean, nil, func(ctx context.Context, _ string, env []string) (json.RawMessage, error) {
		e := envMap(env)
		pool, err := pgxpool.New(ctx, fmt.Sprintf("host=%s port=%s user=%s dbname=%s sslmode=disable", e["PGHOST"], e["PGPORT"], e["PGUSER"], e["PGDATABASE"]))
		if err != nil {
			return nil, err
		}
		defer pool.Close()
		err = pool.QueryRow(ctx, `SELECT concat_ws(',',
			CASE WHEN rolsuper THEN 'superuser' END, CASE WHEN rolcreaterole THEN 'createrole' END,
			CASE WHEN rolcreatedb THEN 'createdb' END,
			CASE WHEN pg_has_role(current_user, 'pg_execute_server_program', 'USAGE') THEN 'program' END,
			CASE WHEN pg_has_role(current_user, 'pg_read_server_files', 'USAGE') THEN 'read' END,
			CASE WHEN pg_has_role(current_user, 'pg_write_server_files', 'USAGE') THEN 'write' END)
			FROM pg_roles WHERE rolname = current_user`).Scan(&privileged)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(`{}`), nil
	})
	if err != nil || string(restored) != "{}" {
		t.Fatalf("clean rehearsal: %s %v", restored, err)
	}
	if privileged != "" {
		t.Fatalf("the restore role holds %s", privileged)
	}

	for _, q := range []string{
		`CREATE FUNCTION northwind_poison(v text) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $f$
		BEGIN
			IF current_database() = 'pages' THEN
				EXECUTE format('COPY (SELECT 1) TO PROGRAM %L', 'touch ` + sentinel + `');
			END IF;
			RETURN v IS NOT NULL;
		END $f$`,
		// The CHECK runs for every row the restore loads.
		"CREATE TABLE northwind_poison_rows (v text CHECK (northwind_poison(v)))",
		"INSERT INTO northwind_poison_rows VALUES ('northwind')",
	} {
		if _, err := d.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	poisoned := backupDir(t, dumpOf(t, dbURL))
	_, err = rehearse(ctx, o, poisoned, nil, func(context.Context, string, []string) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	})
	if _, serr := os.Stat(sentinel); serr == nil {
		t.Fatal("restoring the backup's dump ran a server program")
	}
	wantErr(t, err, `pg_restore failed`)
}

func dumpOf(t *testing.T, dbURL string) string {
	t.Helper()
	pgdump, _ := PostgresTool("pg_dump")
	u, err := url.Parse(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "database.dump")
	pw, _ := u.User.Password()
	cmd := exec.Command(pgdump, "--format=custom", "--no-owner", "--no-acl", "--file="+out)
	cmd.Env = append(os.Environ(), "PGHOST="+u.Hostname(), "PGPORT="+u.Port(), "PGUSER="+u.User.Username(),
		"PGPASSWORD="+pw, "PGDATABASE="+strings.TrimPrefix(u.Path, "/"), "PGSSLMODE=disable")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pg_dump: %v\n%s", err, b)
	}
	return out
}

func backupDir(t *testing.T, dump string) string {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "database.dump"), string(b))
	writeFile(t, filepath.Join(dir, "application/pages"), "#!/bin/false\n")
	writeFile(t, filepath.Join(dir, "config/service.env"), "NORTHWIND_FIXTURE=retained\n")
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestCaptureRefusesDestinationOthersCanWrite: root builds the capture
// inside the destination, so a destination another account could replace
// (a directory above it writable by others, or owned by another account)
// is refused before anything is written there.
func TestCaptureRefusesDestinationOthersCanWrite(t *testing.T) {
	s := installation(t, serviceEnv)
	shared := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(shared, "backups")
	_, err := Capture(context.Background(), captureOptions(fakeCluster(t, false, nil)), s, dest)
	wantErr(t, err, "backup destination must be a directory only root can change.*writable by other accounts")
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Fatalf("an untrusted destination received %v", entries)
	}
	if os.Geteuid() == 0 {
		owned := filepath.Join(t.TempDir(), "backups")
		if err := os.Mkdir(owned, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(owned, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		_, err := Capture(context.Background(), captureOptions(fakeCluster(t, false, nil)), s, owned)
		wantErr(t, err, "owned by uid 65534")
		if entries, _ := os.ReadDir(owned); len(entries) != 0 {
			t.Fatalf("a destination owned by another account received %v", entries)
		}
	}
	// A trusted destination still works, and its partial tree was private.
	dir, err := Capture(context.Background(), captureOptions(fakeCluster(t, false, nil)), s, filepath.Join(t.TempDir(), "backups"))
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("published backup mode %v %v", st, err)
	}
}

// TestCopyTreeNeverFollowsSwappedEntries: the assets tree is the service
// account's to write while root copies it. An entry replaced, after the
// listing, by a symlink to a root-only file or by a FIFO is refused: never
// read through the link, never waited on.
func TestCopyTreeNeverFollowsSwappedEntries(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "root-only")
	writeFile(t, secret, "contoso root-only secret\n")
	for name, swap := range map[string]func(path string) error{
		"symlink":     func(p string) error { return os.Symlink(secret, p) },
		"fifo":        func(p string) error { return syscall.Mkfifo(p, 0o600) },
		"dir symlink": func(p string) error { return os.Symlink(filepath.Dir(secret), p) },
	} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join(t.TempDir(), "assets")
			if name == "dir symlink" {
				writeFile(t, filepath.Join(source, "northwind.txt", "inner"), "Northwind inner\n")
			} else {
				writeFile(t, filepath.Join(source, "northwind.txt"), "Northwind chart\n")
			}
			afterListing = func(dir string) {
				if dir != source {
					return
				}
				victim := filepath.Join(source, "northwind.txt")
				if err := os.RemoveAll(victim); err != nil {
					t.Error(err)
				}
				if err := swap(victim); err != nil {
					t.Error(err)
				}
			}
			defer func() { afterListing = nil }()
			target := filepath.Join(t.TempDir(), "copy")
			done := make(chan error, 1)
			go func() { done <- copyTree(context.Background(), source, target, nil) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				// Unblock the reader so the test can finish.
				if f, ferr := os.OpenFile(filepath.Join(source, "northwind.txt"), os.O_WRONLY, 0); ferr == nil {
					f.Close()
				}
				t.Fatal("copy blocked on an entry swapped in after listing")
			}
			wantErr(t, err, "unsupported link or special file in capture")
			_ = filepath.Walk(target, func(p string, st os.FileInfo, err error) error {
				if err == nil && !st.IsDir() {
					if b, _ := os.ReadFile(p); strings.Contains(string(b), "root-only secret") {
						t.Errorf("copied the link's target into %s", p)
					}
				}
				return nil
			})
			if errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
