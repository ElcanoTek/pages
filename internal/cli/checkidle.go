// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/ops/compat"
)

func init() {
	Register(&Command{
		Name:    "check-idle",
		Summary: "refuse or wait on an automatic update while a staged upload or database write is in progress",
		Hidden:  true,
		Run:     runCheckIdle,
	})
}

func runCheckIdle(args []string) int {
	wait := os.Getenv("PAGES_UPDATE_WAIT") == "1"
	for _, a := range args {
		switch a {
		case "--wait":
			wait = true
		default:
			return Fail("check-idle", "unknown argument %q", a)
		}
	}
	maxWait := 10 * time.Second
	pollInterval := 500 * time.Millisecond
	if wait {
		maxWait = 5 * time.Minute
		pollInterval = time.Second
	}
	d, err := db.OpenFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	defer d.Close()
	sigCtx, stop := signalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(sigCtx, maxWait+30*time.Second)
	defer cancel()
	if err := compat.WaitIdle(ctx, d.Pool, compat.DefaultActiveUploadWindow, maxWait, pollInterval); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}
