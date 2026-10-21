// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package update

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

type fixture struct {
	dir, app string
	out      bytes.Buffer
	fake     *runner.Fake
	err      error
	// services records "<systemctl verb> <release marker>" per call.
	services []string
}

func write(t *testing.T, name, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if mode == 0 {
		mode = 0o644
	}
	if err := os.WriteFile(name, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// marker names the release behind APP_DIR: the Go candidate's binary, or
// the original (legacy Node) install's release-marker file.
func marker(app string) string {
	if b, err := os.ReadFile(filepath.Join(app, "pages")); err == nil {
		return string(b)
	}
	b, _ := os.ReadFile(filepath.Join(app, "release-marker"))
	return string(b)
}

func run(t *testing.T, failure string) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir()}
	f.app = filepath.Join(f.dir, "app")
	source := filepath.Join(f.dir, "source")
	if err := os.MkdirAll(filepath.Join(source, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	motd, err := os.ReadFile("../../../deploy/motd")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(source, "deploy/motd"), string(motd), 0)
	write(t, filepath.Join(f.app, "release-marker"), "original", 0)
	if failure != "legacy-start" {
		write(t, filepath.Join(f.app, "lib/readiness.js"), "", 0)
	}
	write(t, filepath.Join(f.app, "assets/chart.txt"), "Northwind asset bytes", 0)
	write(t, filepath.Join(f.app, ".env"), "LOCAL_SETTING=preserved\n", 0)
	write(t, filepath.Join(f.dir, "pages.env"), "PORT=4312\nOPERATOR_SETTING=preserved\n", 0)
	write(t, filepath.Join(f.dir, "pages.service"), "original service\n", 0)
	write(t, filepath.Join(f.dir, "pages-cli"), "original CLI\n", 0o755)
	if failure == "unmanaged-link" {
		if err := os.Rename(f.app, f.app+"-external"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(f.app+"-external", f.app); err != nil {
			t.Fatal(err)
		}
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	f.fake = &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
		args := strings.Join(c.Args, " ")
		switch c.Name {
		case "git":
			switch {
			case strings.Contains(args, "--abbrev-ref"):
				return "main\n", 0
			case strings.Contains(args, "rev-parse"):
				return "abcdef0\n", 0
			}
			return "", 0
		case "runuser":
			switch {
			case strings.Contains(args, " preflight") && failure == "module":
				return "", 1
			case strings.Contains(args, " check-migrations") && failure == "compatibility":
				return "", 1
			}
			return "", 0
		case "systemctl":
			m := marker(f.app)
			f.services = append(f.services, c.Args[0]+" "+m)
			if c.Args[0] == "start" && strings.HasSuffix(failure, "start") && m == "candidate" {
				return "", 1
			}
		}
		return "", 0
	}}
	settings := install.Settings{
		InstallConfig: filepath.Join(f.dir, "install"), AppDir: f.app, AppUser: me.Username,
		SrcDir: source, EnvFile: filepath.Join(f.dir, "pages.env"), CLITarget: filepath.Join(f.dir, "pages-cli"), Port: "4312",
	}
	var opts Options
	opts = Options{
		Settings: settings, Runner: f.fake, Log: term.Buffer(&f.out),
		NoPull: failure != "pull", Yes: true, SkipTemplateSync: true,
		ServiceFile: filepath.Join(f.dir, "pages.service"), MotdFile: filepath.Join(f.dir, "motd"),
		Build: func(_ context.Context, _, out string) error {
			if failure == "build" {
				return errors.New("go build: synthetic failure")
			}
			return os.WriteFile(out, []byte("candidate"), 0o755)
		},
		Handoff: func(ctx context.Context, _, candidate string) error { return Activate(ctx, opts, candidate) },
		Probe: func(_ context.Context, url string) bool {
			if !strings.Contains(url, ":4312/") {
				return false
			}
			return failure != "readiness" || marker(f.app) != "candidate"
		},
		Sleep: func(time.Duration) {},
		Rename: func(oldpath, newpath string) error {
			if failure == "activate" && newpath == f.app {
				if target, _ := os.Readlink(oldpath); strings.Contains(target, "/abcdef0-") {
					return errors.New("synthetic activation failure")
				}
			}
			return os.Rename(oldpath, newpath)
		},
		Chown: func(context.Context, string, bool) error { return nil },
	}
	f.err = Run(context.Background(), opts)
	return f
}

func TestReleaseUpdate(t *testing.T) {
	deployMotd, err := os.ReadFile("../../../deploy/motd")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"build", "module", "compatibility", "activate", "start", "legacy-start", "readiness", "unmanaged-link", "pull", "none"} {
		t.Run(failure, func(t *testing.T) {
			f := run(t, failure)
			succeeds := failure == "none" || failure == "pull"
			if (f.err == nil) != succeeds {
				t.Fatalf("err = %v, want success=%v\n%s", f.err, succeeds, f.out.String())
			}
			if succeeds {
				if got := read(t, filepath.Join(f.app, "pages")); got != "candidate" {
					t.Fatalf("active release = %q", got)
				}
			} else if got := read(t, filepath.Join(f.app, "release-marker")); got != "original" {
				t.Fatalf("active release marker = %q\n%s", got, f.out.String())
			}
			if got := read(t, filepath.Join(f.app, "assets/chart.txt")); got != "Northwind asset bytes" {
				t.Fatalf("assets = %q", got)
			}
			if got := read(t, filepath.Join(f.app, ".env")); got != "LOCAL_SETTING=preserved\n" {
				t.Fatalf(".env = %q", got)
			}
			if got := read(t, filepath.Join(f.dir, "pages.env")); got != "PORT=4312\nOPERATOR_SETTING=preserved\n" {
				t.Fatalf("service env = %q", got)
			}
			if !succeeds {
				if got := read(t, filepath.Join(f.dir, "pages.service")); got != "original service\n" {
					t.Fatalf("service file = %q", got)
				}
				if got := read(t, filepath.Join(f.dir, "pages-cli")); got != "original CLI\n" {
					t.Fatalf("cli = %q", got)
				}
			}
			switch failure {
			case "activate", "start", "legacy-start", "readiness":
				if !strings.Contains(f.out.String(), "previous release is ready again") {
					t.Fatalf("no recovery report:\n%s", f.out.String())
				}
				if !strings.Contains(strings.Join(f.services, "\n"), "start original") {
					t.Fatalf("predecessor never restarted: %v", f.services)
				}
			}
			if failure == "legacy-start" && !strings.Contains(f.fake.Log(), " ready-check") {
				t.Fatalf("a /healthz-only predecessor must be confirmed by the new release's ready-check:\n%s", f.fake.Log())
			}
			if succeeds {
				if st, err := os.Lstat(f.app); err != nil || st.Mode()&os.ModeSymlink == 0 {
					t.Fatal("APP_DIR is not the release symlink")
				}
				if got := read(t, filepath.Join(f.app+".releases", "previous", "release-marker")); got != "original" {
					t.Fatalf("previous = %q", got)
				}
				if got := read(t, filepath.Join(f.dir, "motd")); got != string(deployMotd) {
					t.Fatalf("motd = %q", got)
				}
				if st, _ := os.Stat(filepath.Join(f.dir, "motd")); st.Mode().Perm() != 0o644 {
					t.Fatalf("motd mode = %v", st.Mode())
				}
				if target, _ := os.Readlink(filepath.Join(f.dir, "pages-cli")); target != filepath.Join(f.app, "pages") {
					t.Fatalf("CLI must be the active binary, links to %q", target)
				}
				if unit := read(t, filepath.Join(f.dir, "pages.service")); !strings.Contains(unit, "ExecStart="+filepath.Join(f.app, "pages")+" serve") {
					t.Fatalf("unit:\n%s", unit)
				}
				for _, want := range []string{" preflight", " check-migrations", " migrate"} {
					if !strings.Contains(f.fake.Log(), want) {
						t.Fatalf("missing %q in:\n%s", want, f.fake.Log())
					}
				}
			} else if _, err := os.Stat(filepath.Join(f.dir, "motd")); err == nil {
				t.Fatal("a failed update must not touch the login banner")
			}
			if failure == "pull" && !strings.Contains(f.fake.Log(), "merge --ff-only origin/main") {
				t.Fatalf("no fast-forward:\n%s", f.fake.Log())
			}
			// No candidate directory survives, success or failure.
			if m, _ := filepath.Glob(filepath.Join(f.app+".releases", ".candidate.*")); len(m) != 0 {
				t.Fatalf("candidate left behind: %v", m)
			}
		})
	}
}

func TestUpdateLockExcludesConcurrentRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.update.lock")
	shared, err := install.Lock(path, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := install.Lock(path, true); err == nil {
		t.Fatal("an update must not start while a backup holds the shared lock")
	}
	if again, err := install.Lock(path, false); err != nil {
		t.Fatal("two backups may share the lock")
	} else {
		again.Close()
	}
	shared.Close()
	excl, err := install.Lock(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer excl.Close()
	if _, err := install.Lock(path, false); err == nil {
		t.Fatal("a backup must not start during an update")
	}
}
