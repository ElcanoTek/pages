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
before publishing — with the installed release binary identical to the
captured one when that binary is root's alone, otherwise with this release;
nothing captured is executed. Prints {"backup_dir": "..."}.
Default destination: /var/backups/pages; it and every directory above it
must be root's alone (not writable by other accounts). Verify later with
pages check-integrity <backup-dir>, which restores the dump as an ordinary,
non-superuser role of the throwaway cluster.

Restoring a backup: run pages check-integrity on it first, restore
database.dump into a NEW database with pg_restore (--exit-on-error
--single-transaction --no-owner --no-acl --role=<app role>), restore assets/
into the shared assets directory and config/ (service.env with its original
HMAC secrets, install.env, local.env) with their original ownership and
modes, then restart. Never install application/pages from the backup itself
(root runs the CLI binary): if check-integrity reported "probed_with", that
root-owned installed release is identical to the captured one, so activate
it; otherwise rebuild the revision the manifest records (pages_version) from
the source checkout, or keep this release and migrate as for a Node-era
backup below.

Node-era backups (taken by the Node release; manifest.json records
node_version and application/ holds server.js and package-lock.json, no
binary): check-integrity accepts them, verifying the application by manifest
only and serving the restored data with this release. Restore the database,
assets and config/ as above, but do NOT stage application/ or run npm: keep
this release installed, point DATABASE_URL in the restored service
environment at the recovered database, and run pages migrate with that
environment to add this release's newer migrations (/readyz stays 503 until
then); then restart. Keep the Node-era backup until a new pages backup of the
recovered installation has verified.
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
