// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"strings"
	"unicode/utf8"
)

// Literal-anchored scanners for the script rules.
//
// WHY NOT REGEXPS. The JS rules were regular expressions, and the first port
// ran them as RE2 patterns over the joined inline scripts. Most of them open
// with `(?:^|[^.\w$])` (or, for declared globals, a delimiter class), which
// gives RE2 no literal prefix to skip ahead with, so every one ran the NFA
// across every byte. Once inline scripts of several MiB became normal that was
// ~2 s per preflight on the deploy path, against ~0.3 s for the actual JS
// parse. Each scanner below finds the one literal every match must contain
// with strings.Index (vectorised), then checks the surroundings by hand. The
// work around one occurrence is bounded by the whitespace runs next to it,
// each of which belongs to exactly one occurrence, so a scan is linear in the
// input however adversarial it is.
//
// Each scanner is EXACTLY its regexp — the patterns live on in scan_test.go as
// the oracle of a differential fuzz test. The semantics they reproduce: RE2
// reads the input as UTF-8 with an invalid byte decoding to U+FFFD (so it is
// neither whitespace nor a word character), `\w` and `\b` are ASCII, and the
// case folding in fold() is ASCII only. An ASCII byte is always a rune of its
// own, so a single-byte look at the neighbour of an ASCII keyword is exact.

// isJSSpace is the jsS class (JS `\s`).
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// isWordByte is RE2's `\w` (and the `\b` word set).
func isWordByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_'
}

// skipSpace returns the end of the maximal run of jsS runes starting at i.
func skipSpace(s string, i int) int {
	for i < len(s) {
		if c := s[i]; c < utf8.RuneSelf {
			if c != ' ' && (c < '\t' || c > '\r') {
				return i
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isJSSpace(r) {
			return i
		}
		i += size
	}
	return i
}

// skipSpaceBack returns the start of the maximal run of jsS runes ending at i.
// Valid UTF-8 sequences decode identically backwards and forwards, and only
// valid ones can be whitespace, so this agrees with RE2's forward reading.
func skipSpaceBack(s string, i int) int {
	for i > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:i])
		if !isJSSpace(r) {
			return i
		}
		i -= size
	}
	return i
}

// notMemberAt is `(?:^|[^.\w$])` matching just before a match body at i.
func notMemberAt(s string, i int) bool {
	if i == 0 {
		return true
	}
	c := s[i-1]
	return c != '.' && c != '$' && !isWordByte(c)
}

// wordEnd is `\b` after an identifier-like keyword ending at i.
func wordEnd(s string, i int) bool { return i == len(s) || !isWordByte(s[i]) }

// callAt is `sp*\(` at i.
func callAt(s string, i int) bool {
	j := skipSpace(s, i)
	return j < len(s) && s[j] == '('
}

// memberOf reports whether the text ending at i is `obj sp*\. sp*`, and if so
// where obj starts.
func memberOf(s string, i int, obj string) (int, bool) {
	j := skipSpaceBack(s, i)
	if j == 0 || s[j-1] != '.' {
		return 0, false
	}
	j = skipSpaceBack(s, j-1)
	if !strings.HasSuffix(s[:j], obj) {
		return 0, false
	}
	return j - len(obj), true
}

// eachIndex calls fn with every offset at which word occurs in s, stopping
// early when fn returns true; it reports whether fn did.
func eachIndex(s, word string, fn func(i int) bool) bool {
	for off := 0; ; {
		i := strings.Index(s[off:], word)
		if i < 0 {
			return false
		}
		i += off
		if fn(i) {
			return true
		}
		off = i + 1
	}
}

// bareUse is notMember + `(?:window sp*\. sp*)?` + word + after (the window
// prefix only when optWindow).
func bareUse(s, word string, optWindow bool, after func(s string, end int) bool) bool {
	return eachIndex(s, word, func(i int) bool {
		if !after(s, i+len(word)) {
			return false
		}
		if notMemberAt(s, i) {
			return true
		}
		if !optWindow {
			return false
		}
		w, ok := memberOf(s, i, "window")
		return ok && notMemberAt(s, w)
	})
}

func isCall(s string, end int) bool    { return callAt(s, end) }
func isWordEnd(s string, end int) bool { return wordEnd(s, end) }
func memberCall(s, obj, method string, needNotMember bool) bool {
	return eachIndex(s, method, func(i int) bool {
		if !callAt(s, i+len(method)) {
			return false
		}
		o, ok := memberOf(s, i, obj)
		return ok && (!needNotMember || notMemberAt(s, o))
	})
}

// ── the rules' patterns ─────────────────────────────────────────────────────

// usesPrint is notMember(?:window sp*\. sp*)?print sp*\(
func usesPrint(s string) bool { return bareUse(s, "print", true, isCall) }

// usesDialog is notMember(?:window sp*\. sp*)?(?:alert|confirm|prompt) sp*\(
func usesDialog(s string) bool {
	return bareUse(s, "alert", true, isCall) || bareUse(s, "confirm", true, isCall) || bareUse(s, "prompt", true, isCall)
}

// usesWindowOpen is notMember window sp*\. sp* open sp*\(
func usesWindowOpen(s string) bool { return memberCall(s, "window", "open", true) }

// usesStorage is notMember(?:window sp*\. sp*)?(localStorage|sessionStorage)\b
func usesStorage(s string) bool {
	return bareUse(s, "localStorage", true, isWordEnd) || bareUse(s, "sessionStorage", true, isWordEnd)
}

