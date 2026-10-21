// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Port of test/page-data-read.test.js (the get_page_data half): the three read
// sizes, the output schema's per-mode shape, export URLs bound to the calling
// token, and the summary's bounds. No database: the read is shaped from what
// versions.GetPageData returns for a real published page, built through the
// same parser. The data_usage field (issue #109) is the one reviewed
// addition to each read.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/dataexport"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/pageurls"
	"github.com/ElcanoTek/pages/internal/versions"
)

const readSourceAsOf = "2026-09-21T00:00:00.000Z"

func readEnv(t *testing.T) {
	t.Helper()
	t.Setenv("RAW_TOKEN_SECRET", "test-secret-do-not-use-in-prod")
	t.Setenv("DASHBOARD_ORIGIN", "")
	t.Setenv("CONTENT_ORIGIN", "")
	t.Setenv("DASHBOARD_HOST", "pages.elcanotek.com")
	t.Setenv("CONTENT_HOST", "elcano-pages.com")
}

func scriptBlock(id string, value any, typ string) string {
	return fmt.Sprintf(`<script id="%s" type="%s">%s</script>`, id, typ, jsonv.Stringify(value))
}

func isoDay(base time.Time, days int) string {
	return base.AddDate(0, 0, days).Format("2006-01-02")
}

// tupleFixture is a synthetic tuple-row payload near the Node release's 1 MB
// limit: a deal registry and ~990 KB of rows.
func tupleFixture() (*jsonv.Object, *jsonv.Object) {
	deals := make([]any, 40)
	for i := range deals {
		deals[i] = arr(fmt.Sprintf("d%d", i), fmt.Sprintf("Northwind_SSP_Contoso_%d_Fabrikam_Display_Q3_2026_Open_Web_US_Standard", i))
	}
	var rows []any
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, size := 0, 0; size < 985_000; i++ {
		row := arr(fmt.Sprintf("d%d", i%40), isoDay(base, i%600), 1000.25+float64(i%997), 310.5+float64(i%89), 12.75, float64(150000+i), float64(300+i%700))
		rows = append(rows, row)
		size += len(jsonv.Stringify(row)) + 1
	}
	schema := o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object", "additionalProperties", false,
		"required", arr("dataThrough", "lastRefreshed", "sourceDetail", "deals", "rows"),
		"properties", o(
			"dataThrough", o("type", "string"),
			"lastRefreshed", o("type", "string"),
			"sourceDetail", o("type", "string"),
			"deals", o("type", "array", "items", o("type", "array", "items", o("type", "string"))),
			"rows", o("type", "array", "items", o("type", "array", "minItems", num(7), "maxItems", num(7))),
		))
	data := o("dataThrough", "2026-09-20", "lastRefreshed", "2026-09-21T06:02:11.000Z",
		"sourceDetail", "Canonical SSP history from the Northwind storage export, Index Exchange and Magnite, normalized to ET.",
		"deals", deals, "rows", rows)
	return schema, data
}

func objectFixture() (*jsonv.Object, *jsonv.Object) {
	var daily []any
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, size := 0, 0; size < 900_000; i++ {
		row := o("date", isoDay(base, i%260), "campaign", fmt.Sprintf("Contoso %d", i%12), "placement", fmt.Sprintf("Fabrikam placement %d", i%300),
			"dsp", []string{"Northwind", "Contoso"}[i%2], "impressions", float64(1000+i), "clicks", float64(i%97), "spend", 12.5+float64(i%31), "conversions", float64(i%7))
		daily = append(daily, row)
		size += len(jsonv.Stringify(row)) + 1
	}
	weekly := make([]any, 40)
	for i := range weekly {
		weekly[i] = o("week_start", fmt.Sprintf("2026-%02d-0%d", 1+i%9, 1+i%7), "spend", float64(i)*10.5)
	}
	flights := make([]any, 12)
	for i := range flights {
		flights[i] = o("flight", fmt.Sprintf("F%d", i), "starts", "2026-01-05", "ends", "2026-12-20", "budget", float64(5000+i))
	}
	schema := o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object")
	return schema, o("dataThrough", "2026-09-18", "note", "Northwind all-channel delivery", "daily", daily, "weekly", weekly, "flights", flights, "totals", o("spend", 99.5))
}

