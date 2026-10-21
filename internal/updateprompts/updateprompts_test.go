// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package updateprompts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Ported from test/update-prompts.test.js and the update-prompts blocks of
// test/unit.test.js. Byte-for-byte agreement with the JavaScript prompts is
// pinned by golden_test.go; these keep the reasons each clause exists.

const slug = "northwind/overview"

func rawSources(t *testing.T, js string) any {
	t.Helper()
	v, err := jsonv.ParseString(js)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Bindings shaped like a production SSP refresh: a path resolver and a download.
const sourcesJSON = `[
  {"source_id":"ssp_history","mcp_server":"fastio_helpers","path":"Storage/northwind_ssp_history.csv","required_tools":["mcp_fastio_helpers_resolve_path"]},
  {"source_id":"ssp_download","mcp_server":"fast_io","required_tools":["mcp_fast_io_download","download_url"]}]`

func normalized(t *testing.T, js string) []Source {
	t.Helper()
	s, err := NormalizeSources(rawSources(t, js))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func code(err error) string {
	if e, ok := apierror.As(err); ok {
		return e.Code
	}
	return ""
}

func embeddedRequirements(t *testing.T, prompt string) map[string]any {
	t.Helper()
	lines := strings.Split(prompt, "\n")
	i := slices.Index(lines, "EXECUTION REQUIREMENTS (JSON):")
	var out map[string]any
	if err := json.Unmarshal([]byte(lines[i+1]), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func step(prompt string, n string) string {
	for _, l := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(l, n+". ") {
			return l
		}
	}
	return ""
}

func mustMatch(t *testing.T, s, pattern string) {
	t.Helper()
	if !regexp.MustCompile(pattern).MatchString(s) {
		t.Errorf("expected /%s/ in:\n%.400s…", pattern, s)
	}
}

func mustNotMatch(t *testing.T, s, pattern string) {
	t.Helper()
	if regexp.MustCompile(pattern).MatchString(s) {
		t.Errorf("unexpected /%s/", pattern)
	}
}

// fixedReader is a published managed page (prepare's DB reads stubbed).
type fixedReader struct{}

func (fixedReader) getPage(context.Context, string) (*versions.PageView, error) {
	return &versions.PageView{Page: versions.Page{ID: 17, Slug: slug, Title: "Northwind", PublishedVersionID: 42},
		Published: &versions.VersionDetail{ID: 42}}, nil
}
func (fixedReader) getPageData(context.Context, string) (*versions.PageDataView, error) {
	return &versions.PageDataView{SchemaSHA256: strings.Repeat("a", 64)}, nil
}
func (fixedReader) binding(context.Context, string) (*templates.Binding, error) { return nil, nil }

func prep(t *testing.T, in PrepareInput) *Result {
	t.Helper()
	if in.Slug == nil {
		in.Slug = slug
	}
	if in.Instructions == nil {
		in.Instructions = "Refresh from the Northwind export."
	}
	r, err := prepare(context.Background(), fixedReader{}, in)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var bothPathsRequiredTools = []string{
	"download_url", "mcp_fast_io_download", "mcp_fastio_helpers_resolve_path", "mcp_pages_append_page_upload",
	"mcp_pages_get_page_config", "mcp_pages_get_page_data", "mcp_pages_preflight_page", "mcp_pages_record_refresh_check",
	"mcp_pages_start_page_upload", "mcp_pages_update_page_data", "mcp_pages_update_page_data_upload",
}

func TestRecurringPromptListsBothCommitTransports(t *testing.T) {
	r := prep(t, PrepareInput{Recurring: true, Sources: rawSources(t, sourcesJSON)})
	if r.Mode != "managed_data" {
		t.Fatal(r.Mode)
	}
	want := `{"mcp_servers":["fast_io","fastio_helpers","pages"],"required_tools":["` + strings.Join(bothPathsRequiredTools, `","`) +
		`"],"roster":"required_tools_only","completion":{"any_succeeded":["mcp_pages_record_refresh_check","mcp_pages_update_page_data","mcp_pages_update_page_data_upload"],` +
		`"blocked_when":{"tool":"mcp_pages_record_refresh_check","argument":"outcome","in":["blocked","failed","source_unreachable"],"detail_argument":"detail"}},` +
		`"serialization_key":"pages:northwind/overview","network":false,"model_required":true,"mode":"managed_data"}`
	if got := r.ExecutionRequirements.JSON(); got != want {
		t.Fatalf("requirements:\n%s\n%s", got, want)
	}
	if !strings.Contains(r.Prompt, "\n"+want+"\n") {
		t.Error("the prompt carries the same contract")
	}
	mustNotMatch(t, r.Prompt, `COMMIT TOOL:`)
	mustMatch(t, step(r.Prompt, "9"), `^9\. Over 20,000 UTF-8 bytes, stage the complete data file\. `)
	if !strings.Contains(step(r.Prompt, "9"), datalimits.FileUploadGuidance("data", "mcp_pages_")) {
		t.Error("by-reference upload guidance is kept")
	}
	var roster string
	for _, l := range strings.Split(r.Prompt, "\n") {
		if strings.HasPrefix(l, "ROSTER: ") {
			roster = l
		}
	}
	if !strings.HasSuffix(roster, "Verify and report from the commit tool's response, mcp_pages_get_page_data and mcp_pages_preflight_page; do not call Pages tools outside required_tools.") {
		t.Error(roster)
	}
	// Every Pages tool the prompt names is offered, except create_upload_ticket
	// (offered by the shared upload guidance, deliberately not required).
	for _, tool := range regexp.MustCompile(`mcp_pages_[a-z_]+`).FindAllString(r.Prompt, -1) {
		if tool != "mcp_pages_create_upload_ticket" && !slices.Contains(r.ExecutionRequirements.RequiredTools, tool) {
			t.Errorf("%s is named by the prompt but not in required_tools", tool)
		}
	}
	if slices.Contains(r.ExecutionRequirements.RequiredTools, "mcp_pages_create_upload_ticket") {
		t.Error("create_upload_ticket must stay out of required_tools")
	}
	for _, name := range append(r.ExecutionRequirements.MCPServers, r.ExecutionRequirements.RequiredTools...) {
		if !RequirementIdentifier.MatchString(name) {
			t.Error(name)
		}
	}
}

func TestUnparseableBindingsAreRefused(t *testing.T) {
	for _, bad := range []string{
		`[{"source_id":"ssp","mcp_server":"file_store + file_helpers"}]`,
		`[{"source_id":"ssp","mcp_server":"fast_io","required_tools":["mcp_fast_io_download download_url"]}]`,
		`[{"source_id":"ssp","mcp_server":"fast_io","required_tools":["mcp_fast_io_download,download_url"]}]`,
	} {
		if _, err := NormalizeSources(rawSources(t, bad)); code(err) != "update_sources_invalid" {
			t.Errorf("%s: %v", bad, err)
		}
	}
	got := normalized(t, `[{"source_id":"ssp","mcp_server":"fast-io.v2","required_tools":["mcp_fast_io_download"]}]`)
	if len(got) != 1 || got[0].MCPServer != "fast-io.v2" {
		t.Error(got)
	}
	// The legacy workflow read never fails: an unparseable server lifts
	// nothing; an unparseable tool name is dropped.
	if SourcesFromWorkflow(rawSources(t, `{"sources":[{"source_id":"combined","mcp_server":"file_store + file_helpers"},{"source_id":"ssp","mcp_server":"fast_io"}]}`)) != nil {
		t.Error("an unparseable server must lift nothing")
	}
	lifted := SourcesFromWorkflow(rawSources(t, `{"sources":[{"source_id":"ssp","mcp_server":"fast_io","required_tools":["mcp_fast_io_download","two tools"]}]}`))
	if len(lifted) != 1 || !slices.Equal(lifted[0].RequiredTools, []string{"mcp_fast_io_download"}) {
		t.Error(lifted)
	}
}

func TestRosterNarrowedOnlyWhenEverySourceNamesTools(t *testing.T) {
	serverOnly := normalized(t, `[{"source_id":"ssp","mcp_server":"fast_io"}]`)
	for _, sources := range [][]Source{nil, serverOnly, append(normalized(t, sourcesJSON), serverOnly...)} {
		req := ExecutionRequirements(sources, "managed_data", RequirementOptions{Slug: slug, Recurring: true})
		if req.Roster != "" || req.Completion == nil {
			t.Error(req)
		}
		p := ManagedPrompt(ManagedInput{Slug: slug, Instructions: "Refresh.", SchemaSHA256: strings.Repeat("a", 64), Publish: true, Recurring: true, Sources: sources})
		mustNotMatch(t, p, `\nROSTER:`)
	}
	sources := normalized(t, sourcesJSON)
	oneTime := ManagedPrompt(ManagedInput{Slug: slug, Instructions: "Refresh.", SchemaSHA256: strings.Repeat("a", 64), Publish: true, Sources: sources})
	if _, ok := embeddedRequirements(t, oneTime)["roster"]; ok {
		t.Error("one-time prompts never narrow")
	}
	adaptive := AdaptivePrompt(AdaptiveInput{Slug: slug, Instructions: "Refresh.", SchemaSHA256: strings.Repeat("a", 64), LiveVersionID: "42", Sources: sources})
	mustNotMatch(t, adaptive, `required_tools_only|\nROSTER:`)
	// Bindings lifted from a legacy workflow never narrow the roster.
	r := prep(t, PrepareInput{Recurring: true, UpdateType: "data", AllowUnboundRecurring: true,
		Sources: sourcesToJSON(SourcesFromWorkflow(rawSources(t, `{"sources":[{"source_id":"ssp","mcp_server":"fast_io","required_tools":["mcp_fast_io_download"]},{"source_id":"helpers","required_tools":["x"]}]}`)))})
	if r.ExecutionRequirements.Roster != "" || r.ExecutionRequirements.Completion == nil {
		t.Error(r.ExecutionRequirements)
	}
	mustNotMatch(t, r.Prompt, `\nROSTER:`)
}

// Golden hashes of the one-time prompt text, with the requirements line and
// the shared upload guidance masked so only this module's own wording is
// pinned — the same constants test/update-prompts.test.js pins.
func TestOneTimeManagedAndAdaptiveGoldens(t *testing.T) {
	masked := func(p string) string {
		lines := strings.Split(p, "\n")
		if i := slices.Index(lines, "EXECUTION REQUIREMENTS (JSON):"); i >= 0 {
			lines[i+1] = "<REQUIREMENTS>"
		}
		return strings.ReplaceAll(strings.Join(lines, "\n"), datalimits.FileUploadGuidance("data", "mcp_pages_"), "<UPLOAD GUIDANCE>")
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	sources := normalized(t, sourcesJSON)
	in := "Refresh from the Northwind export."
	oneTime := ManagedPrompt(ManagedInput{Slug: slug, Instructions: in, SchemaSHA256: strings.Repeat("a", 64), Publish: true, Sources: sources})
	if got := sum(masked(oneTime)); got != "f05966956bd9d44a4e13098d082aa0b8ddd69615650757090810523c1c96f23b" {
		t.Errorf("one-time golden %s", got)
	}
	adaptive := AdaptivePrompt(AdaptiveInput{Slug: slug, Instructions: in, SchemaSHA256: strings.Repeat("a", 64), LiveVersionID: "42", Sources: sources})
	if got := sum(masked(adaptive)); got != "b84d9624e0a5b4c565c6877fb503711bfccf9b114efa8381e2996e174fe8042d" {
		t.Errorf("adaptive golden %s", got)
	}
	req := embeddedRequirements(t, oneTime)
	if _, ok := req["completion"]; ok {
		t.Error("no completion predicate on a one-time prompt")
	}
	if req["serialization_key"] != "pages:northwind/overview" {
		t.Error(req)
	}
	for _, mode := range []string{"full_page", "adaptive", "managed_template", "migration_required"} {
		r := ExecutionRequirements(nil, mode, RequirementOptions{Slug: slug})
		if r.Roster != "" || r.Completion != nil || r.SerializationKey != "" || len(r.RequiredTools) != 0 {
			t.Error(mode, r)
		}
		if !strings.Contains(r.JSON(), `"required_tools":[]`) {
			t.Error(r.JSON())
		}
	}
}

func TestRecurringRequestLosesOnlySchedulingProse(t *testing.T) {
	request := "Create a reusable daily unattended data-only refresh for the Northwind campaign dashboard. " +
		"The sources are the Contoso OpenX Scheduled Report workbook and the Fabrikam daily CSV. " +
		"Run once daily when invoked by the user's scheduler; do not create or modify schedules. " +
		"Daily totals must reconcile to the source. This is prompt creation only; do not install a schedule now."
	r := prep(t, PrepareInput{Recurring: true, Instructions: request, Sources: rawSources(t, sourcesJSON)})
	want := "Data-only refresh for the Northwind campaign dashboard. The sources are the Contoso OpenX Scheduled Report workbook and the Fabrikam daily CSV. Daily totals must reconcile to the source."
	if !strings.Contains(r.Prompt, "USER REQUEST: "+jsonv.Quote(want)+"\n") {
		t.Error("quoted request")
	}
	if !slices.Equal(r.InstructionsRemoved, []string{
		"Create a reusable daily unattended",
		"Run once daily when invoked by the user's scheduler; do not create or modify schedules.",
		"This is prompt creation only; do not install a schedule now.",
	}) {
		t.Error(r.InstructionsRemoved)
	}
	mustNotMatch(t, strings.ReplaceAll(r.Prompt, "Scheduled Report", ""), `(?i)schedul|prompt creation|caller`)
	oneTime := prep(t, PrepareInput{UpdateType: "data", Instructions: request})
	if !strings.Contains(oneTime.Prompt, jsonv.Quote(request)) || len(oneTime.InstructionsRemoved) != 0 {
		t.Error("a one-time request is quoted verbatim")
	}
	if b, _ := json.Marshal(oneTime.InstructionsRemoved); string(b) != "[]" {
		t.Error("instructions_removed must serialize as []")
	}

	for _, ask := range []string{
		"Create a reusable DAILY data-refresh prompt for the existing /northwind-overview dashboard.",
		"Prepare a reusable recurring auto-update prompt for the live dashboard.",
	} {
		got := TaskOnlyInstructions(ask + " Update data only. Keep zero rows.")
		if got.Text != "Update data only. Keep zero rows." || !slices.Equal(got.Removed, []string{ask}) {
			t.Error(got)
		}
	}
	kept := "Write the complete JSON object to one file. Keep zero rows."
	if TaskOnlyInstructions(kept).Text != kept {
		t.Error("a run instruction that merely mentions a prompt-like word is kept")
	}
	if got := TaskOnlyInstructions("Do not install a schedule."); got.Text != "Do not install a schedule." || len(got.Removed) != 0 {
		t.Error(got)
	}
	if got := TaskOnlyInstructions("For each unattended run, update only the data.\nKeep zero rows."); got.Text != "Update only the data.\nKeep zero rows." {
		t.Error(got)
	}
}

func TestRecurringPromptBlocksOnlyOnHardStops(t *testing.T) {
	sources := normalized(t, sourcesJSON)
	recurring := ManagedPrompt(ManagedInput{Slug: slug, Instructions: "x", SchemaSHA256: strings.Repeat("a", 64), Publish: true, Recurring: true, Sources: sources})
	mustMatch(t, step(recurring, "5"), `Select blocked only on a hard stop`)
	mustMatch(t, step(recurring, "5"), `Every other gap follows GAP RULES below`)
	mustMatch(t, step(recurring, "4"), `it is not coverage the sources must reach`)
	if strings.Index(recurring, "\nGAP RULES — ") > strings.Index(recurring, "TERMINAL BRANCHES") {
		t.Error("the rules decide the branch")
	}
	oneTime := ManagedPrompt(ManagedInput{Slug: slug, Instructions: "x", SchemaSHA256: strings.Repeat("a", 64), Publish: true, Sources: sources})
	mustNotMatch(t, oneTime, `GAP RULES`)
	mustMatch(t, step(oneTime, "5"), `Missing, inaccessible, ambiguous or partial required sources select blocked`)
	// pages#109: float-widened counts pushed a 20,000-row payload over 1 MiB.
	for _, p := range []string{recurring, oneTime} {
		mustMatch(t, step(p, "7"), `Write integral counts as JSON integers and keep the source's decimal precision; never widen numbers through float conversion`)
		mustMatch(t, step(p, "7"), `Reuse every identifier the live payload already assigns`)
		mustMatch(t, step(p, "7"), `never mint rows the sources never contained`)
	}
	mustMatch(t, recurring, `source_not_updated: Call mcp_pages_record_refresh_check once`)
	mustNotMatch(t, oneTime, `source_not_updated: Call`)
	mustMatch(t, oneTime, `deliberate republish of already-covered data is allowed here`)
}

func TestBindingsRenderExactly(t *testing.T) {
	sources := normalized(t, `[{"source_id":"ix_ssp","mcp_server":"indexexchange_mcp","account":"nwm","required_tools":["ix_list_deals_v3","ix_get_deal_settings"],"retrieval_instructions":"Both deal ids, complete days only."},{"source_id":"amazon_dsp","mcp_server":"s3_feeds"}]`)
	p := FullPagePrompt(FullPageInput{Slug: "acme/daily", Instructions: "Refresh both deals.", LiveVersionID: "86", Publish: true, Sources: sources})
	mustMatch(t, p, `REQUIRED SOURCE BINDINGS \(exact; no substitutions\)`)
	mustMatch(t, p, `- ix_ssp: server indexexchange_mcp; account nwm; tools ix_list_deals_v3, ix_get_deal_settings — "Both deal ids, complete days only\."`)
	mustMatch(t, p, `(?i)a source absent from that list is out of scope`)
	mustNotMatch(t, p, `bind every data source named in USER REQUEST`)
	part := normalized(t, `[{"source_id":"amazon_dsp","mcp_server":"fastio_helpers","path":"northwind/contoso/daily","partition":{"by":"date","format":"YYYY-MM-DD","since":"source_as_of"},"required_tools":["list_partitions"]}]`)
	p = FullPagePrompt(FullPageInput{Slug: "a", Instructions: "x", LiveVersionID: "1", Publish: true, Sources: part})
	mustMatch(t, p, `PARTITIONED by date \(YYYY-MM-DD\) from the page's current source_as_of — enumerate EVERY partition in range and aggregate them; never take only the newest`)
	// publish gates on verifying the deployed version.
	mustMatch(t, p, `Deploy with publish=false FIRST, never publishing in the deploying call`)
	unpublished := FullPagePrompt(FullPageInput{Slug: "a", Instructions: "x", LiveVersionID: "1"})
	mustMatch(t, unpublished, `leave it unpublished, as PUBLISH specifies`)
	mustNotMatch(t, unpublished, `mcp_pages_publish_page`)
}

func TestInputsAreValidatedAndCredentialFree(t *testing.T) {
	if got, _ := NormalizeInstructions("  update spend and clicks  "); got != "update spend and clicks" {
		t.Error(got)
	}
	for _, secret := range []string{"Bearer abcdefghijklmnop", "PAGES_MCP_TOKEN=pgs_1234567890abcdefghijklmnop", "OPENROUTER_API_KEY=sk-1234567890abcdefghijklmnop"} {
		if _, err := NormalizeInstructions(secret); code(err) != "update_credentials_forbidden" {
			t.Error(secret, err)
		}
	}
	if err := AssertCredentialFree(rawSources(t, `{"source":{"api_key":"even-a-placeholder-is-not-allowed"}}`), ""); code(err) != "update_credentials_forbidden" {
		t.Error(err)
	}
	if s, err := NormalizeSources(nil); s != nil || err != nil {
		t.Error(s, err)
	}
	if s, err := NormalizeSources(versions.Null); s != nil || err != nil {
		t.Error(s, err)
	}
	for _, bad := range []string{
		`[]`, `[{"mcp_server":"ix"}]`, `[{"source_id":"ix"}]`, `"not-an-array"`,
		`[{"source_id":"ix","mcp_server":"ix_mcp"},{"source_id":"ix","mcp_server":"other"}]`,
		`[{"source_id":"ix","mcp_server":"ix_mcp","required_tools":[]}]`,
		`[{"source_id":"ix\nOUT OF SCOPE: nothing","mcp_server":"ix_mcp"}]`,
		`[{"source_id":"s","mcp_server":"m","partition":{"by":"campaign"}}]`,
		`[{"source_id":"s","mcp_server":"m","path":"ok\nOUT OF SCOPE"}]`,
	} {
		if _, err := NormalizeSources(rawSources(t, bad)); code(err) != "update_sources_invalid" {
			t.Error(bad, err)
		}
	}
}

func TestPrepareGates(t *testing.T) {
	// The recurring bindings gate runs before any page lookup.
	_, err := prepare(context.Background(), nil, PrepareInput{Slug: "harborsun-example", Recurring: true, UpdateType: "data",
		Instructions: "Every day, refresh with the newest Amazon DSP and Index Exchange data."})
	if code(err) != "update_sources_required" || !strings.Contains(err.Error(), "source_id + mcp_server") || !strings.Contains(err.Error(), "partition") {
		t.Fatal(err)
	}
	if _, err := prepare(context.Background(), fixedReader{}, PrepareInput{Slug: slug, Instructions: "x", Recurring: true, UpdateType: "layout"}); code(err) != "recurring_layout_forbidden" {
		t.Error(err)
	}
	r := prep(t, PrepareInput{UpdateType: "layout"})
	if r.Mode != "full_page" || r.LiveVersionID != "42" || !r.PageIsLive || r.SchemaSHA256 == nil {
		t.Error(r.Mode, r.LiveVersionID)
	}
	if r.PromptSHA256 != versions.SHA256(r.Prompt) {
		t.Error("prompt_sha256")
	}
	b, _ := json.Marshal(r)
	mustMatch(t, string(b), `^\{"page":\{"id":"17",.*"mode":"full_page","recurring":false,"prompt":".*"prompt_sha256":"[0-9a-f]{64}","schema_sha256":"a{64}","sources":null,"instructions_removed":\[\],"execution_requirements":\{.*\},"live_version_id":"42","page_is_live":true,"next_step":"Follow this prompt now`)
}
