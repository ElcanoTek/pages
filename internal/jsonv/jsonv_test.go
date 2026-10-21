// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jsonv

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"testing"
)

func TestFormatNumberMatchesJavaScript(t *testing.T) {
	// Expected strings are what node prints for JSON.stringify of each value.
	cases := []struct {
		in   float64
		want string
	}{
		{1e21, "1e+21"}, {1e-7, "1e-7"}, {123456789012345680000, "123456789012345680000"},
		{math.Copysign(0, -1), "0"}, {0.30000000000000004, "0.30000000000000004"}, {5e-324, "5e-324"},
		{1.7976931348623157e308, "1.7976931348623157e+308"}, {100, "100"}, {1.5e300, "1.5e+300"},
		{0.000001, "0.000001"}, {1.5e-7, "1.5e-7"}, {-12345, "-12345"}, {20044.103, "20044.103"},
		{9007199254740993, "9007199254740992"}, {1.493135176553913e69, "1.493135176553913e+69"},
		{123e-20, "1.23e-18"}, {0.5, "0.5"}, {-0.0000025, "-0.0000025"},
	}
	for _, c := range cases {
		if got := FormatNumber(c.in); got != c.want {
			t.Errorf("FormatNumber(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := Stringify([]any{math.Inf(1), math.NaN()}); got != "[null,null]" {
		t.Errorf("non-finite: %s", got)
	}
}

func TestParseKeepsJavaScriptKeyOrder(t *testing.T) {
	v, err := ParseString(`{"b":1,"2":true,"a":[1,2,{"x":null}],"1":"one","b":3,"01":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := Stringify(v); got != `{"1":"one","2":true,"b":3,"a":[1,2,{"x":null}],"01":0}` {
		t.Errorf("order: %s", got)
	}
}

func TestStringifyEscapesLikeJavaScript(t *testing.T) {
	s := "<&>\u2028\"\\\n\x01é😀"
	if got := Quote(s); got != "\"<&>\u2028\\\"\\\\\\n\\u0001é😀\"" {
		t.Errorf("quote: %s", got)
	}
	v, err := ParseString(`"\ud83d\ude00 \ud800 \u00e9"`)
	if err != nil || v != "😀 \uFFFD é" {
		t.Errorf("surrogates: %q %v", v, err)
	}
}

func TestParseRejectsWhatJSONParseRejects(t *testing.T) {
	for _, bad := range []string{"", "{", "[1,]", "{\"a\":1,}", "01", "1.", ".5", "+1", "'a'", "\"\t\"", "nul", "1 2", "\u00a0{}", "{a:1}", "\"\\x\""} {
		if _, err := ParseString(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	v, err := ParseString(" \t\r\n1e400 ")
	if err != nil || !math.IsInf(v.(float64), 1) {
		t.Errorf("1e400: %v %v", v, err)
	}
}

// testdata/corpus.json was recorded with Node 22:
// JSON.stringify(JSON.parse(text)) for each text, the texts JSON.parse refuses,
// and String(n) for computed doubles.
type corpus struct {
	Texts []struct {
		In    string `json:"in"`
		Out   string `json:"out"`
		Error bool   `json:"error"`
	} `json:"texts"`
	Numbers []struct {
		Hex string `json:"hex"`
		Out string `json:"out"`
	} `json:"numbers"`
}

func loadCorpus(t *testing.T) corpus {
	t.Helper()
	raw, err := os.ReadFile("testdata/corpus.json")
	if err != nil {
		t.Fatal(err)
	}
	var c corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLosslessRoundTripMatchesNode(t *testing.T) {
	for _, tc := range loadCorpus(t).Texts {
		in, err := base64.StdEncoding.DecodeString(tc.In)
		if err != nil {
			t.Fatal(err)
		}
		v, err := ParseLossless(in)
		if tc.Error {
			if err == nil {
				t.Errorf("%q: parsed as %s, JSON.parse refuses it", in, Stringify(v))
			} else if !errors.Is(err, ErrSyntax) {
				t.Errorf("%q: %v is not ErrSyntax", in, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := Stringify(v); got != tc.Out {
			t.Errorf("%q:\n got %s\nwant %s", in, got, tc.Out)
		}
		// json.RawMessage is re-read the same way.
		if got := Stringify(json.RawMessage(in)); got != tc.Out {
			t.Errorf("RawMessage %q:\n got %s\nwant %s", in, got, tc.Out)
		}
	}
}

func TestFormatNumberMatchesNodeCorpus(t *testing.T) {
	for _, tc := range loadCorpus(t).Numbers {
		b, err := hex.DecodeString(tc.Hex)
		if err != nil {
			t.Fatal(err)
		}
		f := math.Float64frombits(binary.LittleEndian.Uint64(b))
		if got := FormatNumber(f); got != tc.Out {
			t.Errorf("%v: got %s want %s", f, got, tc.Out)
		}
	}
}

func TestParseReplacesLoneSurrogates(t *testing.T) {
	v, err := ParseString(`"a\ud800b"`)
	if err != nil || v != "a\uFFFDb" {
		t.Fatalf("Parse: %q %v", v, err)
	}
	v, err = ParseLossless([]byte(`"a\ud800b"`))
	if err != nil || Stringify(v) != `"a\ud800b"` {
		t.Fatalf("ParseLossless: %q %v", v, err)
	}
}

func TestToString(t *testing.T) {
	cases := map[string]any{"null": nil, "1.5": 1.5, "a": "a", "1,,x": []any{1.0, nil, "x"}, "[object Object]": NewObject(), "true": true}
	for want, v := range cases {
		if got := ToString(v); got != want {
			t.Errorf("ToString(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestIsSafeInteger(t *testing.T) {
	for v, want := range map[any]bool{1.0: true, 1.5: false, 9007199254740991.0: true, 9007199254740992.0: false, "1": false} {
		if IsSafeInteger(v) != want {
			t.Errorf("IsSafeInteger(%v) != %v", v, want)
		}
	}
}

func TestEqualIgnoresKeyOrder(t *testing.T) {
	a, _ := ParseString(`{"a":1,"b":[1,{"c":2}]}`)
	b, _ := ParseString(`{"b":[1,{"c":2}],"a":1.0}`)
	c, _ := ParseString(`{"b":[1,{"c":3}],"a":1}`)
	if !Equal(a, b) || Equal(a, c) {
		t.Error("Equal")
	}
}
