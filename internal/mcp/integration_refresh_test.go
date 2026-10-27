// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/tokens"
)

// Refresh health over the wire: an agent that installs a recurring refresh
// tells Pages its cadence, and every read an agent or a digest makes reports
// how the page stands against it.
func TestRefreshScheduleAndHealthTools(t *testing.T) {
	h := newHarness(t)
	h.data(h.call("deploy_page", o("slug", "mcp-health", "title", "Health", "html", managedPage(isoAgo(4*time.Hour), nil))))

	// Unscheduled: every read carries the block, and it judges nothing.
	summary := h.data(h.call("get_page_data", o("slug", "mcp-health", "detail", "summary")))
	eq(t, dig(summary, "refresh_health", "health"), "unscheduled")
	eq(t, dig(summary, "refresh_health", "refresh_schedule"), nil)
	// Live and unscheduled, but its data is hours old: no hint.
	eq(t, dig(summary, "refresh_health", "unscheduled_stale"), false)
	eq(t, dig(summary, "refresh_health", "days_since_source"), float64(0))
	keys := ordered(t, h.call("get_page_data", o("slug", "mcp-health", "detail", "summary"))).Keys()
	for i, k := range keys {
		if k == "refresh_health" && (i == 0 || keys[i-1] != "freshness") {
			t.Fatalf("refresh_health sits beside freshness: %v", keys)
		}
	}
	if _, has := h.data(h.call("get_page_data", o("slug", "mcp-health")))["refresh_health"]; has {
		t.Fatal("a full read does not carry refresh_health")
	}
	if _, has := h.data(h.call("get_page_data", o("slug", "mcp-health", "detail", "export")))["refresh_health"]; has {
		t.Fatal("an export read does not carry refresh_health")
	}

	// Every minute, so the fire three hours ago is always the one judged; the
	// envelope was refreshed five hours ago and nothing answered it.
	set := h.data(h.call("set_page_refresh_schedule", o("slug", "mcp-health", "schedule", "* * * * *", "owner", "Trader@Example.com")))
	eq(t, set["slug"], "mcp-health")
	eq(t, set["changed"], true)
	eq(t, dig(set, "refresh_health", "refresh_schedule"), "* * * * *")
	eq(t, dig(set, "refresh_health", "refresh_owner"), "trader@example.com")
	eq(t, dig(set, "refresh_health", "health"), "overdue")
	mustMatch(t, `^Expected a refresh at .+ UTC; last activity .+\.$`, strAt(set, "refresh_health", "health_reason"))
	audit := h.query(`SELECT actor_type, token_id, metadata FROM audit_log WHERE action = 'set_refresh_schedule'`)
	eq(t, len(audit), 1)
	eq(t, audit[0]["actor_type"], "agent")
	eq(t, fmt.Sprint(audit[0]["token_id"]), h.tokenID.String())
	eq(t, dig(audit[0]["metadata"], "refresh_schedule"), "* * * * *")
	// The same values again change nothing and are not audited twice.
	eq(t, h.data(h.call("set_page_refresh_schedule", o("slug", "mcp-health", "schedule", "* * * * *")))["changed"], false)
	eq(t, h.count(`SELECT count(*)::integer FROM audit_log WHERE action = 'set_refresh_schedule'`), 1)

	var listed any
	for _, p := range toAnySlice(h.data(h.call("list_pages", o("limit", num(100))))["pages"]) {
		if strAt(p, "slug") == "mcp-health" {
			listed = p
		}
	}
	eq(t, dig(listed, "refresh_health", "health"), "overdue")
	eq(t, dig(listed, "refresh_health", "refresh_owner"), "trader@example.com")

	report := h.data(h.call("list_refresh_health", nil))
	rows := toAnySlice(report["pages"])
	eq(t, len(rows), 1)
	eq(t, strAt(rows[0], "slug"), "mcp-health")
	eq(t, strAt(rows[0], "health"), "overdue")
	eq(t, strAt(rows[0], "refresh_owner"), "trader@example.com")
	mustMatch(t, `/admin/mcp-health$`, strAt(rows[0], "urls", "admin"))
	if strAt(rows[0], "source_as_of") == "" || strAt(rows[0], "last_activity_at") == "" || strAt(rows[0], "health_since") == "" {
		t.Fatalf("%v", rows[0])
	}
	eq(t, dig(report, "counts", "overdue"), float64(1))
	eq(t, dig(report, "counts", "unscheduled_stale"), float64(0))

	// A live page with no schedule whose data stopped nine days ago carries
	// the hint everywhere refresh_health goes, is counted on its own, and is
	// still not something that needs attention.
	h.data(h.call("deploy_page", o("slug", "mcp-stale", "title", "Stale", "html", managedPage(isoAgo(9*24*time.Hour+time.Hour), nil))))
	stale := h.data(h.call("get_page_data", o("slug", "mcp-stale", "detail", "summary")))
	eq(t, dig(stale, "refresh_health", "health"), "unscheduled")
	eq(t, dig(stale, "refresh_health", "unscheduled_stale"), true)
	eq(t, dig(stale, "refresh_health", "days_since_source"), float64(9))
	var staleListed any
	for _, p := range toAnySlice(h.data(h.call("list_pages", o("limit", num(100))))["pages"]) {
		if strAt(p, "slug") == "mcp-stale" {
			staleListed = p
		}
	}
	eq(t, dig(staleListed, "refresh_health", "unscheduled_stale"), true)
	withStale := h.data(h.call("list_refresh_health", nil))
	eq(t, len(toAnySlice(withStale["pages"])), 1)
	eq(t, dig(withStale, "counts", "unscheduled_stale"), float64(1))
	eq(t, dig(withStale, "counts", "unscheduled"), float64(1))
	var staleRow any
	for _, r := range toAnySlice(h.data(h.call("list_refresh_health", o("needs_attention", false)))["pages"]) {
		if strAt(r, "slug") == "mcp-stale" {
			staleRow = r
		}
	}
	eq(t, dig(staleRow, "health"), "unscheduled")
	eq(t, dig(staleRow, "unscheduled_stale"), true)
	eq(t, dig(staleRow, "days_since_source"), float64(9))
	mustMatch(t, `/admin/mcp-stale$`, strAt(staleRow, "urls", "admin"))

	// A blocked run outranks a late one and quotes its own detail.
	h.data(h.call("record_refresh_check", o("slug", "mcp-health", "outcome", "blocked", "detail", "ssp report missing for 2026-10-21")))
	blocked := h.data(h.call("get_page_data", o("slug", "mcp-health", "detail", "summary")))
	eq(t, dig(blocked, "refresh_health", "health"), "blocked")
	eq(t, dig(blocked, "refresh_health", "health_reason"), "ssp report missing for 2026-10-21")
	// A run that found nothing new is a run doing its job.
	h.data(h.call("record_refresh_check", o("slug", "mcp-health", "outcome", "source_not_updated")))
	eq(t, len(toAnySlice(h.data(h.call("list_refresh_health", nil))["pages"])), 0)
	everything := h.data(h.call("list_refresh_health", o("needs_attention", false)))
	if len(toAnySlice(everything["pages"])) == 0 || dig(everything, "counts", "ok") != float64(1) {
		t.Fatalf("%v", everything)
	}

	// Validation happens before anything is written.
	eq(t, h.fails(h.call("set_page_refresh_schedule", o("slug", "mcp-health", "schedule", "0 15 * * MON"))).code(), "refresh_schedule_invalid")
	eq(t, h.fails(h.call("set_page_refresh_schedule", o("slug", "mcp-health", "owner", "nobody"))).code(), "refresh_owner_invalid")
	eq(t, h.fails(h.call("set_page_refresh_schedule", o("slug", "mcp-health"))).code(), "refresh_schedule_required")
	eq(t, h.fails(h.call("set_page_refresh_schedule", o("slug", "mcp-nope", "schedule", "0 15 * * *"))).code(), "page_not_found")
	eq(t, h.scalar(`SELECT refresh_schedule FROM pages WHERE slug = 'mcp-health'`), "* * * * *")

	// null stops the judgement.
	cleared := h.data(h.call("set_page_refresh_schedule", o("slug", "mcp-health", "schedule", nil)))
	eq(t, dig(cleared, "refresh_health", "health"), "unscheduled")
	eq(t, dig(cleared, "refresh_health", "refresh_owner"), "trader@example.com")

	// A data_update token runs refreshes; it neither sets schedules nor reads
	// the estate.
	scoped := h.mint("health-data", tokens.ScopeDataUpdate, "mcp-health")
	h.fails(h.call("set_page_refresh_schedule", o("slug", "mcp-health", "schedule", "0 15 * * *"), as(scoped.Token)))
	h.fails(h.call("list_refresh_health", nil, as(scoped.Token)))
	eq(t, h.scalar(`SELECT refresh_schedule FROM pages WHERE slug = 'mcp-health'`), nil)
	eq(t, dig(h.data(h.call("get_page_data", o("slug", "mcp-health", "detail", "summary"), as(scoped.Token))), "refresh_health", "health"), "unscheduled")
}

