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
	// Links that stay in the tree are fine (the walk checks what they name).
	if err := os.Symlink(filepath.Join("hooks", "post-merge"), filepath.Join(src, ".git", "inside")); err != nil {
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

// A group-writable entry owned by one of the account's supplementary groups
// is as writable as one owned by its primary group.
func TestWritableBySupplementaryGroup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to own files as another account")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip(err)
	}
	const extra = 4242 // no account's primary group
	if nobody.Gid == strconv.Itoa(extra) {
		t.Skip("nobody's primary group collides with the test group")
	}
	saved := groupIDs
	groupIDs = func(u *user.User) ([]string, error) {
		gids, err := saved(u)
		if u.Username == "nobody" {
			gids = append(gids, strconv.Itoa(extra))
		}
		return gids, err
	}
	t.Cleanup(func() { groupIDs = saved })
	src := filepath.Join(t.TempDir(), "src")
	makefile := filepath.Join(src, "Makefile")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(makefile, []byte("all:\n"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(makefile, 0o664); err != nil {
		t.Fatal(err)
	}
	if p, err := WritableBy(src, "nobody"); err != nil || p != "" {
		t.Fatalf("root's group: %q %v", p, err)
	}
	if err := os.Chown(makefile, 0, extra); err != nil {
		t.Fatal(err)
	}
	if p, err := WritableBy(src, "nobody"); err != nil || p != makefile {
		t.Fatalf("a supplementary group's writable Makefile: %q %v", p, err)
	}
}

// A link in the checkout must not let root follow it to something the
// account controls: outside the tree, dangling, or out and back in again.
func TestWritableBySymlinks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to own files as another account")
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip(err)
	}
	base := t.TempDir()
	src := filepath.Join(base, "src")
	if err := os.MkdirAll(filepath.Join(src, "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "build", "Makefile"), []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.WriteFile(outside, []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A link outside the tree that leads back in: whoever can replace it
	// decides what root builds, though it ends inside the tree.
	if err := os.Symlink(filepath.Join(src, "build", "Makefile"), filepath.Join(base, "hop")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(src, "Makefile")
	for _, c := range []struct {
		name, dest string
		ok         bool
	}{
		{"inside", filepath.Join("build", "Makefile"), true},
		{"inside absolute", filepath.Join(src, "build", "Makefile"), true},
		{"outside", outside, false},
		{"outside relative", filepath.Join("..", "outside"), false},
		{"dangling", filepath.Join("build", "missing"), false},
		{"out and back", filepath.Join("..", "hop"), false},
	} {
		_ = os.Remove(link)
		if err := os.Symlink(c.dest, link); err != nil {
			t.Fatal(err)
		}
		p, err := WritableBy(src, "nobody")
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.ok && p != "" {
			t.Fatalf("%s: a link within the tree is safe, got %q", c.name, p)
		}
		if !c.ok && p != link {
			t.Fatalf("%s: got %q, want the link %s", c.name, p, link)
		}
	}
	// A checkout reached through a link is walked where it really lives.
	_ = os.Remove(link)
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(src, alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "build", "Makefile"), 0o666); err != nil {
		t.Fatal(err)
	}
	if p, err := WritableBy(alias, "nobody"); err != nil || p != filepath.Join(src, "build", "Makefile") {
		t.Fatalf("linked checkout: %q %v", p, err)
	}
}
