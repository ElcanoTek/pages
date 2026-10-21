// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/ops/compat"
)

func init() {
	Register(&Command{
		Name:    "check-migrations",
		Summary: "refuse an automatic update whose pending migrations are not declared backward-compatible",
		Hidden:  true,
		Run:     func([]string) int { return withDatabase(checkMigrations) },
	})
}

func withDatabase(fn func(context.Context, *db.DB) error) int {
	d, err := db.OpenFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fn(ctx, d); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}

func checkMigrations(ctx context.Context, d *db.DB) error {
	policy, available, err := compat.LoadPolicy(pages.Migrations())
	if err != nil {
		return err
	}
	applied, err := compat.Applied(ctx, d.Pool)
	if err != nil {
		return err
	}
	return compat.Check(applied, available, policy)
}
