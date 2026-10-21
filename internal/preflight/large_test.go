// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"strings"
	"testing"
	"time"
)

// Preflight runs on the deploy path, and inline scripts of several MiB are
// ordinary since the page limits were raised. These tests pin that a large
// document is analysed quickly and that no input built to defeat the rule
// scanners (every keyword or a near miss of it, over and over) is slow.

// repeatTo repeats unit until the result is at least n bytes.
func repeatTo(unit string, n int) string {
	return strings.Repeat(unit, (n+len(unit)-1)/len(unit))
}

// ordinaryScript is dashboard-shaped code: declarations, calls, literals,
// member access, a little DOM — and none of the APIs the rules flag.
func ordinaryScript(n int) string {
	return repeatTo(`function render_row(a, b) {
  const total = a.value + b.value * 2, label = 'Row ' + a.name;
  let cells = [total, { k: "v", n: 1.5e3 }].map((x) => x.toFixed ? x.toFixed(2) : String(x));
  var el = document.createElement("tr"); el.className = "row"; // a comment ( { [
  el.innerHTML = cells.map(c => `+"`<td>${c}</td>`"+`).join(""); return /[a-z]+\d*/gi.test(label) ? el : null;
}
`, n)
}

// adversarialScripts are each built to make some rule do as much work as it
// can: keywords one character short, keywords whose surroundings almost
// complete a match, and whitespace runs the scanners have to walk.
func adversarialScripts(n int) map[string]string {
	// Every keyword a rule looks for, one character short.
	var nearMiss strings.Builder
	for _, w := range []string{"fetch", "localStorage", "postMessage", "window.print", "document.cookie", "new XMLHttpRequest",
		"indexedDB", "navigator.sendBeacon", "function", "const", "download", "<a"} {
		nearMiss.WriteString(w[:len(w)-1] + " ")
	}
	return map[string]string{
		"near-miss keywords":        repeatTo(nearMiss.String(), n),
		"keywords without the rest": repeatTo("print fetch window.open window . print localStorage$ .download <a download navigator.sendBeacon new  document.cook ", n),
		"member calls":              repeatTo("x.print(); y.fetch(); z.alert(); $confirm(); _prompt(); a.window.open(); ", n),
		"whitespace runs":           repeatTo("window"+strings.Repeat("  　", 64)+"."+strings.Repeat("\t", 64)+"printx ", n),
		"open anchors":              repeatTo("<a x download", n),
		"declarations":              repeatTo("var functionfunction*;const}let ", n),
	}
}

func analyzeScript(src string) (Report, time.Duration) {
	doc := "<!doctype html><html><body><script>" + src + "</script></body></html>"
	start := time.Now()
	r := Analyze(doc, Options{})
	return r, time.Since(start)
}

// assertFast fails when d exceeds bound, unless the race detector (or -short
// on a loaded machine) makes wall time meaningless.
func assertFast(t *testing.T, what string, d, bound time.Duration) {
	t.Helper()
	t.Logf("%s: %v", what, d)
	if raceEnabled {
		return
	}
	if d > bound {
		t.Errorf("%s took %v, want under %v", what, d, bound)
	}
}

func TestAnalyzeLargeOrdinaryScriptIsFast(t *testing.T) {
	src := ordinaryScript(6 << 20)
	r, d := analyzeScript(src)
	if !r.OK || len(r.Warnings) != 0 {
		t.Fatalf("ordinary script should be clean: %+v %+v", r.Errors, r.Warnings)
	}
	// ~0.6 s on a developer machine; the regexp-based rules took ~4 s.
	assertFast(t, "6 MiB ordinary script", d, 3*time.Second)
}

func TestAnalyzeAdversarialScriptsAreFast(t *testing.T) {
	for name, src := range adversarialScripts(maxScriptBytes) {
		t.Run(name, func(t *testing.T) {
			_, d := analyzeScript(src)
			assertFast(t, name, d, 3*time.Second)
		})
	}
}

func BenchmarkAnalyzeLargeScript(b *testing.B) {
	doc := "<!doctype html><html><body><script>" + ordinaryScript(6<<20) + "</script></body></html>"
	b.SetBytes(int64(len(doc)))
	for b.Loop() {
		Analyze(doc, Options{})
	}
}

// A single huge token is valid JavaScript and must parse at any size the
// syntax check accepts. (The golden's >1 MiB `xxxx… }}}` case is NOT one: its
// trailing braces are unbalanced, V8 rejects it too, and the golden only
// records ok:true because the JS skipped the check above 1 MiB.)
func TestHugeTokensParse(t *testing.T) {
	n := 7 << 20
	body := strings.Repeat("x", n)
	for name, src := range map[string]string{
		"identifier":     body,
		"declared ident": "var " + body + " = 1;",
		"string":         `"` + body + `"`,
		"single string":  "'" + body + "'",
		"template":       "`" + body + "`",
		"template subst": "`" + body + "${1}" + body[:1024] + "`",
		"line comment":   "//" + body,
		"block comment":  "/*" + body + "*/ 1",
		"regexp":         "/" + body + "/g.test('x')",
		"number":         strings.Repeat("1", n),
	} {
		for _, module := range []bool{false, true} {
			if v := checkScriptSyntax(src, module); v != (syntaxVerdict{}) {
				t.Errorf("%s (module=%v): %+v", name, module, v)
			}
		}
		if r, _ := analyzeScript(src); !r.OK {
			t.Errorf("%s: Analyze reported %+v", name, r.Errors)
		}
	}
}

func TestHugeInvalidScriptReportsTheError(t *testing.T) {
	// The golden's case 106/107 shape with the 8 MiB bound in force.
	r, _ := analyzeScript(strings.Repeat("x", 1<<20+1) + " }}}")
	if len(r.Errors) != 1 || r.Errors[0].Code != "script_syntax_error" ||
		!strings.Contains(r.Errors[0].Message, "does not parse: Unexpected } in expression (1:1048579).") {
		t.Fatalf("got %+v", r.Errors)
	}
}
