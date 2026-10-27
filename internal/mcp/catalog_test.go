// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/mcpproto"
	"github.com/ElcanoTek/pages/internal/nodedefaults"
	"github.com/ElcanoTek/pages/internal/pagepatch"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/uploads"
)

// jsCatalog is testdata/js-catalog.json: the JavaScript tools/list entries
// and instructions recorded from the Node release at its defaults.
type jsCatalog struct {
	Instructions           string
	DataUpdateInstructions string
	Tools                  []*jsonv.Object
}

func loadJSCatalog(t *testing.T) jsCatalog {
	t.Helper()
	raw, err := os.ReadFile("testdata/js-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonv.ParseLossless(raw)
	if err != nil {
		t.Fatal(err)
	}
	o := v.(*jsonv.Object)
	c := jsCatalog{
		Instructions:           o.Value("instructions").(string),
		DataUpdateInstructions: o.Value("data_update_instructions").(string),
	}
	for _, e := range o.Value("tools").([]any) {
		c.Tools = append(c.Tools, e.(*jsonv.Object))
	}
	return c
}

// nodeCatalog is the catalog at the Node release's limits. pagepatch's
// bounds are constants the Go server raised (not settings), so they are
// pinned here; everything else is read from nodedefaults.
func nodeCatalog(t *testing.T) *catalog {
	t.Helper()
	nodedefaults.Setenv(t)
	l := currentLimits()
	l.patchMaxEdits, l.patchMaxFindChars, l.patchMaxReplace, l.patchMaxCount = 25, 8192, 128*1024, 100
	return buildCatalog(l)
}

// postPortTools are tools added after the Node release was recorded. The
// JavaScript catalog and the SDK golden never had them, so the comparisons
// leave them out; TestRegistryExposesThePromisedTools and the docs allowlist
// pin them instead.
var postPortTools = map[string]bool{"set_page_refresh_schedule": true, "list_refresh_health": true}

// legacyCatalog is the catalog without postPortTools.
func legacyCatalog(cat *catalog) []*toolDef {
	var out []*toolDef
	for _, def := range cat.tools {
		if !postPortTools[def.name] {
			out = append(out, def)
		}
	}
	return out
}

// legacyListPagesFreshness is the list_pages sentence that
// listPagesHealthDescription replaced.
const legacyListPagesFreshness = "Each row carries freshness: source_as_of, refreshed_at, checked_at, and days_since_* for each. That is how you find dashboards that have quietly stopped updating — Pages states no opinion on what counts as overdue, because it does not know any page's expected cadence."

// undoRefreshHealth takes refresh health back out of a tools/list entry:
// the description sentences and the refresh_health output property, both on
// get_page_data and on every list_pages row.
func undoRefreshHealth(tool *jsonv.Object) {
	desc := tool.Value("description").(string)
	desc = strings.Replace(desc, listPagesHealthDescription, legacyListPagesFreshness, 1)
	desc = strings.Replace(desc, pageDataHealthDescription, "", 1)
	tool.Set("description", desc)
	drop := func(schema *jsonv.Object) {
		if props, ok := schema.Value("properties").(*jsonv.Object); ok {
			props.Delete("refresh_health")
		}
		if required, ok := schema.Value("required").([]any); ok {
			kept := []any{}
			for _, r := range required {
				if r != "refresh_health" {
					kept = append(kept, r)
				}
			}
			schema.Set("required", kept)
		}
	}
	out, ok := tool.Value("outputSchema").(*jsonv.Object)
	if !ok {
		return
	}
	drop(out)
	if props, ok := out.Value("properties").(*jsonv.Object); ok {
		if pages, ok := props.Value("pages").(*jsonv.Object); ok {
			if items, ok := pages.Value("items").(*jsonv.Object); ok {
				drop(items)
			}
		}
	}
	if name, _ := tool.Value("name").(string); name == "create_page_from_template" || name == "deploy_page" || name == "deploy_page_upload" {
		if in, ok := tool.Value("inputSchema").(*jsonv.Object); ok {
			if props, ok := in.Value("properties").(*jsonv.Object); ok {
				props.Delete("workspace_id")
			}
		}
	}
}

// legacyDefinition is a Go tools/list entry with the deliberate additions
// the JavaScript never had removed: issue #109's data_usage output field and
// the sentence describing it, refresh health (undoRefreshHealth) and the
// source binding scope (undoSourceScope).
func legacyDefinition(t *testing.T, def *toolDef) *jsonv.Object {
	t.Helper()
	tool := def.protoTool(nil)
	o := jsonv.ObjectOf("name", tool.Name, "title", tool.Title, "description", strings.Replace(tool.Description, usageDescription, "", 1))
	for _, part := range []struct {
		key string
		raw []byte
	}{{"inputSchema", tool.InputSchema}, {"annotations", tool.Annotations}} {
		v, err := jsonv.ParseLossless(part.raw)
		if err != nil {
			t.Fatal(err)
		}
		o.Set(part.key, v)
	}
	o.Set("execution", jsonv.ObjectOf("taskSupport", "forbidden"))
	out, err := jsonv.ParseLossless(tool.OutputSchema)
	if err != nil {
		t.Fatal(err)
	}
	schema := out.(*jsonv.Object)
	if props, ok := schema.Value("properties").(*jsonv.Object); ok {
		props.Delete("data_usage")
	}
	o.Set("outputSchema", schema)
	undoRefreshHealth(o)
	undoSourceScope(o)
	return o
}

// undoSourceScope takes the source binding's scope property (added after the
// port) back out of every schema that carries a binding: an object schema
// whose properties include source_id and mcp_server.
func undoSourceScope(v any) {
	switch x := v.(type) {
	case *jsonv.Object:
		if props, ok := x.Value("properties").(*jsonv.Object); ok {
			if _, hasID := props.Get("source_id"); hasID {
				if _, hasServer := props.Get("mcp_server"); hasServer {
					props.Delete("scope")
				}
			}
		}
		x.Range(func(_ string, e any) bool { undoSourceScope(e); return true })
	case []any:
		for _, e := range x {
			undoSourceScope(e)
		}
	}
}

func TestCatalogMatchesJavaScriptAtNodeDefaults(t *testing.T) {
	js := loadJSCatalog(t)
	tools := legacyCatalog(nodeCatalog(t))
	if len(tools) != len(js.Tools) {
		t.Fatalf("catalog has %d tools, JavaScript %d", len(tools), len(js.Tools))
	}
	cat := nodeCatalog(t)
	for i, def := range tools {
		want := jsonv.Stringify(js.Tools[i])
		got := jsonv.Stringify(legacyDefinition(t, def))
		if got != want {
			t.Errorf("tool %s differs from the JavaScript definition:\n got: %s\nwant: %s", def.name, firstDiff(got, want), firstDiff(want, got))
		}
	}
	if cat.instructions != js.Instructions {
		t.Errorf("instructions differ:\n got: %s\nwant: %s", firstDiff(cat.instructions, js.Instructions), firstDiff(js.Instructions, cat.instructions))
	}
	if cat.dataUpdateInstructions != js.DataUpdateInstructions {
		t.Error("data_update instructions differ")
	}
}

// firstDiff is a window around the first byte where a and b differ.
func firstDiff(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo, hi := max(0, i-80), min(len(a), i+120)
	return a[lo:hi]
}

// The golden pins the tools/list body by digest. The Go list is the
// JavaScript one plus the reviewed additions, so the digest is checked with
// those taken out again.
func TestToolsListMatchesGoldenDigest(t *testing.T) {
	b := []byte(`{"result":{"tools":[`)
	for i, def := range legacyCatalog(nodeCatalog(t)) {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, jsonv.Stringify(legacyDefinition(t, def))...)
	}
	b = append(b, `]},"jsonrpc":"2.0","id":4}`...)
	sum := sha256.Sum256(b)
	got := "sha256:" + hex.EncodeToString(sum[:]) + " bytes:" + itoa(len(b))
	if want := goldenBody(t, "tools/list"); got != want {
		t.Fatalf("tools/list digest %s, golden %s", got, want)
	}
}

