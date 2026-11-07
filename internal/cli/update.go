// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ElcanoTek/pages/internal/ops/term"
	"github.com/ElcanoTek/pages/internal/ops/update"
)

func init() {
	Register(&Command{
		Name:    "update",
		Summary: "verify and activate a signed release (or --from-source); rolls back on failure",
		Run:     func(args []string) int { return runUpdate("update", args, false) },
	})
}

const updateUsage = `Usage: pages update [--yes] [--wait] [--force] [--tag <vYYYY.MM.DD.N>] [--allow-downgrade] [--from-source]

By default, fetches release tags, downloads the latest (or --tag) published
release from GitHub, verifies SHA256SUMS, release.json and pages-linux-<arch>
against the Sigstore keyless OIDC signature using the cosign pinned in
cosign.lock, checks out the release tag, and activates the verified binary
without needing Go or Make on the box.

With --from-source (or PAGES_FROM_SOURCE=1), fast-forwards the source checkout
and compiles a release from it with make/go instead.

Preflights the candidate, checks that no staged upload or database write is in
progress, applies only pending migrations declared backward-compatible, then
atomically switches the active release, restarts and requires /readyz. Any
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
	fromSource := rebuild || os.Getenv("PAGES_FROM_SOURCE") == "1"
	tag := os.Getenv("PAGES_RELEASE_TAG")
	allowDowngrade := os.Getenv("PAGES_ALLOW_DOWNGRADE") == "1"
	activate := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Print(updateUsage)
			return 0
		case a == "-y" || a == "--yes":
			yes = true
		case a == "--wait":
			wait = true
		case a == "--force":
			force = true
		case a == "--from-source":
			fromSource = true
		case a == "--allow-downgrade":
			allowDowngrade = true
		case a == "--tag":
			rest := args[i+1:]
			if len(rest) == 0 || rest[0] == "" {
				return Fail(verb, "--tag needs a release tag (vYYYY.MM.DD.N)")
			}
			tag = rest[0]
			i++
		case strings.HasPrefix(a, "--tag="):
			tag = strings.TrimPrefix(a, "--tag=")
			if tag == "" {
				return Fail(verb, "--tag needs a release tag (vYYYY.MM.DD.N)")
			}
		case a == "--activate": // stage two, run by the candidate binary
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
	if fromSource {
		_ = os.Setenv("PAGES_FROM_SOURCE", "1")
	}
	if tag != "" {
		_ = os.Setenv("PAGES_RELEASE_TAG", tag)
	}
	if allowDowngrade {
		_ = os.Setenv("PAGES_ALLOW_DOWNGRADE", "1")
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
		FromSource:     fromSource,
		Tag:            tag,
		AllowDowngrade: allowDowngrade,
		NoPull:         rebuild || os.Getenv("PAGES_UPDATE_NO_PULL") == "1", Yes: yes || rebuild,
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