func mustTime(s string) jsx.Time { return jsx.NewTime(versions.ParseTime(s)) }

func readFreshness() *versions.Freshness {
	detail := "upstream max date still 2026-09-20; the storage export was re-uploaded without a new day"
	outcome := "source_not_updated"
	one, two := int64(1), int64(2)
	return &versions.Freshness{
		LastCheckAt: mustTime("2026-09-21T06:05:00.000Z"), LatestOutcome: &outcome, LatestDetail: &detail,
		LatestSourceAsOf: mustTime("2026-09-20T00:00:00.000Z"), SourceAsOf: mustTime(readSourceAsOf),
		RefreshedAt: mustTime("2026-09-20T06:02:11.000Z"), CheckedAt: mustTime("2026-09-21T06:05:00.000Z"),
		LastCheckOutcome: &outcome, LastCheckDetail: &detail, LastCheckSourceAsOf: mustTime("2026-09-20T00:00:00.000Z"),
		DaysSinceSource: &one, DaysSinceRefresh: &two, DaysSinceCheck: &one,
	}
}

// publishedView is exactly what versions.GetPageData returns for a published
// managed page.
func publishedView(t *testing.T, schema, data *jsonv.Object, fresh *versions.Freshness) *versions.PageDataView {
	t.Helper()
	envelope := o("contract_version", num(1), "refreshed_at", "2026-09-20T06:02:11.000Z", "source_as_of", readSourceAsOf, "data", data)
	html := "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind</h1>" +
		scriptBlock("pages-data-schema", schema, "application/schema+json") + scriptBlock("pages-data", envelope, "application/json") + "</body></html>"
	m, err := pagedata.ParseManagedHTML(html)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(html))
	ws := "Northwind"
	return &versions.PageDataView{
		Page: versions.Page{ID: 17, Slug: "northwind/overview", Title: "Northwind Client Overview", WorkspaceID: 3, WorkspaceName: &ws,
			ThemeName: "flag", PublishedVersionID: 42, CreatedAt: mustTime("2026-01-02T00:00:00.000Z"), UpdatedAt: mustTime("2026-09-20T06:02:11.000Z"), HasPassword: true},
		Version: versions.Version{ID: 42, PageID: 17, ContentSHA256: hex.EncodeToString(sum[:]), Status: "approved", RenderMode: "raw",
			Author: "refresh-agent", Source: "mcp", CreatedAt: mustTime("2026-09-20T06:02:11.000Z")},
		Schema: m.Schema, Envelope: m.Envelope, DataSHA256: m.DataSHA256, SchemaSHA256: m.SchemaSHA256, TemplateSHA256: m.TemplateSHA256,
		DataProfile: pagedata.ProfileData(m.Envelope.Value("data")), Freshness: fresh, Managed: m,
	}
}

