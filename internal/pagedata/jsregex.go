// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// JavaScript regular expressions in Go.
//
// The JS server compiled a schema `pattern` as `new RegExp(pattern, "u")` and
// checked `format: "regex"` with `new RegExp(str)`. Go's regexp is RE2: a
// different syntax (no lookaround, no backreferences, no lone `{`), different
// classes (`\s` and `.` are ASCII-minded) and linear-time matching. So this
// parses the JS grammar — strictly in unicode mode, with the Annex B
// leniencies otherwise — into a small AST, and then:
//
//   - emits an equivalent RE2 expression whenever one exists (`\s`/`\S` become
//     the ECMAScript WhiteSpace+LineTerminator set, `.` excludes only \n \r
//     U+2028 U+2029, `\d`/`\w` stay ASCII) — every pattern seen in practice;
//   - otherwise (lookaround, backreferences, repeat counts above RE2's 1000)
//     matches with a backtracking interpreter of the same AST, bounded by a
//     step and depth budget. A match that exhausts the budget reports no
//     match rather than stalling the request (JavaScript would grind on).

var errUnsupported = errors.New("regular expression construct not supported")

type jsRegexError struct{ msg string }

func (e *jsRegexError) Error() string { return "Invalid regular expression: " + e.msg }

const maxRune = unicode.MaxRune

type runeRange struct{ lo, hi rune }

var (
	jsSpaceRanges = []runeRange{{0x09, 0x0D}, {0x20, 0x20}, {0xA0, 0xA0}, {0x1680, 0x1680}, {0x2000, 0x200A},
		{0x2028, 0x2029}, {0x202F, 0x202F}, {0x205F, 0x205F}, {0x3000, 0x3000}, {0xFEFF, 0xFEFF}}
	jsDigitRanges = []runeRange{{'0', '9'}}
	jsWordRanges  = []runeRange{{'0', '9'}, {'A', 'Z'}, {'_', '_'}, {'a', 'z'}}
)

func complementRanges(rs []runeRange) []runeRange {
	rs = normalizeRanges(rs)
	var out []runeRange
	next := rune(0)
	for _, r := range rs {
		if r.lo > next {
			out = append(out, runeRange{next, r.lo - 1})
		}
		next = r.hi + 1
	}
	if next <= maxRune {
		out = append(out, runeRange{next, maxRune})
	}
	return out
}

// normalizeRanges sorts, merges and drops the surrogate block (a Go string
// never holds a surrogate, so those ranges can never match).
func normalizeRanges(rs []runeRange) []runeRange {
	var split []runeRange
	for _, r := range rs {
		if r.hi < 0xD800 || r.lo > 0xDFFF {
			split = append(split, r)
			continue
		}
		if r.lo < 0xD800 {
			split = append(split, runeRange{r.lo, 0xD7FF})
		}
		if r.hi > 0xDFFF {
			split = append(split, runeRange{0xE000, r.hi})
		}
	}
	sort.Slice(split, func(i, j int) bool { return split[i].lo < split[j].lo })
	var out []runeRange
	for _, r := range split {
		if n := len(out); n > 0 && r.lo <= out[n-1].hi+1 {
			if r.hi > out[n-1].hi {
				out[n-1].hi = r.hi
			}
			continue
		}
		out = append(out, r)
	}
	return out
}

func renderRanges(rs []runeRange) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, `\x{%X}`, r.lo)
		if r.hi != r.lo {
			fmt.Fprintf(&b, `-\x{%X}`, r.hi)
		}
	}
	return b.String()
}

const neverMatch = `[^\x{0}-\x{10FFFF}]`

// ── AST ──────────────────────────────────────────────────────────────────────

type reNode interface{}

type (
	reAlt  struct{ alts []reNode }
	reSeq  struct{ items []reNode }
	reChar struct{ r rune }
	reSet  struct {
		ranges []runeRange // normalized
		props  []reProp
		negate bool
	}
	reProp struct {
		name  string
		neg   bool
		table *unicode.RangeTable
	}
	reDot    struct{}
	reNever  struct{}
	reAssert struct{ kind byte } // ^ $ b B
	reGroup  struct {
		capture int // 0 for a non-capturing group
		node    reNode
	}
	reLook struct {
		behind, negate bool
		node           reNode
	}
	reBackref struct {
		index int
		name  string
	}
	reRepeat struct {
		node     reNode
		min, max int // max -1: unbounded
		lazy     bool
		caps     []int // capture groups inside, reset each iteration
	}
)

func (s *reSet) has(r rune) bool {
	in := false
	for _, rr := range s.ranges {
		if r >= rr.lo && r <= rr.hi {
			in = true
			break
		}
	}
	if !in {
		for _, p := range s.props {
			if unicode.Is(p.table, r) != p.neg {
				in = true
				break
			}
		}
	}
	return in != s.negate
}

