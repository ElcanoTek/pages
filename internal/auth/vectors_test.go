// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package auth_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/ElcanoTek/pages/internal/auth/central"
	"github.com/ElcanoTek/pages/internal/auth/wire"
)

// testdata/js-vectors.json was recorded from the JS implementation (Node 22,
// lib/central-auth.js, lib/auth.js, encodeurl, URLSearchParams and
// node:querystring), so these compare the port with what Node produced.
type vectors struct {
	EncodeURIComponent [][2]string         `json:"encodeURIComponent"`
	DecodeURIComponent [][]any             `json:"decodeURIComponent"`
	FormEncode         [][2]string         `json:"formEncode"`
	EncodeURL          [][2]string         `json:"encodeUrl"`
	Query              [][]json.RawMessage `json:"query"`
	SafeLocalPath      [][2]string         `json:"safeLocalPath"`
	NormalizeEmail     [][]any             `json:"normalizeEmail"`
	ParseCookies       [][]json.RawMessage `json:"parseCookies"`
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/js-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestJSVectors(t *testing.T) {
	v := loadVectors(t)
	for _, c := range v.EncodeURIComponent {
		if got := wire.EncodeURIComponent(c[0]); got != c[1] {
			t.Errorf("EncodeURIComponent(%q) = %q, JS %q", c[0], got, c[1])
		}
	}
	for _, c := range v.DecodeURIComponent {
		in, want, ok := c[0].(string), c[1].(string), c[2].(bool)
		got, err := wire.DecodeURIComponent(in)
		if (err == nil) != ok || (ok && got != want) {
			t.Errorf("DecodeURIComponent(%q) = %q, %v; JS %q ok=%v", in, got, err, want, ok)
		}
	}
	for _, c := range v.FormEncode {
		if got := wire.FormEncode([2]string{c[0], c[0]}); got != c[1] {
			t.Errorf("FormEncode(%q) = %q, JS %q", c[0], got, c[1])
		}
	}
	for _, c := range v.EncodeURL {
		if got := wire.EncodeURL(c[0]); got != c[1] {
			t.Errorf("EncodeURL(%q) = %q, JS %q", c[0], got, c[1])
		}
	}
	for _, c := range v.Query {
		var in string
		var want map[string][]string
		_ = json.Unmarshal(c[0], &in)
		_ = json.Unmarshal(c[1], &want)
		if got := wire.ParseQuery(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseQuery(%q) = %v, JS %v", in, got, want)
		}
	}
	for _, c := range v.SafeLocalPath {
		if got := central.SafeLocalPath(c[0]); got != c[1] {
			t.Errorf("SafeLocalPath(%q) = %q, JS %q", c[0], got, c[1])
		}
	}
	for _, c := range v.NormalizeEmail {
		in, want, ok := c[0].(string), c[1].(string), c[2].(bool)
		got, err := central.NormalizeEmail(in)
		if (err == nil) != ok || (ok && got != want) {
			t.Errorf("NormalizeEmail(%q) = %q, %v; JS %q ok=%v", in, got, err, want, ok)
		}
	}
	for _, c := range v.ParseCookies {
		var in string
		var want map[string]string
		_ = json.Unmarshal(c[0], &in)
		_ = json.Unmarshal(c[1], &want)
		if got := wire.ParseCookies(in); !reflect.DeepEqual(got, want) {
			t.Errorf("ParseCookies(%q) = %v, JS %v", in, got, want)
		}
	}
}
