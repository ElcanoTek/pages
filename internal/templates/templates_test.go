// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package templates

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/pagedata"
	"github.com/ElcanoTek/pages/internal/preflight"
	"github.com/ElcanoTek/pages/internal/render"
)

// Ported from the templates blocks of test/unit.test.js.

const dataSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["count","label"],"properties":{"count":{"type":"integer","minimum":0},"label":{"type":"string"}}}`
const configSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["campaign"],"properties":{"campaign":{"type":"string","minLength":1},"deals":{"type":"array","items":{"type":"string"}}}}`
const envelope = `{"contract_version":1,"refreshed_at":"2026-07-17T00:00:00.000Z","source_as_of":"2026-07-16T23:00:00.000Z","data":{"count":3,"label":"ready"}}`

func unitTemplateHTML(config string) string {
	if config == "" {
		config = `{"campaign":"Reference Campaign","deals":["9900001"]}`
	}
	return strings.Join([]string{
		`<main data-layout="unchanged">Dashboard</main>`,
		`<script type="application/schema+json" id="pages-data-schema">` + dataSchema + `</script>`,
		`<script id="pages-data" type="application/json">` + envelope + `</script>`,
		`<script type="application/schema+json" id="pages-config-schema">` + configSchema + `</script>`,
		`<script id="pages-config" type="application/json">` + config + `</script>`,
		"<footer>Keep me byte-for-byte</footer>",
	}, "\n")
}

func unitManagedHTML() string {
	return `<!doctype html><html><head><title>Managed</title></head><body>` + strings.Join([]string{
		`<main data-layout="unchanged">Dashboard</main>`,
		`<script type="application/schema+json" id="pages-data-schema">` + dataSchema + `</script>`,
		`<script data-owner="pages" id="pages-data" type="application/json">` + envelope + `</script>`,
		`<footer>Keep me byte-for-byte</footer>`,
	}, "\n") + `</body></html>`
}

func templateWithExample(example string) string {
	ex := ""
	if example != "" {
		ex = `<script type="application/json" id="pages-data-example">` + example + `</script>`
	}
	return `<!doctype html><html><head><title>T</title></head><body>
<h1>design</h1>
<script type="application/schema+json" id="pages-config-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["campaign"],"properties":{"campaign":{"type":"string"}}}</script>
<script type="application/json" id="pages-config">{"campaign":"Reference"}</script>
<script type="application/schema+json" id="pages-data-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["rows"],"properties":{"rows":{"type":"array","items":{"type":"object","properties":{"n":{"type":"integer"}},"additionalProperties":false}}}}</script>
<script type="application/json" id="pages-data">{"contract_version":1,"refreshed_at":"2026-01-01T00:00:00.000Z","source_as_of":"2026-01-01T00:00:00.000Z","data":{"rows":[]}}</script>
` + ex + `
</body></html>`
}

func errCode(err error) string {
	if e, ok := apierror.As(err); ok {
		return e.Code
	}
	return ""
}

func TestTemplateNamesAreFlatAndURLSafe(t *testing.T) {
	if got, err := NormalizeTemplateName(" NWM-Campaign_Dashboard "); err != nil || got != "nwm-campaign_dashboard" {
		t.Fatal(got, err)
	}
	for _, bad := range []string{"nwm/campaign", "nwm campaign", "-leading", "trailing-", "double--dash", "", strings.Repeat("x", MaxNameChars+1), "İnfo"} {
		if _, err := NormalizeTemplateName(bad); errCode(err) != "bad_template_name" {
			t.Errorf("must reject %q: %v", bad, err)
		}
	}
	if _, err := NormalizeTemplateName(nil); errCode(err) != "template_required" {
		t.Error(err)
	}
	// The validator and the migration CHECK must agree, or a name that passes
	// here dies at the INSERT.
	b, err := fs.ReadFile(pages.Migrations(), "015_page_templates.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "name ~ '^[a-z0-9]+([-_][a-z0-9]+)*$'") {
		t.Error("migrations/015 must carry the same name shape as NameRE")
	}
}

