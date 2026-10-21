// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/ElcanoTek/pages/internal/ops/backup"
)

func init() {
	Register(&Command{
		Name:    "check-integrity",
		Summary: "verify a backup's files and repeat its isolated restore rehearsal",
		Run:     runCheckIntegrity,
	})
}

func runCheckIntegrity(args []string) int {
	if wantsHelp(args) {
		fmt.Println("Usage: pages check-integrity <backup-dir>\n\nVerifies every captured file against the manifest and repeats the restore rehearsal with the captured release.")
		return 0
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "backup: usage: pages backup [destination] | pages check-integrity <backup-directory>")
		return 1
	}
	if elevate("check-integrity", args) {
		return 1
	}
	umaskPrivate()
	ctx, stop := signalContext()
	defer stop()
	res, err := backup.Check(ctx, backup.Options{Runner: Runner, EUID: Geteuid()}, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup: "+err.Error())
		return 1
	}
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	return 0
}
