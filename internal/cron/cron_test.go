// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cron

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func TestParseAccepts(t *testing.T) {
	for expr, want := range map[string]string{
		"30 14 * * *":           "30 14 * * *",
		"  0   15 * *   1-5  ":  "0 15 * * 1-5",
		"0\t9\t*\t*\t1":         "0 9 * * 1",
		"*/15 * * * *":          "*/15 * * * *",
		"0 8-18/2 * * *":        "0 8-18/2 * * *",
		"0,30 6,18 1,15 * *":    "0,30 6,18 1,15 * *",
		"0 0 1 1-12/3 *":        "0 0 1 1-12/3 *",
		"5 4 * * 7":             "5 4 * * 7",
		"05 04 01 01 00":        "05 04 01 01 00",
		"0 0 29 2 *":            "0 0 29 2 *",
		"59 23 31 12 0-7":       "59 23 31 12 0-7",
		"0 12 * * 1,3,5":        "0 12 * * 1,3,5",
		"0 0 */2 * *":           "0 0 */2 * *",
		"0-59/59 0-23/23 * * *": "0-59/59 0-23/23 * * *",
		"0 15 1-7 * 1":          "0 15 1-7 * 1",
	} {
		if got := mustParse(t, expr).String(); got != want {
			t.Errorf("Parse(%q).String() = %q, want %q", expr, got, want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for expr, wantErr := range map[string]string{
		"":                       "exactly five fields",
		"* * * *":                "exactly five fields",
		"0 0 * * * *":            "exactly five fields",
		"@daily":                 "exactly five fields",
		"60 * * * *":             "outside 0-59",
		"* 24 * * *":             "outside 0-23",
		"* * 0 * *":              "outside 1-31",
		"* * 32 * *":             "outside 1-31",
		"* * * 0 *":              "outside 1-12",
		"* * * 13 *":             "outside 1-12",
		"* * * * 8":              "outside 0-7",
		"5-1 * * * *":            "outside 0-59",
		"* * * * MON":            "names such as MON",
		"* * * JAN *":            "names such as MON",
		"0 0 ? * *":              "not a number",
		"0 0 L * *":              "not a number",
		"0 0 * * 1#2":            "not a number",
		"0 0 15W * *":            "not a number",
		"-1 * * * *":             "not a number",
		"+1 * * * *":             "not a number",
		"1.5 * * * *":            "not a number",
		"001 * * * *":            "not a number",
		"1--2 * * * *":           "not a number",
		"1-2-3 * * * *":          "not a number",
		"5/15 * * * *":           "steps from a single number",
		"*/0 * * * *":            "invalid step",
		"*/60 * * * *":           "invalid step",
		"*/ * * * *":             "invalid step",
		"*/x * * * *":            "invalid step",
		"1,,2 * * * *":           "empty list item",
		", * * * *":              "empty list item",
		"0 0 30 2 *":             "never fires",
		"0 0 31 4,6,9,11 *":      "never fires",
		strings.Repeat("1,", 60): "at most 100 characters",
	} {
		_, err := Parse(expr)
		if err == nil {
			t.Errorf("Parse(%q) accepted it", expr)
			continue
		}
		if !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Parse(%q) = %q, want it to mention %q", expr, err, wantErr)
		}
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("Parse(%q) error is %T, not *Error", expr, err)
		}
	}
}

func TestPrevAndNext(t *testing.T) {
	cases := []struct {
		expr, now, prev, next string
	}{
		// Daily: the fire at exactly now counts as the previous one.
		{"30 14 * * *", "2026-10-22 14:30", "2026-10-22 14:30", "2026-10-23 14:30"},
		{"30 14 * * *", "2026-10-22 14:29", "2026-10-21 14:30", "2026-10-22 14:30"},
		{"30 14 * * *", "2026-01-01 00:00", "2025-12-31 14:30", "2026-01-01 14:30"},
		// Weekdays only, read over a weekend. 2026-10-24 is a Saturday.
		{"0 15 * * 1-5", "2026-10-24 12:00", "2026-10-23 15:00", "2026-10-26 15:00"},
		{"0 15 * * 1-5", "2026-10-26 14:59", "2026-10-23 15:00", "2026-10-26 15:00"},
		{"0 15 * * 1-5", "2026-10-26 15:00", "2026-10-26 15:00", "2026-10-27 15:00"},
		// Weekly on Monday; 0 and 7 are both Sunday.
		{"0 9 * * 1", "2026-10-22 09:00", "2026-10-19 09:00", "2026-10-26 09:00"},
		{"0 9 * * 7", "2026-10-22 09:00", "2026-10-18 09:00", "2026-10-25 09:00"},
		{"0 9 * * 0", "2026-10-25 09:00", "2026-10-25 09:00", "2026-11-01 09:00"},
		// Monthly, and a leap day.
		{"0 6 1 * *", "2026-10-22 00:00", "2026-10-01 06:00", "2026-11-01 06:00"},
		{"0 0 29 2 *", "2026-10-22 00:00", "2024-02-29 00:00", "2028-02-29 00:00"},
		// Steps and lists inside a day.
		{"*/15 * * * *", "2026-10-22 10:07", "2026-10-22 10:00", "2026-10-22 10:15"},
		{"0,30 6,18 * * *", "2026-10-22 12:00", "2026-10-22 06:30", "2026-10-22 18:00"},
		// Both day fields restricted: either one matches (Vixie). 2026-10-26 is
		// a Monday; the 1st and 15th match too.
		{"0 12 1,15 * 1", "2026-10-20 00:00", "2026-10-19 12:00", "2026-10-26 12:00"},
		{"0 12 1,15 * 1", "2026-10-16 00:00", "2026-10-15 12:00", "2026-10-19 12:00"},
		// Restricted day-of-month with a starred day-of-week: both must match.
		{"0 12 */2 * 1", "2026-10-22 00:00", "2026-10-19 12:00", "2026-11-09 12:00"},
	}
	for _, c := range cases {
		s := mustParse(t, c.expr)
		now := at(c.now)
		if got := s.Prev(now); !got.Equal(at(c.prev)) {
			t.Errorf("%q Prev(%s) = %s, want %s", c.expr, c.now, got.Format(time.RFC3339), c.prev)
		}
		if got := s.Next(now); !got.Equal(at(c.next)) {
			t.Errorf("%q Next(%s) = %s, want %s", c.expr, c.now, got.Format(time.RFC3339), c.next)
		}
		if !s.Matches(at(c.prev)) || !s.Matches(at(c.next)) {
			t.Errorf("%q does not match its own fires", c.expr)
		}
	}
}

// UTC means UTC: a time in another zone is the same instant, and nothing
// moves when the clocks change somewhere.
func TestTimesAreUTC(t *testing.T) {
	s := mustParse(t, "30 14 * * *")
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// 2026-11-01 is the US DST change; 10:30 EST is 15:30 UTC.
	local := time.Date(2026, 11, 1, 10, 30, 0, 0, ny)
	if got, want := s.Prev(local), at("2026-11-01 14:30"); !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("Prev = %v, want %v in UTC", got, want)
	}
	if !s.Matches(time.Date(2026, 3, 8, 10, 30, 0, 0, ny)) {
		t.Fatal("10:30 EDT on the spring-forward day is 14:30 UTC")
	}
	// Seconds inside the fire minute are still that minute.
	if got := s.Prev(at("2026-10-22 14:30").Add(59 * time.Second)); !got.Equal(at("2026-10-22 14:30")) {
		t.Fatalf("Prev inside the fire minute = %v", got)
	}
}

