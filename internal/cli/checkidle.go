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

const checkIdleUsage = "usage: pages check-idle [--wait]"

// `pages check-idle` blocks or waits while an uncommitted staged upload or
// database write transaction is active. Both `pages update` (via appCommand)
// and operators checking a box over ssh call it; on an installed box it drops
// to the service account and loads the service environment file (serviceenv.go).
func init() {
	Register(&Command{
		Name:    "check-idle",
		Summary: "refuse or wait on an automatic update while a staged upload or database write is in progress",
		Hidden:  true,
		Run:     serviceVerb("check-idle", runCheckIdle),
	})
}

func runCheckIdle(args []string) int {
	if wantsHelp(args) || (len(args) > 0 && args[0] == "help") {
		fmt.Println(checkIdleUsage)
		return 0
	}
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
