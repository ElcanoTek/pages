// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

// testdata/golden.json.gz holds lib/preflight.js's reports for ~110 documents
// (recorded with the default CONTENT_ORIGIN and a page-data that reports every
// page as unmanaged). Reports must match key for key, except the text after
// "does not parse: " in a syntax error, which was V8's or Acorn's wording.

type goldenCase struct {
	HTML       string          `json:"html"`
	RenderMode string          `json:"renderMode"`
	Output     json.RawMessage `json:"output"`
}

var parserWording = regexp.MustCompile(`(does not parse: ).*?(\. Nothing in that block runs)`)

func normalise(t *testing.T, raw []byte) string {
	t.Helper()
	s := parserWording.ReplaceAllString(string(raw), "${1}…${2}")
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	_ = enc.Encode(v)
	return buf.String()
}

func TestGoldenAgainstJS(t *testing.T) {
	// Recorded from the JavaScript, which skipped the syntax check above 1 MiB.
	defer func(old int) { maxScriptBytes = old }(maxScriptBytes)
	maxScriptBytes = 1024 * 1024
	t.Setenv("CONTENT_ORIGIN", "")
	t.Setenv("CONTENT_HOST", "")
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
	for i, c := range cases {
		report := Analyze(c.HTML, Options{RenderMode: c.RenderMode})
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(report)
		// Key order is part of the wire: compare the raw encodings' key
		// sequences too, not just the data.
		got, want := normalise(t, buf.Bytes()), normalise(t, c.Output)
		if got != want {
			html := c.HTML
			if len(html) > 160 {
				html = html[:160] + "…"
			}
			t.Errorf("case %d (%s) %q:\n got: %s\nwant: %s", i, c.RenderMode, html, got, want)
		}
		if keyOrder(buf.Bytes()) != keyOrder(c.Output) {
			t.Errorf("case %d: key order differs:\n got %s\nwant %s", i, keyOrder(buf.Bytes()), keyOrder(c.Output))
		}
	}
}

var jsonKey = regexp.MustCompile(`"([a-z_]+)":`)

func keyOrder(raw []byte) string {
	var b bytes.Buffer
	for _, m := range jsonKey.FindAllSubmatch(parserWording.ReplaceAll(raw, []byte("")), -1) {
		b.Write(m[1])
		b.WriteByte(' ')
	}
	return b.String()
}
