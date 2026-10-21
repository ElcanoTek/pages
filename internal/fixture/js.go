// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"fmt"
	"math"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// The fixture state is held as JavaScript values, not Go structs, on purpose.
// It stands in for the admin API, and the browser specs read back exactly what
// they sent: a theme the screen posts is stored verbatim, a request body is
// echoed into /__fixture/events with its keys in the order they arrived, and a
// field set to `undefined` drops out of the payload while keeping its place.
// Typed structs would have to re-invent each of those rules field by field.
//
// Values are what jsonv.Parse produces (nil, bool, float64, string, []any,
// *jsonv.Object) plus two fixture-only kinds: *list, a JavaScript array held
// by reference (so a response built before a delay still shows a later push,
// exactly as res.json(body) did after setTimeout), and undef, a property whose
// value is `undefined`.

type obj = *jsonv.Object

// undefined is JavaScript's undefined: a missing property, or one assigned
// undefined, which JSON.stringify leaves out of an object.
type undefined struct{}

var undef any = undefined{}

// list is a JavaScript array shared by reference.
type list struct{ items []any }

func newList(items ...any) *list { return &list{items: items} }

func (l *list) push(v any)    { l.items = append(l.items, v) }
func (l *list) unshift(v any) { l.items = append([]any{v}, l.items...) }
func (l *list) len() int      { return len(l.items) }
func (l *list) at(i int) obj  { return asObj(l.items[i]) }
func (l *list) filter(keep func(obj) bool) *list {
	out := newList()
	for _, item := range l.items {
		if keep(asObj(item)) {
			out.push(item)
		}
	}
	return out
}

// find is Array#find over a list of objects (nil when nothing matches).
func (l *list) find(match func(obj) bool) obj {
	for _, item := range l.items {
		if o := asObj(item); match(o) {
			return o
		}
	}
	return nil
}

func (l *list) findIndex(match func(obj) bool) int {
	for i, item := range l.items {
		if match(asObj(item)) {
			return i
		}
	}
	return -1
}

func (l *list) splice(i int) any {
	v := l.items[i]
	l.items = append(l.items[:i:i], l.items[i+1:]...)
	return v
}

// o builds an object from key/value pairs in insertion order. Go integer
// literals become float64, the only JavaScript number.
func o(kv ...any) obj {
	out := jsonv.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		out.Set(kv[i].(string), norm(kv[i+1]))
	}
	return out
}

func norm(v any) any {
	switch x := v.(type) {
	case int:
		return float64(x)
	case int64:
		return float64(x)
	}
	return v
}

func asObj(v any) obj {
	if x, ok := v.(*jsonv.Object); ok {
		return x
	}
	return nil
}

// get is `value.key`: undef for a missing key or a value with no properties.
func get(v any, key string) any {
	x := asObj(v)
	if x == nil {
		return undef
	}
	if val, ok := x.Get(key); ok {
		return val
	}
	return undef
}

func has(v any, key string) bool {
	x := asObj(v)
	return x != nil && x.Has(key)
}

func set(x obj, key string, v any) { x.Set(key, norm(v)) }

// spread is `{ ...src, k1: v1, ... }`.
func spread(src obj, kv ...any) obj {
	out := jsonv.NewObject()
	for _, k := range src.Keys() {
		v, _ := src.Get(k)
		out.Set(k, v)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out.Set(kv[i].(string), norm(kv[i+1]))
	}
	return out
}

// without is `({ key, ...rest }) => rest`.
func without(src obj, drop string) obj {
	out := jsonv.NewObject()
	for _, k := range src.Keys() {
		if k == drop {
			continue
		}
		v, _ := src.Get(k)
		out.Set(k, v)
	}
	return out
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil, undefined:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// or is `a || b`.
func or(a, b any) any {
	if truthy(a) {
		return a
	}
	return norm(b)
}

// str is String(v).
func str(v any) string {
	switch x := v.(type) {
	case undefined:
		return "undefined"
	case *list:
		return jsonv.ToString(x.items)
	}
	return jsonv.ToString(v)
}

// num is Number(v).
func num(v any) float64 {
	switch x := v.(type) {
	case undefined:
		return math.NaN()
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		return jsstr.Number(x)
	case []any:
		return jsstr.Number(jsonv.ToString(x))
	}
	return math.NaN()
}

// numOr0 is `Number(v) || 0`.
func numOr0(v any) float64 {
	n := num(v)
	if math.IsNaN(n) {
		return 0
	}
	return n
}

// sameString is `String(a) === String(b)`.
func sameString(a, b any) bool { return str(a) == str(b) }

// clone is JSON.parse(JSON.stringify(v)) for fixture values.
func clone(v any) any {
	switch x := v.(type) {
	case *jsonv.Object:
		out := jsonv.NewObject()
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			if _, skip := val.(undefined); skip {
				continue
			}
			out.Set(k, clone(val))
		}
		return out
	case *list:
		out := newList()
		for _, item := range x.items {
			out.push(clone(item))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = clone(item)
		}
		return out
	}
	return v
}

// marshal is JSON.stringify for fixture values.
func marshal(v any) []byte {
	b, err := appendJS(nil, v)
	if err != nil {
		panic(err) // every value the fixture holds is serialisable
	}
	return b
}

func appendJS(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case undefined:
		return append(b, "null"...), nil // only reachable inside an array
	case *list:
		return appendJS(b, x.items)
	case []any:
		b = append(b, '[')
		for i, item := range x {
			if i > 0 {
				b = append(b, ',')
			}
			var err error
			if b, err = appendJS(b, item); err != nil {
				return nil, err
			}
		}
		return append(b, ']'), nil
	case *jsonv.Object:
		b = append(b, '{')
		first := true
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			if _, skip := val.(undefined); skip {
				continue
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = jsonv.AppendString(b, k)
			b = append(b, ':')
			var err error
			if b, err = appendJS(b, val); err != nil {
				return nil, err
			}
		}
		return append(b, '}'), nil
	case nil, bool, float64, int, int64, string:
		return jsonv.AppendValue(b, x)
	}
	return nil, fmt.Errorf("fixture: cannot serialise %T", v)
}

// replaceFirst is String#replace with a string pattern: the first occurrence.
func replaceFirst(s, old, repl string) string { return strings.Replace(s, old, repl, 1) }
