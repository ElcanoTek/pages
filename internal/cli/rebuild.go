// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

func init() {
	Register(&Command{
		Name:    "rebuild",
		Summary: "build and activate the current checkout without fetching (same checks and rollback as update)",
		Run:     func(args []string) int { return runUpdate("rebuild", args, true) },
	})
}
