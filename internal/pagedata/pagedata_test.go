// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

// Ported from the page-data blocks of test/unit.test.js and
// test/page-data-read.test.js.

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

func mustParse(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonv.ParseString(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return v
}

func obj(t *testing.T, s string) *jsonv.Object { t.Helper(); return mustParse(t, s).(*jsonv.Object) }

func code(err error) string {
	if e, ok := apierror.As(err); ok {
		return e.Code
	}
	return ""
}

func details(err error) string {
	if e, ok := apierror.As(err); ok {
		return jsonv.Stringify(e.Details)
	}
	return ""
}

const dataSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["count","label"],"properties":{"count":{"type":"integer","minimum":0},"label":{"type":"string"}}}`

const defaultEnvelope = `{"contract_version":1,"refreshed_at":"2026-07-17T00:00:00.000Z","source_as_of":"2026-07-16T23:00:00.000Z","data":{"count":3,"label":"ready"}}`

func managedHTML(schema, envelope string, fragment bool) string {
	if schema == "" {
		schema = dataSchema
	}
	if envelope == "" {
		envelope = defaultEnvelope
	}
	body := strings.Join([]string{
		`<main data-layout="unchanged">Dashboard</main>`,
		`<script type="application/schema+json" id="pages-data-schema">` + schema + `</script>`,
		`<script data-owner="pages" id="pages-data" type="application/json">` + envelope + `</script>`,
		`<footer>Keep me byte-for-byte</footer>`,
	}, "\n")
	if fragment {
		return body
	}
	return `<!doctype html><html><head><title>Managed</title></head><body>` + body + `</body></html>`
}

var hexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ms(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParsesFullDocumentsAndFragmentsWithDeterministicHashes(t *testing.T) {
	full, err := ParseManagedHTML(managedHTML("", "", false))
	if err != nil {
		t.Fatal(err)
	}
	frag, err := ParseManagedHTML(managedHTML("", "", true))
	if err != nil {
		t.Fatal(err)
	}
	if jsonv.Stringify(full.Schema) != dataSchema || jsonv.Stringify(full.Data()) != `{"count":3,"label":"ready"}` {
		t.Error("schema/data")
	}
	if full.DataSHA256 != frag.DataSHA256 {
		t.Error("data hash differs between full document and fragment")
	}
	a, _ := SemanticHash(obj(t, `{"b":2,"a":1}`))
	b, _ := SemanticHash(obj(t, `{"a":1,"b":2}`))
	if a != b {
		t.Error("semantic hash depends on key order")
	}
	for _, h := range []string{full.DataSHA256, full.SchemaSHA256, full.TemplateSHA256} {
		if !hexRe.MatchString(h) {
			t.Errorf("hash %q", h)
		}
	}
}

func TestMaterializationChangesOnlyDataContentsAndEscapesInjection(t *testing.T) {
	html := managedHTML("", "", false)
	parsed, err := ParseManagedHTML(html)
	if err != nil {
		t.Fatal(err)
	}
	label := "</script><img src=x onerror=\"alert(1)\">& "
	data := jsonv.ObjectOf("count", 4.0, "label", label)
	updated, err := Materialize(parsed, data, "2026-07-17T00:01:00Z", ms("2026-07-17T00:02:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if updated.HTML[:parsed.DataBlock.ContentStart] != html[:parsed.DataBlock.ContentStart] {
		t.Error("bytes before the data content changed")
	}
	suffix := html[parsed.DataBlock.ContentEnd:]
	if !strings.HasSuffix(updated.HTML, suffix) {
		t.Error("bytes after the data content changed")
	}
	content := updated.HTML[parsed.DataBlock.ContentStart : len(updated.HTML)-len(suffix)]
	if regexp.MustCompile(`(?i)</script`).MatchString(content) {
		t.Error("script terminator leaked into the data block")
	}
	for _, want := range []string{"\x5cu003c/script\x5cu003e", "\x5cu003cimg"} {
		if !strings.Contains(updated.HTML, want) {
			t.Errorf("missing %s", want)
		}
	}
	re, err := ParseManagedHTML(updated.HTML)
	if err != nil {
		t.Fatal(err)
	}
	if jsonv.Stringify(re.Schema) != jsonv.Stringify(parsed.Schema) {
		t.Error("schema changed")
	}
	if re.Data().Value("label") != label || re.RefreshedAt() != "2026-07-17T00:02:00.000Z" || re.SourceAsOf() != "2026-07-17T00:01:00.000Z" {
		t.Errorf("reparsed envelope %s", jsonv.Stringify(re.Envelope))
	}
	if re.TemplateSHA256 != parsed.TemplateSHA256 {
		t.Error("template identity moved on a data-only write")
	}
}

func TestBrokenBlocksFailClosed(t *testing.T) {
	frag := managedHTML("", "", true)
	cases := []struct{ html, code string }{
		{"<main>ordinary page</main>", "page_not_data_managed"},
		{`<script id="pages-data" type="application/json">{}</script>`, "data_contract_invalid"},
		{frag + `<script id="pages-data" type="application/json">{}</script>`, "data_contract_invalid"},
		{strings.Replace(frag, "application/schema+json", "application/json", 1), "data_contract_invalid"},
		{strings.Replace(frag, dataSchema, "{", 1), "data_contract_invalid"},
		{strings.Replace(frag, `"label":"ready"`, `"label":"</script><img src=x>"`, 1), "data_contract_invalid"},
	}
	for _, c := range cases {
		if _, err := ParseManagedHTML(c.html); code(err) != c.code {
			t.Errorf("%q: got %v, want %s", c.html, err, c.code)
		}
	}
}

func TestSchemaMustBeSelfContained2020WithObjectRoot(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object"}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"array"}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"value":{"$ref":"https://example.com/schema.json"}}}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"value":{"$dynamicRef":"other.json#value"}}}`,
	} {
		if _, err := ParseManagedHTML(managedHTML(schema, "", true)); code(err) != "data_contract_invalid" {
			t.Errorf("%s: %v", schema, err)
		}
	}
	local := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","$defs":{"metric":{"type":"integer"}},"properties":{"count":{"$ref":"#/$defs/metric"},"label":{"type":"string"}},"required":["count","label"]}`
	if _, err := ParseManagedHTML(managedHTML(local, "", true)); err != nil {
		t.Errorf("local $ref: %v", err)
	}
}

func TestValidatesCurrentAndProposedDataWithBoundedErrors(t *testing.T) {
	bad := managedHTML("", `{"contract_version":1,"refreshed_at":"2026-07-17T00:00:00Z","source_as_of":"2026-07-16T23:00:00Z","data":{"count":-1,"label":"bad"}}`, true)
	_, err := ParseManagedHTML(bad)
	if code(err) != "data_contract_invalid" || !strings.Contains(details(err), `"total_errors":1`) {
		t.Errorf("invalid current: %v %s", err, details(err))
	}
	parsed, err := ParseManagedHTML(managedHTML("", "", true))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Materialize(parsed, obj(t, `{"count":-2,"label":4}`), "2026-07-17T00:00:00Z", ms("2026-07-17T00:00:00Z"))
	e, _ := apierror.As(err)
	if e == nil || e.Code != "data_validation_failed" {
		t.Fatalf("proposed: %v", err)
	}
	if d := e.Details.(ValidationDetails); len(d.ValidationErrors) > 12 || d.TotalErrors != 2 {
		t.Errorf("details %+v", d)
	}
	_, err = Materialize(parsed, jsonv.ObjectOf("count", 1.0, "label", strings.Repeat("x", MaxDataBytes())), "2026-07-17T00:00:00Z", ms("2026-07-17T00:00:00Z"))
	if code(err) != "data_validation_failed" {
		t.Errorf("oversized: %v", err)
	}
	big := strings.TrimSuffix(dataSchema, "}") + `,"description":"` + strings.Repeat("x", MaxSchemaBytes()) + `"}`
	if _, err := ParseManagedHTML(managedHTML(big, "", true)); code(err) != "data_contract_invalid" {
		t.Errorf("oversized schema: %v", err)
	}
}

