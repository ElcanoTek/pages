// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBodyReadTimeoutScales(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want time.Duration
	}{
		{-1, 30 * time.Second},
		{0, 30 * time.Second},
		{64 * 1024, 31 * time.Second},
		{32 << 20, 30*time.Second + 512*time.Second},
		{64 << 20, 30*time.Second + 1024*time.Second},
		{1 << 40, 20 * time.Minute},
	} {
		if got := BodyReadTimeout(c.n); got != c.want {
			t.Fatalf("BodyReadTimeout(%d) = %v, want %v", c.n, got, c.want)
		}
	}
}

// A recorder has no connection: arming is a harmless no-op, with or without
// the controller recorded.
func TestArmBodyReadDeadlineWithoutConn(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	ArmBodyReadDeadline(r, 1<<20)()
	ArmBodyReadDeadline(WithBodyReadControl(w, r), 1<<20)()
}
