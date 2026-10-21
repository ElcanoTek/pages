// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ElcanoTek/pages/internal/ops/bootstrap"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

func init() {
	Register(&Command{
		Name:    "bootstrap",
		Summary: "install or reconfigure this box (run by scripts/bootstrap.sh; safe to re-run)",
		Run:     runBootstrap,
	})
}

const bootstrapUsage = `Usage: sudo bash scripts/bootstrap.sh     (builds pages, then runs: pages bootstrap)

Installs PostgreSQL, the service account, the release, the environment file,
the systemd unit, the CLI, the login banner and (optionally) Caddy with
auto-TLS for both hostnames. Re-runs keep secrets, the database password, data
and the active release.

Scripted: PAGES_BOOTSTRAP_NON_INTERACTIVE=1 AUTH_SIGNING_PUBKEY=...
PAGES_BOOTSTRAP_DASHBOARD_HOST=... PAGES_BOOTSTRAP_CONTENT_HOST=...
PAGES_BOOTSTRAP_SETUP_CADDY=Y PAGES_BOOTSTRAP_USE_LETSENCRYPT=Y
PAGES_BOOTSTRAP_LE_EMAIL=...
`

func runBootstrap(args []string) int {
	if wantsHelp(args) {
		fmt.Print(bootstrapUsage)
		return 0
	}
	if len(args) > 0 {
		return Fail("bootstrap", "unknown argument %q (try --help)", args[0])
	}
	log := term.New()
	if Geteuid() != 0 {
		log.Error(bootstrap.ErrNotRoot.Error())
		return 1
	}
	self, err := Executable()
	if err != nil {
		return Fail("bootstrap", "%v", err)
	}
	s := settings()
	src := os.Getenv("SRC_DIR")
	if src == "" {
		if wd, err := os.Getwd(); err == nil {
			src = checkoutRoot(wd)
		}
	}
	if src == "" {
		src = s.SrcDir
	}
	// Prompts read the terminal even when stdin is a pipe (curl | bash).
	var prompts io.Reader = os.Stdin
	interactive := term.IsTerminal(os.Stdin)
	if !interactive && term.IsTerminal(os.Stdout) {
		if tty, err := os.Open("/dev/tty"); err == nil {
			defer tty.Close()
			prompts, interactive = tty, true
		}
	}
	err = bootstrap.Run(context.Background(), bootstrap.Options{
		Settings: s, SrcDir: src, Runner: Runner, Log: log, Prompts: prompts, Interactive: interactive, Self: self,
	})
	if err != nil {
		var f *bootstrap.Error
		if errors.As(err, &f) {
			log.Error(f.Msg)
		} else {
			log.Error(err.Error())
		}
		return 1
	}
	return 0
}
