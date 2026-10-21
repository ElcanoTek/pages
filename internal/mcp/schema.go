// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// A small schema language standing in for the zod 4 schemas lib/mcp-tools.js
// declared. One definition does both jobs zod did for the tool registry:
//
//   - JSONSchema emits the advertised draft-07 JSON Schema exactly as
//     z.toJSONSchema(schema, {target: "draft-7", io}) did (zod 4.6.5) — same
//     keys, same key order, same type-union compaction — because the
//     tools/list bytes are a wire contract (test/mcp-wire.golden.json);
//   - parse validates a JSON value as safeParse did: defaults applied, the
//     id transform run, objects rebuilt in shape order, and the issues zod
//     reported with zod's English messages (the SDK quoted them verbatim in
//     "Input validation error" results).
//
// Only what the registry uses is implemented. Builders return copies, so a
// shared schema (Slug, IdOut, UrlsSchema…) can be refined per use, as in zod.
// As in zod, Strict, Loose, Extend and Partial drop the schema's
// description, while checks (Min, Max, Regex…), Describe and Meta keep it.

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/mcpproto"
)

type kind int

const (
	kString kind = iota
	kNumber
	kBool
	kNull
	kUnknown
	kNever
	kLiteral
	kEnum
	kObject
	kArray
	kRecord
	kUnion
	kOptional
	kNullable
	kDefault
	kPipe
)

// catchall modes of an object.
const (
	catchStrip  = iota // zod's default: unknown keys dropped
	catchStrict        // .strict(): unknown keys are an issue
	catchLoose         // .loose(): unknown keys kept
)

const maxSafeInteger = 1<<53 - 1

type kv struct {
	key   string
	value any
}

// check is one zod check, run in declaration order.
type check struct {
	op      string // min, max, length, regex, datetime, uuid, url, int, gt, gte, lte, refine
	n       float64
	pattern *pattern
	refine  func(v any) bool
	message string
}

type pattern struct {
	source string // the JavaScript source, as emitted
	re     *regexp.Regexp
}

func newPattern(source string) *pattern {
	return &pattern{source: source, re: regexp.MustCompile(source)}
}

type field struct {
	name   string
	schema *Schema
}

// Schema is one node of the language.
type Schema struct {
	kind    kind
	meta    []kv // description and other metadata, in registration order
	checks  []check
	literal any
	enum    []string
	fields  []field
	catch   int
	elem    *Schema
	options []*Schema
	inner   *Schema // optional, nullable, default, pipe
	def     any     // default value (a jsonv value)
	// transform is a pipe's output step.
	transform func(any) any
	// superRefine runs on a parsed object that had no aborting issue.
	superRefine func(v *jsonv.Object, add func(path []any, message string))
}

func (s *Schema) clone() *Schema {
	c := *s
	c.meta = append([]kv(nil), s.meta...)
	c.checks = append([]check(nil), s.checks...)
	c.fields = append([]field(nil), s.fields...)
	return &c
}

// withCheck is zod's .check(): a clone that keeps the parent's metadata.
func (s *Schema) withCheck(ch check) *Schema {
	c := s.clone()
	c.checks = append(c.checks, ch)
	return c
}

// ── constructors ────────────────────────────────────────────────────────────

func zString() *Schema  { return &Schema{kind: kString} }
func zNumber() *Schema  { return &Schema{kind: kNumber} }
func zBool() *Schema    { return &Schema{kind: kBool} }
func zNull() *Schema    { return &Schema{kind: kNull} }
func zUnknown() *Schema { return &Schema{kind: kUnknown} }
func zNever() *Schema   { return &Schema{kind: kNever} }

// zInt is z.number().int().
func zInt() *Schema { return zNumber().Int() }

func zLiteral(v any) *Schema { return &Schema{kind: kLiteral, literal: v} }

func zEnum(values ...string) *Schema { return &Schema{kind: kEnum, enum: values} }

func f(name string, s *Schema) field { return field{name: name, schema: s} }

func zObject(fields ...field) *Schema { return &Schema{kind: kObject, fields: fields} }

func zArray(elem *Schema) *Schema { return &Schema{kind: kArray, elem: elem} }

// zRecord is z.record(z.string(), value).
func zRecord(value *Schema) *Schema { return &Schema{kind: kRecord, elem: value} }

