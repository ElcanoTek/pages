// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package env reads Pages' configuration. Pages is configured entirely through
// environment variables (.env.example lists every one), and every read goes
// through here at call time rather than once at package init, so a test can
// t.Setenv a value and the code under test sees it.
package env

import (
	"os"
	"strconv"
	"strings"
)

// String returns the variable, or def when it is unset or empty.
func String(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// Int returns the variable parsed as a base-10 integer, or def when it is
// unset or does not parse.
func Int(name string, def int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// NonNegativeInt is Int, but a negative value also falls back to def. It is
// the shape of every millisecond bound ("0 disables").
func NonNegativeInt(name string, def int) int {
	n := Int(name, def)
	if n < 0 {
		return def
	}
	return n
}

// Bool reports whether the variable is set to 1/true/yes/on (any case).
func Bool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// Production reports whether Pages runs in production mode: PAGES_ENV or the
// legacy NODE_ENV set to "production". Production turns on the fail-closed
// secret checks and hides internal error detail.
func Production() bool {
	return os.Getenv("PAGES_ENV") == "production" || os.Getenv("NODE_ENV") == "production"
}

// DashboardHost is the trusted host: admin shell, /view, /api/v1, /mcp.
func DashboardHost() string { return String("DASHBOARD_HOST", "pages.elcanotek.com") }

// ContentHost is the cookieless, untrusted render host.
func ContentHost() string { return String("CONTENT_HOST", "elcano-pages.com") }

// DashboardOrigin is the exact scheme+host of the dashboard host.
func DashboardOrigin() string { return String("DASHBOARD_ORIGIN", "https://"+DashboardHost()) }

// ContentOrigin is the exact scheme+host of the content host.
func ContentOrigin() string { return String("CONTENT_ORIGIN", "https://"+ContentHost()) }
