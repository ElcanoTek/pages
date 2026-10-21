// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jsstr

import (
	"encoding/json"
	"strconv"
	"testing"
)

func TestSortAndLength(t *testing.T) {
	s := []string{"～", "😀", "abc", "Abc", "10"}
	Sort(s)
	want := []string{"10", "Abc", "abc", "😀", "～"}
	for i := range want {
		if s[i] != want[i] {
			t.Fatalf("sort: %q", s)
		}
	}
	if UTF16Len("a😀é") != 4 {
		t.Fatal(UTF16Len("a😀é"))
	}
}

func TestSlice(t *testing.T) {
	for _, tc := range []struct {
		in         string
		start, end int
		want       string
	}{
		{"abc", 0, 2, "ab"},
		{"abc", 0, 5, "abc"},
		{"abc", 1, 5, "bc"},
		{"abc", 2, 1, ""},
		{"a😀b", 0, 3, "a😀"},
		{"é😀x", 0, 3, "é😀"},
		{"é😀x", 0, 2, "é�"}, // a pair cut in half leaves a lone surrogate → U+FFFD
		{"a\xffb", 0, 5, "a�b"},
	} {
		if got := Slice(tc.in, tc.start, tc.end); got != tc.want {
			t.Errorf("Slice(%q, %d, %d) = %q, want %q", tc.in, tc.start, tc.end, got, tc.want)
		}
	}
}

func TestTrim(t *testing.T) {
	if got := Trim(string(rune(0xfeff)) + "\u00a0 x \u2028\u3000"); got != "x" {
		t.Errorf("Trim = %q", got)
	}
	if got := Trim("\u0085x"); got != "\u0085x" {
		t.Errorf("U+0085 is not JavaScript whitespace: %q", got)
	}
}

func TestNumberMatchesNode(t *testing.T) {
	// [input, String(Number(input))] from Node 22.
	recorded := `[["0x10","16"],[" 1 ","1"],["","0"],["1e3","1000"],["abc","NaN"],["-0x1","NaN"],["Infinity","Infinity"],["-Infinity","-Infinity"],["+.5","0.5"],["5.","5"],["1_0","NaN"],["0b101","5"],["0o17","15"],["0x","NaN"],[" 12 ","12"],["1e400","Infinity"],["--1","NaN"],["+-1","NaN"],["0.5e-1","0.05"],[".","NaN"],["e5","NaN"],["0X1f","31"],["00012","12"],["-0","0"],["1.5","1.5"]]`
	var rows [][2]string
	if err := json.Unmarshal([]byte(recorded), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		got := Number(r[0])
		var s string
		switch {
		case got != got:
			s = "NaN"
		case got > 1e308:
			s = "Infinity"
		case got < -1e308:
			s = "-Infinity"
		case got == 0:
			s = "0"
		default:
			s = strconv.FormatFloat(got, 'f', -1, 64)
		}
		if s != r[1] {
			t.Errorf("Number(%q) = %s, want %s", r[0], s, r[1])
		}
	}
}
