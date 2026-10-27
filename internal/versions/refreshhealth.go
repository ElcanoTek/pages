// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/auth/central"
	"github.com/ElcanoTek/pages/internal/cron"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
)

// ── refresh health ─────────────────────────────────────────────────────────
//
// Freshness (freshness.go) reports facts and refuses a verdict, because Pages
// did not know how often any page was meant to refresh. Traders paid for that
// neutrality: a dashboard whose daily refresh stopped read exactly like one
// refreshed this morning, so finding the stale ones meant opening all of them.
//
// A page can now carry the cadence its refresh was scheduled at
// (pages.refresh_schedule, a five-field UTC cron — internal/cron) and the
// person who answers for it (pages.refresh_owner). With that expectation,
// and only with it, Pages states a verdict. The refreshes themselves are still
// scheduled elsewhere; Pages runs nothing on this schedule.
//
// The rules, in precedence order (the first that holds wins):
//
//  1. unscheduled — no schedule: no judgement at all, exactly the neutral
//     behaviour every page had before. Clearing a page's schedule is also how
//     staff stop Pages judging a page (a finished campaign, say).
//  2. blocked — the latest thing that happened to the page is a recorded
//     check whose outcome is a failure (blocked, source_unreachable, failed):
//     freshness's latest_outcome, except that a refresh written to a version
//     that is not live yet (a pending one on an approval-gated page) also
//     counts as later. The reason is the check's own detail.
//  3. overdue — a scheduled fire passed more than RefreshGrace ago and
//     nothing happened since: no refresh on any version and no recorded check.
//     A run that lands up to RefreshEarly before its fire counts for it (a
//     task scheduled a little earlier than the page says, or a manual run just
//     ahead of it), capped at half the gap to the previous fire so it can
//     never count for two. A page that has never been refreshed is judged
//     from its creation: it is overdue once a fire after it has passed.
//  4. source_stale — refreshes are running, but the published data's
//     source_as_of is older than the schedule's longest gap between fires
//     (over a week; see cron.MaxGap) plus RefreshStaleSlack. Daily: 3 days.
//     Weekdays: 5 (the weekend gap is 3). Weekly: 9. A page created without
//     data stores the epoch as its source_as_of, which means "no source data
//     yet" (noSourceYet), not "data from 1970": it is judged from the page's
//     creation instead, with the same allowance.
//  5. coverage_gap — refreshes run and the data advances, but inside the
//     published payload some number fields stopped for some entities while
//     those entities' rows, and the same fields for other entities, carried
//     on (pagedata.DetectCoverageGaps: at least CoverageGapDays behind both).
//     That is a refresh reading one source for only part of what the page
//     shows: every run succeeds and source_as_of advances, because the other
//     source does. Pages reads it from the published data itself, not from
//     what the refresh reported. The reason is the first finding, and how
//     many more there are.
//  6. ok.
//
// health_since is when the problem started, where Pages knows it: the
// recorded failing check (blocked), the first fire nothing answered
// (overdue), the stale source_as_of or, with no source data yet, the page's
// creation (source_stale), the day after the earliest day a gap's fields were
// last present (coverage_gap).
//
// Beside the verdict, and never instead of it, sits one hint: unscheduled_stale.
// A page with no schedule is not judged, and that stays true — a finished
// campaign legitimately sits there for good. But "no schedule" also describes
// a page whose campaign is still delivering and whose refresh task was never
// installed, and that one looked exactly like the finished ones: a neutral
// "Data 11d old" line and nothing else. So a page that is live (published and
// not disabled), has no schedule, and whose published source_as_of is more
// than UnscheduledStaleAfter old carries unscheduled_stale:true, which the
// staff home shows as a grey "No refresh" chip with a "No refresh set" filter.
// Its health is still unscheduled and it never counts as needing attention.

// The health values.
const (
	HealthOK          = "ok"
	HealthOverdue     = "overdue"
	HealthBlocked     = "blocked"
	HealthSourceStale = "source_stale"
	HealthCoverageGap = "coverage_gap"
	HealthUnscheduled = "unscheduled"
)

// Healths lists every health value (the MCP enum).
var Healths = []string{HealthOK, HealthOverdue, HealthBlocked, HealthSourceStale, HealthCoverageGap, HealthUnscheduled}

