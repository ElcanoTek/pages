// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package cron parses and evaluates the five-field cron expressions a page's
// refresh schedule is written in (pages.refresh_schedule).
//
// Pages does not RUN anything on a schedule — the refreshes are scheduled
// somewhere else. It only needs to answer two questions about a schedule:
// when did it last fire, and how long can it go between fires. That is a
// matcher, not a scheduler, so it is a page of owned code rather than a
// dependency.
//
// The dialect is deliberately small and strict, so a schedule that parses
// here means the same thing in any standard cron:
//
//	minute hour day-of-month month day-of-week
//
// Each field is `*` or a comma-separated list of numbers, ranges (`a-b`) and
// steps (`*/n`, `a-b/n`). Day-of-week is 0-7, with both 0 and 7 meaning
// Sunday. Names (MON, JAN), macros (@daily), `?`, `L`, `W`, `#`, a step on a
// single number (`5/15`) and a sixth seconds field are all refused, because
// cron implementations disagree about them and a schedule two systems read
// differently is worse than one that does not parse.
//
// Day-of-month and day-of-week combine the way Vixie cron combines them:
// when both are restricted (neither field starts with `*`), a day matches if
// EITHER matches; otherwise both must.
//
// Every time is UTC. A schedule never observes daylight saving.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxLength bounds an expression. A real one is a dozen characters; this
// only keeps a pathological list from being stored.
const MaxLength = 100

// searchDays bounds every search. A schedule that fires at all fires within
// any eight-year window (the 29th of February comes round inside it), so a
// search that has to look further means the schedule never fires.
const searchDays = 8*366 + 1

// Schedule is a parsed expression.
type Schedule struct {
	source                     string
	minute, hour, dom, month   uint64 // bit n set ⇔ n matches
	dow                        uint64
	domStar, dowStar           bool
	hasMinute                  [60]bool
	hasHour                    [24]bool
	minutesAsc, minutesDescend []int
}

type field struct {
	name     string
	min, max int
}

var fields = [5]field{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day-of-month", 1, 31},
	{"month", 1, 12},
	{"day-of-week", 0, 7},
}

// Error is a parse failure, worded for the person who typed the schedule.
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func fail(format string, args ...any) error { return &Error{Message: fmt.Sprintf(format, args...)} }

// Parse reads a five-field expression. Surrounding whitespace is ignored and
// fields may be separated by any run of spaces or tabs; String returns the
// fields joined by single spaces.
func Parse(expr string) (*Schedule, error) {
	if len(expr) > MaxLength {
		return nil, fail("a cron expression is at most %d characters", MaxLength)
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return nil, fail("a cron expression has exactly five fields (minute hour day-of-month month day-of-week), e.g. \"30 14 * * *\"; got %d", len(parts))
	}
	s := &Schedule{source: strings.Join(parts, " ")}
	sets := [5]*uint64{&s.minute, &s.hour, &s.dom, &s.month, &s.dow}
	for i, part := range parts {
		bits, err := parseField(part, fields[i])
		if err != nil {
			return nil, err
		}
		*sets[i] = bits
	}
	// 7 is Sunday too.
	if s.dow&(1<<7) != 0 {
		s.dow = (s.dow | 1) &^ (1 << 7)
	}
	s.domStar = strings.HasPrefix(parts[2], "*")
	s.dowStar = strings.HasPrefix(parts[4], "*")
	for m := 0; m < 60; m++ {
		if s.minute&(1<<m) != 0 {
			s.hasMinute[m] = true
			s.minutesAsc = append(s.minutesAsc, m)
		}
	}
	for i := len(s.minutesAsc) - 1; i >= 0; i-- {
		s.minutesDescend = append(s.minutesDescend, s.minutesAsc[i])
	}
	for h := 0; h < 24; h++ {
		s.hasHour[h] = s.hour&(1<<h) != 0
	}
	// "0 0 30 2 *" parses and never fires. Refuse it here rather than store a
	// schedule that can never be due.
	if s.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).IsZero() {
		return nil, fail("%q never fires: no month it names has that day", s.source)
	}
	return s, nil
}

