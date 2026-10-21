// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/ElcanoTek/pages/internal/ops/devenv"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

func init() {
	Register(&Command{
		Name:    "dev",
		Summary: "local dev environment: throwaway Postgres, migrations, agent token, dev SSO, then pages serve (pages dev token prints the token)",
		Hidden:  true,
		Run:     runDev,
	})
}

func runDev(args []string) int {
	if wantsHelp(args) {
		fmt.Println(`Usage: pages dev [token]

Stands up a throwaway PostgreSQL cluster under $PAGES_DEV_DIR (default
/var/tmp/pages-dev; on macOS, or with PAGES_DEV_DATABASE_URL, your own
server), migrates, mints and saves an agent token (.devdata/agent-token),
mints a throwaway SSO keypair and admin cookie, and runs pages serve on :$PORT
(3099) with PAGES_DEV_LOGIN=1. Never use on a production box.`)
		return 0
	}
	wd, _ := os.Getwd()
	root := os.Getenv("PAGES_DEV_ROOT")
	if root == "" {
		root = checkoutRoot(wd)
	}
	if root == "" {
		root = wd
	}
	self, err := Executable()
	if err != nil {
		return Fail("dev", "%v", err)
	}
	o := devenv.Options{Root: root, Runner: Runner, Log: term.New(), EUID: Geteuid(), Self: self}
	if len(args) == 1 && args[0] == "token" {
		tok, err := devenv.Token(o)
		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		fmt.Println(tok)
		return 0
	}
	if len(args) > 0 {
		return Fail("dev", "unknown argument %q (try --help)", args[0])
	}
	e, err := devenv.Prepare(context.Background(), o)
	if err != nil {
		return Fail("dev", "%v", err)
	}
	tok, _ := devenv.Token(o)
	fmt.Print(devenv.Banner(e, tok))
	fmt.Println("▸ starting server (foreground)")
	err = Exec(self, []string{self, "serve"}, e.Environ())
	return Fail("dev", "pages serve: %v", err)
}
