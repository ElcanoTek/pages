// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package jsstr holds the JavaScript string semantics the Node server relied
// on without saying so: String.prototype.trim's whitespace set, lengths and
// slices counted in UTF-16 code units, the code-unit order Array#sort and <
// use, and Number(string). Go strings are UTF-8, so every helper here states
// what happens where the two models disagree (lone surrogates, invalid bytes).
package jsstr

import (
	"errors"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// IsSpace reports whether r is JavaScript whitespace or a line terminator:
// what String.prototype.trim strips and what /\s/ matches. It differs from
// unicode.IsSpace in excluding U+0085 and including U+FEFF.
func IsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Trim is String.prototype.trim.
func Trim(s string) string { return strings.TrimFunc(s, IsSpace) }

// UTF16Len is String.prototype.length: s counted in UTF-16 code units (an
// invalid UTF-8 byte counts as the one U+FFFD it decodes to).
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Slice is s.slice(start, end) in UTF-16 code units, then encoded as UTF-8
// as Node does when it hashes, stores or sends a string. A surrogate pair the
// cut splits leaves a lone surrogate, which encodes as U+FFFD; invalid UTF-8
// in s also becomes U+FFFD, as it would have when the request body was
// decoded into a JavaScript string. Out-of-range bounds are clamped (there is
// no negative-from-the-end indexing).
func Slice(s string, start, end int) string {
	if start <= 0 && len(s) <= end && utf8.ValidString(s) {
		return s // a UTF-8 string never has more UTF-16 code units than bytes
	}
	units := utf16.Encode([]rune(s))
	if start < 0 {
		start = 0
	}
	if end > len(units) {
		end = len(units)
	}
	if start >= end {
		return ""
	}
	return string(utf16.Decode(units[start:end]))
}

// Prefix is s.slice(0, n): Slice(s, 0, n).
func Prefix(s string, n int) string { return Slice(s, 0, n) }

// Compare orders strings by UTF-16 code units, the order JavaScript's default
// sort and its < operator use. It differs from Go's byte order for characters
// above U+FFFF versus U+E000–U+FFFF.
func Compare(a, b string) int {
	for a != "" && b != "" {
		ra, na := utf8.DecodeRuneInString(a)
		rb, nb := utf8.DecodeRuneInString(b)
		if ra != rb {
			ua, ub := firstUnit(ra), firstUnit(rb)
			if ua != ub {
				if ua < ub {
					return -1
				}
				return 1
			}
			// Same high surrogate: the low surrogates order as the runes do.
			if ra < rb {
				return -1
			}
			return 1
		}
		a, b = a[na:], b[nb:]
	}
	switch {
	case a == "" && b == "":
		return 0
	case a == "":
		return -1
	default:
		return 1
	}
}

func firstUnit(r rune) rune {
	if r >= 0x10000 {
		r1, _ := utf16.EncodeRune(r)
		return r1
	}
	return r
}

// Less is Compare(a, b) < 0.
func Less(a, b string) bool { return Compare(a, b) < 0 }

// Sort sorts as Array.prototype.sort() does with no comparator (for strings).
func Sort(s []string) {
	sort.SliceStable(s, func(i, j int) bool { return Less(s[i], s[j]) })
}

// Number is JavaScript's Number(string): a whitespace-trimmed decimal
// literal, a 0x/0o/0b integer, ±Infinity, "" (or only whitespace) → 0, and
// NaN for anything else.
func Number(s string) float64 {
	s = Trim(s)
	if s == "" {
		return 0
	}
	if len(s) > 2 && s[0] == '0' {
		base := 0
		switch s[1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			for i := 2; i < len(s); i++ {
				if d, ok := digitVal(s[i]); !ok || d >= base {
					return math.NaN()
				}
			}
			// Through big.Int so a literal past 2^53 rounds once, correctly.
			n, _ := new(big.Int).SetString(s[2:], base)
			f, _ := new(big.Float).SetInt(n).Float64()
			return f
		}
	}
	body := s
	if body[0] == '+' || body[0] == '-' {
		body = body[1:]
	}
	if body == "Infinity" {
		if s[0] == '-' {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	if !decimalLiteral.MatchString(body) {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return f
}

var decimalLiteral = regexp.MustCompile(`^(?:[0-9]+\.?[0-9]*|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)

func digitVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}
