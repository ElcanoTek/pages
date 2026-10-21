// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package install

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// TrustedFile resolves path and reports whether only a trusted account can
// change what it names: the file is a regular file and it and every
// directory above it are owned by root (or by the effective user, who could
// change it anyway) and are not group- or world-writable. A world-writable
// directory with the sticky bit (/tmp) is accepted: other accounts cannot
// rename or remove a trusted owner's entry in it. The resolved path is
// returned; a caller that executes the file must execute that path, so a
// symlink along the original path cannot be swapped in between.
//
// This is the test root applies before executing a binary: the service
// account (APP_USER) must never be able to replace code root runs.
func TrustedFile(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", resolved)
	}
	if err := trustedEntry(resolved, st, false); err != nil {
		return "", err
	}
	if err := trustedAncestors(filepath.Dir(resolved)); err != nil {
		return "", err
	}
	return resolved, nil
}

// TrustedDir is TrustedFile for a directory: dir (resolved) and every
// directory above it are trusted-owned and not writable by other accounts
// (sticky directories excepted above dir itself).
func TrustedDir(dir string) (string, error) {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	st, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	if err := trustedEntry(resolved, st, false); err != nil {
		return "", err
	}
	if err := trustedAncestors(filepath.Dir(resolved)); err != nil {
		return "", err
	}
	return resolved, nil
}

func trustedAncestors(dir string) error {
	for {
		st, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if err := trustedEntry(dir, st, true); err != nil {
			return err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

// WritableBy returns the first path at or under root, or above it, that
// the account named name could change: owned by it, group-writable with its
// primary group or any supplementary group it belongs to, or world-writable
// (a sticky directory above root excepted). ""
// means none, so root may run what the tree holds (the updater runs its
// version control and build tools in the source checkout as root). An
// account that does not exist owns nothing.
//
// A symbolic link's own permissions do not matter (the directory holding
// one decides who can replace it), but what it names does: a link in the
// tree must resolve, and resolve to a path inside the tree, which the walk
// checks. A link that dangles (its target could be created later), loops,
// or leaves the tree is reported as the path the account could change, since
// the tree's own permissions do not protect what it names.
func WritableBy(root, name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return "", nil
		}
		return "", err
	}
	uid, _ := strconv.Atoi(u.Uid)
	if uid == 0 || uid == os.Geteuid() {
		return "", nil // root (or the caller) is not a less-privileged account
	}
	groups := map[int]bool{}
	if gid, err := strconv.Atoi(u.Gid); err == nil {
		groups[gid] = true
	}
	gids, err := groupIDs(u)
	if err != nil {
		return "", fmt.Errorf("cannot list the groups of %s: %w", name, err)
	}
	for _, g := range gids {
		if gid, err := strconv.Atoi(g); err == nil {
			groups[gid] = true
		}
	}
	writable := func(st os.FileInfo, stickyOK bool) bool {
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok || st.Mode()&os.ModeSymlink != 0 {
			return false
		}
		perm := st.Mode().Perm()
		switch {
		case int(sys.Uid) == uid:
			return true
		case groups[int(sys.Gid)] && perm&0o020 != 0:
			return true
		case perm&0o002 != 0:
			return !stickyOK || !st.IsDir() || st.Mode()&os.ModeSticky == 0
		}
		return false
	}
	ancestors := func(path string) (string, error) {
		for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
			st, err := os.Lstat(dir)
			if err != nil {
				return "", err
			}
			if writable(st, true) {
				return dir, nil
			}
			if filepath.Dir(dir) == dir {
				return "", nil
			}
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if p, err := ancestors(abs); err != nil || p != "" {
		return p, err
	}
	// The walk does not follow links, so walk the tree's resolved location
	// (whose ancestors decide who can swap it, too).
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	if resolved != abs {
		if p, err := ancestors(resolved); err != nil || p != "" {
			return p, err
		}
	}
	inside := func(p string) bool {
		rel, err := filepath.Rel(resolved, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	found := ""
	err = filepath.Walk(resolved, func(p string, st os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			// Each hop must stay in the tree (every link it lands on is
			// walked too), and so must where the chain finally resolves.
			dest, err := os.Readlink(p)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(dest) {
				dest = filepath.Join(filepath.Dir(p), dest)
			}
			target, err := filepath.EvalSymlinks(p)
			if err != nil || !inside(filepath.Clean(dest)) || !inside(target) {
				found = p
				return filepath.SkipAll
			}
			return nil
		}
		if writable(st, false) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

// groupIDs lists the groups an account belongs to (a test seam: test hosts
// rarely have an account with a supplementary group to spare).
var groupIDs = (*user.User).GroupIds

func trustedEntry(path string, st os.FileInfo, stickyOK bool) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("no ownership information for " + path)
	}
	if sys.Uid != 0 && int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not root", path, sys.Uid)
	}
	sticky := stickyOK && st.IsDir() && st.Mode()&os.ModeSticky != 0
	if st.Mode().Perm()&0o022 != 0 && !sticky {
		return fmt.Errorf("%s is writable by other accounts (mode %o)", path, st.Mode().Perm())
	}
	return nil
}