func parseField(text string, f field) (uint64, error) {
	var bits uint64
	for _, item := range strings.Split(text, ",") {
		if item == "" {
			return 0, fail("the %s field %q has an empty list item", f.name, text)
		}
		rangePart, step := item, 1
		if slash := strings.IndexByte(item, '/'); slash >= 0 {
			rangePart = item[:slash]
			n, ok := number(item[slash+1:])
			if !ok || n < 1 || n > f.max {
				return 0, fail("the %s field %q has an invalid step (use a number from 1 to %d)", f.name, item, f.max)
			}
			step = n
			if rangePart != "*" && !strings.Contains(rangePart, "-") {
				return 0, fail("the %s field %q steps from a single number; write a range (%s-%d/%d) or */%d", f.name, item, rangePart, f.max, n, n)
			}
		}
		lo, hi := f.min, f.max
		if rangePart != "*" {
			a, b, isRange := strings.Cut(rangePart, "-")
			var okA, okB bool
			lo, okA = number(a)
			hi, okB = lo, okA
			if isRange {
				hi, okB = number(b)
			}
			if !okA || !okB {
				return 0, fail("the %s field %q is not a number, range, list or step (names such as MON or JAN are not accepted)", f.name, item)
			}
			if lo < f.min || hi > f.max || lo > hi {
				return 0, fail("the %s field %q is outside %d-%d", f.name, item, f.min, f.max)
			}
		}
		for v := lo; v <= hi; v += step {
			bits |= 1 << v
		}
	}
	return bits, nil
}

// number is a plain decimal of one or two digits: no sign, no spaces.
func number(s string) (int, bool) {
	if s == "" || len(s) > 2 {
		return 0, false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// String is the expression as parsed, its fields joined by single spaces.
func (s *Schedule) String() string { return s.source }

func (s *Schedule) dayMatches(d time.Time) bool {
	if s.month&(1<<int(d.Month())) == 0 {
		return false
	}
	dom := s.dom&(1<<d.Day()) != 0
	dow := s.dow&(1<<int(d.Weekday())) != 0
	if !s.domStar && !s.dowStar {
		return dom || dow
	}
	return dom && dow
}

// Matches reports whether the schedule fires in t's minute (UTC).
func (s *Schedule) Matches(t time.Time) bool {
	t = t.UTC()
	return s.dayMatches(t) && s.hasHour[t.Hour()] && s.hasMinute[t.Minute()]
}

func midnight(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// Prev is the latest fire at or before t (to the minute), or the zero time
// when the schedule has not fired in the eight years before t.
func (s *Schedule) Prev(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute)
	day := midnight(t)
	limit := t.Hour()*60 + t.Minute()
	for i := 0; i < searchDays; i++ {
		if s.dayMatches(day) {
			for h := limit / 60; h >= 0; h-- {
				if !s.hasHour[h] {
					continue
				}
				for _, m := range s.minutesDescend {
					if h*60+m <= limit {
						return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
					}
				}
			}
		}
		day = day.AddDate(0, 0, -1)
		limit = 23*60 + 59
	}
	return time.Time{}
}

// Next is the first fire strictly after t, or the zero time when the
// schedule does not fire in the eight years after t.
func (s *Schedule) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	day := midnight(t)
	floor := t.Hour()*60 + t.Minute()
	for i := 0; i < searchDays; i++ {
		if s.dayMatches(day) {
			for h := floor / 60; h < 24; h++ {
				if !s.hasHour[h] {
					continue
				}
				for _, m := range s.minutesAsc {
					if h*60+m >= floor {
						return day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
					}
				}
			}
		}
		day = day.AddDate(0, 0, 1)
		floor = 0
	}
	return time.Time{}
}

// Week is the window MaxGap measures over: one full cycle of every schedule
// that repeats weekly or faster.
const Week = 7 * 24 * time.Hour

// MaxGap is the longest interval between two consecutive fires among the
// fires in the week ending at end — so a weekday schedule reports its
// Friday-to-Monday gap and a weekly one reports a week. A schedule that fires
// less than once a week reports the gap before its latest fire, so a monthly
// schedule measures its month. Zero when the schedule has not fired twice.
func (s *Schedule) MaxGap(end time.Time) time.Duration {
	from := end.UTC().Add(-Week)
	later := s.Prev(end)
	var widest time.Duration
	// An every-minute schedule is 10,080 fires a week; anything denser than
	// that is impossible, so this bound only guards against a bug.
	for i := 0; i <= 7*24*60 && !later.IsZero(); i++ {
		earlier := s.Prev(later.Add(-time.Minute))
		if earlier.IsZero() {
			break
		}
		if gap := later.Sub(earlier); gap > widest {
			widest = gap
		}
		if !earlier.After(from) {
			break
		}
		later = earlier
	}
	return widest
}
