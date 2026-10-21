// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package install

import (
	"errors"
	"syscall"
)

// hasAccessACL reports whether path carries a POSIX access ACL, or whether
// that cannot be ruled out. With one, the group mode bits are the ACL mask,
// so a group-write bit may grant write to named users or groups whatever the
// owning group is.
func hasAccessACL(path string) bool {
	_, err := syscall.Getxattr(path, "system.posix_acl_access", nil)
	switch {
	case err == nil:
		return true
	case errors.Is(err, syscall.ENODATA), errors.Is(err, syscall.ENOTSUP), errors.Is(err, syscall.EOPNOTSUPP):
		return false // no ACL, or a filesystem without them
	}
	return true // unknown: assume the worst
}
