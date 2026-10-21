// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import "github.com/ElcanoTek/pages/internal/ops/install"

func init() {
	Register(&Command{
		Name:    "status",
		Summary: "systemctl status pages.service",
		Run: func(args []string) int {
			if len(args) > 0 {
				return Fail("status", "takes no arguments")
			}
			if elevate("status", args) {
				return 1
			}
			return execTool("status", "systemctl", "status", install.Service)
		},
	})
}