// traderPage is a managed page with one row per (dealId, date) over the last
// twenty days: revenue and clicks from one source, spend from another. D-1's
// and D-2's revenue and clicks end ten days before their spend does.
func traderPage() string {
	last := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	var rows []any
	for day := last.AddDate(0, 0, -19); !day.After(last); day = day.AddDate(0, 0, 1) {
		for i := 1; i <= 4; i++ {
			row := o("date", day.Format("2006-01-02"), "dealId", fmt.Sprintf("D-%d", i), "spend", num(float64(10+i+day.Day()%5)))
			if i > 2 || day.Before(last.AddDate(0, 0, -9)) {
				row.Set("revenue", num(float64(100+i+day.Day()%7)))
				row.Set("clicks", num(float64(i+day.Day()%3)))
			}
			rows = append(rows, row)
		}
	}
	schema := o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object")
	return `<!doctype html><html><head><title>Trader</title></head><body><h1>Northwind trader</h1>` +
		`<script id="pages-data-schema" type="application/schema+json">` + jsonv.Stringify(schema) + `</script>` +
		`<script id="pages-data" type="application/json">` + jsonv.Stringify(o("contract_version", num(1), "refreshed_at", isoAgo(5*time.Hour),
		"source_as_of", isoAgo(4*time.Hour), "data", o("rows", rows))) + `</script></body></html>`
}

