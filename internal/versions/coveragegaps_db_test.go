// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/pagedata"
)

const openDataSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`

// gapDays is the trader payload's window: twenty days ending yesterday.
func gapDays() (first, last time.Time) {
	last = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	return last.AddDate(0, 0, -19), last
}

// gapRows is a trader payload: one row per (dealId, date) for D-1..D-4,
// revenue and clicks from one source and spend from another. With stopped,
// D-1's and D-2's revenue and clicks end ten days before their spend does.
func gapRows(stopped bool) string {
	first, last := gapDays()
	var rows []string
	for day := first; !day.After(last); day = day.AddDate(0, 0, 1) {
		for i := 1; i <= 4; i++ {
			row := fmt.Sprintf(`{"date":"%s","dealId":"D-%d","spend":%d`, day.Format("2006-01-02"), i, 10+i+day.Day()%5)
			if !stopped || i > 2 || day.Before(last.AddDate(0, 0, -9)) {
				row += fmt.Sprintf(`,"revenue":%d,"clicks":%d`, 100+i+day.Day()%7, i+day.Day()%3)
			}
			rows = append(rows, row+"}")
		}
	}
	return `{"rows":[` + strings.Join(rows, ",") + `]}`
}

// storedNone is how jsonb prints a stored result with no findings.
var storedNone = fmt.Sprintf(`{"v": %d, "gaps": []}`, pagedata.CoverageGapsVersion)

func storedPrefix() string { return fmt.Sprintf(`{"v": %d, `, pagedata.CoverageGapsVersion) }

func gapPageHTML(sourceAsOf, data string) string {
	return strings.Replace(managedHTML(sourceAsOf, data), dataSchema, openDataSchema, 1)
}

func storedGapsOf(t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, slug string) *string {
	t.Helper()
	var raw *string
	noErr(t, q.QueryRow(context.Background(),
		`SELECT pv.coverage_gaps::text FROM pages p JOIN page_versions pv ON pv.id = p.published_version_id WHERE p.slug = $1`, slug).Scan(&raw))
	return raw
}

func TestCoverageGapHealth(t *testing.T) {
	d, ctx := setup(t)
	_, last := gapDays()
	stoppedOn := last.AddDate(0, 0, -10)
	base := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
		DeployInput: DeployInput{Slug: "cg/northwind", HTML: gapPageHTML(hoursAgo(4), gapRows(true)), Source: SourceMCP, Publish: true},
		Title:       "Northwind trader",
	}, agent))(t)

	// Written with the version: computed once, at write time.
	raw := storedGapsOf(t, d, "cg/northwind")
	if raw == nil || !strings.HasPrefix(*raw, storedPrefix()+`"gaps": [{`) || !strings.Contains(*raw, `"stopped_on": "`+stoppedOn.Format("2006-01-02")+`"`) {
		t.Fatalf("stored at write time: %v", raw)
	}

	health := func(slug string) *RefreshHealth {
		t.Helper()
		return must(RefreshHealthFor(ctx, d, slug, time.Time{}))(t).RefreshHealth
	}
	// No schedule, no judgement.
	if h := health("cg/northwind"); h.Health != HealthUnscheduled || h.CoverageGaps != nil {
		t.Fatalf("unscheduled: %+v", h)
	}
	must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "cg/northwind", Schedule: everyMinute, Owner: "desk@example.com"}, agent))(t)
	// The run that just checked found nothing new: everything else is fine.
	must(RecordRefreshCheck(ctx, d, RefreshCheckInput{Slug: "cg/northwind", Outcome: "source_not_updated"}, agent))(t)
	want := fmt.Sprintf(`In rows, revenue and clicks end on %s for 2 dealIds ("D-1" and "D-2"), whose rows continue to %s.`,
		stoppedOn.Format("2006-01-02"), last.Format("2006-01-02"))
	h := health("cg/northwind")
	if h.Health != HealthCoverageGap || h.HealthReason != want || !h.HealthSince.Equal(stoppedOn.AddDate(0, 0, 1)) || !h.NeedsAttention() ||
		len(h.CoverageGaps) != 1 || h.CoverageGaps[0].EntityCount != 2 {
		t.Fatalf("coverage_gap: %+v", h)
	}

	// The list rows and the report say the same.
	for _, p := range must(ListPages(ctx, d, ListPagesOptions{}))(t) {
		if p.Slug == "cg/northwind" && (p.RefreshHealth.Health != HealthCoverageGap || p.RefreshHealth.HealthReason != want) {
			t.Fatalf("list row: %+v", p.RefreshHealth)
		}
	}
	report := must(ListRefreshHealth(ctx, d, true))(t)
	if len(report.Pages) != 1 || report.Pages[0].Health != HealthCoverageGap || len(report.Pages[0].CoverageGaps) != 1 ||
		report.Counts != (HealthCounts{CoverageGap: 1}) {
		t.Fatalf("report: %+v", report)
	}

	// A refresh that reads both sources for every deal clears it.
	must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "cg/northwind", Data: jv(t, gapRows(false)), SourceAsOf: hoursAgo(1),
		ExpectedVersion: base.Version.ID.String()}, agent))(t)
	if raw := storedGapsOf(t, d, "cg/northwind"); raw == nil || *raw != storedNone {
		t.Fatalf("the data update stored its own (empty) result: %v", raw)
	}
	if h := health("cg/northwind"); h.Health != HealthOK || h.CoverageGaps != nil {
		t.Fatalf("ok again: %+v", h)
	}

	// A page with no managed data stores an empty result too.
	must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
		DeployInput: DeployInput{Slug: "cg/plain", HTML: "<!doctype html><title>Plain</title><p>Contoso</p>", Source: SourceMCP, Publish: true},
		Title:       "Plain",
	}, agent))(t)
	if raw := storedGapsOf(t, d, "cg/plain"); raw == nil || *raw != storedNone {
		t.Fatalf("plain page: %v", raw)
	}
}

// Versions written before migration 029 (or under an older rule) are filled
// on read, saved for the next read, and never make a read fail or wait.
func TestCoverageGapsFilledLazily(t *testing.T) {
	d, ctx := setup(t)
	for _, slug := range []string{"cg/fabrikam", "cg/tailspin"} {
		must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
			DeployInput: DeployInput{Slug: slug, HTML: gapPageHTML(hoursAgo(4), gapRows(true)), Source: SourceMCP, Publish: true},
			Title:       slug,
		}, agent))(t)
		must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: slug, Schedule: everyMinute}, agent))(t)
		must(RecordRefreshCheck(ctx, d, RefreshCheckInput{Slug: slug, Outcome: "source_not_updated"}, agent))(t)
	}
	forget := func(value string) {
		t.Helper()
		must(d.Exec(ctx, `UPDATE page_versions SET coverage_gaps = `+value))(t)
	}

	// A single page's read computes and saves.
	forget("NULL")
	if h := must(RefreshHealthFor(ctx, d, "cg/fabrikam", time.Time{}))(t).RefreshHealth; h.Health != HealthCoverageGap {
		t.Fatalf("computed on read: %+v", h)
	}
	if storedGapsOf(t, d, "cg/fabrikam") == nil || storedGapsOf(t, d, "cg/tailspin") != nil {
		t.Fatal("saved for the page that was read, and only that one")
	}
	// So does a list, and an older rule version counts as missing.
	forget(fmt.Sprintf(`'{"v":%d,"gaps":[]}'`, pagedata.CoverageGapsVersion-1))
	for _, p := range must(ListPages(ctx, d, ListPagesOptions{}))(t) {
		if p.RefreshHealth.Health != HealthCoverageGap {
			t.Fatalf("%s: %+v", p.Slug, p.RefreshHealth)
		}
	}
	for _, slug := range []string{"cg/fabrikam", "cg/tailspin"} {
		if raw := storedGapsOf(t, d, slug); raw == nil || !strings.HasPrefix(*raw, storedPrefix()) {
			t.Fatalf("%s recomputed under the current rule: %v", slug, raw)
		}
	}

	// Inside a read-only snapshot (the admin index): computed, not saved, and
	// the transaction stays usable.
	forget("NULL")
	noErr(t, d.WithTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY"); err != nil {
			return err
		}
		for _, p := range must(ListPages(ctx, tx, ListPagesOptions{}))(t) {
			if p.RefreshHealth.Health != HealthCoverageGap {
				t.Fatalf("%s in a snapshot: %+v", p.Slug, p.RefreshHealth)
			}
		}
		var one int
		return tx.QueryRow(ctx, "SELECT 1").Scan(&one)
	}))
	if storedGapsOf(t, d, "cg/fabrikam") != nil {
		t.Fatal("a read-only snapshot cannot have saved")
	}
	// Which is why the index fills them first.
	noErr(t, FillCoverageGaps(ctx, d))
	if storedGapsOf(t, d, "cg/fabrikam") == nil || storedGapsOf(t, d, "cg/tailspin") == nil {
		t.Fatal("FillCoverageGaps saves the published versions")
	}

	// A version another transaction has locked is skipped, not waited on:
	// the read still answers, and a later read saves it.
	forget("NULL")
	tx := must(d.Begin(ctx))(t)
	defer func() { _ = tx.Rollback(ctx) }()
	must(tx.Exec(ctx, `SELECT 1 FROM page_versions pv JOIN pages p ON pv.id = p.published_version_id WHERE p.slug = 'cg/fabrikam' FOR UPDATE OF pv`))(t)
	if h := must(RefreshHealthFor(ctx, d, "cg/fabrikam", time.Time{}))(t).RefreshHealth; h.Health != HealthCoverageGap {
		t.Fatalf("answered while locked: %+v", h)
	}
	if storedGapsOf(t, d, "cg/fabrikam") != nil {
		t.Fatal("a locked version is left for a later read")
	}
	noErr(t, tx.Rollback(ctx))
	must(RefreshHealthFor(ctx, d, "cg/fabrikam", time.Time{}))(t)
	if storedGapsOf(t, d, "cg/fabrikam") == nil {
		t.Fatal("saved once the lock is gone")
	}

	// The database refuses a shape no release writes.
	if _, err := d.Exec(ctx, `UPDATE page_versions SET coverage_gaps = '[]'`); err == nil {
		t.Fatal("the coverage_gaps CHECK let an array through")
	}
}

// Each read re-reads at most maxLazyCoverageGaps stored documents.
func TestCoverageGapsLazyFillIsBounded(t *testing.T) {
	d, ctx := setup(t)
	for i := 0; i < maxLazyCoverageGaps+3; i++ {
		slug := fmt.Sprintf("cg/bulk-%02d", i)
		must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
			DeployInput: DeployInput{Slug: slug, HTML: gapPageHTML(hoursAgo(4), `{"count":1}`), Source: SourceMCP, Publish: true},
			Title:       "Bulk",
		}, agent))(t)
		must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: slug, Schedule: everyMinute}, agent))(t)
	}
	// An unscheduled page is not judged, so nothing is computed for it.
	must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
		DeployInput: DeployInput{Slug: "cg/unscheduled", HTML: gapPageHTML(hoursAgo(4), gapRows(true)), Source: SourceMCP, Publish: true},
		Title:       "Unscheduled",
	}, agent))(t)
	must(d.Exec(ctx, `UPDATE page_versions SET coverage_gaps = NULL`))(t)
	count := func() int {
		var n int
		noErr(t, d.QueryRow(ctx, `SELECT count(*) FROM page_versions WHERE coverage_gaps IS NOT NULL`).Scan(&n))
		return n
	}
	must(ListPages(ctx, d, ListPagesOptions{}))(t)
	if n := count(); n != maxLazyCoverageGaps {
		t.Fatalf("one read filled %d", n)
	}
	noErr(t, FillCoverageGaps(ctx, d))
	if n := count(); n != maxLazyCoverageGaps+3 {
		t.Fatalf("the next filled the rest: %d", n)
	}
	must(RefreshHealthFor(ctx, d, "cg/unscheduled", time.Time{}))(t)
	if storedGapsOf(t, d, "cg/unscheduled") != nil {
		t.Fatal("an unscheduled page's gaps are not computed on read")
	}
	var v int
	noErr(t, d.QueryRow(ctx, `SELECT (coverage_gaps->>'v')::int FROM page_versions WHERE coverage_gaps IS NOT NULL LIMIT 1`).Scan(&v))
	if v != pagedata.CoverageGapsVersion {
		t.Fatal(v)
	}
}

// A finding names values from the payload, and jsonb refuses U+0000: the
// write must still succeed, and the finding still be stored.
func TestCoverageGapsWithAwkwardNamesStillWrite(t *testing.T) {
	d, ctx := setup(t)
	data := strings.ReplaceAll(gapRows(true), `"D-1"`, `"D-1\u0000"`)
	must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
		DeployInput: DeployInput{Slug: "cg/awkward", HTML: gapPageHTML(hoursAgo(4), data), Source: SourceMCP, Publish: true},
		Title:       "Awkward",
	}, agent))(t)
	if raw := storedGapsOf(t, d, "cg/awkward"); raw == nil || !strings.Contains(*raw, "D-1�") {
		t.Fatalf("stored: %v", raw)
	}
}

// A page created without data stores the epoch as its source_as_of. Read back,
// that is "no source data yet": no 1970 anywhere, and staleness judged from
// the page's creation.
func TestNoSourceDataYet(t *testing.T) {
	d, ctx := setup(t)
	must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
		DeployInput: DeployInput{Slug: "ns/northwind", HTML: gapPageHTML("1970-01-01T00:00:00Z", `{"rows":[]}`), Source: SourceMCP, Publish: true},
		Title:       "Awaiting data",
	}, agent))(t)
	must(d.Exec(ctx, `UPDATE pages SET created_at = now() - interval '10 days' WHERE slug = 'ns/northwind'`))(t)
	must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "ns/northwind", Schedule: everyMinute}, agent))(t)
	must(RecordRefreshCheck(ctx, d, RefreshCheckInput{Slug: "ns/northwind", Outcome: "source_not_updated"}, agent))(t)

	read := must(RefreshHealthFor(ctx, d, "ns/northwind", time.Time{}))(t)
	h, f := read.RefreshHealth, read.Freshness
	if h.Health != HealthSourceStale || !strings.HasPrefix(h.HealthReason, "No source data yet since ") || h.DaysSinceSource != nil {
		t.Fatalf("health: %+v", h)
	}
	if f == nil || !f.SourceAsOf.IsZero() || f.DaysSinceSource != nil || f.RefreshedAt.IsZero() {
		t.Fatalf("freshness: %+v", f)
	}
	for _, p := range must(ListPages(ctx, d, ListPagesOptions{}))(t) {
		if p.RefreshHealth.Health != HealthSourceStale || p.Freshness.DaysSinceSource != nil || !p.Freshness.SourceAsOf.IsZero() {
			t.Fatalf("list row: %+v %+v", p.RefreshHealth, p.Freshness)
		}
	}
	report := must(ListRefreshHealth(ctx, d, true))(t)
	if len(report.Pages) != 1 || !report.Pages[0].SourceAsOf.IsZero() || report.Pages[0].DaysSinceSource != nil {
		t.Fatalf("report: %+v", report.Pages)
	}
	view := must(GetPageData(ctx, d, "ns/northwind"))(t)
	if !view.Freshness.SourceAsOf.IsZero() || view.Freshness.DaysSinceSource != nil {
		t.Fatalf("get_page_data freshness: %+v", view.Freshness)
	}
}
