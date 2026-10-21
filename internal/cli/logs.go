// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import "github.com/ElcanoTek/pages/internal/ops/install"

func init() {
	Register(&Command{
		Name:    "logs",
		Summary: "follow the service journal (pages logs [journalctl args...])",
		Run: func(args []string) int {
			if elevate("logs", args) {
				return 1
			}
			if len(args) > 0 {
				return execTool("logs", "journalctl", append([]string{"-u", install.Service}, args...)...)
			}
			return execTool("logs", "journalctl", "-fu", install.Service)
		},
	})
}
