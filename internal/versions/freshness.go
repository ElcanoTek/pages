// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
)

// ── freshness ──────────────────────────────────────────────────────────────
//
// A dashboard frozen for six weeks looks exactly like one refreshed this
// morning. Pages has known otherwise the whole time — page_versions carries
// source_as_of and refreshed_at — it just never said so as a fact a caller
// could read. The one staleness signal that did exist (stale-refresh
// warnings) fires only DURING a write, so it is structurally blind to the
// failure that matters: a refresh that never ran, or that correctly declined
// and therefore never called Pages at all.
//
// Deliberately NOT an "is this overdue?" verdict. Pages retired its scheduler
// on purpose and does not know how often any given page should refresh;
// inferring overdue is the consumer's job. These are the facts that let a
// consumer decide it in one query instead of by noticing.

const dayMs = 86400000

// Freshness is the computed freshness block (key order as JavaScript).
type Freshness struct {
	LastCheckAt         jsx.Time `json:"last_check_at"`
	LatestOutcome       *string  `json:"latest_outcome"`
	LatestDetail        *string  `json:"latest_detail"`
	LatestSourceAsOf    jsx.Time `json:"latest_source_as_of"`
	SourceAsOf          jsx.Time `json:"source_as_of"`
	RefreshedAt         jsx.Time `json:"refreshed_at"`
	CheckedAt           jsx.Time `json:"checked_at"`
	LastCheckOutcome    *string  `json:"last_check_outcome"`
	LastCheckDetail     *string  `json:"last_check_detail"`
	LastCheckSourceAsOf jsx.Time `json:"last_check_source_as_of"`
	DaysSinceSource     *int64   `json:"days_since_source"`
	DaysSinceRefresh    *int64   `json:"days_since_refresh"`
	DaysSinceCheck      *int64   `json:"days_since_check"`
}

// FreshnessInput is what FreshnessOf reads: the published version's coverage
// stamps plus the last recorded check. Zero times and empty strings are
// absent. Use ParseTime for string stamps (an unparseable one is absent).
type FreshnessInput struct {
	SourceAsOf          time.Time
	RefreshedAt         time.Time
	LastCheckAt         time.Time
	LastCheckOutcome    string
	LastCheckDetail     string
	LastCheckSourceAsOf time.Time
}

func msTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.UnixMilli(t.UnixMilli()).UTC()
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func daysSince(t time.Time, now time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	// Clamped at zero: a source_as_of a few seconds into the future (clock skew
	// within the write path's tolerance) is not "-1 days old".
	d := int64(math.Floor(float64(now.UnixMilli()-t.UnixMilli()) / dayMs))
	if d < 0 {
		d = 0
	}
	return &d
}

// FreshnessOf builds the freshness block. It returns nil only when the page
// has neither coverage stamps nor a recorded check — a page that has never
// been managed and never been checked has nothing to be stale about. A zero
// now means time.Now().
func FreshnessOf(in FreshnessInput, now time.Time) *Freshness {
	if now.IsZero() {
		now = time.Now()
	}
	source, refreshed, lastCheck := msTime(in.SourceAsOf), msTime(in.RefreshedAt), msTime(in.LastCheckAt)
	if source.IsZero() && refreshed.IsZero() && lastCheck.IsZero() {
		return nil
	}
	// checked_at is the last time anyone LOOKED, whether or not it produced a
	// version. That separates "we check daily and the upstream is dead" from
	// "nobody has run this in three weeks".
	checked := lastCheck
	switch {
	case !lastCheck.IsZero() && !refreshed.IsZero():
		if !lastCheck.After(refreshed) {
			checked = refreshed
		}
	case lastCheck.IsZero():
		checked = refreshed
	}
	// Keep explicit check history, but never attach its failure to a later
	// successful live refresh. On equal timestamps the explicit check wins.
	refreshIsLatest := !refreshed.IsZero() && (lastCheck.IsZero() || refreshed.After(lastCheck))
	f := &Freshness{
		LastCheckAt:         jsx.NewTime(lastCheck),
		SourceAsOf:          jsx.NewTime(source),
		RefreshedAt:         jsx.NewTime(refreshed),
		CheckedAt:           jsx.NewTime(checked),
		LastCheckOutcome:    strPtr(in.LastCheckOutcome),
		LastCheckDetail:     strPtr(in.LastCheckDetail),
		LastCheckSourceAsOf: jsx.NewTime(msTime(in.LastCheckSourceAsOf)),
		DaysSinceSource:     daysSince(source, now),
		DaysSinceRefresh:    daysSince(refreshed, now),
		DaysSinceCheck:      daysSince(checked, now),
	}
	if refreshIsLatest {
		updated := "updated"
		f.LatestOutcome = &updated
		f.LatestSourceAsOf = jsx.NewTime(source)
	} else {
		f.LatestOutcome = strPtr(in.LastCheckOutcome)
		f.LatestDetail = strPtr(in.LastCheckDetail)
		f.LatestSourceAsOf = jsx.NewTime(msTime(in.LastCheckSourceAsOf))
	}
	return f
}

