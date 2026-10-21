// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ElcanoTek/pages/internal/ops/backupprobe"
)

func init() {
	Register(&Command{
		Name:    "backup-probe",
		Summary: "restore probe a backup rehearsal runs with the captured release (private cluster only)",
		Hidden:  true,
		Run: func(args []string) int {
			if len(args) != 1 {
				fmt.Fprintln(os.Stderr, "usage: pages backup-probe <restored-assets-dir>")
				return 2
			}
			res, err := backupprobe.Run(context.Background(), args[0])
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				return 1
			}
			out, err := json.Marshal(res)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				return 1
			}
			fmt.Println(string(out))
			return 0
		},
	})
}
