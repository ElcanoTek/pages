// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package backup

import (
	"os"
	"path/filepath"
	"syscall"
)

// openEntry opens name inside the directory dir is open on, never following
// a symbolic link (openat with O_NOFOLLOW relative to the held descriptor, so
// no path component can be swapped in between) and never blocking on a FIFO
// (O_NONBLOCK). wantDir adds O_DIRECTORY. The caller fstats the result.
func openEntry(dir *os.File, name string, wantDir bool) (*os.File, error) {
	flags := syscall.O_RDONLY | syscall.O_CLOEXEC | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if wantDir {
		flags |= syscall.O_DIRECTORY
	}
	raw, err := dir.SyscallConn()
	if err != nil {
		return nil, err
	}
	fd := -1
	var oerr error
	if err := raw.Control(func(dfd uintptr) {
		for {
			fd, oerr = syscall.Openat(int(dfd), name, flags, 0)
			if oerr != syscall.EINTR {
				return
			}
		}
	}); err != nil {
		return nil, err
	}
	if oerr != nil {
		return nil, &os.PathError{Op: "openat", Path: filepath.Join(dir.Name(), name), Err: oerr}
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir.Name(), name)), nil
}
