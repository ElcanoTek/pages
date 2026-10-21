// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package install

import (
	"encoding/binary"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// TestWritableByACLGrant: a root-owned entry whose owning group is unrelated
// to the account can still be writable by it through a named-user ACL entry
// (the group mode bits then show the ACL mask). Such an entry is reported.
func TestWritableByACLGrant(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to set ACLs on root-owned files")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip(err)
	}
	uid, _ := strconv.Atoi(nobody.Uid)
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	makefile := filepath.Join(src, "Makefile")
	if err := os.WriteFile(makefile, []byte("all:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := WritableBy(src, "nobody"); err != nil || p != "" {
		t.Fatalf("before the ACL: %q %v", p, err)
	}
	// user::rw- user:nobody:rw- group::r-- mask::rw- other::r-- (the Linux
	// xattr encoding: a version header, then tag/perm/id entries).
	acl := binary.LittleEndian.AppendUint32(nil, 2)
	for _, e := range []struct {
		tag, perm uint16
		id        uint32
	}{{0x01, 6, ^uint32(0)}, {0x02, 6, uint32(uid)}, {0x04, 4, ^uint32(0)}, {0x10, 6, ^uint32(0)}, {0x20, 4, ^uint32(0)}} {
		acl = binary.LittleEndian.AppendUint16(acl, e.tag)
		acl = binary.LittleEndian.AppendUint16(acl, e.perm)
		acl = binary.LittleEndian.AppendUint32(acl, e.id)
	}
	if err := syscall.Setxattr(makefile, "system.posix_acl_access", acl, 0); err != nil {
		t.Skipf("this filesystem does not take POSIX ACLs: %v", err)
	}
	st, _ := os.Stat(makefile)
	if st.Mode().Perm()&0o020 == 0 {
		t.Fatalf("the ACL mask should show as group write, mode %v", st.Mode())
	}
	if p, err := WritableBy(src, "nobody"); err != nil || p != makefile {
		t.Fatalf("a named-user ACL write grant was missed: %q %v", p, err)
	}
}
