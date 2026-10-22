// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"runtime"
	"strings"
	"testing"
)

// Preflight runs on the deploy path, and inline scripts of several MiB are
// ordinary since the page limits were raised. These tests pin that the cost of
// analysing a large document grows linearly with it, and that no input built
// to defeat the rule scanners (every keyword or a near miss of it, over and
// over) grows it faster. Cost is COUNTED — allocations and bytes allocated,
// which are deterministic — never timed: a wall-clock bound measures the
// machine as much as the code, and failed under -race on a loaded host.
// BenchmarkAnalyzeLargeScript tracks the absolute speed.

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

func analyzeScript(src string) Report {
	return Analyze("<!doctype html><html><body><script>"+src+"</script></body></html>", Options{})
}

// allocCost is what f allocates: the number of allocations and the bytes.
// Both are deterministic for a given input, unlike elapsed time.
func allocCost(f func()) (mallocs, bytes uint64) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs, after.TotalAlloc - before.TotalAlloc
}

// assertNoBlowup runs half (an input at half size) and whole (the same input
// at full size) and fails if doubling the input more than doubled the cost,
// with headroom: a linear check comes out at or under 2x, a quadratic one
// near 4x. The small additive slack keeps a handful of fixed allocations from
// dominating when the counts are tiny.
func assertNoBlowup(t *testing.T, what string, half, whole func()) {
	t.Helper()
	m1, b1 := allocCost(half)
	m2, b2 := allocCost(whole)
	t.Logf("%s: allocations %d → %d, bytes %d → %d", what, m1, m2, b1, b2)
	if float64(m2) > 2.5*float64(m1)+64 {
		t.Errorf("%s: doubling the input multiplied the allocations by %.2f, want at most 2.5 (linear)", what, float64(m2)/float64(m1))
	}
	if float64(b2) > 2.5*float64(b1)+64<<10 {
		t.Errorf("%s: doubling the input multiplied the bytes allocated by %.2f, want at most 2.5 (linear)", what, float64(b2)/float64(b1))
	}
}

func TestAnalyzeLargeOrdinaryScriptIsLinear(t *testing.T) {
	if r := analyzeScript(ordinaryScript(6 << 20)); !r.OK || len(r.Warnings) != 0 {
		t.Fatalf("ordinary script should be clean: %+v %+v", r.Errors, r.Warnings)
	}
	assertNoBlowup(t, "ordinary script 3 → 6 MiB",
		func() { analyzeScript(ordinaryScript(3 << 20)) },
		func() { analyzeScript(ordinaryScript(6 << 20)) })
}

// The adversarial inputs target the rule scanners, so both sizes sit above
// maxScriptBytes: the syntax check skips them alike and the comparison is
// scanner against scanner, not a parsed run against a skipped one.
func TestAnalyzeAdversarialScriptsAreLinear(t *testing.T) {
	half := adversarialScripts(maxScriptBytes)
	for name, src := range adversarialScripts(2 * maxScriptBytes) {
		t.Run(name, func(t *testing.T) {
			assertNoBlowup(t, name, func() { analyzeScript(half[name]) }, func() { analyzeScript(src) })
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
		if r := analyzeScript(src); !r.OK {
			t.Errorf("%s: Analyze reported %+v", name, r.Errors)
		}
	}
}

func TestHugeInvalidScriptReportsTheError(t *testing.T) {
	// The golden's case 106/107 shape with the 8 MiB bound in force.
	r := analyzeScript(strings.Repeat("x", 1<<20+1) + " }}}")
	if len(r.Errors) != 1 || r.Errors[0].Code != "script_syntax_error" ||
		!strings.Contains(r.Errors[0].Message, "does not parse: Unexpected } in expression (1:1048579).") {
		t.Fatalf("got %+v", r.Errors)
	}
}
