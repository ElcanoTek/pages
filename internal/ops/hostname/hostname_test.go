// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package hostname

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/runner"
	"github.com/ElcanoTek/pages/internal/ops/term"
)

func TestRewriteHosts(t *testing.T) {
	in := "127.0.0.1 localhost vultr.guest vultr # cloud\n::1 localhost vultr.guest\n192.0.2.4 other\n"
	got := RewriteHosts(in, "vultr.guest", "pages.contoso.example")
	want := "127.0.0.1 localhost pages.contoso.example pages # cloud\n::1 localhost pages.contoso.example pages\n192.0.2.4 other\n"
	if got != want {
		t.Fatalf("got:\n%s", got)
	}
	// A localhost name is never replaced: the lines are appended instead.
	got = RewriteHosts("127.0.0.1 localhost localhost.localdomain\n", "localhost.localdomain", "pages.contoso.example")
	if got != "127.0.0.1 localhost localhost.localdomain\n127.0.0.1 pages.contoso.example pages\n::1 pages.contoso.example pages\n" {
		t.Fatalf("got:\n%s", got)
	}
	// Idempotent.
	if again := RewriteHosts(want, "pages.contoso.example", "pages.contoso.example"); again != want {
		t.Fatalf("got:\n%s", again)
	}
}

func setter(t *testing.T, current string, env map[string]string) (Setter, *runner.Fake, *bytes.Buffer, string) {
	t.Helper()
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "etc/cloud"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "etc/hosts"), []byte("127.0.0.1 localhost "+current+"\n"), 0o644)
	fake := &runner.Fake{Handle: func(c runner.Cmd) (string, int) {
		if c.Args[0] == "--static" {
			return current + "\n", 0
		}
		return "", 0
	}}
	var b bytes.Buffer
	return Setter{Root: root, Runner: fake, Log: term.Buffer(&b), Getenv: func(k string) string { return env[k] }}, fake, &b, root
}

func TestSetRenamesOnlyCloudDefaults(t *testing.T) {
	s, fake, out, root := setter(t, "vultr.guest", nil)
	s.Set(context.Background(), "pages.contoso.example")
	if !strings.Contains(fake.Log(), "hostnamectl set-hostname pages.contoso.example") || !strings.Contains(out.String(), "hostname set to pages.contoso.example (was vultr.guest)") {
		t.Fatal(fake.Log(), out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/cloud/cloud.cfg.d/99-preserve-hostname.cfg")); string(b) != CloudConfig {
		t.Fatal(string(b))
	}
	if b, _ := os.ReadFile(filepath.Join(root, "etc/hosts")); !strings.Contains(string(b), "127.0.0.1 localhost pages.contoso.example pages") {
		t.Fatal(string(b))
	}

	s, fake, out, _ = setter(t, "northwind-box.example", nil)
	s.Set(context.Background(), "pages.contoso.example")
	if strings.Contains(fake.Log(), "set-hostname") || !strings.Contains(out.String(), "to name the box after the service: hostnamectl set-hostname pages.contoso.example") {
		t.Fatal(fake.Log(), out.String())
	}
	s, fake, _, _ = setter(t, "northwind-box.example", map[string]string{"PAGES_SET_HOSTNAME": "1"})
	s.Set(context.Background(), "pages.contoso.example")
	if !strings.Contains(fake.Log(), "set-hostname") {
		t.Fatal("PAGES_SET_HOSTNAME=1 must force")
	}
	s, fake, _, _ = setter(t, "vultr.guest", map[string]string{"PAGES_SET_HOSTNAME": "0"})
	s.Set(context.Background(), "pages.contoso.example")
	if fake.Log() != "" {
		t.Fatal("PAGES_SET_HOSTNAME=0 must skip")
	}
	for _, bad := range []string{"192.0.2.4", "single", "bad_name.example"} {
		s, fake, _, _ = setter(t, "vultr.guest", nil)
		s.Set(context.Background(), bad)
		if strings.Contains(fake.Log(), "set-hostname") {
			t.Fatalf("%q accepted", bad)
		}
	}
}