// read is the tool's read path without the database: arguments parsed by the
// input schema, the read shaped, JSON round-tripped and held to the output
// schema.
func read(t *testing.T, r *versions.PageDataView, args *jsonv.Object, tokenID jsx.ID) *jsonv.Object {
	t.Helper()
	def := currentCatalog().byName["get_page_data"]
	full := o("slug", r.Page.Slug)
	args.Range(func(k string, v any) bool { full.Set(k, v); return true })
	parsed, err := def.input.Parse(full)
	if err != nil {
		t.Fatal(err)
	}
	a := parsed.(*jsonv.Object)
	detail, _ := argStr(a, "detail")
	out, err := pageDataRead(r, detail, !argBoolOr(a, "include_data", true), tokenID)
	if err != nil {
		t.Fatal(err)
	}
	v, err := JSONValue(out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := def.output.Parse(v); err != nil {
		t.Fatalf("output schema: %v", err)
	}
	return v.(*jsonv.Object)
}

func size(v any) int { return len(jsonv.Stringify(v)) }

// legacy is a read without the #109 field, which the byte bounds below were
// written against.
func legacy(v *jsonv.Object) *jsonv.Object { return without(v, "data_usage") }

func TestGetPageDataSummaryFitsTheContract(t *testing.T) {
	readEnv(t)
	schema, data := tupleFixture()
	tuple := publishedView(t, schema, data, readFreshness())
	if size(data) <= 980_000 {
		t.Fatal("the fixture is a near-limit payload")
	}
	full := read(t, tuple, o(), 9)
	summary := read(t, tuple, o("detail", "summary"), 9)
	if n := size(legacy(summary)); n > 2048 {
		t.Fatalf("summary is %d bytes", n)
	}
	if size(full) <= 500*size(summary) {
		t.Fatal("the summary is a small fraction of the full read")
	}
	eq(t, summary.Value("detail"), "summary")
	eq(t, jsonv.Stringify(summary.Value("page")), `{"slug":"northwind/overview","title":"Northwind Client Overview","disabled":false,"require_approval":false}`)
	eq(t, summary.Value("live_version_id"), "42")
	for _, k := range []string{"data_sha256", "schema_sha256", "template_sha256"} {
		eq(t, summary.Value(k), full.Value(k))
	}
	eq(t, jsonv.Stringify(summary.Value("envelope")), `{"contract_version":1,"refreshed_at":"2026-09-20T06:02:11.000Z","source_as_of":"2026-09-21T00:00:00.000Z"}`)
	eq(t, jsonv.Stringify(summary.Value("freshness")), jsonv.Stringify(readFreshness()))
	rowCount := len(data.Value("rows").([]any))
	eq(t, jsonv.Stringify(summary.Value("coverage_profile")),
		fmt.Sprintf(`{"arrays":{"deals":{"count":40,"fields":{}},"rows":{"count":%d,"fields":{}}},"scalars":{"dataThrough":"2026-09-20","lastRefreshed":"2026-09-21T06:02:11.000Z"}}`, rowCount))
	for _, absent := range []string{"schema", "version", "data_profile", "urls", "exports", "data_omitted"} {
		if summary.Has(absent) {
			t.Fatalf("summary carries %s", absent)
		}
	}
	if summary.Value("envelope").(*jsonv.Object).Has("data") {
		t.Fatal("summary envelope carries data")
	}
	mustMatch(t, `detail='export'`, summary.Value("next_step").(string))
	// Issue #109: the summary says how close the payload is to the limit.
	usage := summary.Value("data_usage").(*jsonv.Object)
	eq(t, usage.Value("payload_bytes"), size(data))
	eq(t, jsonv.Stringify(usage), jsonv.Stringify(full.Value("data_usage")), "every read reports the same usage")

	oschema, odata := objectFixture()
	objects := publishedView(t, oschema, odata, readFreshness())
	objectSummary := read(t, objects, o("detail", "summary"), 9)
	if n := size(legacy(objectSummary)); n > 2048 {
		t.Fatalf("object-row summary is %d bytes", n)
	}
	daily := objectSummary.Value("coverage_profile").(*jsonv.Object).Value("arrays").(*jsonv.Object).Value("daily").(*jsonv.Object)
	eq(t, daily.Value("count"), len(odata.Value("daily").([]any)))
	fields := daily.Value("fields").(*jsonv.Object)
	eq(t, fields.Keys(), []string{"date"})
	date := fields.Value("date").(*jsonv.Object)
	eq(t, date.Value("kind"), "date")
	eq(t, date.Value("min"), "2026-01-01")
	if _, ok := date.Value("distinct").(float64); !ok {
		t.Fatal("distinct is a number")
	}
	eq(t, objectSummary.Value("coverage_profile").(*jsonv.Object).Value("arrays").(*jsonv.Object).Value("flights").(*jsonv.Object).Value("fields").(*jsonv.Object).Keys(), []string{"starts", "ends"})
	eq(t, jsonv.Stringify(objectSummary.Value("coverage_profile").(*jsonv.Object).Value("scalars")), `{"dataThrough":"2026-09-18"}`)
	mustNotMatch(t, `"sum"|"values"|Fabrikam placement`, jsonv.Stringify(objectSummary))
}

func TestGetPageDataSummaryClipsLongDetails(t *testing.T) {
	readEnv(t)
	detail := strings.Repeat("x", 500)
	fresh := readFreshness()
	fresh.LatestDetail, fresh.LastCheckDetail = &detail, &detail
	schema, data := tupleFixture()
	r := publishedView(t, schema, data, fresh)
	summary := read(t, r, o("detail", "summary"), 9)
	if n := size(legacy(summary)); n > 2048 {
		t.Fatalf("worst-case summary is %d bytes", n)
	}
	f := summary.Value("freshness").(*jsonv.Object)
	clipped := f.Value("latest_detail").(string)
	eq(t, len([]rune(clipped)), 160)
	if !strings.HasSuffix(clipped, "…") {
		t.Fatal("a clipped detail ends with an ellipsis")
	}
	eq(t, f.Value("checked_at"), "2026-09-21T06:05:00.000Z")
	eq(t, read(t, r, o(), 9).Value("freshness").(*jsonv.Object).Value("latest_detail"), detail, "a full read keeps the complete line")
}

func TestGetPageDataFullReadKeepsItsShape(t *testing.T) {
	readEnv(t)
	schema, data := tupleFixture()
	r := publishedView(t, schema, data, readFreshness())
	snapshot := func(omitData bool) string {
		coverage := without(r.Envelope, "data")
		out := o("page", r.Page, "version", r.Version, "schema", r.Schema, "data_sha256", r.DataSHA256, "schema_sha256", r.SchemaSHA256,
			"template_sha256", r.TemplateSHA256, "data_profile", r.DataProfile, "freshness", r.Freshness,
			"version_is_live", true, "page_is_live", true, "live_version_id", "42", "urls", pageurls.For(r.Page.Slug))
		if omitData {
			out.Set("data_omitted", true)
		} else {
			coverage.Set("data", r.Envelope.Value("data"))
		}
		out.Set("envelope", coverage)
		v, _ := JSONValue(out)
		return jsonv.Stringify(v)
	}
	for _, c := range []struct {
		args *jsonv.Object
		omit bool
	}{{o(), false}, {o("detail", "full"), false}, {o("include_data", false), true}, {o("detail", "full", "include_data", false), true}} {
		got := read(t, r, c.args, 9)
		keys := got.Keys()
		eq(t, keys[len(keys)-1], "data_usage", "issue #109's field is appended after every existing key")
		eq(t, jsonv.Stringify(legacy(got)), snapshot(c.omit), "the default read is byte-for-byte what it was before detail existed")
	}
}

func TestGetPageDataOutputSchemaHoldsEachMode(t *testing.T) {
	readEnv(t)
	schema, data := objectFixture()
	r := publishedView(t, schema, data, readFreshness())
	out := currentCatalog().byName["get_page_data"].output
	full := read(t, r, o(), 9)
	summary := read(t, r, o("detail", "summary"), 9)
	exported := read(t, r, o("detail", "export"), 9)
	envelope, _ := toValue(r.Envelope)
	page, _ := toValue(r.Page)
	profile, _ := toValue(r.DataProfile)
	rawSchema, _ := toValue(r.Schema)
	for name, bad := range map[string]*jsonv.Object{
		"a full read cannot drop data_profile": without(full, "data_profile"),
		"a full read has no next_step":         withKey(full, "next_step", "x"),
		"a summary never carries the schema":   withKey(summary, "schema", rawSchema),
		"…or the rows":                         withKey(summary, "envelope", envelope),
		"a summary returns the brief page":     withKey(summary, "page", page),
		"an export carries no profile":         withKey(exported, "data_profile", profile),
		"an export carries its URLs":           without(exported, "exports"),
		"full reads never carry the marker":    withKey(full, "coverage_truncated", true),
	} {
		if _, err := out.Parse(bad); err == nil {
			t.Errorf("%s: the output schema accepted it", name)
		}
	}
	advertised := out.JSONSchema(ioOutput)
	eq(t, advertised.Value("type"), "object")
	required := toStrings(advertised.Value("required"))
	slices.Sort(required)
	eq(t, required, []string{"data_sha256", "envelope", "live_version_id", "page", "schema_sha256", "template_sha256"})
}

func withKey(v *jsonv.Object, key string, value any) *jsonv.Object {
	c := v.Clone()
	c.Set(key, value)
	return c
}

func TestGetPageDataExportIsBoundToTheCaller(t *testing.T) {
	readEnv(t)
	schema, data := objectFixture()
	r := publishedView(t, schema, data, readFreshness())
	exported := read(t, r, o("detail", "export"), 9)
	eq(t, exported.Value("detail"), "export")
	exports := exported.Value("exports").(*jsonv.Object)
	eq(t, exports.Value("version_id"), "42")
	u, err := url.Parse(exports.Value("data_url").(string))
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Split(u.Path, "/")[2]
	for part, key := range map[string]string{"schema": "schema_url", "data": "data_url", "envelope": "envelope_url"} {
		eq(t, exports.Value(key), "https://pages.elcanotek.com/export/"+token+"/"+part+".json", "served from the dashboard host only")
	}
	claims := dataexport.Verify(token)
	if claims == nil {
		t.Fatal("the export token verifies")
	}
	eq(t, []string{claims.PageID, claims.VersionID, claims.TokenID}, []string{"17", "42", "9"})
	if time.Until(versions.ParseTime(exports.Value("expires_at").(string))) > dataexport.ExportTTLSeconds*time.Second {
		t.Fatal("expires within the export TTL")
	}
	if exported.Value("envelope").(*jsonv.Object).Has("data") {
		t.Fatal("an export carries no rows")
	}
	next := exported.Value("next_step").(string)
	mustMatch(t, `download_url`, next)
	mustMatch(t, `Download schema_url and data_url once each`, next)
	mustMatch(t, `(?i)never share`, next)
	if _, err := pageDataRead(r, "export", false, 0); err == nil || !strings.Contains(err.Error(), "tokenId is missing") {
		t.Fatalf("no token, no URL: %v", err)
	}
}

func TestGetPageDataSummaryStaysBoundedOnHostilePayloads(t *testing.T) {
	readEnv(t)
	huge := "2026-01-01" + strings.Repeat("x", 100_000)
	data := o()
	for a := range 8 {
		row := o()
		for f := range 24 {
			v := "2026-09-20"
			if f == 0 {
				v = huge
			}
			row.Set(fmt.Sprintf("date_field_with_a_rather_long_descriptive_name_%02d", f), v)
		}
		second := row.Clone()
		second.Set("date_field_with_a_rather_long_descriptive_name_00", "2025-01-01")
		data.Set(fmt.Sprintf("array_with_a_long_descriptive_name_%d", a), arr(row, second))
	}
	for s := range 24 {
		data.Set(fmt.Sprintf("stamp%d", s), "2026-09-20T00:00:00.000Z")
	}
	r := publishedView(t, o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object"), data, readFreshness())
	if size(pagedata.ProfileData(data)) <= 300_000 {
		t.Fatal("the full profile really is hostile")
	}
	summary := read(t, r, o("detail", "summary"), 9)
	if n := size(summary.Value("coverage_profile")); n > pagedata.CoverageMaxBytes {
		t.Fatalf("coverage profile is %d bytes", n)
	}
	if n := size(legacy(summary)); n > 5*1024 {
		t.Fatalf("summary is %d bytes", n)
	}
	eq(t, summary.Value("coverage_truncated"), true)
	mustMatch(t, `coverage_truncated`, summary.Value("next_step").(string))
	arrays := summary.Value("coverage_profile").(*jsonv.Object).Value("arrays").(*jsonv.Object)
	arrays.Range(func(_ string, e any) bool {
		e.(*jsonv.Object).Value("fields").(*jsonv.Object).Range(func(_ string, f any) bool {
			field := f.(*jsonv.Object)
			if len([]rune(field.Value("min").(string))) > 32 || len([]rune(field.Value("max").(string))) > 32 {
				t.Fatal("extents are clipped")
			}
			return true
		})
		return true
	})
	first := arrays.Value(arrays.Keys()[0]).(*jsonv.Object)
	eq(t, first.Value("count"), 2, "only whole entries are left out")
	tschema, tdata := tupleFixture()
	if read(t, publishedView(t, tschema, tdata, readFreshness()), o("detail", "summary"), 9).Has("coverage_truncated") {
		t.Fatal("a payload inside the bound is not marked")
	}
}

func TestCoverageTruncatedWheneverCoverageIsMissing(t *testing.T) {
	readEnv(t)
	schema := o("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object")
	summaryOf := func(data *jsonv.Object) *jsonv.Object {
		return read(t, publishedView(t, schema, data, readFreshness()), o("detail", "summary"), 9)
	}
	if summaryOf(o("dataThrough", "2026-09-20", "rows", arr(o("day", "2026-09-19"), o("day", "2026-09-20")))).Has("coverage_truncated") {
		t.Fatal("a complete small page is not marked")
	}
	nine := o()
	for i := range 9 {
		nine.Set(fmt.Sprintf("a%d", i), arr(o("day", "2026-09-20")))
	}
	ninth := summaryOf(nine)
	eq(t, ninth.Value("coverage_truncated"), true, "a ninth array")
	eq(t, ninth.Value("coverage_profile").(*jsonv.Object).Value("arrays").(*jsonv.Object).Len(), 8)
	scalars := o()
	for i := range 25 {
		scalars.Set(fmt.Sprintf("s%02d", i), "2026-09-20")
	}
	eq(t, summaryOf(scalars).Value("coverage_truncated"), true, "a 25th scalar")
	wide := o()
	for i := range 24 {
		wide.Set(fmt.Sprintf("n%02d", i), float64(i))
	}
	wide.Set("day", "2026-09-20")
	eq(t, summaryOf(o("rows", arr(wide))).Value("coverage_truncated"), true, "a 25th field")
	deep := o("day", "2026-09-20")
	for range 8 {
		deep = o("nested", deep)
	}
	eq(t, summaryOf(deep).Value("coverage_truncated"), true, "a level past the depth cap")
	long := summaryOf(o("rows", arr(o("day", "2026-09-20T00:00:00.000Z and then a long note"))))
	eq(t, long.Value("coverage_truncated"), true, "a clipped extent")
	day := long.Value("coverage_profile").(*jsonv.Object).Value("arrays").(*jsonv.Object).Value("rows").(*jsonv.Object).Value("fields").(*jsonv.Object).Value("day").(*jsonv.Object)
	if len([]rune(day.Value("max").(string))) > 32 {
		t.Fatal("the extent is clipped")
	}
	fullRead := read(t, publishedView(t, schema, nine, readFreshness()), o(), 9)
	eq(t, jsonv.Stringify(fullRead.Value("data_profile")), jsonv.Stringify(pagedata.ProfileData(nine)))
}

func TestCompactReadsClipALegacyTitle(t *testing.T) {
	readEnv(t)
	schema, data := tupleFixture()
	r := publishedView(t, schema, data, readFreshness())
	r.Page.Title = strings.Repeat("Northwind ", 50_000)
	for _, detail := range []string{"summary", "export"} {
		out := read(t, r, o("detail", detail), 9)
		title := out.Value("page").(*jsonv.Object).Value("title").(string)
		eq(t, len([]rune(title)), 200, detail)
		if !strings.HasSuffix(title, "…") {
			t.Fatal("a clipped title ends with an ellipsis")
		}
		if detail == "summary" && size(legacy(out)) > 5*1024 {
			t.Fatalf("summary is %d bytes", size(out))
		}
	}
	eq(t, read(t, r, o(), 9).Value("page").(*jsonv.Object).Value("title"), r.Page.Title, "a full read is unchanged")
	normal := publishedView(t, schema, data, readFreshness())
	eq(t, read(t, normal, o("detail", "summary"), 9).Value("page").(*jsonv.Object).Value("title"), normal.Page.Title)
}
