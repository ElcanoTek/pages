// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package install

import (
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTrustedFile(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "release", "pages")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "current")
	if err := os.Symlink(filepath.Dir(bin), link); err != nil {
		t.Fatal(err)
	}
	got, err := TrustedFile(filepath.Join(link, "pages"))
	if err != nil || got != bin {
		t.Fatalf("a trusted binary behind a link resolves to its real path: %q %v", got, err)
	}
	if _, err := TrustedDir(filepath.Dir(bin)); err != nil {
		t.Fatal(err)
	}
	// Writable by others: the file, or any directory above it.
	if err := os.Chmod(bin, 0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := TrustedFile(bin); err == nil || !strings.Contains(err.Error(), "writable by other accounts") {
		t.Fatalf("group-writable binary: %v", err)
	}
	_ = os.Chmod(bin, 0o755)
	if err := os.Chmod(filepath.Dir(bin), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := TrustedFile(bin); err == nil {
		t.Fatal("a binary in a world-writable directory is not trusted")
	}
	if _, err := TrustedDir(filepath.Dir(bin)); err == nil {
		t.Fatal("a world-writable directory is not trusted")
	}
	// Sticky world-writable directories ABOVE (like /tmp) are fine.
	if err := os.Chmod(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if _, err := TrustedFile(bin); err != nil {
		t.Fatalf("sticky ancestor: %v", err)
	}
	if _, err := TrustedFile(dir); err == nil {
		t.Fatal("a directory is not a trusted file")
	}
	if os.Geteuid() == 0 {
		nobody, err := user.Lookup("nobody")
		if err != nil {
			t.Skip(err)
		}
		uid, _ := strconv.Atoi(nobody.Uid)
		if err := os.Chown(bin, uid, -1); err != nil {
			t.Fatal(err)
		}
		if _, err := TrustedFile(bin); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Fatalf("an unprivileged account's binary: %v", err)
		}
	}
}

func TestWritableBy(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to own files as another account")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip(err)
	}
	uid, _ := strconv.Atoi(nobody.Uid)
	gid, _ := strconv.Atoi(nobody.Gid)
	src := filepath.Join(t.TempDir(), "src")
	hook := filepath.Join(src, ".git", "hooks", "post-merge")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent", filepath.Join(src, "dangling")); err != nil {
		t.Fatal(err)
	}
	if p, err := WritableBy(src, "nobody"); err != nil || p != "" {
		t.Fatalf("root-owned tree: %q %v", p, err)
	}
	if p, _ := WritableBy(src, "root"); p != "" {
		t.Fatal("root is not a less-privileged account")
	}
	if p, _ := WritableBy(src, "no-such-account-pages-test"); p != "" {
		t.Fatal("an unknown account owns nothing")
	}
	for _, c := range []struct {
		name string
		set  func() error
	}{
		{"owned", func() error { return os.Chown(hook, uid, -1) }},
		{"group", func() error {
			if err := os.Chown(hook, 0, gid); err != nil {
				return err
			}
			return os.Chmod(hook, 0o775)
		}},
		{"world", func() error { return os.Chmod(hook, 0o757) }},
		{"world dir", func() error { return os.Chmod(filepath.Dir(hook), 0o777|os.ModeSticky) }},
	} {
		if err := c.set(); err != nil {
			t.Fatal(err)
		}
		if p, err := WritableBy(src, "nobody"); err != nil || p == "" {
			t.Fatalf("%s: not detected (%v)", c.name, err)
		}
		_ = os.Chown(hook, 0, 0)
		_ = os.Chmod(hook, 0o755)
		_ = os.Chmod(filepath.Dir(hook), 0o755)
	}
	// An ancestor the account owns lets it swap the whole tree.
	parent := filepath.Dir(src)
	if err := os.Chown(parent, uid, -1); err != nil {
		t.Fatal(err)
	}
	if p, _ := WritableBy(src, "nobody"); p != parent {
		t.Fatalf("ancestor: %q", p)
	}
}