// FailedCheckOutcomes are the recorded check outcomes that mean a run could
// not do its job. "updated" and "source_not_updated" are a run doing it.
var FailedCheckOutcomes = []string{"source_unreachable", "blocked", "failed"}

const (
	// RefreshGrace is how long after its fire a refresh has to land before
	// the page is overdue: a run takes minutes, a slow source or a queue
	// hours.
	RefreshGrace = 3 * time.Hour
	// RefreshEarly is how far ahead of its fire a run may land and still
	// count for it.
	RefreshEarly = time.Hour
	// RefreshStaleSlack is the source lag allowed on top of the schedule's
	// longest gap before source_stale: a source normally reports through
	// yesterday or the day before.
	RefreshStaleSlack = 48 * time.Hour
	// UnscheduledStaleAfter is how old a live, unscheduled page's source data
	// may get before Pages hints that nobody has set a refresh for it
	// (RefreshHealth.UnscheduledStale). A week: longer than any refresh
	// cadence in use leaves a page between runs, short enough to catch a
	// running campaign whose task was never installed within its first week.
	UnscheduledStaleAfter = 7 * 24 * time.Hour
	// maxReasonDetail clips a recorded check detail quoted as a reason.
	maxReasonDetail = 300
)

// RefreshHealth is the computed refresh-health block, sibling of Freshness.
type RefreshHealth struct {
	RefreshSchedule *string  `json:"refresh_schedule"`
	RefreshOwner    *string  `json:"refresh_owner"`
	Health          string   `json:"health"`
	HealthReason    string   `json:"health_reason"`
	HealthSince     jsx.Time `json:"health_since"`
	LastDueAt       jsx.Time `json:"last_due_at"`
	NextDueAt       jsx.Time `json:"next_due_at"`
	LastActivityAt  jsx.Time `json:"last_activity_at"`
	// UnscheduledStale is the hint beside the verdict (see the rules above):
	// live, no schedule, and source data more than UnscheduledStaleAfter old.
	// It never changes Health and never makes NeedsAttention true.
	UnscheduledStale bool `json:"unscheduled_stale"`
	// DaysSinceSource is freshness's days_since_source for the published
	// data: whole days since source_as_of, null without one.
	DaysSinceSource *int64 `json:"days_since_source"`
	// CoverageGaps are the published data's findings, present only when they
	// are the verdict (health coverage_gap).
	CoverageGaps []pagedata.CoverageGap `json:"coverage_gaps,omitempty"`
}

// UnscheduledStaleHint is what a staff surface says about an
// unscheduled_stale page, given its days_since_source. The staff home says
// the same words (public/shell-assets/refresh-health.js).
func UnscheduledStaleHint(daysSinceSource int64) string {
	unit := "days"
	if daysSinceSource == 1 {
		unit = "day"
	}
	return fmt.Sprintf("No refresh schedule · data %d %s old — set a schedule or ignore if the campaign has ended", daysSinceSource, unit)
}

// NeedsAttention reports whether a human should look: blocked, overdue,
// source_stale or coverage_gap.
func (h *RefreshHealth) NeedsAttention() bool {
	return h != nil && (h.Health == HealthBlocked || h.Health == HealthOverdue || h.Health == HealthSourceStale ||
		h.Health == HealthCoverageGap)
}

// HealthInput is what HealthOf reads. Zero times and empty strings are
// absent.
type HealthInput struct {
	Schedule string
	Owner    string
	// IsLive is whether the page is serving: published and not disabled. Only
	// a live page can be unscheduled_stale.
	IsLive bool
	// CreatedAt anchors a page that has never been refreshed.
	CreatedAt time.Time
	// SourceAsOf and RefreshedAt are the published version's coverage stamps.
	SourceAsOf  time.Time
	RefreshedAt time.Time
	// NewestRefreshAt is the refreshed_at of the newest version that carries
	// one, published or not.
	NewestRefreshAt  time.Time
	LastCheckAt      time.Time
	LastCheckOutcome string
	LastCheckDetail  string
	// CoverageGaps are the published version's coverage gaps
	// (page_versions.coverage_gaps).
	CoverageGaps []pagedata.CoverageGap
}

func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

func isFailedOutcome(o string) bool {
	for _, f := range FailedCheckOutcomes {
		if o == f {
			return true
		}
	}
	return false
}

