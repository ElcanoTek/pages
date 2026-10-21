// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package jsonv is a JSON value model with JavaScript semantics. The Node
// server stored, hashed and served JSON exactly as JSON.parse and
// JSON.stringify produce it, and those bytes are now in the database: key order
// (insertion order, with array-index keys first), number formatting
// (Number.prototype.toString), and string escaping all have to match for
// stored hashes and served pages to stay byte-identical.
//
// A value is one of:
//
//	nil        JSON null
//	bool
//	float64    every JSON number (JS has only doubles)
//	string
//	[]any
//	*Object    an ordered object
//
// Parse produces exactly these. Stringify also accepts common Go shapes
// (map[string]any, json.Number, ints, structs via encoding/json) so callers can
// hand it what they have, but only *Object preserves a caller's key order.
package jsonv

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Object is an ordered JSON object with JavaScript property order: keys that
// are canonical array indices ("0".."4294967294") enumerate first in ascending
// numeric order, every other key in insertion order. Setting an existing key
// keeps its position, which is also what JSON.parse does with a duplicate key.
// The zero value is an empty object ready to use.
type Object struct {
	keys  []string
	vals  []any
	index map[string]int // built once an object outgrows a linear scan
	nidx  int            // how many keys are array indices
}

const indexThreshold = 8

// NewObject returns an empty object.
func NewObject() *Object { return &Object{} }

// ObjectOf builds an object from alternating key, value arguments, in order.
// It panics on an odd argument count or a non-string key (a programming error).
func ObjectOf(kv ...any) *Object {
	if len(kv)%2 != 0 {
		panic("jsonv.ObjectOf: odd argument count")
	}
	o := &Object{keys: make([]string, 0, len(kv)/2), vals: make([]any, 0, len(kv)/2)}
	for i := 0; i < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Len is the number of keys.
func (o *Object) Len() int {
	if o == nil {
		return 0
	}
	return len(o.keys)
}

func (o *Object) find(key string) int {
	if o == nil {
		return -1
	}
	if o.index != nil {
		if i, ok := o.index[key]; ok {
			return i
		}
		return -1
	}
	for i, k := range o.keys {
		if k == key {
			return i
		}
	}
	return -1
}

func (o *Object) reindex() {
	o.index = make(map[string]int, len(o.keys))
	for i, k := range o.keys {
		o.index[k] = i
	}
}

// Get returns the value at key and whether the key is present.
func (o *Object) Get(key string) (any, bool) {
	if i := o.find(key); i >= 0 {
		return o.vals[i], true
	}
	return nil, false
}

// Value returns the value at key, or nil when absent.
func (o *Object) Value(key string) any {
	v, _ := o.Get(key)
	return v
}

// Has reports whether key is present.
func (o *Object) Has(key string) bool { return o.find(key) >= 0 }

// Set stores value at key, appending the key when it is new.
func (o *Object) Set(key string, value any) {
	if i := o.find(key); i >= 0 {
		o.vals[i] = value
		return
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, value)
	if IsArrayIndex(key) {
		o.nidx++
	}
	switch {
	case o.index != nil:
		o.index[key] = len(o.keys) - 1
	case len(o.keys) > indexThreshold:
		o.reindex()
	}
}

// Delete removes key.
func (o *Object) Delete(key string) {
	i := o.find(key)
	if i < 0 {
		return
	}
	o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
	o.vals = append(o.vals[:i:i], o.vals[i+1:]...)
	if IsArrayIndex(key) {
		o.nidx--
	}
	if o.index != nil {
		o.reindex()
	}
}

// Keys returns the keys in JavaScript enumeration order (Object.keys), as a
// new slice the caller may modify.
func (o *Object) Keys() []string {
	if o == nil {
		return nil
	}
	if o.nidx == 0 {
		return append([]string(nil), o.keys...)
	}
	out := make([]string, 0, len(o.keys))
	var indices []string
	for _, k := range o.keys {
		if IsArrayIndex(k) {
			indices = append(indices, k)
		} else {
			out = append(out, k)
		}
	}
	sort.Slice(indices, func(i, j int) bool {
		a, _ := strconv.ParseUint(indices[i], 10, 64)
		b, _ := strconv.ParseUint(indices[j], 10, 64)
		return a < b
	})
	return append(indices, out...)
}

// Range calls fn for each entry in JavaScript enumeration order until fn
// returns false. It does not allocate for objects without array-index keys.
func (o *Object) Range(fn func(key string, value any) bool) {
	if o == nil {
		return
	}
	if o.nidx == 0 {
		for i, k := range o.keys {
			if !fn(k, o.vals[i]) {
				return
			}
		}
		return
	}
	for _, k := range o.Keys() {
		if !fn(k, o.vals[o.find(k)]) {
			return
		}
	}
}

// Clone returns a shallow copy.
func (o *Object) Clone() *Object {
	c := &Object{keys: append([]string(nil), o.keys...), vals: append([]any(nil), o.vals...), nidx: o.nidx}
	if o.index != nil {
		c.reindex()
	}
	return c
}

// MarshalJSON writes the object as JSON.stringify would.
func (o *Object) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	if err := write(&b, o); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// UnmarshalJSON parses with JSON.parse semantics into the object.
func (o *Object) UnmarshalJSON(data []byte) error {
	v, err := Parse(data)
	if err != nil {
		return err
	}
	obj, ok := v.(*Object)
	if !ok {
		return errors.New("jsonv: JSON value is not an object")
	}
	*o = *obj
	return nil
}

// IsArrayIndex reports whether key is a canonical array index, the keys
// JavaScript enumerates before all others.
func IsArrayIndex(key string) bool {
	if key == "" || len(key) > 10 {
		return false
	}
	if key == "0" {
		return true
	}
	if key[0] < '1' || key[0] > '9' {
		return false
	}
	for i := 1; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseUint(key, 10, 64)
	return err == nil && n < 4294967295
}

// ── parsing ─────────────────────────────────────────────────────────────────

// SyntaxError is a JSON.parse failure.
type SyntaxError struct {
	Offset int
	msg    string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("%s at position %d", e.msg, e.Offset) }

// Parse is JSON.parse: the strict JSON grammar, whitespace limited to space,
// tab, CR and LF, numbers as doubles (an out-of-range literal becomes ±Inf, as
// in JavaScript), and a duplicate key keeping its first position with the last
// value. Invalid UTF-8 and lone surrogate escapes become U+FFFD, since a Go
// string (and a Postgres text column) cannot hold a lone surrogate.
func Parse(data []byte) (any, error) {
	p := parser{s: data}
	p.ws()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, p.err("Unexpected non-whitespace character after JSON")
	}
	return v, nil
}

