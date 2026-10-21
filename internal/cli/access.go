// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ElcanoTek/pages/internal/auth/central"
	"github.com/ElcanoTek/pages/internal/db"
)

// `pages access` — the central-mode access list (PAGES_AUTH_MODE=central).
// Auth keeps it current through signed access events once the Pages app has
// a back-channel URL; this command is for bootstrap and break-glass.
// Everyone on the list is a Pages admin. Reads DATABASE_URL (or PG*); on an
// installed box, from the service environment file (serviceenv.go).
func init() {
	Register(&Command{
		Name:    "access",
		Summary: "central sign-in access list: grant <email> | revoke <email> | list",
		Run:     serviceVerb("access", runAccess),
	})
}

const accessUsage = "usage: pages access grant <email> | revoke <email> | list"

func runAccess(args []string) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(accessUsage)
		fmt.Println("\n  grant <email>   add the email to this Pages' access list")
		fmt.Println("  revoke <email>  remove it and end that email's sessions")
		fmt.Println("  list            print every email with access")
		return 0
	}
	d, err := db.OpenFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	defer d.Close()
	return accessMain(context.Background(), central.NewStore(d), args, os.Stdout, os.Stderr)
}

// accessMain is the verb against a store, writing to stdout/stderr as the
// Node release's access script did.
func accessMain(ctx context.Context, store *central.Store, args []string, stdout, stderr io.Writer) int {
	cmd, email := "", ""
	if len(args) > 0 {
		cmd = args[0]
	}
	if len(args) > 1 {
		email = args[1]
	}
	switch cmd {
	case "grant":
		if email == "" {
			fmt.Fprintln(stderr, "usage: pages access grant <email>")
			return 1
		}
		granted, err := store.GrantAccess(ctx, email)
		if err != nil {
			fmt.Fprintln(stderr, err.Error())
			return 1
		}
		fmt.Fprintf(stdout, "granted %s\n", granted)
	case "revoke":
		if email == "" {
			fmt.Fprintln(stderr, "usage: pages access revoke <email>")
			return 1
		}
		ok, err := store.RevokeAccess(ctx, email)
		if err != nil {
			fmt.Fprintln(stderr, err.Error())
			return 1
		}
		if ok {
			fmt.Fprintf(stdout, "revoked %s and ended its sessions\n", email)
		} else {
			fmt.Fprintf(stdout, "%s had no active access\n", email)
		}
	case "list":
		emails, err := store.ListAccess(ctx)
		if err != nil {
			fmt.Fprintln(stderr, err.Error())
			return 1
		}
		if len(emails) == 0 {
			fmt.Fprintln(stdout, "(no one has access)")
		} else {
			fmt.Fprintln(stdout, strings.Join(emails, "\n"))
		}
	default:
		fmt.Fprintln(stdout, accessUsage)
		return 1
	}
	return 0
}