func zUnion(options ...*Schema) *Schema { return &Schema{kind: kUnion, options: options} }

// zPipe is in.transform(fn).
func zPipe(in *Schema, fn func(any) any) *Schema {
	return &Schema{kind: kPipe, inner: in, transform: fn}
}

// ── modifiers ───────────────────────────────────────────────────────────────

// Describe is .describe(text).
func (s *Schema) Describe(text string) *Schema { return s.Meta("description", text) }

// Meta is .meta({key: value}): metadata merged over the inherited set.
func (s *Schema) Meta(key string, value any) *Schema {
	c := s.clone()
	for i := range c.meta {
		if c.meta[i].key == key {
			c.meta[i].value = value
			return c
		}
	}
	c.meta = append(c.meta, kv{key, value})
	return c
}

func (s *Schema) Optional() *Schema { return &Schema{kind: kOptional, inner: s} }
func (s *Schema) Nullable() *Schema { return &Schema{kind: kNullable, inner: s} }

// Default is .default(v): v is a jsonv value.
func (s *Schema) Default(v any) *Schema { return &Schema{kind: kDefault, inner: s, def: v} }

// Min is .min(n): a length bound on strings and arrays, >= on numbers.
func (s *Schema) Min(n float64) *Schema {
	if s.kind == kNumber {
		return s.withCheck(check{op: "gte", n: n})
	}
	return s.withCheck(check{op: "min", n: n})
}

// Max is .max(n): a length bound on strings and arrays, <= on numbers.
func (s *Schema) Max(n float64) *Schema {
	if s.kind == kNumber {
		return s.withCheck(check{op: "lte", n: n})
	}
	return s.withCheck(check{op: "max", n: n})
}

// Length is .length(n) on an array.
func (s *Schema) Length(n float64) *Schema { return s.withCheck(check{op: "length", n: n}) }

func (s *Schema) Int() *Schema         { return s.withCheck(check{op: "int"}) }
func (s *Schema) Positive() *Schema    { return s.withCheck(check{op: "gt", n: 0}) }
func (s *Schema) NonNegative() *Schema { return s.withCheck(check{op: "gte", n: 0}) }

// Regex is .regex(/source/).
func (s *Schema) Regex(source string) *Schema {
	return s.withCheck(check{op: "regex", pattern: newPattern(source)})
}

// zod's emitted patterns for its string formats.
const (
	dateTimePattern = `^(?:(?:\d\d[2468][048]|\d\d[13579][26]|\d\d0[48]|[02468][048]00|[13579][26]00)-02-29|\d{4}-(?:(?:0[13578]|1[02])-(?:0[1-9]|[12]\d|3[01])|(?:0[469]|11)-(?:0[1-9]|[12]\d|30)|(?:02)-(?:0[1-9]|1\d|2[0-8])))T(?:(?:[01]\d|2[0-3]):[0-5]\d:[0-5]\d(?:\.\d+)?(?:Z|([+-](?:[01]\d|2[0-3]):[0-5]\d)))$`
	uuidPattern     = `^([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`
)

var (
	dateTimeRE = newPattern(dateTimePattern)
	uuidRE     = newPattern(uuidPattern)
)

// DateTime is .datetime({offset: true}).
func (s *Schema) DateTime() *Schema { return s.withCheck(check{op: "datetime", pattern: dateTimeRE}) }

// UUID is .uuid().
func (s *Schema) UUID() *Schema { return s.withCheck(check{op: "uuid", pattern: uuidRE}) }

// URL is .url().
func (s *Schema) URL() *Schema { return s.withCheck(check{op: "url"}) }

// Refine is .refine(fn, message).
func (s *Schema) Refine(fn func(v any) bool, message string) *Schema {
	return s.withCheck(check{op: "refine", refine: fn, message: message})
}

// SuperRefine is .superRefine(fn) on an object.
func (s *Schema) SuperRefine(fn func(v *jsonv.Object, add func(path []any, message string))) *Schema {
	c := s.clone()
	c.superRefine = fn
	return c
}

func (s *Schema) Strict() *Schema {
	c := s.clone()
	c.meta = nil
	c.catch = catchStrict
	return c
}

func (s *Schema) Loose() *Schema {
	c := s.clone()
	c.meta = nil
	c.catch = catchLoose
	return c
}

