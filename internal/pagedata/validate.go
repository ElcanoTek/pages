// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"math"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// ── validation ───────────────────────────────────────────────────────────────
// The interpreter walks the compiled schema the way Ajv's generated code does.
// Instance and schema paths are kept as segment stacks and rendered only when
// an error is reported, so a valid megabyte payload builds no path strings.

type evaluated struct {
	allProps bool
	props    map[string]bool
	allItems bool
	items    int
}

func (e *evaluated) merge(o evaluated) {
	if o.allProps {
		e.allProps = true
	} else if !e.allProps {
		for k := range o.props {
			if e.props == nil {
				e.props = map[string]bool{}
			}
			e.props[k] = true
		}
	}
	if o.allItems {
		e.allItems = true
	} else if o.items > e.items {
		e.items = o.items
	}
}

func (e *evaluated) addProp(k string) {
	if e.allProps {
		return
	}
	if e.props == nil {
		e.props = map[string]bool{}
	}
	e.props[k] = true
}

// seg is one path segment: a raw key (escaped when rendered), an index, or
// literal text.
type seg struct {
	s    string
	n    int
	kind byte // 'k' key, 'i' index, 't' text
}

type vstate struct {
	v    *Validator
	errs []ValidationError
	ip   []seg // instance path
	sp   []seg // schema path; sp[0] is the root text ("#" or a $ref)
	// track records whether unevaluated* keywords exist anywhere; without
	// them, evaluated-property bookkeeping is skipped.
	track bool
	// depth is the current schema-application nesting; see maxValidateDepth.
	depth   int
	tooDeep bool
}

// maxValidateDepth bounds how deeply schema applications may nest during one
// validation. Compilation already refuses reference cycles that do not
// descend into the data (checkRefCycles), and data nesting is bounded by the
// parser, so a legitimate validation stays far below this; it is the backstop
// that turns any recursion the compile-time check missed into a refusal
// instead of a fatal, unrecoverable goroutine stack overflow.
const maxValidateDepth = 100000

// Validate validates data and returns Ajv's error list (nil when valid).
func (v *Validator) Validate(data any) []ValidationError {
	s := &vstate{v: v, sp: []seg{{s: "#", kind: 't'}}, track: v.unevaluated}
	s.node(v.root, data)
	if s.tooDeep {
		// Reported alone and after the walk: applicators such as "not" and
		// "anyOf" discard their branches' errors, and must not turn an
		// abandoned validation into a pass.
		return []ValidationError{{InstancePath: "", SchemaPath: "#", Keyword: "$ref",
			Params: jsonv.NewObject(), Message: "schema recursion is too deep"}}
	}
	return s.errs
}

// Valid reports whether data satisfies the schema.
func (v *Validator) Valid(data any) bool { return len(v.Validate(data)) == 0 }

