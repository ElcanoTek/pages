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
		Name:    "backup",
		Summary: "capture a complete backup and rehearse its restore (default destination /var/backups/pages)",
		Run:     runBackup,
	})
}

const backupUsage = `Usage: pages backup [destination]

Captures the database (pg_dump), the shared assets, the installed release and
its configuration under the updater's shared lock, without stopping Pages,
then restores it into a private throwaway PostgreSQL cluster and probes it
with the captured release before publishing. Prints {"backup_dir": "..."}.
Default destination: /var/backups/pages. Verify later with
pages check-integrity <backup-dir>.
`

func runBackup(args []string) int {
	if wantsHelp(args) {
		fmt.Print(backupUsage)
		return 0
	}
	if len(args) > 1 {
		fmt.Fprintln(os.Stderr, "backup: usage: pages backup [destination] | pages check-integrity <backup-directory>")
		return 1
	}
	if elevate("backup", args) {
		return 1
	}
	umaskPrivate()
	dest := ""
	if len(args) == 1 {
		dest = args[0]
	}
	ctx, stop := signalContext()
	defer stop()
	dir, err := backup.Capture(ctx, backup.Options{Runner: Runner, EUID: Geteuid()}, settings(), dest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup: "+err.Error())
		return 1
	}
	out, _ := json.Marshal(map[string]string{"backup_dir": dir})
	fmt.Println(string(out))
	return 0
}