// Extend is .extend({...}): fields replace same-named ones in place or are
// appended.
func (s *Schema) Extend(fields ...field) *Schema {
	c := s.clone()
	c.meta = nil
	c.superRefine = nil
	for _, nf := range fields {
		replaced := false
		for i := range c.fields {
			if c.fields[i].name == nf.name {
				c.fields[i] = nf
				replaced = true
				break
			}
		}
		if !replaced {
			c.fields = append(c.fields, nf)
		}
	}
	return c
}

// Partial is .partial({name: true, ...}).
func (s *Schema) Partial(names ...string) *Schema {
	c := s.clone()
	c.meta = nil
	c.superRefine = nil
	for i := range c.fields {
		for _, n := range names {
			if c.fields[i].name == n {
				c.fields[i].schema = c.fields[i].schema.Optional()
			}
		}
	}
	return c
}

// ── optionality ─────────────────────────────────────────────────────────────

const (
	optNone = iota
	optOptional
	optDefaulted
)

func (s *Schema) optin() int {
	switch s.kind {
	case kOptional, kUnknown:
		return optOptional
	case kDefault:
		return optDefaulted
	case kNullable:
		return s.inner.optin()
	case kPipe:
		return s.inner.optin()
	case kUnion:
		result := optNone
		for _, o := range s.options {
			switch o.optin() {
			case optDefaulted:
				return optDefaulted
			case optOptional:
				result = optOptional
			}
		}
		return result
	}
	return optNone
}

func (s *Schema) optoutOptional() bool {
	switch s.kind {
	case kOptional, kUnknown:
		return true
	case kNullable:
		return s.inner.optoutOptional()
	case kUnion:
		for _, o := range s.options {
			if o.optoutOptional() {
				return true
			}
		}
	}
	return false
}

// ── JSON Schema ─────────────────────────────────────────────────────────────

type ioDir int

const (
	ioInput ioDir = iota
	ioOutput
)

// JSONSchema is the draft-07 document for s, as z.toJSONSchema emitted it.
func (s *Schema) JSONSchema(dir ioDir) *jsonv.Object {
	root := s.emit(dir)
	compact(root)
	out := jsonv.ObjectOf("$schema", "http://json-schema.org/draft-07/schema#")
	root.Range(func(k string, v any) bool {
		out.Set(k, v)
		return true
	})
	return out
}

func (s *Schema) emit(dir ioDir) *jsonv.Object {
	o := jsonv.NewObject()
	wrapper := false
	switch s.kind {
	case kString:
		emitString(o, s)
	case kNumber:
		emitNumber(o, s)
	case kBool:
		o.Set("type", "boolean")
	case kNull:
		o.Set("type", "null")
	case kUnknown:
	case kNever:
		o.Set("not", jsonv.NewObject())
	case kLiteral:
		o.Set("type", jsTypeof(s.literal))
		o.Set("const", s.literal)
	case kEnum:
		o.Set("type", "string")
		vals := make([]any, len(s.enum))
		for i, v := range s.enum {
			vals[i] = v
		}
		o.Set("enum", vals)
	case kObject:
		o.Set("type", "object")
		props := jsonv.NewObject()
		var required []any
		for _, fd := range s.fields {
			props.Set(fd.name, fd.schema.emit(dir))
			if dir == ioInput && fd.schema.optin() == optNone || dir == ioOutput && !fd.schema.optoutOptional() {
				required = append(required, fd.name)
			}
		}
		o.Set("properties", props)
		if len(required) > 0 {
			o.Set("required", required)
		}
		switch {
		case s.catch == catchStrict:
			o.Set("additionalProperties", false)
		case s.catch == catchLoose:
			o.Set("additionalProperties", jsonv.NewObject())
		case dir == ioOutput:
			o.Set("additionalProperties", false)
		}
	case kArray:
		lo, hi := lengthBounds(s)
		if lo >= 0 {
			o.Set("minItems", float64(lo))
		}
		if hi >= 0 {
			o.Set("maxItems", float64(hi))
		}
		o.Set("type", "array")
		o.Set("items", s.elem.emit(dir))
	case kRecord:
		o.Set("type", "object")
		o.Set("propertyNames", jsonv.ObjectOf("type", "string"))
		o.Set("additionalProperties", s.elem.emit(dir))
	case kUnion:
		opts := make([]any, len(s.options))
		for i, opt := range s.options {
			opts[i] = opt.emit(dir)
		}
		o.Set("anyOf", opts)
	case kNullable:
		o.Set("anyOf", []any{s.inner.emit(dir), jsonv.ObjectOf("type", "null")})
	case kOptional, kPipe:
		wrapper = true
	case kDefault:
		o.Set("default", s.def)
		wrapper = true
	}
	for _, m := range s.meta {
		o.Set(m.key, m.value)
	}
	if wrapper {
		// The wrapper's own keys first, then the wrapped schema's, the
		// wrapper winning on a shared key (zod's flattenRef).
		s.inner.emit(dir).Range(func(k string, v any) bool {
			if !o.Has(k) {
				o.Set(k, v)
			}
			return true
		})
	}
	return o
}