func TestSourceTimestampsAreCanonicalAndNotInTheFuture(t *testing.T) {
	parsed, _ := ParseManagedHTML(managedHTML("", "", true))
	d := jsonv.ObjectOf("count", 1.0, "label", "x")
	if _, err := Materialize(parsed, d, "2026-07-17", time.Now()); code(err) != "source_as_of_invalid" {
		t.Errorf("date only: %v", err)
	}
	if _, err := Materialize(parsed, d, "2026-07-17T01:00:00Z", ms("2026-07-17T00:00:00Z")); code(err) != "source_in_future" {
		t.Errorf("future: %v", err)
	}
	got, err := NormalizeSourceAsOf("2026-07-17T01:00:00.123456+01:00", time.Now())
	if err != nil || got != "2026-07-17T00:00:00.123Z" {
		t.Errorf("normalize: %q %v", got, err)
	}
}

const configSchema = `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["campaign"],"properties":{"campaign":{"type":"string","minLength":1},"deals":{"type":"array","items":{"type":"string"}}}}`

func templateHTML(configFirst bool, omit, config, cfgSchema string) string {
	if config == "" {
		config = `{"campaign":"Reference Campaign","deals":["9900001"]}`
	}
	if cfgSchema == "" {
		cfgSchema = configSchema
	}
	var cfg []string
	if omit != "config-schema" {
		cfg = append(cfg, `<script type="application/schema+json" id="pages-config-schema">`+cfgSchema+`</script>`)
	}
	if omit != "config" {
		cfg = append(cfg, `<script id="pages-config" type="application/json">`+config+`</script>`)
	}
	data := []string{
		`<script type="application/schema+json" id="pages-data-schema">` + dataSchema + `</script>`,
		`<script id="pages-data" type="application/json">` + defaultEnvelope + `</script>`,
	}
	parts := []string{`<main data-layout="unchanged">Dashboard</main>`}
	if configFirst {
		parts = append(append(parts, cfg...), data...)
	} else {
		parts = append(append(parts, data...), cfg...)
	}
	return strings.Join(append(parts, "<footer>Keep me byte-for-byte</footer>"), "\n")
}

func TestTemplateSHAOfConfiglessPageMatchesPreTemplateFormula(t *testing.T) {
	// Pinned literal from the JS suite: every page deployed to date keys its
	// data-update dedupe on this value.
	parsed, err := ParseManagedHTML(managedHTML("", "", false))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.TemplateSHA256 != "ac673b6743050c8dfccc29109ada6b41bbec1d70fad53d8f206cd2cd15b62214" {
		t.Errorf("template_sha256 = %s", parsed.TemplateSHA256)
	}
	if parsed.Config != nil || parsed.ConfigSHA256 != "" || parsed.ConfigBlock != nil {
		t.Error("a legacy page reports no config")
	}
}

func TestConfigPairIsOptionalButWhole(t *testing.T) {
	for _, first := range []bool{false, true} {
		p, err := ParseManagedHTML(templateHTML(first, "", "", ""))
		if err != nil {
			t.Fatal(err)
		}
		if jsonv.Stringify(p.Config) != `{"campaign":"Reference Campaign","deals":["9900001"]}` || jsonv.Stringify(p.ConfigSchema) != configSchema {
			t.Error("config")
		}
		if !hexRe.MatchString(p.ConfigSHA256) || !hexRe.MatchString(p.ConfigSchemaSHA256) {
			t.Error("config hashes")
		}
	}
	for _, omit := range []string{"config", "config-schema"} {
		if _, err := ParseManagedHTML(templateHTML(false, omit, "", "")); code(err) != "data_contract_invalid" {
			t.Errorf("half pair %s: %v", omit, err)
		}
	}
	if _, err := ParseManaged(managedHTML("", "", false), TemplateSpec); code(err) != "template_contract_invalid" {
		t.Errorf("template spec: %v", err)
	}
	if _, err := ParseManaged(templateHTML(false, "", "", ""), TemplateSpec); err != nil {
		t.Error(err)
	}
}

