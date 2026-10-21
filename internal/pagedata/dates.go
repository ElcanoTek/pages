// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"
)

// JavaScript Date parsing and printing, for the ISO forms Pages uses. V8
// accepts day 1–31 for every month (overflowing into the next), hour 24 only
// as 24:00:00.000, any number of fraction digits (truncated to milliseconds),
// and offsets up to ±23:59. A date-time without an offset is local time in
// JavaScript; this treats it as UTC, which is what the server runs in.

var isoDateRe = regexp.MustCompile(`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?(?:T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?(Z|[+-]\d{2}:\d{2})?)?$`)

const maxDateMs = 8.64e15

// jsDateParse is Date.parse for ISO-format strings; ok is false where
// JavaScript returns NaN (or for non-ISO forms this does not implement).
func jsDateParse(s string) (float64, bool) {
	m := isoDateRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	year, _ := strconv.Atoi(m[1])
	if m[1] == "-000000" {
		return 0, false
	}
	atoi := func(v string, def int) int {
		if v == "" {
			return def
		}
		n, _ := strconv.Atoi(v)
		return n
	}
	month, day := atoi(m[2], 1), atoi(m[3], 1)
	hour, minute, sec := atoi(m[4], 0), atoi(m[5], 0), atoi(m[6], 0)
	ms := 0
	if frac := m[7]; frac != "" {
		for len(frac) < 3 {
			frac += "0"
		}
		ms, _ = strconv.Atoi(frac[:3])
	}
	if month < 1 || month > 12 || day < 1 || day > 31 || hour > 24 || minute > 59 || sec > 59 {
		return 0, false
	}
	if hour == 24 && (minute != 0 || sec != 0 || ms != 0) {
		return 0, false
	}
	offset := 0
	if tz := m[8]; tz != "" && tz != "Z" {
		th, _ := strconv.Atoi(tz[1:3])
		tm, _ := strconv.Atoi(tz[4:6])
		if th > 23 || tm > 59 {
			return 0, false
		}
		offset = th*60 + tm
		if tz[0] == '-' {
			offset = -offset
		}
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, ms*int(time.Millisecond), time.UTC)
	v := float64(t.UnixMilli()) - float64(offset)*60000
	if math.Abs(v) > maxDateMs {
		return 0, false
	}
	return v, true
}

// jsISOString is Date.prototype.toISOString for a millisecond time value:
// four-digit years in 0000–9999, signed six-digit years outside.
func jsISOString(ms float64) string {
	t := time.UnixMilli(int64(ms)).UTC()
	y := t.Year()
	var year string
	switch {
	case y >= 0 && y <= 9999:
		year = fmt.Sprintf("%04d", y)
	case y < 0:
		year = fmt.Sprintf("-%06d", -y)
	default:
		year = fmt.Sprintf("+%06d", y)
	}
	return fmt.Sprintf("%s-%02d-%02dT%02d:%02d:%02d.%03dZ", year, int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond()/int(time.Millisecond))
}

// timeMs is a time as a JavaScript millisecond time value.
func timeMs(t time.Time) float64 { return float64(t.UnixMilli()) }