func jsTypeof(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	}
	return "string"
}

// lengthBounds folds min/max/length checks (-1 when absent).
func lengthBounds(s *Schema) (lo, hi int) {
	lo, hi = -1, -1
	for _, ch := range s.checks {
		n := int(ch.n)
		switch ch.op {
		case "min":
			if lo < 0 || n > lo {
				lo = n
			}
		case "max":
			if hi < 0 || n < hi {
				hi = n
			}
		case "length":
			if lo < 0 || n > lo {
				lo = n
			}
			if hi < 0 || n < hi {
				hi = n
			}
		}
	}
	return lo, hi
}

func emitString(o *jsonv.Object, s *Schema) {
	o.Set("type", "string")
	lo, hi := lengthBounds(s)
	if lo >= 0 {
		o.Set("minLength", float64(lo))
	}
	if hi >= 0 {
		o.Set("maxLength", float64(hi))
	}
	format := ""
	var pat *pattern
	for _, ch := range s.checks {
		switch ch.op {
		case "datetime":
			format, pat = "date-time", ch.pattern
		case "uuid":
			format, pat = "uuid", ch.pattern
		case "url":
			format = "uri"
		case "regex":
			pat = ch.pattern
		}
	}
	if format != "" {
		o.Set("format", format)
	}
	if pat != nil {
		o.Set("pattern", pat.source)
	}
}

func emitNumber(o *jsonv.Object, s *Schema) {
	isInt := false
	var minimum, maximum, exMin *float64
	narrowMin := func(p **float64, v float64) {
		if *p == nil || v > **p {
			*p = &v
		}
	}
	narrowMax := func(p **float64, v float64) {
		if *p == nil || v < **p {
			*p = &v
		}
	}
	for _, ch := range s.checks {
		switch ch.op {
		case "int":
			isInt = true
			narrowMin(&minimum, -maxSafeInteger)
			narrowMax(&maximum, maxSafeInteger)
		case "gt":
			narrowMin(&exMin, ch.n)
		case "gte":
			narrowMin(&minimum, ch.n)
		case "lte":
			narrowMax(&maximum, ch.n)
		}
	}
	if isInt {
		o.Set("type", "integer")
	} else {
		o.Set("type", "number")
	}
	switch {
	case exMin != nil && (minimum == nil || *exMin >= *minimum):
		o.Set("exclusiveMinimum", *exMin)
	case minimum != nil:
		o.Set("minimum", *minimum)
	}
	if maximum != nil {
		o.Set("maximum", *maximum)
	}
}

// schemaChildKeys are the keywords whose values are subschemas.
var schemaChildKeys = []string{"items", "additionalProperties", "propertyNames", "not"}

// compact is zod's compactTypeUnion, applied bottom-up to every subschema:
// an anyOf of bare {"type": …} branches becomes a type array.
func compact(o *jsonv.Object) {
	if props, ok := o.Value("properties").(*jsonv.Object); ok {
		props.Range(func(_ string, v any) bool {
			if child, ok := v.(*jsonv.Object); ok {
				compact(child)
			}
			return true
		})
	}
	for _, k := range schemaChildKeys {
		if child, ok := o.Value(k).(*jsonv.Object); ok {
			compact(child)
		}
	}
	opts, ok := o.Value("anyOf").([]any)
	if !ok {
		return
	}
	for _, opt := range opts {
		if child, ok := opt.(*jsonv.Object); ok {
			compact(child)
		}
	}
	if len(opts) == 0 || o.Has("type") {
		return
	}
	var types []any
	for _, opt := range opts {
		child, ok := opt.(*jsonv.Object)
		if !ok || child.Len() != 1 || !child.Has("type") {
			return
		}
		members := []any{child.Value("type")}
		if arr, ok := child.Value("type").([]any); ok {
			members = arr
		}
		for _, m := range members {
			if _, ok := m.(string); !ok {
				return
			}
			dup := false
			for _, t := range types {
				if t == m {
					dup = true
				}
			}
			if !dup {
				types = append(types, m)
			}
		}
	}
	o.Delete("anyOf")
	if len(types) == 1 {
		o.Set("type", types[0])
	} else {
		o.Set("type", types)
	}
}