func itoa(n int) string { return jsonv.FormatNumber(float64(n)) }

func goldenBody(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/mcp-wire.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonv.ParseLossless(raw)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := v.(*jsonv.Object).Value(name).(*jsonv.Object)
	if !ok {
		t.Fatalf("golden has no case %q", name)
	}
	return c.Value("body").(string)
}

// The live catalog advertises the limits in force, not the Node numbers.
func TestCatalogAdvertisesCurrentLimits(t *testing.T) {
	cat := currentCatalog()
	input := func(tool string) *jsonv.Object {
		return cat.byName[tool].input.JSONSchema(ioInput).Value("properties").(*jsonv.Object)
	}
	if got := input("create_upload_ticket").Value("total_bytes").(*jsonv.Object).Value("maximum"); got != float64(uploads.MaxUploadBytes()) {
		t.Errorf("create_upload_ticket total_bytes maximum %v, want %d", got, uploads.MaxUploadBytes())
	}
	if got := input("append_page_upload").Value("chunk_base64").(*jsonv.Object).Value("maxLength"); got != float64(uploads.MaxChunkBase64Chars()) {
		t.Errorf("chunk_base64 maxLength %v, want %d", got, uploads.MaxChunkBase64Chars())
	}
	edits := input("patch_page").Value("edits").(*jsonv.Object)
	if edits.Value("maxItems") != float64(pagepatch.MaxEdits) {
		t.Errorf("patch_page edits maxItems %v, want %d", edits.Value("maxItems"), pagepatch.MaxEdits)
	}
	if !strings.Contains(cat.byName["create_upload_ticket"].description, "staged raw file "+itoa(uploads.MaxUploadBytes())) {
		t.Error("create_upload_ticket does not quote the staged-file limit in force")
	}
	t.Setenv("PAGES_UPLOAD_MAX_BYTES", "4mb")
	t.Setenv("PAGES_MCP_MAX_INLINE_HTML_BYTES", "30000")
	moved := currentCatalog()
	if moved == cat {
		t.Fatal("changing a limit must rebuild the catalog")
	}
	if got := moved.byName["start_page_upload"].input.JSONSchema(ioInput).Value("properties").(*jsonv.Object).Value("total_bytes").(*jsonv.Object).Value("maximum"); got != float64(4<<20) {
		t.Errorf("total_bytes maximum %v after PAGES_UPLOAD_MAX_BYTES=4mb", got)
	}
	if !strings.Contains(moved.byName["deploy_page"].description, "over 30,000 UTF-8 bytes") || !strings.Contains(moved.instructions, "over 30,000 UTF-8 bytes") {
		t.Error("the inline HTML limit is not derived from PAGES_MCP_MAX_INLINE_HTML_BYTES")
	}
}

