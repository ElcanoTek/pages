// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import "github.com/ElcanoTek/pages/internal/ops/install"

func init() {
	Register(&Command{
		Name:    "start",
		Summary: "start pages.service",
		Run: func(args []string) int {
			if len(args) > 0 {
				return Fail("start", "takes no arguments")
			}
			if elevate("start", args) {
				return 1
			}
			return execTool("start", "systemctl", "start", install.Service)
		},
	})
}