// ── parsing ─────────────────────────────────────────────────────────────────
//
// zod's run loop: a schema parses its type, then runs its checks over the
// same value. A length check (min/max/length) has a `when` guard and runs
// whenever the value has a length (a string or an array) unless an issue
// explicitly aborted; every other check runs only while nothing has aborted.
// A type mismatch does not explicitly abort, which is why zod reports
// "expected string, received array" followed by "Too small: expected array
// to have >=1 items" for z.string().min(1) given [].

type issue struct {
	path    []any
	message string
	// cont is zod's `continue`: contTrue, contUnset (a type mismatch) or
	// contFalse (an explicit abort: the int check).
	cont int
}

const (
	contUnset = 0
	contTrue  = 1
	contFalse = -1
)

type result struct {
	value  any
	issues []issue
	// aborted is payload.aborted, set by a failed pipe.
	aborted bool
}

func (r *result) isAborted(from int) bool {
	if r.aborted {
		return true
	}
	for _, is := range r.issues[from:] {
		if is.cont != contTrue {
			return true
		}
	}
	return false
}

func (r *result) explicitlyAborted() bool {
	if r.aborted {
		return true
	}
	for _, is := range r.issues {
		if is.cont == contFalse {
			return true
		}
	}
	return false
}

func (r *result) add(message string) {
	r.issues = append(r.issues, issue{message: message, cont: contTrue})
}

func prefix(key any, issues []issue) []issue {
	for i := range issues {
		issues[i].path = append([]any{key}, issues[i].path...)
	}
	return issues
}

