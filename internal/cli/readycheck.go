// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"time"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/ops/compat"
)

func init() {
	Register(&Command{
		Name:    "ready-check",
		Summary: "bounded database readiness probe (what /readyz answers), for a predecessor without /readyz",
		Hidden:  true,
		Run:     func([]string) int { return withDatabase(readyCheck) },
	})
}

func readyCheck(ctx context.Context, d *db.DB) error {
	_, available, err := compat.LoadPolicy(pages.Migrations())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return compat.Ready(ctx, d.Pool, available)
}
