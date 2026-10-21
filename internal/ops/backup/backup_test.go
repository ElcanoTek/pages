// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "database.dump"), "Northwind database fixture")
	writeFile(t, filepath.Join(dir, "application/pages"), "#!/bin/false\n")
	writeFile(t, filepath.Join(dir, "config/service.env"), "NORTHWIND_FIXTURE=retained\n")
	writeFile(t, filepath.Join(dir, "assets/northwind.txt"), "Northwind persistent bytes\n")
	return dir
}

func wantErr(t *testing.T, err error, pattern string) {
	t.Helper()
	if err == nil || !regexp.MustCompile(pattern).MatchString(err.Error()) {
		t.Fatalf("error %v, want /%s/", err, pattern)
	}
}

func TestInventoryRejectsExtraOrChangedFiles(t *testing.T) {
	dir := fixture(t)
	files, err := Inventory(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{FormatVersion: 1, Status: "complete", Files: files}
	b, _ := json.Marshal(m)
	writeFile(t, filepath.Join(dir, "manifest.json"), string(b))
	got, err := VerifyFiles(dir, m)
	if err != nil || !reflect.DeepEqual(got, files) {
		t.Fatal(got, err)
	}
	writeFile(t, filepath.Join(dir, "assets/northwind.txt"), "Northwind changed bytes\n")
	_, err = VerifyFiles(dir, m)
	wantErr(t, err, "checksum mismatch")
	writeFile(t, filepath.Join(dir, "assets/northwind.txt"), "Northwind persistent bytes\n")
	writeFile(t, filepath.Join(dir, "assets/contoso.txt"), "unexpected additional artifact")
	_, err = VerifyFiles(dir, m)
	wantErr(t, err, "checksum mismatch")
}

func TestVerifyRejectsIncompleteManifestsAndMissingInputs(t *testing.T) {
	dir := fixture(t)
	files, _ := Inventory(dir)
	for _, m := range []*Manifest{{}, {FormatVersion: 2, Status: "complete", Files: files},
		{FormatVersion: 1, Status: "partial", Files: files}, {FormatVersion: 1, Status: "complete"}} {
		_, err := VerifyFiles(dir, m)
		wantErr(t, err, "unsupported or incomplete")
	}
	// A manifest whose JSON has no "files" is incomplete; an empty list is not.
	var m Manifest
	_ = json.Unmarshal([]byte(`{"format_version":1,"status":"complete"}`), &m)
	_, err := VerifyFiles(dir, &m)
	wantErr(t, err, "unsupported or incomplete")
	if err := os.Remove(filepath.Join(dir, "config/service.env")); err != nil {
		t.Fatal(err)
	}
	files, _ = Inventory(dir)
	_, err = VerifyFiles(dir, &Manifest{FormatVersion: 1, Status: "complete", Files: files})
	wantErr(t, err, "missing required recovery input: config/service.env")
}

func TestInventoryRejectsLinks(t *testing.T) {
	dir := fixture(t)
	if err := os.Symlink(filepath.Join(dir, "application/pages"), filepath.Join(dir, "assets/linked")); err != nil {
		t.Fatal(err)
	}
	_, err := Inventory(dir)
	wantErr(t, err, "unsupported link or special file")
}

func TestIsolatedEnvReplacesTargetsAndCredentials(t *testing.T) {
	base := []string{"DATABASE_URL=postgres://northwind:fixture@192.0.2.1/live", "PGHOST=/northwind-live",
		"PGPORT=5497", "PGDATABASE=northwind_live", "PGUSER=northwind_live", "PGPASSWORD=northwind-fixture",
		"PGSERVICE=northwind-service", "PGSERVICEFILE=/northwind-service.conf", "PGPASSFILE=/northwind-pass",
		"PGOPTIONS=-c default_transaction_read_only=off", "PAGES_DATA_MAX_BYTES=999999", "MAX_HTML_BYTES=999mb",
		"RAW_TOKEN_SECRET=original-northwind-fixture", "API_TOKEN_PEPPER=original-northwind-fixture",
		"PAGE_COOKIE_SECRET=original-northwind-fixture", "PAGES_DEV_LOGIN=1"}
	env := IsolatedEnv(base, "/tmp/northwind-private", map[string]string{"PAGES_DATA_MAX_BYTES": "2048", "MAX_HTML_BYTES": "3mb",
		"DATABASE_URL": "postgres://ignored.invalid/live", "PGHOST": "/ignored-live", "PAGE_COOKIE_SECRET": "ignored-secret"})
	pg := map[string]string{}
	for k, v := range env {
		if strings.HasPrefix(k, "PG") {
			pg[k] = v
		}
	}
	if !reflect.DeepEqual(pg, map[string]string{"PGHOST": "/tmp/northwind-private", "PGPORT": "5432", "PGUSER": "pages_backup", "PGDATABASE": "pages"}) {
		t.Fatal(pg)
	}
	if _, ok := env["DATABASE_URL"]; ok {
		t.Fatal("DATABASE_URL carried")
	}
	if env["PAGES_DATA_MAX_BYTES"] != "2048" || env["MAX_HTML_BYTES"] != "3mb" || env["PAGES_DEV_LOGIN"] != "0" {
		t.Fatal(env)
	}
	for _, k := range []string{"RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "PAGE_COOKIE_SECRET"} {
		if env[k] == "" || env[k] == "original-northwind-fixture" || env[k] == "ignored-secret" {
			t.Fatalf("%s = %q", k, env[k])
		}
	}
}

func TestIsolatedEnvNeverCarriesCentralSignIn(t *testing.T) {
	env := IsolatedEnv([]string{"PAGES_AUTH_MODE=central", "PAGES_PUBLIC_URL=https://pages.northwind.example",
		"AUTH_ISSUER_URL=https://auth.northwind.example", "AUTH_CLIENT_ID=pages",
		"AUTH_CLIENT_SECRET=northwind-central-client-fixture-0000000000", "AUTH_SIGNING_PUBKEY=northwind-key-fixture",
		"PAGES_AUTH_COOKIE_SECURE=1", "AUTH_COOKIE_NAME=northwind_auth"}, "/tmp/northwind-private", nil)
	if env["PAGES_AUTH_MODE"] != "elcano" || env["AUTH_SIGNING_PUBKEY"] != "" {
		t.Fatal(env)
	}
	for _, k := range []string{"PAGES_PUBLIC_URL", "AUTH_ISSUER_URL", "AUTH_CLIENT_ID", "AUTH_CLIENT_SECRET", "PAGES_AUTH_COOKIE_SECURE", "AUTH_COOKIE_NAME"} {
		if _, ok := env[k]; ok {
			t.Fatalf("%s carried", k)
		}
	}
}

// The fake scratch cluster: initdb creates the data dir, pg_ctl start
// "starts" it (marker file) and then fails, stop removes the marker unless
// told to fail, status answers 0 running / 3 stopped.
func TestFailedStartStopsOrRetainsOwnedCluster(t *testing.T) {
	for _, retain := range []bool{false, true} {
		t.Run(map[bool]string{false: "stops", true: "retains"}[retain], func(t *testing.T) {
			dir := fixture(t)
			var events []string
			var data string
			fake := &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
				args := c.Args
				if c.Name == "runuser" {
					// Root delegation: -u postgres -- tool args...
					if args[0] != "-u" || args[1] != "postgres" || args[2] != "--" {
						t.Errorf("bad delegation %v", args)
					}
					c.Name, args = args[3], args[4:]
				}
				env := envMap(c.Env)
				if c.Name != "chown" {
					if env["PGHOST"] == "" || !strings.HasSuffix(env["PGHOST"], "/socket") || env["PGPORT"] != "5432" || env["PGDATABASE"] != "pages" {
						t.Errorf("%s connects to %v", c.Name, env)
					}
					if _, ok := env["DATABASE_URL"]; ok {
						t.Errorf("%s inherited DATABASE_URL", c.Name)
					}
				}
				tool := filepath.Base(c.Name)
				for i, a := range args {
					if a == "-D" {
						if data == "" {
							data = args[i+1]
						} else if args[i+1] != data {
							t.Errorf("controls a foreign cluster %s", args[i+1])
						}
					}
				}
				switch tool {
				case "initdb":
					events = append(events, "initdb")
					_ = os.MkdirAll(data, 0o700)
				case "pg_ctl":
					verb := args[len(args)-1]
					events = append(events, verb)
					switch verb {
					case "start":
						_ = os.WriteFile(filepath.Join(data, "fixture-running"), []byte("owned fixture cluster"), 0o600)
						return "", 19
					case "stop":
						if retain {
							return "", 20
						}
						_ = os.Remove(filepath.Join(data, "fixture-running"))
					case "status":
						if _, err := os.Stat(filepath.Join(data, "fixture-running")); err == nil {
							return "", 0
						}
						return "", 3
					}
				case "pg_restore":
					return "", 99
				}
				return "", 0
			}}
			o := Options{Runner: fake, EUID: 0, Environ: []string{"PGHOST=/northwind-live", "PGPORT=5497", "PGDATABASE=northwind_live", "DATABASE_URL=postgres://ignored.invalid/live"},
				Tool: func(name string) (string, error) { return "/fake/bin/" + name, nil }}
			_, _, err := Rehearse(context.Background(), o, dir, nil)
			if retain {
				wantErr(t, err, `cannot confirm scratch PostgreSQL stopped; retained`)
			} else {
				wantErr(t, err, `pg_ctl failed \(19\)`)
			}
			want := []string{"initdb", "start", "stop"}
			if retain {
				want = append(want, "status")
			}
			if !reflect.DeepEqual(events, want) {
				t.Fatalf("events %v, want %v", events, want)
			}
			scratch := filepath.Dir(data)
			defer os.RemoveAll(scratch)
			if _, err := os.Stat(scratch); (err == nil) != retain {
				t.Fatalf("scratch retained = %v, want %v", err == nil, retain)
			}
			if retain {
				if b, _ := os.ReadFile(filepath.Join(data, "fixture-running")); string(b) != "owned fixture cluster" {
					t.Fatal("running cluster storage was touched")
				}
			}
		})
	}
}

