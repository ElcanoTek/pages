// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import "github.com/ElcanoTek/pages/internal/ops/install"

func init() {
	Register(&Command{
		Name:    "restart",
		Summary: "restart pages.service (applies a changed environment file)",
		Run: func(args []string) int {
			if len(args) > 0 {
				return Fail("restart", "takes no arguments")
			}
			if elevate("restart", args) {
				return 1
			}
			return execTool("restart", "systemctl", "restart", install.Service)
		},
	})
}
