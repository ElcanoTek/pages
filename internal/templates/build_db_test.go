// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package templates

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/versions"
)

func parseEnvelope(t *testing.T, html string) string {
	t.Helper()
	m, err := pagedata.ParseManaged(html, pagedata.TemplateSpec)
	if err != nil {
		t.Fatal(err)
	}
	return jsonv.Stringify(m.Envelope)
}

func versionHTML(t *testing.T, d *db.DB, id jsx.ID) string {
	t.Helper()
	var html string
	if err := d.QueryRow(context.Background(), `SELECT html FROM page_versions WHERE id = $1`, id).Scan(&html); err != nil {
		t.Fatal(err)
	}
	return html
}

func TestTemplateIdentityIncludesImmutableBinding(t *testing.T) {
	d, ctx := setup(t)
	config := obj(t, `{"campaign":"Northwind Spring"}`)
	register(t, ctx, d, "provenance-a", fixtureHTML)
	b := register(t, ctx, d, "provenance-b", fixtureHTML)
	built := must(CreatePage(ctx, d, CreatePageInput{Template: "provenance-a", Slug: "provenance-page", Config: config,
		Data: obj(t, `{"count":7}`), SourceAsOf: "2026-08-01T00:00:00Z"}, agent))(t)
	moved := must(RerenderPage(ctx, d, RerenderInput{Slug: "provenance-page", Template: "provenance-b"}, agent))(t)
	if moved.Deduped || moved.Version.ID == built.Version.ID {
		t.Fatal("identical materialized bytes with a different binding require a new immutable version")
	}
	var persisted jsx.ID
	if err := d.QueryRow(ctx, `SELECT template_version_id FROM page_versions WHERE id = $1`, moved.Version.ID).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if persisted != b.Revision.VersionID || moved.TemplateVersionID != persisted {
		t.Fatal(persisted, b.Revision.VersionID, moved.TemplateVersionID)
	}
	retry := must(RerenderPage(ctx, d, RerenderInput{Slug: "provenance-page", Template: "provenance-b"}, agent))(t)
	if !retry.Deduped || retry.Version.ID != moved.Version.ID {
		t.Fatal("an exact target-binding retry still dedupes")
	}
	revision := register(t, ctx, d, "provenance-a", strings.Replace(fixtureHTML, "Northwind Spring", "Reference only", 1))
	revised := must(RerenderPage(ctx, d, RerenderInput{Slug: "provenance-page", Revision: intPtr(2)}, agent))(t)
	if revised.Deduped || revised.TemplateVersionID != revision.Revision.VersionID {
		t.Fatal("a revision with only reference-config changes still records provenance")
	}
	if jsonv.Stringify(revised.Config) != `{"campaign":"Northwind Spring"}` {
		t.Fatal("reference config is never implicitly inherited")
	}
	if revised.FromRevision == nil || *revised.FromRevision != 1 || revised.Reattached || revised.Template != "provenance-a" {
		t.Fatal(revised.FromRevision, revised.Template)
	}
	if st := stateOf(t, d, "provenance-page"); st.Published != built.Version.ID {
		t.Fatal("binding changes remain drafts")
	}
	original := versionHTML(t, d, built.Version.ID)
	if parseEnvelope(t, moved.HTML) != parseEnvelope(t, original) {
		t.Fatal("layout rerender preserves the complete published data envelope")
	}
	var refreshedAt, sourceAsOf jsx.Time
	if err := d.QueryRow(ctx, `SELECT refreshed_at, source_as_of FROM page_versions WHERE id = $1`, moved.Version.ID).Scan(&refreshedAt, &sourceAsOf); err != nil {
		t.Fatal(err)
	}
	if jsx.FormatTime(refreshedAt.Time) != jsonv.ToString(built.Envelope.Value("refreshed_at")) {
		t.Fatal("indexed refreshed_at", refreshedAt)
	}
	must(versions.Publish(ctx, d, versions.PublishInput{Slug: "provenance-page", VersionID: moved.Version.ID, ExpectedVersion: built.Version.ID}, agent))(t)
	freshness := func(want string) {
		t.Helper()
		pages := must(versions.ListPages(ctx, d, versions.ListPagesOptions{}))(t)
		detail := must(versions.GetPageData(ctx, d, "provenance-page"))(t)
		env := obj(t, want)
		for _, p := range pages {
			if p.Slug == "provenance-page" && jsx.FormatTime(p.Freshness.RefreshedAt.Time) != jsonv.ToString(env.Value("refreshed_at")) {
				t.Error("listed freshness", p.Freshness.RefreshedAt)
			}
		}
		if jsonv.Stringify(detail.Envelope) != want {
			t.Error("detail envelope", jsonv.Stringify(detail.Envelope))
		}
	}
	freshness(parseEnvelope(t, original))
	refreshed := must(versions.UpdatePageData(ctx, d, versions.DataUpdateInput{Slug: "provenance-page", Data: obj(t, `{"count":8}`),
		SourceAsOf: "2026-08-02T00:00:00Z", ExpectedVersion: moved.Version.ID, Publish: true}, agent))(t)
	freshness(jsonv.Stringify(refreshed.Envelope))
	must(versions.Rollback(ctx, d, versions.RollbackInput{Slug: "provenance-page", VersionID: built.Version.ID, ExpectedVersion: refreshed.Version.ID}, agent))(t)
	freshness(parseEnvelope(t, original))
}

