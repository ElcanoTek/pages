// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package updateprompts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/nodedefaults"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/versions"
)

// testdata/golden.json was recorded from the Node release's update-prompts
// module (prompts and other long strings pinned by SHA-256).
// Every prompt the agents read must be byte-identical to the JavaScript one.

func loadGolden(t *testing.T) *jsonv.Object {
	t.Helper()
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonv.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return v.(*jsonv.Object)
}

// pin hashes long strings the way the recorder did.
func pin(v any) any {
	switch x := v.(type) {
	case string:
		if utf16Len(x) > 600 {
			sum := sha256.Sum256([]byte(x))
			return "sha256:" + hex.EncodeToString(sum[:])
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = pin(e)
		}
		return out
	case *jsonv.Object:
		o := jsonv.NewObject()
		x.Range(func(k string, e any) bool { o.Set(k, pin(e)); return true })
		return o
	}
	return v
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// asJSON round-trips a Go result into the JSON value model.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := httpx.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := jsonv.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return pin(out)
}

func errJSON(err error) any {
	e, ok := apierror.As(err)
	if !ok {
		return jsonv.ObjectOf("error", err.Error())
	}
	return jsonv.ObjectOf("error", jsonv.ObjectOf("status", float64(e.Status), "code", e.Code, "message", e.Message))
}

func captured(t *testing.T, v any, err error) any {
	if err != nil {
		return errJSON(err)
	}
	return jsonv.ObjectOf("ok", asJSON(t, v))
}

func same(t *testing.T, label string, want, got any) {
	t.Helper()
	w, g := jsonv.Stringify(want), jsonv.Stringify(got)
	if w != g {
		t.Errorf("%s:\n want %s\n  got %s", label, w, g)
	}
}

func cases(t *testing.T, golden *jsonv.Object, name string) []*jsonv.Object {
	t.Helper()
	raw := golden.Value("cases").(*jsonv.Object).Value(name).([]any)
	out := make([]*jsonv.Object, len(raw))
	for i, c := range raw {
		out[i] = c.(*jsonv.Object)
	}
	if len(out) == 0 {
		t.Fatalf("no %s cases", name)
	}
	return out
}

func str(o *jsonv.Object, k string) string { s, _ := o.Value(k).(string); return s }
func boolean(o *jsonv.Object, k string) bool {
	b, _ := o.Value(k).(bool)
	return b
}

