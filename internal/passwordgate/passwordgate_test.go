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

func TestTurnsSerialiseAndTimeOut(t *testing.T) {
	var turns Turns
	ctx := context.Background()
	release, ok := turns.Acquire(ctx, time.Second, PageKey(1), PortalKey(2))
	if !ok {
		t.Fatal("a free turn is granted")
	}
	if _, ok := turns.Acquire(ctx, 20*time.Millisecond, PortalKey(2)); ok {
		t.Fatal("a held turn is not granted twice")
	}
	if r, ok := turns.Acquire(ctx, 20*time.Millisecond, PageKey(3)); !ok {
		t.Fatal("other credentials are independent")
	} else {
		r()
	}
	// A waiter gets the turn as soon as it is released.
	got := make(chan bool, 1)
	go func() {
		r, ok := turns.Acquire(ctx, 5*time.Second, PortalKey(2), PageKey(1))
		got <- ok
		r()
	}()
	time.Sleep(20 * time.Millisecond)
	release()
	release() // idempotent
	if !<-got {
		t.Fatal("the waiter gets the released turn")
	}
	// A failed acquisition holds nothing.
	hold, _ := turns.Acquire(ctx, time.Second, PortalKey(2))
	if _, ok := turns.Acquire(ctx, 20*time.Millisecond, PageKey(1), PortalKey(2)); ok {
		t.Fatal("partial turns are not granted")
	}
	if r, ok := turns.Acquire(ctx, 20*time.Millisecond, PageKey(1)); !ok {
		t.Fatal("a failed acquisition released what it had taken")
	} else {
		r()
	}
	hold()
}

// An attempt waiting for a set of turns holds none of them while it waits,
// so a busy credential cannot starve the others it is requested with.
func TestTurnsAreTakenAllOrNothing(t *testing.T) {
	var turns Turns
	ctx := context.Background()
	holdB, _ := turns.Acquire(ctx, time.Second, PortalKey(2))
	waiting := make(chan struct{}, 1)
	turns.OnWait = func([]string) {
		select {
		case waiting <- struct{}{}:
		default:
		}
	}
	got := make(chan bool, 1)
	go func() {
		r, ok := turns.Acquire(ctx, 30*time.Second, PortalKey(1), PortalKey(2), PageKey(9))
		got <- ok
		r()
	}()
	<-waiting // the waiter found B held and is waiting
	for _, k := range []string{PortalKey(1), PageKey(9)} {
		r, ok := turns.Acquire(ctx, 20*time.Millisecond, k)
		if !ok {
			t.Fatalf("%s is held by an attempt still waiting for another turn", k)
		}
		r()
	}
	holdB()
	if !<-got {
		t.Fatal("the waiter gets the whole set once it is free")
	}
}
