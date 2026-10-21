// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package update

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/ops/install"
)

// TestCandidateStaysRootsButRunsAsServiceAccount runs the real thing as
// root: a candidate made trusted is executable by an unprivileged account
// (the preflight steps run as APP_USER) yet not replaceable by it.
func TestCandidateStaysRootsButRunsAsServiceAccount(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := exec.LookPath("runuser"); err != nil {
		t.Skip(err)
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip(err)
	}
	base := t.TempDir()
	for p := base; p != "/tmp" && p != "/"; p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o755); err != nil { // t.TempDir is 0700
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(base, ".candidate.x")
	if err := os.Mkdir(candidate, 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(candidate, install.BinaryName)
	write(t, bin, "#!/bin/sh\necho preflight ok\n", 0o700)
	trusted, err := trustedCandidate(candidate)
	if err != nil || trusted != bin {
		t.Fatalf("%q %v", trusted, err)
	}
	out, err := exec.Command("runuser", "-u", "nobody", "--", trusted).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "preflight ok" {
		t.Fatalf("service account cannot run the candidate: %v %s", err, out)
	}
	for _, attempt := range [][]string{
		{"sh", "-c", "echo pwned > " + bin},
		{"mv", bin, bin + ".old"},
		{"touch", filepath.Join(candidate, "planted")},
	} {
		if out, err := exec.Command("runuser", append([]string{"-u", "nobody", "--"}, attempt...)...).CombinedOutput(); err == nil {
			t.Fatalf("service account could %v: %s", attempt, out)
		}
	}
	if got := read(t, bin); got != "#!/bin/sh\necho preflight ok\n" {
		t.Fatalf("candidate changed: %q", got)
	}
	// Owned by another account, the candidate is refused.
	if err := os.Chown(bin, 65534, 65534); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedCandidate(candidate); err == nil || !strings.Contains(err.Error(), "not root") {
		t.Fatalf("a service-account-owned binary must be refused: %v", err)
	}
}
