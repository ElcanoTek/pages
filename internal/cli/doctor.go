// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"

	"github.com/ElcanoTek/pages/internal/ops/doctor"
	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

func init() {
	Register(&Command{
		Name:    "doctor",
		Summary: "diagnose the box (--check: read-only, exit 1 on failure); plain doctor repairs as root",
		Run:     runDoctor,
	})
}

// droppedMarker marks a doctor already re-executed as the service account.
const droppedMarker = "PAGES_DOCTOR_AS_SERVICE_USER=1"

// doctorPlan decides how a doctor invocation runs: read-only modes run AS
// THE SERVICE ACCOUNT, never root — they need the 0640 root:pages
// environment file to be honest, but must not hold root; repairs need root.
// It returns the argv to re-execute (nil: run here) or an error message.
func doctorPlan(args []string, euid int, me string, s install.Settings, self string, dropped bool) (argv []string, msg string) {
	readOnly := false
	for _, a := range args {
		switch a {
		case "--check", "--dry-run", "--help", "-h":
			readOnly = true
		}
	}
	switch {
	case !readOnly && euid != 0:
		return nil, "sudo"
	case !readOnly, dropped, me == s.AppUser:
		return nil, ""
	case euid == 0:
		runuser, err := exec.LookPath("runuser")
		if err != nil {
			runuser = "runuser"
		}
		return reexecArgv([]string{runuser, "-u", s.AppUser, "--"}, s, self, "doctor", args, "GIT_OPTIONAL_LOCKS=0", droppedMarker), ""
	default:
		sudo, err := exec.LookPath("sudo")
		if err != nil {
			// No way to become the service account: report what this
			// account can see (the doctor says what it could not read).
			return nil, ""
		}
		return reexecArgv([]string{sudo, "-u", s.AppUser, "--"}, s, self, "doctor", args, "GIT_OPTIONAL_LOCKS=0", droppedMarker), ""
	}
}

func runDoctor(args []string) int {
	o := doctor.Options{Runner: Runner, Log: term.New(), EUID: Geteuid(), OSRelease: os.Getenv("PAGES_OS_RELEASE")}
	for _, a := range args {
		switch a {
		case "--check":
			o.Check = true
		case "--no-restart":
			o.NoRestart = true
		case "--dry-run":
			o.DryRun = true
		case "--strict":
			o.Strict = true
		case "-h", "--help":
			fmt.Print(doctor.Usage)
			return 0
		default:
			fmt.Fprintf(os.Stderr, "error: unknown argument: %s (try --help)\n", a)
			return 2
		}
	}
	s := settings()
	self, err := Executable()
	if err != nil {
		return Fail("doctor", "%v", err)
	}
	// Without the service account (nothing installed yet) there is no one to
	// drop to: diagnose as whoever runs it.
	_, noAccount := user.Lookup(s.AppUser)
	dropped := os.Getenv("PAGES_DOCTOR_AS_SERVICE_USER") == "1" || noAccount != nil
	argv, msg := doctorPlan(args, Geteuid(), currentUser(), s, self, dropped)
	if msg == "sudo" {
		if elevate("doctor", args) {
			return 1
		}
	}
	if argv != nil {
		err := Exec(argv[0], argv, os.Environ())
		return Fail("doctor", "could not run as %s: %v", s.AppUser, err)
	}
	o.Settings = s
	return doctor.Run(context.Background(), o)
}
