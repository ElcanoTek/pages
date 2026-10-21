// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package render

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// escapedJSON is data-limits' escapedJson: JSON.stringify, then every `<`, `>`,
// `&`, U+2028 and U+2029 written as a \u escape, so the text can sit inside a
// <script> element without ending it. A private copy until internal/datalimits
// lands; the two are deduplicated at merge.
//
// v must encode through encoding/json in the key order the JS built it in, i.e.
// a struct (or json.RawMessage), not a map. A value encoding/json refuses is a
// programming error; it renders as null rather than failing the page.
func escapedJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "null"
	}
	return escapeJSONText(strings.TrimSuffix(buf.String(), "\n"))
}

var jsonTextEscaper = strings.NewReplacer(
	"<", `\u003c`,
	">", `\u003e`,
	"&", `\u0026`,
	"\u2028", `\u2028`,
	"\u2029", `\u2029`,
)

func escapeJSONText(s string) string { return jsonTextEscaper.Replace(s) }

// jsonString is JSON.stringify of one string (QuoteJSONString): unlike
// encoding/json it leaves <, >, &, U+2028 and U+2029 alone.
func jsonString(s string) string {
	units := utf16.Encode([]rune(s))
	var b strings.Builder
	writeQuoted(&b, units)
	return b.String()
}

// ── an ECMAScript-faithful JSON value ───────────────────────────────────────
//
// stripCollab re-serialises a deck's document block, and the bytes it writes are
// stored. JSON.parse → JSON.stringify in V8 is not encoding/json: object keys keep
// insertion order except that array-index keys ("0", "17") move to the front in
// numeric order; a duplicate key keeps its first position and its last value;
// numbers are doubles printed by Number#toString; strings are UTF-16 and may hold
// lone surrogates, which stringify writes back as \udXXX escapes. jsValue models
// exactly that.

type jsKind uint8

const (
	jsNull jsKind = iota
	jsBool
	jsNumber
	jsString
	jsArray
	jsObject
)

type jsValue struct {
	kind jsKind
	b    bool
	num  float64
	str  []uint16
	arr  []*jsValue
	obj  *jsObj
}

type jsProp struct {
	key []uint16
	val *jsValue
}

type jsObj struct {
	props []jsProp
	index map[string]int
}

func unitsKey(u []uint16) string {
	b := make([]byte, 0, len(u)*2)
	for _, c := range u {
		b = binary.BigEndian.AppendUint16(b, c)
	}
	return string(b)
}

func (o *jsObj) set(key []uint16, v *jsValue) {
	k := unitsKey(key)
	if i, ok := o.index[k]; ok {
		o.props[i].val = v
		return
	}
	o.index[k] = len(o.props)
	o.props = append(o.props, jsProp{key: key, val: v})
}

func (o *jsObj) has(key string) bool {
	_, ok := o.index[unitsKey(utf16.Encode([]rune(key)))]
	return ok
}

func (o *jsObj) delete(key string) {
	k := unitsKey(utf16.Encode([]rune(key)))
	i, ok := o.index[k]
	if !ok {
		return
	}
	o.props = append(o.props[:i], o.props[i+1:]...)
	o.index = make(map[string]int, len(o.props))
	for j, p := range o.props {
		o.index[unitsKey(p.key)] = j
	}
}

// arrayIndex reports whether key is a canonical array index (0 … 2^32-2) and
// its value: those keys are ordered numerically ahead of all others.
func arrayIndex(key []uint16) (uint64, bool) {
	if len(key) == 0 || len(key) > 10 {
		return 0, false
	}
	if key[0] == '0' && len(key) > 1 {
		return 0, false
	}
	var n uint64
	for _, c := range key {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + uint64(c-'0')
	}
	return n, n < 1<<32-1
}

