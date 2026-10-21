// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/migrate"
)

func init() {
	Register(&Command{
		Name:    "migrate",
		Summary: "apply pending database migrations (idempotent)",
		Run:     runMigrate,
	})
}

func runMigrate(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "Usage: pages migrate\n\nApplies every pending migration in filename order. Reads DATABASE_URL (or PG*).")
		if args[0] == "-h" || args[0] == "--help" {
			return 0
		}
		return 2
	}
	d, err := db.OpenFromEnv()
	if err != nil {
		return Fail("migrate", "%v", err)
	}
	defer d.Close()
	timeout := time.Duration(env.NonNegativeInt("PAGES_MIGRATION_LOCK_TIMEOUT_MS", 30000)) * time.Millisecond
	if _, err := migrate.Run(context.Background(), d.Pool, pages.Migrations(), migrate.Options{LockTimeout: timeout, Log: os.Stdout}); err != nil {
		if errors.Is(err, migrate.ErrBusy) {
			fmt.Fprintln(os.Stderr, err.Error())
			return 1
		}
		return Fail("migrate", "%v", err)
	}
	return 0
}
