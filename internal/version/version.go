// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package version is the build identity stamped into the pages binary by the
// Makefile (`-ldflags -X …/internal/version.Version=<git describe>`).
package version

import "runtime/debug"

// Version is set at link time; an unstamped build reports "dev" plus the VCS
// revision Go recorded, if any.
var Version = "dev"

// String is Version, with the VCS revision appended to an unstamped build.
func String() string {
	if Version != "dev" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				return "dev+" + s.Value[:12]
			}
		}
	}
	return Version
}
