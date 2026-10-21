// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/testdb"
	"github.com/ElcanoTek/pages/internal/tokens"
)

func TestJSInteger(t *testing.T) {
	for _, tc := range []struct {
		in      string
		present bool
		want    int64
		ok      bool
	}{
		{"7", true, 7, true}, {" 7 ", true, 7, true}, {"", true, 0, true}, {"1e3", true, 1000, true},
		{"0x10", true, 16, true}, {"-2", true, -2, true}, {"1.5", true, 0, false}, {"abc", true, 0, false},
		{"Infinity", true, 0, false}, {"NaN", true, 0, false}, {"", false, 0, false},
	} {
		if got, ok := jsInteger(tc.in, tc.present); got != tc.want || ok != tc.ok {
			t.Errorf("jsInteger(%q) = %d %v", tc.in, got, ok)
		}
	}
}

func token(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runToken(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestTokenVerb(t *testing.T) {
	t.Setenv("API_TOKEN_PEPPER", "cli-pepper")
	d := testdb.New(t)

	if code, out, _ := token(t); code != 1 || !strings.HasPrefix(out, "usage: pages token add") {
		t.Fatalf("no verb: %d %q", code, out)
	}
	if code, out, _ := token(t, "list"); code != 0 || out != "(no tokens)\n" {
		t.Fatalf("empty list: %d %q", code, out)
	}
	code, out, _ := token(t, "add", "northwind-agent")
	if code != 0 || !regexp.MustCompile(`^created token #1  label=northwind-agent  scope=deploy  prefix=pgs_[A-Za-z0-9_-]{8}\n\n  pgs_[A-Za-z0-9_-]{32}\n\n\^ copy this now — it will not be shown again\.\n$`).MatchString(out) {
		t.Fatalf("add: %d %q", code, out)
	}
	raw := strings.TrimSpace(strings.Split(out, "\n")[2])
	if v, err := tokens.Verify(context.Background(), d, raw); err != nil || v == nil {
		t.Fatal("the printed token verifies with the same pepper")
	}
	code, out, _ = token(t, "add", "northwind-data", "data_update", "mcpdata", "Alpha")
	if code != 0 || !strings.Contains(out, "scope=data_update") || !strings.Contains(out, "\nallowed slugs: alpha, mcpdata\n") {
		t.Fatalf("scoped add: %q", out)
	}
	if code, _, errOut := token(t, "add", "x", "deploy", "mcpdata"); code != 1 || errOut != "exact slug grants are only valid for data_update tokens\n" {
		t.Fatalf("bad grant: %d %q", code, errOut)
	}
	if code, _, errOut := token(t, "add"); code != 1 || !strings.HasPrefix(errOut, "usage: pages token add") {
		t.Fatalf("add without label: %q", errOut)
	}

	if code, out, _ := token(t, "revoke", "2"); code != 0 || out != "revoked token #2\n" {
		t.Fatalf("revoke: %q", out)
	}
	if code, out, _ := token(t, "revoke", "2"); code != 0 || out != "no active token #2\n" {
		t.Fatalf("revoke again: %q", out)
	}
	if code, _, errOut := token(t, "revoke", "two"); code != 1 || errOut != "usage: pages token revoke <id>\n" {
		t.Fatalf("revoke bad id: %q", errOut)
	}
	code, out, _ = token(t, "list")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 ||
		!regexp.MustCompile(`^#2  pgs_[A-Za-z0-9_-]{8}…  northwind-data  \[data_update\]  REVOKED  never used  slugs=alpha,mcpdata$`).MatchString(lines[0]) ||
		!regexp.MustCompile(`^#1  pgs_[A-Za-z0-9_-]{8}…  northwind-agent  \[deploy\]  active  (never used|used \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z)$`).MatchString(lines[1]) {
		t.Fatalf("list: %q", out)
	}

	t.Setenv("API_TOKEN_PEPPER", "")
	t.Setenv("PAGES_ENV", "production")
	if code, _, errOut := token(t, "list"); code != 1 || !strings.Contains(errOut, "API_TOKEN_PEPPER is required in production") {
		t.Fatalf("production without pepper: %q", errOut)
	}
}