// shortDay is "Oct 20", or "Oct 20, 2025" outside now's year.
func shortDay(t, now time.Time) string {
	t = t.UTC()
	if t.Year() != now.UTC().Year() {
		return t.Format("Jan 2, 2006")
	}
	return t.Format("Jan 2")
}

// shortWhen is "Oct 22 15:00 UTC" (with the year outside now's).
func shortWhen(t, now time.Time) string {
	t = t.UTC()
	if t.Year() != now.UTC().Year() {
		return t.Format("Jan 2, 2006 15:04 UTC")
	}
	return t.Format("Jan 2 15:04 UTC")
}

// earlyTolerance is how far ahead of fire activity may land and count for it.
func earlyTolerance(s *cron.Schedule, fire time.Time) time.Duration {
	tol := RefreshEarly
	if prev := s.Prev(fire.Add(-time.Minute)); !prev.IsZero() {
		if half := fire.Sub(prev) / 2; half < tol {
			tol = half
		}
	}
	return tol
}

// covers reports whether activity at a answers the fire.
func covers(s *cron.Schedule, a, fire time.Time) bool {
	return !a.IsZero() && !a.Before(fire.Add(-earlyTolerance(s, fire)))
}

// firstMissed is the first fire after activity a that a does not answer, no
// later than due (which a is known not to answer). The tolerance is at most
// half a gap, so a answers at most the one fire right after it.
func firstMissed(s *cron.Schedule, a, due time.Time) time.Time {
	f := s.Next(a)
	if !f.IsZero() && covers(s, a, f) {
		f = s.Next(f)
	}
	if f.IsZero() || f.After(due) {
		return due
	}
	return f
}

func outcomeWords(o string) string { return strings.ReplaceAll(o, "_", " ") }

// HealthOf computes the refresh-health block (rules above). A zero now means
// time.Now(). It never returns nil.
func HealthOf(in HealthInput, now time.Time) *RefreshHealth {
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	h := &RefreshHealth{RefreshSchedule: strPtr(in.Schedule), RefreshOwner: strPtr(in.Owner)}
	lastRefresh := latest(msTime(in.RefreshedAt), msTime(in.NewestRefreshAt))
	lastCheck := msTime(in.LastCheckAt)
	activity := latest(lastRefresh, lastCheck)
	h.LastActivityAt = jsx.NewTime(activity)
	// The epoch is "no source data yet": no age, no hint, and staleness
	// counted from the page's creation (rule 4).
	waiting := noSourceYet(in.SourceAsOf)
	source := msTime(sourceStamp(in.SourceAsOf))
	h.DaysSinceSource = daysSince(source, now)

	if in.Schedule == "" {
		h.Health = HealthUnscheduled
		h.HealthReason = "No refresh schedule is set, so Pages does not judge this page."
		h.UnscheduledStale = in.IsLive && !source.IsZero() && now.Sub(source) > UnscheduledStaleAfter
		return h
	}
	s, err := cron.Parse(in.Schedule)
	if err != nil {
		// Only a row written around the validation could hold one; say so
		// rather than judge the page against a schedule nobody can read.
		h.Health = HealthUnscheduled
		h.HealthReason = "The stored refresh schedule is not a valid cron expression, so Pages does not judge this page; set it again."
		return h
	}
	h.LastDueAt = jsx.NewTime(s.Prev(now))
	h.NextDueAt = jsx.NewTime(s.Next(now))

	// Blocked: the latest outcome, as freshness defines it (on equal
	// timestamps the explicit check wins), is a failed check.
	if !lastCheck.IsZero() && !lastRefresh.After(lastCheck) && isFailedOutcome(in.LastCheckOutcome) {
		h.Health = HealthBlocked
		h.HealthSince = jsx.NewTime(lastCheck)
		if detail := jsTrim(in.LastCheckDetail); detail != "" {
			h.HealthReason = clipReason(detail)
		} else {
			h.HealthReason = fmt.Sprintf("The latest refresh run recorded %s on %s, with no detail.", outcomeWords(in.LastCheckOutcome), shortDay(lastCheck, now))
		}
		return h
	}

	// Overdue: the latest fire whose grace has passed, unanswered.
	if due := s.Prev(now.Add(-RefreshGrace)); !due.IsZero() {
		switch {
		case !activity.IsZero() && !covers(s, activity, due):
			h.Health = HealthOverdue
			h.HealthSince = jsx.NewTime(firstMissed(s, activity, due))
			h.HealthReason = fmt.Sprintf("Expected a refresh at %s; last activity %s.", shortWhen(due, now), shortDay(activity, now))
			return h
		case activity.IsZero() && !in.CreatedAt.IsZero() && in.CreatedAt.Before(due):
			h.Health = HealthOverdue
			since := s.Next(in.CreatedAt)
			if since.IsZero() || since.After(due) {
				since = due
			}
			h.HealthSince = jsx.NewTime(since)
			h.HealthReason = fmt.Sprintf("Expected a refresh at %s; no refresh or check has been recorded yet.", shortWhen(due, now))
			return h
		}
	}

	// Source stale: runs are happening, the data is not moving — or, for a
	// page created without data, has never arrived.
	since, words := source, "Source data hasn't advanced since %s"
	if waiting {
		since, words = msTime(in.CreatedAt), "No source data yet since %s"
	}
	if gap := s.MaxGap(now); !since.IsZero() && gap > 0 && now.Sub(since) > gap+RefreshStaleSlack {
		h.Health = HealthSourceStale
		h.HealthSince = jsx.NewTime(since)
		h.HealthReason = fmt.Sprintf(words+".", shortDay(since, now))
		if !activity.IsZero() {
			h.HealthReason = fmt.Sprintf(words+"; refreshes are still running (last %s).", shortDay(since, now), shortDay(activity, now))
		}
		return h
	}

	// Coverage gap: the data advances, but part of it stopped.
	if len(in.CoverageGaps) > 0 {
		h.Health = HealthCoverageGap
		h.HealthReason = clipReason(pagedata.CoverageGapsSummary(in.CoverageGaps))
		h.HealthSince = jsx.NewTime(coverageGapSince(in.CoverageGaps))
		h.CoverageGaps = in.CoverageGaps
		return h
	}

	h.Health = HealthOK
	if activity.IsZero() {
		h.HealthReason = fmt.Sprintf("Scheduled; the first refresh is due %s.", shortWhen(s.Next(now), now))
	} else {
		h.HealthReason = fmt.Sprintf("Refreshing on schedule; last activity %s.", shortWhen(activity, now))
	}
	return h
}

