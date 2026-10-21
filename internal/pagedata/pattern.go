// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// ── pattern safety (ReDoS screen) ────────────────────────────────────────────
// Schema `pattern` keywords were compiled straight to RegExp and run against
// caller-supplied data on the shared event loop, where no statement_timeout
// applied. Two shapes are rejected at compile time:
//   • a quantifier nested inside another — (a+)+, (\d+)*, ([a-z]+){2,}
//   • a repeated group with two IDENTICAL alternatives — (a|a)+ — which
//     backtracks with no inner quantifier at all (14.5 seconds of blocked event
//     loop on a 29-character NON-matching input).
//
// The Go server matches with RE2, which cannot backtrack, so the hazard itself
// is gone; the screen stays because which schemas are accepted is part of the
// contract clients see, and a pattern refused before must stay refused.
//
// A bounded outer repetition around an unbounded inner one — (.*a){20},
// (a{1,10})* — is equally explosive for a backtracking engine and is NOT
// screened. Treating any {n,m} with max>1 as a repetition also files a BOUNDED
// inner quantifier as unbounded, which rejects the grouped-repetition idiom
// every real id format is built from — IPv4, IPv6, MAC, UUID, semver, cron,
// time ranges. The delimited idiom (-[a-z0-9]+)* is NOT that shape: every pass
// through the group must start with the literal "-", which the inner repetition
// can never consume, so matches partition linearly. The screen keeps that idiom
// (the "literal delimiter" exemption) and errs conservative past one level of
// nesting.

const maxPatternChars = 1000

type patAtom struct {
	typ      string // literal, class, group, assertion
	char     rune   // literal; -1 when the escape ended the pattern
	body     []rune // class
	group    *patGroup
	optional bool
}

type patGroup struct {
	body           []*patAtom
	innerUnbounded []*patAtom
	hasAlternation bool
	zeroWidth      bool
	branchStarts   []int
	branches       []string
}

var asciiWordRe = regexp.MustCompile(`^[A-Za-z0-9_]$`)

func units16(s string) []rune {
	u := utf16.Encode([]rune(s))
	out := make([]rune, len(u))
	for i, c := range u {
		out[i] = rune(c)
	}
	return out
}

func unitsString(u []rune) string {
	w := make([]uint16, len(u))
	for i, c := range u {
		w[i] = uint16(c) //nolint:gosec // UTF-16 code units are below 0x10000
	}
	return string(utf16.Decode(w))
}

// asciiPrefix renders the leading ASCII units of u (enough for a {n,m}).
func asciiPrefix(u []rune) string {
	var b strings.Builder
	for _, c := range u {
		if c >= 0x80 || b.Len() > 64 {
			break
		}
		b.WriteRune(c)
	}
	return b.String()
}

// codePointAt is String.prototype.codePointAt over UTF-16 units.
func codePointAt(u []rune, i int) rune {
	if i < 0 || i >= len(u) {
		return -1
	}
	if u[i] >= 0xD800 && u[i] <= 0xDBFF && i+1 < len(u) && u[i+1] >= 0xDC00 && u[i+1] <= 0xDFFF {
		return (u[i]-0xD800)<<10 + (u[i+1] - 0xDC00) + 0x10000
	}
	return u[i]
}

func runeAt(s []rune, i int) rune {
	if i >= 0 && i < len(s) {
		return s[i]
	}
	return -1
}

