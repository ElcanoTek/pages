// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package adminapi

// Refresh health on the staff surface: the detail screen's schedule editor
// (cookie + CSRF, audited as the person) and the estate read a digest uses.

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

func TestRefreshScheduleAndHealth(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.createPage(versions.CreatePageInput{Slug: "rh/northwind", Title: "Northwind"})
	e.deploy("rh/northwind", html(1), true)
	e.createPage(versions.CreatePageInput{Slug: "rh/contoso", Title: "Contoso"})
	if _, err := e.d.Exec(ctx, `UPDATE pages SET created_at = now() - interval '2 days' WHERE slug = 'rh/contoso'`); err != nil {
		t.Fatal(err)
	}
	// Live, no schedule, data twelve days old: hinted, never judged.
	e.createPage(versions.CreatePageInput{Slug: "rh/fabrikam", Title: "Fabrikam"})
	source := jsx.FormatTime(time.Now().Add(-12*24*time.Hour - time.Hour))
	e.deploy("rh/fabrikam", `<!doctype html><html><body><h1>Fabrikam</h1>`+
		`<script id="pages-data-schema" type="application/schema+json">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}</script>`+
		`<script id="pages-data" type="application/json">{"contract_version":1,"refreshed_at":"`+source+`","source_as_of":"`+source+`","data":{}}</script></body></html>`, true)
	const path = "/api/v1/admin/pages/rh/northwind/refresh-schedule"

	// The detail payload carries the block, unscheduled until someone sets it.
	detail := e.get("/api/v1/admin/pages/rh/northwind")
	expect(t, detail, 200, "detail")
	eq(t, detail.at("refresh_health", "health"), "unscheduled", "a new page is unscheduled")
	eq(t, detail.at("refresh_health", "unscheduled_stale"), false, "no data, no hint")
	stale := e.get("/api/v1/admin/pages/rh/fabrikam")
	eq(t, stale.at("refresh_health", "health"), "unscheduled", "the hinted page is still unscheduled")
	eq(t, stale.at("refresh_health", "unscheduled_stale"), true, "live, unscheduled, twelve days")
	eq(t, stale.at("refresh_health", "days_since_source"), 12, "days since source")
	if _, ok := detail.json.(map[string]any)["freshness"]; !ok {
		t.Fatal("the detail carries freshness beside refresh_health")
	}

	// Who may set it: staff with CSRF. Not without a session, not from
	// outside, not without the CSRF pair, and not with a bearer token on the
	// admin surface.
	body := map[string]any{"schedule": "30 14 * * 1-5", "owner": "Trader@Example.com"}
	expect(t, e.req("POST", path, opts{body: body}), 401, "no session")
	expect(t, e.req("POST", path, opts{cookie: e.outsideCk, csrf: true, origin: origin, body: body}), 403, "outsider")
	expect(t, e.req("POST", path, opts{cookie: e.adminCk, origin: origin, body: body}), 403, "CSRF is required")
	minted := must[*tokens.Minted](t)(tokens.Mint(ctx, e.d, tokens.MintParams{Label: "rh-agent", Scope: "deploy"}))
	expect(t, e.req("POST", path, opts{bearer: minted.Token, body: body}), 401, "bearer on the admin surface")
	eq(t, e.auditCount("set_refresh_schedule"), 0, "nothing was written")

	set := e.post(path, body)
	expect(t, set, 200, "set schedule")
	eq(t, set.at("slug"), "rh/northwind", "slug")
	eq(t, set.at("changed"), true, "changed")
	eq(t, set.at("refresh_health", "refresh_schedule"), "30 14 * * 1-5", "schedule")
	eq(t, set.at("refresh_health", "refresh_owner"), "trader@example.com", "owner is normalized")
	var actor, actorType string
	if err := e.d.QueryRow(ctx, `SELECT actor, actor_type FROM audit_log WHERE action = 'set_refresh_schedule' ORDER BY id DESC LIMIT 1`).Scan(&actor, &actorType); err != nil {
		t.Fatal(err)
	}
	eq(t, actor, admin, "the audit row names the person")
	eq(t, actorType, "user", "actor type")
	eq(t, e.get("/api/v1/admin/pages/rh/northwind").at("refresh_health", "refresh_schedule"), "30 14 * * 1-5", "the detail reads it back")
	// The index rows carry it too.
	row := find(e.get("/api/v1/admin/pages").at("pages"), "slug", "rh/northwind")
	eq(t, at(row, "refresh_health", "refresh_owner"), "trader@example.com", "index row")
	eq(t, at(find(e.get("/api/v1/admin/pages").at("pages"), "slug", "rh/fabrikam"), "refresh_health", "unscheduled_stale"), true, "index row hint")

	expectCode(t, e.post(path, map[string]any{"schedule": "30 14 * * MON-FRI"}), 400, "refresh_schedule_invalid", "names are refused")
	expectCode(t, e.post(path, map[string]any{"owner": "trader"}), 400, "refresh_owner_invalid", "not an address")
	expectCode(t, e.post(path, map[string]any{}), 400, "refresh_schedule_required", "nothing to set")
	expect(t, e.post("/api/v1/admin/pages/rh/nope/refresh-schedule", body), 404, "unknown page")

	// The estate read: only what needs a human by default, every page on
	// request. A page created two days ago on an every-minute schedule with
	// nothing ever recorded is overdue.
	e.post("/api/v1/admin/pages/rh/contoso/refresh-schedule", map[string]any{"schedule": "* * * * *"})
	report := e.get("/api/v1/admin/refresh-health")
	expect(t, report, 200, "refresh health")
	eq(t, pluck(report.at("pages"), "slug"), []string{"rh/contoso"}, "attention only")
	eq(t, report.at("pages", 0, "health"), "overdue", "health")
	eq(t, report.at("counts", "overdue"), 1, "counts")
	eq(t, report.at("counts", "unscheduled_stale"), 1, "the hinted page is counted, not listed")
	if report.str("generated_at") == "" || report.str("pages", 0, "health_since") == "" {
		t.Fatal(report.body)
	}
	everything := e.get("/api/v1/admin/refresh-health?needs_attention=false")
	eq(t, len(list(everything.at("pages"))), 3, "every page")
	eq(t, everything.at("pages", 2, "slug"), "rh/fabrikam", "the hinted page")
	eq(t, everything.at("pages", 2, "unscheduled_stale"), true, "row hint")
	eq(t, everything.at("pages", 2, "days_since_source"), 12, "row days since source")
	expectCode(t, e.get("/api/v1/admin/refresh-health?needs_attention=maybe"), 400, "bad_boolean", "bad flag")
	expect(t, e.req("GET", "/api/v1/admin/refresh-health", opts{}), 401, "the estate read needs a session")
	expect(t, e.req("GET", "/api/v1/admin/refresh-health", opts{cookie: e.outsideCk}), 403, "and an Elcano one")

	// Clearing: null stops the judgement; the owner stays.
	cleared := e.post(path, map[string]any{"schedule": nil})
	eq(t, cleared.at("refresh_health", "health"), "unscheduled", "cleared")
	eq(t, cleared.at("refresh_health", "refresh_owner"), "trader@example.com", "owner kept")
}