// ── JavaScript Date.parse for ISO strings ───────────────────────────────────

var isoDateRE = regexp.MustCompile(`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?(?:T(\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?(Z|[+-]\d{2}:\d{2})?)?$`)

// ParseTime is JavaScript's Date.parse for the ISO forms Pages stores and
// accepts, truncated to milliseconds; the zero time where JavaScript would
// return NaN. A date-time without an offset is taken as UTC (the server's
// zone).
func ParseTime(s string) time.Time {
	m := isoDateRE.FindStringSubmatch(s)
	if m == nil || m[1] == "-000000" {
		return time.Time{}
	}
	atoi := func(v string, def int) int {
		if v == "" {
			return def
		}
		n, _ := strconv.Atoi(v)
		return n
	}
	year := atoi(m[1], 0)
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
		return time.Time{}
	}
	if hour == 24 && (minute != 0 || sec != 0 || ms != 0) {
		return time.Time{}
	}
	offset := 0
	if tz := m[8]; tz != "" && tz != "Z" {
		th, _ := strconv.Atoi(tz[1:3])
		tm, _ := strconv.Atoi(tz[4:6])
		if th > 23 || tm > 59 {
			return time.Time{}
		}
		offset = th*60 + tm
		if tz[0] == '-' {
			offset = -offset
		}
	}
	t := time.Date(year, time.Month(month), day, hour, minute, sec, ms*int(time.Millisecond), time.UTC)
	t = t.Add(-time.Duration(offset) * time.Minute)
	if math.Abs(float64(t.UnixMilli())) > 8.64e15 {
		return time.Time{}
	}
	return t
}

// isoString is Date#toISOString for t (ms precision, UTC); "" for zero.
func isoString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return jsx.FormatTime(msTime(t))
}

// ── legacy coverage ─────────────────────────────────────────────────────────

// Legacy full-source versions predate indexed managed metadata. Read their
// validated envelope without rewriting immutable history. New writes always
// populate the index; unrelated plain HTML and malformed historical contracts
// contribute no invented data timestamps.
func legacyManagedMetadata(html *string) (*pagedata.Managed, error) {
	if html == nil || *html == "" {
		return nil, nil
	}
	m, err := pagedata.ParseManagedHTML(*html)
	if err != nil {
		if _, ok := apierror.As(err); ok {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

type coverage struct{ sourceAsOf, refreshedAt time.Time }

func indexedOrLegacyCoverage(sourceAsOf, refreshedAt jsx.Time, legacyHTML *string) (coverage, error) {
	c := coverage{sourceAsOf: sourceAsOf.Time, refreshedAt: refreshedAt.Time}
	if c.sourceAsOf.IsZero() || c.refreshedAt.IsZero() {
		m, err := legacyManagedMetadata(legacyHTML)
		if err != nil {
			return c, err
		}
		if m != nil {
			if c.sourceAsOf.IsZero() {
				c.sourceAsOf = ParseTime(m.SourceAsOf())
			}
			if c.refreshedAt.IsZero() {
				c.refreshedAt = ParseTime(m.RefreshedAt())
			}
		}
	}
	return c, nil
}

// SourceCoverageFloor is the latest source_as_of across every non-rejected
// version of the page (indexed, or read from a legacy full-source envelope),
// and currentSource (an ISO string, "" for none). Source coverage is
// monotonic across all of them — not just the live pointer — which also
// serializes publish:false canaries safely. Returns "" when there is none.
func SourceCoverageFloor(ctx context.Context, q db.Querier, pageID jsx.ID, currentSource string) (string, error) {
	rows, err := q.Query(ctx,
		`SELECT max(source_as_of) AS source_as_of, NULL::text AS legacy_managed_html
		   FROM page_versions WHERE page_id=$1 AND status <> 'rejected'
		 UNION ALL
		 SELECT NULL::timestamptz, html FROM page_versions
		  WHERE page_id=$1 AND status <> 'rejected' AND source_as_of IS NULL
		    AND data_sha256 IS NULL AND position('pages-data' IN html) > 0`, pageID)
	if err != nil {
		return "", err
	}
	type row struct {
		source jsx.Time
		html   *string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.source, &r.html); err != nil {
			rows.Close()
			return "", err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	var floor time.Time
	if currentSource != "" {
		floor = ParseTime(currentSource)
	}
	for _, r := range all {
		c, err := indexedOrLegacyCoverage(r.source, jsx.Time{}, r.html)
		if err != nil {
			return "", err
		}
		if v := msTime(c.sourceAsOf); !v.IsZero() && (floor.IsZero() || v.After(floor)) {
			floor = v
		}
	}
	return isoString(floor), nil
}