// A page whose rows stopped carrying one source for some deals reads
// coverage_gap on every agent surface, with the findings, and every output
// still satisfies its declared schema.
func TestCoverageGapOverTheWire(t *testing.T) {
	h := newHarness(t)
	h.data(h.call("deploy_page", o("slug", "mcp-gap", "title", "Gap", "html", traderPage())))
	h.data(h.call("set_page_refresh_schedule", o("slug", "mcp-gap", "schedule", "* * * * *", "owner", "desk@example.com")))
	h.data(h.call("record_refresh_check", o("slug", "mcp-gap", "outcome", "source_not_updated")))

	summary := h.data(h.call("get_page_data", o("slug", "mcp-gap", "detail", "summary")))
	eq(t, dig(summary, "refresh_health", "health"), "coverage_gap")
	mustMatch(t, `^In rows, revenue and clicks end on \d{4}-\d{2}-\d{2} for 2 dealIds \("D-1" and "D-2"\), whose rows continue to \d{4}-\d{2}-\d{2}\.$`,
		strAt(summary, "refresh_health", "health_reason"))
	gaps := toAnySlice(dig(summary, "refresh_health", "coverage_gaps"))
	eq(t, len(gaps), 1)
	eq(t, dig(gaps[0], "key_field"), "dealId")
	eq(t, dig(gaps[0], "entity_count"), float64(2))

	var listed any
	for _, p := range toAnySlice(h.data(h.call("list_pages", o("limit", num(100))))["pages"]) {
		if strAt(p, "slug") == "mcp-gap" {
			listed = p
		}
	}
	eq(t, dig(listed, "refresh_health", "health"), "coverage_gap")

	report := h.data(h.call("list_refresh_health", nil))
	rows := toAnySlice(report["pages"])
	eq(t, len(rows), 1)
	eq(t, strAt(rows[0], "health"), "coverage_gap")
	eq(t, len(toAnySlice(dig(rows[0], "coverage_gaps"))), 1)
	eq(t, dig(report, "counts", "coverage_gap"), float64(1))
	if !strings.HasPrefix(strAt(rows[0], "health_since"), "20") {
		t.Fatalf("%v", rows[0])
	}
}