func TestTemplateMetadataChangesAreAuditedAtomically(t *testing.T) {
	d, ctx := setup(t)
	str := func(s string) *string { return &s }
	initial := must(Register(ctx, d, RegisterInput{Name: "metadata-test", HTML: fixtureHTML, Title: str("Northwind"), Description: str("Original")}, agent))(t)
	if !initial.Created {
		t.Fatal("created")
	}
	count := func() int {
		var n int
		if err := d.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE metadata->>'template' = $1`, "metadata-test").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := count()
	title := must(Register(ctx, d, RegisterInput{Name: "metadata-test", HTML: fixtureHTML, Title: str("Contoso")}, agent))(t)
	if count() != before+1 || !title.Deduped || !title.MetadataUpdated || title.Revision.VersionID != initial.Revision.VersionID {
		t.Fatal("a title-only change is a transactionally audited mutation")
	}
	description := must(Register(ctx, d, RegisterInput{Name: "metadata-test", HTML: fixtureHTML, Description: str("Revised")}, agent))(t)
	if count() != before+2 {
		t.Fatal("a description-only change is audited once")
	}
	retry := must(Register(ctx, d, RegisterInput{Name: "metadata-test", HTML: fixtureHTML, Title: str("Contoso"), Description: str("Revised")}, agent))(t)
	if retry.MetadataUpdated || count() != before+2 || !retry.Template.UpdatedAt.Equal(description.Template.UpdatedAt.Time) {
		t.Fatal("an identical retry is a genuine no-op")
	}
	hook(t, func(tx pgx.Tx) db.Querier {
		return &hookedTx{Tx: tx, exec: func(sql string) error {
			if strings.Contains(sql, "INSERT INTO audit_log") {
				return errors.New("Synthetic audit failure")
			}
			return nil
		}}
	})
	if _, err := Register(ctx, d, RegisterInput{Name: "metadata-test", HTML: fixtureHTML, Title: str("Rollback")}, agent); err == nil || err.Error() != "Synthetic audit failure" {
		t.Fatal(err)
	}
	var gotTitle, gotDescription string
	if err := d.QueryRow(ctx, `SELECT title, description FROM page_templates WHERE name = $1`, "metadata-test").Scan(&gotTitle, &gotDescription); err != nil {
		t.Fatal(err)
	}
	if gotTitle != "Contoso" || gotDescription != "Revised" {
		t.Fatal("audit failure rolls metadata back", gotTitle, gotDescription)
	}
}

func TestTargetShapedTemplateMigrations(t *testing.T) {
	d, ctx := setup(t)
	config := obj(t, `{"campaign":"Northwind Spring"}`)
	targetConfig := `{"campaign":"Northwind Spring","region":"North"}`
	state := func(slug string) pageState { return stateOf(t, d, slug) }
	register(t, ctx, d, "migration-design", fixtureHTML)
	built := must(CreatePage(ctx, d, CreatePageInput{Template: "migration-design", Slug: "migration-page", Config: config,
		Data: obj(t, `{"count":7}`), SourceAsOf: "2026-08-01T00:00:00Z"}, agent))(t)
	targetConfigSchema := block("pages-config-schema", schema(`{"campaign":{"type":"string"},"region":{"type":"string"}}`, "campaign", "region"), "application/schema+json")
	targetHTML := strings.NewReplacer(
		campaignConfigSchema, targetConfigSchema,
		campaignConfig, block("pages-config", targetConfig, ""),
		countDataSchema, block("pages-data-schema", schema(`{"total":{"type":"number"}}`, "total"), "application/schema+json"),
		`"data":{"count":0}`, `"data":{"total":0}`,
	).Replace(fixtureHTML)
	revision := register(t, ctx, d, "migration-design", targetHTML)
	args := RerenderInput{Slug: "migration-page", Revision: intPtr(2), Config: obj(t, targetConfig), Data: obj(t, `{"total":7}`),
		SourceAsOf: "2026-08-01T00:00:00Z", ExpectedVersion: built.Version.ID}
	before := state("migration-page")
	_, err := RerenderPage(ctx, d, RerenderInput{Slug: "migration-page", Revision: intPtr(2)}, agent)
	wantCode(t, err, "data_validation_failed")
	_, err = versions.UpdatePageConfig(ctx, d, versions.ConfigUpdateInput{Slug: "migration-page", Config: obj(t, targetConfig), ExpectedVersion: built.Version.ID}, agent)
	wantCode(t, err, "config_validation_failed")
	bad := args
	bad.Data = obj(t, `{"total":"bad"}`)
	_, err = RerenderPage(ctx, d, bad, agent)
	wantCode(t, err, "data_validation_failed")
	stale := args
	stale.ExpectedVersion = "999999"
	_, err = RerenderPage(ctx, d, stale, agent)
	wantCode(t, err, "stale_version")
	missing := args
	missing.ExpectedVersion = nil
	_, err = RerenderPage(ctx, d, missing, agent)
	wantCode(t, err, "expected_version_required")
	if state("migration-page") != before {
		t.Fatal("failed migrations do not change history, audit or the live pointer")
	}
	migrated := must(RerenderPage(ctx, d, args, agent))(t)
	if migrated.Published || migrated.Version.Status != "draft" || migrated.TemplateVersionID != revision.Revision.VersionID ||
		jsonv.Stringify(migrated.Config) != targetConfig || jsonv.Stringify(migrated.Envelope.Value("data")) != `{"total":7}` {
		t.Fatalf("%+v", migrated.DeployResult)
	}
	after := state("migration-page")
	if after.Published != before.Published || after.Versions != before.Versions+1 || after.Audits != before.Audits+1 {
		t.Fatal(after, before)
	}
	retry := must(RerenderPage(ctx, d, args, agent))(t)
	if !retry.Deduped || retry.Version.ID != migrated.Version.ID || jsonv.Stringify(retry.Envelope) != jsonv.Stringify(migrated.Envelope) || retry.HTML != migrated.HTML {
		t.Fatal("a retry reports stored freshness, not another generated timestamp")
	}
	newer := args
	newer.Data, newer.SourceAsOf = obj(t, `{"total":9}`), "2026-08-03T00:00:00Z"
	must(RerenderPage(ctx, d, newer, agent))(t)
	regress := args
	regress.SourceAsOf = "2026-08-02T00:00:00Z"
	_, err = RerenderPage(ctx, d, regress, agent)
	wantCode(t, err, "source_regression")
	must(versions.SetApproval(ctx, d, "migration-page", true, staff))(t)
	gatedIn := newer
	gatedIn.Publish = true
	gated := must(RerenderPage(ctx, d, gatedIn, agent))(t)
	if gated.Version.Status != "pending" || gated.Published || state("migration-page").Published != built.Version.ID {
		t.Fatal("gated", gated.Version.Status)
	}
	// Pre-index full-source drafts also reserve source coverage.
	legacy := strings.ReplaceAll(fixtureHTML, "2026-08-01", "2026-08-05")
	if _, err := d.Exec(ctx, `INSERT INTO page_versions(page_id,html,content_sha256,status,render_mode,author,source)
		SELECT id,$1,$2,'draft','themed','source-fixture','mcp' FROM pages WHERE slug=$3`, legacy, versions.SHA256(legacy), "migration-page"); err != nil {
		t.Fatal(err)
	}
	beforeLegacy := state("migration-page")
	legacyIn := args
	legacyIn.Data, legacyIn.SourceAsOf = obj(t, `{"total":10}`), "2026-08-04T00:00:00Z"
	_, err = RerenderPage(ctx, d, legacyIn, agent)
	wantCode(t, err, "source_regression")
	if state("migration-page") != beforeLegacy {
		t.Fatal("legacy coverage rejection is atomic")
	}

	// A config-schema migration supplies no new data, so it retains freshness.
	register(t, ctx, d, "config-migration-design", fixtureHTML)
	configBuilt := must(CreatePage(ctx, d, CreatePageInput{Template: "config-migration-design", Slug: "config-migration-page", Config: config,
		Data: obj(t, `{"count":11}`), SourceAsOf: "2026-08-01T00:00:00Z"}, agent))(t)
	register(t, ctx, d, "config-migration-design", strings.NewReplacer(campaignConfigSchema, targetConfigSchema,
		campaignConfig, block("pages-config", targetConfig, "")).Replace(fixtureHTML))
	configMigrated := must(RerenderPage(ctx, d, RerenderInput{Slug: "config-migration-page", Config: obj(t, targetConfig), ExpectedVersion: configBuilt.Version.ID}, agent))(t)
	if jsonv.Stringify(configMigrated.Envelope) != parseEnvelope(t, versionHTML(t, d, configBuilt.Version.ID)) {
		t.Fatal("a config-only migration keeps the envelope")
	}
}

func TestReadsListAndRemove(t *testing.T) {
	d, ctx := setup(t)
	reg := register(t, ctx, d, "reads-design", fixtureHTML)
	if !reg.Created || reg.Deduped || reg.Revision.Revision != 1 || reg.Revision.Source != "mcp" || !reg.Preflight.OK {
		t.Fatalf("%+v", reg)
	}
	again := register(t, ctx, d, "reads-design", fixtureHTML)
	if again.Created || !again.Deduped || again.Revision.VersionID != reg.Revision.VersionID {
		t.Fatal("an identical re-register is the same revision")
	}
	_, err := Register(ctx, d, RegisterInput{Name: "reads-design", HTML: fixtureHTML, Source: "smuggled"}, agent)
	wantCode(t, err, "bad_source")
	_, err = Register(ctx, d, RegisterInput{Name: "reads-design", HTML: "  "}, agent)
	wantCode(t, err, "html_required")

	g := must(Get(ctx, d, "reads-design", nil, false))(t)
	b, _ := json.Marshal(g)
	if strings.Contains(string(b), `"html"`) || *g.Template.CurrentRevision != 1 || jsonv.Stringify(g.ReferenceConfig) != `{"campaign":"Northwind Spring"}` {
		t.Fatal(string(b))
	}
	if !regexp.MustCompile(`^\{"template":\{"id":"\d+","name":"reads-design","title":"","description":"","current_revision":1,"current_version_id":"\d+","created_at":"[^"]+Z","updated_at":"[^"]+Z"\},"revision":\{"version_id":"\d+","revision":1,"content_sha256":"[0-9a-f]{64}","config_schema_sha256":"[0-9a-f]{64}","data_schema_sha256":"[0-9a-f]{64}","author":"template-create-test","source":"mcp","note":null,"has_sample_data":false,"created_at":"[^"]+"\},"config_schema":\{`).Match(b) {
		t.Fatal(string(b))
	}
	_, err = Get(ctx, d, "reads-design", intPtr(9), false)
	wantCode(t, err, "template_revision_not_found")
	_, err = Get(ctx, d, "nope", nil, false)
	wantCode(t, err, "template_not_found")

	register(t, ctx, d, "reads-design", strings.Replace(fixtureHTML, "</body>", block("pages-data-example", `{"count":7}`, "")+"</body>", 1))
	rh := must(GetRevisionHTML(ctx, d, "reads-design", nil))(t)
	if rh.Revision != 2 || !rh.HasSampleData {
		t.Fatal(rh)
	}
	preview := must(PreviewBytes(ctx, d, rh.TemplateVersionID, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)))(t)
	if !preview.UsedSampleData || strings.Contains(preview.HTML, "pages-data-example") || !strings.Contains(preview.HTML, `"data":{"count":7}`) ||
		!strings.Contains(preview.HTML, `"refreshed_at":"2026-09-01T00:00:00.000Z"`) {
		t.Fatal(preview.HTML)
	}
	revs := must(Revisions(ctx, d, "reads-design"))(t)
	if len(revs.Revisions) != 2 || revs.Revisions[0].Revision.Revision != 2 || !revs.Revisions[0].IsCurrent || revs.Revisions[1].IsCurrent || !revs.Revisions[0].HasSampleData {
		t.Fatal(revs)
	}

	must(CreatePage(ctx, d, CreatePageInput{Template: "reads-design", Revision: intPtr(1), Slug: "northwind/reads", Config: obj(t, `{"campaign":"Northwind"}`)}, agent))(t)
	binding := must(PageTemplateBinding(ctx, d, "northwind/reads"))(t)
	if binding == nil || binding.Revision != 1 || binding.IsCurrentRevision || *binding.CurrentRevision != 2 || binding.ConfigSHA256 == nil {
		t.Fatalf("%+v", binding)
	}
	pagesOf := must(ListTemplatePages(ctx, d, "reads-design"))(t)
	if pagesOf.CurrentRevision != 2 || pagesOf.ServingCount != 1 || len(pagesOf.Pages) != 1 || !pagesOf.Pages[0].Behind || pagesOf.Pages[0].Drifted || !pagesOf.Pages[0].PageIsLive {
		t.Fatalf("%+v", pagesOf)
	}
	// Drift: a raw deploy replaces the template-built version.
	must(versions.Deploy(ctx, d, versions.DeployInput{Slug: "northwind/reads", HTML: "<!doctype html><p>hand made</p>", Publish: true}, agent))(t)
	if b := must(PageTemplateBinding(ctx, d, "northwind/reads"))(t); b != nil {
		t.Fatal("a drifted page has no binding")
	}
	drift := must(ListTemplatePages(ctx, d, "reads-design"))(t)
	if drift.DriftedCount != 1 || !drift.Pages[0].Drifted || drift.Pages[0].Revision != nil || *drift.Pages[0].LastRevision != 1 {
		t.Fatalf("%+v", drift.Pages[0])
	}
	_, err = RerenderPage(ctx, d, RerenderInput{Slug: "northwind/reads"}, agent)
	wantCode(t, err, "page_not_template_managed")
	_, err = RerenderPage(ctx, d, RerenderInput{Slug: "northwind/reads", Template: "reads-design"}, agent)
	wantCode(t, err, "page_not_template_managed")
	listed := must(List(ctx, d))(t)
	if len(listed.Templates) != 1 || listed.Templates[0].PageCount != 1 || listed.Templates[0].DriftedCount != 1 {
		t.Fatalf("%+v", listed)
	}

	_, err = Remove(ctx, d, "reads-design", false, agent)
	wantCode(t, err, "template_has_pages")
	removed := must(Remove(ctx, d, "reads-design", true, agent))(t)
	if !removed.Deleted || removed.PagesBuilt != 1 {
		t.Fatal(removed)
	}
	if l := must(List(ctx, d))(t); len(l.Templates) != 0 {
		t.Fatal("retired")
	}
	// The name is free again.
	if r := register(t, ctx, d, "reads-design", fixtureHTML); !r.Created || r.Revision.Revision != 1 {
		t.Fatal("re-registration after retirement")
	}
}

