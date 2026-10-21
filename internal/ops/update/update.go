// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package update is `pages update` / `pages rebuild`: build a complete
// release, then atomically switch the active symlink. The predecessor is
// retained and restored on activation/startup/readiness failure. Database
// changes are never reversed; only pending migrations explicitly declared
// backward-compatible run here.
//
// Layout (default paths):
//
//	/opt/pages            → symlink to the active release
//	/opt/pages.releases/  <sha>-<UTC>-<pid>/pages (the binary), assets → /opt/pages.assets
//	/opt/pages.releases/previous → the last successful predecessor
//	/opt/pages.assets     writable assets shared by every release
//	/opt/pages.env        optional local .env shared by every release
//	/usr/local/bin/pages  → /opt/pages/pages (the CLI is the active binary)
//
// The work happens in two stages. Stage one (whichever binary the operator
// ran — possibly the previous release, or the scripts/update.sh shim's
// temporary build) fast-forwards the checkout and builds the candidate
// binary. It then hands off to the CANDIDATE (`pages update --activate DIR`),
// so the code that preflights, migrates, renders the systemd unit and decides
// readiness is always the code being installed — the Go counterpart of the
// shell updater re-executing itself when scripts/update.sh changed.
package update

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

// Options configures an update. Zero hooks take production defaults.
type Options struct {
	Settings install.Settings
	Runner   runner.Runner
	Log      *term.Printer

	// NoPull builds the current checkout without fetching (pages rebuild).
	NoPull bool
	// Yes skips the confirmation prompt (PAGES_UPDATE_YES=1).
	Yes bool
	// SkipTemplateSync skips `pages template sync` after activation.
	SkipTemplateSync bool
	// ServiceFile is the installed unit (PAGES_SERVICE_FILE).
	ServiceFile string
	// MotdFile is the login banner (PAGES_MOTD_FILE).
	MotdFile string
	// Stdin answers the prompt when Interactive.
	Stdin       io.Reader
	Interactive bool

	// Build compiles the checkout at src into the binary out.
	Build func(ctx context.Context, src, out string) error
	// Handoff runs stage two in the candidate binary. The default execs it
	// (never returning on success).
	Handoff func(ctx context.Context, binary, candidate string) error
	// Probe GETs url and reports a 2xx answer.
	Probe func(ctx context.Context, url string) bool
	// Sleep waits between readiness polls.
	Sleep func(time.Duration)
	// Rename is os.Rename (tests inject activation failures).
	Rename func(oldpath, newpath string) error
	// Now is the release-name clock.
	Now func() time.Time
	// Chown gives path to the service account (recursively for a
	// directory when recursive; the link itself for a symlink).
	Chown func(ctx context.Context, path string, recursive bool) error
	// ProcCwd resolves a process's working directory (/proc/PID/cwd): the
	// release a running service process was started from.
	ProcCwd func(pid int) (string, error)
}