func sourcesArg(t *testing.T, o *jsonv.Object) []Source {
	t.Helper()
	s, err := NormalizeSources(o.Value("sources"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGoldenPrompts(t *testing.T) {
	// Recorded from the JavaScript at its size defaults, which the prompts quote.
	nodedefaults.Setenv(t)
	g := loadGolden(t)
	for i, c := range cases(t, g, "managed") {
		a := c.Value("args").(*jsonv.Object)
		got := ManagedPrompt(ManagedInput{Slug: str(a, "slug"), Instructions: str(a, "instructions"), SchemaSHA256: str(a, "schemaSha256"),
			Publish: boolean(a, "publish"), Recurring: boolean(a, "recurring"), Sources: sourcesArg(t, a), KeepFullRoster: !boolean(a, "narrowRoster")})
		same(t, fmt.Sprintf("managed[%d]", i), c.Value("out"), pin(got))
	}
	for i, c := range cases(t, g, "full_page") {
		a := c.Value("args").(*jsonv.Object)
		got := FullPagePrompt(FullPageInput{Slug: str(a, "slug"), Instructions: str(a, "instructions"), LiveVersionID: str(a, "liveVersionId"),
			Publish: boolean(a, "publish"), Sources: sourcesArg(t, a)})
		same(t, fmt.Sprintf("full_page[%d]", i), c.Value("out"), pin(got))
	}
	for i, c := range cases(t, g, "migration") {
		a := c.Value("args").(*jsonv.Object)
		got := MigrationPrompt(MigrationInput{Slug: str(a, "slug"), Instructions: str(a, "instructions"), LiveVersionID: str(a, "liveVersionId"),
			Publish: boolean(a, "publish"), Recurring: boolean(a, "recurring"), Sources: sourcesArg(t, a)})
		same(t, fmt.Sprintf("migration[%d]", i), c.Value("out"), pin(got))
	}
	for i, c := range cases(t, g, "adaptive") {
		a := c.Value("args").(*jsonv.Object)
		got := AdaptivePrompt(AdaptiveInput{Slug: str(a, "slug"), Instructions: str(a, "instructions"), SchemaSHA256: str(a, "schemaSha256"),
			LiveVersionID: str(a, "liveVersionId"), Publish: boolean(a, "publish"), Sources: sourcesArg(t, a)})
		same(t, fmt.Sprintf("adaptive[%d]", i), c.Value("out"), pin(got))
	}
	for i, c := range cases(t, g, "template") {
		a := c.Value("args").(*jsonv.Object)
		got := TemplatePrompt(TemplateInput{Slug: str(a, "slug"), Instructions: str(a, "instructions"), Template: str(a, "template"),
			Revision: int(a.Value("revision").(float64)), ConfigSchemaSHA256: str(a, "configSchemaSha256"), LiveVersionID: str(a, "liveVersionId"),
			Publish: boolean(a, "publish")})
		same(t, fmt.Sprintf("template[%d]", i), c.Value("out"), pin(got))
	}
	for i, c := range cases(t, g, "freshness") {
		same(t, fmt.Sprintf("freshness[%d]", i), c.Value("out"), pin(FreshnessGateStep(boolean(c, "recurring"))))
	}
}

func TestGoldenInputs(t *testing.T) {
	g := loadGolden(t)
	for i, c := range cases(t, g, "task_only") {
		same(t, fmt.Sprintf("task_only[%d] %q", i, str(c, "text")), c.Value("out"), asJSON(t, TaskOnlyInstructions(str(c, "text"))))
	}
	for i, c := range cases(t, g, "sources") {
		value := c.Value("value")
		if value == "__undefined__" {
			value = nil
		}
		got, err := NormalizeSources(value)
		var gotJSON any
		if err == nil && got == nil {
			gotJSON = jsonv.ObjectOf("ok", nil)
		} else {
			gotJSON = captured(t, got, err)
		}
		same(t, fmt.Sprintf("sources[%d]", i), c.Value("out"), gotJSON)
	}
	for i, c := range cases(t, g, "workflow") {
		got := SourcesFromWorkflow(c.Value("workflow"))
		var gotJSON any
		if got != nil {
			gotJSON = asJSON(t, got)
		}
		same(t, fmt.Sprintf("workflow[%d]", i), c.Value("out"), gotJSON)
	}
	for i, c := range cases(t, g, "requirements") {
		got := ExecutionRequirements(sourcesArg(t, c), str(c, "mode"),
			RequirementOptions{Slug: str(c, "slug"), Recurring: boolean(c, "recurring"), KeepFullRoster: !boolean(c, "narrowRoster")})
		same(t, fmt.Sprintf("requirements[%d]", i), c.Value("out"), asJSON(t, got))
	}
}

// stubReader serves one recorded page state.
type stubReader struct{ state *jsonv.Object }

func (s stubReader) getPage(context.Context, string) (*versions.PageView, error) {
	p, ok := s.state.Value("page").(*jsonv.Object)
	if !ok {
		return nil, apierror.NotFound("page not found", "page_not_found", nil)
	}
	id, _ := jsx.ParseID(p.Value("id"))
	pv := &versions.PageView{Page: versions.Page{ID: id, Slug: str(p, "slug"), Title: str(p, "title"),
		Disabled: boolean(p, "disabled"), RequireApproval: boolean(p, "require_approval")}}
	if v, ok := p.Value("published_version_id").(string); ok {
		pv.Page.PublishedVersionID, _ = jsx.ParseID(v)
		pv.Published = &versions.VersionDetail{ID: pv.Page.PublishedVersionID}
	}
	return pv, nil
}

func (s stubReader) getPageData(context.Context, string) (*versions.PageDataView, error) {
	switch m := s.state.Value("managed").(type) {
	case nil:
		return nil, apierror.Conflict("not managed", "page_not_data_managed", nil)
	case string:
		return nil, apierror.Conflict("broken", m, nil)
	case *jsonv.Object:
		return &versions.PageDataView{SchemaSHA256: str(m, "schema_sha256")}, nil
	}
	return nil, nil
}

func (s stubReader) binding(context.Context, string) (*templates.Binding, error) {
	switch b := s.state.Value("binding").(type) {
	case string:
		return nil, apierror.BadRequest("ignored", "whatever", nil)
	case *jsonv.Object:
		return &templates.Binding{Template: str(b, "template"), Revision: int(b.Value("revision").(float64)),
			ConfigSchemaSHA256: str(b, "config_schema_sha256")}, nil
	}
	return nil, nil
}

func TestGoldenPrepare(t *testing.T) {
	// Recorded from the JavaScript at its size defaults, which the prompts quote.
	nodedefaults.Setenv(t)
	g := loadGolden(t)
	states := g.Value("states").(*jsonv.Object)
	for i, c := range cases(t, g, "prepare") {
		a := c.Value("args").(*jsonv.Object)
		in := PrepareInput{Slug: a.Value("slug"), Instructions: a.Value("instructions"), Recurring: boolean(a, "recurring"),
			UpdateType: str(a, "updateType"), Sources: a.Value("sources"), AllowUnboundRecurring: boolean(a, "allowUnboundRecurring")}
		var n int
		if _, err := fmt.Sscanf(str(a, "instructions"), "__x%d__", &n); err == nil {
			in.Instructions = strings.Repeat("x", n)
		}
		if p, ok := a.Get("publish"); ok {
			b := p.(bool)
			in.Publish = &b
		}
		res, err := prepare(context.Background(), stubReader{states.Value(str(c, "state")).(*jsonv.Object)}, in)
		label := fmt.Sprintf("prepare[%d] state=%s args=%s", i, str(c, "state"), jsonv.Stringify(pin(a)))
		want := c.Value("out").(*jsonv.Object)
		if err != nil {
			same(t, label, want, errJSON(err))
			continue
		}
		wantOK, ok := want.Value("ok").(*jsonv.Object)
		if !ok {
			t.Errorf("%s: want %s, got a result", label, jsonv.Stringify(want))
			continue
		}
		got := asJSON(t, res).(*jsonv.Object)
		// The stubbed page carried only some of the page keys; compare those.
		wantPage, gotPage := wantOK.Value("page").(*jsonv.Object), got.Value("page").(*jsonv.Object)
		wantPage.Range(func(k string, v any) bool {
			same(t, label+" page."+k, v, gotPage.Value(k))
			return true
		})
		wantOK, got = wantOK.Clone(), got.Clone()
		wantOK.Delete("page")
		got.Delete("page")
		// A reviewed addition the JavaScript never had: a recurring next_step
		// ends by handing the task's cadence back to Pages.
		if step, ok := got.Value("next_step").(string); ok && boolean(a, "recurring") {
			trimmed := strings.TrimSuffix(step, ScheduleHandoffStep)
			if trimmed == step && strings.HasPrefix(step, "Use prompt verbatim") {
				t.Errorf("%s: a recurring next_step ends with the schedule handoff", label)
			}
			got.Set("next_step", trimmed)
		}
		same(t, label, wantOK, got)
	}
}
