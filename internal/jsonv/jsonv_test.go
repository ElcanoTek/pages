// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jsonv

import (
	"math"
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

func TestCompareUTF16(t *testing.T) {
	s := []string{"～", "😀", "abc", "Abc", "10"}
	SortStrings(s)
	want := []string{"10", "Abc", "abc", "😀", "～"}
	for i := range want {
		if s[i] != want[i] {
			t.Fatalf("sort: %q", s)
		}
	}
	if UTF16Len("a😀") != 3 || SliceUTF16("a😀b", 0, 3) != "a😀" {
		t.Error("utf16 helpers")
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
