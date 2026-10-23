// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/jsx"
)

// The schedule here fires every minute, so "the latest fire whose grace has
// passed" is always three hours ago, whatever the wall clock says.
const everyMinute = "* * * * *"

func TestRefreshScheduleAndHealth(t *testing.T) {
	d, ctx := setup(t)
	source := hoursAgo(4)
	base := must(CreateAndDeploy(ctx, d, CreateAndDeployInput{
		DeployInput: DeployInput{Slug: "rh/northwind", HTML: managedHTML(source, `{"count":1,"label":"a"}`), Source: SourceMCP, Publish: true},
		Title:       "Northwind refresh",
	}, agent))(t)

	health := func(slug string) *RefreshHealth {
		t.Helper()
		return must(RefreshHealthFor(ctx, d, slug, time.Time{}))(t).RefreshHealth
	}
	auditRows := func() int {
		t.Helper()
		var n int
		noErr(t, d.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'set_refresh_schedule'`).Scan(&n))
		return n
	}

	if h := health("rh/northwind"); h.Health != HealthUnscheduled || h.RefreshSchedule != nil || h.LastActivityAt.IsZero() {
		t.Fatalf("a new page is unscheduled: %+v", h)
	}

	// The envelope was refreshed five hours ago; a fire three hours ago went
	// unanswered.
	before := getPage(t, d, "rh/northwind").Page.UpdatedAt
	set := must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind", Schedule: "  *  * * * * ", Owner: "Trader@Example.COM"}, agent))(t)
	if !set.Changed || set.Slug != "rh/northwind" || *set.RefreshHealth.RefreshSchedule != everyMinute ||
		*set.RefreshHealth.RefreshOwner != "trader@example.com" || set.RefreshHealth.Health != HealthOverdue {
		t.Fatalf("%+v %+v", set, set.RefreshHealth)
	}
	var actor, actorType string
	var meta map[string]any
	noErr(t, d.QueryRow(ctx, `SELECT actor, actor_type, metadata FROM audit_log WHERE action = 'set_refresh_schedule' ORDER BY id DESC LIMIT 1`).
		Scan(&actor, &actorType, &meta))
	if actor != agent.Actor || actorType != audit.Agent || meta["refresh_schedule"] != everyMinute || meta["refresh_owner"] != "trader@example.com" ||
		meta["previous_schedule"] != nil || meta["previous_owner"] != nil {
		t.Fatal(actor, actorType, meta)
	}
	// Monitoring metadata is not a page change: updated_at stays put.
	if after := getPage(t, d, "rh/northwind").Page.UpdatedAt; !after.Equal(before.Time) {
		t.Fatalf("updated_at moved: %v → %v", before, after)
	}
	// Unchanged is a no-op with no audit row.
	again := must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind", Schedule: everyMinute}, admin))(t)
	if again.Changed || auditRows() != 1 {
		t.Fatalf("%+v, %d audit rows", again, auditRows())
	}

	// A failed run is the latest outcome: blocked, quoting the run.
	must(RecordRefreshCheck(ctx, d, RefreshCheckInput{Slug: "rh/northwind", Outcome: "blocked", Detail: "schema drift on rows.date"}, agent))(t)
	if h := health("rh/northwind"); h.Health != HealthBlocked || h.HealthReason != "schema drift on rows.date" || h.HealthSince.IsZero() {
		t.Fatalf("%+v", h)
	}
	// A run that found nothing new: the page is ok again (the data is four
	// hours old and the schedule's gap is a minute, well inside the slack).
	must(RecordRefreshCheck(ctx, d, RefreshCheckInput{Slug: "rh/northwind", Outcome: "source_not_updated"}, agent))(t)
	if h := health("rh/northwind"); h.Health != HealthOK || h.NeedsAttention() {
		t.Fatalf("%+v", h)
	}

	// A refresh waiting on approval is activity even though it is not live.
	must(SetApproval(ctx, d, "rh/northwind", true, admin))(t)
	pending := must(UpdatePageData(ctx, d, DataUpdateInput{Slug: "rh/northwind", Data: jv(t, `{"count":2,"label":"b"}`), SourceAsOf: hoursAgo(1),
		ExpectedVersion: base.Version.ID.String()}, agent))(t)
	if pending.Version.Status != "pending" {
		t.Fatalf("%+v", pending.Version)
	}
	read := must(RefreshHealthFor(ctx, d, "rh/northwind", time.Time{}))(t)
	if want := pending.Envelope.Value("refreshed_at"); jsx.FormatTime(read.RefreshHealth.LastActivityAt.Time) != want {
		t.Fatalf("last activity %v, want the pending refresh %v", read.RefreshHealth.LastActivityAt, want)
	}
	if jsx.FormatTime(read.Freshness.SourceAsOf.Time) != source {
		t.Fatal("freshness still describes the live version")
	}

	// A second page that has never been refreshed, created a day ago.
	must(CreatePage(ctx, d, CreatePageInput{Slug: "rh/contoso", Title: "Contoso"}, agent))(t)
	must(d.Exec(ctx, `UPDATE pages SET created_at = now() - interval '1 day' WHERE slug = 'rh/contoso'`))(t)
	must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/contoso", Schedule: everyMinute}, admin))(t)
	if h := health("rh/contoso"); h.Health != HealthOverdue || !strings.Contains(h.HealthReason, "no refresh or check has been recorded yet") || h.RefreshOwner != nil {
		t.Fatalf("%+v", h)
	}
	must(CreatePage(ctx, d, CreatePageInput{Slug: "rh/fabrikam", Title: "Fabrikam"}, agent))(t)

	// Every list row carries the block; the report keeps only what needs a
	// human, worst first, and counts everything.
	rows := must(ListPages(ctx, d, ListPagesOptions{}))(t)
	seen := map[string]string{}
	for _, r := range rows {
		if r.RefreshHealth == nil {
			t.Fatalf("%s has no refresh_health", r.Slug)
		}
		seen[r.Slug] = r.RefreshHealth.Health
	}
	if seen["rh/northwind"] != HealthOK || seen["rh/contoso"] != HealthOverdue || seen["rh/fabrikam"] != HealthUnscheduled {
		t.Fatal(seen)
	}
	report := must(ListRefreshHealth(ctx, d, true))(t)
	if len(report.Pages) != 1 || report.Pages[0].Slug != "rh/contoso" || report.Pages[0].Health != HealthOverdue ||
		report.Counts != (HealthCounts{Overdue: 1, OK: 1, Unscheduled: 1}) || report.GeneratedAt.IsZero() {
		t.Fatalf("%+v", report)
	}
	all := must(ListRefreshHealth(ctx, d, false))(t)
	if len(all.Pages) != 3 || all.Pages[0].Slug != "rh/contoso" || all.Pages[1].Slug != "rh/northwind" || all.Pages[2].Health != HealthUnscheduled {
		t.Fatalf("%+v", all.Pages)
	}
	if all.Pages[1].SourceAsOf.IsZero() || *all.Pages[1].RefreshOwner != "trader@example.com" {
		t.Fatalf("%+v", all.Pages[1])
	}

	// Clearing the schedule stops the judgement and keeps the owner.
	cleared := must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind", Schedule: Null}, admin))(t)
	if !cleared.Changed || cleared.RefreshHealth.Health != HealthUnscheduled || *cleared.RefreshHealth.RefreshOwner != "trader@example.com" {
		t.Fatalf("%+v", cleared.RefreshHealth)
	}
	noErr(t, d.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE action = 'set_refresh_schedule' ORDER BY id DESC LIMIT 1`).Scan(&meta))
	if meta["refresh_schedule"] != nil || meta["previous_schedule"] != everyMinute {
		t.Fatal(meta)
	}
	blank := must(SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind", Owner: "  "}, admin))(t)
	if blank.RefreshHealth.RefreshOwner != nil {
		t.Fatal("a blank owner clears it")
	}

	_, err := SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind", Schedule: "0 15 * * MON"}, agent)
	wantCode(t, err, 400, "refresh_schedule_invalid")
	_, err = SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind", Owner: "not an email"}, agent)
	wantCode(t, err, 400, "refresh_owner_invalid")
	_, err = SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind"}, agent)
	wantCode(t, err, 400, "refresh_schedule_required")
	_, err = SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/northwind", Schedule: float64(15)}, agent)
	wantCode(t, err, 400, "refresh_schedule_invalid")
	_, err = SetRefreshSchedule(ctx, d, RefreshScheduleInput{Slug: "rh/nope", Schedule: everyMinute}, agent)
	wantCode(t, err, 404, "page_not_found")
	_, err = RefreshHealthFor(ctx, d, "rh/nope", time.Time{})
	wantCode(t, err, 404, "page_not_found")

	// The database refuses what no release could have written.
	if _, err := d.Exec(ctx, `UPDATE pages SET refresh_schedule = '@daily' WHERE slug = 'rh/northwind'`); err == nil {
		t.Fatal("the schedule CHECK let a macro through")
	}
	if _, err := d.Exec(ctx, `UPDATE pages SET refresh_owner = 'nobody' WHERE slug = 'rh/northwind'`); err == nil {
		t.Fatal("the owner CHECK let a non-address through")
	}
	// A deleted page leaves the report.
	must(DeletePage(ctx, d, "rh/contoso", admin))(t)
	if r := must(ListRefreshHealth(ctx, d, true))(t); len(r.Pages) != 0 {
		t.Fatalf("%+v", r.Pages)
	}
}
