// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/ElcanoTek/pages/internal/ops/term"
	"github.com/ElcanoTek/pages/internal/ops/update"
)

func init() {
	Register(&Command{
		Name:    "update",
		Summary: "fetch, build and activate a new release; rolls back on failure (PAGES_UPDATE_YES=1: no prompt)",
		Run:     func(args []string) int { return runUpdate("update", args, false) },
	})
}

const updateUsage = `Usage: pages update [--yes] [--wait] [--force]

Fast-forwards the source checkout, builds a complete release from it under
<app>.releases, preflights it, checks that no staged upload or database write
is in progress, applies only pending migrations declared backward-compatible,
then atomically switches the active release, restarts and requires /readyz. Any
activation, start or readiness failure restores the previous release and
installed control files. PAGES_UPDATE_YES=1 (or --yes) skips the confirmation
prompt; PAGES_UPDATE_WAIT=1 (or --wait) waits up to 5 minutes for active writes
to finish; PAGES_UPDATE_FORCE=1 (or --force) skips the active-write blocker;
PAGES_SKIP_TEMPLATE_SYNC=1 skips template sync.
`

// runUpdate is `pages update` and (rebuild) `pages rebuild`.
func runUpdate(verb string, args []string, rebuild bool) int {
	yes := os.Getenv("PAGES_UPDATE_YES") == "1"
	wait := os.Getenv("PAGES_UPDATE_WAIT") == "1"
	force := os.Getenv("PAGES_UPDATE_FORCE") == "1"
	activate := ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-h", "--help":
			fmt.Print(updateUsage)
			return 0
		case "-y", "--yes":
			yes = true
		case "--wait":
			wait = true
		case "--force":
			force = true
		case "--activate": // stage two, run by the candidate binary
			rest := args[i+1:]
			if len(rest) == 0 {
				return Fail(verb, "--activate needs the candidate directory")
			}
			activate = rest[0]
			i++
		default:
			return Fail(verb, "unknown argument %q (try --help)", a)
		}
	}
	if wait {
		_ = os.Setenv("PAGES_UPDATE_WAIT", "1")
	}
	if force {
		_ = os.Setenv("PAGES_UPDATE_FORCE", "1")
	}
	if Geteuid() != 0 {
		if yes {
			_ = os.Setenv("PAGES_UPDATE_YES", "1")
		}
		if rebuild {
			_ = os.Setenv("PAGES_UPDATE_NO_PULL", "1")
			_ = os.Setenv("PAGES_UPDATE_YES", "1")
		}
		if elevate(verb, args) {
			return 1
		}
	}
	ctx, stop := signalContext()
	defer stop()
	o := update.Options{
		Settings: settings(), Runner: Runner, Log: term.New(),
		NoPull: rebuild || os.Getenv("PAGES_UPDATE_NO_PULL") == "1", Yes: yes || rebuild,
		Wait:             wait,
		Force:            force,
		SkipTemplateSync: os.Getenv("PAGES_SKIP_TEMPLATE_SYNC") == "1",
		ServiceFile:      os.Getenv("PAGES_SERVICE_FILE"), MotdFile: os.Getenv("PAGES_MOTD_FILE"),
		Interactive: term.IsTerminal(os.Stdin),
	}
	var err error
	if activate != "" {
		err = update.Activate(ctx, o, activate)
	} else {
		err = update.Run(ctx, o)
	}
	if err == nil {
		return 0
	}
	var f *update.Error
	if !errors.As(err, &f) || !f.Reported {
		fmt.Fprintln(os.Stderr, err.Error())
	}
	if interrupted(ctx) {
		return 130
	}
	return 1
}