func (o *Options) defaults() {
	if o.Runner == nil {
		o.Runner = runner.Default
	}
	if o.Log == nil {
		o.Log = term.New()
	}
	if o.ServiceFile == "" {
		o.ServiceFile = "/etc/systemd/system/" + install.Service
	}
	if o.MotdFile == "" {
		o.MotdFile = "/etc/motd"
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	if o.Build == nil {
		o.Build = o.makeBuild
	}
	if o.Handoff == nil {
		o.Handoff = execHandoff
	}
	if o.Probe == nil {
		o.Probe = httpProbe
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	if o.Rename == nil {
		o.Rename = os.Rename
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Chown == nil {
		o.Chown = o.chownUser
	}
	if o.ProcCwd == nil {
		o.ProcCwd = func(pid int) (string, error) { return filepath.EvalSymlinks(fmt.Sprintf("/proc/%d/cwd", pid)) }
	}
}

// Error is an update error with an operator-facing message. Reported is
// set when the message has already been printed (stage two prints it before
// its recovery lines, so the order reads like the shell updater's).
type Error struct {
	Msg      string
	Reported bool
}

func (e *Error) Error() string { return e.Msg }

func fail(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

func (o *Options) git(ctx context.Context, args ...string) (string, error) {
	return runner.Output(ctx, o.Runner, runner.Cmd{Name: "git", Args: append([]string{"-C", o.Settings.SrcDir}, args...)})
}

// precheck validates the installation before any change.
func (o *Options) precheck() error {
	s := o.Settings
	if err := s.Validate(); err != nil {
		o.Log.PlainErr(err.Error())
		return fail("invalid installation settings")
	}
	if _, err := os.Stat(filepath.Join(s.SrcDir, ".git")); err != nil {
		return fail("no source checkout at %s", s.SrcDir)
	}
	if st, err := os.Stat(s.AppDir); err != nil || !st.IsDir() {
		return fail("no installed application at %s — run bootstrap first", s.AppDir)
	}
	if st, err := os.Stat(s.EnvFile); err != nil || !st.Mode().IsRegular() {
		return fail("missing environment file %s", s.EnvFile)
	}
	if isSymlink(s.AppDir) {
		target, err1 := filepath.EvalSymlinks(s.AppDir)
		root, err2 := resolveMissing(s.ReleaseRoot())
		st, err3 := os.Stat(s.SharedAssets())
		if err1 != nil || err2 != nil || !strings.HasPrefix(target, root+"/") || err3 != nil || !st.IsDir() {
			return fail("unmanaged or incomplete application symlink at %s; reconcile its release/assets layout before updating", s.AppDir)
		}
	}
	return nil
}

// Run is stage one: lock, fetch, confirm, build the candidate, hand off.
func Run(ctx context.Context, o Options) error {
	o.defaults()
	s := o.Settings
	if err := o.precheck(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.ReleaseRoot(), 0o755); err != nil {
		return err
	}
	lock, err := install.Lock(s.LockFile(), true)
	if err != nil {
		return fail("another Pages update is running for %s", s.AppDir)
	}
	locked := true
	defer func() {
		if locked {
			lock.Close()
		}
	}()
	if !o.NoPull {
		o.Log.Plain("Fetching latest")
		if _, err := o.git(ctx, "fetch", "--quiet", "origin"); err != nil {
			return fail("git fetch failed: %v", err)
		}
		branch, _ := o.git(ctx, "rev-parse", "--abbrev-ref", "HEAD")
		if branch == "" || branch == "HEAD" {
			branch = "main"
		}
		if _, err := o.Runner.Run(ctx, runner.Cmd{Name: "git", Args: []string{"-C", s.SrcDir, "merge", "--ff-only", "origin/" + branch}, Stdout: o.Log.Out, Stderr: o.Log.Err}); err != nil {
			return fail("fast-forward failed — resolve the source checkout first")
		}
	}
	if !o.Yes && o.Interactive {
		fmt.Fprint(o.Log.Out, "Build and activate a new release? [y/N] ")
		answer, _ := bufio.NewReader(o.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "y", "yes":
		default:
			return fail("aborted")
		}
	}
	o.Log.Plain("Building release")
	candidate, err := os.MkdirTemp(s.ReleaseRoot(), ".candidate.")
	if err != nil {
		return err
	}
	handed := false
	defer func() {
		if !handed {
			_ = os.RemoveAll(candidate)
		}
	}()
	if err := os.Chmod(candidate, 0o755); err != nil {
		return err
	}
	bin := filepath.Join(candidate, install.BinaryName)
	if err := o.Build(ctx, s.SrcDir, bin); err != nil {
		o.Log.PlainErr(err.Error())
		return fail("build failed — installed service untouched")
	}
	if err := o.Chown(ctx, candidate, true); err != nil {
		return fail("could not give %s to %s: %v", candidate, s.AppUser, err)
	}
	// The candidate takes its own lock; release ours first so the handoff
	// never deadlocks against itself.
	lock.Close()
	locked = false
	handed = true
	if err := o.Handoff(ctx, bin, candidate); err != nil {
		_ = os.RemoveAll(candidate)
		return err
	}
	return nil
}

// Activate is stage two, run by the candidate binary: preflight, migrate,
// switch, start, verify — or restore the predecessor.
func Activate(ctx context.Context, o Options, candidate string) (err error) {
	o.defaults()
	s := o.Settings
	if dir := os.Getenv("PAGES_UPDATE_SHIM_DIR"); strings.HasPrefix(dir, os.TempDir()+"/pages-update-") {
		_ = os.RemoveAll(dir) // the scripts/update.sh bootstrap build has done its job
	}
	if err := o.precheck(); err != nil {
		_ = os.RemoveAll(candidate)
		return err
	}
	root, _ := resolveMissing(s.ReleaseRoot())
	if c, cerr := resolveMissing(candidate); cerr != nil || filepath.Dir(c) != root || !strings.HasPrefix(filepath.Base(c), ".candidate.") {
		return fail("refusing to activate %s: not a candidate under %s", candidate, s.ReleaseRoot())
	}
	lock, lerr := install.Lock(s.LockFile(), true)
	if lerr != nil {
		return fail("another Pages update is running for %s", s.AppDir)
	}
	defer lock.Close()

	a := &activation{o: &o, candidate: candidate}
	defer func() { err = a.cleanup(err) }()
	return a.run(ctx)
}

type activation struct {
	o                                      *Options
	candidate, release, previous, recovery string
	switching                              bool
	nextLink                               string
}

// appCommand runs argv as the service account with the service environment,
// from directory — the way the service itself sees its configuration.
func (o *Options) appCommand(ctx context.Context, directory string, argv ...string) error {
	script := `env_file="$1"; directory="$2"; shift 2
set -a; . "$env_file"; set +a
export PAGES_ENV=production
cd "$directory"
exec "$@"`
	args := append([]string{"-u", o.Settings.AppUser, "--", "bash", "-c", script, "_", o.Settings.EnvFile, directory}, argv...)
	_, err := o.Runner.Run(ctx, runner.Cmd{Name: "runuser", Args: args, Stdout: o.Log.Out, Stderr: o.Log.Err})
	return err
}

func (a *activation) run(ctx context.Context) error {
	o, s := a.o, a.o.Settings
	bin := filepath.Join(a.candidate, install.BinaryName)
	if err := o.appCommand(ctx, a.candidate, bin, "preflight"); err != nil {
		return fail("release preflight failed — installed service untouched")
	}
	if err := o.appCommand(ctx, a.candidate, bin, "check-migrations"); err != nil {
		return fail("migration compatibility check failed — installed service untouched")
	}
	if err := o.appCommand(ctx, a.candidate, bin, "migrate"); err != nil {
		return fail("migrations failed — installed service untouched")
	}
	sha, err := o.git(ctx, "rev-parse", "--short", "HEAD")
	if err != nil || sha == "" {
		sha = "unknown"
	}
	stamp := o.Now().UTC().Format("20060102T150405Z")
	a.release = filepath.Join(s.ReleaseRoot(), fmt.Sprintf("%s-%s-%d", sha, stamp, os.Getpid()))
	if err := o.Rename(a.candidate, a.release); err != nil {
		return err
	}
	a.candidate = ""

	// Snapshot the small installed control files before stopping anything.
	// Release contents are retained; mutable paths live outside releases.
	if a.recovery, err = os.MkdirTemp(s.ReleaseRoot(), ".recovery."); err != nil {
		return err
	}
	for _, f := range [][2]string{{o.ServiceFile, "service"}, {s.CLITarget, "cli"}} {
		if _, err := os.Lstat(f[0]); err == nil {
			if err := install.CopyPreserving(f[0], filepath.Join(a.recovery, f[1])); err != nil {
				return err
			}
		}
	}
	legacy := !isSymlink(s.AppDir)
	if legacy {
		a.previous = filepath.Join(s.ReleaseRoot(), fmt.Sprintf("legacy-%s-%d", stamp, os.Getpid()))
		if exists(s.SharedAssets()) {
			return fail("%s already exists; reconcile it before converting the legacy install", s.SharedAssets())
		}
		if exists(s.SharedEnv()) {
			return fail("%s already exists; reconcile it before converting the legacy install", s.SharedEnv())
		}
	} else if a.previous, err = filepath.EvalSymlinks(s.AppDir); err != nil {
		return err
	}

	o.Log.Plain("Activating " + a.release)
	a.switching = true
	// A failed stop can leave the old process serving: switching the link
	// and control files under it would make the later start a no-op and let
	// the old process's readiness pass for the new release.
	if err := o.systemctl(ctx, "stop", install.Service); err != nil {
		return fail("could not stop %s (%v) — active release not switched", install.Service, err)
	}
	if legacy {
		if err := o.Rename(s.AppDir, a.previous); err != nil {
			return err
		}
		if exists(filepath.Join(a.previous, "assets")) {
			if err := o.Rename(filepath.Join(a.previous, "assets"), s.SharedAssets()); err != nil {
				return err
			}
		} else if err := os.Mkdir(s.SharedAssets(), 0o755); err != nil {
			return err
		}
		if err := os.Symlink(s.SharedAssets(), filepath.Join(a.previous, "assets")); err != nil {
			return err
		}
		if exists(filepath.Join(a.previous, ".env")) {
			if err := o.Rename(filepath.Join(a.previous, ".env"), s.SharedEnv()); err != nil {
				return err
			}
			if err := os.Symlink(s.SharedEnv(), filepath.Join(a.previous, ".env")); err != nil {
				return err
			}
		}
	}
	if err := os.Symlink(s.SharedAssets(), filepath.Join(a.release, "assets")); err != nil {
		return err
	}
	if exists(s.SharedEnv()) {
		if err := os.Symlink(s.SharedEnv(), filepath.Join(a.release, ".env")); err != nil {
			return err
		}
	}
	if err := o.Chown(ctx, s.SharedAssets(), false); err != nil {
		return err
	}
	if err := o.Chown(ctx, filepath.Join(a.release, "assets"), false); err != nil {
		return err
	}
	if err := a.switchLink(a.release); err != nil {
		return err
	}
	unit, err := s.RenderService()
	if err != nil {
		return err
	}
	if err := install.WriteFileAtomic(o.ServiceFile, []byte(unit), 0o644); err != nil {
		return err
	}
	if err := install.SymlinkAtomic(s.Binary(), s.CLITarget); err != nil {
		return err
	}
	if err := o.systemctl(ctx, "daemon-reload"); err != nil {
		return fail("systemctl daemon-reload failed (%v) — the new unit was not loaded", err)
	}
	if err := o.systemctl(ctx, "start", install.Service); err != nil {
		return fail("new release failed to start")
	}
	if !a.ready(ctx) {
		return fail("new release failed readiness")
	}
	if err := a.servingRelease(ctx); err != nil {
		return err
	}
	if err := install.SymlinkAtomic(a.previous, filepath.Join(s.ReleaseRoot(), "previous")); err != nil {
		return err
	}
	a.switching = false
	o.Log.Plain(fmt.Sprintf("Pages ready on :%s; previous release retained at %s", s.Port, a.previous))
	o.Log.Plain("Installed control-file backup retained at " + a.recovery)

	if !o.SkipTemplateSync {
		if err := o.appCommand(ctx, s.AppDir, s.Binary(), "template", "sync"); err != nil {
			o.Log.PlainErr("template sync reported problems; previous revisions remain current")
		}
	}
	a.syncMotd()
	o.Log.Plain("update complete")
	return nil
}

// syncMotd keeps the login banner in sync with deploy/motd: boxes
// bootstrapped before the banner existed kept the image's motd. It reads the
// root-owned source checkout the release was built from; nonfatal, the
// release is already active.
func (a *activation) syncMotd() {
	o := a.o
	want, err := os.ReadFile(filepath.Join(o.Settings.SrcDir, "deploy", "motd"))
	if err != nil {
		return
	}
	if have, err := os.ReadFile(o.MotdFile); err == nil && string(have) == string(want) {
		return
	}
	if err := install.WriteFileAtomic(o.MotdFile, want, 0o644); err != nil {
		o.Log.PlainErr("could not install the login banner to " + o.MotdFile)
		return
	}
	o.Log.Plain("motd installed/refreshed")
}

func (a *activation) switchLink(target string) error {
	s := a.o.Settings
	a.nextLink = fmt.Sprintf("%s.next.%d", s.AppDir, os.Getpid())
	_ = os.Remove(a.nextLink)
	if err := os.Symlink(target, a.nextLink); err != nil {
		return err
	}
	if err := a.o.Rename(a.nextLink, s.AppDir); err != nil {
		return err
	}
	return nil
}

// ready polls readiness of the release now behind APP_DIR. A predecessor
// from before /readyz existed answers only /healthz; then the NEW release's
// bounded database readiness probe (`pages ready-check`) stands in for the
// schema half.
func (a *activation) ready(ctx context.Context) bool {
	o, s := a.o, a.o.Settings
	endpoint := "readyz"
	if !exists(filepath.Join(s.AppDir, install.BinaryName)) && !exists(filepath.Join(s.AppDir, "lib", "readiness.js")) {
		endpoint = "healthz"
	}
	url := fmt.Sprintf("http://127.0.0.1:%s/%s", s.Port, endpoint)
	for attempt := 0; attempt < 10; attempt++ {
		if o.Probe(ctx, url) {
			if endpoint == "readyz" {
				return true
			}
			if a.release != "" && o.appCommand(ctx, a.release, filepath.Join(a.release, install.BinaryName), "ready-check") == nil {
				return true
			}
		}
		o.Sleep(time.Second)
	}
	return false
}

// servingRelease confirms the service's main process runs the new release:
// a start that was a no-op on a process left over from the predecessor would
// otherwise pass readiness on the old code. A definite mismatch fails; an
// unreadable MainPID or cwd is left to readiness, which has already passed.
func (a *activation) servingRelease(ctx context.Context) error {
	o := a.o
	out, err := runner.Output(ctx, o.Runner, runner.Cmd{Name: "systemctl", Args: []string{"show", "-p", "MainPID", "--value", install.Service}})
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(out)
	if err != nil || pid <= 0 {
		return nil
	}
	running, err := o.ProcCwd(pid)
	if err != nil || running == "" {
		return nil
	}
	want, err := filepath.EvalSymlinks(a.release)
	if err != nil {
		want = a.release
	}
	if running != want {
		return fail("%s (pid %d) still runs %s, not the new release %s", install.Service, pid, running, a.release)
	}
	return nil
}

func (a *activation) restoreFile(name, target string) error {
	src := filepath.Join(a.recovery, name)
	if _, err := os.Lstat(src); err == nil {
		return install.CopyPreserving(src, target)
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// cleanup restores the predecessor when the failure happened after the
// service was stopped, and always removes an unconsumed candidate.
func (a *activation) cleanup(result error) error {
	o, s := a.o, a.o.Settings
	ctx := context.Background()
	if result != nil {
		o.Log.PlainErr(result.Error())
	}
	if a.switching {
		o.Log.PlainErr("update failed; restoring the previous compatible release")
		restored := true
		if err := o.systemctl(ctx, "stop", install.Service); err != nil {
			o.Log.PlainErr(fmt.Sprintf("could not stop %s: %v", install.Service, err))
			restored = false
		}
		if st, err := os.Stat(a.previous); err == nil && st.IsDir() {
			// Repair the first legacy conversion if it failed between moving
			// a mutable path and installing its shared symlink.
			for _, l := range [][2]string{{s.SharedAssets(), "assets"}, {s.SharedEnv(), ".env"}} {
				if link := filepath.Join(a.previous, l[1]); exists(l[0]) && !lexists(link) {
					if err := os.Symlink(l[0], link); err != nil {
						o.Log.PlainErr("could not relink " + link + ": " + err.Error())
						restored = false
					}
				}
			}
			if a.nextLink != "" {
				_ = os.Remove(a.nextLink)
			}
			if err := a.switchLink(a.previous); err != nil {
				o.Log.PlainErr("could not switch back to " + a.previous + ": " + err.Error())
				restored = false
			}
		}
		if a.recovery != "" {
			for _, f := range [][2]string{{"service", o.ServiceFile}, {"cli", s.CLITarget}} {
				if err := a.restoreFile(f[0], f[1]); err != nil {
					o.Log.PlainErr("could not restore " + f[1] + ": " + err.Error())
					restored = false
				}
			}
		}
		if err := o.systemctl(ctx, "daemon-reload"); err != nil {
			o.Log.PlainErr(fmt.Sprintf("systemctl daemon-reload failed: %v", err))
			restored = false
		}
		// Start regardless (best effort), but only a fully restored install
		// — after a successful stop, so the started process is known to be
		// the predecessor rather than whatever kept running — is reported
		// as recovered.
		started := o.systemctl(ctx, "start", install.Service) == nil && a.ready(ctx)
		if restored && started {
			o.Log.PlainErr("previous release is ready again; the update failed and was not committed")
		} else {
			o.Log.PlainErr(fmt.Sprintf("automatic recovery needs operator attention; predecessor=%s; configuration backup=%s", a.previous, a.recovery))
		}
	}
	if a.candidate != "" {
		_ = os.RemoveAll(a.candidate)
	}
	if a.nextLink != "" {
		_ = os.Remove(a.nextLink)
	}
	if result != nil {
		return &Error{Msg: result.Error(), Reported: true}
	}
	return nil
}

func (o *Options) systemctl(ctx context.Context, args ...string) error {
	_, err := o.Runner.Run(ctx, runner.Cmd{Name: "systemctl", Args: args, Stdout: o.Log.Out, Stderr: o.Log.Err})
	return err
}

// makeBuild is the production builder: the checkout's own Makefile, which
// stamps the version and sets GOTOOLCHAIN=auto (a stock Fedora golang is
// usually older than go.mod's pin and its go.env says GOTOOLCHAIN=local).
func (o *Options) makeBuild(ctx context.Context, src, out string) error {
	env := append(os.Environ(), "GOTOOLCHAIN=auto")
	_, err := o.Runner.Run(ctx, runner.Cmd{Name: "make", Args: []string{"-C", src, "build", "BIN=" + out}, Env: env, Stdout: o.Log.Out, Stderr: o.Log.Err})
	return err
}

func (o *Options) chownUser(ctx context.Context, path string, recursive bool) error {
	args := []string{}
	if recursive {
		args = append(args, "-R")
	} else {
		args = append(args, "-h")
	}
	args = append(args, o.Settings.AppUser+":"+o.Settings.AppUser, path)
	_, err := o.Runner.Run(ctx, runner.Cmd{Name: "chown", Args: args})
	return err
}

func execHandoff(_ context.Context, binary, candidate string) error {
	env := append(os.Environ(), "PAGES_UPDATE_NO_PULL=1", "PAGES_UPDATE_YES=1")
	return syscall.Exec(binary, []string{binary, "update", "--activate", candidate}, env)
}

func httpProbe(ctx context.Context, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func isSymlink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&os.ModeSymlink != 0
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// resolveMissing is `readlink -m`: the path with every existing prefix's
// symlinks resolved.
func resolveMissing(p string) (string, error) {
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r, nil
	}
	parent, err := resolveMissing(filepath.Dir(p))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(p)), nil
}
