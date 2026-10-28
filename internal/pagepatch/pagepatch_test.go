// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagepatch

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// Ported from the page-patch block of test/unit.test.js.

func code(err error) string {
	if e, ok := apierror.As(err); ok {
		return e.Code
	}
	return ""
}

func TestAnchoredEditReplacesExactlyItsMatch(t *testing.T) {
	res, err := ApplyEdits("<style>.ctl-group{display:flex;}</style><button onclick='go()'>x</button>",
		[]Edit{{Find: ".ctl-group{display:flex;", Replace: ".ctl-group{position:relative;display:flex;"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.HTML, ".ctl-group{position:relative;display:flex;}") || res.Applied[0].Count != 1 || res.Applied[0].BytesDelta != len("position:relative;") {
		t.Errorf("%+v", res)
	}
}

func TestMissingAnchorFailsLoudly(t *testing.T) {
	_, err := ApplyEdits("<p>hello</p>", []Edit{{Find: "goodbye", Replace: "hi"}})
	e, _ := apierror.As(err)
	if e == nil || e.Code != "patch_anchor_mismatch" || e.Status != 409 || e.Details.(MismatchDetails).ActualCount != 0 || !strings.Contains(e.Message, "find_in_version") {
		t.Errorf("%v", err)
	}
}

func TestAmbiguousAnchorIsRejected(t *testing.T) {
	_, err := ApplyEdits("<b>x</b><b>x</b>", []Edit{{Find: "<b>x</b>", Replace: "<i>x</i>"}})
	e, _ := apierror.As(err)
	if e == nil || e.Code != "patch_anchor_mismatch" || e.Details.(MismatchDetails).ActualCount != 2 || !strings.Contains(e.Message, "count:2") {
		t.Errorf("%v", err)
	}
}

func TestExplicitCountReplacesEveryOccurrence(t *testing.T) {
	res, err := ApplyEdits("togglePopover() togglePopover()", []Edit{{Find: "togglePopover", Replace: "drTogglePopover", Count: 2}})
	if err != nil || res.HTML != "drTogglePopover() drTogglePopover()" || res.Applied[0].Count != 2 {
		t.Errorf("%+v %v", res, err)
	}
}

func TestEditsApplyInOrder(t *testing.T) {
	res, err := ApplyEdits("<p>one</p>", []Edit{{Find: "one", Replace: "two"}, {Find: "two", Replace: "three"}})
	if err != nil || res.HTML != "<p>three</p>" {
		t.Errorf("%v %v", res, err)
	}
	_, err = ApplyEdits("<p>one</p>", []Edit{{Find: "one", Replace: "two"}, {Find: "one", Replace: "three"}})
	if e, _ := apierror.As(err); e == nil || e.Details.(MismatchDetails).EditIndex != 1 {
		t.Errorf("destroyed anchor: %v", err)
	}
}

func TestEditBoundsAreEnforced(t *testing.T) {
	many := make([]Edit, MaxEdits+1)
	for i := range many {
		many[i] = Edit{Find: "a", Replace: "b"}
	}
	if _, err := ApplyEdits("a", many); code(err) != "patch_edits_invalid" {
		t.Error(err)
	}
	if _, err := ApplyEdits("a", nil); code(err) != "patch_edits_invalid" {
		t.Error(err)
	}
	parse := func(s string) error {
		v, _ := jsonv.ParseString(s)
		_, err := ParseEdits(v)
		return err
	}
	if code(parse(`[{"find":"a","replace":"b","count":0}]`)) != "patch_edits_invalid" || code(parse(`[{"find":"a","replace":5}]`)) != "patch_edits_invalid" {
		t.Error("ParseEdits bounds")
	}
}

func TestAnchorsAreLiteral(t *testing.T) {
	res, err := ApplyEdits("keep <b>.*</b> keep", []Edit{{Find: ".*", Replace: "OK"}})
	if err != nil || res.HTML != "keep <b>OK</b> keep" {
		t.Errorf("%v %v", res, err)
	}
}

func TestSearchReturnsBoundedExcerpts(t *testing.T) {
	src := "line one\n<div class=\"dr-popover\" id=\"drPopover\">\n" + strings.Repeat("x", 5000) + "\n"
	res, err := FindMatches(src, "dr-popover", FindOptions{})
	if err != nil || res.TotalMatches != 1 || res.Matches[0].Line != 2 || jsstr.UTF16Len(res.Matches[0].Excerpt) > 260 || strings.Contains(res.Matches[0].Excerpt, strings.Repeat("x", 300)) {
		t.Errorf("%+v %v", res, err)
	}
	filler := strings.Repeat("<p>padding padding padding</p>\n", 12000)
	big := filler + `<div class="dr-popover"></div>` + filler
	res, _ = FindMatches(big, "dr-popover", FindOptions{})
	if res.TotalMatches != 1 || len(jsonv.Stringify(res)) >= 600 {
		t.Errorf("search output %d bytes", len(jsonv.Stringify(res)))
	}
	res, _ = FindMatches(strings.Repeat("hit ", 200), "hit", FindOptions{MaxMatches: 5})
	if res.TotalMatches != 200 || len(res.Matches) != 5 || res.MatchesOmitted != 195 {
		t.Errorf("%+v", res)
	}
	res, _ = FindMatches("<DIV>Hello</DIV>", "hello", FindOptions{IgnoreCase: true})
	if res.TotalMatches != 1 || !regexp.MustCompile("Hello").MatchString(res.Matches[0].Excerpt) {
		t.Errorf("%+v", res)
	}
}

// TestGoldenPatch compares against the Node release's page-patch module
// (testdata/patch.jsonl, recorded from that release). The golden was
// recorded under the Node release's bounds, so they are pinned here.
func TestGoldenPatch(t *testing.T) {
	withLimits(t, nodeLimits)
	f, err := os.Open(filepath.Join("testdata", "patch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	var editSets []any
	for sc.Scan() {
		v, err := jsonv.ParseString(sc.Text())
		if err != nil {
			t.Fatal(err)
		}
		r := v.(*jsonv.Object)
		if es, ok := r.Get("edits"); ok && editSets == nil && !r.Has("kind") {
			editSets = es.([]any)
			continue
		}
		src, _ := r.Value("src").(string)
		var got any
		var gotErr error
		switch r.Value("kind") {
		case "edit":
			edits, perr := ParseEdits(editSets[int(r.Value("edits").(float64))])
			if perr != nil {
				gotErr = perr
				break
			}
			res, err := ApplyEdits(src, edits)
			if err != nil {
				gotErr = err
				break
			}
			got = res
		case "find":
			opts := r.Value("opts").(*jsonv.Object)
			o := FindOptions{IgnoreCase: opts.Value("ignoreCase") == true}
			if m, ok := opts.Value("maxMatches").(float64); ok {
				o.MaxMatches = int(m)
				if m == 0 {
					o.MaxMatches = -1 // Math.max(1, 0): one match
				}
			}
			res, err := FindMatches(src, r.Value("query").(string), o)
			if err != nil {
				gotErr = err
				break
			}
			got = res
		}
		what := jsonv.Stringify(r.Value("kind")) + " " + jsonv.Stringify(r.Value("query")) + jsonv.Stringify(r.Value("edits")) + " on " + src[:min(len(src), 40)]
		if want := r.Value("error"); want != nil {
			w := want.(*jsonv.Object)
			e, ok := apierror.As(gotErr)
			if !ok || e.Message != w.Value("message") || e.Code != w.Value("code") || float64(e.Status) != w.Value("status") || jsonv.Stringify(e.Details) != jsonv.Stringify(w.Value("details")) {
				t.Errorf("%s: got %v, want %s", what, gotErr, jsonv.Stringify(w))
			}
			continue
		}
		if gotErr != nil {
			t.Errorf("%s: %v", what, gotErr)
			continue
		}
		if g, w := jsonv.Stringify(got), jsonv.Stringify(r.Value("result")); g != w {
			t.Errorf("%s:\n got %.500s\nwant %.500s", what, g, w)
		}
	}
}

// nodeLimits are lib/page-patch.js's bounds, which the golden was recorded
// under.
var nodeLimits = Limits{Edits: 25, FindChars: 8192, ReplaceChars: 128 * 1024, ExpectedCount: 100}

func withLimits(t *testing.T, l Limits) {
	t.Helper()
	saved := limits
	limits = l
	t.Cleanup(func() { limits = saved })
}

func TestRaisedPatchBounds(t *testing.T) {
	if MaxEdits != 100 || MaxFindChars != 64*1024 || MaxReplaceChars != 1024*1024 || MaxExpectedCount != 1000 || MaxMatches != 20 || MaxContextChars != 240 {
		t.Fatal("bounds changed")
	}
	edits := make([]Edit, MaxEdits)
	for i := range edits {
		edits[i] = Edit{Find: strings.Repeat("a", MaxFindChars), Replace: strings.Repeat("b", MaxReplaceChars), Count: MaxExpectedCount}
	}
	if err := ValidateEdits(edits); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEdits(append(edits, Edit{Find: "a"})); code(err) != "patch_edits_invalid" {
		t.Error(err)
	}
	for _, e := range []Edit{{Find: strings.Repeat("a", MaxFindChars+1)}, {Find: "a", Replace: strings.Repeat("b", MaxReplaceChars+1)}, {Find: "a", Count: MaxExpectedCount + 1}} {
		if err := ValidateEdits([]Edit{e}); code(err) != "patch_edits_invalid" {
			t.Error(err)
		}
	}
}

// A patch may not grow the document past what could have been deployed whole;
// the projection is refused before the replacement is built.
func TestPatchResultIsBounded(t *testing.T) {
	t.Setenv("MAX_HTML_BYTES", "64kb")
	t.Setenv("PAGES_UPLOAD_MAX_BYTES", "128kb")
	if MaxResultBytes() != 128*1024 {
		t.Fatal(MaxResultBytes())
	}
	src := strings.Repeat("x,", 1000)
	_, err := ApplyEdits(src, []Edit{{Find: "x", Replace: strings.Repeat("y", 200), Count: 1000}})
	e, _ := apierror.As(err)
	if e == nil || e.Code != "patch_result_too_large" || e.Status != 400 ||
		e.Message != "edit 0 would make the document 201000 bytes; the limit is 131072 bytes" {
		t.Fatalf("%v", err)
	}
	if d := e.Details.(TooLargeDetails); d.EditIndex != 0 || d.ResultBytes != 201000 || d.MaxBytes != 131072 {
		t.Errorf("%+v", d)
	}
	if _, err := ApplyEdits(src, []Edit{{Find: "x", Replace: strings.Repeat("y", 100), Count: 1000}}); err != nil {
		t.Error(err)
	}
	// A document already over the bound may still shrink.
	big := strings.Repeat("z", 200*1024)
	if _, err := ApplyEdits(big+"<p>", []Edit{{Find: "<p>", Replace: ""}}); err != nil {
		t.Error(err)
	}
}

func TestSearchWithContextLinesPreservesExactNewlinesAndIndentation(t *testing.T) {
	src := strings.Join([]string{
		"<div>",
		"  <header>",
		"    <h1 class=\"hero-title\">Q3 Executive Brief</h1>",
		"    <p class=\"hero-sub\">Prepared for Leadership</p>",
		"  </header>",
		"</div>",
	}, "\n")
	res, err := FindMatches(src, "Q3 Executive Brief", FindOptions{ContextLines: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.TotalMatches != 1 || res.Matches[0].Line != 3 {
		t.Fatalf("unexpected matches: %+v", res)
	}
	want := "  <header>\n    <h1 class=\"hero-title\">Q3 Executive Brief</h1>\n    <p class=\"hero-sub\">Prepared for Leadership</p>"
	if res.Matches[0].Excerpt != want {
		t.Fatalf("excerpt = %q, want %q", res.Matches[0].Excerpt, want)
	}
	// The multi-line excerpt can be copied directly as a patch_page anchor.
	patched, err := ApplyEdits(src, []Edit{{
		Find:    res.Matches[0].Excerpt,
		Replace: strings.Replace(res.Matches[0].Excerpt, "Q3 Executive Brief", "Q4 Executive Brief", 1),
	}})
	if err != nil || !strings.Contains(patched.HTML, "Q4 Executive Brief") {
		t.Fatalf("patch from multiline excerpt failed: %v", err)
	}

	// Even when a single line exceeds context_chars, the excerpt remains an
	// exact literal substring (no synthetic ellipsis) so patch_page succeeds.
	minified := strings.Repeat("a", 5000) + "<span id=\"target\">old</span>" + strings.Repeat("b", 5000)
	mres, err := FindMatches(minified, "id=\"target\">old", FindOptions{ContextLines: 2, ContextChars: 80})
	if err != nil || mres.TotalMatches != 1 {
		t.Fatalf("minified FindMatches failed: %+v %v", mres, err)
	}
	if !strings.Contains(minified, mres.Matches[0].Excerpt) || strings.Contains(mres.Matches[0].Excerpt, "…") {
		t.Fatalf("truncated multiline excerpt must be literal substring without ellipsis: %q", mres.Matches[0].Excerpt)
	}
}
