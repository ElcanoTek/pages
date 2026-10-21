// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package db

import "testing"

func TestPoolMaxClamps(t *testing.T) {
	for v, want := range map[string]int32{
		"": 10, "abc": 10, "25": 25, "0": 1, "-3": 1, "1001": 1000,
		"4294967297": 1000, "9223372036854775807": 1000,
	} {
		t.Setenv("PG_POOL_MAX", v)
		if got := BoundsFromEnv().MaxConns; got != want {
			t.Errorf("PG_POOL_MAX=%q: MaxConns = %d, want %d", v, got, want)
		}
	}
}
