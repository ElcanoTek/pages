// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/mcp.integration.js sections 19–26: managed data (canaries,
// dedupe, coverage, profiles and reconciliation, serialization, governance,
// audit, refresh checks, the staged data path), update prompts, the legacy
// refresh retirement, and data_update token scoping — plus issue #109's
// data_usage reporting.

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/audit"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

func isoAgo(d time.Duration) string { return jsx.FormatTime(time.Now().Add(-d)) }

func countLabelSchema() *jsonv.Object {
	return o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object", "additionalProperties", false,
		"required", arr("count", "label"),
		"properties", o("count", o("type", "integer", "minimum", num(0)), "label", o("type", "string")))
}

func managedPage(sourceAsOf string, data *jsonv.Object) string {
	if data == nil {
		data = o("count", num(1), "label", "initial")
	}
	return `<!doctype html><html><head><title>Managed</title></head><body>` +
		`<main data-layout="preserve-me"><h1>Managed dashboard</h1></main>` +
		`<script id="pages-data-schema" type="application/schema+json">` + jsonv.Stringify(countLabelSchema()) + `</script>` +
		`<script id="pages-data" type="application/json">` + jsonv.Stringify(o("contract_version", num(1), "refreshed_at", isoAgo(5*time.Hour), "source_as_of", sourceAsOf, "data", data)) + `</script>` +
		`<footer>immutable layout tail</footer></body></html>`
}

func campaignRows() []any {
	var out []any
	for day := 1; day <= 31; day++ {
		date := fmt.Sprintf("2026-03-%02d", day)
		if day <= 11 {
			spend := 97.161
			if day == 1 {
				spend = 412.37
			}
			out = append(out, o("date", date, "dealId", "example-deal-a", "spend", spend))
		}
		if day >= 2 {
			out = append(out, o("date", date, "dealId", "example-deal-b", "spend", 201.3373))
			out = append(out, o("date", date, "dealId", "example-deal-c", "spend", 233.0251))
			out = append(out, o("date", date, "dealId", "example-deal-d", "spend", 187.6417))
		}
	}
	return out
}

func filterRows(rows []any, keep func(date, deal string) bool) []any {
	var out []any
	for _, r := range rows {
		row := r.(*jsonv.Object)
		if keep(row.Value("date").(string), row.Value("dealId").(string)) {
			out = append(out, row)
		}
	}
	if out == nil {
		out = []any{}
	}
	return out
}

func warningCodes(v any) []string {
	var out []string
	for _, w := range toAnySlice(v) {
		out = append(out, strAt(w, "code"))
	}
	return out
}

// assertUsage checks issue #109's data_usage against the payload it reports.
func assertUsage(t *testing.T, result map[string]any, payload any) {
	t.Helper()
	usage, ok := result["data_usage"].(map[string]any)
	if !ok {
		t.Fatalf("no data_usage in %s", clipText(jsonText(result)))
	}
	want := datalimits.MeasureData(payload)
	eq(t, usage["payload_bytes"], want.PayloadBytes, "payload_bytes is the compact payload")
	eq(t, usage["limit_bytes"], datalimits.MaxDataBytes(), "limit_bytes is the managed-data limit in force")
	if numAt(usage, "envelope_bytes") < float64(want.EscapedPayloadBytes+datalimits.MinEnvelopeOverheadBytes) {
		t.Fatalf("envelope_bytes %v is below the escaped payload plus metadata", usage["envelope_bytes"])
	}
	eq(t, usage["headroom_bytes"], numAt(usage, "limit_bytes")-max(numAt(usage, "payload_bytes"), numAt(usage, "envelope_bytes")))
}