// ── parser ───────────────────────────────────────────────────────────────────

type jsRegexParser struct {
	src     []rune
	i       int
	unicode bool
	groups  int // total capturing groups (prescan)
	named   bool
	capture int
	names   map[string]int
}

func (p *jsRegexParser) fail(msg string) error { return &jsRegexError{msg: msg} }

func (p *jsRegexParser) eof() bool { return p.i >= len(p.src) }

func (p *jsRegexParser) peek(k int) rune {
	if p.i+k < len(p.src) {
		return p.src[p.i+k]
	}
	return -1
}

// prescan counts capturing groups and notes named groups, which decide how a
// decimal escape and \k parse.
func (p *jsRegexParser) prescan() {
	inClass := false
	for i := 0; i < len(p.src); i++ {
		switch c := p.src[i]; {
		case c == '\\':
			i++
		case inClass:
			if c == ']' {
				inClass = false
			}
		case c == '[':
			inClass = true
		case c == '(':
			if i+1 < len(p.src) && p.src[i+1] == '?' {
				if i+2 < len(p.src) && p.src[i+2] == '<' && i+3 < len(p.src) && p.src[i+3] != '=' && p.src[i+3] != '!' {
					p.groups++
					p.named = true
				}
				continue
			}
			p.groups++
		}
	}
}

type jsRegex struct {
	root  reNode
	ncap  int
	names map[string]int
}

func parseJSRegex(src string, unicodeMode bool) (*jsRegex, error) {
	p := &jsRegexParser{src: []rune(src), unicode: unicodeMode, names: map[string]int{}}
	p.prescan()
	root, err := p.disjunction(0)
	if err != nil {
		return nil, err
	}
	if !p.eof() {
		return nil, p.fail("Unmatched ')'")
	}
	re := &jsRegex{root: root, ncap: p.capture, names: p.names}
	if err := re.resolveBackrefs(root); err != nil {
		return nil, err
	}
	return re, nil
}

func (re *jsRegex) resolveBackrefs(n reNode) error {
	switch x := n.(type) {
	case *reAlt:
		for _, a := range x.alts {
			if err := re.resolveBackrefs(a); err != nil {
				return err
			}
		}
	case *reSeq:
		for _, a := range x.items {
			if err := re.resolveBackrefs(a); err != nil {
				return err
			}
		}
	case *reGroup:
		return re.resolveBackrefs(x.node)
	case *reLook:
		return re.resolveBackrefs(x.node)
	case *reRepeat:
		return re.resolveBackrefs(x.node)
	case *reBackref:
		if x.name != "" {
			idx, ok := re.names[x.name]
			if !ok {
				return &jsRegexError{msg: "Invalid named capture referenced"}
			}
			x.index = idx
		}
	}
	return nil
}

// translateJSRegex checks a pattern's JavaScript syntax: a *jsRegexError when
// JavaScript would reject it, nil otherwise.
func translateJSRegex(src string, unicodeMode bool) (string, error) {
	re, err := parseJSRegex(src, unicodeMode)
	if err != nil {
		return "", err
	}
	out, ok := re2Of(re.root)
	if !ok {
		return "", errUnsupported
	}
	return out, nil
}

// jsMatcher is a compiled JavaScript regular expression; MatchString is
// RegExp.prototype.test.
type jsMatcher interface{ MatchString(string) bool }

// compileJSRegex compiles a JavaScript pattern with the given flags (any of
// "u", "i", "m"): to RE2 when it can be expressed, to the backtracking matcher
// otherwise (which supports neither "i" nor "m", only needed by formats that
// RE2 expresses).
func compileJSRegex(src, flags string) (jsMatcher, error) {
	re, err := parseJSRegex(src, strings.Contains(flags, "u"))
	if err != nil {
		return nil, err
	}
	if out, ok := re2Of(re.root); ok {
		prefix := ""
		if strings.Contains(flags, "i") {
			prefix += "(?i)"
		}
		if strings.Contains(flags, "m") {
			prefix += "(?m)"
		}
		if compiled, err := regexp.Compile(prefix + out); err == nil {
			return compiled, nil
		}
	}
	if strings.ContainsAny(flags, "im") {
		return nil, errUnsupported
	}
	return &btRegexp{re: re}, nil
}

func (p *jsRegexParser) disjunction(depth int) (reNode, error) {
	if depth > 500 {
		return nil, p.fail("pattern nested too deeply")
	}
	var alts []reNode
	for {
		seq, err := p.alternative(depth)
		if err != nil {
			return nil, err
		}
		alts = append(alts, seq)
		if p.eof() || p.src[p.i] != '|' {
			break
		}
		p.i++
	}
	if len(alts) == 1 {
		return alts[0], nil
	}
	return &reAlt{alts: alts}, nil
}