func TestDumpEnvironmentConnectsWhereTheApplicationDoes(t *testing.T) {
	pg := func(url string) map[string]string {
		t.Helper()
		env, err := DumpEnvironment(map[string]string{"DATABASE_URL": url})
		if err != nil {
			t.Fatalf("%s: %v", url, err)
		}
		out := map[string]string{}
		for k, v := range env {
			if strings.HasPrefix(k, "PG") {
				out[k] = v
			}
		}
		return out
	}
	eq := func(got, want map[string]string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	// The shape bootstrap writes.
	eq(pg("postgres://pages:0f3a@127.0.0.1:5432/pages?sslmode=disable"),
		map[string]string{"PGHOST": "127.0.0.1", "PGPORT": "5432", "PGUSER": "pages", "PGPASSWORD": "0f3a", "PGDATABASE": "pages", "PGSSLMODE": "disable"})
	// The URL is the only source: PG* beside it is refused, never guessed at.
	for _, extra := range []string{"PGSSLMODE", "PGHOSTADDR", "PGUSER", "PGSERVICE"} {
		_, err := DumpEnvironment(map[string]string{"DATABASE_URL": "postgres://pages:p@db.test/pages", extra: "x"})
		wantErr(t, err, `set together with PG\* variables`)
	}
	if env, _ := DumpEnvironment(map[string]string{"DATABASE_URL": "postgres://p:q@h/d", "KEEP": "kept"}); env["KEEP"] != "kept" || env["DATABASE_URL"] != "" {
		t.Fatal(env)
	}
	// Decoding and precedence follow the driver.
	eq(pg("postgresql://pages:fix%40ture@%2Ftmp%2Fpg-sock/north%20wind"),
		map[string]string{"PGHOST": "/tmp/pg-sock", "PGUSER": "pages", "PGPASSWORD": "fix@ture", "PGDATABASE": "north wind", "PGSSLMODE": "disable"})
	if h := pg("postgres://pages:p@/pages?host=%2Ftmp%2Fpg-sock&port=5497")["PGHOST"]; h != "/tmp/pg-sock" {
		t.Fatal(h)
	}
	eq(pg("postgres://old:old@localhost:5432/pages?user=new&password=new&port=5497"),
		map[string]string{"PGHOST": "localhost", "PGPORT": "5497", "PGUSER": "new", "PGPASSWORD": "new", "PGDATABASE": "pages", "PGSSLMODE": "disable"})
	if h := pg("postgres://p:q@[::1]:5433/d")["PGHOST"]; h != "::1" {
		t.Fatal(h)
	}
	if h := pg("postgres://p:q@/d")["PGHOST"]; h != "localhost" {
		t.Fatal(h)
	}
	if v := pg("postgres://p:q@h/d?sslmode=verify-full"); v["PGSSLMODE"] != "verify-full" || v["PGSSLROOTCERT"] != "system" {
		t.Fatal(v)
	}
	// Without a URL the PG* settings are the source, untouched.
	if env, _ := DumpEnvironment(map[string]string{"PGDATABASE": "northwind", "PGHOST": "/s"}); !reflect.DeepEqual(env, map[string]string{"PGDATABASE": "northwind", "PGHOST": "/s"}) {
		t.Fatal(env)
	}
	leak := regexp.MustCompile("hunter-fixture|tail|%00|\x00|forged|/etc/northwind|mysql:|\n")
	for _, c := range []struct{ url, why string }{
		{"not a url", `is not a postgres:// connection URL`},
		{"mysql://pages:hunter-fixture@host/pages", `is not a postgres:// connection URL`},
		{"postgres://pages:hunter-fixture@host/pages?sslrootcert=/etc/northwind.crt", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture@host/pages?options=-c%20x", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture@host/pages?sslmode=disable&sslmode=verify-full", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture@host/pages?sslmode=verify-full&sslmode=disable", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture@host/pages?sslmode=disable&sslmode=require", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture@host/pages?port=5432&port=6543", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture@host/pages?hunter-fixture-in-a-key=1", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture@host/pages?x%0Aforged-line%20hunter-fixture=1", `unsupported or repeated option`},
		{"postgres://pages:hunter-fixture%00tail@host/pages", `NUL character`},
		{"postgres://pages@host/pages?password=hunter-fixture%00tail", `NUL character`},
		{"postgres://hunter-fixture%00tail:p@host/pages", `NUL character`},
		{"postgres://pages:p@hunter-fixture%00tail/pages", `NUL character`},
		{"postgres://pages:p@host/hunter-fixture%00tail", `NUL character`},
		{"postgres://pages:hunter-fixture@host/pages?sslmode=require", `sslmode must be disable or verify-full`},
		{"postgres://pages:hunter-fixture@host/pages?sslmode=prefer", `sslmode must be disable or verify-full`},
	} {
		_, err := DumpEnvironment(map[string]string{"DATABASE_URL": c.url})
		wantErr(t, err, c.why)
		if leak.MatchString(err.Error()) {
			t.Fatalf("%s: error quotes the URL: %v", c.url, err)
		}
	}
}

func TestRedactionNeverEchoesConnectionStringsOrSecretParts(t *testing.T) {
	minted := strings.Repeat("0123456789abcdef", 3)
	url := "postgres://pages:" + minted + "@127.0.0.1:5432/pages?sslmode=disable"
	env := map[string]string{"DATABASE_URL": url, "PGPASSWORD": minted, "RAW_TOKEN_SECRET": "northwind-raw-fixture-value"}
	leaks := regexp.MustCompile(`(?i)0123456789ab|postgres(ql)?://|northwind-raw`)
	for _, line := range []string{
		`FATAL:  database "` + url[:63] + `" does not exist`,
		`FATAL:  database "` + ("postgres://pages:alpha'" + strings.Repeat("x", 70) + "@h/p")[:63] + `" does not exist`,
		`error at "PostgreSQL://pages:` + minted[:20] + `"`,
		`password authentication failed; tried ` + minted[:30],
	} {
		if got := Redact(line, env); got != RemovedLine {
			t.Fatalf("%q → %q", line, got)
		}
	}
	if got := Redact("token northwind-raw-fixture-value refused\nplain context", env); got != "token [redacted] refused\nplain context" {
		t.Fatal(got)
	}
	for _, line := range []string{"full=" + minted + " cut=" + minted[:20], "a=northwind-raw-fixture-value b=northwind-raw-fix", "b=northwind-raw-fix a=" + minted} {
		if got := Redact(line, env); got != RemovedLine {
			t.Fatalf("%q → %q", line, got)
		}
	}
	split := NewStderrCollector(env, 0)
	for _, p := range []string{"pg_dump: error: FATAL: database \"postg", "res://pages:0123", "456789ab\" does not exist\n", "pg_dump: hint\n"} {
		_, _ = split.Write([]byte(p))
	}
	if got := split.Text(); got != RemovedLine+"\npg_dump: hint" {
		t.Fatal(got)
	}
	tail := NewStderrCollector(env, 200)
	_, _ = tail.Write([]byte(strings.Repeat("noise ", 40) + "\nFATAL: database \"" + url[:63] + "\"\n" + strings.Repeat("z", 150) + "\n"))
	if got := tail.Text(); leaks.MatchString(got) {
		t.Fatal(got)
	}
	big := NewStderrCollector(env, 100)
	_, _ = big.Write([]byte(strings.Repeat("a", 150)))
	_, _ = big.Write([]byte(minted[12:] + " tail of the oversized line\nnext line\n"))
	if got := big.Text(); got != RemovedLine+"\nnext line" {
		t.Fatal(got)
	}
	oversized := strings.Repeat("x", 150) + "tail\nnext line\n"
	var chars []string
	for _, c := range oversized {
		chars = append(chars, string(c))
	}
	for _, pieces := range [][]string{{oversized}, {oversized[:154], oversized[154:]}, chars} {
		c := NewStderrCollector(env, 100)
		for _, p := range pieces {
			_, _ = c.Write([]byte(p))
		}
		if got := c.Text(); got != RemovedLine+"\nnext line" {
			t.Fatalf("%d chunk(s): %q", len(pieces), got)
		}
	}
	last := NewStderrCollector(env, 0)
	_, _ = last.Write([]byte(`FATAL: "` + url))
	if got := last.Text(); got != RemovedLine {
		t.Fatal(got)
	}
}

func TestChildThatCannotStartNeverReportsItsValues(t *testing.T) {
	_, err := Run(context.Background(), runner.Exec{}, "/bin/true", nil, []string{"PGPASSWORD=hunter-fixture\x00tail"}, "")
	if err == nil || err.Error() != "true could not start (EINVAL)" || strings.Contains(err.Error(), "hunter") {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), runner.Exec{}, "/nonexistent/northwind-tool", nil, []string{"PGPASSWORD=hunter-fixture"}, "")
	if err == nil || err.Error() != "northwind-tool could not start (ENOENT)" {
		t.Fatal(err)
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatal("not a ToolError")
	}
}

// installation builds a release-managed install for Capture.
func installation(t *testing.T, envText string) install.Settings {
	t.Helper()
	dir := t.TempDir()
	release := filepath.Join(dir, "app.releases", "r1")
	writeFile(t, filepath.Join(release, "pages"), "northwind binary")
	writeFile(t, filepath.Join(dir, "app.assets", "northwind-chart.txt"), "Northwind persistent chart bytes\n")
	if err := os.Symlink(filepath.Join(dir, "app.assets"), filepath.Join(release, "assets")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "local.env"), "NORTHWIND_LOCAL_SETTING=retained\n")
	if err := os.Symlink(filepath.Join(dir, "local.env"), filepath.Join(release, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(release, filepath.Join(dir, "app")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "pages.env"), envText)
	writeFile(t, filepath.Join(dir, "install"), "PAGES_APP_DIR=\"${PAGES_APP_DIR:-"+filepath.Join(dir, "app")+"}\"\n")
	return install.Settings{AppDir: filepath.Join(dir, "app"), AppUser: "contoso", EnvFile: filepath.Join(dir, "pages.env"),
		InstallConfig: filepath.Join(dir, "install"), Port: "4317"}
}

const serviceEnv = `DATABASE_URL='postgres://pages:northwind-fixture-pass@localhost:5432/northwind?sslmode=disable'
RAW_TOKEN_SECRET='northwind-backup-raw-fixture'
API_TOKEN_PEPPER='northwind-backup-token-fixture'
PAGE_COOKIE_SECRET='northwind-backup-cookie-fixture'
PAGES_DATA_MAX_BYTES='2048'
`

// fakeCluster scripts every PostgreSQL tool; pg_dump writes a dump unless
// dumpFails.
func fakeCluster(t *testing.T, dumpFails bool, seen *map[string]string) *runner.Fake {
	return &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
		name, args := c.Name, c.Args
		if name == "runuser" {
			name, args = args[3], args[4:]
		}
		switch filepath.Base(name) {
		case "pg_dump":
			if len(args) > 0 && args[0] == "--version" {
				return "pg_dump (PostgreSQL) 16.4\n", 0
			}
			if seen != nil {
				*seen = envMap(c.Env)
			}
			if dumpFails {
				_, _ = c.Stderr.Write([]byte("pg_dump: error: connection to postgres://pages:northwind-fixture-pass@x failed\n"))
				return "", 27
			}
			_, _ = c.Stdout.Write([]byte("PGDMP northwind"))
		case "initdb":
			for i, a := range args {
				if a == "-D" {
					_ = os.MkdirAll(args[i+1], 0o700)
				}
			}
		}
		return "", 0
	}}
}

