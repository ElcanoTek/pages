// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/ElcanoTek/pages/internal/ops/doctor"
	"github.com/ElcanoTek/pages/internal/ops/install"
	"github.com/ElcanoTek/pages/internal/ops/runner"
)

func init() {
	Register(&Command{
		Name:    "tls",
		Summary: "Caddy front end: pages tls status|reload|restart",
		Run:     runTLS,
	})
}

func runTLS(args []string) int {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	if sub == "-h" || sub == "--help" {
		fmt.Println("Usage: pages tls [status|reload|restart]\n\n  status   caddy.service state and the Pages site addresses\n  reload   caddy reload --config /etc/caddy/Caddyfile\n  restart  systemctl restart caddy.service")
		return 0
	}
	if elevate("tls", args) {
		return 1
	}
	switch sub {
	case "reload":
		return execTool("tls", "caddy", "reload", "--config", doctor.Caddyfile)
	case "restart":
		return execTool("tls", "systemctl", "restart", "caddy.service")
	default:
		res, _ := Runner.Run(context.Background(), runner.Cmd{Name: "systemctl", Args: []string{"is-active", "caddy.service"}})
		fmt.Print(res.Stdout + res.Stderr)
		if b, err := os.ReadFile(doctor.CaddySnippet); err == nil {
			if lines := install.SiteBlockLines(string(b)); len(lines) > 0 {
				fmt.Println(strings.Join(lines, "\n"))
			}
		}
		return 0
	}
}
