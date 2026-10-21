// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package jsjson parses and serializes JSON exactly as JavaScript's JSON.parse
// and JSON.stringify do, so a Go server can answer byte-for-byte like the Node
// one it replaced. The differences from encoding/json that matter on the wire:
//
//   - Object key order is JavaScript's own-property order: array-index keys
//     ("0", "7", "42") first in ascending numeric order, then every other key
//     in insertion order. A duplicated key keeps its first position and takes
//     its last value.
//   - Numbers are IEEE doubles and print as Number#toString does (1e21, 1.5e-7,
//     100, never 1e+02); NaN and ±Infinity print as null.
//   - Strings escape only '"', '\\' and C0 controls (\b \f \n \r \t, else
//     \u00xx); '<', '>', '&', U+2028 and U+2029 stay raw. A lone surrogate
//     escape such as "\ud800" survives a round trip: it is held in the Go
//     string as its WTF-8 bytes and printed back as the escape.
//
// Parsed values are nil (null), bool, float64, string, []any and *Object.
package jsjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Object is a JSON object with JavaScript property order.
type Object struct {
	index []string // array-index keys, ascending numerically
	named []string // other keys, insertion order
	vals  map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object { return &Object{vals: map[string]any{}} }

// Len is the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.vals)
}

// Get returns the value of an own key.
func (o *Object) Get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.vals[key]
	return v, ok
}

// Has reports whether key is an own key.
func (o *Object) Has(key string) bool {
	_, ok := o.Get(key)
	return ok
}

// Set creates or replaces key. A new key takes its JavaScript position.
func (o *Object) Set(key string, v any) {
	if o.vals == nil {
		o.vals = map[string]any{}
	}
	if _, ok := o.vals[key]; !ok {
		if n, isIndex := arrayIndex(key); isIndex {
			at := sort.Search(len(o.index), func(i int) bool {
				m, _ := arrayIndex(o.index[i])
				return m >= n
			})
			o.index = append(o.index, "")
			copy(o.index[at+1:], o.index[at:])
			o.index[at] = key
		} else {
			o.named = append(o.named, key)
		}
	}
	o.vals[key] = v
}

// Keys returns the own keys in JavaScript order.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	keys := make([]string, 0, len(o.index)+len(o.named))
	keys = append(keys, o.index...)
	return append(keys, o.named...)
}

// arrayIndex reports whether key is a canonical array index (0 … 2^32-2).
func arrayIndex(key string) (uint64, bool) {
	if key == "" || len(key) > 10 || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	var n uint64
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, n < 1<<32-1
}

// ErrSyntax is returned (wrapped) for text JSON.parse would refuse.
var ErrSyntax = errors.New("invalid JSON")

// maxDepth bounds nesting. V8 parses deeper documents, but nothing a Pages
// endpoint accepts nests anywhere near this, and a bound keeps a hostile body
// from costing a deep recursion.
const maxDepth = 10000

// Parse parses one JSON text (surrounding JSON whitespace allowed). Input that
// is not valid UTF-8 is decoded first as a WHATWG decoder would (each maximal
// invalid subsequence becomes U+FFFD), the way Node turns bytes into a string.
func Parse(data []byte) (any, error) {
	if !utf8.Valid(data) {
		data = []byte(DecodeUTF8(data))
	}
	p := parser{s: data}
	p.skip()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.skip()
	if p.i != len(p.s) {
		return nil, p.fail("unexpected trailing data")
	}
	return v, nil
}

type parser struct {
	s []byte
	i int
}

func (p *parser) fail(what string) error {
	return fmt.Errorf("%w: %s at position %d", ErrSyntax, what, p.i)
}

func (p *parser) skip() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (any, error) {
	if depth > maxDepth {
		return nil, p.fail("nesting too deep")
	}
	if p.i >= len(p.s) {
		return nil, p.fail("unexpected end")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		return p.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case bytes.HasPrefix(p.s[p.i:], []byte("true")):
		p.i += 4
		return true, nil
	case bytes.HasPrefix(p.s[p.i:], []byte("false")):
		p.i += 5
		return false, nil
	case bytes.HasPrefix(p.s[p.i:], []byte("null")):
		p.i += 4
		return nil, nil
	}
	return nil, p.fail("unexpected token")
}

func (p *parser) object(depth int) (any, error) {
	p.i++ // {
	obj := NewObject()
	p.skip()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return obj, nil
	}
	for {
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, p.fail("expected property name")
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.fail("expected ':'")
		}
		p.i++
		p.skip()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		obj.Set(key, v)
		p.skip()
		if p.i >= len(p.s) {
			return nil, p.fail("unterminated object")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return obj, nil
		default:
			return nil, p.fail("expected ',' or '}'")
		}
	}
}

