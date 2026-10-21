// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package jsjson

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"
)

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

func TestRoundTripMatchesNode(t *testing.T) {
	for _, tc := range loadCorpus(t).Texts {
		in, err := base64.StdEncoding.DecodeString(tc.In)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Normalize(in)
		if tc.Error {
			if err == nil {
				t.Errorf("%q: parsed as %s, JSON.parse refuses it", in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if string(got) != tc.Out {
			t.Errorf("%q:\n got %s\nwant %s", in, got, tc.Out)
		}
	}
}

func TestFormatNumberMatchesNode(t *testing.T) {
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

func TestObjectOrder(t *testing.T) {
	o := NewObject()
	o.Set("b", 1.0)
	o.Set("2", 2.0)
	o.Set("a", 3.0)
	o.Set("1", 4.0)
	o.Set("b", 5.0)
	if got := string(MustMarshal(o)); got != `{"1":4,"2":2,"b":5,"a":3}` {
		t.Fatalf("got %s", got)
	}
}

func TestNumberMatchesNode(t *testing.T) {
	// [input, String(Number(input))] from Node 22.
	recorded := `[["0x10","16"],[" 1 ","1"],["","0"],["1e3","1000"],["abc","NaN"],["-0x1","NaN"],["Infinity","Infinity"],["-Infinity","-Infinity"],["+.5","0.5"],["5.","5"],["1_0","NaN"],["0b101","5"],["0o17","15"],["0x","NaN"],[" 12 ","12"],["1e400","Infinity"],["--1","NaN"],["+-1","NaN"],["0.5e-1","0.05"],[".","NaN"],["e5","NaN"],["0X1f","31"],["00012","12"],["-0","0"],["1.5","1.5"]]`
	var rows [][2]string
	if err := json.Unmarshal([]byte(recorded), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if got := FormatNumber(Number(r[0])); got != r[1] {
			t.Errorf("Number(%q) = %s, want %s", r[0], got, r[1])
		}
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