// usesCookie is document sp*\. sp* cookie
func usesCookie(s string) bool {
	return eachIndex(s, "cookie", func(i int) bool { _, ok := memberOf(s, i, "document"); return ok })
}

// usesIndexedDB is notMember indexedDB\b
func usesIndexedDB(s string) bool { return bareUse(s, "indexedDB", false, isWordEnd) }

// usesFetch is notMember fetch sp*\(
func usesFetch(s string) bool { return bareUse(s, "fetch", false, isCall) }

// newOf is new sp+ ctor\b
func newOf(s, ctor string) bool {
	return eachIndex(s, ctor, func(i int) bool {
		if !wordEnd(s, i+len(ctor)) {
			return false
		}
		j := skipSpaceBack(s, i)
		return j < i && strings.HasSuffix(s[:j], "new")
	})
}

// usesXHR is new sp+ XMLHttpRequest\b
func usesXHR(s string) bool { return newOf(s, "XMLHttpRequest") }

// usesSocket is new sp+ (?:WebSocket|EventSource)\b
func usesSocket(s string) bool { return newOf(s, "WebSocket") || newOf(s, "EventSource") }

// usesBeacon is notMember navigator sp*\. sp* sendBeacon sp*\(
func usesBeacon(s string) bool { return memberCall(s, "navigator", "sendBeacon", true) }

// eachFold is eachIndex for an ASCII-lowercase word matched with ASCII case
// folding (fold()).
func eachFold(s, word string, fn func(i int) bool) bool {
	first := word[0]
	for i := 0; i+len(word) <= len(s); i++ {
		if s[i]|0x20 != first {
			continue
		}
		match := true
		for k := 1; k < len(word); k++ {
			if s[i+k]|0x20 != word[k] {
				match = false
				break
			}
		}
		if match && fn(i) {
			return true
		}
	}
	return false
}

// usesDownload is
//
//	\. sp* download sp* =  |  < a [^>]+ sp download [jsS=>]
//
// with download and a ASCII-case-insensitive.
func usesDownload(s string) bool {
	// State for the markup branch, advanced monotonically through s: the last
	// '>' before the cursor, and the first `<a` after it.
	cursor, firstA := 0, -1
	return eachFold(s, "download", func(i int) bool {
		end := i + len("download")
		// Property branch.
		if j := skipSpaceBack(s, i); j > 0 && s[j-1] == '.' {
			if k := skipSpace(s, end); k < len(s) && s[k] == '=' {
				return true
			}
		}
		// Markup branch: the rune before must be whitespace and the one after
		// whitespace, '=' or '>'.
		if i == 0 || end >= len(s) {
			return false
		}
		r, size := utf8.DecodeLastRuneInString(s[:i])
		if !isJSSpace(r) {
			return false
		}
		if c := s[end]; c != '=' && c != '>' {
			if a, _ := utf8.DecodeRuneInString(s[end:]); !isJSSpace(a) {
				return false
			}
		}
		d := i - size // where the separating whitespace rune starts
		for cursor < d {
			k := strings.IndexAny(s[cursor:d], "<>")
			if k < 0 {
				cursor = d
				break
			}
			cursor += k
			if s[cursor] == '>' {
				firstA = -1
			} else if firstA < 0 && cursor+1 < len(s) && s[cursor+1]|0x20 == 'a' {
				firstA = cursor
			}
			cursor++
		}
		// `<a` at q, then [^>]+ (at least one rune) up to d, with no '>'.
		return firstA >= 0 && firstA+3 <= d
	})
}

// declaredGlobals is every capture of
//
//	(?:^|[jsS;}])(?:function sp* \*? sp* | (?:const|let|var) sp+)([A-Za-z_$][A-Za-z0-9_$]*)
//
// in FindAll order: leftmost, non-overlapping.
func declaredGlobals(s string) map[string]bool {
	defined := map[string]bool{}
	keywords := [...]string{"function", "const", "let", "var"}
	// next[k] caches the first occurrence of keywords[k] at or after the last
	// offset it was searched from: -1 when there is none, -2 before the first
	// search. It is searched again only once off has passed it.
	var next [len(keywords)]int
	for k := range keywords {
		next[k] = -2
	}
	find := func(k, off int) {
		if next[k] == -1 || next[k] >= off {
			return
		}
		if i := strings.Index(s[off:], keywords[k]); i >= 0 {
			next[k] = off + i
		} else {
			next[k] = -1
		}
	}
	pos := 0 // where the previous match ended
	for off := 0; ; {
		i, keyword := -1, ""
		for k := range keywords {
			find(k, off)
			if at := next[k]; at >= 0 && (i < 0 || at < i) {
				i, keyword = at, keywords[k]
			}
		}
		if i < 0 {
			return defined
		}
		off = i + 1
		// The match starts at ^ (i == 0) or at the delimiter rune before i.
		start := i
		if i > 0 {
			r, size := utf8.DecodeLastRuneInString(s[:i])
			if r != ';' && r != '}' && !isJSSpace(r) {
				continue
			}
			start = i - size
		}
		if start < pos {
			continue
		}
		j := i + len(keyword)
		if keyword == "function" {
			j = skipSpace(s, j)
			if j < len(s) && s[j] == '*' {
				j++
			}
			j = skipSpace(s, j)
		} else {
			k := skipSpace(s, j)
			if k == j {
				continue
			}
			j = k
		}
		if j >= len(s) || !isIdentStart(s[j]) {
			continue
		}
		e := j + 1
		for e < len(s) && isIdentPart(s[e]) {
			e++
		}
		defined[s[j:e]] = true
		pos = e
		off = e
	}
}