// parsedType is zod's util.parsedType for a JSON value (absent = undefined).
func parsedType(v any, present bool) string {
	if !present {
		return "undefined"
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		if math.IsInf(x, 0) {
			return jsonv.FormatNumber(x)
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	}
	return "object"
}

func invalidType(expected string, v any, present bool) issue {
	return issue{message: fmt.Sprintf("Invalid input: expected %s, received %s", expected, parsedType(v, present))}
}

func stringifyPrimitive(v any) string {
	if s, ok := v.(string); ok {
		return `"` + s + `"`
	}
	return jsonv.ToString(v)
}

func numberString(n float64) string { return jsonv.FormatNumber(n) }

// Parse validates v (a jsonv value) and returns the parsed value, or the
// issues as an *mcpproto.ValidationError.
func (s *Schema) Parse(v any) (any, error) {
	r := s.run(v, true)
	if len(r.issues) == 0 {
		return r.value, nil
	}
	ve := &mcpproto.ValidationError{}
	for _, is := range r.issues {
		ve.Issues = append(ve.Issues, mcpproto.Issue{Path: is.path, Message: is.message})
	}
	return nil, ve
}

func fail(is issue) result { return result{issues: []issue{is}} }

func (s *Schema) run(v any, present bool) result {
	r := s.parseType(v, present)
	if len(s.checks) > 0 {
		s.runChecks(v, present, &r)
	}
	return r
}

func (s *Schema) parseType(v any, present bool) result {
	switch s.kind {
	case kOptional:
		if !present {
			return result{}
		}
		return s.inner.run(v, true)
	case kNullable:
		if present && v == nil {
			return result{}
		}
		return s.inner.run(v, present)
	case kDefault:
		if !present {
			return result{value: cloneValue(s.def)}
		}
		return s.inner.run(v, true)
	case kPipe:
		r := s.inner.run(v, present)
		if len(r.issues) > 0 {
			r.aborted = true
			return r
		}
		return result{value: s.transform(r.value)}
	case kUnknown:
		return result{value: v}
	case kNever:
		return fail(invalidType("never", v, present))
	case kLiteral:
		if present && v == s.literal {
			return result{value: v}
		}
		return fail(issue{message: "Invalid input: expected " + stringifyPrimitive(s.literal)})
	case kEnum:
		if str, ok := v.(string); ok && present {
			for _, e := range s.enum {
				if e == str {
					return result{value: v}
				}
			}
		}
		if len(s.enum) == 1 {
			return fail(issue{message: "Invalid input: expected " + stringifyPrimitive(s.enum[0])})
		}
		quoted := make([]string, len(s.enum))
		for i, e := range s.enum {
			quoted[i] = stringifyPrimitive(e)
		}
		return fail(issue{message: "Invalid option: expected one of " + strings.Join(quoted, "|")})
	case kBool:
		if _, ok := v.(bool); ok && present {
			return result{value: v}
		}
		return fail(invalidType("boolean", v, present))
	case kNull:
		if present && v == nil {
			return result{}
		}
		return fail(invalidType("null", v, present))
	case kString:
		if _, ok := v.(string); ok && present {
			return result{value: v}
		}
		return fail(invalidType("string", v, present))
	case kNumber:
		if n, ok := v.(float64); ok && present && !math.IsInf(n, 0) {
			return result{value: v}
		}
		return fail(invalidType("number", v, present))
	case kObject:
		return s.runObject(v, present)
	case kArray:
		arr, ok := v.([]any)
		if !ok || !present {
			return fail(invalidType("array", v, present))
		}
		out := make([]any, len(arr))
		var issues []issue
		for i, e := range arr {
			er := s.elem.run(e, true)
			issues = append(issues, prefix(i, er.issues)...)
			out[i] = er.value
		}
		return result{value: out, issues: issues}
	case kRecord:
		obj, ok := v.(*jsonv.Object)
		if !ok || !present {
			return fail(invalidType("record", v, present))
		}
		out := jsonv.NewObject()
		var issues []issue
		obj.Range(func(k string, e any) bool {
			er := s.elem.run(e, true)
			if len(er.issues) > 0 {
				issues = append(issues, prefix(k, er.issues)...)
			} else {
				out.Set(k, er.value)
			}
			return true
		})
		return result{value: out, issues: issues}
	case kUnion:
		var results []result
		for _, o := range s.options {
			r := o.run(v, present)
			if len(r.issues) == 0 {
				return r
			}
			results = append(results, r)
		}
		var nonAborted []result
		for _, r := range results {
			if !r.isAborted(0) {
				nonAborted = append(nonAborted, r)
			}
		}
		if len(nonAborted) == 1 {
			return nonAborted[0]
		}
		return fail(issue{message: "Invalid input"})
	}
	return result{value: v}
}

// lengthOf is the `.length` zod's length checks see, and the origin they
// report: code points of a string (zod measures strings in code points),
// elements of an array.
func lengthOf(v any, present bool) (int, string, bool) {
	if !present {
		return 0, "", false
	}
	switch x := v.(type) {
	case string:
		return utf8.RuneCountInString(x), "string", true
	case []any:
		return len(x), "array", true
	}
	return 0, "", false
}

var lengthUnits = map[string]string{"string": "characters", "array": "items"}

func (s *Schema) runChecks(v any, present bool, r *result) {
	aborted := r.isAborted(0)
	for _, ch := range s.checks {
		from := len(r.issues)
		switch ch.op {
		case "min", "max", "length":
			if r.explicitlyAborted() {
				continue
			}
			n, origin, ok := lengthOf(v, present)
			if !ok {
				continue
			}
			unit := lengthUnits[origin]
			size := float64(n)
			switch {
			case ch.op == "min" && size < ch.n:
				r.add(fmt.Sprintf("Too small: expected %s to have >=%s %s", origin, numberString(ch.n), unit))
			case ch.op == "max" && size > ch.n:
				r.add(fmt.Sprintf("Too big: expected %s to have <=%s %s", origin, numberString(ch.n), unit))
			case ch.op == "length" && size > ch.n:
				r.add(fmt.Sprintf("Too big: expected %s to have exactly %s %s", origin, numberString(ch.n), unit))
			case ch.op == "length" && size < ch.n:
				r.add(fmt.Sprintf("Too small: expected %s to have exactly %s %s", origin, numberString(ch.n), unit))
			}
		default:
			if aborted {
				continue
			}
			runValueCheck(ch, v, r)
		}
		if !aborted && len(r.issues) > from {
			aborted = r.isAborted(from)
		}
	}
}

func runValueCheck(ch check, v any, r *result) {
	switch ch.op {
	case "regex", "datetime", "uuid", "url":
		str, _ := v.(string)
		switch {
		case ch.op == "url":
			if !isURL(str) {
				r.add("Invalid URL")
			}
		case ch.pattern.re.MatchString(str):
		case ch.op == "regex":
			r.add("Invalid string: must match pattern /" + ch.pattern.source + "/")
		case ch.op == "datetime":
			r.add("Invalid ISO datetime")
		default:
			r.add("Invalid UUID")
		}
	case "refine":
		if !ch.refine(v) {
			r.add(ch.message)
		}
	case "int":
		n, _ := v.(float64)
		if n != math.Trunc(n) {
			r.issues = append(r.issues, issue{message: "Invalid input: expected int, received number", cont: contFalse})
			return
		}
		if math.Abs(n) > maxSafeInteger {
			if n > 0 {
				r.add("Too big: expected int to be <=" + strconv.Itoa(maxSafeInteger))
			} else {
				r.add("Too small: expected int to be >=-" + strconv.Itoa(maxSafeInteger))
			}
		}
	case "gt":
		if n, _ := v.(float64); !(n > ch.n) {
			r.add("Too small: expected number to be >" + numberString(ch.n))
		}
	case "gte":
		if n, _ := v.(float64); !(n >= ch.n) {
			r.add("Too small: expected number to be >=" + numberString(ch.n))
		}
	case "lte":
		if n, _ := v.(float64); !(n <= ch.n) {
			r.add("Too big: expected number to be <=" + numberString(ch.n))
		}
	}
}

// isURL approximates new URL(value.trim()) succeeding: an absolute URL with a
// scheme.
func isURL(s string) bool {
	u, err := url.Parse(jsstr.Trim(s))
	return err == nil && u.Scheme != "" && (u.Host != "" || u.Opaque != "" || u.Path != "")
}

func (s *Schema) runObject(v any, present bool) result {
	obj, ok := v.(*jsonv.Object)
	if !ok || !present {
		return fail(invalidType("object", v, present))
	}
	out := jsonv.NewObject()
	r := result{value: out}
	known := map[string]bool{}
	for _, fd := range s.fields {
		known[fd.name] = true
		val, has := obj.Get(fd.name)
		fr := fd.schema.run(val, has)
		optin := fd.schema.optin()
		optOut := fd.schema.optoutOptional()
		if !has && optOut && optin == optOptional {
			continue
		}
		if len(fr.issues) > 0 {
			if optin != optNone && optOut && !has {
				continue
			}
			r.issues = append(r.issues, prefix(fd.name, fr.issues)...)
		}
		if !has && optin == optNone {
			continue
		}
		if len(fr.issues) == 0 {
			out.Set(fd.name, fr.value)
		}
	}
	var unrecognized []string
	obj.Range(func(k string, e any) bool {
		if known[k] {
			return true
		}
		switch s.catch {
		case catchStrict:
			unrecognized = append(unrecognized, k)
		case catchLoose:
			out.Set(k, e)
		}
		return true
	})
	if len(unrecognized) > 0 {
		quoted := make([]string, len(unrecognized))
		for i, k := range unrecognized {
			quoted[i] = stringifyPrimitive(k)
		}
		plural := ""
		if len(unrecognized) > 1 {
			plural = "s"
		}
		r.add("Unrecognized key" + plural + ": " + strings.Join(quoted, ", "))
	}
	if s.superRefine != nil && !r.isAborted(0) {
		s.superRefine(out, func(path []any, message string) {
			r.issues = append(r.issues, issue{path: path, message: message, cont: contTrue})
		})
	}
	return r
}

// cloneValue deep-copies a jsonv value (defaults must not be shared).
func cloneValue(v any) any {
	switch x := v.(type) {
	case *jsonv.Object:
		c := jsonv.NewObject()
		x.Range(func(k string, e any) bool {
			c.Set(k, cloneValue(e))
			return true
		})
		return c
	case []any:
		c := make([]any, len(x))
		for i, e := range x {
			c[i] = cloneValue(e)
		}
		return c
	}
	return v
}
