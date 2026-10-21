// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

//go:build !linux

package install

// hasAccessACL cannot inspect ACLs here, so it assumes one: a group-write
// bit then always counts as writable.
func hasAccessACL(string) bool { return true }
