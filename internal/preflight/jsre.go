// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// The rules were JavaScript regular expressions. RE2 differs from ECMAScript in
// three ways that change matches, and the patterns here are written around
// them: JS `\s` is Unicode whitespace (jsS spells it out; RE2's is ASCII), JS
// `/i` folds ASCII only (fold() spells [xX] per letter; RE2's (?i) also folds
// the Kelvin sign and the long s), and RE2 has no lookbehind (the one rule that
// used it, CALL_RE, is a hand-written scanner).

const jsS = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

const sp = `[` + jsS + `]`

func fold(word string) string {
	var b strings.Builder
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteString("[" + string(r) + string(r-32) + "]")
		case r >= 'A' && r <= 'Z':
			b.WriteString("[" + string(r+32) + string(r) + "]")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	return b.String()
}

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim is String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

var jsSpaceRun = regexp.MustCompile(sp + `+`)

// splitJSWhitespace is s.split(/\s+/).
func splitJSWhitespace(s string) []string { return jsSpaceRun.Split(s, -1) }

// jsSlice is String.prototype.slice(0, n): n UTF-16 code units. A surrogate
// pair the cut would split is dropped whole (JS keeps its lone high half).
func jsSlice(s string, n int) string {
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xffff {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

func isIdentStart(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_' || c == '$'
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// bareCalls is CALL_RE: /(?<![.\w$])([A-Za-z_$][A-Za-z0-9_$]*)\s*\(/g — a BARE
// `foo(` in call position. The lookbehind matters twice: it keeps member calls
// out (`this.togglePopover()`, `window.togglePopover()`, `a?.togglePopover()`
// all resolve correctly and must not be flagged) and it does not eat the
// delimiter, so adjacent calls like `a(b())` are both seen.
func bareCalls(s string) []string {
	var names []string
	for i := 0; i < len(s); {
		if !isIdentStart(s[i]) || i > 0 && (s[i-1] == '.' || isIdentPart(s[i-1])) {
			i++
			continue
		}
		j := i + 1
		for j < len(s) && isIdentPart(s[j]) {
			j++
		}
		k := j
		for k < len(s) {
			r, size := utf8.DecodeRuneInString(s[k:])
			if !isJSSpace(r) {
				break
			}
			k += size
		}
		if k < len(s) && s[k] == '(' {
			names = append(names, s[i:j])
			i = k + 1
			continue
		}
		i++
	}
	return names
}
