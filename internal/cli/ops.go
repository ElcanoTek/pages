// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
)

// The operator verbs' seams. Tests replace them; production uses the real
// system.
var (
	// Runner runs every external command the operator verbs need.
	Runner runner.Runner = runner.Default
	// Exec replaces the process (sudo/runuser re-execution, systemctl,
	// journalctl, `pages serve`). It only returns on failure.
	Exec = syscall.Exec
	// Geteuid is the effective uid.
	Geteuid = os.Geteuid
	// Executable is this binary's path.
	Executable = os.Executable
)

// settings resolves the installation layout from the environment and the
// persisted installation file.
func settings() install.Settings { return install.Load(os.Getenv) }

// passthroughEnv are the variables a re-executed verb must keep (sudo
// filters the environment).
var passthroughEnv = []string{
	"PAGES_UPDATE_YES", "PAGES_UPDATE_NO_PULL", "PAGES_SKIP_TEMPLATE_SYNC", "PAGES_SERVICE_FILE", "PAGES_MOTD_FILE",
	"PAGES_OS_RELEASE", "PAGES_BOOTSTRAP_NON_INTERACTIVE", "TERM",
}

// reexecArgv builds the argv that runs `pages verb args...` again under a
// privilege wrapper with the installation settings preserved.
func reexecArgv(wrapper []string, s install.Settings, self, verb string, args []string, extra ...string) []string {
	argv := append([]string{}, wrapper...)
	argv = append(argv, "env")
	argv = append(argv, s.Env()...)
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			argv = append(argv, k+"="+v)
		}
	}
	argv = append(argv, extra...)
	argv = append(argv, self, verb)
	return append(argv, args...)
}

// elevate re-executes `pages verb args...` through sudo when not root. It
// returns only when already root (false) or on failure (true, after
// printing why).
func elevate(verb string, args []string) (failed bool) {
	if Geteuid() == 0 {
		return false
	}
	self, err := Executable()
	if err != nil {
		Fail(verb, "%v", err)
		return true
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		Fail(verb, "needs root: run it as root (sudo is not installed)")
		return true
	}
	argv := reexecArgv([]string{sudo, "--"}, settings(), self, verb, args)
	err = Exec(sudo, argv, os.Environ())
	Fail(verb, "could not re-run with sudo: %v", err)
	return true
}

// execTool replaces the process with name args (systemctl, journalctl,
// caddy), so the operator gets its output, pager and exit code as is.
func execTool(verb, name string, args ...string) int {
	path, err := exec.LookPath(name)
	if err != nil {
		return Fail(verb, "%s not found", name)
	}
	err = Exec(path, append([]string{name}, args...), os.Environ())
	return Fail(verb, "%s: %v", name, err)
}

// signalContext is cancelled by SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// interrupted maps a cancelled context to the shell's 128+signal codes.
func interrupted(ctx context.Context) bool { return errors.Is(ctx.Err(), context.Canceled) }

// wantsHelp reports a -h/--help anywhere in args.
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return fmt.Sprint(Geteuid())
}

// checkoutRoot finds the Pages checkout at or above dir (go.mod naming the
// Pages module).
func checkoutRoot(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if b, err := os.ReadFile(filepath.Join(d, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/ElcanoTek/pages\n") {
			return d
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

// umaskPrivate makes every file a backup writes private to its owner.
func umaskPrivate() { syscall.Umask(0o077) }