func (p *jsRegexParser) alternative(depth int) (reNode, error) {
	seq := &reSeq{}
	for !p.eof() {
		c := p.src[p.i]
		if c == '|' || c == ')' {
			break
		}
		if !p.unicode && c >= 0x10000 {
			// Without the u flag JavaScript sees an astral character as two
			// code units and a quantifier binds to the low one. Neither half
			// can match a Go string.
			seq.items = append(seq.items, &reNever{})
		}
		capBefore := p.capture
		node, kind, err := p.atom(depth)
		if err != nil {
			return nil, err
		}
		q, ok, err := p.quantifier()
		if err != nil {
			return nil, err
		}
		if ok {
			switch kind {
			case kindAssertion:
				return nil, p.fail("Nothing to repeat")
			case kindLookahead:
				if p.unicode {
					return nil, p.fail("Invalid quantifier")
				}
			}
			q.node = node
			for c := capBefore + 1; c <= p.capture; c++ {
				q.caps = append(q.caps, c)
			}
			node = q
		}
		seq.items = append(seq.items, node)
	}
	return seq, nil
}

// atomKind says whether a term may take a quantifier.
type atomKind int

const (
	kindAtom atomKind = iota
	kindAssertion
	kindLookahead
)

// quantifier parses *, +, ?, {n}, {n,}, {n,m} with an optional lazy ?. In
// non-unicode mode a "{" that does not form a quantifier is not one.
func (p *jsRegexParser) quantifier() (*reRepeat, bool, error) {
	if p.eof() {
		return nil, false, nil
	}
	q := &reRepeat{}
	switch c := p.src[p.i]; c {
	case '*':
		p.i++
		q.min, q.max = 0, -1
	case '+':
		p.i++
		q.min, q.max = 1, -1
	case '?':
		p.i++
		q.min, q.max = 0, 1
	case '{':
		lo, hi, hasComma, hasHi, n, ok := p.braces(p.i)
		if !ok {
			if p.unicode {
				return nil, false, p.fail("Incomplete quantifier")
			}
			return nil, false, nil
		}
		if hasHi && hi < lo {
			return nil, false, p.fail("numbers out of order in {} quantifier")
		}
		p.i += n
		q.min = lo
		switch {
		case !hasComma:
			q.max = lo
		case !hasHi:
			q.max = -1
		default:
			q.max = hi
		}
	default:
		return nil, false, nil
	}
	if !p.eof() && p.src[p.i] == '?' {
		p.i++
		q.lazy = true
	}
	return q, true, nil
}

// braces reads {n}, {n,} or {n,m} at position at.
func (p *jsRegexParser) braces(at int) (lo, hi int, hasComma, hasHi bool, n int, ok bool) {
	j := at + 1
	digits := func() (int, bool) {
		s := j
		for j < len(p.src) && p.src[j] >= '0' && p.src[j] <= '9' {
			j++
		}
		if j == s {
			return 0, false
		}
		v, err := strconv.Atoi(string(p.src[s:j]))
		if err != nil || v > 1<<30 {
			v = 1 << 30
		}
		return v, true
	}
	var okLo bool
	lo, okLo = digits()
	if !okLo {
		return 0, 0, false, false, 0, false
	}
	if j < len(p.src) && p.src[j] == ',' {
		hasComma = true
		j++
		hi, hasHi = digits()
	}
	if j >= len(p.src) || p.src[j] != '}' {
		return 0, 0, false, false, 0, false
	}
	return lo, hi, hasComma, hasHi, j + 1 - at, true
}

func literal(r rune) reNode {
	if r >= 0xD800 && r <= 0xDFFF {
		return &reNever{}
	}
	return &reChar{r: r}
}

func (p *jsRegexParser) atom(depth int) (reNode, atomKind, error) {
	c := p.src[p.i]
	switch c {
	case '^', '$':
		p.i++
		return &reAssert{kind: byte(c)}, kindAssertion, nil
	case '.':
		p.i++
		return &reDot{}, kindAtom, nil
	case '*', '+', '?':
		return nil, 0, p.fail("Nothing to repeat")
	case '{':
		if _, _, _, _, _, ok := p.braces(p.i); ok {
			return nil, 0, p.fail("Nothing to repeat")
		}
		if p.unicode {
			return nil, 0, p.fail("Lone quantifier brackets")
		}
		p.i++
		return literal('{'), kindAtom, nil
	case '}', ']':
		if p.unicode {
			return nil, 0, p.fail("Lone quantifier brackets")
		}
		p.i++
		return literal(c), kindAtom, nil
	case '(':
		return p.group(depth)
	case '[':
		s, err := p.class()
		return s, kindAtom, err
	case '\\':
		return p.escape()
	}
	p.i++
	return literal(c), kindAtom, nil
}

