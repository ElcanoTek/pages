// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package render

import (
	"regexp"
	"strings"
)

// The JS module this package ports matched markup with JavaScript regular
// expressions, and its output has to be byte-identical. Two differences between
// RE2 and ECMAScript change what a pattern matches, so every pattern here is
// written to avoid them:
//
//   - JS `\s` is Unicode whitespace (plus \v and U+FEFF); RE2 `\s` is ASCII only.
//     jsS spells the JS class out.
//   - JS `/i` (non-unicode) folds ASCII only for ASCII patterns; RE2 `(?i)` also
//     folds K to the Kelvin sign and s to the long s, so `<ſcript` would match.
//     fold() spells each ASCII letter as a two-case class instead.
//
// RE2 has no lookahead either; each pattern that used one is rewritten to an
// equivalent that consumes the looked-at byte only where the match span is
// never used.

// jsS is the ECMAScript WhiteSpace + LineTerminator set, as a class body.
const jsS = `\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

// sp is the JS `\s` as a complete class.
const sp = `[` + jsS + `]`

// fold returns word with every ASCII letter written as [xX], so the pattern is
// ASCII-case-insensitive exactly as a JS /i pattern is.
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

// isJSSpace reports whether r is in the set String.prototype.trim removes.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// jsTrim is String.prototype.trim.
func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// replaceFirst replaces the first match of re in s with what fn returns for it,
// as String.prototype.replace does with a non-global regex and a function.
func replaceFirst(re *regexp.Regexp, s string, fn func(match string) string) string {
	loc := re.FindStringIndex(s)
	if loc == nil {
		return s
	}
	return s[:loc[0]] + fn(s[loc[0]:loc[1]]) + s[loc[1]:]
}