func TestMaxGap(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		expr, end string
		want      time.Duration
	}{
		{"30 14 * * *", "2026-10-22 18:00", day},
		// Every weekday: the Friday-to-Monday gap, wherever in the week it is
		// read from — including over the weekend itself.
		{"0 15 * * 1-5", "2026-10-21 18:00", 3 * day},
		{"0 15 * * 1-5", "2026-10-24 12:00", 3 * day},
		{"0 15 * * 1-5", "2026-10-26 16:00", 3 * day},
		{"0 9 * * 1", "2026-10-22 09:00", 7 * day},
		{"0 9 * * 1,4", "2026-10-22 10:00", 4 * day},
		{"*/15 * * * *", "2026-10-22 10:07", 15 * time.Minute},
		{"0 6,18 * * *", "2026-10-22 12:00", 12 * time.Hour},
		// Less than weekly: the gap before the latest fire.
		{"0 6 1 * *", "2026-10-22 00:00", 30 * day},
		{"0 6 1 * *", "2026-11-02 00:00", 31 * day},
	}
	for _, c := range cases {
		if got := mustParse(t, c.expr).MaxGap(at(c.end)); got != c.want {
			t.Errorf("%q MaxGap(%s) = %s, want %s", c.expr, c.end, got, c.want)
		}
	}
	// A leap day measures the four years back to the previous one.
	if got := mustParse(t, "0 0 29 2 *").MaxGap(at("2028-03-01 00:00")); got != (366+365+365+365)*day {
		t.Errorf("leap-day MaxGap = %s", got)
	}
}

// Every minute of every week is the densest schedule there is; it must still
// answer quickly (the welcome index computes this per page per request).
func TestDenseScheduleIsCheap(t *testing.T) {
	s := mustParse(t, "* * * * *")
	start := time.Now()
	if got := s.MaxGap(at("2026-10-22 10:00")); got != time.Minute {
		t.Fatalf("MaxGap = %s", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("MaxGap of an every-minute schedule took %s", elapsed)
	}
}
