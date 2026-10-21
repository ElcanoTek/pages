// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Command pages is Elcano Pages: `pages serve` runs the two-host server, and
// every other verb is the operator CLI. Run `pages help` for the list.
package main

import (
	"os"

	"github.com/ElcanoTek/pages/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