func TestManagedDataPromptsAndScopes(t *testing.T) {
	h := newHarness(t)
	h.data(h.call("deploy_page", o("slug", "mcpdemo", "title", "MCP Demo", "html", pageHTML(1), "publish", true)))

	// 19. Managed data uses the published immutable version as its template.
	source0, source1, source2, source3 := isoAgo(4*time.Hour), isoAgo(3*time.Hour), isoAgo(2*time.Hour), isoAgo(time.Hour)
	initialHTML := managedPage(source0, nil)
	dataV0 := strAt(h.data(h.call("deploy_page", o("slug", "mcpdata", "title", "Managed Data", "html", initialHTML))), "version", "id")
	initialResp := h.call("get_page_data", o("slug", "mcpdata"))
	initialData := h.data(initialResp)
	eq(t, jsonv.Stringify(ordered(t, initialResp).Value("schema")), jsonv.Stringify(countLabelSchema()))
	eq(t, dig(initialData, "envelope", "data"), map[string]any{"count": 1, "label": "initial"})
	eq(t, initialData["live_version_id"], dataV0)
	eq(t, initialData["version_is_live"], true)
	mustMatch(t, `^[0-9a-f]{64}$`, strAt(initialData, "data_sha256"))
	assertUsage(t, initialData, o("count", num(1), "label", "initial"))

	invalid := h.fails(h.call("update_page_data", o("slug", "mcpdata", "data", o("count", num(-1), "label", num(4)), "source_as_of", source1, "expected_version", dataV0)))
	eq(t, invalid.code(), "data_validation_failed")
	if len(toAnySlice(dig(invalid.data, "details", "validation_errors"))) < 1 {
		t.Fatal("validation errors are reported")
	}

	injected := o("count", num(2), "label", "</script><img src=x onerror=alert(1)>")
	canary := h.data(h.call("update_page_data", o("slug", "mcpdata", "data", injected, "source_as_of", source1, "expected_version", dataV0, "publish", false, "note", "canary")))
	eq(t, canary["published"], false)
	eq(t, canary["version_is_live"], false)
	eq(t, canary["live_version_id"], dataV0)
	assertUsage(t, canary, injected)
	canaryVersion := strAt(canary, "version", "id")
	canaryHTML := strAt(h.data(h.call("get_version", o("slug", "mcpdata", "version_id", canaryVersion))), "version", "html")
	parsedInitial, err := pagedata.ParseManagedHTML(initialHTML)
	if err != nil {
		t.Fatal(err)
	}
	parsedCanary, err := pagedata.ParseManagedHTML(canaryHTML)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, canaryHTML[:parsedCanary.DataBlock.ContentStart], initialHTML[:parsedInitial.DataBlock.ContentStart], "all bytes before managed data content are preserved")
	if !strings.HasSuffix(canaryHTML, initialHTML[parsedInitial.DataBlock.ContentEnd:]) {
		t.Fatal("all bytes after managed data content are preserved")
	}
	eq(t, jsonv.Stringify(parsedCanary.Data()), jsonv.Stringify(injected))
	mustNotMatch(t, `(?i)</script`, canaryHTML[parsedCanary.DataBlock.ContentStart:parsedCanary.DataBlock.ContentEnd])

	canaryRetry := h.data(h.call("update_page_data", o("slug", "mcpdata", "data", injected, "source_as_of", source1, "expected_version", dataV0, "publish", false)))
	eq(t, canaryRetry["deduped"], true)
	eq(t, dig(canaryRetry, "version", "id"), canaryVersion)
	eq(t, dig(canaryRetry, "envelope", "refreshed_at"), dig(canary, "envelope", "refreshed_at"))
	publishCanary := h.data(h.call("update_page_data", o("slug", "mcpdata", "data", injected, "source_as_of", source1, "expected_version", dataV0)))
	eq(t, publishCanary["deduped"], true)
	eq(t, publishCanary["published"], true)
	eq(t, dig(publishCanary, "version", "id"), canaryVersion)
	eq(t, publishCanary["version_is_live"], true)
	coverageAdvance := h.data(h.call("update_page_data", o("slug", "mcpdata", "data", injected, "source_as_of", source2, "expected_version", canaryVersion)))
	eq(t, coverageAdvance["deduped"], false, "new source coverage creates a version when metrics are unchanged")
	dataV2 := strAt(coverageAdvance, "version", "id")
	if dataV2 == canaryVersion {
		t.Fatal("a new version")
	}
	exactLive := h.data(h.call("update_page_data", o("slug", "mcpdata", "data", injected, "source_as_of", source2, "expected_version", dataV2)))
	eq(t, exactLive["deduped"], true)
	eq(t, exactLive["published"], false, "an exact live retry does not rewrite the pointer/audit row")
	eq(t, exactLive["version_is_live"], true)
	regression := h.fails(h.call("update_page_data", o("slug", "mcpdata", "data", o("count", num(3), "label", "regress"), "source_as_of", source1, "expected_version", dataV2)))
	eq(t, regression.code(), "source_regression")
	eq(t, dig(regression.data, "details", "current_source_as_of"), source2)
	eq(t, h.fails(h.call("update_page_data", o("slug", "mcpdata", "data", o("count", num(3), "label", "future"),
		"source_as_of", jsx.FormatTime(time.Now().Add(time.Hour)), "expected_version", dataV2))).code(), "source_in_future")
	eq(t, h.fails(h.call("update_page_data", o("slug", "mcpdata", "data", o("count", num(3), "label", "stale writer"), "source_as_of", source3, "expected_version", canaryVersion))).code(), "stale_version")

	// 19b. Issue #102: a payload can satisfy its schema and still be wrong.
	rowSchema := o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object", "additionalProperties", false, "required", arr("rows"),
		"properties", o("dataThrough", o("type", arr("string", "null")),
			"rows", o("type", "array", "items", o("type", "object", "additionalProperties", false, "required", arr("date", "dealId", "spend"),
				"properties", o("date", o("type", "string"), "dealId", o("type", "string"), "spend", o("type", "number"))))))
	profileHTML := `<!doctype html><html><head><title>Campaign</title></head><body><main>Rows</main>` +
		`<script type="application/schema+json" id="pages-data-schema">` + jsonv.Stringify(rowSchema) + `</script>` +
		`<script type="application/json" id="pages-data">` + jsonv.Stringify(o("contract_version", num(1), "refreshed_at", source0, "source_as_of", source0,
		"data", o("dataThrough", nil, "rows", arr()))) + `</script></body></html>`
	profileBase := h.data(h.call("deploy_page", o("slug", "mcp-profile", "title", "Profile", "html", profileHTML, "render_mode", "raw")))
	good := h.data(h.call("update_page_data", o("slug", "mcp-profile", "data", o("dataThrough", "2026-03-31", "rows", campaignRows()),
		"source_as_of", source1, "expected_version", dig(profileBase, "version", "id"))))
	rows := dig(good, "data_profile", "arrays", "rows")
	eq(t, dig(rows, "count"), 101)
	eq(t, dig(rows, "fields", "date", "min"), "2026-03-01")
	eq(t, dig(rows, "fields", "date", "max"), "2026-03-31")
	eq(t, dig(rows, "fields", "spend", "sum"), 20044.103)
	var deals []string
	for k := range dig(rows, "fields", "dealId", "values").(map[string]any) {
		deals = append(deals, k)
	}
	sort.Strings(deals)
	eq(t, deals, []string{"example-deal-a", "example-deal-b", "example-deal-c", "example-deal-d"})
	assertUsage(t, good, o("dataThrough", "2026-03-31", "rows", campaignRows()))

	// A caller's own numbers come back as numbers: id canonicalization stays
	// out of the caller's document.
	idSchema := o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object", "additionalProperties", false, "required", arr("rows"),
		"properties", o("rows", o("type", "array", "items", o("type", "object", "additionalProperties", false, "required", arr("campaign_id", "id", "spend"),
			"properties", o("campaign_id", o("type", "integer"), "id", o("type", "integer"), "spend", o("type", "number"))))))
	idHTML := `<!doctype html><html><head><title>Ids</title></head><body><main>Rows</main>` +
		`<script type="application/schema+json" id="pages-data-schema">` + jsonv.Stringify(idSchema) + `</script>` +
		`<script type="application/json" id="pages-data">` + envelopeJSON(o("rows", arr())) + `</script></body></html>`
	idBase := h.data(h.call("deploy_page", o("slug", "mcp-numeric-ids", "title", "Numeric ids", "html", idHTML, "render_mode", "raw", "publish", true)))
	idPayload := o("rows", arr(o("campaign_id", num(12345), "id", num(7), "spend", 10.5)))
	idWrite := h.data(h.call("update_page_data", o("slug", "mcp-numeric-ids", "data", idPayload, "source_as_of", "2026-08-05T00:00:00Z", "expected_version", dig(idBase, "version", "id"))))
	want := map[string]any{"rows": []any{map[string]any{"campaign_id": 12345, "id": 7, "spend": 10.5}}}
	eq(t, dig(idWrite, "envelope", "data"), want, "the write echoes the caller's numbers unchanged")
	idRead := h.data(h.call("get_page_data", o("slug", "mcp-numeric-ids")))
	eq(t, dig(idRead, "envelope", "data"), want, "and so does the read")
	if _, ok := dig(idRead, "envelope", "data", "rows", 0, "campaign_id").(float64); !ok {
		t.Fatal("campaign_id stays a number")
	}
	if _, ok := idRead["live_version_id"].(string); !ok {
		t.Fatal("Pages' own ids stay decimal strings")
	}
	roundTrip := h.data(h.call("update_page_data", o("slug", "mcp-numeric-ids", "data", dig(idRead, "envelope", "data"), "source_as_of", "2026-08-06T00:00:00Z", "expected_version", idRead["live_version_id"])))
	eq(t, roundTrip["version_is_live"], true, "re-writing what get_page_data returned must validate")
	eq(t, dig(roundTrip, "data_profile", "arrays", "rows", "fields", "campaign_id", "kind"), "number")

	var paths []string
	for _, w := range toAnySlice(good["data_warnings"]) {
		paths = append(paths, strAt(w, "code")+"@"+strAt(w, "path"))
	}
	eq(t, paths, []string{"coverage_unverified@rows"})
	mustMatch(t, `RECONCILE BEFORE SHARING`, strAt(good, "next_step"))
	eq(t, dig(good, "data_profile", "scalars", "dataThrough"), "2026-03-31")

	bad := h.data(h.call("update_page_data", o("slug", "mcp-profile", "data", o("dataThrough", "2026-03-31",
		"rows", filterRows(campaignRows(), func(date, deal string) bool { return date >= "2026-03-25" && deal != "example-deal-a" })),
		"source_as_of", source2, "expected_version", dig(good, "version", "id"))))
	warned := warningCodes(bad["data_warnings"])
	for _, code := range []string{"coverage_start_regressed", "dimension_values_missing", "row_count_dropped"} {
		if !slices.Contains(warned, code) {
			t.Fatalf("missing warning %s in %v", code, warned)
		}
	}
	eq(t, bad["version_is_live"], true)
	mustMatch(t, `RECONCILE BEFORE SHARING`, strAt(bad, "next_step"))
	mustMatch(t, `coverage_start_regressed`, strAt(bad, "next_step"))
	readBack := h.data(h.call("get_page_data", o("slug", "mcp-profile")))
	eq(t, dig(readBack, "data_profile", "arrays", "rows", "fields", "date", "min"), "2026-03-25")
	eq(t, dig(readBack, "data_profile", "arrays", "rows", "count"), 21)

	refused := h.fails(h.call("update_page_data", o("slug", "mcp-profile", "data", o("dataThrough", "2026-03-31",
		"rows", filterRows(campaignRows(), func(_, deal string) bool { return deal != "example-deal-a" })),
		"source_as_of", source3, "expected_version", dig(bad, "version", "id"),
		"expect", o("row_count", o("rows", num(101)), "totals", o("rows.spend", 20044.103)))))
	eq(t, refused.code(), "data_reconciliation_failed")
	eq(t, len(toAnySlice(dig(refused.data, "details", "mismatches"))), 2)
	eq(t, h.data(h.call("get_page_data", o("slug", "mcp-profile")))["live_version_id"], dig(bad, "version", "id"), "a refused reconciliation writes nothing at all")
	reconciled := h.data(h.call("update_page_data", o("slug", "mcp-profile", "data", o("dataThrough", "2026-03-31", "rows", campaignRows()),
		"source_as_of", source3, "expected_version", dig(bad, "version", "id"),
		"expect", o("row_count", o("rows", num(101)), "totals", o("rows.spend", 20044.103), "date_range", o("rows.date", arr("2026-03-01", "2026-03-31"))))))
	eq(t, reconciled["version_is_live"], true)
	eq(t, dig(reconciled, "data_profile", "arrays", "rows", "count"), 101)
	eq(t, reconciled["data_warnings"], []any{}, "declaring the window in expect withdraws the warning")

	sameRows := func() *jsonv.Object { return o("dataThrough", "2026-03-31", "rows", campaignRows()) }
	later := isoAgo(30 * time.Minute)
	noNewData := h.data(h.call("update_page_data", o("slug", "mcp-profile", "data", sameRows(), "source_as_of", later, "expected_version", dig(reconciled, "version", "id"))))
	eq(t, noNewData["deduped"], false)
	eq(t, noNewData["version_is_live"], true)
	eq(t, warningCodes(noNewData["data_warnings"]), []string{"data_unchanged"})
	mustMatch(t, `THIS REFRESH ADDED NO NEW DATA`, strAt(noNewData, "next_step"))
	mustMatch(t, `Do not describe this as a data update`, strAt(noNewData, "next_step"))
	mustNotMatch(t, `RECONCILE BEFORE SHARING`, strAt(noNewData, "next_step"))
	replay := h.data(h.call("update_page_data", o("slug", "mcp-profile", "data", sameRows(), "source_as_of", later, "expected_version", dig(noNewData, "version", "id"))))
	eq(t, replay["deduped"], true)
	eq(t, replay["data_warnings"], []any{})
	mustNotMatch(t, `ADDED NO NEW DATA`, strAt(replay, "next_step"))
	extendedRows := append(campaignRows(), o("date", "2026-04-01", "dealId", "example-deal-b", "spend", 12.5))
	extended := h.data(h.call("update_page_data", o("slug", "mcp-profile", "data", o("dataThrough", "2026-04-01", "rows", extendedRows),
		"source_as_of", isoAgo(15*time.Minute), "expected_version", dig(noNewData, "version", "id"))))
	eq(t, extended["data_warnings"], []any{})
	h.data(h.call("delete_page", o("slug", "mcp-profile")))

	// 20. Overlapping writers serialize on the page lock.
	var wg sync.WaitGroup
	overlap := make([]response, 2)
	for i, label := range []string{"overlap-a", "overlap-b"} {
		wg.Add(1)
		go func(i int, label string) {
			defer wg.Done()
			overlap[i] = h.call("update_page_data", o("slug", "mcpdata", "data", o("count", num(float64(4+i)), "label", label), "source_as_of", source3, "expected_version", dataV2))
		}(i, label)
	}
	wg.Wait()
	var winner map[string]any
	errorsSeen := 0
	for _, r := range overlap {
		if r.get("result", "isError") == true {
			errorsSeen++
			eq(t, h.fails(r).code(), "stale_version")
		} else {
			winner = h.data(r)
		}
	}
	eq(t, errorsSeen, 1)
	eq(t, winner["version_is_live"], true)
	winnerID := strAt(winner, "version", "id")

	// 21. Approval and takedown governance; unmanaged pages fail closed.
	eq(t, h.fails(h.call("get_page_data", o("slug", "mcpdemo"))).code(), "page_not_data_managed")
	gatedBase := h.data(h.call("deploy_page", o("slug", "mcpgateddata", "title", "Gated Managed Data", "html", managedPage(source0, nil))))
	h.exec("UPDATE pages SET require_approval = true WHERE slug = 'mcpgateddata'")
	gatedData := h.data(h.call("update_page_data", o("slug", "mcpgateddata", "data", o("count", num(2), "label", "pending"), "source_as_of", source1, "expected_version", dig(gatedBase, "version", "id"))))
	eq(t, gatedData["gated"], true)
	eq(t, dig(gatedData, "version", "status"), "pending")
	eq(t, gatedData["version_is_live"], false)
	eq(t, gatedData["live_version_id"], dig(gatedBase, "version", "id"))
	h.exec("UPDATE pages SET disabled = true WHERE slug = 'mcpdata'")
	eq(t, h.fails(h.call("update_page_data", o("slug", "mcpdata", "data", o("count", num(6), "label", "disabled"), "source_as_of", isoAgo(30*time.Minute), "expected_version", winnerID))).code(), "disabled_takedown")
	disabledRead := h.data(h.call("get_page_data", o("slug", "mcpdata")))
	eq(t, disabledRead["page_is_live"], false)
	eq(t, disabledRead["live_version_id"], winnerID, "published pointer is retained during takedown")
	h.exec("UPDATE pages SET disabled = false WHERE slug = 'mcpdata'")

	// 22. Structured-data identity is immutable; writes are attributed.
	if _, err := h.d.Exec(context.Background(), "UPDATE page_versions SET data_sha256 = repeat('0', 64) WHERE id = $1", winnerID); err == nil || !strings.Contains(strings.ToLower(err.Error()), "immutable") {
		t.Fatalf("data identity must be immutable: %v", err)
	}
	auditRows := h.query(`SELECT action, token_id, metadata FROM audit_log WHERE version_id = $1 AND action = 'data_update'`, winnerID)
	eq(t, len(auditRows), 1)
	eq(t, fmt.Sprint(auditRows[0]["token_id"]), h.tokenID.String())
	eq(t, dig(auditRows[0]["metadata"], "source_as_of"), source3)

	// 22b. A refresh that published nothing is recordable without a version.
	beforeCheck := h.data(h.call("get_page_data", o("slug", "mcpdata")))
	versionsBefore := h.count("SELECT count(*)::integer FROM page_versions WHERE page_id = (SELECT id FROM pages WHERE slug = 'mcpdata')")
	if beforeCheck["freshness"] == nil {
		t.Fatal("a managed page reports freshness")
	}
	eq(t, dig(beforeCheck, "freshness", "source_as_of"), dig(beforeCheck, "envelope", "source_as_of"))
	eq(t, dig(beforeCheck, "freshness", "last_check_outcome"), nil)
	eq(t, dig(beforeCheck, "freshness", "checked_at"), dig(beforeCheck, "freshness", "refreshed_at"))
	recorded := h.data(h.call("record_refresh_check", o("slug", "mcpdata", "outcome", "source_not_updated", "detail", "upstream max date unchanged",
		"source_as_of_seen", dig(beforeCheck, "envelope", "source_as_of"))))
	eq(t, dig(recorded, "freshness", "last_check_outcome"), "source_not_updated")
	eq(t, dig(recorded, "freshness", "days_since_check"), 0)
	eq(t, dig(recorded, "freshness", "source_as_of"), dig(beforeCheck, "freshness", "source_as_of"))
	afterCheck := h.data(h.call("get_page_data", o("slug", "mcpdata")))
	eq(t, afterCheck["data_sha256"], beforeCheck["data_sha256"])
	eq(t, afterCheck["live_version_id"], beforeCheck["live_version_id"])
	eq(t, dig(afterCheck, "freshness", "last_check_outcome"), "source_not_updated")
	eq(t, dig(afterCheck, "freshness", "last_check_detail"), "upstream max date unchanged")
	if versions.ParseTime(strAt(afterCheck, "freshness", "checked_at")).Before(versions.ParseTime(strAt(afterCheck, "freshness", "refreshed_at"))) {
		t.Fatal("checked_at moved past the last write")
	}
	eq(t, h.count("SELECT count(*)::integer FROM page_versions WHERE page_id = (SELECT id FROM pages WHERE slug = 'mcpdata')"), versionsBefore, "recording a check creates no version")
	var listed any
	for _, p := range toAnySlice(h.data(h.call("list_pages", o("limit", num(100))))["pages"]) {
		if strAt(p, "slug") == "mcpdata" {
			listed = p
		}
	}
	eq(t, dig(listed, "freshness", "last_check_outcome"), "source_not_updated")
	eq(t, dig(listed, "freshness", "source_as_of"), dig(afterCheck, "freshness", "source_as_of"))
	mustMatch(t, `(?i)outcome`, h.fails(h.call("record_refresh_check", o("slug", "mcpdata", "outcome", "probably_fine"))).text)
	eq(t, h.scalar("SELECT last_check_outcome FROM pages WHERE slug = 'mcpdata'"), "source_not_updated")
	checkAudit := h.query(`SELECT action, token_id, metadata FROM audit_log WHERE action = 'record_refresh_check' AND page_id = (SELECT id FROM pages WHERE slug = 'mcpdata')`)
	eq(t, len(checkAudit), 1)
	eq(t, fmt.Sprint(checkAudit[0]["token_id"]), h.tokenID.String())
	eq(t, dig(checkAudit[0]["metadata"], "outcome"), "source_not_updated")

	// 22c. Managed data has the staged path HTML has.
	stagedPayload := o("count", num(41), "label", "staged")
	stagedBytes := []byte(jsonv.Stringify(stagedPayload))
	beforeStaged := h.data(h.call("get_page_data", o("slug", "mcpdata")))
	dataTicket := h.stage("slug", "mcpdata", stagedBytes, "kind", "data")
	eq(t, dataTicket["target_kind"], "data")
	eq(t, dataTicket["template"], nil)
	mustMatch(t, `update_page_data_upload`, strAt(dataTicket, "next_step"))
	if _, ok := dataTicket["data_limits"].(map[string]any); !ok {
		t.Fatal("a data upload reports its limits")
	}
	dataTicketID := strAt(dataTicket, "upload_id")
	asPage := h.fails(h.call("deploy_page_upload", o("upload_id", dataTicketID, "expected_version", beforeStaged["live_version_id"])))
	eq(t, asPage.code(), "page_upload_target_mismatch")
	mustMatch(t, `update_page_data_upload`, strAt(asPage.data, "error"))
	stagedSource := jsx.FormatTime(versions.ParseTime(strAt(beforeStaged, "envelope", "source_as_of")).Add(time.Second))
	misreconciled := h.fails(h.call("update_page_data_upload", o("upload_id", dataTicketID, "slug", "mcpdata", "source_as_of", stagedSource,
		"expected_version", beforeStaged["live_version_id"], "expect", o("row_count", o("rows", num(12))))))
	eq(t, misreconciled.code(), "data_reconciliation_failed")
	eq(t, h.data(h.call("get_page_data", o("slug", "mcpdata")))["data_sha256"], beforeStaged["data_sha256"], "a refused reconciliation changes nothing")
	stagedArgs := func(extra ...any) *jsonv.Object {
		a := o("upload_id", dataTicketID, "slug", "mcpdata", "source_as_of", stagedSource, "expected_version", beforeStaged["live_version_id"])
		for i := 0; i+1 < len(extra); i += 2 {
			a.Set(extra[i].(string), extra[i+1])
		}
		return a
	}
	staged := h.data(h.call("update_page_data_upload", stagedArgs()))
	eq(t, staged["version_is_live"], true)
	eq(t, staged["upload_id"], dataTicketID)
	eq(t, dig(staged, "envelope", "data"), map[string]any{"count": 41, "label": "staged"})
	if staged["data_profile"] == nil {
		t.Fatal("the staged route profiles the payload like the inline one")
	}
	assertUsage(t, staged, stagedPayload)
	inlineEcho := h.data(h.call("update_page_data", o("slug", "mcpdata", "data", stagedPayload,
		"source_as_of", jsx.FormatTime(versions.ParseTime(stagedSource).Add(time.Second)), "expected_version", dig(staged, "version", "id"))))
	eq(t, inlineEcho["data_sha256"], staged["data_sha256"], "staged and inline writes agree byte-for-byte")
	eq(t, inlineEcho["schema_sha256"], staged["schema_sha256"])
	eq(t, inlineEcho["data_usage"], staged["data_usage"], "and report the same usage")
	afterSuccess := h.data(h.call("get_page_data", o("slug", "mcpdata")))
	eq(t, dig(afterSuccess, "freshness", "latest_outcome"), "updated")
	eq(t, dig(afterSuccess, "freshness", "latest_detail"), nil)
	eq(t, dig(afterSuccess, "freshness", "last_check_outcome"), "source_not_updated", "check history survives publication")
	eq(t, dig(afterSuccess, "freshness", "last_check_at"), dig(afterCheck, "freshness", "last_check_at"))
	stagedRetry := h.data(h.call("update_page_data_upload", stagedArgs()))
	eq(t, dig(stagedRetry, "version", "id"), dig(staged, "version", "id"), "exact retry replays the commit result")
	eq(t, stagedRetry["replayed"], true)
	eq(t, h.fails(h.call("update_page_data_upload", stagedArgs("note", "different options"))).code(), "page_upload_commit_conflict")

	htmlTicket := h.stage("slug", "mcpdata", []byte(pageHTML(400)))
	eq(t, h.fails(h.call("update_page_data_upload", o("upload_id", strAt(htmlTicket, "upload_id"), "slug", "mcpdata",
		"source_as_of", jsx.FormatTime(time.Now()), "expected_version", dig(inlineEcho, "version", "id")))).code(), "page_upload_target_mismatch")
	arrayTicket := h.stage("slug", "mcpdata", []byte("[1,2,3]"), "kind", "data")
	eq(t, h.fails(h.call("update_page_data_upload", o("upload_id", strAt(arrayTicket, "upload_id"), "slug", "mcpdata",
		"source_as_of", jsx.FormatTime(time.Now()), "expected_version", dig(inlineEcho, "version", "id")))).code(), "data_invalid")
	eq(t, h.fails(h.call("update_page_data_upload", o("upload_id", strAt(arrayTicket, "upload_id"), "slug", "mcpgateddata",
		"source_as_of", jsx.FormatTime(time.Now()), "expected_version", dig(inlineEcho, "version", "id")))).code(), "page_upload_target_mismatch")
	// A domain overflow cannot be rescued by moving the same data through
	// staging. The label is sized from the limit in force.
	overflow := h.fails(h.call("update_page_data", o("slug", "mcpdata", "data", o("count", num(1), "label", strings.Repeat("x", datalimits.MaxDataBytes()+1)),
		"source_as_of", jsx.FormatTime(time.Now()), "expected_version", dig(inlineEcho, "version", "id"))))
	mustMatch(t, `Changing upload transport cannot help`, strAt(overflow.data, "error"))
	chunkPayload := o("count", num(7), "label", "chunked")
	chunkBytes := []byte(jsonv.Stringify(chunkPayload))
	chunkStart := h.data(h.call("start_page_upload", o("slug", "mcpdata", "kind", "data", "total_bytes", num(float64(len(chunkBytes))), "content_sha256", shaHex(chunkBytes))))
	eq(t, chunkStart["target_kind"], "data")
	appended := h.data(h.call("append_page_upload", o("upload_id", chunkStart["upload_id"], "sequence", num(0), "chunk_base64", base64.StdEncoding.EncodeToString(chunkBytes))))
	eq(t, appended["target_kind"], "data", "an append must not forget what the upload is for")
	mustMatch(t, `update_page_data_upload`, strAt(appended, "next_step"))
	eq(t, appended["complete"], true)
	chunkPublished := h.data(h.call("update_page_data_upload", o("upload_id", chunkStart["upload_id"], "slug", "mcpdata",
		"source_as_of", jsx.FormatTime(versions.ParseTime(stagedSource).Add(2*time.Second)), "expected_version", dig(inlineEcho, "version", "id"))))
	eq(t, dig(chunkPublished, "envelope", "data"), map[string]any{"count": 7, "label": "chunked"})

	// 23. Prompt preparation is read-only and never creates a companion page.
	versionsBeforePrompts := h.count("SELECT count(*)::integer FROM page_versions")
	request := "Use yesterday's complete reporting export to update count and label."
	prepared := h.data(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", request, "update_type", "data")))
	eq(t, prepared["mode"], "managed_data")
	eq(t, prepared["recurring"], false)
	eq(t, prepared["live_version_id"], dig(chunkPublished, "version", "id"), "pinned to whatever is live now")
	eq(t, prepared["schema_sha256"], initialData["schema_sha256"])
	for _, p := range []string{`TARGET SLUG: mcpdata`, `mcp_pages_get_page_data`, `mcp_pages_update_page_data`, `(?i)never create another page, companion data page, or replacement slug`} {
		mustMatch(t, p, strAt(prepared, "prompt"))
	}
	mustMatch(t, `(?i)current conversation`, strAt(prepared, "next_step"))
	eq(t, prepared["prompt_sha256"], shaHex([]byte(strAt(prepared, "prompt"))))
	boundSources := arr(o("source_id", "reporting", "mcp_server", "reporting_mcp", "account", "acme", "required_tools", arr("daily_report"), "retrieval_instructions", "Complete days only."))
	bound := h.data(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", request, "update_type", "data", "sources", boundSources)))
	eq(t, jsonText(bound["sources"]), jsonText(mustAny(t, jsonv.Stringify(boundSources))), "parsed bindings are echoed back to the caller")
	mustMatch(t, `REQUIRED SOURCE BINDINGS \(exact; no substitutions\)`, strAt(bound, "prompt"))
	mustMatch(t, `- reporting: server reporting_mcp; account acme; tools daily_report — "Complete days only\."`, strAt(bound, "prompt"))
	mustMatch(t, `(?i)a source absent from that list is out of scope`, strAt(bound, "prompt"))
	if bound["prompt_sha256"] == prepared["prompt_sha256"] {
		t.Fatal("bindings change the prepared prompt")
	}
	eq(t, prepared["sources"], nil, "an unbound request reports no bindings")
	h.fails(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", request, "update_type", "data",
		"sources", arr(o("source_id", "x", "mcp_server", "y", "retrieval_instructions", "authenticate with Bearer abcdefghijklmnop")))))

	compat := h.data(h.call("get_page_refresh", o("slug", "mcpdata")))
	eq(t, compat["scheduling"], "user_owned")
	mustMatch(t, `configure_page_refresh`, strAt(compat, "next_step"))
	eq(t, h.data(h.call("configure_page_refresh", o("slug", "mcpdata", "instructions", request, "update_type", "data")))["prompt_sha256"], prepared["prompt_sha256"],
		"static-client alias is the canonical read-only operation")
	legacyPrepared := h.data(h.call("configure_page_refresh", o("slug", "mcpdata", "daily_at_utc", "08:00",
		"workflow", o("sources", arr(o("source_id", "reporting", "required_tools", arr("daily_report"))),
			"mapping", arr(o("target_path", "/count", "expression", "daily_report.count"))),
		"publish", true, "run_now", true)))
	eq(t, legacyPrepared["recurring"], true)
	mustNotMatch(t, `(?i)08:00|scheduler`, strAt(legacyPrepared, "prompt"))
	mustMatch(t, `(?i)did not honor run_now`, strAt(legacyPrepared, "next_step"))
	mustMatch(t, `(?i)Use prompt verbatim`, strAt(legacyPrepared, "next_step"))
	eq(t, h.fails(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", request, "recurring", true))).code(), "update_sources_required")
	recurring := h.data(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", request, "recurring", true,
		"sources", arr(o("source_id", "amazon_dsp", "mcp_server", "fastio_helpers", "path", "northwind/contoso/daily",
			"partition", o("by", "date", "format", "YYYY-MM-DD", "since", "source_as_of"), "required_tools", arr("list_partitions"))))))
	eq(t, recurring["mode"], "managed_data")
	eq(t, recurring["recurring"], true)
	contractOnly := h.data(h.call("get_page_data", o("slug", "mcpdata", "include_data", false)))
	complete := h.data(h.call("get_page_data", o("slug", "mcpdata")))
	eq(t, contractOnly["data_omitted"], true)
	if _, has := dig(contractOnly, "envelope").(map[string]any)["data"]; has {
		t.Fatal("a contract-only read carries no data")
	}
	if _, has := dig(complete, "envelope").(map[string]any)["data"]; !has {
		t.Fatal("a complete read carries data")
	}
	for _, field := range []string{"schema", "schema_sha256", "template_sha256", "data_sha256", "live_version_id", "data_profile", "freshness", "urls", "data_usage"} {
		eq(t, contractOnly[field], complete[field], "contract-only read preserves "+field)
	}
	eq(t, dig(contractOnly, "envelope", "source_as_of"), dig(complete, "envelope", "source_as_of"))
	prompt := strAt(recurring, "prompt")
	mustNotMatch(t, `(?i)scheduler`, prompt)
	mustMatch(t, `(?i)Use prompt verbatim`, strAt(recurring, "next_step"))
	mustMatch(t, `call set_page_refresh_schedule with this slug, the task's cadence as a five-field UTC cron`, strAt(recurring, "next_step"))
	eq(t, recurring["instructions_removed"], []any{})
	mustMatch(t, `PARTITIONED by date`, prompt)
	mustMatch(t, `never take only the newest`, prompt)
	eq(t, dig(recurring, "execution_requirements", "mcp_servers"), []any{"fastio_helpers", "pages"})
	eq(t, dig(recurring, "execution_requirements", "model_required"), true)
	mustNotMatch(t, `EXPECTED SCHEMA SHA-256`, prompt)
	eq(t, recurring["schema_sha256"], prepared["schema_sha256"])
	lines := strings.Split(prompt, "\n")
	reqLine := slices.Index(lines, "EXECUTION REQUIREMENTS (JSON):")
	eq(t, mustAny(t, lines[reqLine+1]), dig(recurring, "execution_requirements"), "copying only the prompt retains the scheduling prerequisites")
	if len(jsonText(dig(complete, "envelope", "data"))) > 20000 {
		t.Fatal("fixture payload is small")
	}
	eq(t, dig(recurring, "execution_requirements", "roster"), "required_tools_only")
	eq(t, dig(recurring, "execution_requirements", "completion"), map[string]any{
		"any_succeeded": []any{"mcp_pages_record_refresh_check", "mcp_pages_update_page_data", "mcp_pages_update_page_data_upload"},
		"blocked_when":  map[string]any{"tool": "mcp_pages_record_refresh_check", "argument": "outcome", "in": []any{"blocked", "failed", "source_unreachable"}, "detail_argument": "detail"},
	})
	eq(t, dig(recurring, "execution_requirements", "serialization_key"), "pages:mcpdata")
	for _, tool := range []string{"mcp_pages_update_page_data", "mcp_pages_update_page_data_upload", "mcp_pages_start_page_upload", "mcp_pages_append_page_upload"} {
		if !slices.Contains(toStrings(dig(recurring, "execution_requirements", "required_tools")), tool) {
			t.Fatalf("required_tools lacks %s", tool)
		}
	}
	mustNotMatch(t, `COMMIT TOOL:`, prompt)
	mustMatch(t, `source_not_updated: Call mcp_pages_record_refresh_check once`, prompt)
	mustMatch(t, `incompatible grain, required fields, source identity or mappings are blocked`, prompt)
	mustMatch(t, `UPDATE BRANCH ONLY`, prompt)
	eq(t, h.data(h.call("get_page_data", o("slug", "mcpdata")))["live_version_id"], recurring["live_version_id"], "preparing a recurring prompt never writes a version")
	adaptive := h.data(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", "Update the totals and improve the summary card if needed.")))
	eq(t, adaptive["mode"], "adaptive")
	mustMatch(t, `Classify USER REQUEST before writing`, strAt(adaptive, "prompt"))
	migration := h.data(h.call("prepare_dashboard_update", o("slug", "mcpdemo", "instructions", "Replace revenue with the latest finance export.", "update_type", "data")))
	eq(t, migration["mode"], "migration_required")
	eq(t, migration["schema_sha256"], nil)
	mustMatch(t, `MANAGED-DATA MIGRATION REQUIRED`, strAt(migration, "prompt"))
	mustMatch(t, `(?i)Never create a new slug or companion data page`, strAt(migration, "prompt"))
	layout := h.data(h.call("prepare_dashboard_update", o("slug", "mcpdemo", "instructions", "Add a compact date selector above the chart.", "update_type", "layout", "publish", false)))
	eq(t, layout["mode"], "full_page")
	mustMatch(t, `PUBLISH: false`, strAt(layout, "prompt"))
	mustMatch(t, `mcp_pages_deploy_page_upload`, strAt(layout, "prompt"))
	eq(t, h.fails(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", "Use token pgs_1234567890abcdefghijklmnop to update it.", "update_type", "data"))).code(), "update_credentials_forbidden")
	eq(t, h.fails(h.call("prepare_dashboard_update", o("slug", "mcpdata", "instructions", "Redesign the cards daily.", "recurring", true, "update_type", "layout"))).code(), "recurring_layout_forbidden")
	eq(t, h.fails(h.call("prepare_dashboard_update", o("slug", "does-not-exist", "instructions", "Update it."))).code(), "page_not_found")
	eq(t, h.count("SELECT count(*)::integer FROM page_versions"), versionsBeforePrompts, "prompt preparation never mutates page versions")
	eq(t, h.count("SELECT count(*)::integer FROM page_refreshes"), 0)

	// 24. The forward migration makes legacy Pages/MOC refresh state inert.
	ctx := context.Background()
	runner := h.mint("retired-refresh-runner", tokens.ScopeDataUpdate, "mcpdata")
	legacyID := h.scalar(`INSERT INTO page_refreshes
	     (page_id, status, daily_at_utc, workflow, prompt, schema_sha256, publish, target_node_name, runtime_token_id, next_run_at)
	   SELECT id, 'active', '08:00', '{}'::jsonb, 'legacy prompt', $1, true, 'legacy-runner-*', $2, now()
	     FROM pages WHERE slug = 'mcpdata' AND deleted_at IS NULL
	   RETURNING id`, strAt(initialData, "schema_sha256"), runner.ID)
	h.exec(`INSERT INTO page_refresh_runs (refresh_id, kind, scheduled_for, prompt, target_node_name, run_key, status, next_attempt_at)
	        VALUES ($1, 'manual', now(), 'queued', 'legacy-runner-*', $2, 'queued', now()),
	               ($1, 'scheduled', now() + interval '1 minute', 'failed', 'legacy-runner-*', $3, 'error', now()),
	               ($1, 'manual', now() + interval '2 minutes', 'sent', 'legacy-runner-*', $4, 'dispatched', now())`,
		legacyID, "legacy-"+randomHex(), "legacy-"+randomHex(), "legacy-"+randomHex())
	retirement, err := fs.ReadFile(pages.Migrations(), "010_retire_moc_refresh_dispatch.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		h.exec(string(retirement))
	}
	retired := h.query("SELECT status, last_error FROM page_refreshes WHERE id = $1", legacyID)[0]
	eq(t, retired["status"], "paused")
	mustMatch(t, `(?i)user-owned scheduler`, fmt.Sprint(retired["last_error"]))
	var runStatuses []string
	for _, r := range h.query("SELECT status FROM page_refresh_runs WHERE refresh_id = $1 ORDER BY scheduled_for", legacyID) {
		runStatuses = append(runStatuses, fmt.Sprint(r["status"]))
	}
	eq(t, runStatuses, []string{"cancelled", "cancelled", "dispatched"})
	verified, err := tokens.Verify(ctx, h.d, runner.Token)
	if err != nil || verified == nil {
		t.Fatalf("verify: %v", err)
	}
	eq(t, verified.AllowedSlugs, []string{})

	// 25. data_update tokens see and call only the data tools, on exact
	// granted slugs. REST and every other tool fail closed.
	scoped := h.mint("daily-mcpdata", tokens.ScopeDataUpdate, "mcpdata")
	rowsList, err := tokens.List(ctx, h.d)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rowsList {
		if r.ID == scoped.ID {
			eq(t, r.AllowedSlugs, []string{"mcpdata"})
		}
	}
	scopedInit := h.initialize(protocol, rpcOpts{reqOpts: reqOpts{token: scoped.Token}})
	mustMatch(t, `(?i)restricted to Pages managed-data automation`, strAt(scopedInit.json, "result", "instructions"))
	dataScopeTools := []string{"get_page_data", "record_refresh_check", "update_page_data"}
	var scopedNames []string
	for _, tool := range toAnySlice(h.rpc("tools/list", o(), rpcOpts{reqOpts: reqOpts{token: scoped.Token}}).get("result", "tools")) {
		scopedNames = append(scopedNames, strAt(tool, "name"))
	}
	sort.Strings(scopedNames)
	eq(t, scopedNames, dataScopeTools)
	eq(t, dig(h.data(h.call("get_page_data", o("slug", "mcpdata"), as(scoped.Token))), "page", "slug"), "mcpdata")
	eq(t, h.fails(h.call("get_page_data", o("slug", "mcpgateddata"), as(scoped.Token))).code(), "slug_not_allowed")
	eq(t, h.fails(h.call("record_refresh_check", o("slug", "mcpgateddata", "outcome", "source_not_updated"), as(scoped.Token))).code(), "slug_not_allowed")
	h.data(h.call("record_refresh_check", o("slug", "mcpdata", "outcome", "blocked"), as(scoped.Token)))
	for _, name := range expectedTools {
		if slices.Contains(dataScopeTools, name) {
			continue
		}
		denied := h.call(name, o(), as(scoped.Token))
		eq(t, denied.status, 200, name+" denial stays on the MCP result channel")
		eq(t, denied.get("result", "isError"), true, name+" denied for data_update token")
	}
	rest := h.request("GET", "/api/v1/pages/mcpdata", reqOpts{token: scoped.Token})
	eq(t, rest.status, 403)
	eq(t, rest.get("code"), "token_scope_denied")
	revoked, err := tokens.Revoke(ctx, h.d, int64(scoped.ID))
	if err != nil || !revoked {
		t.Fatalf("revoke: %v %v", revoked, err)
	}
	eq(t, h.rpc("ping", o(), rpcOpts{reqOpts: reqOpts{token: scoped.Token}}).status, 401)
	var legacyNames []string
	for _, tool := range toAnySlice(h.rpc("tools/list", o(), rpcOpts{}).get("result", "tools")) {
		legacyNames = append(legacyNames, strAt(tool, "name"))
	}
	sort.Strings(legacyNames)
	eq(t, legacyNames, expectedTools)

	// 26. Grants bind to page IDENTITY, not slug text (issue #50).
	rebind := h.mint("rebind-watch", tokens.ScopeDataUpdate, "grantrebind")
	eq(t, h.fails(h.call("get_page_data", o("slug", "grantrebind"), as(rebind.Token))).code(), "page_not_found")
	h.data(h.call("deploy_page", o("slug", "grantrebind", "title", "Rebind", "html", pageHTML(30))))
	eq(t, h.fails(h.call("get_page_data", o("slug", "grantrebind"), as(rebind.Token))).code(), "page_not_data_managed", "grant lazily bound to the first live page")
	h.data(h.call("delete_page", o("slug", "grantrebind")))
	eq(t, h.fails(h.call("get_page_data", o("slug", "grantrebind"), as(rebind.Token))).code(), "page_not_found")
	h.data(h.call("deploy_page", o("slug", "grantrebind", "title", "Rebind Reborn", "html", pageHTML(31))))
	eq(t, h.fails(h.call("get_page_data", o("slug", "grantrebind"), as(rebind.Token))).code(), "slug_not_allowed", "delete→recreate must NOT re-arm the stale grant")
	keep := h.mint("keep-watch", tokens.ScopeDataUpdate, "grantkeep")
	h.data(h.call("deploy_page", o("slug", "grantkeep", "title", "Keep", "html", pageHTML(32))))
	eq(t, h.fails(h.call("get_page_data", o("slug", "grantkeep"), as(keep.Token))).code(), "page_not_data_managed")
	h.data(h.call("delete_page", o("slug", "grantkeep")))
	if _, err := versions.RestorePage(ctx, h.d, "grantkeep", versions.Actor{Actor: "t@elcanotek.com", ActorType: audit.User}); err != nil {
		t.Fatal(err)
	}
	eq(t, h.fails(h.call("get_page_data", o("slug", "grantkeep"), as(keep.Token))).code(), "page_not_data_managed", "delete→restore keeps the same page row")
}

func mustAny(t *testing.T, text string) any {
	t.Helper()
	var v any
	if err := jsonUnmarshal(text, &v); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return v
}

func randomHex() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
