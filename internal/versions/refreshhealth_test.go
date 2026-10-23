// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsx"
)

// The calendar these cases read against: 2026-10-22 is a Thursday, 10-24 a
// Saturday, 10-26 a Monday.

func iso(t *testing.T, tm jsx.Time) string {
	t.Helper()
	if tm.IsZero() {
		return "null"
	}
	return jsx.FormatTime(tm.Time)
}

func wantHealth(t *testing.T, h *RefreshHealth, health, reason, since string) {
	t.Helper()
	if h.Health != health {
		t.Fatalf("health %q, want %q (reason %q)", h.Health, health, h.HealthReason)
	}
	if reason != "" && h.HealthReason != reason {
		t.Fatalf("reason %q, want %q", h.HealthReason, reason)
	}
	if got := iso(t, h.HealthSince); got != since {
		t.Fatalf("health_since %s, want %s", got, since)
	}
}

// A healthy daily page: refreshed yesterday and today, data through
// yesterday. Every case below breaks one thing about it.
func daily() HealthInput {
	return HealthInput{
		Schedule:    "0 15 * * *",
		Owner:       "trader@example.com",
		CreatedAt:   ts("2026-09-01T10:00:00.000Z"),
		SourceAsOf:  ts("2026-10-21T00:00:00.000Z"),
		RefreshedAt: ts("2026-10-22T15:12:00.000Z"),
	}
}

func TestHealthUnscheduledIsNoJudgement(t *testing.T) {
	now := ts("2026-10-22T18:30:00.000Z")
	in := daily()
	in.Schedule = ""
	// Even a failed run and a six-week-old source: no schedule, no verdict.
	in.SourceAsOf = ts("2026-09-01T00:00:00.000Z")
	in.LastCheckAt, in.LastCheckOutcome, in.LastCheckDetail = ts("2026-10-22T16:00:00.000Z"), "blocked", "schema drift"
	h := HealthOf(in, now)
	wantHealth(t, h, HealthUnscheduled, "No refresh schedule is set, so Pages does not judge this page.", "null")
	if h.RefreshSchedule != nil || *h.RefreshOwner != "trader@example.com" || !h.LastDueAt.IsZero() || !h.NextDueAt.IsZero() {
		t.Fatalf("%+v", h)
	}
	if iso(t, h.LastActivityAt) != "2026-10-22T16:00:00.000Z" || h.NeedsAttention() {
		t.Fatalf("activity is still reported, and nothing needs attention: %+v", h)
	}
	in.Schedule = "0 15 * * MON"
	wantHealth(t, HealthOf(in, now), HealthUnscheduled,
		"The stored refresh schedule is not a valid cron expression, so Pages does not judge this page; set it again.", "null")
}

