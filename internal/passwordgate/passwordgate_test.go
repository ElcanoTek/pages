// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package passwordgate

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/testdb"
)

func TestDelayForFailures(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{0, 0}, {-3, 0}, {1, BaseDelay}, {2, time.Second}, {3, 2 * time.Second},
		{4, 4 * time.Second}, {5, MaxDelay}, {64, MaxDelay}, {1 << 40, MaxDelay},
	} {
		if got := DelayForFailures(tc.n); got != tc.want {
			t.Errorf("DelayForFailures(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestRecordAndClear(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	var pageID, portalID int64
	if err := d.QueryRow(ctx, `INSERT INTO pages (slug) VALUES ('northwind') RETURNING id`).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(ctx, `INSERT INTO page_portals (slug, name, password_hash) VALUES ('nwm', 'Northwind', 'h') RETURNING id`).Scan(&portalID); err != nil {
		t.Fatal(err)
	}
	for i, want := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} {
		got, err := RecordFailure(ctx, d, pageID)
		if err != nil || got != want {
			t.Fatalf("failure %d: %v %v, want %v", i+1, got, err, want)
		}
	}
	// Quiet past the window → the counter resets.
	if _, err := d.Exec(ctx, `UPDATE page_password_failures SET last_fail_at = now() - interval '16 minutes'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := RecordFailure(ctx, d, pageID); got != BaseDelay {
		t.Fatalf("after the window: %v", got)
	}
	ClearFailures(ctx, d, pageID)
	if got, _ := RecordFailure(ctx, d, pageID); got != BaseDelay {
		t.Fatalf("after clear: %v", got)
	}

	// The portal counter is its own budget.
	if got, err := RecordPortalFailure(ctx, d, portalID); err != nil || got != BaseDelay {
		t.Fatalf("portal: %v %v", got, err)
	}
	if got, _ := RecordPortalFailure(ctx, d, portalID); got != time.Second {
		t.Fatalf("portal second: %v", got)
	}
	ClearPortalFailures(ctx, d, portalID)
	if got, _ := RecordPortalFailure(ctx, d, portalID); got != BaseDelay {
		t.Fatalf("portal after clear: %v", got)
	}
}
