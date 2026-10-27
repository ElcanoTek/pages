// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/jsx"
)

// A page created without data stores the epoch as its source_as_of
// (templates.noSourceCoverage): "no source data yet", never "data from 1970".
const epoch = "1970-01-01T00:00:00.000Z"

func TestFreshnessHasNoSourceYetAtTheEpoch(t *testing.T) {
	now := ts("2026-10-27T12:00:00.000Z")
	f := FreshnessOf(FreshnessInput{SourceAsOf: ts(epoch), RefreshedAt: ts("2026-10-20T09:00:00.000Z")}, now)
	if f == nil || !f.SourceAsOf.IsZero() || f.DaysSinceSource != nil || *f.DaysSinceRefresh != 7 ||
		*f.LatestOutcome != "updated" || !f.LatestSourceAsOf.IsZero() {
		t.Fatalf("%+v", f)
	}
	// A recorded check that saw the epoch reports no source either.
	f = FreshnessOf(FreshnessInput{SourceAsOf: ts(epoch), RefreshedAt: ts("2026-10-20T09:00:00.000Z"),
		LastCheckAt: ts("2026-10-27T09:00:00.000Z"), LastCheckOutcome: "source_not_updated", LastCheckSourceAsOf: ts(epoch)}, now)
	if !f.LastCheckSourceAsOf.IsZero() || !f.LatestSourceAsOf.IsZero() || *f.LatestOutcome != "source_not_updated" {
		t.Fatalf("%+v", f)
	}
	// Only the epoch (or earlier) means "none yet"; a real stamp is a fact.
	f = FreshnessOf(FreshnessInput{SourceAsOf: ts("1970-01-01T00:00:01.000Z")}, now)
	if f.SourceAsOf.IsZero() {
		t.Fatal("a second after the epoch is a stamp")
	}
}

func TestHealthNoSourceYetIsJudgedFromCreation(t *testing.T) {
	now := ts("2026-10-22T18:30:00.000Z")
	in := daily()
	in.SourceAsOf = ts(epoch)
	in.CreatedAt = ts("2026-10-10T08:00:00.000Z")
	h := HealthOf(in, now)
	wantHealth(t, h, HealthSourceStale, "No source data yet since Oct 10; refreshes are still running (last Oct 22).", "2026-10-10T08:00:00.000Z")
	if h.DaysSinceSource != nil || strings.Contains(h.HealthReason, "1970") {
		t.Fatalf("%+v", h)
	}
	// Same allowance as a real stamp: a daily page has three days.
	in.CreatedAt = now.Add(-72 * time.Hour)
	wantHealth(t, HealthOf(in, now), HealthOK, "", "null")
	in.CreatedAt = now.Add(-72*time.Hour - time.Minute)
	wantHealth(t, HealthOf(in, now), HealthSourceStale, "", iso(t, jsx.NewTime(in.CreatedAt)))
	// A recorded check is activity too.
	in = HealthInput{Schedule: "0 15 * * *", SourceAsOf: ts(epoch), CreatedAt: ts("2026-10-10T08:00:00.000Z"),
		LastCheckAt: ts("2026-10-22T15:01:00.000Z"), LastCheckOutcome: "source_not_updated"}
	wantHealth(t, HealthOf(in, now), HealthSourceStale, "No source data yet since Oct 10; refreshes are still running (last Oct 22).", "2026-10-10T08:00:00.000Z")

	// Unscheduled and live: no age, so no hint either.
	in = daily()
	in.Schedule, in.IsLive, in.SourceAsOf = "", true, ts(epoch)
	if h := HealthOf(in, now); h.UnscheduledStale || h.DaysSinceSource != nil || h.Health != HealthUnscheduled {
		t.Fatalf("%+v", h)
	}
}