// ParseString is Parse over a string.
func ParseString(s string) (any, error) { return Parse([]byte(s)) }

const maxDepth = 10000

type parser struct {
	s []byte
	i int
}

func (p *parser) err(msg string) error { return &SyntaxError{Offset: p.i, msg: msg} }

func (p *parser) ws() {
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
		return nil, p.err("JSON nested too deeply")
	}
	if p.i >= len(p.s) {
		return nil, p.err("Unexpected end of JSON input")
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		return p.str()
	case c == 't':
		return p.lit("true", true)
	case c == 'f':
		return p.lit("false", false)
	case c == 'n':
		return p.lit("null", nil)
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	default:
		return nil, p.err("Unexpected token")
	}
}

func (p *parser) lit(word string, v any) (any, error) {
	if !bytes.HasPrefix(p.s[p.i:], []byte(word)) {
		return nil, p.err("Unexpected token")
	}
	p.i += len(word)
	return v, nil
}

func (p *parser) object(depth int) (any, error) {
	p.i++ // {
	o := NewObject()
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return o, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, p.err("Expected property name")
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, p.err("Expected ':' after property name")
		}
		p.i++
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		o.Set(k.(string), v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, p.err("Unexpected end of JSON input")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return o, nil
		default:
			return nil, p.err("Expected ',' or '}' after property value")
		}
	}
}

func (p *parser) array(depth int) (any, error) {
	p.i++ // [
	arr := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return arr, nil
	}
	for {
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		arr = append(arr, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, p.err("Unexpected end of JSON input")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return arr, nil
		default:
			return nil, p.err("Expected ',' or ']' after array element")
		}
	}
}

func hexVal(c byte) rune {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0')
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10
	}
	return -1
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	var r rune
	for k := 0; k < 4; k++ {
		h := hexVal(p.s[p.i+k])
		if h < 0 {
			return 0, false
		}
		r = r<<4 | h
	}
	p.i += 4
	return r, true
}

