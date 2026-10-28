// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

import (
	"errors"
	"os"
	"testing"

	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/mcpproto"
)

// TestParseInputMatchesZod replays testdata/js-parse-corpus.json: thousands
// of generated arguments and how zod's safeParse answered each (the parsed
// value, or the issues as the SDK rendered them).
func TestParseInputMatchesZod(t *testing.T) {
	raw, err := os.ReadFile("testdata/js-parse-corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonv.ParseLossless(raw)
	if err != nil {
		t.Fatal(err)
	}
	cat := nodeCatalog(t)
	failures := 0
	for i, e := range v.([]any) {
		c := e.(*jsonv.Object)
		name := c.Value("tool").(string)
		if name == "get_version" {
			if ao, ok := c.Value("args").(*jsonv.Object); ok && !ao.Has("version_id") {
				// Go makes version_id optional on get_version (defaulting to the
				// published version, matching find_in_version and preflight_page).
				continue
			}
		}
		got, perr := cat.byName[name].input.Parse(c.Value("args"))
		args := jsonv.Stringify(c.Value("args"))
		if len(args) > 300 {
			args = args[:300] + "…"
		}
		if want, isErr := c.Value("error").(string); isErr {
			var ve *mcpproto.ValidationError
			switch {
			case perr == nil:
				t.Errorf("case %d %s %s: parsed, zod said %q", i, name, args, want)
				failures++
			case !errors.As(perr, &ve):
				t.Errorf("case %d %s: %v", i, name, perr)
				failures++
			case ve.Error() != want:
				t.Errorf("case %d %s %s:\n got: %q\nwant: %q", i, name, args, ve.Error(), want)
				failures++
			}
		} else if perr != nil {
			t.Errorf("case %d %s %s: %v, zod parsed it", i, name, args, perr)
			failures++
		} else if g, w := jsonv.Stringify(got), jsonv.Stringify(c.Value("data")); g != w {
			t.Errorf("case %d %s: parsed\n %s\nwant\n %s", i, name, g, w)
			failures++
		}
		if failures > 40 {
			t.Fatal("too many failures")
		}
	}
}
