// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/ops/backupprobe"
	"github.com/ElcanoTek/pages/internal/server"
)

// mcpFactory builds the /mcp endpoint for `pages serve`.
// ORCHESTRATOR: wire internal/mcp here once it is merged, e.g.
//
//	mcpFactory = func(d *db.DB, ip func(*http.Request) string, max int64, l *slog.Logger) http.Handler {
//		return mcp.Handler(mcp.Deps{DB: d, RateLimit: ratelimit.NewMCP(ratelimit.Config{ClientIP: ip}).Middleware, ClientIP: ip, MaxBodyBytes: max, Logger: l})
//	}
var mcpFactory server.MCPFactory // ORCHESTRATOR: set to the internal/mcp handler (see above)

func init() {
	Register(&Command{
		Name:    "serve",
		Summary: "run the two-host Pages server on :$PORT (default 3002)",
		Run:     runServe,
	})
	// The server's fail-closed startup validation, so `pages update` refuses
	// a release whose configuration would crash at startup before the
	// running service is stopped.
	PreflightChecks = append(PreflightChecks, server.Validate)
	// The backup rehearsal serves the restored data in process.
	backupprobe.Hooks.Serve = func(ctx context.Context, ln net.Listener) error {
		return server.Run(ctx, ln, server.RunOptions{MCP: mcpFactory})
	}
}

func runServe(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "Usage: pages serve\n\nServes the dashboard and content hosts on :$PORT (default 3002). Configured entirely by the environment (/etc/default/pages).")
		if args[0] == "-h" || args[0] == "--help" {
			return 0
		}
		return 2
	}
	port := env.String("PORT", "3002")
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return Fail("serve", "PORT must be a port number, got %q", port)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", ":"+port)
	if err != nil {
		return Fail("serve", "%v", err)
	}
	err = server.Run(ctx, ln, server.RunOptions{
		MCP: mcpFactory,
		Started: func(addr net.Addr) {
			fmt.Println(server.Banner(strconv.Itoa(addr.(*net.TCPAddr).Port)))
		},
	})
	if err != nil {
		return Fail("serve", "%v", err)
	}
	return 0
}
