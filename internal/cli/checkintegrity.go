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

const checkIntegrityUsage = `Usage: pages check-integrity <backup-dir>

Verifies every captured file against the manifest and repeats the restore
rehearsal with the captured release: the dump is restored into a private
throwaway PostgreSQL cluster and probed with the captured binary. Prints
{"status":"verified",...} and exits 0; any failure exits 1.

Node-era backups (taken by the Node release's pages backup; the manifest
records node_version) are accepted. Every file, the captured Node application
included, is checked against the manifest's lengths and SHA-256s; the restored
snapshot's counts, migrations, source hashes and assets must match what the
Node rehearsal recorded; and the HTTP checks run with THIS release's server
against a migrated scratch copy. The result carries "format":"node" and the
notice:
  ` + backup.NodeEraNotice + `
See pages backup --help for restoring one.
`

func runCheckIntegrity(args []string) int {
	if wantsHelp(args) {
		fmt.Print(checkIntegrityUsage)
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
	if res.Notice != "" {
		fmt.Fprintln(os.Stderr, "check-integrity: "+res.Notice)
	}
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	return 0
}
