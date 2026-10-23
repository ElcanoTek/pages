// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

import (
	"fmt"
	"testing"
	"time"

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
