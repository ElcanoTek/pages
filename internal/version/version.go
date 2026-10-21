// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package version is the pages build identity. The Makefile stamps Version
// from `git describe` with -ldflags -X; an unstamped build (go build, go
// test) says "dev". The VCS revision always comes from the toolchain's
// embedded build info.
package version

import "runtime/debug"

// Version is the stamped release (a var so the linker can set it).
var Version = "dev"

// Revision is the 12-hex-digit VCS revision this binary was built from,
// "+dirty" for a modified tree, or "unknown".
func Revision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