// coverageGapSince is the day after the earliest day a finding's fields were
// last present: the first day the page showed less than it should.
func coverageGapSince(gaps []pagedata.CoverageGap) time.Time {
	var since time.Time
	for _, g := range gaps {
		day, err := time.Parse("2006-01-02", g.StoppedOn)
		if err != nil {
			continue
		}
		if day = day.AddDate(0, 0, 1); since.IsZero() || day.Before(since) {
			since = day
		}
	}
	return since
}

func clipReason(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if jsstr.UTF16Len(s) > maxReasonDetail {
		return jsstr.Slice(s, 0, maxReasonDetail-1) + "…"
	}
	return s
}

// ── reading it ─────────────────────────────────────────────────────────────

// newestRefreshSQL is the refreshed_at of the page's newest version that
// carries one. A refresh on an approval-gated page lands as a pending
// version, and the run that wrote it did its job.
const newestRefreshSQL = `(SELECT nv.refreshed_at FROM page_versions nv
             WHERE nv.page_id = p.id AND nv.refreshed_at IS NOT NULL
             ORDER BY nv.created_at DESC, nv.id DESC LIMIT 1)`

// healthInput assembles HealthInput from a row's columns.
func healthInput(schedule, owner *string, isLive bool, created jsx.Time, c coverage, newest jsx.Time, lc LastCheck, gaps []pagedata.CoverageGap) HealthInput {
	return HealthInput{
		Schedule: deref(schedule), Owner: deref(owner), IsLive: isLive, CreatedAt: created.Time,
		SourceAsOf: c.sourceAsOf, RefreshedAt: c.refreshedAt, NewestRefreshAt: newest.Time,
		LastCheckAt: lc.LastCheckAt.Time, LastCheckOutcome: deref(lc.LastCheckOutcome), LastCheckDetail: deref(lc.LastCheckDetail),
		CoverageGaps: gaps,
	}
}

