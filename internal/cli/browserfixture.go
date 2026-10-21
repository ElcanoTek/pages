// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/fixture"
)

func init() {
	Register(&Command{
		Name:    "browser-fixture",
		Summary: "serve the deterministic in-memory fixture the Playwright suite runs against (127.0.0.1:$PLAYWRIGHT_FIXTURE_PORT)",
		Hidden:  true,
		Run:     runBrowserFixture,
	})
}

func runBrowserFixture(args []string) int {
	if wantsHelp(args) {
		fmt.Println(`Usage: pages browser-fixture

Serves the Playwright fixture (test/browser/*.spec.js) on
127.0.0.1:$PLAYWRIGHT_FIXTURE_PORT (default 3210): the real admin shells,
gate pages and browser assets over an in-memory admin API with a fixed clock.
No database, no SSO. Test harness only; GET /healthz answers "ok".`)
		return 0
	}
	if len(args) > 0 {
		return Fail("browser-fixture", "unknown argument %q (try --help)", args[0])
	}
	// The port is configurable so several checkouts can run the browser
	// suite at once (playwright.config.js passes the same variable).
	port := env.Int("PLAYWRIGHT_FIXTURE_PORT", 3210)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return Fail("browser-fixture", "%v", err)
	}
	srv := &http.Server{Handler: fixture.New(port), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	fmt.Printf("fixture server on %d\n", port)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return Fail("browser-fixture", "%v", err)
	}
	return 0
}