func (p *parser) array(depth int) (any, error) {
	p.i++ // [
	arr := []any{}
	p.skip()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return arr, nil
	}
	for {
		p.skip()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.skip()
		if p.i >= len(p.s) {
			return nil, p.fail("unterminated array")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return arr, nil
		default:
			return nil, p.fail("expected ',' or ']'")
		}
	}
}

func hexVal(c byte) (rune, bool) {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0'), true
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10, true
	}
	return 0, false
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	var r rune
	for k := 0; k < 4; k++ {
		h, ok := hexVal(p.s[p.i+k])
		if !ok {
			return 0, false
		}
		r = r<<4 | h
	}
	p.i += 4
	return r, true
}

func (p *parser) str() (string, error) {
	p.i++ // opening quote
	var b []byte
	start := p.i
	for {
		if p.i >= len(p.s) {
			return "", p.fail("unterminated string")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			b = append(b, p.s[start:p.i]...)
			p.i++
			return string(b), nil
		case c < 0x20:
			return "", p.fail("bad control character in string")
		case c == '\\':
			b = append(b, p.s[start:p.i]...)
			p.i++
			if p.i >= len(p.s) {
				return "", p.fail("unterminated string")
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case '"', '\\', '/':
				b = append(b, e)
			case 'b':
				b = append(b, '\b')
			case 'f':
				b = append(b, '\f')
			case 'n':
				b = append(b, '\n')
			case 'r':
				b = append(b, '\r')
			case 't':
				b = append(b, '\t')
			case 'u':
				r, ok := p.hex4()
				if !ok {
					return "", p.fail("bad unicode escape")
				}
				if utf16.IsSurrogate(r) && r < 0xDC00 && p.i+6 <= len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
					save := p.i
					p.i += 2
					if lo, ok := p.hex4(); ok && lo >= 0xDC00 && lo <= 0xDFFF {
						b = utf8.AppendRune(b, utf16.DecodeRune(r, lo))
						break
					}
					p.i = save
				}
				b = appendWTF8(b, r)
			default:
				p.i--
				return "", p.fail("bad escape")
			}
			start = p.i
		default:
			p.i++
		}
	}
}

// appendWTF8 is utf8.AppendRune, except that a surrogate code point is kept
// as its generalized UTF-8 bytes instead of becoming U+FFFD.
func appendWTF8(b []byte, r rune) []byte {
	if utf16.IsSurrogate(r) {
		return append(b, 0xED, byte(0x80|(r>>6)&0x3F), byte(0x80|r&0x3F)) //nolint:gosec // G115: each value is masked to 6 bits
	}
	return utf8.AppendRune(b, r)
}

func (p *parser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	digits := func() int {
		n := 0
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
			n++
		}
		return n
	}
	if p.i < len(p.s) && p.s[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		return nil, p.fail("bad number")
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if digits() == 0 {
			return nil, p.fail("bad number")
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			return nil, p.fail("bad number")
		}
	}
	// Out-of-range literals become ±Infinity or 0, as in JavaScript.
	f, err := strconv.ParseFloat(string(p.s[start:p.i]), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return nil, p.fail("bad number")
	}
	return f, nil
}

// DecodeUTF8 converts bytes to a valid UTF-8 string the way the WHATWG
// decoder (and so Node's Buffer#toString) does: each maximal subpart of an
// ill-formed sequence becomes one U+FFFD.
func DecodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 0, len(b)+8)
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			out = append(out, c)
			i++
			continue
		}
		need, lo, hi := 0, byte(0x80), byte(0xBF)
		switch {
		case c >= 0xC2 && c <= 0xDF:
			need = 1
		case c >= 0xE0 && c <= 0xEF:
			need = 2
			switch c {
			case 0xE0:
				lo = 0xA0
			case 0xED:
				hi = 0x9F
			}
		case c >= 0xF0 && c <= 0xF4:
			need = 3
			switch c {
			case 0xF0:
				lo = 0x90
			case 0xF4:
				hi = 0x8F
			}
		default:
			out = append(out, "\uFFFD"...)
			i++
			continue
		}
		j := i + 1
		ok := true
		for k := 0; k < need; k++ {
			if j >= len(b) || b[j] < lo || b[j] > hi {
				ok = false
				break
			}
			lo, hi = 0x80, 0xBF
			j++
		}
		if !ok {
			out = append(out, "\uFFFD"...)
			i = j
			continue
		}
		out = append(out, b[i:j]...)
		i = j
	}
	return string(out)
}

// Marshal serializes v as JSON.stringify would. v may be any parsed value
// (nil, bool, float64, string, []any, *Object), an int/int64, or a
// json.RawMessage / []byte holding JSON, which is re-read and printed in
// JavaScript form.
func Marshal(v any) ([]byte, error) {
	return AppendValue(nil, v)
}

