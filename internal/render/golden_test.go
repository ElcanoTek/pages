// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package render

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// testdata/golden.json.gz holds the outputs lib/render.js and lib/bento.js
// produced for 900+ inputs (recorded with the default CONTENT_ORIGIN and
// FLAG_ASSETS_BASE). The served bytes must be identical, so every case is
// compared byte for byte.

type goldenCase struct {
	Fn     string          `json:"fn"`
	Input  goldenInput     `json:"input"`
	Output json.RawMessage `json:"output"`
}

type goldenInput struct {
	HTML          string          `json:"html"`
	RenderMode    string          `json:"render_mode"`
	OverrideCSS   string          `json:"override_css"`
	Nav           json.RawMessage `json:"nav"`
	CSS           string          `json:"css"`
	SaveURL       string          `json:"saveUrl"`
	Token         string          `json:"token"`
	VersionID     string          `json:"versionId"`
	ContentOrigin string          `json:"contentOrigin"`
}

func loadGolden(t *testing.T) []goldenCase {
	t.Helper()
	f, err := os.Open("testdata/golden.json.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenCase
	if err := json.NewDecoder(zr).Decode(&cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestGoldenAgainstJS(t *testing.T) {
	t.Setenv("CONTENT_ORIGIN", "")
	t.Setenv("CONTENT_HOST", "")
	t.Setenv("FLAG_ASSETS_BASE", "")
	cases := loadGolden(t)
	dollarFixed := 0
	for i, c := range cases {
		in := c.Input
		var nav any
		if len(in.Nav) > 0 && string(in.Nav) != "null" {
			nav = in.Nav
		}
		var got any
		switch c.Fn {
		case "renderVersion":
			got = RenderVersion(Renderable{HTML: in.HTML, RenderMode: in.RenderMode, OverrideCSS: in.OverrideCSS, Nav: nav})
		case "readsNavBlock":
			got = ReadsNavBlock(in.HTML)
		case "isBentoDeck":
			got = IsBentoDeck(in.HTML)
		case "adaptDeckToHost":
			got = AdaptDeckToHost(in.HTML)
		case "stripDeckHostAdaptation":
			got = StripDeckHostAdaptation(in.HTML)
		case "stripInjectedNav":
			got = StripInjectedNav(in.HTML)
		case "escapeStyleText":
			got = EscapeStyleText(in.CSS)
		case "stripCollab":
			out, stripped := StripCollab(in.HTML)
			got = map[string]any{"html": out, "stripped": stripped}
		case "readDoc":
			// The golden is JSON.stringify(readDoc(html)): compare the parsed value
			// re-serialised the same way (Infinity and -0 cannot round-trip through
			// encoding/json).
			got = "null"
			if _, ok := ReadDoc(in.HTML); ok {
				m := DocBlockRE.FindStringSubmatch(in.HTML)
				v, _ := jsonv.ParseLossless([]byte(m[2]))
				got = jsonv.Stringify(v)
			}
		case "editSessionScript":
			got = EditSessionScript(in.SaveURL, in.Token, in.VersionID)
		case "editSession":
			got = EditSession(in.HTML, EditSessionOptions{SaveURL: in.SaveURL, Token: in.Token, VersionID: in.VersionID, ContentOrigin: in.ContentOrigin})
		default:
			t.Fatalf("case %d: unknown fn %q", i, c.Fn)
		}
		if !matchesGolden(t, got, c.Output) {
			if c.Fn == "renderVersion" && dollarExpansionCase(in) {
				// The JS passed the head as a String#replace pattern string, so `$&`
				// and friends in a theme or a title were expanded. Fixed here;
				// TestThemedInsertionIsLiteral pins the fix.
				dollarFixed++
				continue
			}
			var want any
			_ = json.Unmarshal(c.Output, &want)
			gs, _ := got.(string)
			ws, _ := want.(string)
			at := 0
			for at < len(gs) && at < len(ws) && gs[at] == ws[at] {
				at++
			}
			from := max(0, at-80)
			t.Errorf("case %d %s(%q, mode=%q) differs at byte %d:\n got: %.300q\nwant: %.300q", i, c.Fn, trunc(in.HTML), in.RenderMode, at, gs[min(from, len(gs)):], ws[min(from, len(ws)):])
		}
	}
	if dollarFixed == 0 {
		t.Error("expected the golden set to exercise the String#replace $-expansion fix")
	}
}

func dollarExpansionCase(in goldenInput) bool {
	return !headOpenRE.MatchString(in.HTML) && htmlOpenRE.MatchString(in.HTML) &&
		(strings.Contains(in.OverrideCSS, "$") || strings.Contains(string(in.Nav), "$"))
}

func trunc(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

func matchesGolden(t *testing.T, got any, want json.RawMessage) bool {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	_ = json.Unmarshal(gotJSON, &a)
	_ = json.Unmarshal(want, &b)
	return jsonEqual(a, b)
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