func (p *jsRegexParser) group(depth int) (reNode, atomKind, error) {
	p.i++ // (
	var wrap func(reNode) reNode
	kind := kindAtom
	switch {
	case p.peek(0) == '?' && (p.peek(1) == '=' || p.peek(1) == '!'):
		neg := p.peek(1) == '!'
		p.i += 2
		kind = kindLookahead
		wrap = func(n reNode) reNode { return &reLook{negate: neg, node: n} }
	case p.peek(0) == '?' && p.peek(1) == '<' && (p.peek(2) == '=' || p.peek(2) == '!'):
		neg := p.peek(2) == '!'
		p.i += 3
		kind = kindAssertion
		wrap = func(n reNode) reNode { return &reLook{behind: true, negate: neg, node: n} }
	case p.peek(0) == '?' && p.peek(1) == ':':
		p.i += 2
		wrap = func(n reNode) reNode { return &reGroup{node: n} }
	case p.peek(0) == '?' && p.peek(1) == '<':
		p.i += 2
		s := p.i
		for !p.eof() && p.src[p.i] != '>' {
			r := p.src[p.i]
			start := r == '$' || r == '_' || unicode.IsLetter(r)
			part := p.i > s && (unicode.IsDigit(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Pc, r))
			if !start && !part {
				return nil, 0, p.fail("Invalid capture group name")
			}
			p.i++
		}
		if p.eof() || p.i == s {
			return nil, 0, p.fail("Invalid capture group name")
		}
		name := string(p.src[s:p.i])
		if _, dup := p.names[name]; dup {
			return nil, 0, p.fail("Duplicate capture group name")
		}
		p.i++ // >
		p.capture++
		idx := p.capture
		p.names[name] = idx
		wrap = func(n reNode) reNode { return &reGroup{capture: idx, node: n} }
	case p.peek(0) == '?':
		return nil, 0, p.fail("Invalid group")
	default:
		p.capture++
		idx := p.capture
		wrap = func(n reNode) reNode { return &reGroup{capture: idx, node: n} }
	}
	inner, err := p.disjunction(depth + 1)
	if err != nil {
		return nil, 0, err
	}
	if p.eof() {
		return nil, 0, p.fail("Unterminated group")
	}
	p.i++ // )
	return wrap(inner), kind, nil
}

func isSyntaxChar(r rune) bool { return strings.ContainsRune(`^$\.*+?()[]{}|/`, r) }

