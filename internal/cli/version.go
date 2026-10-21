// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"fmt"
	"runtime"

	"github.com/ElcanoTek/pages/internal/version"
)

func init() {
	Register(&Command{
		Name:    "version",
		Aliases: []string{"--version"},
		Summary: "print the build version and revision",
		Run: func([]string) int {
			fmt.Printf("pages %s (%s, %s)\n", version.Version, version.Revision(), runtime.Version())
			return 0
		},
	})
}
