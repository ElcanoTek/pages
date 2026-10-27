// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/pagedata"
)

// gapFinding is a synthetic finding as pagedata reports one.
func gapFinding(stopped string, entities ...string) pagedata.CoverageGap {
	g := pagedata.CoverageGap{
		Path: "rows", DateField: "date", KeyField: "dealId",
		Fields: []string{"revenue", "clicks"}, StoppedOn: stopped, RowsThrough: "2026-10-21", FieldsThrough: "2026-10-21",
		EntityCount: len(entities), Entities: entities,
	}
	g.Message = "In rows, revenue and clicks end on " + stopped + " for " + strings.Join(entities, ", ") + ", whose rows continue to 2026-10-21."
	return g
}

func TestHealthCoverageGap(t *testing.T) {
	now := ts("2026-10-22T18:30:00.000Z")
	in := daily()
	in.CoverageGaps = []pagedata.CoverageGap{gapFinding("2026-09-30", "D-1", "D-2")}
	h := HealthOf(in, now)
	wantHealth(t, h, HealthCoverageGap, in.CoverageGaps[0].Message, "2026-10-01T00:00:00.000Z")
	if !h.NeedsAttention() || len(h.CoverageGaps) != 1 {
		t.Fatalf("coverage_gap needs attention and carries its findings: %+v", h)
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"health":"coverage_gap",`) ||
		!strings.HasSuffix(string(b), `"days_since_source":1,"coverage_gaps":[{"path":"rows","date_field":"date","key_field":"dealId","fields":["revenue","clicks"],`+
			`"fields_omitted":0,"stopped_on":"2026-09-30","rows_through":"2026-10-21","fields_through":"2026-10-21","entity_count":2,"entities":["D-1","D-2"],`+
			`"message":"In rows, revenue and clicks end on 2026-09-30 for D-1, D-2, whose rows continue to 2026-10-21."}]}`) {
		t.Fatalf("wire shape: %s", b)
	}

	// Several findings: the first is the reason, the rest are counted, and
	// the problem started the day after the earliest one.
	in.CoverageGaps = []pagedata.CoverageGap{gapFinding("2026-10-05", "D-3"), gapFinding("2026-09-12", "D-4"), gapFinding("2026-10-09", "D-5")}
	wantHealth(t, HealthOf(in, now), HealthCoverageGap, in.CoverageGaps[0].Message+" (+2 more gaps)", "2026-09-13T00:00:00.000Z")

	// A long reason is clipped like any other.
	long := gapFinding("2026-09-30", strings.Repeat("Fabrikam deal ", 40))
	in.CoverageGaps = []pagedata.CoverageGap{long}
	if r := HealthOf(in, now).HealthReason; len([]rune(r)) != maxReasonDetail || !strings.HasSuffix(r, "…") {
		t.Fatalf("clipped reason: %d %q", len([]rune(r)), r)
	}
}

// coverage_gap sits after source_stale and before ok, and like every other
// verdict it needs a schedule.
func TestHealthCoverageGapPrecedence(t *testing.T) {
	now := ts("2026-10-22T18:30:00.000Z")
	gaps := []pagedata.CoverageGap{gapFinding("2026-09-30", "D-1")}

	in := daily()
	in.CoverageGaps = gaps
	in.Schedule, in.IsLive = "", true
	if h := HealthOf(in, now); h.Health != HealthUnscheduled || h.NeedsAttention() || h.CoverageGaps != nil {
		t.Fatalf("unscheduled stays unjudged: %+v", h)
	}

	in = daily()
	in.CoverageGaps = gaps
	in.LastCheckAt, in.LastCheckOutcome, in.LastCheckDetail = ts("2026-10-22T16:00:00.000Z"), "failed", "the run errored"
	if h := HealthOf(in, now); h.Health != HealthBlocked || h.CoverageGaps != nil {
		t.Fatalf("blocked first: %+v", h)
	}

	in = daily()
	in.CoverageGaps = gaps
	in.RefreshedAt = ts("2026-10-20T15:05:00.000Z")
	if h := HealthOf(in, now); h.Health != HealthOverdue {
		t.Fatalf("overdue first: %+v", h)
	}

	in = daily()
	in.CoverageGaps = gaps
	in.SourceAsOf = ts("2026-10-01T00:00:00.000Z")
	if h := HealthOf(in, now); h.Health != HealthSourceStale {
		t.Fatalf("source_stale first: %+v", h)
	}

	in = daily()
	in.CoverageGaps = []pagedata.CoverageGap{}
	wantHealth(t, HealthOf(in, now), HealthOK, "Refreshing on schedule; last activity Oct 22 15:12 UTC.", "null")
}

func TestHealthCountsAndRankKnowCoverageGap(t *testing.T) {
	var c HealthCounts
	for _, h := range []string{HealthCoverageGap, HealthCoverageGap, HealthOK, HealthSourceStale} {
		c.add(&RefreshHealth{Health: h})
	}
	if c.CoverageGap != 2 || c.OK != 1 || c.SourceStale != 1 || c.Unscheduled != 0 {
		t.Fatalf("%+v", c)
	}
	b, _ := json.Marshal(c)
	if string(b) != `{"blocked":0,"overdue":0,"source_stale":1,"coverage_gap":2,"ok":1,"unscheduled":0,"unscheduled_stale":0}` {
		t.Fatalf("counts wire shape: %s", b)
	}
	if healthRank[HealthSourceStale] >= healthRank[HealthCoverageGap] || healthRank[HealthCoverageGap] >= healthRank[HealthOK] {
		t.Fatal("coverage_gap ranks after source_stale and before ok")
	}
	if len(healthRank) != len(Healths) {
		t.Fatalf("every health has a rank: %v", Healths)
	}
}

func TestStoredCoverageGaps(t *testing.T) {
	if _, ok := storedCoverageGaps(nil); ok {
		t.Fatal("NULL is not stored")
	}
	if _, ok := storedCoverageGaps([]byte(`{"v":0,"gaps":[]}`)); ok {
		t.Fatal("an older rule version is recomputed")
	}
	if _, ok := storedCoverageGaps([]byte(`not json`)); ok {
		t.Fatal("garbage is recomputed")
	}
	g, ok := storedCoverageGaps([]byte(coverageGapsJSON(pagedata.NoCoverageGaps())))
	if !ok || g.Gaps == nil || len(g.Gaps) != 0 {
		t.Fatalf("none round-trips as an empty list: %+v %v", g, ok)
	}
	want := &pagedata.CoverageGaps{Version: pagedata.CoverageGapsVersion, Gaps: []pagedata.CoverageGap{gapFinding("2026-09-30", "D-1")}}
	g, ok = storedCoverageGaps([]byte(coverageGapsJSON(want)))
	if !ok || len(g.Gaps) != 1 || g.Gaps[0].Message != want.Gaps[0].Message {
		t.Fatalf("round trip: %+v", g)
	}
}
