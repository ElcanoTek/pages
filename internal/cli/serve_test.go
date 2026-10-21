// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/backupprobe"
)

// `pages preflight` (run by update) refuses a release whose configuration
// the server would refuse at startup.
func TestPreflightRunsServerValidation(t *testing.T) {
	for k, v := range map[string]string{"PAGES_ENV": "", "NODE_ENV": "", "PAGES_AUTH_MODE": "", "MAX_HTML_BYTES": "", "PAGES_TRUST_PROXY": ""} {
		t.Setenv(k, v)
	}
	for _, c := range []struct{ key, value, want string }{
		{"PAGES_TRUST_PROXY", "true", "PAGES_TRUST_PROXY"},
		{"MAX_HTML_BYTES", "lots", "MAX_HTML_BYTES"},
		{"PAGES_AUTH_MODE", "central", "central"},
	} {
		t.Run(c.key, func(t *testing.T) {
			t.Setenv(c.key, c.value)
			if err := Preflight(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Preflight = %v", err)
			}
		})
	}
	if err := Preflight(); err != nil {
		t.Fatalf("Preflight on a valid configuration = %v", err)
	}
	if backupprobe.Hooks.Serve == nil {
		t.Fatal("backup rehearsal serve hook not set")
	}
}

func TestServeUsage(t *testing.T) {
	if code := runServe([]string{"--help"}); code != 0 {
		t.Fatalf("--help = %d", code)
	}
	if code := runServe([]string{"extra"}); code != 2 {
		t.Fatalf("extra arg = %d", code)
	}
	t.Setenv("PORT", "nope")
	if code := runServe(nil); code != 1 {
		t.Fatalf("bad PORT = %d", code)
	}
}