func (p *parser) str() (any, error) {
	p.i++ // opening quote
	start := p.i
	// Fast path: no escapes, valid UTF-8.
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '"' {
			s := p.s[start:p.i]
			p.i++
			if utf8.Valid(s) {
				return string(s), nil
			}
			return strings.ToValidUTF8(string(s), "�"), nil
		}
		if c == '\\' || c < 0x20 {
			break
		}
		p.i++
	}
	var b strings.Builder
	b.Write(p.s[start:p.i])
	for {
		if p.i >= len(p.s) {
			return nil, p.err("Unterminated string in JSON")
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			s := b.String()
			if !utf8.ValidString(s) {
				s = strings.ToValidUTF8(s, "�")
			}
			return s, nil
		case c < 0x20:
			return nil, p.err("Bad control character in string literal")
		case c != '\\':
			b.WriteByte(c)
			p.i++
			continue
		}
		p.i++ // backslash
		if p.i >= len(p.s) {
			return nil, p.err("Unterminated string in JSON")
		}
		e := p.s[p.i]
		p.i++
		switch e {
		case '"', '\\', '/':
			b.WriteByte(e)
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return nil, p.err("Bad Unicode escape in JSON")
			}
			if utf16.IsSurrogate(r) {
				if r < 0xDC00 && p.i+1 < len(p.s) && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
					save := p.i
					p.i += 2
					if lo, ok := p.hex4(); ok && lo >= 0xDC00 && lo <= 0xDFFF {
						b.WriteRune(utf16.DecodeRune(r, lo))
						continue
					}
					p.i = save
				}
				r = utf8.RuneError
			}
			b.WriteRune(r)
		default:
			p.i--
			return nil, p.err("Bad escaped character in JSON")
		}
	}
}

func (p *parser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	if p.i >= len(p.s) {
		return nil, p.err("No number after minus sign in JSON")
	}
	switch {
	case p.s[p.i] == '0':
		p.i++
	case p.s[p.i] >= '1' && p.s[p.i] <= '9':
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
	default:
		return nil, p.err("No number after minus sign in JSON")
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		d := p.i
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		if p.i == d {
			return nil, p.err("Unterminated fractional number in JSON")
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		d := p.i
		for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
			p.i++
		}
		if p.i == d {
			return nil, p.err("Exponent part is missing a number in JSON")
		}
	}
	f, err := strconv.ParseFloat(string(p.s[start:p.i]), 64)
	if err != nil {
		var ne *strconv.NumError
		if !errors.As(err, &ne) || !errors.Is(ne.Err, strconv.ErrRange) {
			return nil, p.err("Bad number")
		}
		// ParseFloat already returns ±Inf or ±0 for out-of-range literals,
		// which is what JavaScript produces.
	}
	return f, nil
}

// ── stringifying ────────────────────────────────────────────────────────────

// Stringify is JSON.stringify(v) for the value model, with JavaScript number
// formatting and string escaping. Non-finite numbers become null, as in
// JavaScript. Values outside the model are converted through encoding/json
// first. It panics only on a value encoding/json cannot marshal either; use
// Marshal to get that as an error.
func Stringify(v any) string {
	s, err := Marshal(v)
	if err != nil {
		panic(err)
	}
	return s
}

// Marshal is Stringify returning conversion failures as an error.
func Marshal(v any) (string, error) {
	var b strings.Builder
	if err := write(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Writer is implemented by types that write themselves in JSON.stringify form
// (with JavaScript number formatting). Stringify prefers it over MarshalJSON.
type Writer interface {
	WriteJSON(b *strings.Builder) error
}

func write(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case float64:
		writeNumber(b, x)
	case float32:
		writeNumber(b, float64(x))
	case int:
		b.WriteString(strconv.Itoa(x))
	case int64:
		writeNumber(b, float64(x))
	case int32:
		b.WriteString(strconv.FormatInt(int64(x), 10))
	case uint:
		writeNumber(b, float64(x))
	case uint64:
		writeNumber(b, float64(x))
	case json.Number:
		f, err := x.Float64()
		if err != nil {
			return fmt.Errorf("jsonv: bad number %q", string(x))
		}
		writeNumber(b, f)
	case string:
		WriteString(b, x)
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := write(b, e); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			WriteString(b, e)
		}
		b.WriteByte(']')
	case *Object:
		if x == nil {
			b.WriteString("null")
			return nil
		}
		b.WriteByte('{')
		first := true
		var err error
		x.Range(func(k string, v any) bool {
			if !first {
				b.WriteByte(',')
			}
			first = false
			WriteString(b, k)
			b.WriteByte(':')
			err = write(b, v)
			return err == nil
		})
		if err != nil {
			return err
		}
		b.WriteByte('}')
	case map[string]any:
		return write(b, FromMap(x))
	case Writer:
		if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer && rv.IsNil() {
			b.WriteString("null")
			return nil
		}
		return x.WriteJSON(b)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			return err
		}
		parsed, err := Parse(raw)
		if err != nil {
			return err
		}
		return write(b, parsed)
	}
	return nil
}