func TestMovingAnEnvelopePreservesFreshness(t *testing.T) {
	target, err := ParseManaged(templateHTML(false, "", "", ""), TemplateSpec)
	if err != nil {
		t.Fatal(err)
	}
	env := obj(t, `{"contract_version":1,"refreshed_at":"2026-07-01T10:00:00.000Z","source_as_of":"2026-07-01T00:00:00.000Z","data":{"count":9,"label":"Northwind"}}`)
	moved, err := MaterializeBlocks(target, BlockUpdate{Config: obj(t, `{"campaign":"Contoso"}`), SetConfig: true, Envelope: env, SetEnvelope: true}, MaterializeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	re, err := ParseManaged(moved.HTML, TemplateSpec)
	if err != nil || jsonv.Stringify(re.Envelope) != jsonv.Stringify(env) {
		t.Fatalf("moved envelope: %v", err)
	}
	if h, _ := SemanticHash(env.Value("data")); moved.DataSHA256 != h {
		t.Error("data hash")
	}
	bad := obj(t, `{"contract_version":1,"refreshed_at":"2026-07-01T10:00:00.000Z","source_as_of":"2026-07-01T00:00:00.000Z","data":{"count":"bad"}}`)
	if _, err := MaterializeBlocks(target, BlockUpdate{Envelope: bad, SetEnvelope: true}, MaterializeOptions{}); code(err) != "data_validation_failed" {
		t.Errorf("bad envelope: %v", err)
	}
}

func TestConfigAndDataWritesLeaveEachOtherByteIdentical(t *testing.T) {
	opts := MaterializeOptions{SourceAsOf: "2026-07-17T00:01:00Z", Now: ms("2026-07-17T00:02:00Z")}
	for _, first := range []bool{false, true} {
		html := templateHTML(first, "", "", "")
		p, err := ParseManaged(html, TemplateSpec)
		if err != nil {
			t.Fatal(err)
		}
		dataBytes := html[p.DataBlock.ContentStart:p.DataBlock.ContentEnd]
		configBytes := html[p.ConfigBlock.ContentStart:p.ConfigBlock.ContentEnd]

		cfgOnly, err := MaterializeBlocks(p, BlockUpdate{Config: obj(t, `{"campaign":"Second Campaign"}`), SetConfig: true}, MaterializeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		after, _ := ParseManaged(cfgOnly.HTML, TemplateSpec)
		if cfgOnly.HTML[after.DataBlock.ContentStart:after.DataBlock.ContentEnd] != dataBytes || cfgOnly.DataSHA256 != p.DataSHA256 {
			t.Error("a config update touched data bytes")
		}
		dataOnly, err := MaterializeBlocks(p, BlockUpdate{Data: obj(t, `{"count":9,"label":"fresh"}`), SetData: true}, opts)
		if err != nil {
			t.Fatal(err)
		}
		after, _ = ParseManaged(dataOnly.HTML, TemplateSpec)
		if dataOnly.HTML[after.ConfigBlock.ContentStart:after.ConfigBlock.ContentEnd] != configBytes {
			t.Error("a data update touched config bytes")
		}
		both, err := MaterializeBlocks(p, BlockUpdate{Config: obj(t, `{"campaign":"Third","deals":["a","b"]}`), SetConfig: true, Data: obj(t, `{"count":1,"label":"both"}`), SetData: true}, opts)
		if err != nil {
			t.Fatal(err)
		}
		after, _ = ParseManaged(both.HTML, TemplateSpec)
		if jsonv.Stringify(after.Config) != `{"campaign":"Third","deals":["a","b"]}` || jsonv.Stringify(after.Data()) != `{"count":1,"label":"both"}` || after.TemplateSHA256 != both.TemplateSHA256 {
			t.Error("both at once")
		}
	}
}

func TestEditingConfigMovesTemplateSHA(t *testing.T) {
	p, _ := ParseManaged(templateHTML(false, "", "", ""), TemplateSpec)
	opts := MaterializeOptions{SourceAsOf: "2026-07-17T00:01:00Z", Now: ms("2026-07-17T00:02:00Z")}
	dataOnly, _ := MaterializeBlocks(p, BlockUpdate{Data: obj(t, `{"count":5,"label":"x"}`), SetData: true}, opts)
	if dataOnly.TemplateSHA256 != p.TemplateSHA256 {
		t.Error("data-only write moved the identity")
	}
	cfgOnly, _ := MaterializeBlocks(p, BlockUpdate{Config: obj(t, `{"campaign":"Different"}`), SetConfig: true}, MaterializeOptions{})
	if cfgOnly.TemplateSHA256 == p.TemplateSHA256 || cfgOnly.DataSHA256 != p.DataSHA256 {
		t.Error("config-only write must move the identity and keep the data hash")
	}
	re, _ := ParseManaged(cfgOnly.HTML, TemplateSpec)
	if re.TemplateSHA256 != cfgOnly.TemplateSHA256 {
		t.Error("reported identity differs from the written bytes")
	}
}

func TestConfigPayloadsAreValidatedEscapedAndBounded(t *testing.T) {
	p, _ := ParseManaged(templateHTML(false, "", "", ""), TemplateSpec)
	write := func(cfg *jsonv.Object) (*Materialized, error) {
		return MaterializeBlocks(p, BlockUpdate{Config: cfg, SetConfig: true}, MaterializeOptions{})
	}
	if _, err := write(obj(t, `{"campaign":""}`)); code(err) != "config_validation_failed" || !strings.Contains(details(err), `"total_errors":1`) {
		t.Errorf("empty campaign: %v", err)
	}
	if _, err := write(obj(t, `{"campaign":"ok","unexpected":true}`)); code(err) != "config_validation_failed" {
		t.Errorf("unexpected key: %v", err)
	}
	if _, err := write(jsonv.ObjectOf("campaign", strings.Repeat("x", MaxConfigBytes()))); code(err) != "config_validation_failed" {
		t.Errorf("oversized: %v", err)
	}
	inj, err := write(jsonv.ObjectOf("campaign", `</script><img src=x onerror="alert(1)">`))
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)</script><img`).MatchString(inj.HTML[p.ConfigBlock.ContentStart:]) {
		t.Error("config not escaped")
	}
	if _, err := ParseManaged(inj.HTML, TemplateSpec); err != nil {
		t.Error(err)
	}
	if _, err := ParseManagedHTML(templateHTML(false, "", `{"campaign":42}`, "")); code(err) != "data_contract_invalid" {
		t.Errorf("current config: %v", err)
	}
	if _, err := ParseManagedHTML(templateHTML(false, "", "", `{"type":"object"}`)); code(err) != "data_contract_invalid" {
		t.Errorf("config schema: %v", err)
	}
	redos := strings.Replace(configSchema, `"campaign":{"type":"string","minLength":1}`, `"campaign":{"type":"string","pattern":"(a+)+$"}`, 1)
	if _, err := ParseManagedHTML(templateHTML(false, "", "", redos)); code(err) != "data_contract_invalid" {
		t.Errorf("ReDoS config schema: %v", err)
	}
}

func TestWritingConfigToAPageWithoutConfigFailsClosed(t *testing.T) {
	p, _ := ParseManagedHTML(managedHTML("", "", false))
	if _, err := MaterializeBlocks(p, BlockUpdate{Config: obj(t, `{"campaign":"nope"}`), SetConfig: true}, MaterializeOptions{}); code(err) != "page_not_template_managed" {
		t.Errorf("%v", err)
	}
}

func patternManagedHTML(pattern, code string) string {
	schema := jsonv.ObjectOf("$schema", "https://json-schema.org/draft/2020-12/schema", "type", "object", "additionalProperties", false,
		"required", []any{"code"}, "properties", jsonv.ObjectOf("code", jsonv.ObjectOf("type", "string", "pattern", pattern)))
	env := jsonv.ObjectOf("contract_version", 1.0, "refreshed_at", "2026-07-17T00:00:00.000Z", "source_as_of", "2026-07-16T23:00:00.000Z", "data", jsonv.ObjectOf("code", code))
	return managedHTML(jsonv.Stringify(schema), jsonv.Stringify(env), false)
}

func TestCatastrophicPatternsAreRejected(t *testing.T) {
	for _, p := range []string{
		"^(a+)+$", "^([a-z]+)*$", "^(\\d+)+$", "^(x+y)+$", "^(?<digits>\\d+)*$", "^(x|\\d+)*$", "^(a?\\d+)*$",
		"^(a{0,1}\\d+)*$", "^((\\d+))*$", "^(a|a)+$", "^(a|b|a)+$", "^(a+a)+$", "^(([a-z]+)+)$",
		(fmt.Sprintf("^a{%s}$", strings.Repeat("1", 1001)) + strings.Repeat("a", 100))[:1100],
	} {
		_, err := ParseManagedHTML(patternManagedHTML(p, "abc-123"))
		e, _ := apierror.As(err)
		if e == nil || e.Code != "data_contract_invalid" || !strings.Contains(e.Message, "backtrack catastrophically") {
			t.Errorf("must reject %.40s: %v", p, err)
		}
	}
}

func TestCommonSafePatternsValidate(t *testing.T) {
	for p, sample := range map[string]string{
		"^[0-9]{4}-[0-9]{2}-[0-9]{2}$": "2026-07-23", "^(true|false)$": "true", "^[a-z0-9]+(?:-[a-z0-9]+)*$": "abc-123",
		"^https?://\\S+$": "https://example.com/x", "^[A-Z][A-Za-z ]+$": "Hello World", "^[^\\s]+@[^\\s]+$": "a@b.co",
		"^(ab+c)+$": "abbc", "^(?<suffix>-[a-z0-9]+)*$": "-abc-123",
	} {
		if _, err := ParseManagedHTML(patternManagedHTML(p, sample)); err != nil {
			t.Errorf("must accept %s: %v", p, err)
		}
	}
	slug, err := ParseManagedHTML(patternManagedHTML("^[a-z0-9]+(?:-[a-z0-9]+)*$", "acme-q2"))
	if err != nil {
		t.Fatal(err)
	}
	if h, _ := SemanticHash(obj(t, `{"code":"acme-q2"}`)); h != slug.DataSHA256 {
		t.Error("hash")
	}
	if _, err := Materialize(slug, obj(t, `{"code":"Not A Slug!"}`), "2026-07-17T00:01:00Z", time.Now()); code(err) != "data_validation_failed" {
		t.Errorf("slug mismatch: %v", err)
	}
}

func TestIsSafePatternMatrix(t *testing.T) {
	safe := []string{"^(\\d{1,3}\\.){3}\\d{1,3}$", "^([0-9a-fA-F]{1,4}:){7}[0-9a-fA-F]{1,4}$", "^([0-9A-Fa-f]{2}:){5}[0-9A-Fa-f]{2}$",
		"^[0-9a-f]{8}-([0-9a-f]{4}-){3}[0-9a-f]{12}$", "^(\\d{2}:\\d{2}){2}$", "^(\\d+\\.){2}\\d+$", "^(\\S+\\s+){4}\\S+$",
		"^(\\d{4}[ -]?){3}\\d{4}$", "^(cat|car|dog)+$", "^(a|ab)+$", "^(GET|G)+$", "^(https|http)://.+$",
		"^[a-z0-9]+(?:-[a-z0-9]+)*$", "^(?<suffix>-[a-z0-9]+)*$", "^[0-9]{4}$", "^[^\\s]+$", "^(ab+c){3}$"}
	unsafe := []string{"^(a|a)+$", "^(a|b|a)+$", "^(a+)+$", "^([a-z]+)+$", "^(\\d+)*$", "^([a-z]+){2,}$", "^(?<digits>\\d+)*$",
		"^(x|\\d+)*$", "^(a?\\d+)*$", "^(a{0,1}\\d+)*$", "^((\\d+))*$", strings.Repeat("^a", 600)}
	for _, p := range safe {
		if !IsSafePattern(p) {
			t.Errorf("must accept %s", p)
		}
	}
	for _, p := range unsafe {
		if IsSafePattern(p) {
			t.Errorf("must reject %.40s", p)
		}
	}
}

func TestCompiledSchemasAreCached(t *testing.T) {
	schema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["uniq_cache_probe"],"properties":{"uniq_cache_probe":{"type":"integer"}}}`
	html := managedHTML(schema, `{"contract_version":1,"refreshed_at":"2026-07-17T00:00:00.000Z","source_as_of":"2026-07-16T23:00:00.000Z","data":{"uniq_cache_probe":1}}`, false)
	before := CompileCacheStats()
	for i := 0; i < 2; i++ {
		if _, err := ParseManagedHTML(html); err != nil {
			t.Fatal(err)
		}
	}
	after := CompileCacheStats()
	if after.Misses-before.Misses != 1 || after.Hits-before.Hits != 1 {
		t.Errorf("cache: %+v → %+v", before, after)
	}
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

func TestExampleBlocks(t *testing.T) {
	p, err := ParseManaged(templateWithExample(`{"rows":[{"n":1},{"n":2}]}`), TemplateSpec)
	if err != nil || jsonv.Stringify(p.Example) != `{"rows":[{"n":1},{"n":2}]}` || p.ExampleBlock == nil {
		t.Fatalf("example: %v", err)
	}
	without, _ := ParseManaged(templateWithExample(""), TemplateSpec)
	if without.Example != nil || without.ExampleBlock != nil {
		t.Error("absent example")
	}
	_, err = ParseManaged(templateWithExample(`{"rows":[{"n":"not-an-integer"}]}`), TemplateSpec)
	if code(err) != "data_contract_invalid" || !strings.Contains(err.Error(), "pages-data-example") {
		t.Errorf("invalid example: %v", err)
	}
	doubled := strings.Replace(templateWithExample(`{"rows":[]}`), "</body>", `<script type="application/json" id="pages-data-example">{"rows":[]}</script></body>`, 1)
	if _, err := ParseManaged(doubled, TemplateSpec); err == nil || !strings.Contains(err.Error(), "at most one") {
		t.Errorf("doubled: %v", err)
	}

	opts := MaterializeOptions{SourceAsOf: "2026-01-02T00:00:00Z", Now: ms("2026-01-03T00:00:00Z")}
	update := BlockUpdate{Data: obj(t, `{"rows":[{"n":9}]}`), SetData: true, Config: obj(t, `{"campaign":"Live"}`), SetConfig: true}
	withEx, _ := ParseManaged(templateWithExample(`{"rows":[{"n":7}]}`), TemplateSpec)
	out, err := MaterializeBlocks(withEx, update, opts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.HTML, "pages-data-example") || strings.Contains(out.HTML, `"n":7`) || !strings.Contains(out.HTML, `"n":9`) {
		t.Error("example element must be deleted and real data written")
	}
	asPage, err := ParseManaged(out.HTML, PageSpec)
	if err != nil || jsonv.Stringify(asPage.Config) != `{"campaign":"Live"}` {
		t.Fatalf("as page: %v", err)
	}
	plain, _ := MaterializeBlocks(without, update, opts)
	if out.TemplateSHA256 != plain.TemplateSHA256 {
		t.Error("an example block changed the identity of the page produced from it")
	}
}

func compileInferred(t *testing.T, config string) *Validator {
	t.Helper()
	s, err := InferConfigSchema(mustParse(t, config))
	if err != nil {
		t.Fatal(err)
	}
	v, err := compileForTest(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDerivedConfigSchema(t *testing.T) {
	v := compileInferred(t, `{"client":"Acme","kpiTarget":3.5,"live":true,"tags":["a","b"]}`)
	for doc, want := range map[string]bool{
		`{"client":"Globex","kpiTarget":4,"live":false,"tags":["c"]}`: true,
		`{"clientt":"Globex","kpiTarget":4,"live":false,"tags":[]}`:   false,
		`{"client":"Globex","kpiTarget":"4","live":false,"tags":[]}`:  false,
		`{"kpiTarget":4,"live":false,"tags":[]}`:                      false,
	} {
		if v.Valid(mustParse(t, doc)) != want {
			t.Errorf("%s: want %v", doc, want)
		}
	}
	dict := compileInferred(t, `{"revshareMap":{"X1":{"pct":50,"type":"margin"},"X2":{"pct":40,"type":"margin"}}}`)
	if !dict.Valid(mustParse(t, `{"revshareMap":{"X3":{"pct":30,"type":"margin"}}}`)) || dict.Valid(mustParse(t, `{"revshareMap":{"X3":{"pct":"thirty","type":"margin"}}}`)) {
		t.Error("dictionary")
	}
	if compileInferred(t, `{"sspLabel":"Index Exchange","dspLabel":"Amazon DSP"}`).Valid(mustParse(t, `{"sspLabel":"a","dspLabl":"b"}`)) {
		t.Error("record keeps additionalProperties:false")
	}
	nullable := compileInferred(t, `{"campaign":"x","flightEnd":null}`)
	if !nullable.Valid(mustParse(t, `{"campaign":"y"}`)) || !nullable.Valid(mustParse(t, `{"campaign":"y","flightEnd":"2026-01-01"}`)) {
		t.Error("null means optional")
	}
	if !compileInferred(t, `{"mix":[{"a":1},{"b":2}]}`).Valid(mustParse(t, `{"mix":[{"c":3}]}`)) {
		t.Error("heterogeneous array")
	}
}

func TestEnsureConfigSchema(t *testing.T) {
	withConfig := func(extra string) string {
		return `<!doctype html><html><head><title>T</title></head><body>
` + extra + `<script type="application/json" id="pages-config">{"client":"Acme"}</script>
<script type="application/schema+json" id="pages-data-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["rows"],"properties":{"rows":{"type":"array"}}}</script>
<script type="application/json" id="pages-data">{"contract_version":1,"refreshed_at":"1970-01-01T00:00:00.000Z","source_as_of":"1970-01-01T00:00:00.000Z","data":{"rows":[]}}</script>
</body></html>`
	}
	if _, err := ParseManaged(withConfig(""), PageSpec); code(err) != "data_contract_invalid" {
		t.Errorf("raw file must not parse: %v", err)
	}
	first, err := EnsureConfigSchema(withConfig(""))
	if err != nil || !first.Generated {
		t.Fatalf("ensure: %v", err)
	}
	p, err := ParseManaged(first.HTML, PageSpec)
	if err != nil || jsonv.Stringify(p.Config) != `{"client":"Acme"}` {
		t.Fatalf("as page: %v", err)
	}
	if _, err := ParseManaged(first.HTML, TemplateSpec); err != nil {
		t.Error(err)
	}
	again, _ := EnsureConfigSchema(withConfig(""))
	second, _ := EnsureConfigSchema(first.HTML)
	if again.HTML != first.HTML || second.Generated || second.HTML != first.HTML {
		t.Error("deterministic and idempotent")
	}
	hand, _ := EnsureConfigSchema(withConfig(`<script type="application/schema+json" id="pages-config-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}</script>` + "\n"))
	if hand.Generated {
		t.Error("a hand-written schema is authoritative")
	}
	plain := "<!doctype html><html><body><p>no blocks</p></body></html>"
	if r, _ := EnsureConfigSchema(plain); r.HTML != plain {
		t.Error("plain page untouched")
	}
}

func TestAssembleTemplate(t *testing.T) {
	ensured, err := EnsureConfigSchema(`<!doctype html><html><head><title>Acme Corp</title></head><body>
<script type="application/json" id="pages-config">{"client":"Acme Corp"}</script>
<script type="application/schema+json" id="pages-data-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["rows"],"properties":{"rows":{"type":"array"}}}</script>
<script type="application/json" id="pages-data">{"contract_version":1,"refreshed_at":"2026-08-01T00:00:00.000Z","source_as_of":"2026-08-01T00:00:00.000Z","data":{"rows":[{"secret":"acme-only"}]}}</script>
</body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	page := ensured.HTML
	live, _ := ParseManaged(page, PageSpec)
	tpl, err := AssembleTemplate(page, obj(t, `{"rows":[]}`), live.Data())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManaged(tpl.HTML, TemplateSpec)
	if err != nil {
		t.Fatal(err)
	}
	if jsonv.Stringify(parsed.Data()) != `{"rows":[]}` || parsed.SourceAsOf() != "1970-01-01T00:00:00.000Z" || jsonv.Stringify(parsed.Example) != `{"rows":[{"secret":"acme-only"}]}` {
		t.Error("template shape")
	}
	next, err := MaterializeBlocks(parsed, BlockUpdate{Data: obj(t, `{"rows":[]}`), SetData: true, Config: obj(t, `{"client":"Globex"}`), SetConfig: true},
		MaterializeOptions{SourceAsOf: "2026-09-01T00:00:00Z", Now: ms("2026-09-02T00:00:00Z")})
	if err != nil || strings.Contains(next.HTML, "pages-data-example") || strings.Contains(next.HTML, "acme-only") {
		t.Errorf("one client's data must not reach another's page: %v", err)
	}
	noConfig := regexp.MustCompile(`<script type="application/schema\+json" id="pages-config-schema">[\s\S]*?</script>`).ReplaceAllString(
		regexp.MustCompile(`<script type="application/json" id="pages-config">[\s\S]*?</script>`).ReplaceAllString(page, ""), "")
	if _, err := AssembleTemplate(noConfig, obj(t, `{"rows":[]}`), nil); code(err) != "page_not_template_managed" {
		t.Errorf("no config: %v", err)
	}
}

func TestWritesRefuseUnrepresentableIntegers(t *testing.T) {
	schema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["count","label"],"properties":{"count":{"type":"integer","minimum":0},"label":{"type":"string"},"rows":{"type":"array"}}}`
	p, err := ParseManagedHTML(managedHTML(schema, "", false))
	if err != nil {
		t.Fatal(err)
	}
	write := func(data string) (*Materialized, error) {
		return MaterializeBlocks(p, BlockUpdate{Data: obj(t, data), SetData: true}, MaterializeOptions{SourceAsOf: "2026-08-06T00:00:00Z", Now: ms("2026-08-06T00:01:00Z")})
	}
	_, err = write(`{"count":1493135176553912936157935211526176291021245144185842148994011022538174,"label":"ready"}`)
	if code(err) != "data_validation_failed" || !strings.Contains(err.Error(), "string concatenation") {
		t.Errorf("concatenated count: %v", err)
	}
	if _, err := write(`{"count":9007199254740993,"label":"ready"}`); code(err) != "data_validation_failed" {
		t.Errorf("2^53+1: %v", err)
	}
	if _, err := write(`{"count":1,"label":"ready","rows":[{"impressions":1e300}]}`); err == nil || !strings.Contains(err.Error(), "/rows/0/impressions") {
		t.Errorf("nested: %v", err)
	}
	ok, err := write(`{"count":9007199254740991,"label":"ready","rows":[{"spend":1234.56,"imp":9000000000}]}`)
	if err != nil || !strings.Contains(ok.HTML, "9007199254740991") {
		t.Errorf("max safe: %v", err)
	}
	if _, err := write(`{"count":0,"label":"ready","rows":[{"ctr":0.00046,"neg":-12345}]}`); err != nil {
		t.Error(err)
	}
}

// ── profiles (issue #102) ────────────────────────────────────────────────────

func sampleRows() []any {
	var rows []any
	for day := 1; day <= 31; day++ {
		date := fmt.Sprintf("2026-03-%02d", day)
		if day <= 11 {
			spend := 97.161
			if day == 1 {
				spend = 412.37
			}
			rows = append(rows, jsonv.ObjectOf("date", date, "dealId", "example-deal-a", "spend", spend))
		}
		if day >= 2 {
			rows = append(rows, jsonv.ObjectOf("date", date, "dealId", "example-deal-b", "spend", 201.3373),
				jsonv.ObjectOf("date", date, "dealId", "example-deal-c", "spend", 233.0251),
				jsonv.ObjectOf("date", date, "dealId", "example-deal-d", "spend", 187.6417))
		}
	}
	return rows
}

func filterRows(rows []any, keep func(*jsonv.Object) bool) []any {
	var out []any
	for _, r := range rows {
		if keep(r.(*jsonv.Object)) {
			out = append(out, r)
		}
	}
	return out
}

func field(p *Profile, arr, name string) *FieldProfile {
	a, _ := p.Arrays.Get(arr)
	f, _ := a.Fields.Get(name)
	return f
}

func codes(ws []Warning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

func TestProfileDataReportsContents(t *testing.T) {
	p := ProfileData(jsonv.ObjectOf("dataThrough", "2026-03-31", "rows", sampleRows()))
	rows, _ := p.Arrays.Get("rows")
	if rows.Count != 101 {
		t.Errorf("count %d", rows.Count)
	}
	d := field(p, "rows", "date")
	if d.Kind != "date" || d.Min != "2026-03-01" || d.Max != "2026-03-31" || d.Distinct != 31 {
		t.Errorf("date %+v", d)
	}
	deal := field(p, "rows", "dealId")
	if deal.Kind != "key" || deal.Values.Value("example-deal-a") != 11.0 || deal.Values.Len() != 4 {
		t.Errorf("deal %+v", deal)
	}
	raw := 0.0
	for _, r := range sampleRows() {
		raw += r.(*jsonv.Object).Value("spend").(float64)
	}
	if raw == 20044.103 {
		t.Error("fixture: the raw sum must carry a float artifact")
	}
	spend := field(p, "rows", "spend")
	if spend.Kind != "number" || spend.Sum != 20044.103 || spend.Max != 412.37 {
		t.Errorf("spend %+v", spend)
	}
	if jsonv.Stringify(p.Scalars) != `{"dataThrough":"2026-03-31"}` {
		t.Error("scalars")
	}
	var notes []any
	for i := 0; i < 80; i++ {
		notes = append(notes, jsonv.ObjectOf("note", fmt.Sprintf("unique note %d", i), "n", 1.0))
	}
	note := field(ProfileData(jsonv.ObjectOf("rows", notes)), "rows", "note")
	if note.Kind != "text" || !note.DistinctOverflow || note.Values != nil {
		t.Errorf("free text %+v", note)
	}
}

func TestDimensionDiffIsOverTheWholeSet(t *testing.T) {
	rowsFor := func(order []int) []any {
		var rows []any
		for rank, deal := range order {
			for i := 0; i < 25-rank; i++ {
				rows = append(rows, jsonv.ObjectOf("dealId", fmt.Sprintf("deal%02d", deal), "spend", 1.0))
			}
		}
		return rows
	}
	ids := make([]int, 25)
	for i := range ids {
		ids[i] = i
	}
	publishedRows := rowsFor(ids)
	published := ProfileData(jsonv.ObjectOf("rows", publishedRows))
	f := field(published, "rows", "dealId")
	if f.Distinct != 25 || f.Values.Len() != 20 || f.ValuesOmitted != 5 {
		t.Errorf("wire summary %+v", f)
	}
	if !strings.Contains(jsonv.Stringify(published), `"values_omitted":5`) {
		t.Error("values_omitted on the wire")
	}
	reshuffled := rowsFor(append(append([]int{}, ids[1:]...), ids[0]))
	if ws := CompareDataProfiles(published, ProfileData(jsonv.ObjectOf("rows", reshuffled)), CompareOptions{}); len(ws) != 0 {
		t.Errorf("reshuffle fired %v", codes(ws))
	}
	dropped := filterRows(publishedRows, func(r *jsonv.Object) bool { return r.Value("dealId") != "deal24" })
	var w *Warning
	for _, x := range CompareDataProfiles(published, ProfileData(jsonv.ObjectOf("rows", dropped)), CompareOptions{}) {
		if x.Code == "dimension_values_missing" {
			x := x
			w = &x
		}
	}
	if w == nil || !strings.Contains(w.Message, "deal24") || len(w.Previous.([]any)) > 8 {
		t.Errorf("missing deal: %+v", w)
	}
}

func TestLongValueDoesNotSwitchOffDimensionCheck(t *testing.T) {
	long := "Acme Corp | Summer Flight | Programmatic Display | Placement 999 | Geo: NY-Metro | Creative 300x250 | Viewability Optimized"
	if len(long) <= 120 {
		t.Fatal("fixture")
	}
	before := ProfileData(obj(t, `{"rows":[{"placement":"a"},{"placement":"b"},{"placement":"c"}]}`))
	after := ProfileData(jsonv.ObjectOf("rows", []any{jsonv.ObjectOf("placement", "a"), jsonv.ObjectOf("placement", long)}))
	f := field(after, "rows", "placement")
	if f.Kind != "key" || f.Values.Has(long) || f.ValuesOmitted != 1 {
		t.Errorf("%+v", f)
	}
	if !strings.Contains(strings.Join(codes(CompareDataProfiles(before, after, CompareOptions{})), ","), "dimension_values_missing") {
		t.Error("the check must still run")
	}
}

func TestWarningListIsCapped(t *testing.T) {
	wide := func(n int) []any {
		var rows []any
		for r := 0; r < 20; r++ {
			row := jsonv.NewObject()
			for f := 0; f < 24; f++ {
				row.Set(fmt.Sprintf("f%d", f), fmt.Sprintf("v%d_%d", f, r%n))
			}
			rows = append(rows, row)
		}
		return rows
	}
	ws := CompareDataProfiles(ProfileData(jsonv.ObjectOf("a", wide(20), "b", wide(20), "c", wide(20))),
		ProfileData(jsonv.ObjectOf("a", wide(3), "b", wide(3), "c", wide(3))), CompareOptions{})
	if len(ws) <= 5 || len(ws) > 41 {
		t.Fatalf("%d warnings", len(ws))
	}
	last := ws[len(ws)-1]
	if last.Code != "warnings_truncated" || !regexp.MustCompile(`further warnings? were omitted`).MatchString(last.Message) || last.Previous.(float64) <= last.Current.(float64) {
		t.Errorf("%+v", last)
	}
}

func TestCompareCatchesBothHalvesOf102(t *testing.T) {
	published := ProfileData(jsonv.ObjectOf("rows", sampleRows()))
	broken := ProfileData(jsonv.ObjectOf("rows", filterRows(sampleRows(), func(r *jsonv.Object) bool {
		return r.Value("date").(string) >= "2026-03-25" && r.Value("dealId") != "example-deal-a"
	})))
	ws := CompareDataProfiles(published, broken, CompareOptions{})
	all := strings.Join(codes(ws), ",")
	for _, c := range []string{"coverage_start_regressed", "dimension_values_missing", "row_count_dropped"} {
		if !strings.Contains(all, c) {
			t.Errorf("missing %s in %s", c, all)
		}
	}
	for _, w := range ws {
		if w.Code == "coverage_start_regressed" && (w.Previous != "2026-03-01" || w.Current != "2026-03-25" || !strings.Contains(w.Message, "trailing-window source export")) {
			t.Errorf("%+v", w)
		}
	}
	grown := ProfileData(jsonv.ObjectOf("rows", append(sampleRows(), jsonv.ObjectOf("date", "2026-04-01", "dealId", "example-deal-b", "spend", 10.0),
		jsonv.ObjectOf("date", "2026-04-01", "dealId", "newdeal", "spend", 5.0))))
	if ws := CompareDataProfiles(published, grown, CompareOptions{}); len(ws) != 0 {
		t.Errorf("growth fired %v", codes(ws))
	}
	if ws := CompareDataProfiles(published, published, CompareOptions{}); len(ws) != 0 {
		t.Error("identical fired")
	}
}

func TestExpectIsEnforced(t *testing.T) {
	p := ProfileData(jsonv.ObjectOf("rows", sampleRows()))
	if err := AssertExpectedProfile(p, obj(t, `{"row_count":{"rows":101},"totals":{"rows.spend":20044.1030000001},"date_range":{"rows.date":["2026-03-01","2026-03-31"]}}`), ""); err != nil {
		t.Error(err)
	}
	short := ProfileData(jsonv.ObjectOf("rows", filterRows(sampleRows(), func(r *jsonv.Object) bool { return r.Value("dealId") != "example-deal-a" })))
	err := AssertExpectedProfile(short, obj(t, `{"totals":{"rows.spend":20044.103}}`), "")
	e, _ := apierror.As(err)
	if e == nil || e.Code != "data_reconciliation_failed" {
		t.Fatalf("%v", err)
	}
	m := e.Details.(ReconciliationDetails).Mismatches
	if len(m) != 1 || m[0].Value("check") != "total" || m[0].Value("expected") != 20044.103 || m[0].Value("actual").(float64) >= 20044.103 {
		t.Errorf("%s", jsonv.Stringify(m))
	}
	trunc := ProfileData(jsonv.ObjectOf("rows", filterRows(sampleRows(), func(r *jsonv.Object) bool { return r.Value("date").(string) >= "2026-03-25" })))
	if err := AssertExpectedProfile(trunc, obj(t, `{"date_range":{"rows.date":["2026-03-01","2026-03-31"]}}`), ""); code(err) != "data_reconciliation_failed" || !strings.Contains(err.Error(), "date_range") {
		t.Errorf("%v", err)
	}
	err = AssertExpectedProfile(p, obj(t, `{"row_count":{"rows":999},"totals":{"rows.spend":1},"date_range":{"rows.date":["2020-01-01","2020-01-02"]}}`), "")
	if e, _ := apierror.As(err); e == nil || len(e.Details.(ReconciliationDetails).Mismatches) != 3 {
		t.Errorf("all mismatches at once: %v", err)
	}
	if err := AssertExpectedProfile(p, obj(t, `{"totals":{"rows.revenue":100}}`), ""); !strings.Contains(details(err), "no numeric field at that path") {
		t.Errorf("%v", err)
	}
	if err := AssertExpectedProfile(p, nil, ""); err != nil {
		t.Error(err)
	}
	if err := AssertExpectedProfile(p, obj(t, `{"tolerance":5}`), ""); code(err) != "data_reconciliation_failed" {
		t.Error(err)
	}
}

func firstPayload() *jsonv.Object {
	var rows []any
	for _, deal := range []string{"DL900000000000000001", "DL900000000000000002", "DL900000000000000003"} {
		for _, tail := range []string{"03-25", "03-26", "03-27", "03-28", "03-29", "03-30", "03-31"} {
			rows = append(rows, jsonv.ObjectOf("date", "2026-"+tail, "dealId", deal, "dspSpend", 80.0, "dspImpressions", 25000.0, "clicks", 20.0))
		}
	}
	return jsonv.ObjectOf("dataThrough", "2026-03-31", "lastRefreshed", "Apr 1, 2026", "rows", rows,
		"unmapped", jsonv.ObjectOf("notes", []any{"Source rows accounted for: 21 of 21; 0 dropped."}))
}

func TestFirstPayloadIsFlagged(t *testing.T) {
	empty := ProfileData(obj(t, `{"dataThrough":null,"rows":[]}`))
	shipped := ProfileData(firstPayload())
	ws := CompareDataProfiles(empty, shipped, CompareOptions{})
	if len(ws) != 1 || ws[0].Code != "coverage_unverified" || ws[0].Path != "rows" {
		t.Fatalf("%s", jsonv.Stringify(ws))
	}
	for _, want := range []string{"UNVERIFIED", "2026-03-25 to 2026-03-31", "expect"} {
		if !strings.Contains(ws[0].Message, want) {
			t.Errorf("message lacks %s", want)
		}
	}
	for _, e := range []string{`{"date_range":{"rows.date":["2026-03-01","2026-03-31"]}}`, `{"row_count":{"rows":21}}`, `{"totals":{"rows.dspSpend":1680}}`} {
		if ws := CompareDataProfiles(empty, shipped, CompareOptions{Expected: obj(t, e)}); len(ws) != 0 {
			t.Errorf("%s should withdraw the warning", e)
		}
	}
	if ws := CompareDataProfiles(empty, shipped, CompareOptions{Expected: obj(t, `{"row_count":{"other":1}}`)}); len(ws) != 1 {
		t.Error("another array's expect does not cover this one")
	}
	if ws := CompareDataProfiles(shipped, shipped, CompareOptions{}); len(ws) != 0 {
		t.Error("ordinary refresh fired")
	}
	err := AssertExpectedProfile(shipped, obj(t, `{"date_range":{"rows.date":["2026-03-01","2026-03-31"]}}`), "")
	if e, _ := apierror.As(err); e == nil || jsonv.Stringify(e.Details.(ReconciliationDetails).Mismatches[0]) != `{"check":"date_range","path":"rows.date","expected":["2026-03-01","2026-03-31"],"actual":["2026-03-25","2026-03-31"]}` {
		t.Errorf("%v", err)
	}
	err = AssertExpectedProfile(shipped, obj(t, `{"totals":{"rows.revenue":20044.103}}`), "")
	if e, _ := apierror.As(err); e == nil || e.Details.(ReconciliationDetails).Mismatches[0].Value("actual") != nil {
		t.Errorf("%v", err)
	}
}

func TestRefreshThatAddedNothingSaysSo(t *testing.T) {
	day := func(last string, spend float64) []any {
		var out []any
		for _, d := range []string{"2026-04-01", "2026-04-02", "2026-04-03", "2026-04-04"} {
			if d > last {
				continue
			}
			for _, deal := range []string{"example-deal-a", "example-deal-b"} {
				out = append(out, jsonv.ObjectOf("date", d, "dealId", deal, "dspSpend", spend))
			}
		}
		return out
	}
	published := jsonv.ObjectOf("rows", day("2026-04-03", 100.5))
	warn := func(next *jsonv.Object) []Warning {
		return CompareDataProfiles(ProfileData(published), ProfileData(next), CompareOptions{PreviousDataSHA: mustHash(published), NextDataSHA: mustHash(next)})
	}
	identical := warn(jsonv.ObjectOf("rows", day("2026-04-03", 100.5)))
	if len(identical) != 1 || identical[0].Code != "data_unchanged" || !strings.Contains(identical[0].Message, "Do not report this as a data update") {
		t.Errorf("%v", codes(identical))
	}
	restated := warn(jsonv.ObjectOf("rows", day("2026-04-03", 101.25)))
	if len(restated) != 1 || restated[0].Code != "coverage_did_not_advance" || !strings.Contains(restated[0].Message, "still ends at 2026-04-03") || restated[0].Previous != "2026-04-03" {
		t.Errorf("%v", codes(restated))
	}
	if ws := warn(jsonv.ObjectOf("rows", day("2026-04-04", 100.5))); len(ws) != 0 {
		t.Error("an advancing window fired")
	}
	if ws := CompareDataProfiles(ProfileData(published), ProfileData(published), CompareOptions{}); len(ws) != 0 {
		t.Error("no hashes, no stale warning")
	}
	backfilled := warn(jsonv.ObjectOf("rows", append([]any{jsonv.ObjectOf("date", "2026-03-01", "dealId", "example-deal-a", "dspSpend", 50.0)}, day("2026-04-03", 100.5)...)))
	if len(backfilled) != 0 {
		t.Errorf("backfill fired %v", codes(backfilled))
	}
	removed := warn(jsonv.ObjectOf("rows", filterRows(day("2026-04-03", 100.5), func(r *jsonv.Object) bool {
		return r.Value("date") != "2026-04-02" || r.Value("dealId") != "example-deal-b"
	})))
	if strings.Join(codes(removed), ",") != "row_count_dropped" {
		t.Errorf("%v", codes(removed))
	}
	var swappedRows []any
	for _, r := range day("2026-04-03", 100.5) {
		o := r.(*jsonv.Object).Clone()
		if o.Value("dealId") == "example-deal-b" {
			o.Set("dealId", "example-deal-z")
		}
		swappedRows = append(swappedRows, o)
	}
	if got := strings.Join(codes(warn(jsonv.ObjectOf("rows", swappedRows))), ","); got != "coverage_did_not_advance,dimension_values_missing" {
		t.Errorf("swapped: %s", got)
	}
}

func TestCoverageProfileKeepsCountsAndDates(t *testing.T) {
	p := ProfileData(obj(t, `{"through":"2026-09-20","label":"Contoso","count":4,"rows":[{"day":"2026-09-01","deal":"a","spend":1.5},{"day":"2026-09-20","deal":"b","spend":2}],"ids":["a","b","c"]}`))
	want := `{"arrays":{"rows":{"count":2,"fields":{"day":{"kind":"date","min":"2026-09-01","max":"2026-09-20","distinct":2,"nulls":0}}},"ids":{"count":3,"fields":{}}},"scalars":{"through":"2026-09-20"}}`
	if got := jsonv.Stringify(CoverageProfile(p)); got != want {
		t.Errorf("got %s", got)
	}
}

func TestHostilePayloadCannotPushCoveragePastItsBound(t *testing.T) {
	huge := "2026-01-01" + strings.Repeat("x", 100000)
	data := jsonv.NewObject()
	for a := 0; a < 8; a++ {
		row := jsonv.NewObject()
		for f := 0; f < 24; f++ {
			v := "2026-09-20"
			if f == 0 {
				v = huge
			}
			row.Set(fmt.Sprintf("date_field_with_a_rather_long_descriptive_name_%02d", f), v)
		}
		other := row.Clone()
		other.Set("date_field_with_a_rather_long_descriptive_name_00", "2025-01-01")
		data.Set(fmt.Sprintf("array_with_a_long_descriptive_name_%d", a), []any{row, other})
	}
	for s := 0; s < 24; s++ {
		data.Set(fmt.Sprintf("stamp%d", s), "2026-09-20T00:00:00.000Z")
	}
	full := ProfileData(data)
	if len(jsonv.Stringify(full)) <= 300000 {
		t.Fatal("fixture: the full profile is hostile")
	}
	cov, truncated := BoundedCoverageProfile(full, 0)
	if n := len(jsonv.Stringify(cov)); n > CoverageMaxBytes || !truncated {
		t.Errorf("%d bytes, truncated %v", n, truncated)
	}
	for _, path := range cov.Arrays.Keys() {
		a, _ := cov.Arrays.Get(path)
		for _, name := range a.Fields.Keys() {
			f, _ := a.Fields.Get(name)
			if jsstr.UTF16Len(f.Min.(string)) > 32 || jsstr.UTF16Len(f.Max.(string)) > 32 {
				t.Error("extents are clipped")
			}
		}
	}
	first, _ := cov.Arrays.Get(cov.Arrays.Keys()[0])
	if first.Count != 2 {
		t.Error("kept arrays keep their count")
	}
}

func TestJSToFixedTies(t *testing.T) {
	for v, want := range map[float64]string{0.0078125: "0.007813", 2.5e-7: "0.000000", -0.0000005: "-0.000000", 20044.102999999992: "20044.103000", 1.5: "1.500000"} {
		if got := jsToFixed(v, 6); got != want {
			t.Errorf("toFixed(%v) = %s, want %s", v, got, want)
		}
	}
	if roundSum(math.Inf(1)) != nil {
		t.Error("non-finite sums are null")
	}
}

func TestConcurrentReadsShareTheCompiledSchema(t *testing.T) {
	html := patternManagedHTML("^(?=a)[a-z]+(?:-[a-z0-9]+)*$", "abc-123")
	done := make(chan error)
	for i := 0; i < 8; i++ {
		go func() {
			m, err := ParseManagedHTML(html)
			if err == nil {
				_, err = Materialize(m, obj(t, `{"code":"ab-1"}`), "2026-07-17T00:01:00Z", ms("2026-07-17T00:02:00Z"))
			}
			done <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

func TestLookaroundPatternsStillValidate(t *testing.T) {
	// RE2 cannot express lookahead; such patterns run on the bounded
	// backtracking matcher instead of making the page unreadable.
	m, err := ParseManagedHTML(patternManagedHTML("^(?=.*\\d)[a-z0-9-]+$", "abc-123"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(m, obj(t, `{"code":"no-digits"}`), "2026-07-17T00:01:00Z", time.Now()); code(err) != "data_validation_failed" ||
		!strings.Contains(details(err), `must match pattern`) {
		t.Errorf("%v %s", err, details(err))
	}
}

func TestDataUsageReportsHeadroom(t *testing.T) {
	p, err := ParseManagedHTML(managedHTML("", "", false))
	if err != nil {
		t.Fatal(err)
	}
	u := p.DataUsage()
	if u.PayloadBytes != len(`{"count":3,"label":"ready"}`) || u.LimitBytes != MaxDataBytes() || u.HeadroomBytes != u.LimitBytes-u.EnvelopeBytes || u.EnvelopeBytes <= u.PayloadBytes {
		t.Errorf("%+v", u)
	}
}

func TestRaisedDataDefaults(t *testing.T) {
	for _, k := range []string{"PAGES_DATA_SCHEMA_MAX_BYTES", "PAGES_DATA_CONFIG_MAX_BYTES", "PAGES_DATA_TEMPLATE_MAX_BYTES", "PAGES_DATA_SCHEMA_MAX_NODES", "PAGES_DATA_SCHEMA_MAX_DEPTH", "PAGES_DATA_MAX_BYTES"} {
		t.Setenv(k, "")
	}
	if MaxSchemaBytes() != 1<<20 || MaxConfigBytes() != 1<<20 || MaxTemplateBytes() != 16<<20 || maxSchemaNodes() != 50000 || maxSchemaDepth() != 100 || MaxDataBytes() != 8<<20 {
		t.Fatalf("schema %d config %d template %d nodes %d depth %d data %d", MaxSchemaBytes(), MaxConfigBytes(), MaxTemplateBytes(), maxSchemaNodes(), maxSchemaDepth(), MaxDataBytes())
	}
	if l := TemplateLimit(); l.Warning != "" {
		t.Fatal(l.Warning)
	}
}

// The template ceiling must stay above the data budget: a document has to hold
// a maximal envelope and a page around it.
func TestTemplateCeilingStaysAboveTheDataBudget(t *testing.T) {
	t.Setenv("PAGES_DATA_MAX_BYTES", "")
	t.Setenv("PAGES_DATA_TEMPLATE_MAX_BYTES", "2097152") // the Node default, left in an old .env
	l := TemplateLimit()
	if l.Bytes != 9<<20 || MaxTemplateBytes() != 9<<20 ||
		l.Warning != "WARNING: PAGES_DATA_TEMPLATE_MAX_BYTES (2097152) is not above PAGES_DATA_MAX_BYTES (8388608), so no managed document could hold a full data envelope; managed documents are limited to 9437184 bytes instead." {
		t.Fatalf("%+v", l)
	}
	t.Setenv("PAGES_DATA_TEMPLATE_MAX_BYTES", "8388609")
	if l := TemplateLimit(); l.Bytes != 8388609 || l.Warning != "" {
		t.Fatalf("%+v", l)
	}
	t.Setenv("PAGES_DATA_MAX_BYTES", "1048576")
	t.Setenv("PAGES_DATA_TEMPLATE_MAX_BYTES", "2097152")
	if l := TemplateLimit(); l.Bytes != 2097152 || l.Warning != "" {
		t.Fatalf("%+v", l)
	}
}