func TestValidateHTMLWithoutWriting(t *testing.T) {
	good := ValidateHTML(unitTemplateHTML(""), "Nwm-Campaign_Dashboard")
	if !good.ContractOK || good.Name == nil || *good.Name != "nwm-campaign_dashboard" || good.NameError != nil {
		t.Fatal(good)
	}
	// null, not false: this fixture's data has no rows[] array.
	if good.ShipsEmpty != nil {
		t.Error("ships_empty must be null")
	}
	if strings.Join(good.DataKeys, ",") != "count,label" || *good.Bytes == 0 || !good.Preflight.OK {
		t.Error(good.DataKeys, good.Preflight)
	}
	if jsonv.Stringify(good.ReferenceConfig) != `{"campaign":"Reference Campaign","deals":["9900001"]}` {
		t.Error(jsonv.Stringify(good.ReferenceConfig))
	}
	badName := ValidateHTML(unitTemplateHTML(""), "Not A Name!")
	if badName.Name != nil || badName.NameError.Code != "bad_template_name" || !badName.ContractOK {
		t.Error(badName)
	}
	notATemplate := ValidateHTML(unitManagedHTML(), nil)
	if notATemplate.ContractOK || notATemplate.ContractError.Code != "template_contract_invalid" || notATemplate.Preflight == nil {
		t.Error(notATemplate)
	}
	for _, empty := range []any{"", "   ", nil, 5} {
		r := ValidateHTML(empty, nil)
		if r.ContractOK || r.ContractError.Code != "html_required" {
			t.Error(empty, r)
		}
	}
	// The wire shape: every key present, nulls where not applicable.
	b, _ := json.Marshal(ValidateHTML("", nil))
	if string(b) != `{"name":null,"name_error":null,"contract_ok":false,"contract_error":{"code":"html_required","message":"no HTML was supplied"},"config_schema":null,"data_schema":null,"reference_config":null,"data_keys":null,"ships_empty":null,"has_sample_data":null,"sample_data_keys":null,"hardcoded_config_values":null,"bytes":null,"preflight":null}` {
		t.Error(string(b))
	}

	with := ValidateHTML(templateWithExample(`{"rows":[{"n":1}]}`), "t")
	if !with.ContractOK || !*with.HasSampleData || strings.Join(with.SampleDataKeys, ",") != "rows" || !*with.ShipsEmpty {
		t.Error(with)
	}
	without := ValidateHTML(templateWithExample(""), "t")
	if *without.HasSampleData || without.SampleDataKeys != nil {
		t.Error(without)
	}
}

func TestTemplateContract(t *testing.T) {
	ok, err := ParseTemplateHTML(unitTemplateHTML(""))
	if err != nil {
		t.Fatal(err)
	}
	if jsonv.Stringify(ok.Data()) != `{"count":3,"label":"ready"}` {
		t.Error(jsonv.Stringify(ok.Data()))
	}
	// An ordinary managed page is not a template.
	if _, err := ParseTemplateHTML(unitManagedHTML()); errCode(err) != "template_contract_invalid" {
		t.Error(err)
	}
	// A shipped reference config that violates its own schema is rejected.
	if _, err := ParseTemplateHTML(unitTemplateHTML(`{"campaign":""}`)); errCode(err) != "data_contract_invalid" {
		t.Error(err)
	}
}

