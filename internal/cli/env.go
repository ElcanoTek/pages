// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ElcanoTek/pages/internal/ops/envfile"
)

func init() {
	Register(&Command{
		Name:    "env",
		Summary: "print the service environment file, secrets redacted (safe to paste)",
		Run:     runEnv,
	})
}

// runEnv prints the environment file with whole values of secret-named keys
// and URL userinfo passwords redacted (envfile.Redact). The file is
// root:pages 0640, so a non-root operator is re-run through sudo.
func runEnv(args []string) int {
	if wantsHelp(args) {
		fmt.Println("Usage: pages env\n\nPrints the service environment file with secrets redacted; comments are dropped.")
		return 0
	}
	if len(args) > 0 {
		return Fail("env", "takes no arguments")
	}
	return printEnv(os.Stdout, settings().EnvFile)
}

func printEnv(w io.Writer, file string) int {
	b, err := os.ReadFile(file)
	switch {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(os.Stderr, "%s missing — run scripts/bootstrap.sh\n", file)
		return 1
	case errors.Is(err, os.ErrPermission) && Geteuid() != 0:
		if elevate("env", nil) {
			return 1
		}
		return 1
	case err != nil:
		return Fail("env", "%v", err)
	}
	fmt.Fprint(w, envfile.Redact(string(b)))
	return 0
}
