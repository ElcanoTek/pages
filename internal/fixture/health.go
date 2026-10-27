// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Refresh health in the fixture is the server's own verdict, not a second
// copy of the rules: each row's block is versions.HealthOf over that row's
// freshness stamps at the fixture's NOW, and a schedule the detail screen
// saves is validated by the same functions the admin API calls. So a spec
// that sees "Overdue" is seeing what production would say about that row.

// seededSchedules gives five seeded rows a schedule, one per state the staff
// screens render. Everything else stays unscheduled, like a page nobody has
// set one on — including the detail page, whose frozen data must carry no
// verdict precisely because it has no schedule. The live ones among them whose
// data is more than a week old (the detail page, and most clients from 08 on,
// since a generated row's data is as many days old as its number) carry the
// unscheduled_stale hint instead, exactly as the server would mark them.
var seededSchedules = map[string][2]string{
	"client-01": {"0 9 * * *", "desk@northwind.example"},  // ok: refreshed after yesterday's fire
	"client-02": {"0 9 * * *", "analyst@contoso.example"}, // overdue: nothing since the day before
	"client-03": {"30 8 * * 1-5", "ops@fabrikam.example"}, // blocked: today's run recorded a failure
	"client-05": {"0 9 * * *", "desk@northwind.example"},  // source_stale: runs, but the data is five days old
	"client-06": {"0 9 * * *", "desk@northwind.example"},  // coverage_gap: on time, but part of its rows stopped
}

// seededCoverageGaps are what the server would find in a row's published
// data: pagedata.DetectCoverageGaps over a synthetic trader payload, not a
// hand-written finding. client-06's SSP fields stop on Jun 30 for four of its
// eight deals while their DSP fields carry on through yesterday.
var seededCoverageGaps = map[string][]pagedata.CoverageGap{
	"client-06": traderCoverageGaps(),
}

func traderCoverageGaps() []pagedata.CoverageGap {
	first := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	stopped := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)
	last := nowTime.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	var rows []any
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		for deal := 101; deal <= 108; deal++ {
			n := float64(day.Day() % 7)
			row := jsonv.ObjectOf("date", day.Format("2006-01-02"), "dealId", fmt.Sprintf("deal-%d", deal),
				"dspSpend", 40+n, "dspImpressions", 9000+100*n)
			if deal > 104 || !day.After(stopped) {
				row.Set("revenue", 52+n)
				row.Set("grossMargin", 11+n)
				row.Set("sspImpressions", 8800+100*n)
			}
			rows = append(rows, row)
		}
	}
	return pagedata.DetectCoverageGaps(jsonv.ObjectOf("rows", rows)).Gaps
}

func stamp(f any, key string) string {
	s, _ := get(f, key).(string)
	return s
}

// healthFor is the refresh_health block the server would compute for page
// with this schedule and owner, at NOW.
func healthFor(page obj, schedule, owner string) obj {
	live, _ := get(page, "is_live").(bool)
	in := versions.HealthInput{Schedule: schedule, Owner: owner, IsLive: live, CreatedAt: versions.ParseTime(stamp(page, "created_at")),
		CoverageGaps: seededCoverageGaps[str(get(page, "slug"))]}
	if f := asObj(get(page, "freshness")); f != nil {
		in.SourceAsOf = versions.ParseTime(stamp(f, "source_as_of"))
		in.RefreshedAt = versions.ParseTime(stamp(f, "refreshed_at"))
		in.LastCheckAt = versions.ParseTime(stamp(f, "last_check_at"))
		in.LastCheckOutcome = stamp(f, "last_check_outcome")
		in.LastCheckDetail = stamp(f, "last_check_detail")
	}
	b, err := json.Marshal(versions.HealthOf(in, nowTime))
	if err != nil {
		panic(err)
	}
	v, err := jsonv.Parse(b)
	if err != nil {
		panic(err)
	}
	return asObj(v)
}

// setLive sets a row's serving state and judges its refresh health again,
// since the block reads it: only a live page can be unscheduled_stale.
func setLive(page obj, live bool) {
	set(page, "is_live", live)
	schedule, owner := scheduleOf(page)
	set(page, "refresh_health", healthFor(page, schedule, owner))
}

// scheduleOf reads a row's current schedule and owner back off its block.
func scheduleOf(page obj) (schedule, owner string) {
	h := get(page, "refresh_health")
	schedule, _ = get(h, "refresh_schedule").(string)
	owner, _ = get(h, "refresh_owner").(string)
	return schedule, owner
}

// setRefreshSchedule is POST /pages/<slug>/refresh-schedule: absent keeps a
// field, null or blank clears it, anything else is validated as the server
// validates it.
func setRefreshSchedule(page obj, body any) (obj, *apierror.Error) {
	schedule, owner := scheduleOf(page)
	present := false
	for _, field := range []struct {
		key       string
		target    *string
		normalize func(string) (string, error)
	}{
		{"schedule", &schedule, versions.NormalizeRefreshSchedule},
		{"owner", &owner, versions.NormalizeRefreshOwner},
	} {
		v := get(body, field.key)
		if _, missing := v.(undefined); missing {
			continue
		}
		present = true
		if v == nil {
			*field.target = ""
			continue
		}
		s, ok := v.(string)
		if !ok {
			return nil, apierror.BadRequest("refresh_"+field.key+" must be a string or null", "refresh_"+field.key+"_invalid", nil)
		}
		clean, err := field.normalize(s)
		if err != nil {
			e, _ := apierror.As(err)
			return nil, e
		}
		*field.target = clean
	}
	if !present {
		return nil, apierror.BadRequest("pass schedule, owner or both (null clears one)", "refresh_schedule_required", nil)
	}
	before, beforeOwner := scheduleOf(page)
	set(page, "refresh_health", healthFor(page, schedule, owner))
	return o("slug", get(page, "slug"), "changed", before != schedule || beforeOwner != owner, "refresh_health", get(page, "refresh_health")), nil
}