func isJSSpaceRune(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// classMatchesChar reports whether a class body can match char (a rough,
// conservative reading: complement escapes are assumed to match).
func classMatchesChar(body []rune, char rune) bool {
	negated := false
	i := 0
	if len(body) > 0 && body[0] == '^' {
		negated, i = true, 1
	}
	matched := false
	for i < len(body) {
		var lo rune
		if body[i] == '\\' {
			esc := runeAt(body, i+1)
			switch esc {
			case 'd':
				if char >= '0' && char <= '9' {
					matched = true
				}
				i += 2
				continue
			case 'w':
				if char >= 0 && asciiWordRe.MatchString(string(char)) {
					matched = true
				}
				i += 2
				continue
			case 's':
				if isJSSpaceRune(char) {
					matched = true
				}
				i += 2
				continue
			case 'S', 'D', 'W':
				return true
			}
			lo = esc
			i += 2
		} else {
			lo = codePointAt(body, i)
			i++
		}
		if runeAt(body, i) == '-' && i+1 < len(body) {
			var hi rune
			if body[i+1] == '\\' {
				hi = runeAt(body, i+2)
				i += 3
			} else {
				hi = codePointAt(body, i+1)
				i += 2
			}
			if lo >= 0 && hi >= 0 && char >= 0 && lo <= char && char <= hi {
				matched = true
			}
		} else if lo >= 0 && lo == char {
			matched = true
		}
	}
	if negated {
		return !matched
	}
	return matched
}

// branchesOverlap: only exact duplicate alternatives. A prefix relation was
// tried and reverted: (a|ab)+, (GET|G)+, (https|http)+ and (1|10|100)+ all
// parse uniquely. This is a source-text comparison, so it deters an accident
// and not an author: (a|[a])+ is still accepted (see docs/SECURITY.md).
func branchesOverlap(branches []string) bool {
	for a := 0; a < len(branches); a++ {
		for b := a + 1; b < len(branches); b++ {
			if branches[a] == branches[b] {
				return true
			}
		}
	}
	return false
}

// groupIsAmbiguous: a group is unsafe to quantify unboundedly when its body
// carries an inner unbounded quantifier that could also consume the group's
// own starting delimiter, or when it has two identical alternatives.
func groupIsAmbiguous(g *patGroup) bool {
	if g.hasAlternation && branchesOverlap(g.branches) {
		return true
	}
	if len(g.innerUnbounded) == 0 {
		return false
	}
	var first *patAtom
	if len(g.body) > 0 {
		first = g.body[0]
	}
	// The delimiter exemption holds only when EVERY pass through the group
	// must consume the same leading literal.
	if !g.hasAlternation && first != nil && first.typ == "literal" && !first.optional {
		for _, a := range g.innerUnbounded {
			switch a.typ {
			case "literal":
				if a.char == first.char {
					return true
				}
			case "class":
				if classMatchesChar(a.body, first.char) {
					return true
				}
			default:
				return true // nested-group atom: conservative
			}
		}
		return false
	}
	return true
}

var braceQuantRe = regexp.MustCompile(`^\{(\d+)(?:,(\d*))?\}`)

// IsSafePattern is the ReDoS screen applied to every schema "pattern".
func IsSafePattern(pattern string) bool {
	if jsonv.UTF16Len(pattern) > maxPatternChars {
		return false
	}
	// JavaScript indexes the pattern by UTF-16 code units, so the screen does
	// too (an astral character is two "literals").
	p := units16(pattern)
	var stack []*patGroup
	current := func() *patGroup {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	var prev *patAtom
	push := func(a *patAtom) {
		if c := current(); c != nil {
			c.body = append(c.body, a)
		}
	}
	i := 0
	for i < len(p) {
		ch := p[i]
		switch ch {
		case '\\':
			next := runeAt(p, i+1)
			switch next {
			case 'd', 'w', 's', 'b', 'S', 'D', 'W', 'p', 'P':
				prev = &patAtom{typ: "class", body: []rune{'\\', next}}
			default:
				prev = &patAtom{typ: "literal", char: next}
			}
			push(prev)
			i += 2
			continue
		case '[':
			j := i + 1
			var body []rune
			if runeAt(p, j) == '^' {
				body = append(body, '^')
				j++
			}
			if runeAt(p, j) == ']' {
				body = append(body, ']')
				j++
			}
			for j < len(p) && p[j] != ']' {
				if p[j] == '\\' {
					body = append(body, p[j])
					if j+1 < len(p) {
						body = append(body, p[j+1])
					}
					j += 2
					continue
				}
				body = append(body, p[j])
				j++
			}
			if j >= len(p) {
				return false
			}
			prev = &patAtom{typ: "class", body: body}
			push(prev)
			i = j + 1
			continue
		case '(':
			j := i + 1
			zeroWidth := false
			if runeAt(p, j) == '?' {
				j++
				switch {
				case runeAt(p, j) == ':':
					j++
				case runeAt(p, j) == '=' || runeAt(p, j) == '!':
					zeroWidth = true
					j++
				case runeAt(p, j) == '<' && (runeAt(p, j+1) == '=' || runeAt(p, j+1) == '!'):
					zeroWidth = true
					j += 2
				case runeAt(p, j) == '<':
					// Named capture: the name is syntax, not a literal prefix.
					end := -1
					for k := j + 1; k < len(p); k++ {
						if p[k] == '>' {
							end = k
							break
						}
					}
					if end < 0 || end == j+1 {
						return false
					}
					j = end + 1
				default:
					// Unknown group syntax cannot take part in the delimiter
					// proof; fail closed.
					return false
				}
			}
			stack = append(stack, &patGroup{zeroWidth: zeroWidth, branchStarts: []int{j}})
			prev = nil
			i = j
			continue
		case ')':
			g := current()
			if g == nil {
				return false
			}
			stack = stack[:len(stack)-1]
			for n, s := range g.branchStarts {
				end := i
				if n+1 < len(g.branchStarts) {
					end = g.branchStarts[n+1] - 1
				}
				if s > end {
					s = end
				}
				g.branches = append(g.branches, unitsString(p[s:end]))
			}
			if g.zeroWidth {
				prev = &patAtom{typ: "assertion"}
			} else {
				prev = &patAtom{typ: "group", group: g}
			}
			push(prev)
			// Parentheses do not erase repetition depth: ((\d+))* is screened
			// exactly like (\d+)*.
			if parent := current(); parent != nil && len(g.innerUnbounded) > 0 {
				parent.innerUnbounded = append(parent.innerUnbounded, prev)
			}
			i++
			continue
		case '+', '*', '?', '{':
			unbounded := ch == '+' || ch == '*'
			optional := ch == '*' || ch == '?'
			span := 1
			if ch == '{' {
				s := asciiPrefix(p[i:])
				m := braceQuantRe.FindStringSubmatchIndex(s)
				if m == nil {
					prev = &patAtom{typ: "literal", char: '{'}
					push(prev)
					i++
					continue
				}
				n, _ := strconv.ParseFloat(s[m[2]:m[3]], 64)
				optional = n == 0
				unbounded = m[4] >= 0 && m[4] == m[5] // "{n,}"
				span = m[1] - m[0]
			}
			if prev != nil && optional {
				prev.optional = true
			}
			if unbounded {
				if prev != nil && prev.typ == "group" && groupIsAmbiguous(prev.group) {
					return false
				}
				if c := current(); c != nil && prev != nil {
					c.innerUnbounded = append(c.innerUnbounded, prev)
				}
			}
			prev = nil
			i += span
			continue
		case '|':
			if c := current(); c != nil {
				c.hasAlternation = true
				c.branchStarts = append(c.branchStarts, i+1)
			}
			prev = nil
			i++
			continue
		case '^', '$':
			prev = &patAtom{typ: "assertion"}
			push(prev)
			i++
			continue
		case '.':
			prev = &patAtom{typ: "class", body: []rune{'\\', 'S'}}
			push(prev)
			i++
			continue
		}
		prev = &patAtom{typ: "literal", char: ch}
		push(prev)
		i++
	}
	return len(stack) == 0
}