func isHex(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// hexDigits reads n hex digits at the current position.
func (p *jsRegexParser) hexDigits(n int) (rune, bool) {
	if p.i+n > len(p.src) {
		return 0, false
	}
	var v rune
	for k := 0; k < n; k++ {
		r := p.src[p.i+k]
		if !isHex(r) {
			return 0, false
		}
		h, _ := strconv.ParseUint(string(r), 16, 8)
		v = v<<4 | rune(h)
	}
	p.i += n
	return v, true
}

// charEscape parses a CharacterEscape after the backslash (p.i at the escape
// letter); in non-unicode mode anything else is an identity escape.
func (p *jsRegexParser) charEscape(inClass bool) (rune, error) {
	c := p.src[p.i]
	switch c {
	case 'f':
		p.i++
		return '\f', nil
	case 'n':
		p.i++
		return '\n', nil
	case 'r':
		p.i++
		return '\r', nil
	case 't':
		p.i++
		return '\t', nil
	case 'v':
		p.i++
		return '\v', nil
	case 'c':
		if n := p.peek(1); (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') {
			p.i += 2
			return n % 32, nil
		}
		if inClass && !p.unicode {
			if n := p.peek(1); (n >= '0' && n <= '9') || n == '_' {
				p.i += 2
				return n % 32, nil
			}
		}
		if p.unicode {
			return 0, p.fail("Invalid unicode escape")
		}
		// Annex B: "\c" not followed by a letter is a literal backslash; the
		// "c" is read again as an ordinary character.
		return '\\', nil
	case '0':
		if n := p.peek(1); n < '0' || n > '9' {
			p.i++
			return 0, nil
		}
		if p.unicode {
			return 0, p.fail("Invalid decimal escape")
		}
		return p.legacyOctal(), nil
	case 'x':
		p.i++
		if r, ok := p.hexDigits(2); ok {
			return r, nil
		}
		if p.unicode {
			return 0, p.fail("Invalid escape")
		}
		return 'x', nil
	case 'u':
		p.i++
		if p.unicode && p.peek(0) == '{' {
			save := p.i
			p.i++
			s := p.i
			for !p.eof() && isHex(p.src[p.i]) {
				p.i++
			}
			if p.i == s || p.eof() || p.src[p.i] != '}' {
				p.i = save
				return 0, p.fail("Invalid Unicode escape")
			}
			v, err := strconv.ParseUint(string(p.src[s:p.i]), 16, 32)
			p.i++
			if err != nil || v > maxRune {
				return 0, p.fail("Invalid Unicode escape")
			}
			return rune(v), nil
		}
		r, ok := p.hexDigits(4)
		if !ok {
			if p.unicode {
				return 0, p.fail("Invalid Unicode escape")
			}
			return 'u', nil
		}
		if p.unicode && r >= 0xD800 && r <= 0xDBFF && p.peek(0) == '\\' && p.peek(1) == 'u' {
			save := p.i
			p.i += 2
			if lo, ok := p.hexDigits(4); ok && lo >= 0xDC00 && lo <= 0xDFFF {
				return (r-0xD800)<<10 + (lo - 0xDC00) + 0x10000, nil
			}
			p.i = save
		}
		return r, nil
	}
	if p.unicode {
		if isSyntaxChar(c) || (inClass && c == '-') {
			p.i++
			return c, nil
		}
		return 0, p.fail("Invalid escape")
	}
	p.i++
	return c, nil
}

func (p *jsRegexParser) legacyOctal() rune {
	var v rune
	n := 0
	for !p.eof() && n < 3 && p.src[p.i] >= '0' && p.src[p.i] <= '7' {
		nv := v*8 + (p.src[p.i] - '0')
		if nv > 0377 {
			break
		}
		v = nv
		p.i++
		n++
	}
	if n == 0 { // \8 or \9: identity
		r := p.src[p.i]
		p.i++
		return r
	}
	return v
}

// classEscapeSet returns the range set for \d \D \w \W \s \S.
func classEscapeSet(c rune) ([]runeRange, bool) {
	switch c {
	case 'd':
		return jsDigitRanges, true
	case 'D':
		return complementRanges(jsDigitRanges), true
	case 'w':
		return jsWordRanges, true
	case 'W':
		return complementRanges(jsWordRanges), true
	case 's':
		return jsSpaceRanges, true
	case 'S':
		return complementRanges(jsSpaceRanges), true
	}
	return nil, false
}

var longCategoryNames = map[string]string{
	"Letter": "L", "Cased_Letter": "LC", "Uppercase_Letter": "Lu", "Lowercase_Letter": "Ll", "Titlecase_Letter": "Lt",
	"Modifier_Letter": "Lm", "Other_Letter": "Lo", "Mark": "M", "Combining_Mark": "M", "Nonspacing_Mark": "Mn",
	"Spacing_Mark": "Mc", "Enclosing_Mark": "Me", "Number": "N", "Decimal_Number": "Nd", "digit": "Nd",
	"Letter_Number": "Nl", "Other_Number": "No", "Punctuation": "P", "punct": "P", "Connector_Punctuation": "Pc",
	"Dash_Punctuation": "Pd", "Open_Punctuation": "Ps", "Close_Punctuation": "Pe", "Initial_Punctuation": "Pi",
	"Final_Punctuation": "Pf", "Other_Punctuation": "Po", "Symbol": "S", "Math_Symbol": "Sm", "Currency_Symbol": "Sc",
	"Modifier_Symbol": "Sk", "Other_Symbol": "So", "Separator": "Z", "Space_Separator": "Zs", "Line_Separator": "Zl",
	"Paragraph_Separator": "Zp", "Other": "C", "Control": "Cc", "cntrl": "Cc", "Format": "Cf", "Surrogate": "Cs",
	"Private_Use": "Co", "Unassigned": "Cn",
}

// property parses \p{...} / \P{...} (unicode mode).
func (p *jsRegexParser) property() (reProp, error) {
	neg := p.src[p.i] == 'P'
	p.i++
	if p.eof() || p.src[p.i] != '{' {
		return reProp{}, p.fail("Invalid property name")
	}
	p.i++
	s := p.i
	for !p.eof() && p.src[p.i] != '}' {
		p.i++
	}
	if p.eof() {
		return reProp{}, p.fail("Invalid property name")
	}
	body := string(p.src[s:p.i])
	p.i++
	key, value, hasValue := strings.Cut(body, "=")
	var table *unicode.RangeTable
	name := ""
	if hasValue {
		switch key {
		case "General_Category", "gc":
			if long, ok := longCategoryNames[value]; ok {
				value = long
			}
			name, table = value, unicode.Categories[value]
		case "Script", "sc", "Script_Extensions", "scx":
			name, table = value, unicode.Scripts[value]
		}
	} else {
		if long, ok := longCategoryNames[key]; ok {
			key = long
		}
		name = key
		switch key {
		case "Any":
			table = &unicode.RangeTable{R16: []unicode.Range16{{Lo: 0, Hi: 0xFFFF, Stride: 1}}, R32: []unicode.Range32{{Lo: 0x10000, Hi: maxRune, Stride: 1}}}
		case "ASCII":
			table = &unicode.RangeTable{R16: []unicode.Range16{{Lo: 0, Hi: 0x7F, Stride: 1}}, LatinOffset: 1}
		default:
			if t, ok := unicode.Categories[key]; ok {
				table = t
			} else if t, ok := unicode.Properties[key]; ok {
				table = t
			}
		}
	}
	if table == nil {
		return reProp{}, p.fail("Invalid property name")
	}
	return reProp{name: name, neg: neg, table: table}, nil
}

func (p *jsRegexParser) escape() (reNode, atomKind, error) {
	p.i++ // backslash
	if p.eof() {
		return nil, 0, p.fail("\\ at end of pattern")
	}
	c := p.src[p.i]
	switch {
	case c == 'b' || c == 'B':
		p.i++
		return &reAssert{kind: byte(c)}, kindAssertion, nil
	case c == 'p' || c == 'P':
		if !p.unicode {
			p.i++
			return literal(c), kindAtom, nil
		}
		prop, err := p.property()
		if err != nil {
			return nil, 0, err
		}
		return &reSet{props: []reProp{prop}}, kindAtom, nil
	case c >= '1' && c <= '9':
		save := p.i
		for !p.eof() && p.src[p.i] >= '0' && p.src[p.i] <= '9' {
			p.i++
		}
		n, _ := strconv.Atoi(string(p.src[save:p.i]))
		if n <= p.groups {
			return &reBackref{index: n}, kindAtom, nil
		}
		if p.unicode {
			return nil, 0, p.fail("Invalid escape")
		}
		p.i = save
		return literal(p.legacyOctal()), kindAtom, nil
	case c == 'k' && (p.unicode || p.named):
		p.i++
		if p.eof() || p.src[p.i] != '<' {
			return nil, 0, p.fail("Invalid named reference")
		}
		s := p.i + 1
		for !p.eof() && p.src[p.i] != '>' {
			p.i++
		}
		if p.eof() {
			return nil, 0, p.fail("Invalid named reference")
		}
		name := string(p.src[s:p.i])
		p.i++
		return &reBackref{name: name}, kindAtom, nil
	}
	if set, ok := classEscapeSet(c); ok {
		p.i++
		return &reSet{ranges: normalizeRanges(set)}, kindAtom, nil
	}
	r, err := p.charEscape(false)
	if err != nil {
		return nil, 0, err
	}
	return literal(r), kindAtom, nil
}

// classAtom reads one class member: a single rune, a set, or a property.
func (p *jsRegexParser) classAtom() (r rune, set []runeRange, prop *reProp, err error) {
	c := p.src[p.i]
	if c != '\\' {
		p.i++
		return c, nil, nil, nil
	}
	p.i++
	if p.eof() {
		return 0, nil, nil, p.fail("\\ at end of pattern")
	}
	c = p.src[p.i]
	switch {
	case c == 'b':
		p.i++
		return '\b', nil, nil, nil
	case c == '-' && p.unicode:
		p.i++
		return '-', nil, nil, nil
	case (c == 'p' || c == 'P') && p.unicode:
		pr, err := p.property()
		return 0, nil, &pr, err
	case c >= '1' && c <= '9':
		if p.unicode {
			return 0, nil, nil, p.fail("Invalid class escape")
		}
		return p.legacyOctal(), nil, nil, nil
	case c == 'k' && p.unicode:
		return 0, nil, nil, p.fail("Invalid class escape")
	}
	if s, ok := classEscapeSet(c); ok {
		p.i++
		return 0, s, nil, nil
	}
	r, err = p.charEscape(true)
	return r, nil, nil, err
}

func (p *jsRegexParser) class() (*reSet, error) {
	p.i++ // [
	set := &reSet{}
	if !p.eof() && p.src[p.i] == '^' {
		set.negate = true
		p.i++
	}
	var ranges []runeRange
	add := func(r rune) {
		if !p.unicode && r >= 0x10000 {
			return // two surrogate code units in non-unicode mode; neither can match
		}
		ranges = append(ranges, runeRange{r, r})
	}
	for {
		if p.eof() {
			return nil, p.fail("Unterminated character class")
		}
		if p.src[p.i] == ']' {
			p.i++
			break
		}
		lo, loSet, loProp, err := p.classAtom()
		if err != nil {
			return nil, err
		}
		isSet := loSet != nil || loProp != nil
		if !p.eof() && p.src[p.i] == '-' && p.peek(1) != ']' && p.peek(1) != -1 {
			p.i++ // -
			hi, hiSet, hiProp, err := p.classAtom()
			if err != nil {
				return nil, err
			}
			hiIsSet := hiSet != nil || hiProp != nil
			if isSet || hiIsSet {
				if p.unicode {
					return nil, p.fail("Invalid character class")
				}
				// Annex B: the dash is literal.
				ranges = append(ranges, loSet...)
				ranges = append(ranges, hiSet...)
				if !isSet {
					add(lo)
				}
				if !hiIsSet {
					add(hi)
				}
				add('-')
				continue
			}
			if hi < lo {
				return nil, p.fail("Range out of order in character class")
			}
			ranges = append(ranges, runeRange{lo, hi})
			continue
		}
		switch {
		case loProp != nil:
			set.props = append(set.props, *loProp)
		case loSet != nil:
			ranges = append(ranges, loSet...)
		default:
			add(lo)
		}
	}
	set.ranges = normalizeRanges(ranges)
	return set, nil
}

// ── RE2 emission ─────────────────────────────────────────────────────────────

// re2Of renders the AST as RE2 syntax; ok is false when it holds lookaround,
// backreferences or repeat counts RE2 refuses.
func re2Of(n reNode) (string, bool) {
	var b strings.Builder
	ok := writeRE2(&b, n)
	return b.String(), ok
}

func writeRE2(b *strings.Builder, n reNode) bool {
	switch x := n.(type) {
	case *reAlt:
		for i, a := range x.alts {
			if i > 0 {
				b.WriteByte('|')
			}
			if !writeRE2(b, a) {
				return false
			}
		}
	case *reSeq:
		for _, it := range x.items {
			if _, isAlt := it.(*reAlt); isAlt {
				b.WriteString("(?:")
				if !writeRE2(b, it) {
					return false
				}
				b.WriteByte(')')
				continue
			}
			if !writeRE2(b, it) {
				return false
			}
		}
	case *reChar:
		b.WriteString(regexp.QuoteMeta(string(x.r)))
	case *reNever:
		b.WriteString(neverMatch)
	case *reDot:
		b.WriteString(`[^\n\r\x{2028}\x{2029}]`)
	case *reAssert:
		switch x.kind {
		case '^', '$':
			b.WriteByte(x.kind)
		default:
			b.WriteString(`\` + string(x.kind))
		}
	case *reSet:
		if len(x.props) == 0 {
			switch {
			case len(x.ranges) == 0 && x.negate:
				b.WriteString(`[\x{0}-\x{10FFFF}]`)
			case len(x.ranges) == 0:
				b.WriteString(neverMatch)
			case x.negate:
				b.WriteString("[^" + renderRanges(x.ranges) + "]")
			default:
				b.WriteString("[" + renderRanges(x.ranges) + "]")
			}
			return true
		}
		body := renderRanges(x.ranges)
		for _, pr := range x.props {
			tok := `\p{`
			if pr.neg {
				tok = `\P{`
			}
			if _, known := unicode.Categories[pr.name]; !known {
				if _, known := unicode.Scripts[pr.name]; !known {
					return false // a binary property RE2 does not name
				}
			}
			body += tok + pr.name + "}"
		}
		if x.negate {
			b.WriteString("[^" + body + "]")
		} else {
			b.WriteString("[" + body + "]")
		}
	case *reGroup:
		if x.capture > 0 {
			b.WriteByte('(')
		} else {
			b.WriteString("(?:")
		}
		if !writeRE2(b, x.node) {
			return false
		}
		b.WriteByte(')')
	case *reRepeat:
		if x.min > 1000 || x.max > 1000 {
			return false
		}
		b.WriteString("(?:")
		if !writeRE2(b, x.node) {
			return false
		}
		b.WriteByte(')')
		switch {
		case x.min == 0 && x.max == -1:
			b.WriteByte('*')
		case x.min == 1 && x.max == -1:
			b.WriteByte('+')
		case x.min == 0 && x.max == 1:
			b.WriteByte('?')
		case x.max == -1:
			fmt.Fprintf(b, "{%d,}", x.min)
		case x.min == x.max:
			fmt.Fprintf(b, "{%d}", x.min)
		default:
			fmt.Fprintf(b, "{%d,%d}", x.min, x.max)
		}
		if x.lazy {
			b.WriteByte('?')
		}
	default: // *reLook, *reBackref
		return false
	}
	return true
}

// ── backtracking matcher ─────────────────────────────────────────────────────

const (
	btMaxSteps = 5_000_000
	btMaxDepth = 200_000
)

type btRegexp struct{ re *jsRegex }

type btExhausted struct{}

type btMatch struct {
	in           []rune
	caps         []int
	steps, depth int
}

func (m *btMatch) tick() {
	m.steps++
	if m.steps > btMaxSteps || m.depth > btMaxDepth {
		panic(btExhausted{})
	}
}

// MatchString is RegExp.prototype.test: a match anywhere in s. A match that
// runs out of budget reports false.
func (b *btRegexp) MatchString(s string) (matched bool) {
	m := &btMatch{in: []rune(s), caps: make([]int, 2*(b.re.ncap+1))}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(btExhausted); !ok {
				panic(r)
			}
			matched = false
		}
	}()
	for start := 0; start <= len(m.in); start++ {
		for i := range m.caps {
			m.caps[i] = -1
		}
		if m.match(b.re.root, start, func(int) bool { return true }) {
			return true
		}
	}
	return false
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

func (m *btMatch) wordAt(i int) bool { return i >= 0 && i < len(m.in) && isWordRune(m.in[i]) }

func (m *btMatch) match(n reNode, i int, k func(int) bool) bool {
	m.tick()
	m.depth++
	defer func() { m.depth-- }()
	switch x := n.(type) {
	case *reChar:
		return i < len(m.in) && m.in[i] == x.r && k(i+1)
	case *reSet:
		return i < len(m.in) && x.has(m.in[i]) && k(i+1)
	case *reDot:
		if i >= len(m.in) {
			return false
		}
		switch m.in[i] {
		case '\n', '\r', 0x2028, 0x2029:
			return false
		}
		return k(i + 1)
	case *reNever:
		return false
	case *reAssert:
		switch x.kind {
		case '^':
			return i == 0 && k(i)
		case '$':
			return i == len(m.in) && k(i)
		case 'b':
			return m.wordAt(i-1) != m.wordAt(i) && k(i)
		default:
			return m.wordAt(i-1) == m.wordAt(i) && k(i)
		}
	case *reSeq:
		return m.seq(x.items, i, k)
	case *reAlt:
		for _, a := range x.alts {
			if m.match(a, i, k) {
				return true
			}
		}
		return false
	case *reGroup:
		if x.capture == 0 {
			return m.match(x.node, i, k)
		}
		c := x.capture
		return m.match(x.node, i, func(j int) bool {
			s, e := m.caps[2*c], m.caps[2*c+1]
			m.caps[2*c], m.caps[2*c+1] = i, j
			if k(j) {
				return true
			}
			m.caps[2*c], m.caps[2*c+1] = s, e
			return false
		})
	case *reLook:
		saved := append([]int(nil), m.caps...)
		found := false
		if x.behind {
			for j := i; j >= 0 && !found; j-- {
				found = m.match(x.node, j, func(e int) bool { return e == i })
			}
		} else {
			found = m.match(x.node, i, func(int) bool { return true })
		}
		if x.negate {
			copy(m.caps, saved)
			return !found && k(i)
		}
		if found && k(i) {
			return true
		}
		copy(m.caps, saved)
		return false
	case *reBackref:
		s, e := m.caps[2*x.index], m.caps[2*x.index+1]
		if s < 0 || e < 0 {
			return k(i)
		}
		n := e - s
		if i+n > len(m.in) {
			return false
		}
		for d := 0; d < n; d++ {
			if m.in[i+d] != m.in[s+d] {
				return false
			}
		}
		return k(i + n)
	case *reRepeat:
		return m.repeat(x, i, 0, k)
	}
	return false
}

func (m *btMatch) seq(items []reNode, i int, k func(int) bool) bool {
	if len(items) == 0 {
		return k(i)
	}
	return m.match(items[0], i, func(j int) bool { return m.seq(items[1:], j, k) })
}

func (m *btMatch) repeat(x *reRepeat, i, count int, k func(int) bool) bool {
	m.tick()
	if count < x.min {
		return m.iterate(x, i, count, k)
	}
	if x.max != -1 && count >= x.max {
		return k(i)
	}
	if x.lazy {
		return k(i) || m.iterate(x, i, count, k)
	}
	return m.iterate(x, i, count, k) || k(i)
}

// iterate runs one more iteration. Captures inside the atom reset at the start
// of each iteration, and once the minimum is met an empty iteration fails, as
// in the ECMAScript RepeatMatcher.
func (m *btMatch) iterate(x *reRepeat, i, count int, k func(int) bool) bool {
	saved := make([]int, 0, 2*len(x.caps))
	for _, c := range x.caps {
		saved = append(saved, m.caps[2*c], m.caps[2*c+1])
		m.caps[2*c], m.caps[2*c+1] = -1, -1
	}
	ok := m.match(x.node, i, func(j int) bool {
		if j == i && count >= x.min {
			return false
		}
		return m.repeat(x, j, count+1, k)
	})
	if !ok {
		for n, c := range x.caps {
			m.caps[2*c], m.caps[2*c+1] = saved[2*n], saved[2*n+1]
		}
	}
	return ok
}