// MustMarshal is Marshal for values that cannot fail (no RawMessage inside).
func MustMarshal(v any) []byte {
	b, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// Normalize re-serializes JSON text in JavaScript form:
// JSON.stringify(JSON.parse(text)).
func Normalize(raw []byte) ([]byte, error) {
	v, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return Marshal(v)
}

// AppendValue appends the serialization of v to b.
func AppendValue(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, "null"...), nil
	case bool:
		if x {
			return append(b, "true"...), nil
		}
		return append(b, "false"...), nil
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return append(b, "null"...), nil
		}
		return append(b, FormatNumber(x)...), nil
	case int:
		return strconv.AppendInt(b, int64(x), 10), nil
	case int64:
		return strconv.AppendInt(b, x, 10), nil
	case string:
		return AppendString(b, x), nil
	case []any:
		b = append(b, '[')
		for i, item := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = AppendValue(b, item); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case *Object:
		b = append(b, '{')
		for i, k := range x.Keys() {
			if i > 0 {
				b = append(b, ',')
			}
			b = AppendString(b, k)
			b = append(b, ':')
			var err error
			if b, err = AppendValue(b, x.vals[k]); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	case json.RawMessage:
		return appendRaw(b, x)
	case []byte:
		return appendRaw(b, x)
	}
	return nil, fmt.Errorf("jsjson: cannot serialize %T", v)
}

func appendRaw(b, raw []byte) ([]byte, error) {
	parsed, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return AppendValue(b, parsed)
}

const hexDigits = "0123456789abcdef"

// AppendString appends s as a JSON string literal, escaped as
// JSON.stringify escapes it.
func AppendString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				b = append(b, '\\', '"')
			case '\\':
				b = append(b, '\\', '\\')
			case '\b':
				b = append(b, '\\', 'b')
			case '\f':
				b = append(b, '\\', 'f')
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				if c < 0x20 {
					b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xF])
				} else {
					b = append(b, c)
				}
			}
			i++
			continue
		}
		// A WTF-8 surrogate (ED A0..BF xx) is a lone surrogate from a parsed
		// escape: print it back as the escape.
		if c == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			r := rune(0xD000) | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
			b = append(b, '\\', 'u', 'd', hexDigits[(r>>8)&0xF], hexDigits[(r>>4)&0xF], hexDigits[r&0xF])
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, "\uFFFD"...)
		} else {
			b = append(b, s[i:i+size]...)
		}
		i += size
	}
	return append(b, '"')
}

// FormatNumber renders f as JavaScript's Number#toString does.
func FormatNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	case f < 0:
		return "-" + FormatNumber(-f)
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±xx
	mant, expPart, _ := bytes.Cut([]byte(e), []byte("e"))
	digits := string(bytes.Replace(mant, []byte("."), nil, 1))
	exp, _ := strconv.Atoi(string(expPart))
	k := len(digits)
	n := exp + 1
	switch {
	case k <= n && n <= 21:
		return digits + string(bytes.Repeat([]byte("0"), n-k))
	case 0 < n && n <= 21:
		return digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return "0." + string(bytes.Repeat([]byte("0"), -n)) + digits
	}
	sign := "+"
	if n-1 < 0 {
		sign = "-"
	}
	abs := n - 1
	if abs < 0 {
		abs = -abs
	}
	if k == 1 {
		return digits + "e" + sign + strconv.Itoa(abs)
	}
	return digits[:1] + "." + digits[1:] + "e" + sign + strconv.Itoa(abs)
}

// IsSafeInteger is Number.isSafeInteger for a parsed value.
func IsSafeInteger(v any) bool {
	f, ok := v.(float64)
	return ok && !math.IsInf(f, 0) && f == math.Trunc(f) && math.Abs(f) <= 1<<53-1
}

// ToString is JavaScript's String(v) for parsed primitive values (objects
// print as "[object Object]", arrays join their elements with ",").
func ToString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return FormatNumber(x)
	case string:
		return x
	case []any:
		var b []byte
		for i, item := range x {
			if i > 0 {
				b = append(b, ',')
			}
			if item != nil {
				b = append(b, ToString(item)...)
			}
		}
		return string(b)
	case *Object:
		return "[object Object]"
	}
	return fmt.Sprint(v)
}

// Number is JavaScript's Number(string): a whitespace-trimmed decimal
// literal, a 0x/0o/0b integer, ±Infinity, "" (or only whitespace) → 0, and
// NaN for anything else.
func Number(s string) float64 {
	s = strings.TrimFunc(s, isJSSpace)
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
			f := 0.0
			for i := 2; i < len(s); i++ {
				d, ok := hexVal(s[i])
				if !ok || int(d) >= base {
					return math.NaN()
				}
				f = f*float64(base) + float64(d)
			}
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

// isJSSpace is JavaScript's WhiteSpace and LineTerminator set.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}
