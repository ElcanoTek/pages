// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"

	"github.com/ElcanoTek/pages/internal/ops/envfile"
	"github.com/ElcanoTek/pages/internal/ops/install"
)

// The app-data verbs (token, template, access) talk to the database the
// server uses, and `token` must hash with the SERVICE's API_TOKEN_PEPPER — a
// token minted with a different or absent pepper is one the server never
// matches. On an installed box neither is in the operator's shell: they live
// in the service environment file (root:pages 0640). So, like the unit
// itself, these verbs run as the service account with that file's
// assignments, and `sudo pages token add agent deploy` just works.
//
// In development and tests the environment is already set (pages dev, the
// test cluster): when DATABASE_URL or API_TOKEN_PEPPER is present, or there
// is no environment file, the verb runs exactly as invoked.
//
// The file is parsed with envfile, never sourced: a tampered file must not
// run code as whoever invoked the CLI. Its values are applied in-process by
// the service account (which can read the file), never passed on a command
// line, where any local user could read them from the process table.

// serviceEnvMarker marks a verb already re-executed as the service account.
const serviceEnvMarker = "PAGES_CLI_AS_SERVICE_USER=1"

// serviceEnvStep is what an app-data verb does before it runs.
type serviceEnvStep int

const (
	// runAsInvoked: development or tests — the environment is the caller's.
	runAsInvoked serviceEnvStep = iota
	// loadHere: this process is (or must act as) the service account; apply
	// the environment file in-process and run.
	loadHere
	// becomeService: re-execute as the service account (argv is the command).
	becomeService
)

// serviceEnvInput is everything serviceEnvPlan decides on.
type serviceEnvInput struct {
	Settings      install.Settings
	Getenv        func(string) string
	EUID          int
	Me            string // the current account name
	AccountExists bool   // the service account exists
	EnvFileExists bool
	HaveSudo      string // sudo's path, "" when not installed
	Runuser       string // runuser's path
	Self          string // this executable
}

// serviceEnvPlan decides how verb runs, and the argv to re-execute for
// becomeService. A non-empty msg is an error to report instead.
func serviceEnvPlan(in serviceEnvInput, verb string, args []string) (step serviceEnvStep, argv []string, msg string) {
	switch {
	case in.Getenv("DATABASE_URL") != "" || in.Getenv("API_TOKEN_PEPPER") != "":
		return runAsInvoked, nil, ""
	case !in.EnvFileExists:
		return runAsInvoked, nil, ""
	case in.Getenv("PAGES_CLI_AS_SERVICE_USER") == "1", in.Me == in.Settings.AppUser, !in.AccountExists:
		// Already the service account (or there is none to become: read the
		// file as whoever this is, which on such a box is root).
		return loadHere, nil, ""
	case in.EUID == 0:
		runuser := in.Runuser
		if runuser == "" {
			runuser = "runuser"
		}
		return becomeService, reexecArgv([]string{runuser, "-u", in.Settings.AppUser, "--"}, in.Settings, in.Self, verb, args, serviceEnvMarker), ""
	case in.HaveSudo != "":
		return becomeService, reexecArgv([]string{in.HaveSudo, "-u", in.Settings.AppUser, "--"}, in.Settings, in.Self, verb, args, serviceEnvMarker), ""
	default:
		return 0, nil, fmt.Sprintf("needs the service environment (%s): run it with sudo, or as %s", in.Settings.EnvFile, in.Settings.AppUser)
	}
}

// applyServiceEnv sets every assignment in file that the process environment
// does not already hold (an empty value counts as unset, as it does for the
// plan), plus PAGES_ENV=production as the unit sets it.
func applyServiceEnv(file string) error {
	f, err := envfile.Read(file)
	if err != nil {
		return err
	}
	for k, v := range f.Map() {
		if os.Getenv(k) == "" {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	if os.Getenv("PAGES_ENV") == "" {
		return os.Setenv("PAGES_ENV", "production")
	}
	return nil
}

// withServiceEnv prepares an app-data verb's environment. It returns
// done=true with the exit code when the verb must not continue in this
// process (re-executed, or failed).
func withServiceEnv(verb string, args []string) (code int, done bool) {
	if wantsHelp(args) || (len(args) > 0 && args[0] == "help") {
		return 0, false
	}
	s := settings()
	in := serviceEnvInput{Settings: s, Getenv: os.Getenv, EUID: Geteuid(), Me: currentUser()}
	if _, err := os.Stat(s.EnvFile); err == nil || !errors.Is(err, os.ErrNotExist) {
		in.EnvFileExists = true
	}
	if _, err := user.Lookup(s.AppUser); err == nil {
		in.AccountExists = true
	}
	in.HaveSudo, _ = exec.LookPath("sudo")
	in.Runuser, _ = exec.LookPath("runuser")
	if self, err := Executable(); err == nil {
		in.Self = self
	} else {
		in.Self = os.Args[0]
	}
	step, argv, msg := serviceEnvPlan(in, verb, args)
	switch {
	case msg != "":
		return Fail(verb, "%s", msg), true
	case step == loadHere:
		if err := applyServiceEnv(s.EnvFile); err != nil {
			if errors.Is(err, os.ErrPermission) {
				return Fail(verb, "cannot read %s as %s — run it with sudo", s.EnvFile, in.Me), true
			}
			return Fail(verb, "%v", err), true
		}
	case step == becomeService:
		err := Exec(argv[0], argv, os.Environ())
		return Fail(verb, "could not run as %s: %v", s.AppUser, err), true
	}
	return 0, false
}

// serviceVerb wraps an app-data verb's Run with withServiceEnv.
func serviceVerb(verb string, run func(args []string) int) func(args []string) int {
	return func(args []string) int {
		if code, done := withServiceEnv(verb, args); done {
			return code
		}
		return run(args)
	}
}
