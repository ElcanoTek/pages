// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
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
	// running is the directory the simulated service process was started
	// in ("" when stopped); stops counts systemctl stop calls.
	running string
	stops   int
	// chowns records every Chown ("<path> recursive=<bool>"); handoff the
	// binary stage one executed, with its mode and owner at that moment;
	// asApp each binary a runuser command ran, with its mode then.
	chowns  []string
	handoff string
	asApp   []string
}

// describe is "<path> <mode> uid=<uid>" for a file as it is now.
func describe(t *testing.T, p string) string {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		return p + " missing"
	}
	return fmt.Sprintf("%s %o uid=%d", p, st.Mode().Perm(), st.Sys().(*syscall.Stat_t).Uid)
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
	switch failure {
	case "linked-worktree":
		// .git as a "gitdir:" file naming Git data outside the checkout.
		if err := os.Remove(filepath.Join(source, ".git")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(source, ".git"), "gitdir: "+filepath.Join(f.dir, "elsewhere")+"\n", 0)
	case "commondir":
		write(t, filepath.Join(source, ".git", "commondir"), filepath.Join(f.dir, "elsewhere")+"\n", 0)
	}
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
	f.running = f.app
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
			for i, a := range c.Args {
				if a == "_" && i+3 < len(c.Args) {
					f.asApp = append(f.asApp, describe(t, c.Args[i+3])+" "+c.Args[i+4])
				}
			}
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
			switch c.Args[0] {
			case "stop":
				f.stops++
				switch {
				case failure == "stop" && f.stops == 1:
					return "", 1 // the old process stays up
				case failure == "stale" && f.stops == 1:
					return "", 0 // "stopped", yet the old process keeps serving
				}
				f.running = ""
			case "daemon-reload":
				if failure == "reload" && m == "candidate" {
					return "", 1
				}
			case "start":
				if f.running != "" {
					return "", 0 // systemctl start on an active unit is a no-op
				}
				if strings.HasSuffix(failure, "start") && m == "candidate" {
					return "", 1
				}
				f.running, _ = filepath.EvalSymlinks(f.app)
			case "show":
				if f.running == "" {
					return "0\n", 0
				}
				return "4242\n", 0
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
			switch failure {
			case "symlink-build":
				// A build that leaves a link: root must not follow it into
				// a file another account could control.
				elsewhere := filepath.Join(f.dir, "elsewhere")
				if err := os.WriteFile(elsewhere, []byte("candidate"), 0o755); err != nil {
					return err
				}
				return os.Symlink(elsewhere, out)
			case "writable-build":
				if err := os.WriteFile(out, []byte("candidate"), 0o755); err != nil {
					return err
				}
				return os.Chmod(out, 0o777)
			}
			return os.WriteFile(out, []byte("candidate"), 0o755)
		},
		Handoff: func(ctx context.Context, bin, candidate string) error {
			f.handoff = describe(t, bin) + " " + describe(t, candidate)
			return Activate(ctx, opts, candidate)
		},
		Probe: func(_ context.Context, url string) bool {
			if !strings.Contains(url, ":4312/") {
				return false
			}
			// The process answering is whichever one is running — after a
			// no-op start, the old one.
			return f.running != "" && (failure != "readiness" || marker(f.running) != "candidate")
		},
		ProcCwd: func(int) (string, error) { return f.running, nil },
		Sleep:   func(time.Duration) {},
		Rename: func(oldpath, newpath string) error {
			if failure == "activate" && newpath == f.app {
				if target, _ := os.Readlink(oldpath); strings.Contains(target, "/abcdef0-") {
					return errors.New("synthetic activation failure")
				}
			}
			return os.Rename(oldpath, newpath)
		},
		Chown: func(_ context.Context, p string, recursive bool) error {
			f.chowns = append(f.chowns, fmt.Sprintf("%s recursive=%v", p, recursive))
			return nil
		},
	}
	if failure == "unsafe-root" {
		// A release root the service account could write to.
		if err := os.Mkdir(settings.ReleaseRoot(), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(settings.ReleaseRoot(), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	f.err = Run(context.Background(), opts)
	return f
}

func TestReleaseUpdate(t *testing.T) {
	deployMotd, err := os.ReadFile("../../../deploy/motd")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"build", "module", "compatibility", "activate", "start", "legacy-start", "readiness", "stop", "stale", "reload", "unmanaged-link", "pull", "none", "writable-build", "symlink-build", "unsafe-root", "linked-worktree", "commondir"} {
		t.Run(failure, func(t *testing.T) {
			f := run(t, failure)
			succeeds := failure == "none" || failure == "pull" || failure == "writable-build"
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
			case "activate", "start", "legacy-start", "readiness", "stop", "stale", "reload":
				if !strings.Contains(f.out.String(), "previous release is ready again") {
					t.Fatalf("no recovery report:\n%s", f.out.String())
				}
				if !strings.Contains(strings.Join(f.services, "\n"), "start original") {
					t.Fatalf("predecessor never restarted: %v", f.services)
				}
			}
			if failure == "stop" {
				// A failed stop aborts before anything is switched: no
				// systemctl call ever sees the candidate behind APP_DIR.
				for _, call := range f.services {
					if strings.HasSuffix(call, " candidate") {
						t.Fatalf("activation continued after a failed stop: %v", f.services)
					}
				}
				if !strings.Contains(f.out.String(), "could not stop pages.service") {
					t.Fatalf("stop failure not reported:\n%s", f.out.String())
				}
			}
			if failure == "stale" && !strings.Contains(f.out.String(), "still runs") {
				t.Fatalf("a no-op start onto the old process must fail the update:\n%s", f.out.String())
			}
			if failure == "reload" && !strings.Contains(f.out.String(), "daemon-reload failed") {
				t.Fatalf("daemon-reload failure not reported:\n%s", f.out.String())
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
			switch failure {
			case "symlink-build":
				if !strings.Contains(f.err.Error(), "refusing to hand off") || f.handoff != "" {
					t.Fatalf("a linked build output must not be executed:\n%s", f.out.String())
				}
			case "linked-worktree", "commondir":
				if !strings.Contains(f.out.String()+f.err.Error(), "plain clone") || f.handoff != "" || strings.Contains(f.fake.Log(), "fetch") {
					t.Fatalf("an external Git directory must stop the update before git runs: %v\n%s\n%s", f.err, f.out.String(), f.fake.Log())
				}
			case "unsafe-root":
				if !strings.Contains(f.out.String()+f.err.Error(), "unsafe installation directory") || f.handoff != "" {
					t.Fatalf("a writable release root must stop the update: %v\n%s", f.err, f.out.String())
				}
			}
			if succeeds {
				// Stage one executed a root-owned 0755 binary in a 0755
				// candidate, never one the service account could swap; no
				// release directory was ever given to the service account.
				euid := os.Geteuid()
				if !strings.Contains(f.handoff, "/.candidate.") || strings.Count(f.handoff, fmt.Sprintf(" 755 uid=%d", euid)) != 2 {
					t.Fatalf("handoff: %s", f.handoff)
				}
				for _, c := range f.chowns {
					if strings.Contains(c, "recursive=true") || (strings.Contains(c, ".releases/") && !strings.HasSuffix(c, "/assets recursive=false")) {
						t.Fatalf("release handed to the service account: %v", f.chowns)
					}
				}
				// The service account still ran the root-owned binary (world
				// readable and executable) for every preflight step.
				for _, want := range []string{"preflight", "check-migrations", "migrate"} {
					found := false
					for _, a := range f.asApp {
						if strings.HasSuffix(a, " "+want) && strings.Contains(a, fmt.Sprintf(" 755 uid=%d ", euid)) {
							found = true
						}
					}
					if !found {
						t.Fatalf("%s not run as the service account on a 0755 binary: %v", want, f.asApp)
					}
				}
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