// ordered returns the properties in [[OwnPropertyKeys]] order.
func (o *jsObj) ordered() []jsProp {
	var idx, rest []jsProp
	for _, p := range o.props {
		if _, ok := arrayIndex(p.key); ok {
			idx = append(idx, p)
		} else {
			rest = append(rest, p)
		}
	}
	if len(idx) == 0 {
		return rest
	}
	// Insertion sort: index keys are few in a deck document.
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0; j-- {
			a, _ := arrayIndex(idx[j-1].key)
			b, _ := arrayIndex(idx[j].key)
			if a <= b {
				break
			}
			idx[j-1], idx[j] = idx[j], idx[j-1]
		}
	}
	return append(idx, rest...)
}

// maxJSONDepth bounds the recursive parser so a hostile block cannot exhaust
// the goroutine stack (V8 would throw RangeError on the stringify instead). A
// deeper document is treated as one that does not parse.
const maxJSONDepth = 10000

var errJSON = errors.New("invalid JSON")

type jsonParser struct {
	s     string
	i     int
	depth int
}

// parseJSValue is JSON.parse.
func parseJSValue(s string) (*jsValue, error) {
	p := &jsonParser{s: s}
	p.ws()
	v, err := p.value()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, errJSON
	}
	return v, nil
}

func (p *jsonParser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *jsonParser) lit(word string) bool {
	if strings.HasPrefix(p.s[p.i:], word) {
		p.i += len(word)
		return true
	}
	return false
}

func (p *jsonParser) value() (*jsValue, error) {
	if p.i >= len(p.s) {
		return nil, errJSON
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		u, err := p.str()
		if err != nil {
			return nil, err
		}
		return &jsValue{kind: jsString, str: u}, nil
	case c == 't' && p.lit("true"):
		return &jsValue{kind: jsBool, b: true}, nil
	case c == 'f' && p.lit("false"):
		return &jsValue{kind: jsBool}, nil
	case c == 'n' && p.lit("null"):
		return &jsValue{kind: jsNull}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	}
	return nil, errJSON
}

func (p *jsonParser) enter() error {
	p.depth++
	if p.depth > maxJSONDepth {
		return errJSON
	}
	return nil
}

func (p *jsonParser) object() (*jsValue, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.i++ // {
	o := &jsObj{index: map[string]int{}}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return &jsValue{kind: jsObject, obj: o}, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, errJSON
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, errJSON
		}
		p.i++
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		o.set(k, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, errJSON
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return &jsValue{kind: jsObject, obj: o}, nil
		default:
			return nil, errJSON
		}
	}
}

func (p *jsonParser) array() (*jsValue, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer func() { p.depth-- }()
	p.i++ // [
	a := &jsValue{kind: jsArray, arr: []*jsValue{}}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return a, nil
	}
	for {
		p.ws()
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		a.arr = append(a.arr, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, errJSON
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return a, nil
		default:
			return nil, errJSON
		}
	}
}

func hexVal(c byte) (uint16, bool) {
	switch {
	case c >= '0' && c <= '9':
		return uint16(c - '0'), true
	case c >= 'a' && c <= 'f':
		return uint16(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return uint16(c-'A') + 10, true
	}
	return 0, false
}

func (p *jsonParser) str() ([]uint16, error) {
	p.i++ // opening quote
	out := []uint16{}
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return out, nil
		case c < 0x20:
			return nil, errJSON
		case c == '\\':
			if p.i+1 >= len(p.s) {
				return nil, errJSON
			}
			e := p.s[p.i+1]
			p.i += 2
			switch e {
			case '"', '\\', '/':
				out = append(out, uint16(e))
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				if p.i+4 > len(p.s) {
					return nil, errJSON
				}
				var u uint16
				for k := 0; k < 4; k++ {
					h, ok := hexVal(p.s[p.i+k])
					if !ok {
						return nil, errJSON
					}
					u = u<<4 | h
				}
				p.i += 4
				out = append(out, u)
			default:
				return nil, errJSON
			}
		case c < utf8.RuneSelf:
			out = append(out, uint16(c))
			p.i++
		default:
			r, size := utf8.DecodeRuneInString(p.s[p.i:])
			p.i += size
			out = utf16.AppendRune(out, r)
		}
	}
	return nil, errJSON
}

