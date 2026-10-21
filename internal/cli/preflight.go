// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
)

// PreflightChecks are extra configuration checks `pages preflight` runs —
// the server's own fail-closed startup validation belongs here (appended
// from the file that registers `serve`), so a release whose configuration
// would crash at startup is refused before the running service is stopped.
var PreflightChecks []func() error

func init() {
	Register(&Command{
		Name:    "preflight",
		Summary: "validate this release's configuration without serving or writing (run by update)",
		Hidden:  true,
		Run: func(args []string) int {
			if err := Preflight(); err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				return 1
			}
			return 0
		},
	})
}

// Preflight loads the production configuration the way the server would at
// startup — secrets, database settings, embedded trees — without binding a
// listener or touching the database.
func Preflight() error {
	if env.Production() {
		for _, k := range []string{"RAW_TOKEN_SECRET", "PAGE_COOKIE_SECRET", "API_TOKEN_PEPPER"} {
			if os.Getenv(k) == "" {
				return fmt.Errorf("%s is required in production", k)
			}
		}
	}
	if _, err := pgxpool.ParseConfig(db.ConnString()); err != nil {
		return fmt.Errorf("database configuration: %w", err)
	}
	if _, err := fs.Stat(pages.Migrations(), "compatibility.json"); err != nil {
		return errors.New("this binary carries no migrations/compatibility.json")
	}
	for _, check := range PreflightChecks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}