func captureOptions(fake *runner.Fake) Options {
	return Options{Runner: fake, EUID: 1000, Environ: []string{"PATH=/usr/bin", "RAW_TOKEN_SECRET=inherited-northwind-fixture"},
		Tool: func(name string) (string, error) { return "/fake/bin/" + name, nil },
		Probe: func(_ context.Context, binary, assets string, env []string) (json.RawMessage, error) {
			if _, err := os.Stat(binary); err != nil {
				return nil, errors.New("trusted binary missing")
			}
			if strings.Contains(binary, "pages-backup-") || strings.Contains(binary, ".pages-partial-") {
				return nil, errors.New("probe executed a binary from the backup: " + binary)
			}
			return json.RawMessage(`{"counts": {"pages": "0"}, "probes": {}}`), nil
		},
		// Never the default: it would execute this test binary.
		CurrentProbe: func(context.Context, string, []string) (json.RawMessage, error) {
			return nil, errors.New("served with this release")
		}}
}

// withReleases trusts the fixture installation's release binaries.
func withReleases(o Options, s install.Settings) Options {
	o.ReleaseRoot = s.ReleaseRoot()
	return o
}

func TestCaptureRequiresServiceSecretsAndTheSharedLock(t *testing.T) {
	for _, secret := range []string{"RAW_TOKEN_SECRET", "API_TOKEN_PEPPER", "PAGE_COOKIE_SECRET"} {
		var kept []string
		for _, l := range strings.Split(serviceEnv, "\n") {
			if !strings.HasPrefix(l, secret+"=") {
				kept = append(kept, l)
			}
		}
		s := installation(t, strings.Join(kept, "\n"))
		dest := filepath.Join(t.TempDir(), "backups")
		// An inherited value must not mask the service file's omission.
		o := captureOptions(fakeCluster(t, false, nil))
		o.Environ = append(o.Environ, secret+"=inherited-northwind-fixture")
		_, err := Capture(context.Background(), o, s, dest)
		wantErr(t, err, "service environment must define "+secret)
		if entries, _ := os.ReadDir(dest); len(entries) != 0 {
			t.Fatal("configuration rejection left an artifact")
		}
	}
	s := installation(t, serviceEnv)
	lock, err := install.Lock(s.LockFile(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, err = Capture(context.Background(), captureOptions(fakeCluster(t, false, nil)), s, filepath.Join(t.TempDir(), "b"))
	wantErr(t, err, "a Pages update is running")
}

func TestCapturePublishesCompleteBackupsOnly(t *testing.T) {
	s := installation(t, serviceEnv)
	dest := filepath.Join(t.TempDir(), "backups")
	var dumpEnv map[string]string
	dir, err := Capture(context.Background(), captureOptions(fakeCluster(t, false, &dumpEnv)), s, dest)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(dir) != dest {
		t.Fatal(dir)
	}
	// pg_dump connected with the service URL's parts, not argv, not the
	// caller's inherited secret.
	if dumpEnv["PGPASSWORD"] != "northwind-fixture-pass" || dumpEnv["PGDATABASE"] != "northwind" || dumpEnv["RAW_TOKEN_SECRET"] != "northwind-backup-raw-fixture" {
		t.Fatal(dumpEnv)
	}
	var m Manifest
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Status != "complete" || m.FormatVersion != 1 || m.RuntimeConfiguration["PAGES_DATA_MAX_BYTES"] != "2048" ||
		!m.OptionalConfiguration.InstallEnv || !m.OptionalConfiguration.LocalEnv || compact(m.Restored) != `{"counts":{"pages":"0"},"probes":{}}` {
		t.Fatalf("%s", b)
	}
	for archived, original := range map[string]string{"config/service.env": s.EnvFile, "config/install.env": s.InstallConfig,
		"assets/northwind-chart.txt": filepath.Join(s.SharedAssets(), "northwind-chart.txt"), "application/pages": filepath.Join(s.AppDir, "pages")} {
		a, _ := os.ReadFile(filepath.Join(dir, archived))
		o, _ := os.ReadFile(original)
		if string(a) != string(o) {
			t.Fatalf("%s does not preserve bytes", archived)
		}
	}
	for _, absent := range []string{"application/assets", "application/.env"} {
		if _, err := os.Lstat(filepath.Join(dir, absent)); err == nil {
			t.Fatalf("%s captured", absent)
		}
	}
	if st, _ := os.Lstat(filepath.Join(dir, "assets")); st.Mode()&os.ModeSymlink != 0 {
		t.Fatal("assets archived as a link")
	}
	if _, err := VerifyFiles(dir, &m); err != nil {
		t.Fatal(err)
	}
	res, err := Check(context.Background(), withReleases(captureOptions(fakeCluster(t, false, nil)), s), dir)
	if err != nil || res.Status != "verified" {
		t.Fatal(res, err)
	}
	// Corruption never verifies.
	f, _ := os.OpenFile(filepath.Join(dir, "assets/northwind-chart.txt"), os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString("corrupted\n")
	f.Close()
	_, err = Check(context.Background(), withReleases(captureOptions(fakeCluster(t, false, nil)), s), dir)
	wantErr(t, err, "checksum mismatch")

	// A failed dump publishes nothing, partial or complete, and its error
	// never echoes the connection string.
	failed := filepath.Join(t.TempDir(), "failed")
	_, err = Capture(context.Background(), captureOptions(fakeCluster(t, true, nil)), s, failed)
	wantErr(t, err, `pg_dump failed \(27\)`)
	if strings.Contains(err.Error(), "northwind-fixture-pass") || strings.Contains(err.Error(), "postgres://") {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(failed); len(entries) != 0 {
		t.Fatalf("failed capture left %v", entries)
	}
}

func compact(b []byte) string {
	var out bytes.Buffer
	_ = json.Compact(&out, b)
	return out.String()
}

func TestCaptureRefusesDestinationInsideTheInstall(t *testing.T) {
	s := installation(t, serviceEnv)
	_, err := Capture(context.Background(), captureOptions(fakeCluster(t, false, nil)), s, filepath.Join(s.SharedAssets(), "b"))
	wantErr(t, err, "outside application and asset directories")
}
