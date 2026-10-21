// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"maps"
	"math/rand/v2"
	"regexp"
	"strings"
	"testing"
)

// The scanners in scan.go replace these RE2 ports of lib/preflight.js's
// regexps; the patterns stay here as the oracle the scanners must agree with
// on every input.

const notMember = `(?:^|[^.\w$])`

var oracle = []struct {
	name string
	re   *regexp.Regexp
	scan func(string) bool
}{
	{"print", regexp.MustCompile(notMember + `(?:window` + sp + `*\.` + sp + `*)?print` + sp + `*\(`), usesPrint},
	{"dialog", regexp.MustCompile(notMember + `(?:window` + sp + `*\.` + sp + `*)?(?:alert|confirm|prompt)` + sp + `*\(`), usesDialog},
	{"download", regexp.MustCompile(`\.` + sp + `*` + fold("download") + sp + `*=|<` + fold("a") + `[^>]+` + sp + fold("download") + `[` + jsS + `=>]`), usesDownload},
	{"window.open", regexp.MustCompile(notMember + `window` + sp + `*\.` + sp + `*open` + sp + `*\(`), usesWindowOpen},
	{"storage", regexp.MustCompile(notMember + `(?:window` + sp + `*\.` + sp + `*)?(localStorage|sessionStorage)\b`), usesStorage},
	{"cookie", regexp.MustCompile(`document` + sp + `*\.` + sp + `*cookie`), usesCookie},
	{"indexedDB", regexp.MustCompile(notMember + `indexedDB\b`), usesIndexedDB},
	{"fetch", regexp.MustCompile(notMember + `fetch` + sp + `*\(`), usesFetch},
	{"xhr", regexp.MustCompile(`new` + sp + `+XMLHttpRequest\b`), usesXHR},
	{"socket", regexp.MustCompile(`new` + sp + `+(?:WebSocket|EventSource)\b`), usesSocket},
	{"beacon", regexp.MustCompile(notMember + `navigator` + sp + `*\.` + sp + `*sendBeacon` + sp + `*\(`), usesBeacon},
}

var definedGlobalsRE = regexp.MustCompile(`(?:^|[` + jsS + `;}])(?:function` + sp + `*\*?` + sp + `*|(?:const|let|var)` + sp + `+)([A-Za-z_$][A-Za-z0-9_$]*)`)

func checkAgainstOracle(t *testing.T, s string) {
	t.Helper()
	for _, o := range oracle {
		if got, want := o.scan(s), o.re.MatchString(s); got != want {
			t.Fatalf("%s(%q) = %v, regexp says %v", o.name, s, got, want)
		}
	}
	want := map[string]bool{}
	for _, m := range definedGlobalsRE.FindAllStringSubmatch(s, -1) {
		want[m[1]] = true
	}
	if got := declaredGlobals(s); !maps.Equal(got, want) {
		t.Fatalf("declaredGlobals(%q) = %v, regexp says %v", s, got, want)
	}
}

// pieces are the building blocks of the generated inputs: every keyword and
// its near misses, every delimiter the patterns look at, both cases of the
// folded words, the Unicode whitespace the jsS class adds, and invalid or
// truncated UTF-8 next to all of them.
var pieces = []string{
	"window", "windo", "print", "alert", "confirm", "prompt", "open", "localStorage", "sessionStorage",
	"document", "cookie", "indexedDB", "fetch", "new", "XMLHttpRequest", "WebSocket", "EventSource",
	"navigator", "sendBeacon", "download", "DOWNLOAD", "DownLoad", "<a", "<A", "<", "a", ">",
	"function", "functio", "const", "let", "var", "*", ".", "(", ")", "=", ";", "}", "{", "$", "_", "x", "Q", "1",
	" ", "\u2009", "\t", "\n", "\v", "\u00a0", "\u1680", "\u2003", "\u200a", "\u200b", "\u2028", "\u3000", "\ufeff", "\u0085",
	"\xc2", "\xa0", "\xe3\x80", "\x80", "\xff", "\u00e9", "\u212a", "\u017f",
}

func randomInput(r *rand.Rand) string {
	var b strings.Builder
	for n := 1 + r.IntN(24); n > 0; n-- {
		b.WriteString(pieces[r.IntN(len(pieces))])
	}
	return b.String()
}

func TestScannersMatchRegexps(t *testing.T) {
	fixed := []string{
		"", "print(", ".print(", "window.print(", "window . \u3000print (", "xwindow.print(", "$print(", "\u00e9 print(",
		"<a download>", "<a  download>", "<a x download=", "<A\ndownload\u00a0", "<a > download ", "<a<a download=",
		"a.download=1", "a. DownLoad \t=", "var var x", "function*gen", "functionfoo", "letter x", ";let\u2028y",
		"}const z", "var a;var b", "\xc2\xa0var q", "\xe3\x80var q", "document.cookie", "document . cookie",
		"renew XMLHttpRequest", "new XMLHttpRequest_", "new XMLHttpRequest$", "new\u00a0WebSocket(", "navigator.sendBeacon(",
		"x.navigator.sendBeacon(", "localStorage$", "localStorageX", "indexedDB.open", "fetch\n(", "afetch(",
		"x }}}", "\ufeffconst k",
	}
	for _, s := range fixed {
		checkAgainstOracle(t, s)
	}
	r := rand.New(rand.NewPCG(1, 2))
	n := 50000
	if testing.Short() {
		n = 5000
	}
	for range n {
		checkAgainstOracle(t, randomInput(r))
	}
}

func FuzzScannersMatchRegexps(f *testing.F) {
	r := rand.New(rand.NewPCG(3, 4))
	for range 64 {
		f.Add(randomInput(r))
	}
	f.Fuzz(func(t *testing.T, s string) { checkAgainstOracle(t, s) })
}
