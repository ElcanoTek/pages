// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

// Differential tests against golden files produced by the JavaScript
// implementation itself: the Node release's page-data module, Ajv, parse5 and
// RegExp run once over a fixed corpus, recorded into testdata/.

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/nodedefaults"
)

func readGolden(t *testing.T, name string) []*jsonv.Object {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []*jsonv.Object
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		v, err := jsonv.ParseString(sc.Text())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rows = append(rows, v.(*jsonv.Object))
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func str(o *jsonv.Object, k string) string { s, _ := o.Value(k).(string); return s }

func TestGoldenNumbers(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	for _, r := range readGolden(t, "numbers.jsonl") {
		v, err := jsonv.ParseString(str(r, "text"))
		if err != nil {
			t.Fatal(err)
		}
		f := v.(float64)
		if got := jsonv.FormatNumber(f); got != str(r, "str") && (r.Value("isNegZero") != true || got != "0") {
			t.Errorf("FormatNumber(%s) = %s, want %s", str(r, "text"), got, str(r, "str"))
		}
		if want, ok := r.Value("fixed6").(string); ok {
			if got := jsToFixed(f, 6); got != want {
				t.Errorf("toFixed(%s) = %s, want %s", str(r, "text"), got, want)
			}
		}
	}
}

func TestGoldenCanonicalJSON(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	for _, r := range readGolden(t, "canonical.jsonl") {
		v, err := jsonv.ParseString(str(r, "json"))
		if err != nil {
			t.Fatal(err)
		}
		if got := jsonv.Stringify(v); got != str(r, "json") {
			t.Errorf("Stringify round trip: %s != %s", got, str(r, "json"))
		}
		c, err := CanonicalJSON(v)
		if err != nil || c != str(r, "canonical") {
			t.Errorf("canonical %s: got %s want %s (%v)", str(r, "json"), c, str(r, "canonical"), err)
		}
		if h, _ := SemanticHash(v); h != str(r, "sha") {
			t.Errorf("hash of %s", str(r, "json"))
		}
		if e := datalimits.EscapedJSON(v); e != str(r, "escaped") {
			t.Errorf("escaped %s: %s", str(r, "json"), e)
		}
	}
}

func TestGoldenPatterns(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	rows := readGolden(t, "patterns.jsonl")
	var inputs []string
	for _, in := range rows[0].Value("inputs").([]any) {
		inputs = append(inputs, in.(string))
	}
	unsupported := 0
	for _, r := range rows[1:] {
		p := str(r, "pattern")
		if got := IsSafePattern(p); got != (r.Value("safe") == true) {
			t.Errorf("IsSafePattern(%q) = %v", p, got)
		}
		for _, flags := range []string{"u", ""} {
			suffix := flags
			if suffix == "" {
				suffix = "none"
			}
			re, err := compileJSRegex(p, flags)
			if r.Value("err_"+suffix) == true {
				if err == nil {
					t.Errorf("/%s/%s: JavaScript rejects it, the translator accepted it", p, flags)
				} else if errors.Is(err, errUnsupported) {
					t.Errorf("/%s/%s: JavaScript rejects it, the translator called it unsupported", p, flags)
				}
				continue
			}
			if err != nil {
				if errors.Is(err, errUnsupported) {
					unsupported++
					continue
				}
				t.Errorf("/%s/%s: JavaScript accepts it, translator: %v", p, flags, err)
				continue
			}
			for i, m := range r.Value("match_" + suffix).([]any) {
				if re.MatchString(inputs[i]) != (m == true) && (flags == "u" || !strings.ContainsAny(inputs[i], "😀")) {
					t.Errorf("/%s/%s.test(%q) = %v, JavaScript %v", p, flags, inputs[i], m != true, m)
				}
			}
		}
	}
	t.Logf("%d valid JavaScript patterns failed to compile", unsupported)
}

func utf16Offset(s string, byteOff int) int { return len(utf16.Encode([]rune(s[:byteOff]))) }

func TestGoldenHTMLBlocks(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	for _, r := range readGolden(t, "html.jsonl") {
		html := str(r, "html")
		matches := map[string][]*Block{"pages-data": nil, "pages-data-schema": nil, "pages-config": nil}
		err := walkHTML(html, matches)
		if want, ok := r.Value("error").(string); ok {
			id := strings.TrimPrefix(want, "explicit:")
			if err == nil || !strings.Contains(err.Error(), "the "+id+" block") {
				t.Errorf("%q: want error %s, got %v", html, want, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", html, err)
			continue
		}
		want := r.Value("matches").(*jsonv.Object)
		for _, id := range want.Keys() {
			wl := want.Value(id).([]any)
			if len(wl) != len(matches[id]) {
				t.Errorf("%q #%s: %d blocks, want %d", html, id, len(matches[id]), len(wl))
				continue
			}
			for i, w := range wl {
				wb := w.(*jsonv.Object)
				b := matches[id][i]
				got := []int{utf16Offset(html, b.ContentStart), utf16Offset(html, b.ContentEnd), utf16Offset(html, b.ElementStart), utf16Offset(html, b.ElementEnd)}
				exp := []float64{wb.Value("contentStart").(float64), wb.Value("contentEnd").(float64), wb.Value("elementStart").(float64), wb.Value("elementEnd").(float64)}
				for k := range got {
					if float64(got[k]) != exp[k] {
						t.Errorf("%q #%s[%d]: offsets %v, want %v", html, id, i, got, exp)
						break
					}
				}
				wantType, _ := wb.Value("type").(string)
				if b.Type != wantType {
					t.Errorf("%q #%s: type %q want %q", html, id, b.Type, wantType)
				}
			}
		}
	}
}

func compileForTest(schema any) (*Validator, error) {
	o, ok := schema.(*jsonv.Object)
	if !ok {
		return nil, errCompile
	}
	if err := validateMeta(o); err != nil {
		return nil, err
	}
	return compileValidator(o)
}

func TestGoldenAjv(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	raw, err := os.ReadFile(filepath.Join("testdata", "ajv-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	corpusV, err := jsonv.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	corpus := corpusV.([]any)
	results := readGolden(t, "ajv.jsonl")
	if len(corpus) != len(results) {
		t.Fatalf("corpus %d vs results %d", len(corpus), len(results))
	}
	for i, c := range corpus {
		cs := c.(*jsonv.Object)
		schema, data := cs.Value("schema"), cs.Value("data")
		want := results[i]
		v, err := compileForTest(schema)
		if want.Value("compile") != true {
			if err == nil {
				t.Errorf("case %d: %s compiled; Ajv: %s", i, jsonv.Stringify(schema), str(want, "reason"))
			}
			continue
		}
		if err != nil {
			t.Errorf("case %d: %s: %v", i, jsonv.Stringify(schema), err)
			continue
		}
		errs := v.Validate(data)
		got := make([]any, len(errs))
		for k, e := range errs {
			got[k] = []any{e.InstancePath, e.SchemaPath, e.Keyword, e.Message}
		}
		if g, w := jsonv.Stringify(got), jsonv.Stringify(want.Value("errors")); g != w {
			t.Errorf("case %d: schema %s data %s\n got %s\nwant %s", i, jsonv.Stringify(schema), jsonv.Stringify(data), g, w)
		}
	}
}

func TestGoldenProfiles(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	rows := readGolden(t, "profiles.jsonl")
	var payloads []any
	for _, r := range rows {
		switch str(r, "kind") {
		case "profile":
			data := r.Value("data")
			payloads = append(payloads, data)
			p := ProfileData(data)
			if got := jsonv.Stringify(p); got != str(r, "profile") {
				t.Errorf("profile of %.200s\n got %.600s\nwant %.600s", jsonv.Stringify(data), got, str(r, "profile"))
			}
			cov, tr := BoundedCoverageProfile(p, 0)
			if got := jsonv.Stringify(cov); got != str(r, "coverage") || tr != (r.Value("truncated") == true) {
				t.Errorf("coverage of %.200s\n got %v %.600s\nwant %v %.600s", jsonv.Stringify(data), tr, got, r.Value("truncated"), str(r, "coverage"))
			}
			small, tr := BoundedCoverageProfile(p, 300)
			if got := jsonv.Stringify(small); got != str(r, "small") || tr != (r.Value("smallTruncated") == true) {
				t.Errorf("small coverage of %.200s\n got %v %s\nwant %v %s", jsonv.Stringify(data), tr, got, r.Value("smallTruncated"), str(r, "small"))
			}
		case "compare":
			prev, next := payloads[int(r.Value("prev").(float64))], payloads[int(r.Value("next").(float64))]
			opts := CompareOptions{Expected: r.Value("expected")}
			if r.Value("shas") == true {
				opts.PreviousDataSHA = mustHash(prev)
				opts.NextDataSHA = mustHash(next)
			}
			got := jsonv.Stringify(CompareDataProfiles(ProfileData(prev), ProfileData(next), opts))
			if got != str(r, "warnings") {
				t.Errorf("compare %v→%v (%s, shas %v)\n got %.800s\nwant %.800s", r.Value("prev"), r.Value("next"), jsonv.Stringify(r.Value("expected")), r.Value("shas"), got, str(r, "warnings"))
			}
		case "expect":
			data := payloads[int(r.Value("data").(float64))]
			err := AssertExpectedProfile(ProfileData(data), r.Value("expect"), "")
			checkError(t, "expect "+jsonv.Stringify(r.Value("expect")), err, r.Value("error"))
		}
	}
}

// checkError compares a Go error with a JS {status, code, message, details}.
func checkError(t *testing.T, what string, err error, want any) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Errorf("%s: unexpected error %v", what, err)
		}
		return
	}
	w := want.(*jsonv.Object)
	e, ok := apierror.As(err)
	if !ok {
		t.Errorf("%s: want %s error %q, got %v", what, str(w, "code"), str(w, "message"), err)
		return
	}
	if float64(e.Status) != w.Value("status") || e.Code != str(w, "code") || e.Message != str(w, "message") {
		t.Errorf("%s:\n got %d %s %q\nwant %v %s %q", what, e.Status, e.Code, e.Message, w.Value("status"), str(w, "code"), str(w, "message"))
		return
	}
	if g, wd := jsonv.Stringify(e.Details), jsonv.Stringify(w.Value("details")); g != wd {
		t.Errorf("%s details:\n got %s\nwant %s", what, g, wd)
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func asAny(o *jsonv.Object) any {
	if o == nil {
		return nil
	}
	return o
}

func TestGoldenManaged(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	now := time.Date(2026, 7, 17, 0, 2, 0, 0, time.UTC)
	for _, r := range readGolden(t, "managed.jsonl") {
		switch str(r, "kind") {
		case "parse":
			spec := PageSpec
			if str(r, "spec") == "template" {
				spec = TemplateSpec
			}
			html := str(r, "html")
			m, err := ParseManaged(html, spec)
			what := str(r, "spec") + " " + html
			checkError(t, what, err, r.Value("error"))
			if err != nil || r.Value("result") == nil {
				continue
			}
			want := r.Value("result").(*jsonv.Object)
			got := jsonv.ObjectOf("schema", m.Schema, "envelope", m.Envelope, "config", asAny(m.Config), "configSchema", asAny(m.ConfigSchema),
				"example", asAny(m.Example), "data_sha256", m.DataSHA256, "schema_sha256", m.SchemaSHA256, "config_sha256", nullable(m.ConfigSHA256),
				"config_schema_sha256", nullable(m.ConfigSchemaSHA256), "template_sha256", m.TemplateSHA256,
				"dataBlock", []any{float64(utf16Offset(html, m.DataBlock.ContentStart)), float64(utf16Offset(html, m.DataBlock.ContentEnd))})
			if g, w := jsonv.Stringify(got), jsonv.Stringify(want); g != w {
				t.Errorf("%s\n got %s\nwant %s", what, g, w)
			}
			for _, wr := range r.Value("writes").([]any) {
				w := wr.(*jsonv.Object)
				in := w.Value("write").(*jsonv.Object)
				var u BlockUpdate
				u.Data, u.SetData = in.Get("data")
				u.Config, u.SetConfig = in.Get("config")
				u.Envelope, u.SetEnvelope = in.Get("envelope")
				out, err := MaterializeBlocks(m, u, MaterializeOptions{SourceAsOf: in.Value("sourceAsOf"), Now: now})
				wwhat := what + " ← " + jsonv.Stringify(in)
				checkError(t, wwhat, err, w.Value("error"))
				if err != nil || w.Value("result") == nil {
					continue
				}
				got := jsonv.ObjectOf("html", out.HTML, "envelope", out.Envelope, "config", asAny(out.Config), "data_sha256", out.DataSHA256,
					"schema_sha256", out.SchemaSHA256, "config_sha256", nullable(out.ConfigSHA256), "config_schema_sha256", nullable(out.ConfigSchemaSHA256),
					"template_sha256", out.TemplateSHA256)
				if g, w := jsonv.Stringify(got), jsonv.Stringify(w.Value("result")); g != w {
					t.Errorf("%s\n got %s\nwant %s", wwhat, g, w)
				}
			}
		case "infer":
			s, err := InferConfigSchema(r.Value("config"))
			checkError(t, "infer "+jsonv.Stringify(r.Value("config")), err, r.Value("error"))
			if err == nil && jsonv.Stringify(s) != str(r, "schema") {
				t.Errorf("infer %s:\n got %s\nwant %s", jsonv.Stringify(r.Value("config")), jsonv.Stringify(s), str(r, "schema"))
			}
		case "ensure":
			res, err := EnsureConfigSchema(str(r, "html"))
			checkError(t, "ensure "+str(r, "html"), err, r.Value("error"))
			if err == nil {
				got := jsonv.ObjectOf("html", res.HTML, "generated", res.Generated, "schema", asAny(res.Schema))
				if g, w := jsonv.Stringify(got), jsonv.Stringify(r.Value("result")); g != w {
					t.Errorf("ensure:\n got %s\nwant %s", g, w)
				}
			}
		case "assemble":
			res, err := AssembleTemplate(str(r, "html"), r.Value("emptyData"), r.Value("exampleData"))
			what := "assemble " + jsonv.Stringify(r.Value("emptyData")) + " " + jsonv.Stringify(r.Value("exampleData"))
			checkError(t, what, err, r.Value("error"))
			if err == nil {
				got := jsonv.ObjectOf("html", res.HTML, "config", asAny(res.Config), "configSchema", asAny(res.ConfigSchema))
				if g, w := jsonv.Stringify(got), jsonv.Stringify(r.Value("result")); g != w {
					t.Errorf("%s:\n got %s\nwant %s", what, g, w)
				}
			}
		}
	}
}

func TestGoldenShippedTemplates(t *testing.T) {
	nodedefaults.Setenv(t) // recorded from the JavaScript at the Node release limits
	for _, r := range readGolden(t, "templates.jsonl") {
		raw, err := os.ReadFile(filepath.Join("..", "..", "templates", str(r, "name"), "template.html"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := ParseManaged(string(raw), TemplateSpec)
		if err != nil {
			t.Fatalf("%s: %v", str(r, "name"), err)
		}
		data := any(m.Example)
		if m.Example == nil {
			data = m.Data()
		}
		page, err := MaterializeBlocks(m, BlockUpdate{Config: m.Config, SetConfig: true, Data: data, SetData: true},
			MaterializeOptions{SourceAsOf: "2026-01-01T00:00:00Z", Now: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		p := ProfileData(data)
		got := map[string]string{
			"data_sha256": m.DataSHA256, "schema_sha256": m.SchemaSHA256, "config_sha256": m.ConfigSHA256,
			"config_schema_sha256": m.ConfigSchemaSHA256, "template_sha256": m.TemplateSHA256,
			"page_sha256": sha256Hex(page.HTML), "page_template_sha256": page.TemplateSHA256,
			"profile": jsonv.Stringify(p), "coverage": jsonv.Stringify(CoverageProfile(p)),
		}
		for k, v := range got {
			if v != str(r, k) {
				t.Errorf("%s %s:\n got %.300s\nwant %.300s", str(r, "name"), k, v, str(r, k))
			}
		}
	}
}
