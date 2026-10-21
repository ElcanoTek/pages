// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

//go:build !linux

package backup

import (
	"os"
	"path/filepath"
	"syscall"
)

// openEntry opens name under dir without following a final symbolic link
// and without blocking on a FIFO. Only Linux (where backups run as root)
// opens relative to the held descriptor; elsewhere the path is re-joined,
// which is sufficient for a developer's own files.
func openEntry(dir *os.File, name string, wantDir bool) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_NONBLOCK
	if wantDir {
		flags |= syscall.O_DIRECTORY
	}
	return os.OpenFile(filepath.Join(dir.Name(), name), flags, 0)
}