// Issue #109: the managed-data reads and writes advertise data_usage.
func TestDataUsageIsAdvertised(t *testing.T) {
	cat := currentCatalog()
	for _, name := range []string{"get_page_data", "update_page_data", "update_page_data_upload"} {
		def := cat.byName[name]
		props := def.output.JSONSchema(ioOutput).Value("properties").(*jsonv.Object)
		keys := props.Keys()
		if keys[len(keys)-1] != "data_usage" {
			t.Errorf("%s: data_usage is not the last output property (%v)", name, keys)
		}
		usage := props.Value("data_usage").(*jsonv.Object).Value("properties").(*jsonv.Object)
		if got := usage.Keys(); !slices.Equal(got, []string{"payload_bytes", "envelope_bytes", "limit_bytes", "headroom_bytes"}) {
			t.Errorf("%s: data_usage keys %v", name, got)
		}
		if !strings.Contains(def.description, "headroom_bytes") {
			t.Errorf("%s: description does not mention the headroom", name)
		}
		required, _ := def.output.JSONSchema(ioOutput).Value("required").([]any)
		if slices.Contains(required, any("data_usage")) {
			t.Errorf("%s: data_usage must be optional", name)
		}
	}
}

// The revision source enum must stay in step with the domain's, or every
// template registered by `pages template sync` fails output validation.
func TestTemplateRevisionSourcesMatchDomain(t *testing.T) {
	enum := templateRevisionSchema.JSONSchema(ioOutput).Value("properties").(*jsonv.Object).Value("source").(*jsonv.Object).Value("enum").([]any)
	var got []string
	for _, e := range enum {
		got = append(got, e.(string))
	}
	if !slices.Equal(got, templates.Sources) {
		t.Fatalf("revision source enum %v, templates.Sources %v", got, templates.Sources)
	}
}

func TestDataUpdateTokensSeeOnlyTheirTools(t *testing.T) {
	agent := &tokens.Agent{Actor: "x", ActorType: "agent", Scope: tokens.ScopeDataUpdate, TokenID: 1, AllowedSlugs: []string{"a"}}
	got := sortedNames(visibleTools(currentCatalog(), agent))
	want := slices.Clone(tokens.DataUpdateTools)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("data_update tools %v, want %v", got, want)
	}
	s, err := NewServer(agent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.ToolNames(), []string{"get_page_data", "record_refresh_check", "update_page_data"}) {
		t.Fatalf("server tools %v", s.ToolNames())
	}
	if broad := len(visibleTools(currentCatalog(), &tokens.Agent{Scope: tokens.ScopeDeploy})); broad != len(currentCatalog().tools) {
		t.Fatalf("a deploy token sees %d tools", broad)
	}
}

func TestCreateToolsAdvertiseWorkspaceID(t *testing.T) {
	cat := currentCatalog()
	for _, name := range []string{"create_page_from_template", "deploy_page", "deploy_page_upload"} {
		props := cat.byName[name].input.JSONSchema(ioInput).Value("properties").(*jsonv.Object)
		if !props.Has("workspace_id") {
			t.Errorf("%s does not advertise workspace_id", name)
		}
	}
}

var _ = mcpproto.InvalidParams