// PageRefreshHealth is one page's freshness and refresh health.
type PageRefreshHealth struct {
	Slug          string
	Freshness     *Freshness
	RefreshHealth *RefreshHealth
}

// RefreshHealthFor reads one page's refresh health (and the freshness it is
// derived from). 404 page_not_found when absent or soft-deleted. A zero now
// means time.Now().
func RefreshHealthFor(ctx context.Context, q db.Querier, slug string, now time.Time) (*PageRefreshHealth, error) {
	s, err := NormalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	var (
		out               PageRefreshHealth
		schedule, owner   *string
		isLive            bool
		created           jsx.Time
		source, refreshed jsx.Time
		legacy            *string
		newest            jsx.Time
		lc                LastCheck
		gaps              storedGaps
	)
	err = q.QueryRow(ctx,
		`SELECT p.slug, p.refresh_schedule, p.refresh_owner,
		        (p.published_version_id IS NOT NULL AND NOT p.disabled) AS is_live, p.created_at,
		        COALESCE(pv.id, 0), pv.coverage_gaps,
		        pv.source_as_of, pv.refreshed_at,
		        CASE WHEN pv.data_sha256 IS NULL AND position('pages-data' IN pv.html) > 0
		             THEN pv.html END AS legacy_managed_html,
		        `+newestRefreshSQL+` AS newest_refreshed_at,
		        p.last_check_at, p.last_check_outcome, p.last_check_detail, p.last_check_source_as_of
		   FROM pages p LEFT JOIN page_versions pv ON pv.id = p.published_version_id
		  WHERE p.slug = $1 AND p.deleted_at IS NULL`, s).Scan(
		&out.Slug, &schedule, &owner, &isLive, &created, &gaps.versionID, &gaps.raw, &source, &refreshed, &legacy, &newest,
		&lc.LastCheckAt, &lc.LastCheckOutcome, &lc.LastCheckDetail, &lc.LastCheckSourceAsOf)
	if db.IsNoRows(err) {
		return nil, pageNotFound(s)
	}
	if err != nil {
		return nil, err
	}
	c, err := indexedOrLegacyCoverage(source, refreshed, legacy)
	if err != nil {
		return nil, err
	}
	if schedule == nil {
		gaps.versionID = 0 // not judged, so not needed
	}
	found, err := resolveCoverageGaps(ctx, q, []storedGaps{gaps})
	if err != nil {
		return nil, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	out.Freshness = FreshnessOf(lastCheckInput(lc, c), now)
	out.RefreshHealth = HealthOf(healthInput(schedule, owner, isLive, created, c, newest, lc, found[gaps.versionID]), now)
	return &out, nil
}

// HealthCounts is how many pages are in each health state, plus how many of
// the unscheduled ones carry the unscheduled_stale hint (a subset of
// Unscheduled, not a seventh state).
type HealthCounts struct {
	Blocked          int `json:"blocked"`
	Overdue          int `json:"overdue"`
	SourceStale      int `json:"source_stale"`
	CoverageGap      int `json:"coverage_gap"`
	OK               int `json:"ok"`
	Unscheduled      int `json:"unscheduled"`
	UnscheduledStale int `json:"unscheduled_stale"`
}

func (c *HealthCounts) add(h *RefreshHealth) {
	if h.UnscheduledStale {
		c.UnscheduledStale++
	}
	switch h.Health {
	case HealthBlocked:
		c.Blocked++
	case HealthOverdue:
		c.Overdue++
	case HealthSourceStale:
		c.SourceStale++
	case HealthCoverageGap:
		c.CoverageGap++
	case HealthOK:
		c.OK++
	default:
		c.Unscheduled++
	}
}

// RefreshHealthRow is one page of the refresh-health report: who to tell,
// what is wrong, since when, and the two facts a reader checks first.
type RefreshHealthRow struct {
	Slug            string   `json:"slug"`
	Title           string   `json:"title"`
	WorkspaceID     jsx.ID   `json:"workspace_id"`
	WorkspaceName   *string  `json:"workspace_name"`
	IsLive          bool     `json:"is_live"`
	RefreshOwner    *string  `json:"refresh_owner"`
	RefreshSchedule *string  `json:"refresh_schedule"`
	Health          string   `json:"health"`
	HealthReason    string   `json:"health_reason"`
	HealthSince     jsx.Time `json:"health_since"`
	LastDueAt       jsx.Time `json:"last_due_at"`
	SourceAsOf      jsx.Time `json:"source_as_of"`
	LastActivityAt  jsx.Time `json:"last_activity_at"`
	// UnscheduledStale, DaysSinceSource and CoverageGaps are the page's
	// RefreshHealth fields of the same names.
	UnscheduledStale bool                   `json:"unscheduled_stale"`
	DaysSinceSource  *int64                 `json:"days_since_source"`
	CoverageGaps     []pagedata.CoverageGap `json:"coverage_gaps,omitempty"`
}

// RefreshHealthReport is ListRefreshHealth's result. Counts cover every
// (non-deleted) page, whichever rows Pages carries.
type RefreshHealthReport struct {
	GeneratedAt jsx.Time           `json:"generated_at"`
	Counts      HealthCounts       `json:"counts"`
	Pages       []RefreshHealthRow `json:"pages"`
}

var healthRank = map[string]int{HealthBlocked: 0, HealthOverdue: 1, HealthSourceStale: 2, HealthCoverageGap: 3, HealthOK: 4, HealthUnscheduled: 5}

// ListRefreshHealth is the estate's refresh health, worst first: blocked,
// overdue, source_stale, coverage_gap (each oldest problem first), then ok
// and unscheduled, the unscheduled_stale ones first among those (oldest data
// first). onlyAttention keeps the first four — the daily-digest read — and
// so never the unscheduled_stale rows, which Counts still counts.
func ListRefreshHealth(ctx context.Context, q db.Querier, onlyAttention bool) (*RefreshHealthReport, error) {
	pages, err := ListPages(ctx, q, ListPagesOptions{})
	if err != nil {
		return nil, err
	}
	report := &RefreshHealthReport{GeneratedAt: jsx.NewTime(msTime(time.Now())), Pages: []RefreshHealthRow{}}
	for _, p := range pages {
		h := p.RefreshHealth
		report.Counts.add(h)
		if onlyAttention && !h.NeedsAttention() {
			continue
		}
		row := RefreshHealthRow{
			Slug: p.Slug, Title: p.Title, WorkspaceID: p.WorkspaceID, WorkspaceName: p.WorkspaceName, IsLive: p.IsLive,
			RefreshOwner: h.RefreshOwner, RefreshSchedule: h.RefreshSchedule,
			Health: h.Health, HealthReason: h.HealthReason, HealthSince: h.HealthSince, LastDueAt: h.LastDueAt,
			LastActivityAt: h.LastActivityAt, UnscheduledStale: h.UnscheduledStale, DaysSinceSource: h.DaysSinceSource,
			CoverageGaps: h.CoverageGaps,
		}
		if p.Freshness != nil {
			row.SourceAsOf = p.Freshness.SourceAsOf
		}
		report.Pages = append(report.Pages, row)
	}
	sort.SliceStable(report.Pages, func(i, j int) bool {
		a, b := report.Pages[i], report.Pages[j]
		if healthRank[a.Health] != healthRank[b.Health] {
			return healthRank[a.Health] < healthRank[b.Health]
		}
		// Among the unscheduled, the hinted ones lead, the oldest data first.
		if a.UnscheduledStale != b.UnscheduledStale {
			return a.UnscheduledStale
		}
		if a.UnscheduledStale && !a.SourceAsOf.Equal(b.SourceAsOf.Time) {
			return a.SourceAsOf.Before(b.SourceAsOf.Time)
		}
		if !a.HealthSince.Equal(b.HealthSince.Time) {
			// Oldest problem first; a row with no start date last.
			if a.HealthSince.IsZero() || b.HealthSince.IsZero() {
				return b.HealthSince.IsZero()
			}
			return a.HealthSince.Before(b.HealthSince.Time)
		}
		return a.Slug < b.Slug
	})
	return report, nil
}

// ── setting it ─────────────────────────────────────────────────────────────

// RefreshScheduleInput sets or clears a page's refresh schedule and owner.
// Each of Schedule and Owner is absent (nil: unchanged), Null or a blank
// string (clear), or a string. At least one must be present.
type RefreshScheduleInput struct {
	Slug     string
	Schedule any
	Owner    any
}

// RefreshScheduleResult is SetRefreshSchedule's result.
type RefreshScheduleResult struct {
	Slug          string         `json:"slug"`
	Changed       bool           `json:"changed"`
	RefreshHealth *RefreshHealth `json:"refresh_health"`
}

// NormalizeRefreshSchedule validates a schedule: "" for blank, else the
// expression with single-spaced fields. 400 refresh_schedule_invalid.
func NormalizeRefreshSchedule(v string) (string, error) {
	if jsTrim(v) == "" {
		return "", nil
	}
	s, err := cron.Parse(v)
	if err != nil {
		return "", apierror.BadRequest("refresh schedule: "+err.Error(), "refresh_schedule_invalid", nil)
	}
	return s.String(), nil
}

// NormalizeRefreshOwner validates an owner email: "" for blank, else the
// address trimmed, NFC-normalized and lower-cased the way sign-in normalizes
// it. 400 refresh_owner_invalid.
func NormalizeRefreshOwner(v string) (string, error) {
	if jsTrim(v) == "" {
		return "", nil
	}
	email, err := central.NormalizeEmail(v)
	if err != nil {
		return "", apierror.BadRequest("refresh owner must be an email address", "refresh_owner_invalid", nil)
	}
	return email, nil
}

func scheduleArg(v any, normalize func(string) (string, error), name string) (value string, set bool, err error) {
	if v == nil {
		return "", false, nil
	}
	if v == Null {
		return "", true, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, apierror.BadRequest(name+" must be a string or null", name+"_invalid", nil)
	}
	value, err = normalize(s)
	return value, true, err
}

// SetRefreshSchedule records how often a page is expected to refresh and
// who answers for it. Agents may call it — they are the ones that schedule
// refreshes — as may staff and the operator CLI. Monitoring metadata, not
// content: the published pointer, versions and the page's updated_at are
// untouched, so backfilling every page's schedule does not make every row of
// the index read "updated just now". An unchanged value writes no audit row.
func SetRefreshSchedule(ctx context.Context, d *db.DB, in RefreshScheduleInput, a Actor) (*RefreshScheduleResult, error) {
	schedule, setSchedule, err := scheduleArg(in.Schedule, NormalizeRefreshSchedule, "refresh_schedule")
	if err != nil {
		return nil, err
	}
	owner, setOwner, err := scheduleArg(in.Owner, NormalizeRefreshOwner, "refresh_owner")
	if err != nil {
		return nil, err
	}
	if !setSchedule && !setOwner {
		return nil, apierror.BadRequest("pass schedule, owner or both (null clears one)", "refresh_schedule_required", nil)
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*RefreshScheduleResult, error) {
		page, err := LockPage(ctx, tx, in.Slug)
		if err != nil {
			return nil, err
		}
		var oldSchedule, oldOwner *string
		if err := tx.QueryRow(ctx, `SELECT refresh_schedule, refresh_owner FROM pages WHERE id = $1`, page.ID).
			Scan(&oldSchedule, &oldOwner); err != nil {
			return nil, err
		}
		newSchedule, newOwner := deref(oldSchedule), deref(oldOwner)
		if setSchedule {
			newSchedule = schedule
		}
		if setOwner {
			newOwner = owner
		}
		changed := newSchedule != deref(oldSchedule) || newOwner != deref(oldOwner)
		if changed {
			if _, err := tx.Exec(ctx, `UPDATE pages SET refresh_schedule = $1, refresh_owner = $2 WHERE id = $3`,
				strPtr(newSchedule), strPtr(newOwner), page.ID); err != nil {
				return nil, err
			}
			if err := audit.Write(ctx, tx, a.entry("set_refresh_schedule", page.ID, 0, map[string]any{
				"refresh_schedule": strPtr(newSchedule), "refresh_owner": strPtr(newOwner),
				"previous_schedule": oldSchedule, "previous_owner": oldOwner,
			})); err != nil {
				return nil, err
			}
		}
		health, err := RefreshHealthFor(ctx, tx, page.Slug, time.Time{})
		if err != nil {
			return nil, err
		}
		return &RefreshScheduleResult{Slug: page.Slug, Changed: changed, RefreshHealth: health.RefreshHealth}, nil
	})
}
