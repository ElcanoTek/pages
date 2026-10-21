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
// primary group, or world-writable (a sticky directory above root
// excepted). Symbolic links are not followed; their own permissions do not
// matter, and the directory holding one decides who can replace it. ""
// means none, so root may run what the tree holds (the updater runs its
// version control and build tools in the source checkout as root). An
// account that does not exist owns nothing.
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
	gid, _ := strconv.Atoi(u.Gid)
	if uid == 0 || uid == os.Geteuid() {
		return "", nil // root (or the caller) is not a less-privileged account
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
		case int(sys.Gid) == gid && perm&0o020 != 0:
			return true
		case perm&0o002 != 0:
			return !stickyOK || !st.IsDir() || st.Mode()&os.ModeSticky == 0
		}
		return false
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		st, err := os.Lstat(dir)
		if err != nil {
			return "", err
		}
		if writable(st, true) {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	found := ""
	err = filepath.Walk(abs, func(p string, st os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if writable(st, false) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

func trustedEntry(path string, st os.FileInfo, stickyOK bool) error {
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("no ownership information for " + path)
	}
	if sys.Uid != 0 && int(sys.Uid) != os.Geteuid() {
		return fmt.Errorf("%s is owned by uid %d, not root", path, sys.Uid)
	}
	if st.Mode().Perm()&0o022 != 0 && !(stickyOK && st.IsDir() && st.Mode()&os.ModeSticky != 0) {
		return fmt.Errorf("%s is writable by other accounts (mode %o)", path, st.Mode().Perm())
	}
	return nil
}