func TestCreateFromPage(t *testing.T) {
	d, ctx := setup(t)
	page := "<!doctype html><html><head><title>Northwind Spring</title></head><body>" + campaignConfig + countDataSchema +
		block("pages-data", `{"contract_version":1,"refreshed_at":"2026-08-01T00:00:00.000Z","source_as_of":"2026-08-01T00:00:00.000Z","data":{"count":5}}`, "") +
		"</body></html>"
	must(versions.CreateAndDeploy(ctx, d, versions.CreateAndDeployInput{DeployInput: versions.DeployInput{Slug: "northwind/promote", HTML: page, Publish: true}}, agent))(t)
	_, err := CreateFromPage(ctx, d, PromoteInput{Slug: "northwind/promote", Name: "promoted"}, agent)
	wantCode(t, err, "empty_data_required")
	r := must(CreateFromPage(ctx, d, PromoteInput{Slug: "northwind/promote", Name: "promoted", EmptyData: obj(t, `{"count":0}`), ExampleFromCurrentData: true}, agent))(t)
	if !r.Created || r.FromPage != "northwind/promote" || !r.ExampleFromCurrentData || !r.HasSampleData {
		t.Fatalf("%+v", r)
	}
	if len(r.HardcodedConfigValues) != 1 || r.HardcodedConfigValues[0].Path != "campaign" {
		t.Fatal(r.HardcodedConfigValues)
	}
	_, err = CreateFromPage(ctx, d, PromoteInput{Slug: "northwind/none", Name: "x", EmptyData: obj(t, `{}`)}, agent)
	wantCode(t, err, "page_not_published")
}
