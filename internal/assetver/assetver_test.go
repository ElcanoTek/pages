// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package assetver

import (
	"regexp"
	"testing"
	"testing/fstest"

	pages "github.com/ElcanoTek/pages"
)

func TestVersionIsAStableContentHash(t *testing.T) {
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(Version()) {
		t.Fatalf("version %q is not 12 hex digits", Version())
	}
	if v := Of(pages.Public()); Version() != v {
		t.Fatalf("version %q is not the hash of the embedded tree (%q)", Version(), v)
	}
	a := fstest.MapFS{"x.css": {Data: []byte("a")}}
	b := fstest.MapFS{"x.css": {Data: []byte("b")}}
	c := fstest.MapFS{"y.css": {Data: []byte("a")}}
	if Of(a) == Of(b) || Of(a) == Of(c) {
		t.Fatal("a change of bytes or of path must change the version")
	}
	if Of(a) != Of(fstest.MapFS{"x.css": {Data: []byte("a")}}) {
		t.Fatal("the same tree must hash the same")
	}
}

func TestBases(t *testing.T) {
	v := Version()
	for got, want := range map[string]string{
		Shell():       "/shell-assets/_v/" + v,
		ShellFlag():   "/shell-assets/_v/" + v + "/flag",
		Content():     "/assets/_v/" + v,
		ContentFlag(): "/assets/_v/" + v + "/flag",
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestStrip(t *testing.T) {
	v := Version()
	cases := []struct {
		in, rest           string
		versioned, current bool
	}{
		{"/_v/" + v + "/flag/fonts/fonts.css", "/flag/fonts/fonts.css", true, true},
		{"/_v/000000000000/flag/fonts/fonts.css", "/flag/fonts/fonts.css", true, false},
		{"/_v/" + v, "/", true, true},
		{"/flag/fonts/fonts.css", "/flag/fonts/fonts.css", false, false},
		{"", "", false, false},
		{"/_vx/" + v + "/a.css", "/_vx/" + v + "/a.css", false, false},
	}
	for _, c := range cases {
		rest, versioned, current := Strip(c.in)
		if rest != c.rest || versioned != c.versioned || current != c.current {
			t.Errorf("Strip(%q) = %q, %v, %v; want %q, %v, %v", c.in, rest, versioned, current, c.rest, c.versioned, c.current)
		}
	}
}
