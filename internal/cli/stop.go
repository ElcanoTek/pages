// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import "github.com/ElcanoTek/pages/internal/ops/install"

func init() {
	Register(&Command{
		Name:    "stop",
		Summary: "stop pages.service",
		Run: func(args []string) int {
			if len(args) > 0 {
				return Fail("stop", "takes no arguments")
			}
			if elevate("stop", args) {
				return 1
			}
			return execTool("stop", "systemctl", "stop", install.Service)
		},
	})
}