// FromMap converts a Go map into an *Object. Go maps have no order, so keys
// are taken in sorted order; callers that care about a client's key order must
// parse with Parse instead.
func FromMap(m map[string]any) *Object {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	o := NewObject()
	for _, k := range keys {
		o.Set(k, m[k])
	}
	return o
}

func writeNumber(b *strings.Builder, f float64) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		b.WriteString("null")
		return
	}
	b.WriteString(FormatNumber(f))
}

// FormatNumber is Number.prototype.toString(10) for a double: the shortest
// round-tripping digits, positional between 1e-7 and 1e21, exponential
// ("1e+21", "1.5e-7") outside, "0" for both zeros, "NaN" and "Infinity".
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
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	// Shortest digits and exponent: d.ddde±x
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expStr, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expStr)
	k := len(digits)
	n := exp + 1 // position of the decimal point relative to the digits
	var out string
	switch {
	case k <= n && n <= 21:
		out = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		out = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		out = "0." + strings.Repeat("0", -n) + digits
	default:
		es := "+"
		if n-1 < 0 {
			es = "-"
		}
		ex := n - 1
		if ex < 0 {
			ex = -ex
		}
		if k == 1 {
			out = digits + "e" + es + strconv.Itoa(ex)
		} else {
			out = digits[:1] + "." + digits[1:] + "e" + es + strconv.Itoa(ex)
		}
	}
	return sign + out
}

const hexDigits = "0123456789abcdef"

// WriteString writes s as JSON.stringify quotes a string: ", \ and the
// control characters escaped (\b \f \n \r \t or \u00xx), everything else
// verbatim — including <, >, & and U+2028/2029, which Go's encoding/json would
// escape.
func WriteString(b *strings.Builder, s string) {
	b.WriteByte('"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		b.WriteString(s[start:i])
		switch c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xF])
		}
		start = i + 1
	}
	b.WriteString(s[start:])
	b.WriteByte('"')
}

// Quote returns s quoted as JSON.stringify(s).
func Quote(s string) string {
	var b strings.Builder
	WriteString(&b, s)
	return b.String()
}

// ── comparisons and helpers ─────────────────────────────────────────────────

// Equal is deep equality as fast-deep-equal (Ajv's const/enum/uniqueItems)
// computes it: numbers by ===, objects by key set regardless of order.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case *Object:
		y, ok := b.(*Object)
		if !ok || x.Len() != y.Len() {
			return false
		}
		for i, k := range x.keys {
			yv, ok := y.Get(k)
			if !ok || !Equal(x.vals[i], yv) {
				return false
			}
		}
		return true
	}
	return false
}

// CompareUTF16 orders strings by UTF-16 code units, the order JavaScript's
// default sort and its < operator use. It differs from Go's byte order for
// characters above U+FFFF versus U+E000–U+FFFF.
func CompareUTF16(a, b string) int {
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

// LessUTF16 is CompareUTF16(a, b) < 0.
func LessUTF16(a, b string) bool { return CompareUTF16(a, b) < 0 }

// SortStrings sorts as Array.prototype.sort() does with no comparator (for
// strings).
func SortStrings(s []string) {
	sort.SliceStable(s, func(i, j int) bool { return LessUTF16(s[i], s[j]) })
}

// UTF16Len is String.prototype.length.
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

// SliceUTF16 is s.slice(start, end) in UTF-16 code units. A cut through a
// surrogate pair yields U+FFFD for the orphaned half, since a Go string cannot
// hold a lone surrogate.
func SliceUTF16(s string, start, end int) string {
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

// TypeName is typeof-style naming for diagnostics.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case *Object:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// Normalize converts common Go shapes (map[string]any, json.Number, ints,
// []string, structs) into the value model. Values already in the model are
// returned as they are (not copied).
func Normalize(v any) (any, error) {
	switch x := v.(type) {
	case nil, bool, float64, string:
		return v, nil
	case *Object:
		if x == nil {
			return nil, nil
		}
		for i := range x.vals {
			n, err := Normalize(x.vals[i])
			if err != nil {
				return nil, err
			}
			x.vals[i] = n
		}
		return x, nil
	case []any:
		for i, e := range x {
			n, err := Normalize(e)
			if err != nil {
				return nil, err
			}
			x[i] = n
		}
		return x, nil
	}
	s, err := Marshal(v)
	if err != nil {
		return nil, err
	}
	return ParseString(s)
}