func TestHealthOK(t *testing.T) {
	now := ts("2026-10-22T18:30:00.000Z")
	h := HealthOf(daily(), now)
	wantHealth(t, h, HealthOK, "Refreshing on schedule; last activity Oct 22 15:12 UTC.", "null")
	if iso(t, h.LastDueAt) != "2026-10-22T15:00:00.000Z" || iso(t, h.NextDueAt) != "2026-10-23T15:00:00.000Z" || *h.RefreshSchedule != "0 15 * * *" {
		t.Fatalf("%+v", h)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"refresh_schedule":"0 15 * * *","refresh_owner":"trader@example.com","health":"ok",` +
		`"health_reason":"Refreshing on schedule; last activity Oct 22 15:12 UTC.","health_since":null,` +
		`"last_due_at":"2026-10-22T15:00:00.000Z","next_due_at":"2026-10-23T15:00:00.000Z","last_activity_at":"2026-10-22T15:12:00.000Z"}`
	if string(b) != want {
		t.Fatalf("wire shape:\n%s\n%s", b, want)
	}
	// "No new data today" is a run doing its job.
	in := daily()
	in.RefreshedAt = ts("2026-10-21T15:05:00.000Z")
	in.LastCheckAt, in.LastCheckOutcome = ts("2026-10-22T15:03:00.000Z"), "source_not_updated"
	wantHealth(t, HealthOf(in, now), HealthOK, "Refreshing on schedule; last activity Oct 22 15:03 UTC.", "null")
}

func TestHealthBlocked(t *testing.T) {
	now := ts("2026-10-22T18:30:00.000Z")
	for _, outcome := range FailedCheckOutcomes {
		in := daily()
		in.RefreshedAt = ts("2026-10-21T15:05:00.000Z")
		in.LastCheckAt, in.LastCheckOutcome = ts("2026-10-22T15:20:00.000Z"), outcome
		in.LastCheckDetail = "  required source ssp_daily\n   could not be authenticated  "
		h := HealthOf(in, now)
		wantHealth(t, h, HealthBlocked, "required source ssp_daily could not be authenticated", "2026-10-22T15:20:00.000Z")
		if !h.NeedsAttention() {
			t.Fatal("blocked needs attention")
		}
	}
	in := daily()
	in.RefreshedAt = ts("2026-10-21T15:05:00.000Z")
	in.LastCheckAt, in.LastCheckOutcome = ts("2026-10-22T15:20:00.000Z"), "source_unreachable"
	wantHealth(t, HealthOf(in, now), HealthBlocked, "The latest refresh run recorded source unreachable on Oct 22, with no detail.", "2026-10-22T15:20:00.000Z")

	in.LastCheckDetail = strings.Repeat("x", 600)
	if r := HealthOf(in, now).HealthReason; len([]rune(r)) != maxReasonDetail || !strings.HasSuffix(r, "…") {
		t.Fatalf("a long detail is clipped: %d", len([]rune(r)))
	}

	// Blocked outranks overdue: blocked three days ago and nothing since.
	in = daily()
	in.RefreshedAt = ts("2026-10-18T15:05:00.000Z")
	in.LastCheckAt, in.LastCheckOutcome, in.LastCheckDetail = ts("2026-10-19T15:10:00.000Z"), "blocked", "schema drift"
	wantHealth(t, HealthOf(in, now), HealthBlocked, "schema drift", "2026-10-19T15:10:00.000Z")

	// A later refresh clears it — published, or waiting for approval.
	in = daily()
	in.LastCheckAt, in.LastCheckOutcome = ts("2026-10-22T15:01:00.000Z"), "blocked"
	wantHealth(t, HealthOf(in, now), HealthOK, "", "null")
	in.RefreshedAt = ts("2026-10-20T15:05:00.000Z")
	in.NewestRefreshAt = ts("2026-10-22T15:30:00.000Z")
	wantHealth(t, HealthOf(in, now), HealthOK, "Refreshing on schedule; last activity Oct 22 15:30 UTC.", "null")
	// On equal timestamps the explicit check wins, as in freshness.
	in.NewestRefreshAt = time.Time{}
	in.RefreshedAt = in.LastCheckAt
	wantHealth(t, HealthOf(in, now), HealthBlocked, "", "2026-10-22T15:01:00.000Z")
}

func TestHealthOverdueDaily(t *testing.T) {
	in := daily()
	in.RefreshedAt = ts("2026-10-20T15:12:00.000Z")
	// Today's 15:00 fire is still inside its grace, but yesterday's went
	// unanswered: the page does not flap back to ok every afternoon.
	h := HealthOf(in, ts("2026-10-22T16:00:00.000Z"))
	wantHealth(t, h, HealthOverdue, "Expected a refresh at Oct 21 15:00 UTC; last activity Oct 20.", "2026-10-21T15:00:00.000Z")
	if !h.NeedsAttention() || iso(t, h.LastDueAt) != "2026-10-22T15:00:00.000Z" {
		t.Fatalf("%+v", h)
	}
	// Once today's grace has passed, today's fire is the one named; the
	// problem still started with the first fire that went unanswered.
	wantHealth(t, HealthOf(in, ts("2026-10-22T18:01:00.000Z")), HealthOverdue,
		"Expected a refresh at Oct 22 15:00 UTC; last activity Oct 20.", "2026-10-21T15:00:00.000Z")

	// The grace boundary: a run that has not landed by 18:00 is late.
	in = daily()
	in.RefreshedAt = ts("2026-10-21T15:12:00.000Z")
	wantHealth(t, HealthOf(in, ts("2026-10-22T17:59:00.000Z")), HealthOK, "", "null")
	wantHealth(t, HealthOf(in, ts("2026-10-22T18:00:00.000Z")), HealthOverdue,
		"Expected a refresh at Oct 22 15:00 UTC; last activity Oct 21.", "2026-10-22T15:00:00.000Z")

	// A recorded check is activity too.
	in.LastCheckAt, in.LastCheckOutcome = ts("2026-10-22T15:40:00.000Z"), "source_not_updated"
	wantHealth(t, HealthOf(in, ts("2026-10-22T18:00:00.000Z")), HealthOK, "", "null")
}

func TestHealthEarlyRunCountsForItsFire(t *testing.T) {
	now := ts("2026-10-22T20:00:00.000Z")
	in := daily()
	// The task runs at 14:30, the page says 15:00.
	in.RefreshedAt = ts("2026-10-22T14:41:00.000Z")
	wantHealth(t, HealthOf(in, now), HealthOK, "", "null")
	// More than an hour early is a different run.
	in.RefreshedAt = ts("2026-10-22T13:59:00.000Z")
	wantHealth(t, HealthOf(in, now), HealthOverdue, "Expected a refresh at Oct 22 15:00 UTC; last activity Oct 22.", "2026-10-22T15:00:00.000Z")
	// The tolerance never exceeds half the gap: every 30 minutes, a run 20
	// minutes early answers nothing.
	in.Schedule = "*/30 * * * *"
	in.RefreshedAt = ts("2026-10-22T16:40:00.000Z")
	wantHealth(t, HealthOf(in, now), HealthOverdue, "", "2026-10-22T17:00:00.000Z")
	// ...and a run 10 minutes early does: the 17:00 fire is answered, so the
	// first one missed is 17:30.
	in.RefreshedAt = ts("2026-10-22T16:50:00.000Z")
	wantHealth(t, HealthOf(in, now), HealthOK, "", "null")
	wantHealth(t, HealthOf(in, ts("2026-10-22T20:31:00.000Z")), HealthOverdue, "", "2026-10-22T17:30:00.000Z")
}

func TestHealthWeekdaysOverAWeekend(t *testing.T) {
	in := daily()
	in.Schedule = "0 15 * * 1-5"
	in.SourceAsOf = ts("2026-10-22T00:00:00.000Z")
	in.RefreshedAt = ts("2026-10-23T15:10:00.000Z") // Friday's run
	for _, now := range []string{"2026-10-24T12:00:00.000Z", "2026-10-25T23:00:00.000Z", "2026-10-26T17:59:00.000Z"} {
		h := HealthOf(in, ts(now))
		wantHealth(t, h, HealthOK, "Refreshing on schedule; last activity Oct 23 15:10 UTC.", "null")
		if iso(t, h.NextDueAt) != "2026-10-26T15:00:00.000Z" && now != "2026-10-26T17:59:00.000Z" {
			t.Fatalf("%s: next due %s", now, iso(t, h.NextDueAt))
		}
	}
	// Monday's run never came.
	wantHealth(t, HealthOf(in, ts("2026-10-26T18:00:00.000Z")), HealthOverdue,
		"Expected a refresh at Oct 26 15:00 UTC; last activity Oct 23.", "2026-10-26T15:00:00.000Z")
	// Friday's never came either: overdue all weekend, since Friday.
	in.RefreshedAt = ts("2026-10-22T15:10:00.000Z")
	wantHealth(t, HealthOf(in, ts("2026-10-25T09:00:00.000Z")), HealthOverdue,
		"Expected a refresh at Oct 23 15:00 UTC; last activity Oct 22.", "2026-10-23T15:00:00.000Z")
}

func TestHealthWeekly(t *testing.T) {
	in := daily()
	in.Schedule = "0 9 * * 1"
	in.SourceAsOf = ts("2026-10-18T00:00:00.000Z")
	in.RefreshedAt = ts("2026-10-19T09:20:00.000Z")
	wantHealth(t, HealthOf(in, ts("2026-10-25T23:00:00.000Z")), HealthOK, "", "null")
	wantHealth(t, HealthOf(in, ts("2026-10-26T12:00:00.000Z")), HealthOverdue,
		"Expected a refresh at Oct 26 09:00 UTC; last activity Oct 19.", "2026-10-26T09:00:00.000Z")
	// The weekly run happened and found nothing new: nine days of slack (a
	// week between fires plus two).
	in.LastCheckAt, in.LastCheckOutcome = ts("2026-10-26T09:15:00.000Z"), "source_not_updated"
	wantHealth(t, HealthOf(in, ts("2026-10-26T12:00:00.000Z")), HealthOK, "", "null")
	wantHealth(t, HealthOf(in, ts("2026-10-27T00:01:00.000Z")), HealthSourceStale,
		"Source data hasn't advanced since Oct 18; refreshes are still running (last Oct 26).", "2026-10-18T00:00:00.000Z")
}

func TestHealthSourceStale(t *testing.T) {
	now := ts("2026-10-22T18:30:00.000Z")
	in := daily()
	in.RefreshedAt = ts("2026-09-29T15:05:00.000Z")
	in.SourceAsOf = ts("2026-09-28T00:00:00.000Z")
	in.LastCheckAt, in.LastCheckOutcome = ts("2026-10-22T15:04:00.000Z"), "source_not_updated"
	h := HealthOf(in, now)
	wantHealth(t, h, HealthSourceStale, "Source data hasn't advanced since Sep 28; refreshes are still running (last Oct 22).", "2026-09-28T00:00:00.000Z")
	if !h.NeedsAttention() {
		t.Fatal("source_stale needs attention")
	}
	// Daily: three days of slack (one between fires plus two).
	in.SourceAsOf = now.Add(-72 * time.Hour)
	wantHealth(t, HealthOf(in, now), HealthOK, "", "null")
	in.SourceAsOf = now.Add(-72*time.Hour - time.Minute)
	wantHealth(t, HealthOf(in, now), HealthSourceStale, "", iso(t, jsx.NewTime(in.SourceAsOf)))
	// Weekdays: five (the weekend gap is three). Read on a Tuesday, data
	// through last Thursday is fine; through last Wednesday is not.
	in.Schedule = "0 15 * * 1-5"
	tuesday := ts("2026-10-27T18:30:00.000Z")
	in.LastCheckAt = ts("2026-10-27T15:04:00.000Z")
	in.SourceAsOf = ts("2026-10-22T19:00:00.000Z")
	wantHealth(t, HealthOf(in, tuesday), HealthOK, "", "null")
	in.SourceAsOf = ts("2026-10-22T18:00:00.000Z")
	wantHealth(t, HealthOf(in, tuesday), HealthSourceStale, "", "2026-10-22T18:00:00.000Z")
	// A page with no managed data has no source to be stale about.
	in.SourceAsOf = time.Time{}
	wantHealth(t, HealthOf(in, tuesday), HealthOK, "", "null")
}

func TestHealthNeverRefreshed(t *testing.T) {
	in := HealthInput{Schedule: "30 14 * * *", CreatedAt: ts("2026-10-20T09:00:00.000Z")}
	h := HealthOf(in, ts("2026-10-22T18:00:00.000Z"))
	wantHealth(t, h, HealthOverdue, "Expected a refresh at Oct 22 14:30 UTC; no refresh or check has been recorded yet.", "2026-10-20T14:30:00.000Z")
	if !h.LastActivityAt.IsZero() || h.RefreshOwner != nil {
		t.Fatalf("%+v", h)
	}
	// Created after the last fire whose grace has passed: nothing is due yet.
	in.CreatedAt = ts("2026-10-22T12:00:00.000Z")
	wantHealth(t, HealthOf(in, ts("2026-10-22T16:00:00.000Z")), HealthOK, "Scheduled; the first refresh is due Oct 23 14:30 UTC.", "null")
	// No creation stamp at all is never a reason to accuse a page.
	in.CreatedAt = time.Time{}
	wantHealth(t, HealthOf(in, ts("2026-10-22T18:00:00.000Z")), HealthOK, "", "null")
}

func TestHealthIsUTC(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	in := daily()
	in.RefreshedAt = ts("2026-10-31T15:12:00.000Z")
	// 2026-11-01 is the US DST change. 13:59 EST is 18:59 UTC: past the
	// 15:00 UTC fire's grace, whatever the local clock did that night.
	local := time.Date(2026, 11, 1, 13, 59, 0, 0, ny)
	h := HealthOf(in, local)
	wantHealth(t, h, HealthOverdue, "Expected a refresh at Nov 1 15:00 UTC; last activity Oct 31.", "2026-11-01T15:00:00.000Z")
	if iso(t, h.LastDueAt) != "2026-11-01T15:00:00.000Z" {
		t.Fatal(iso(t, h.LastDueAt))
	}
	// Reasons name the year only outside the current one.
	in.RefreshedAt = ts("2025-12-30T15:12:00.000Z")
	wantHealth(t, HealthOf(in, ts("2026-01-02T18:30:00.000Z")), HealthOverdue,
		"Expected a refresh at Jan 2 15:00 UTC; last activity Dec 30, 2025.", "2025-12-31T15:00:00.000Z")
}

func TestNormalizeRefreshScheduleAndOwner(t *testing.T) {
	if s, err := NormalizeRefreshSchedule("  0  15 * * 1-5 "); err != nil || s != "0 15 * * 1-5" {
		t.Fatal(s, err)
	}
	if s, err := NormalizeRefreshSchedule("   "); err != nil || s != "" {
		t.Fatal("blank clears", s, err)
	}
	_, err := NormalizeRefreshSchedule("0 15 * * MON")
	var e *apierror.Error
	if !errors.As(err, &e) || e.Code != "refresh_schedule_invalid" || e.Status != 400 || !strings.Contains(e.Message, "MON") {
		t.Fatalf("%v", err)
	}
	if o, err := NormalizeRefreshOwner("  Trader@Example.COM "); err != nil || o != "trader@example.com" {
		t.Fatal(o, err)
	}
	for _, bad := range []string{"trader", "@example.com", "a@b@c", "trader@", "tr ader@example.com"} {
		if _, err := NormalizeRefreshOwner(bad); !errors.As(err, &e) || e.Code != "refresh_owner_invalid" {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, set, err := scheduleArg(float64(5), NormalizeRefreshSchedule, "refresh_schedule"); err == nil || set {
		t.Fatal("a number is not a schedule")
	}
	if v, set, err := scheduleArg(Null, NormalizeRefreshSchedule, "refresh_schedule"); err != nil || !set || v != "" {
		t.Fatal("null clears")
	}
	if _, set, err := scheduleArg(nil, NormalizeRefreshSchedule, "refresh_schedule"); err != nil || set {
		t.Fatal("absent leaves it alone")
	}
}