func (p *jsonParser) number() (*jsValue, error) {
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
	if p.i >= len(p.s) {
		return nil, errJSON
	}
	if p.s[p.i] == '0' {
		p.i++
	} else if digits() == 0 {
		return nil, errJSON
	}
	if p.i < len(p.s) && p.s[p.i] == '.' {
		p.i++
		if digits() == 0 {
			return nil, errJSON
		}
	}
	if p.i < len(p.s) && (p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		p.i++
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			p.i++
		}
		if digits() == 0 {
			return nil, errJSON
		}
	}
	f, err := strconv.ParseFloat(p.s[start:p.i], 64)
	if err != nil {
		var ne *strconv.NumError
		if !errors.As(err, &ne) || !errors.Is(ne.Err, strconv.ErrRange) {
			return nil, errJSON
		}
		// Out of range: ParseFloat returns ±Inf or ±0, as JS does.
	}
	return &jsValue{kind: jsNumber, num: f}, nil
}

// jsNumberString is Number.prototype.toString() for a finite double.
func jsNumberString(f float64) string {
	if f == 0 {
		return "0"
	}
	neg := f < 0
	if neg {
		f = -f
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddddde±XX
	mant, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	k := len(digits)
	n := exp + 1
	var s string
	switch {
	case k <= n && n <= 21:
		s = digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		s = digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		s = "0." + strings.Repeat("0", -n) + digits
	default:
		sign := "+"
		if n-1 < 0 {
			sign = "-"
		}
		s = digits[:1]
		if k > 1 {
			s += "." + digits[1:]
		}
		x := n - 1
		if x < 0 {
			x = -x
		}
		s += "e" + sign + strconv.Itoa(x)
	}
	if neg {
		return "-" + s
	}
	return s
}

const hexDigits = "0123456789abcdef"

func writeQuoted(b *strings.Builder, units []uint16) {
	b.WriteByte('"')
	for i := 0; i < len(units); i++ {
		c := units[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		case c >= 0xd800 && c <= 0xdbff && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] <= 0xdfff:
			b.WriteRune(utf16.DecodeRune(rune(c), rune(units[i+1])))
			i++
		case c >= 0xd800 && c <= 0xdfff:
			b.WriteString(`\u`)
			for shift := 12; shift >= 0; shift -= 4 {
				b.WriteByte(hexDigits[(c>>uint(shift))&0xf])
			}
		default:
			b.WriteRune(rune(c))
		}
	}
	b.WriteByte('"')
}

// stringify is JSON.stringify for a parsed value.
func (v *jsValue) stringify(b *strings.Builder) {
	switch v.kind {
	case jsNull:
		b.WriteString("null")
	case jsBool:
		if v.b {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case jsNumber:
		if math.IsInf(v.num, 0) || math.IsNaN(v.num) {
			b.WriteString("null")
		} else {
			b.WriteString(jsNumberString(v.num))
		}
	case jsString:
		writeQuoted(b, v.str)
	case jsArray:
		b.WriteByte('[')
		for i, el := range v.arr {
			if i > 0 {
				b.WriteByte(',')
			}
			el.stringify(b)
		}
		b.WriteByte(']')
	case jsObject:
		b.WriteByte('{')
		for i, p := range v.obj.ordered() {
			if i > 0 {
				b.WriteByte(',')
			}
			writeQuoted(b, p.key)
			b.WriteByte(':')
			p.val.stringify(b)
		}
		b.WriteByte('}')
	}
}

// toAny converts to the encoding/json shapes (map[string]any, []any, float64,
// string, bool, nil) for callers that only read the document.
func (v *jsValue) toAny() any {
	switch v.kind {
	case jsBool:
		return v.b
	case jsNumber:
		return v.num
	case jsString:
		return string(utf16.Decode(v.str))
	case jsArray:
		out := make([]any, len(v.arr))
		for i, el := range v.arr {
			out[i] = el.toAny()
		}
		return out
	case jsObject:
		out := make(map[string]any, len(v.obj.props))
		for _, p := range v.obj.props {
			out[string(utf16.Decode(p.key))] = p.val.toAny()
		}
		return out
	}
	return nil
}
