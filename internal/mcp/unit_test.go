// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Ports of the registry-level unit tests: the MCP blocks of test/unit.test.js,
// the get_guide tool tests of test/agent-guide.test.js, and the tool-registry
// assertions of test/update-prompts.test.js.

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ElcanoTek/pages/internal/agentguide"
	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/nodedefaults"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/updateprompts"
)

func hint(t *testing.T, tool, name string) bool {
	t.Helper()
	def := currentCatalog().byName[tool]
	if def == nil {
		t.Fatalf("%s must be registered", tool)
	}
	b, _ := def.annotations.Value(name).(bool)
	return b
}

func TestRegistryExposesThePromisedTools(t *testing.T) {
	cat := currentCatalog()
	eq(t, len(cat.tools), 48)
	for _, retired := range []string{"run_page_refresh_now", "pause_page_refresh"} {
		if cat.byName[retired] != nil {
			t.Fatalf("%s must be retired", retired)
		}
	}
	for name, want := range map[string]map[string]bool{
		"preflight_page":              {"readOnlyHint": true},
		"create_upload_ticket":        {"readOnlyHint": false},
		"update_page_data_upload":     {"idempotentHint": true},
		"find_in_version":             {"readOnlyHint": true},
		"patch_page":                  {"readOnlyHint": false},
		"get_template":                {"readOnlyHint": true},
		"list_templates":              {"readOnlyHint": true},
		"list_template_revisions":     {"readOnlyHint": true},
		"register_template_upload":    {"readOnlyHint": false, "idempotentHint": true},
		"record_refresh_check":        {"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true},
		"set_page_refresh_schedule":   {"readOnlyHint": false, "idempotentHint": true},
		"list_refresh_health":         {"readOnlyHint": true},
		"get_page_config":             {"readOnlyHint": true},
		"update_page_config":          {"idempotentHint": true},
		"create_page_from_template":   {"idempotentHint": true},
		"list_template_pages":         {"readOnlyHint": true},
		"get_guide":                   {"readOnlyHint": true, "destructiveHint": false},
		"prepare_dashboard_update":    {"readOnlyHint": true, "destructiveHint": false},
		"get_page_refresh":            {"readOnlyHint": true},
		"configure_page_refresh":      {"readOnlyHint": true},
		"start_page_upload":           {"destructiveHint": false},
		"append_page_upload":          {"idempotentHint": true, "destructiveHint": false},
		"cancel_page_upload":          {"destructiveHint": true, "idempotentHint": true},
		"deploy_page_upload":          {"idempotentHint": true},
		"rerender_page_from_template": {},
	} {
		for h, v := range want {
			if hint(t, name, h) != v {
				t.Errorf("%s.%s should be %v", name, h, v)
			}
		}
	}
	mustMatch(t, `(?i)publish defaults to FALSE`, cat.byName["rerender_page_from_template"].description)
	mustMatch(t, `(?i)One page per call`, cat.byName["rerender_page_from_template"].description)
	mustMatch(t, `(?i)already-deployed clients need no code change`, cat.byName["configure_page_refresh"].description)
	mustMatch(t, `(?i)staged page-upload tools`, cat.byName["update_page"].description)
	mustMatch(t, `as few appends as the returned max_chunk_bytes allows`, cat.byName["start_page_upload"].description)
	mustMatch(t, `as few appends as max_chunk_bytes allows`, cat.byName["append_page_upload"].description)

	// A data_update token never gains authoring reach: exactly the managed-data
	// tools, and record_refresh_check (the outcome a run that publishes
	// nothing has to record).
	reach := []string{"get_page_data", "update_page_data", "record_refresh_check"}
	agent := &tokens.Agent{Scope: tokens.ScopeDataUpdate}
	for _, def := range cat.tools {
		if tokens.IsMcpToolAllowed(agent, def.name) != slices.Contains(reach, def.name) {
			t.Errorf("data_update scope authorization for %s", def.name)
		}
	}
	if tokens.IsMcpToolAllowed(agent, "get_guide") || !tokens.IsMcpToolAllowed(&tokens.Agent{Scope: tokens.ScopeDeploy}, "get_guide") {
		t.Fatal("a data_update token cannot read the guide; a deploy token can")
	}
}

func TestUploadPathsAreAdvertisedConsistently(t *testing.T) {
	data := currentCatalog().byName["update_page_data"].input.JSONSchema(ioInput).Value("properties").(*jsonv.Object).Value("data").(*jsonv.Object).Value("description").(string)
	mustMatch(t, `create_upload_ticket kind='data'`, data)
	mustMatch(t, `update_page_data_upload`, data)
	mustMatch(t, `If outbound HTTP is unavailable`, data)
	// The number is what stops the guessing; "do not split" is what stops a
	// cautious caller publishing a partial payload.
	mustMatch(t, fmt.Sprintf(`compact payload %d`, datalimits.MaxDataBytes()), data)
	mustMatch(t, fmt.Sprintf(`stored HTML-escaped envelope %d`, datalimits.MaxDataBytes()), data)
	mustMatch(t, `(?i)whole|not split`, data)
	req := updateprompts.ExecutionRequirements(nil, "managed_data", updateprompts.RequirementOptions{})
	eq(t, req.Network, false)
	for _, tool := range req.RequiredTools {
		if currentCatalog().byName[strings.TrimPrefix(tool, "mcp_pages_")] == nil {
			t.Errorf("%s is not a Pages tool", tool)
		}
	}
}

// The execution requirements a prompt carries are accepted by the advertised
// output schema, every Pages tool they name exists, and the blocked_when
// detail argument is a real record_refresh_check argument.
func TestExecutionRequirementsFitTheRegistry(t *testing.T) {
	cat := currentCatalog()
	schema := cat.byName["prepare_dashboard_update"].output
	var reqSchema *Schema
	for _, f := range schema.fields {
		if f.name == "execution_requirements" {
			reqSchema = f.schema
		}
	}
	sources := []updateprompts.Source{
		{SourceID: "ssp_history", MCPServer: "fastio_helpers", Path: "Storage/northwind_ssp_history.csv", RequiredTools: []string{"mcp_fastio_helpers_resolve_path"}},
		{SourceID: "ssp_download", MCPServer: "fast_io", RequiredTools: []string{"mcp_fast_io_download", "download_url"}},
	}
	for _, mode := range []string{"managed_data", "full_page", "adaptive", "managed_template", "migration_required"} {
		for _, recurring := range []bool{false, true} {
			req := updateprompts.ExecutionRequirements(sources, mode, updateprompts.RequirementOptions{Slug: "northwind/overview", Recurring: recurring})
			v, err := toValue(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reqSchema.Parse(v); err != nil {
				t.Errorf("%s recurring=%v: %v", mode, recurring, err)
			}
			for _, tool := range req.RequiredTools {
				if strings.HasPrefix(tool, "mcp_pages_") && cat.byName[strings.TrimPrefix(tool, "mcp_pages_")] == nil {
					t.Errorf("%s is not a real Pages tool", tool)
				}
			}
			if req.Completion != nil {
				found := false
				for _, f := range cat.byName["record_refresh_check"].input.fields {
					found = found || f.name == req.Completion.BlockedWhen.DetailArgument
				}
				if !found {
					t.Errorf("record_refresh_check has no %q argument", req.Completion.BlockedWhen.DetailArgument)
				}
			}
		}
	}
	for _, tool := range updateprompts.CommitTools {
		if cat.byName[strings.TrimPrefix(tool, "mcp_pages_")] == nil {
			t.Errorf("commit tool %s is not registered", tool)
		}
	}
}

func TestInlineHTMLIsCappedAndStagedBytesAreNot(t *testing.T) {
	big := "<!doctype html><html><body>" + strings.Repeat("x", 43000) + "</body></html>"
	_, err := AssertInlineHTML(big)
	e, ok := apierror.As(err)
	if !ok || e.Code != "html_too_large_for_inline" {
		t.Fatalf("inline cap: %v", err)
	}
	d, _ := toValue(e.Details)
	eq(t, d.(*jsonv.Object).Value("bytes"), len(big))
	// An error that only says no teaches nothing: both cheap paths by name.
	mustMatch(t, `create_page_from_template`, e.Message)
	mustMatch(t, `create_upload_ticket`, e.Message)
	if _, err := AssertInlineHTML("<!doctype html><html><body><p>small</p></body></html>"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAGES_MCP_MAX_INLINE_HTML_BYTES", "50000")
	if _, err := AssertInlineHTML(big); err != nil {
		t.Fatalf("the cap follows PAGES_MCP_MAX_INLINE_HTML_BYTES: %v", err)
	}
	// The cap does NOT apply to staged bytes — that is the path it points at.
	if _, err := AssertMcpHTML("<!doctype html><html><body>" + strings.Repeat("y", 140000) + "</body></html>"); err != nil {
		t.Fatal(err)
	}
	for html, code := range map[string]string{"$(cat dashboard.html)": "html_placeholder", "   ": "html_required", "PLACEHOLDER": "html_placeholder", "$( cat  x.html )": "html_placeholder"} {
		_, err := AssertMcpHTML(html)
		if e, ok := apierror.As(err); !ok || e.Code != code {
			t.Errorf("%q: %v", html, err)
		}
	}
}

func TestInlineDataContract(t *testing.T) {
	nodedefaults.Setenv(t)
	if _, err := AssertInlineData(o("rows", arr()), nil); err != nil {
		t.Fatal(err)
	}
	rows := make([]any, 20000)
	for i := range rows {
		rows[i] = o("i", float64(i), "pad", strings.Repeat("x", 200))
	}
	_, err := AssertInlineData(o("rows", rows), nil)
	e, ok := apierror.As(err)
	if !ok || e.Code != "data_validation_failed" {
		t.Fatalf("a payload past the cap must be refused: %v", err)
	}
	d, _ := toValue(e.Details)
	details := d.(*jsonv.Object)
	eq(t, details.Value("changing_transport_can_help"), false)
	mustMatch(t, `(?i)not split, sample or summarize`, e.Message)
	if details.Value("bytes").(float64) <= details.Value("max_bytes").(float64) {
		t.Fatal("bytes > max_bytes")
	}
	under := make([]any, 12600)
	for i := range under {
		under[i] = o("date", "2026-08-16", "deal", fmt.Sprintf("deal-%d", i), "spend", 1234.56, "impressions", float64(98765))
	}
	n := len(jsonv.Stringify(o("rows", under)))
	if n <= 950000 || n >= 1000000 {
		t.Fatalf("fixture should be just under 1 MB, got %d", n)
	}
	if _, err := AssertInlineData(o("rows", under), nil); err != nil {
		t.Fatalf("a payload just under the limit must pass: %v", err)
	}
	if _, err := AssertInlineData(arr(), nil); err == nil {
		t.Fatal("data must be an object")
	}
}

func TestIDCanonicalizationStaysOutOfCallerDocuments(t *testing.T) {
	out, err := JSONValue(o(
		"live_version_id", num(42),
		"page_id", num(11),
		"envelope", o("data", o("rows", arr(o("campaign_id", num(12345), "id", num(7))))),
		"config", o("client_id", num(9)),
		"reference_config", o("campaign_id", num(12345)),
		"schema", o("enum", arr(o("id", num(5)))),
		"config_schema", o("properties", o("account_id", o("type", "integer"))),
		"data_profile", o("scalars", o("campaign_id", num(12345))),
		"nested", arr(o("version_id", num(3), "negative_id", num(-1), "fraction_id", 1.5)),
	))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, jsonv.Stringify(out), `{"live_version_id":"42","page_id":"11","envelope":{"data":{"rows":[{"campaign_id":12345,"id":7}]}},"config":{"client_id":9},"reference_config":{"campaign_id":12345},"schema":{"enum":[{"id":5}]},"config_schema":{"properties":{"account_id":{"type":"integer"}}},"data_profile":{"scalars":{"campaign_id":12345}},"nested":[{"version_id":"3","negative_id":-1,"fraction_id":1.5}]}`)
	// JSON semantics are kept: a __proto__ own key survives, times render
	// as JavaScript does.
	parsed, _ := jsonv.ParseString(`{"keep":1,"__proto__":{"id":9}}`)
	proto, _ := JSONValue(parsed)
	eq(t, jsonv.Stringify(proto), `{"keep":1,"__proto__":{"id":"9"}}`)
	when, _ := JSONValue(o("when", jsx.NewTime(mustTime("2026-08-05T00:00:00Z").Time)))
	eq(t, jsonv.Stringify(when), `{"when":"2026-08-05T00:00:00.000Z"}`)
}

// Anthropic's API validates every tool input_schema against JSON Schema
// draft 2020-12 and rejects the whole request if one fails, so a single bad
// schema breaks every Claude turn that has Pages connected.
func TestEveryAdvertisedSchemaIsValidDraft202012(t *testing.T) {
	for _, def := range currentCatalog().tools {
		tool := def.protoTool(nil)
		for key, raw := range map[string][]byte{"inputSchema": tool.InputSchema, "outputSchema": tool.OutputSchema} {
			doc, err := jsonschema.UnmarshalJSON(strings.NewReader(strings.Replace(string(raw), "http://json-schema.org/draft-07/schema#", "https://json-schema.org/draft/2020-12/schema", 1)))
			if err != nil {
				t.Fatal(err)
			}
			c := jsonschema.NewCompiler()
			c.DefaultDraft(jsonschema.Draft2020)
			if err := c.AddResource("mem://"+def.name+"/"+key, doc); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Compile("mem://" + def.name + "/" + key); err != nil {
				t.Errorf("%s.%s: %v", def.name, key, err)
			}
		}
	}
}

func templateRevisionFixture(source string) *jsonv.Object {
	rev := templates.Revision{VersionID: 1, Revision: 1, ContentSHA256: strings.Repeat("a", 64), ConfigSchemaSHA256: strings.Repeat("b", 64),
		DataSchemaSHA256: strings.Repeat("c", 64), Author: "pages-cli:operator", Source: source, CreatedAt: mustTime("2026-08-04T15:25:22.352Z")}
	v, _ := JSONValue(rev)
	return v.(*jsonv.Object)
}

// The wire enum for revision.source must accept every domain source: a CLI
// registered template once failed get_template output validation.
func TestRevisionSourceEnumAcceptsEveryDomainSource(t *testing.T) {
	cat := currentCatalog()
	tpl, _ := JSONValue(templates.Template{ID: 1, Name: "t", Title: "T", CurrentRevision: ptr(1), CurrentVersionID: 1,
		CreatedAt: mustTime("2026-08-04T15:25:22.352Z"), UpdatedAt: mustTime("2026-08-04T15:25:22.352Z")})
	for _, source := range append(slices.Clone(templates.Sources), "smuggled") {
		get := o("template", tpl, "revision", templateRevisionFixture(source), "config_schema", o(), "data_schema", o(), "reference_config", o())
		listed := templateRevisionFixture(source)
		listed.Set("is_current", true)
		list := o("template", "t", "revisions", arr(listed))
		_, errGet := cat.byName["get_template"].output.Parse(get)
		_, errList := cat.byName["list_template_revisions"].output.Parse(list)
		want := source != "smuggled"
		if (errGet == nil) != want || (errList == nil) != want {
			t.Errorf("source %s: get %v, list %v", source, errGet, errList)
		}
	}
}

// ── get_guide (test/agent-guide.test.js) ──

func TestGuideAcceptsOnlyFixedTopicsAndSlugSections(t *testing.T) {
	in := currentCatalog().byName["get_guide"].input
	for _, good := range []*jsonv.Object{o(), o("topic", "workflow", "section", "changing-a-live-page")} {
		if _, err := in.Parse(good); err != nil {
			t.Errorf("%s: %v", jsonv.Stringify(good), err)
		}
	}
	for _, bad := range []*jsonv.Object{
		o("topic", "../../etc/passwd"), o("topic", "README"), o("topic", "workflow", "section", "../SECURITY"),
		o("topic", "workflow", "section", "Never"), o("topic", "workflow", "section", strings.Repeat("x", 81)), o("topic", "workflow", "extra", true),
	} {
		if _, err := in.Parse(bad); err == nil {
			t.Errorf("%s was accepted", jsonv.Stringify(bad))
		}
	}
}

func TestGuideResultsFitTheOutputSchemaAndRenderAsText(t *testing.T) {
	def := currentCatalog().byName["get_guide"]
	reads := []*jsonv.Object{o(), o("topic", "motion"), o("topic", "workflow", "section", "never")}
	for _, topic := range agentguide.TopicNames {
		reads = append(reads, o("topic", topic))
		r, err := agentguide.Get(&topic, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range r.Entries {
			reads = append(reads, o("topic", topic, "section", *e.Section))
		}
	}
	for _, args := range reads {
		out, err := def.handler(nil, nil, args)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := JSONValue(out)
		if _, err := def.output.Parse(v); err != nil {
			t.Fatalf("%s: %v", jsonv.Stringify(args), err)
		}
		r := out.(*agentguide.Result)
		text := def.renderText(out)
		if !strings.HasPrefix(text, r.Content) {
			t.Fatal("text leads with the guide itself, not JSON")
		}
		mustMatch(t, `get_guide · .* · Pages \d+\.\d+\.\d+ · sha256 [0-9a-f]{12}`, text)
		eq(t, r.ContentSHA256, shaHex([]byte(r.Content)))
		if len(text) > 16*1024 {
			t.Fatalf("%s renders %d bytes", jsonv.Stringify(args), len(text))
		}
	}
	section, _ := def.handler(nil, nil, o("topic", "workflow", "section", "never"))
	mustMatch(t, `Other sections in workflow: who-owns-what, `, def.renderText(section))
	// Only get_guide renders text; every other tool keeps its JSON text.
	var rendering []string
	for _, d := range currentCatalog().tools {
		if d.renderText != nil {
			rendering = append(rendering, d.name)
		}
	}
	eq(t, rendering, []string{"get_guide"})
	if _, err := def.handler(nil, nil, o("section", "never")); err == nil {
		t.Fatal("a section needs its topic")
	}
}

var workflowFields = []string{
	"get_guide", "mcp_pages_list_templates", "hardcoded_config_values", "page_not_template_managed", "page_is_live",
	"version_is_live", "live_version_id", "next_step", "stale_version", "required_tools", "source_not_updated",
	"data_profile", "data_reconciliation_failed", "data_warnings", "details.retry_with_expected_version",
	"details.upload_still_staged", "urls.live",
}

func TestWorkflowNamesOnlyToolsThatExist(t *testing.T) {
	raw, err := os.ReadFile("../../docs/" + agentguide.Topics["workflow"])
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z]+(?:[._][a-z]+)+)`").FindAllStringSubmatch(string(raw), -1) {
		names[m[1]] = true
	}
	cat := currentCatalog()
	for name := range names {
		if cat.byName[name] == nil && !slices.Contains(workflowFields, name) {
			t.Errorf("unknown name in AGENT_WORKFLOW.md: %s", name)
		}
	}
	for _, tool := range []string{"list_templates", "create_page_from_template", "prepare_dashboard_update", "update_page_data"} {
		if !names[tool] {
			t.Errorf("the workflow covers %s", tool)
		}
	}
}

// ── the consumer allowlists in the docs (test/unit.test.js) ──

var (
	allowlistBlockRE = regexp.MustCompile("(?s)<!-- pages:allowlist:start -->\\s*```text\n(.*?)```\\s*<!-- pages:allowlist:end -->")
	markedSpanRE     = regexp.MustCompile(`(?s)<!-- pages:allowlist:start -->.*?<!-- pages:allowlist:end -->`)
	fenceRE          = regexp.MustCompile("(?ms)^[ \t]*```[^\n]*\n(.*?)^[ \t]*```")
)

func readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile("../../" + rel)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDocsAllowlistsMatchTheCatalog(t *testing.T) {
	registered := ToolNames()
	slices.Sort(registered)
	for _, doc := range []struct {
		rel     string
		countRE string
	}{
		{"docs/INTEGRATION.md", fmt.Sprintf(`Current catalog \(%d tools\)`, len(registered))},
		{"docs/API.md", fmt.Sprintf(`complete\s+\*\*%d-tool\*\*\s+catalog`, len(registered))},
	} {
		text := readDoc(t, doc.rel)
		blocks := allowlistBlockRE.FindAllStringSubmatch(text, -1)
		if len(blocks) != 1 {
			t.Fatalf("%s must carry exactly one pages:allowlist block", doc.rel)
		}
		var documented []string
		for _, n := range strings.Split(blocks[0][1], ",") {
			if n = strings.TrimSpace(n); n != "" {
				documented = append(documented, n)
			}
		}
		sorted := slices.Clone(documented)
		slices.Sort(sorted)
		eq(t, sorted, registered, doc.rel+" allowlist must list exactly the registered tools")
		if len(slices.Compact(sorted)) != len(documented) {
			t.Errorf("duplicates in the %s allowlist", doc.rel)
		}
		mustMatch(t, doc.countRE, text, doc.rel+" must state the current tool count")
	}
	// No unpinned copy of the catalog can drift back in.
	half := len(registered) / 2
	words := regexp.MustCompile(`[^a-z_]+`)
	for _, scan := range []struct {
		rel string
		max int
	}{{"docs/API.md", 2}, {"docs/INTEGRATION.md", half}, {"docs/TEMPLATES.md", half}, {"docs/AUTHORING.md", half}, {"docs/DATA_UPDATES.md", half}, {"README.md", half}} {
		rest := markedSpanRE.ReplaceAllString(readDoc(t, scan.rel), "")
		for _, fence := range fenceRE.FindAllStringSubmatch(rest, -1) {
			named := map[string]bool{}
			for _, w := range words.Split(fence[1], -1) {
				if slices.Contains(registered, w) {
					named[w] = true
				}
			}
			if len(named) > scan.max {
				t.Errorf("%s has an unpinned tool list of %d names — move it inside the pages:allowlist markers", scan.rel, len(named))
			}
		}
	}
	// Every registered tool has exactly one row in docs/API.md's catalog table.
	api := readDoc(t, "docs/API.md")
	start := strings.Index(api, "\n### Tools\n")
	if start < 0 {
		t.Fatal("docs/API.md must have a '### Tools' section")
	}
	section := api[start:]
	if after := strings.Index(section[1:], "\n### "); after >= 0 {
		section = section[:after+1]
	}
	var rows []string
	for _, m := range regexp.MustCompile("(?m)^\\|\\s*`([a-z_]+)`\\s*\\|").FindAllStringSubmatch(section, -1) {
		rows = append(rows, m[1])
	}
	slices.Sort(rows)
	eq(t, rows, registered, "docs/API.md '### Tools' table must have exactly one row per registered tool")
	for _, name := range registered {
		if strings.Contains(name, "template") && !strings.Contains(api, "`"+name+"`") {
			t.Errorf("docs/API.md must document %s", name)
		}
	}
}