func escapePointer(s string) string {
	if !strings.ContainsAny(s, "~/") {
		return s
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// escapeFragment is encodeURIComponent(escapeJsonPointer(s)).
func escapeFragment(s string) string { return encodeURIComponent(escapePointer(s)) }

func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func (s *vstate) instancePath() string {
	var b strings.Builder
	for _, g := range s.ip {
		b.WriteByte('/')
		if g.kind == 'i' {
			b.WriteString(strconv.Itoa(g.n))
		} else {
			b.WriteString(escapePointer(g.s))
		}
	}
	return b.String()
}

func (s *vstate) schemaPath(kw string) string {
	var b strings.Builder
	for i, g := range s.sp {
		if i > 0 {
			b.WriteByte('/')
		}
		switch g.kind {
		case 'i':
			b.WriteString(strconv.Itoa(g.n))
		case 'k':
			b.WriteString(escapeFragment(g.s))
		default:
			b.WriteString(g.s)
		}
	}
	if kw != "" {
		b.WriteByte('/')
		b.WriteString(kw)
	}
	return b.String()
}

// add reports an error of keyword kw at the current paths (the schema path
// is the current one plus pathKw, which is kw unless overridden).
func (s *vstate) add(pathKw, kw string, params any, msg string) {
	s.errs = append(s.errs, ValidationError{InstancePath: s.instancePath(), SchemaPath: s.schemaPath(pathKw), Keyword: kw, Params: params, Message: msg})
}

func (s *vstate) pushI(g seg) { s.ip = append(s.ip, g) }
func (s *vstate) popI()       { s.ip = s.ip[:len(s.ip)-1] }

// sub validates a subschema at schema-path segments segs (same data).
func (s *vstate) sub(sch any, data any, segs ...seg) evaluated {
	s.sp = append(s.sp, segs...)
	ev := s.node(sch, data)
	s.sp = s.sp[:len(s.sp)-len(segs)]
	return ev
}

// subData validates a subschema against a child of the data.
func (s *vstate) subData(sch any, data any, at seg, segs ...seg) evaluated {
	s.pushI(at)
	ev := s.sub(sch, data, segs...)
	s.popI()
	return ev
}

func dataIs(t string, d any) bool {
	switch t {
	case "number":
		f, ok := d.(float64)
		return ok && !math.IsInf(f, 0) && !math.IsNaN(f)
	case "integer":
		f, ok := d.(float64)
		return ok && f == math.Trunc(f) && !math.IsInf(f, 0)
	case "string":
		_, ok := d.(string)
		return ok
	case "boolean":
		_, ok := d.(bool)
		return ok
	case "null":
		return d == nil
	case "array":
		_, ok := d.([]any)
		return ok
	case "object":
		_, ok := d.(*jsonv.Object)
		return ok
	}
	return false
}

func num(f float64) string { return jsonv.FormatNumber(f) }

func kseg(k string) seg { return seg{s: k, kind: 'k'} }
func iseg(i int) seg    { return seg{n: i, kind: 'i'} }
func tseg(t string) seg { return seg{s: t, kind: 't'} }

// av is alwaysValid at validation time: every schema the compiler did not
// skip as always valid has node info with rules.
func (s *vstate) av(sch any) bool {
	switch x := sch.(type) {
	case bool:
		return x
	case *jsonv.Object:
		ni := s.v.info[x]
		return ni == nil || !ni.hasRules
	}
	return true
}

func (s *vstate) typeError(ni *nodeInfo) {
	s.add("type", "type", jsonv.ObjectOf("type", ni.typeParam), "must be "+ni.typeMsg)
}

func (s *vstate) node(sch any, data any) evaluated {
	var ev evaluated
	if s.tooDeep {
		return ev
	}
	s.depth++
	defer func() { s.depth-- }()
	if s.depth > maxValidateDepth {
		s.tooDeep = true
		return ev
	}
	o, ok := sch.(*jsonv.Object)
	if !ok {
		if sch == false {
			s.add("false schema", "false schema", jsonv.NewObject(), "boolean schema is false")
		}
		return ev
	}
	ni := s.v.info[o]
	if ni == nil || !ni.hasRules {
		return ev
	}
	if ni.refOnly {
		return s.ref(o, ni.base, data)
	}
	if ni.check {
		matched := false
		for _, t := range ni.types {
			if dataIs(t, data) {
				matched = true
			}
		}
		if !matched {
			s.typeError(ni)
		}
	}
	for g := range groupRules {
		if !ni.groups[g] {
			continue
		}
		if t := groupType[g]; t != "" && !dataIs(t, data) {
			if len(ni.types) == 1 && ni.types[0] == t && !ni.check {
				s.typeError(ni)
			}
			continue
		}
		for _, kw := range ni.keywords[g] {
			s.keyword(o, ni, kw, groupType[g], data, &ev)
		}
	}
	return ev
}

func (s *vstate) ref(o *jsonv.Object, base string, data any) evaluated {
	ref, _ := o.Value("$ref").(string)
	t := s.v.refs[refKey(base, ref)]
	if t == nil {
		return evaluated{}
	}
	return s.call(t, ref, data)
}

// call validates against a reference target: inlined (schema path continues
// from the $ref text, as Ajv's inlined code reports it) or separately
// compiled (schema path restarts at "#").
func (s *vstate) call(t *refTarget, errPath string, data any) evaluated {
	saved := s.sp
	if t.inline {
		s.sp = []seg{tseg(errPath)}
		ev := s.node(t.node, data)
		s.sp = saved
		return ev
	}
	s.sp = []seg{tseg("#")}
	before := len(s.errs)
	ev := s.node(t.node, data)
	s.sp = saved
	if len(s.errs) != before {
		return evaluated{}
	}
	return ev
}

func (s *vstate) keyword(o *jsonv.Object, ni *nodeInfo, kw, group string, data any, ev *evaluated) {
	val := o.Value(kw)
	switch kw {
	case "$ref":
		ev.merge(s.ref(o, ni.base, data))
	case "$recursiveRef", "$dynamicRef":
		ref, _ := val.(string)
		if t := s.v.refs[refKey(ni.base, "dyn:"+ref)]; t != nil {
			ev.merge(s.call(t, "#", data))
		}
	case "const":
		if !jsonv.Equal(data, val) {
			s.add(kw, kw, jsonv.ObjectOf("allowedValue", val), "must be equal to constant")
		}
	case "enum":
		for _, e := range val.([]any) {
			if jsonv.Equal(data, e) {
				return
			}
		}
		s.add(kw, kw, jsonv.ObjectOf("allowedValues", val), "must be equal to one of the allowed values")
	case "not":
		if s.av(val) {
			s.add(kw, kw, jsonv.NewObject(), "must NOT be valid")
			return
		}
		before := len(s.errs)
		s.sub(val, data, tseg(kw))
		valid := len(s.errs) == before
		s.errs = s.errs[:before]
		if valid {
			s.add(kw, kw, jsonv.NewObject(), "must NOT be valid")
		}
	case "anyOf":
		before := len(s.errs)
		valid := false
		for i, sub := range val.([]any) {
			b := len(s.errs)
			e := s.sub(sub, data, tseg(kw), iseg(i))
			if len(s.errs) == b {
				valid = true
				ev.merge(e)
			}
		}
		if valid {
			s.errs = s.errs[:before]
		} else {
			s.add(kw, kw, jsonv.NewObject(), "must match a schema in anyOf")
		}
	case "oneOf":
		before := len(s.errs)
		var passing []int
		var passEv evaluated
		for i, sub := range val.([]any) {
			if s.av(sub) {
				passing = append(passing, i)
				continue
			}
			b := len(s.errs)
			e := s.sub(sub, data, tseg(kw), iseg(i))
			if len(s.errs) == b {
				passing = append(passing, i)
				passEv.merge(e)
			}
		}
		if len(passing) == 1 {
			s.errs = s.errs[:before]
			ev.merge(passEv)
			return
		}
		var p any
		if len(passing) >= 2 {
			p = []any{float64(passing[0]), float64(passing[1])}
		}
		s.add(kw, kw, jsonv.ObjectOf("passingSchemas", p), "must match exactly one schema in oneOf")
	case "allOf":
		for i, sub := range val.([]any) {
			if !s.av(sub) {
				ev.merge(s.sub(sub, data, tseg(kw), iseg(i)))
			}
		}
	case "if":
		thenS, hasThen := o.Get("then")
		elseS, hasElse := o.Get("else")
		hasThen = hasThen && !s.av(thenS)
		hasElse = hasElse && !s.av(elseS)
		if !hasThen && !hasElse {
			return
		}
		before := len(s.errs)
		e := s.sub(val, data, tseg(kw))
		ifValid := len(s.errs) == before
		s.errs = s.errs[:before]
		ev.merge(e)
		clause, cs := "", any(nil)
		switch {
		case ifValid && hasThen:
			clause, cs = "then", thenS
		case !ifValid && hasElse:
			clause, cs = "else", elseS
		default:
			return
		}
		b := len(s.errs)
		ce := s.sub(cs, data, tseg(clause))
		if len(s.errs) == b {
			ev.merge(ce)
			return
		}
		s.add(kw, kw, jsonv.ObjectOf("failingKeyword", clause), `must match "`+clause+`" schema`)

	// number
	case "maximum", "minimum", "exclusiveMaximum", "exclusiveMinimum":
		d := data.(float64)
		lim, _ := numberValue(val)
		var fail bool
		var op string
		switch kw {
		case "maximum":
			fail, op = d > lim, "<="
		case "minimum":
			fail, op = d < lim, ">="
		case "exclusiveMaximum":
			fail, op = d >= lim, "<"
		default:
			fail, op = d <= lim, ">"
		}
		if fail {
			s.add(kw, kw, jsonv.ObjectOf("comparison", op, "limit", val), "must be "+op+" "+num(lim))
		}
	case "multipleOf":
		d := data.(float64)
		m, _ := numberValue(val)
		if m == 0 || !jsIntegral(d/m) {
			s.add(kw, kw, jsonv.ObjectOf("multipleOf", val), "must be multiple of "+num(m))
		}
	case "format":
		name, _ := val.(string)
		f := formats[name]
		if f == nil || f.validate == nil || f.kind != group {
			return
		}
		if !f.validate(data) {
			s.add(kw, kw, jsonv.ObjectOf("format", name), `must match format "`+name+`"`)
		}

	// string
	case "maxLength", "minLength":
		lim, _ := numberValue(val)
		n := float64(countRunes(data.(string), lim))
		if (kw == "maxLength" && n > lim) || (kw == "minLength" && n < lim) {
			comp := "more"
			if kw == "minLength" {
				comp = "fewer"
			}
			s.add(kw, kw, jsonv.ObjectOf("limit", val), "must NOT have "+comp+" than "+num(lim)+" characters")
		}
	case "pattern":
		p, _ := val.(string)
		if re := s.v.patterns[p]; re != nil && !re.MatchString(data.(string)) {
			s.add(kw, kw, jsonv.ObjectOf("pattern", p), `must match pattern "`+p+`"`)
		}
	case "formatMaximum", "formatMinimum", "formatExclusiveMaximum", "formatExclusiveMinimum":
		name, _ := o.Value("format").(string)
		f := formats[name]
		limit, _ := val.(string)
		if f == nil || f.compare == nil {
			return
		}
		cmp, ok := f.compare(data.(string), limit)
		if !ok {
			return
		}
		var fail bool
		var op string
		switch kw {
		case "formatMaximum":
			fail, op = cmp > 0, "<="
		case "formatMinimum":
			fail, op = cmp < 0, ">="
		case "formatExclusiveMaximum":
			fail, op = cmp >= 0, "<"
		default:
			fail, op = cmp <= 0, ">"
		}
		if fail {
			s.add(kw, kw, jsonv.ObjectOf("comparison", op, "limit", limit), "should be "+op+" "+limit)
		}

	// array
	case "maxItems", "minItems":
		n := float64(len(data.([]any)))
		lim, _ := numberValue(val)
		if (kw == "maxItems" && n > lim) || (kw == "minItems" && n < lim) {
			comp := "more"
			if kw == "minItems" {
				comp = "fewer"
			}
			s.add(kw, kw, jsonv.ObjectOf("limit", val), "must NOT have "+comp+" than "+num(lim)+" items")
		}
	case "prefixItems":
		arr := data.([]any)
		prefix := val.([]any)
		for i, sub := range prefix {
			if i >= len(arr) {
				break
			}
			if !s.av(sub) {
				s.subData(sub, arr[i], iseg(i), tseg(kw), iseg(i))
			}
		}
		if len(prefix) > ev.items {
			ev.items = len(prefix)
		}
	case "items":
		arr := data.([]any)
		ev.allItems = true
		if s.av(val) {
			return
		}
		start := 0
		if p, ok := o.Value("prefixItems").([]any); ok {
			start = len(p)
			if val == false {
				if len(arr) > start {
					s.add(kw, kw, jsonv.ObjectOf("limit", float64(start)), "must NOT have more than "+strconv.Itoa(start)+" items")
				}
				return
			}
		}
		for i := start; i < len(arr); i++ {
			s.subData(val, arr[i], iseg(i), tseg(kw))
		}
	case "contains":
		s.contains(o, val, data.([]any))
		ev.allItems = true
	case "uniqueItems":
		if val == true {
			s.uniqueItems(o, data.([]any))
		}
	case "unevaluatedItems":
		arr := data.([]any)
		if ev.allItems {
			return
		}
		items := ev.items
		if val == false {
			if len(arr) > items {
				s.add(kw, kw, jsonv.ObjectOf("limit", float64(items)), "must NOT have more than "+strconv.Itoa(items)+" items")
			}
		} else if !s.av(val) {
			for i := items; i < len(arr); i++ {
				s.subData(val, arr[i], iseg(i), tseg(kw))
			}
		}
		ev.allItems = true

	// object
	case "maxProperties", "minProperties":
		n := float64(data.(*jsonv.Object).Len())
		lim, _ := numberValue(val)
		if (kw == "maxProperties" && n > lim) || (kw == "minProperties" && n < lim) {
			comp := "more"
			if kw == "minProperties" {
				comp = "fewer"
			}
			s.add(kw, kw, jsonv.ObjectOf("limit", val), "must NOT have "+comp+" than "+num(lim)+" properties")
		}
	case "required":
		obj := data.(*jsonv.Object)
		for _, r := range val.([]any) {
			name, _ := r.(string)
			if !obj.Has(name) {
				s.add(kw, kw, jsonv.ObjectOf("missingProperty", name), "must have required property '"+name+"'")
			}
		}
	case "propertyNames":
		if s.av(val) {
			return
		}
		data.(*jsonv.Object).Range(func(k string, _ any) bool {
			b := len(s.errs)
			s.sub(val, k, tseg(kw))
			if len(s.errs) != b {
				s.add(kw, kw, jsonv.ObjectOf("propertyName", k), "property name must be valid")
			}
			return true
		})
	case "additionalProperties":
		ev.allProps = true
		if s.av(val) {
			return
		}
		data.(*jsonv.Object).Range(func(k string, v any) bool {
			if s.definedProp(ni, k) {
				return true
			}
			if val == false {
				s.add(kw, kw, jsonv.ObjectOf("additionalProperty", k), "must NOT have additional properties")
				return true
			}
			s.subData(val, v, kseg(k), tseg(kw))
			return true
		})
	case "dependencies", "dependentRequired":
		obj := data.(*jsonv.Object)
		deps := asObject(val)
		deps.Range(func(k string, d any) bool {
			list, isArr := d.([]any)
			if !isArr || len(list) == 0 || !obj.Has(k) {
				return true
			}
			names := make([]string, len(list))
			for i, d := range list {
				names[i], _ = d.(string)
			}
			word := "properties"
			if len(list) == 1 {
				word = "property"
			}
			for _, d := range names {
				if !obj.Has(d) {
					s.add(kw, kw, jsonv.ObjectOf("property", k, "missingProperty", d, "depsCount", float64(len(list)), "deps", strings.Join(names, ", ")),
						"must have "+word+" "+strings.Join(names, ", ")+" when property "+k+" is present")
				}
			}
			return true
		})
		if kw == "dependencies" {
			s.schemaDeps(kw, deps, true, data, ev)
		}
	case "dependentSchemas":
		s.schemaDeps(kw, asObject(val), false, data, ev)
	case "properties":
		obj := data.(*jsonv.Object)
		props := asObject(val)
		for _, k := range ni.propList {
			if s.track {
				ev.addProp(k)
			}
			ps := props.Value(k)
			if s.av(ps) {
				continue
			}
			if v, ok := obj.Get(k); ok {
				s.subData(ps, v, kseg(k), tseg(kw), kseg(k))
			}
		}
	case "patternProperties":
		obj := data.(*jsonv.Object)
		pats := asObject(val)
		for _, p := range ni.patternList {
			re := s.v.patterns[p]
			if re == nil {
				continue
			}
			ps := pats.Value(p)
			trivial := s.av(ps)
			obj.Range(func(k string, v any) bool {
				if !re.MatchString(k) {
					return true
				}
				if !trivial {
					s.subData(ps, v, kseg(k), tseg(kw), kseg(p))
				}
				if s.track {
					ev.addProp(k)
				}
				return true
			})
		}
	case "unevaluatedProperties":
		if ev.allProps {
			return
		}
		data.(*jsonv.Object).Range(func(k string, v any) bool {
			if ev.props[k] {
				return true
			}
			if val == false {
				s.add(kw, kw, jsonv.ObjectOf("unevaluatedProperty", k), "must NOT have unevaluated properties")
				return true
			}
			if !s.av(val) {
				s.subData(val, v, kseg(k), tseg(kw))
			}
			return true
		})
		ev.allProps = true
	}
}

// countRunes counts code points (Ajv's ucs2length), stopping once the count
// passes limit+1 — enough to decide either length keyword.
func countRunes(s string, limit float64) int {
	n := 0
	for range s {
		n++
		if float64(n) > limit+1 {
			break
		}
	}
	return n
}

func (s *vstate) definedProp(ni *nodeInfo, k string) bool {
	if ni.propSet[k] {
		return true
	}
	for _, p := range ni.additionalPatterns {
		if re := s.v.patterns[p]; re != nil && re.MatchString(k) {
			return true
		}
	}
	return false
}

func (s *vstate) schemaDeps(kw string, deps *jsonv.Object, skipArrays bool, data any, ev *evaluated) {
	obj := data.(*jsonv.Object)
	deps.Range(func(k string, d any) bool {
		if _, isArr := d.([]any); isArr && skipArrays {
			return true
		}
		if k == "__proto__" || s.av(d) || !obj.Has(k) {
			return true
		}
		b := len(s.errs)
		e := s.sub(d, data, tseg(kw), kseg(k))
		if len(s.errs) == b {
			ev.merge(e)
		}
		return true
	})
}

func (s *vstate) contains(o *jsonv.Object, sch any, arr []any) {
	minC, hasMin := numberValue(o.Value("minContains"))
	maxC, hasMax := numberValue(o.Value("maxContains"))
	if !hasMin {
		minC = 1
	}
	report := func() {
		params := jsonv.ObjectOf("minContains", minC)
		msg := "must contain at least " + num(minC) + " valid item(s)"
		if hasMax {
			params.Set("maxContains", maxC)
			msg = "must contain at least " + num(minC) + " and no more than " + num(maxC) + " valid item(s)"
		}
		s.add("contains", "contains", params, msg)
	}
	if s.av(sch) {
		n := float64(len(arr))
		if n < minC || (hasMax && n > maxC) {
			report()
		}
		return
	}
	before := len(s.errs)
	valid := false
	item := func(i int) bool {
		b := len(s.errs)
		s.subData(sch, arr[i], iseg(i), tseg("contains"))
		return len(s.errs) == b
	}
	switch {
	case !hasMax && minC == 1:
		for i := range arr {
			if item(i) {
				valid = true
				break
			}
		}
	case minC == 0:
		valid = true
		count := 0.0
		for i := range arr {
			if !item(i) {
				continue
			}
			count++
			if count > maxC {
				valid = false
				break
			}
		}
	default:
		count := 0.0
		for i := range arr {
			if !item(i) {
				continue
			}
			count++
			if !hasMax {
				if count >= minC {
					valid = true
					break
				}
				continue
			}
			if count > maxC {
				valid = false
				break
			}
			if minC == 1 || count >= minC {
				valid = true
			}
		}
	}
	if valid {
		s.errs = s.errs[:before]
		return
	}
	report()
}

func (s *vstate) uniqueItems(o *jsonv.Object, arr []any) {
	if len(arr) <= 1 {
		return
	}
	var itemTypes []string
	if it, ok := o.Value("items").(*jsonv.Object); ok {
		itemTypes, _ = schemaTypes(it)
	}
	optimize := len(itemTypes) > 0 && !contains(itemTypes, "object") && !contains(itemTypes, "array")
	report := func(i, j int) {
		s.add("uniqueItems", "uniqueItems", jsonv.ObjectOf("i", float64(i), "j", float64(j)),
			fmt.Sprintf("must NOT have duplicate items (items ## %d and %d are identical)", j, i))
	}
	if optimize {
		indices := map[string]int{}
		for i := len(arr) - 1; i >= 0; i-- {
			item := arr[i]
			matched := false
			for _, t := range itemTypes {
				if dataIs(t, item) {
					matched = true
				}
			}
			if !matched {
				continue
			}
			key := jsPropertyKey(item)
			if len(itemTypes) > 1 {
				if str, ok := item.(string); ok {
					key = str + "_"
				}
			}
			if j, seen := indices[key]; seen {
				report(i, j)
				return
			}
			indices[key] = i
		}
		return
	}
	// Ajv's general case compares every pair, from the end: the reported
	// pair is the largest i with an equal earlier item, and the largest such
	// j. That is quadratic in the array length (and the managed write that
	// runs it holds a database connection), so find the same pair in one
	// pass instead: hash each item by value (jsonv.Equal semantics, object
	// key order ignored) and confirm candidates with jsonv.Equal. The hash is
	// seeded per process, so collisions cannot be chosen by a client.
	buckets := make(map[uint64][]int, len(arr))
	bi, bj := -1, -1
	for i, item := range arr {
		h := equalHash(item)
		for k := len(buckets[h]) - 1; k >= 0; k-- {
			if j := buckets[h][k]; jsonv.Equal(item, arr[j]) {
				bi, bj = i, j
				break
			}
		}
		buckets[h] = append(buckets[h], i)
	}
	if bi >= 0 {
		report(bi, bj)
	}
}

var equalSeed = maphash.MakeSeed()

// equalHash hashes a parsed JSON value so that jsonv.Equal values hash alike:
// numbers by value (0 and -0 alike), objects independently of key order.
func equalHash(v any) uint64 {
	var h maphash.Hash
	h.SetSeed(equalSeed)
	writeEqualHash(&h, v)
	return h.Sum64()
}

// writeLen writes a length (decimal and terminated, so no two sequences of
// lengths and contents run together).
func writeLen(h *maphash.Hash, n int) {
	var buf [24]byte
	_, _ = h.Write(strconv.AppendInt(buf[:0], int64(n), 10))
	_ = h.WriteByte(':')
}

func writeEqualHash(h *maphash.Hash, v any) {
	var buf [8]byte
	switch x := v.(type) {
	case nil:
		_ = h.WriteByte('n')
	case bool:
		if x {
			_ = h.WriteByte('t')
		} else {
			_ = h.WriteByte('f')
		}
	case float64:
		if x == 0 {
			x = 0 // -0 == 0
		}
		_ = h.WriteByte('d')
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(x))
		_, _ = h.Write(buf[:])
	case string:
		_ = h.WriteByte('s')
		writeLen(h, len(x))
		_, _ = h.WriteString(x)
	case []any:
		_ = h.WriteByte('a')
		writeLen(h, len(x))
		for _, e := range x {
			writeEqualHash(h, e)
		}
	case *jsonv.Object:
		// Sum the entries' own hashes: commutative, so key order is ignored.
		var sum uint64
		for _, k := range x.Keys() {
			var eh maphash.Hash
			eh.SetSeed(equalSeed)
			writeLen(&eh, len(k))
			_, _ = eh.WriteString(k)
			writeEqualHash(&eh, x.Value(k))
			sum += eh.Sum64()
		}
		_ = h.WriteByte('o')
		writeLen(h, x.Len())
		binary.LittleEndian.PutUint64(buf[:], sum)
		_, _ = h.Write(buf[:])
	}
}

// jsPropertyKey is String(value) for a scalar used as an object key.
func jsPropertyKey(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return num(x)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// jsIntegral is `res === parseInt(res)` for a double: parseInt reads the
// leading digits of String(res), so values printed in exponent form (1e21,
// 1e-7) are never integral.
func jsIntegral(f float64) bool {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return false
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e21 {
		return true
	}
	s := num(f)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return false
	}
	p, err := strconv.ParseFloat(s[:end], 64)
	if err != nil {
		return false
	}
	if neg {
		p = -p
	}
	return p == f
}