func TestPromotionFlagsHardcodedConfigValues(t *testing.T) {
	e, err := pagedata.EnsureConfigSchema(`<!doctype html><html><head><title>Acme Corp</title></head><body>
<h1>Acme Corp</h1>
<script type="application/json" id="pages-config">{"client":"Acme Corp","tone":"ok"}</script>
<script type="application/schema+json" id="pages-data-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["rows"],"properties":{"rows":{"type":"array"}}}</script>
<script type="application/json" id="pages-data">{"contract_version":1,"refreshed_at":"1970-01-01T00:00:00.000Z","source_as_of":"1970-01-01T00:00:00.000Z","data":{"rows":[]}}</script>
</body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	report := ValidateHTML(e.HTML, "acme-family")
	if !report.ContractOK {
		t.Fatal(report.ContractError)
	}
	if len(report.HardcodedConfigValues) != 1 || report.HardcodedConfigValues[0] != (HardcodedValue{Path: "client", Value: "Acme Corp", Occurrences: 2}) {
		t.Error(report.HardcodedConfigValues)
	}
}

// Templates ship in the repo and are registered by a deploy step, so CI is
// where a broken one has to be caught: preflight errors are inherited,
// silently, by every page built from it.
func TestShippedTemplatesAreValidAndPreflightClean(t *testing.T) {
	entries, err := Discover(pages.Templates(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no shipped templates")
	}
	for _, entry := range entries {
		parsed, err := ParseTemplateHTML(entry.HTML)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name, err)
		}
		if parsed.Config.Len() == 0 || parsed.ConfigSchema.Value("type") != "object" || parsed.Schema.Value("type") != "object" {
			t.Errorf("%s: contract", entry.Name)
		}
		if n, err := NormalizeTemplateName(entry.Name); err != nil || n != entry.Name {
			t.Errorf("%s: name", entry.Name)
		}
		if rows, ok := parsed.Data().Value("rows").([]any); ok && len(rows) != 0 {
			t.Errorf("%s: ships with data rows", entry.Name)
		}
		for _, mode := range []string{"themed", "raw"} {
			r := preflight.Analyze(entry.HTML, preflight.Options{RenderMode: mode, ManagedCheck: managedCheck})
			if !r.OK {
				t.Errorf("%s (%s) preflight: %v", entry.Name, mode, r.Errors)
			}
		}
		// A page built from it must still run after the deploy normalization.
		m, err := pagedata.MaterializeBlocks(parsed, pagedata.BlockUpdate{Config: parsed.Config, SetConfig: true, Data: parsed.Data(), SetData: true},
			pagedata.MaterializeOptions{SourceAsOf: "1970-01-01T00:00:00Z"})
		if err != nil {
			t.Fatal(err)
		}
		deployed := render.StripInjectedNav(m.HTML)
		if len(deployed) != len(m.HTML) {
			t.Errorf("%s: the deploy normalisation removed bytes", entry.Name)
		}
		if r := preflight.Analyze(deployed, preflight.Options{RenderMode: "themed", ManagedCheck: managedCheck}); len(r.Errors) != 0 {
			t.Errorf("%s: %v", entry.Name, r.Errors)
		}
	}
}

func TestNWMTemplateShipsExampleData(t *testing.T) {
	b, err := fs.ReadFile(pages.Templates(), "nwm-campaign-dashboard/template.html")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseTemplateHTML(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Example == nil {
		t.Fatal("nwm-campaign-dashboard must ship an example dataset")
	}
	rows := parsed.Example.Value("rows").([]any)
	if len(rows) < 30 {
		t.Errorf("expected a real series, got %d rows", len(rows))
	}
	declared := map[string]bool{}
	for _, d := range parsed.Config.Value("deals").([]any) {
		declared[jsonv.ToString(d.(*jsonv.Object).Value("id"))] = true
	}
	for _, r := range rows {
		if id := jsonv.ToString(r.(*jsonv.Object).Value("dealId")); !declared[id] {
			t.Errorf("example row references undeclared deal %s", id)
		}
	}
	identity := jsonv.Stringify(jsonv.ObjectOf("config", parsed.Config, "example", parsed.Example))
	for _, leaked := range []string{"Contoso", "Fabrikam", "Initech", "9900001", "9900002"} {
		if strings.Contains(identity, leaked) {
			t.Errorf("reference config still names %s", leaked)
		}
	}
	for _, code := range regexp.MustCompile(`(?i)\bacct\d+\b`).FindAllString(identity, -1) {
		if code != "ACCT00000" {
			t.Errorf("real-looking account code %s", code)
		}
	}

	// DSP rows that do not report viewability still fit.
	report := ValidateHTML(string(b), "nwm-campaign-dashboard")
	if !report.ContractOK {
		t.Fatal(report.ContractError)
	}
	validate, err := pagedata.CompileSchema(report.DataSchema, "data")
	if err != nil {
		t.Fatal(err)
	}
	var dsp *jsonv.Object
	for _, r := range rows {
		if _, ok := r.(*jsonv.Object).Value("dspSpend").(float64); ok {
			dsp = r.(*jsonv.Object)
			break
		}
	}
	if dsp == nil {
		t.Fatal("no DSP row")
	}
	with := func(drop string) any {
		row := dsp.Clone()
		row.Delete(drop)
		ex := parsed.Example.Clone()
		ex.Set("rows", []any{row})
		return ex
	}
	if errs := validate.Validate(with("viewableImpressions")); len(errs) != 0 {
		t.Error(errs)
	}
	if validate.Valid(with("clicks")) {
		t.Error("the rest of the DSP group still travels together")
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("nwm-campaign-dashboard/template.html", unitTemplateHTML(""))
	write("nwm-campaign-dashboard/template.json", `{"title":"NWM Campaign Dashboard","description":"Per-campaign delivery."}`)
	write("folder-name/template.html", unitTemplateHTML(""))
	write("folder-name/template.json", `{"name":"declared-name"}`)
	write("not-a-template/notes.md", "nope")
	write("stray.html", unitTemplateHTML(""))
	found, err := Discover(os.DirFS(root), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].Name != "declared-name" || found[1].Name != "nwm-campaign-dashboard" {
		t.Fatal(found)
	}
	if found[1].Title == nil || *found[1].Title != "NWM Campaign Dashboard" || found[1].Path != "nwm-campaign-dashboard/template.html" {
		t.Error(found[1])
	}
	if got, err := Discover(os.DirFS(filepath.Join(root, "nope")), ""); err != nil || len(got) != 0 {
		t.Error("a missing directory is empty", got, err)
	}
	write("folder-name/template.json", `{"titel":"typo"}`)
	if _, err := Discover(os.DirFS(root), root); err == nil || !strings.Contains(err.Error(), `unknown key "titel"`) {
		t.Error(err)
	}
	write("folder-name/template.json", `{not json`)
	if _, err := Discover(os.DirFS(root), root); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Error(err)
	}
}
