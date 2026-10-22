// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

//go:build race

package render

// raceEnabled: the race detector slows the timing tests by an order of
// magnitude, so they only assert their bounds without it.
const raceEnabled = true
