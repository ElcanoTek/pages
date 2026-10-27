// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// JSON Schema 2020-12 validation with Ajv's observable behaviour.
//
// The JS server compiled every managed schema with
//
//	new Ajv2020({ allErrors: true, strict: true, validateFormats: true }) + ajv-formats
//
// and agents read the resulting errors (bounded to 12) in every refused write.
// So this reproduces Ajv 8 rather than the specification in the places a
// client can see: which schemas compile at all (strict mode refuses unknown
// keywords, union types, keywords without an applicable "type", "required"
// names no "properties" defines, open tuples, "if" without "then"/"else", …),
// and the exact error list — keyword evaluation order, instancePath,
// schemaPath (including Ajv's "#/$defs/x/…" for inlined references and "#/…"
// for separately compiled ones) and message strings.
//
// The structure follows Ajv's code generator: a schema's keywords run in rule
// groups (untyped, number, string, array, object), each typed group guarded by
// a data-type check, with the type error reported first or in place of the
// matching group exactly where Ajv reports it.
//
// Meta-schema validation of the schema document itself is delegated to
// github.com/santhosh-tekuri/jsonschema/v6 (see meta.go).

// ValidationError is one Ajv error object.
type ValidationError struct {
	InstancePath string `json:"instancePath"`
	SchemaPath   string `json:"schemaPath"`
	Keyword      string `json:"keyword"`
	Params       any    `json:"params"`
	Message      string `json:"message"`
}

// WriteJSON writes the error object with JavaScript number formatting.
func (e ValidationError) WriteJSON(b *strings.Builder) error {
	params := e.Params
	if params == nil {
		params = jsonv.NewObject()
	}
	s, err := jsonv.Marshal(jsonv.ObjectOf("instancePath", e.InstancePath, "schemaPath", e.SchemaPath,
		"keyword", e.Keyword, "params", params, "message", e.Message))
	b.WriteString(s)
	return err
}

// MarshalJSON writes the error object as JSON.
func (e ValidationError) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	err := e.WriteJSON(&b)
	return []byte(b.String()), err
}

// Validator is a compiled schema. It is safe for concurrent use.
type Validator struct {
	root      *jsonv.Object
	rootBase  string
	info      map[*jsonv.Object]*nodeInfo
	refs      map[string]*refTarget
	patterns  map[string]jsMatcher
	resources map[string]any // absolute id (no fragment) → schema
	anchors   map[string]any // absolute id#anchor → schema
	// unevaluated is set when any compiled schema uses unevaluated*, the only
	// keywords that need evaluated-property bookkeeping.
	unevaluated bool
}

type nodeInfo struct {
	base      string
	hasRules  bool
	refOnly   bool
	types     []string
	typeMsg   string
	typeParam any
	check     bool // report the type error up front
	groups    [5]bool
	// keywords holds, per rule group, the keywords this schema uses, in
	// Ajv's evaluation order.
	keywords [5][]string
	// propList/propSet are the "properties" names (without __proto__),
	// patternList the "patternProperties" patterns; additionalPatterns is
	// the same list as additionalProperties consults it.
	propList           []string
	propSet            map[string]bool
	patternList        []string
	additionalPatterns []string
}

type refTarget struct {
	node   any
	base   string
	inline bool
	root   bool // the root schema of the validator (callRootRef)
}

// ── keyword tables ───────────────────────────────────────────────────────────

var knownKeywords = toSet("$async $dynamicAnchor $dynamicRef $recursiveAnchor $recursiveRef $schema $id $defs $vocabulary $comment definitions id $ref maximum minimum exclusiveMaximum exclusiveMinimum multipleOf maxLength minLength pattern maxProperties minProperties required maxItems minItems uniqueItems type nullable const enum not anyOf oneOf allOf if then else propertyNames additionalProperties dependencies properties patternProperties prefixItems items contains format title description default deprecated readOnly writeOnly examples contentMediaType contentEncoding contentSchema dependentRequired dependentSchemas maxContains minContains unevaluatedProperties unevaluatedItems formatMaximum formatMinimum formatExclusiveMaximum formatExclusiveMinimum")

// groupRules lists, per rule group, the keywords Ajv evaluates, in order.
var groupRules = [5][]string{
	{"$dynamicAnchor", "$dynamicRef", "$recursiveAnchor", "$recursiveRef", "$comment", "id", "$ref", "type", "nullable", "const", "enum", "not", "anyOf", "oneOf", "allOf", "if", "then", "else"},
	{"maximum", "minimum", "exclusiveMaximum", "exclusiveMinimum", "multipleOf", "format"},
	{"maxLength", "minLength", "pattern", "format", "formatMaximum", "formatMinimum", "formatExclusiveMaximum", "formatExclusiveMinimum"},
	{"maxItems", "minItems", "prefixItems", "items", "contains", "uniqueItems", "maxContains", "minContains", "unevaluatedItems"},
	{"maxProperties", "minProperties", "required", "propertyNames", "additionalProperties", "dependencies", "properties", "patternProperties", "dependentRequired", "dependentSchemas", "unevaluatedProperties"},
}

var groupType = [5]string{"", "number", "string", "array", "object"}

// ruleKeywords is RULES.all: keywords that make a schema "have rules".
var ruleKeywords = func() map[string]bool {
	m := map[string]bool{}
	for _, g := range groupRules {
		for _, k := range g {
			m[k] = true
		}
	}
	return m
}()

// keywordTypes is each typed keyword's definition.type, in RULES.all order.
var keywordTypes = []struct {
	kw    string
	types []string
}{
	{"maximum", []string{"number"}}, {"minimum", []string{"number"}}, {"exclusiveMaximum", []string{"number"}}, {"exclusiveMinimum", []string{"number"}},
	{"multipleOf", []string{"number"}}, {"maxLength", []string{"string"}}, {"minLength", []string{"string"}}, {"pattern", []string{"string"}},
	{"maxProperties", []string{"object"}}, {"minProperties", []string{"object"}}, {"required", []string{"object"}},
	{"maxItems", []string{"array"}}, {"minItems", []string{"array"}}, {"uniqueItems", []string{"array"}},
	{"propertyNames", []string{"object"}}, {"additionalProperties", []string{"object"}}, {"dependencies", []string{"object"}},
	{"properties", []string{"object"}}, {"patternProperties", []string{"object"}}, {"prefixItems", []string{"array"}},
	{"items", []string{"array"}}, {"contains", []string{"array"}}, {"format", []string{"number", "string"}},
	{"dependentRequired", []string{"object"}}, {"dependentSchemas", []string{"object"}}, {"maxContains", []string{"array"}},
	{"minContains", []string{"array"}}, {"unevaluatedProperties", []string{"object"}}, {"unevaluatedItems", []string{"array"}},
	{"formatMaximum", []string{"string"}}, {"formatMinimum", []string{"string"}}, {"formatExclusiveMaximum", []string{"string"}}, {"formatExclusiveMinimum", []string{"string"}},
}

var jsonTypes = toSet("string number integer boolean null object array")

func toSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, k := range strings.Fields(s) {
		m[k] = true
	}
	return m
}

var errCompile = errors.New("schema does not compile")

func strictErr(format string, args ...any) error {
	return fmt.Errorf("%w: strict mode: %s", errCompile, fmt.Sprintf(format, args...))
}

func compileErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errCompile, fmt.Sprintf(format, args...))
}

func hasRules(o *jsonv.Object) bool {
	for _, k := range o.Keys() {
		if ruleKeywords[k] {
			return true
		}
	}
	return false
}

func hasRulesButRef(o *jsonv.Object) bool {
	for _, k := range o.Keys() {
		if k != "$ref" && ruleKeywords[k] {
			return true
		}
	}
	return false
}

func checkUnknown(o *jsonv.Object) error {
	for _, k := range o.Keys() {
		if !knownKeywords[k] {
			return strictErr("unknown keyword: %q", k)
		}
	}
	return nil
}

// alwaysValid is Ajv's alwaysValidSchema, including its unknown-keyword check.
func alwaysValid(s any) (bool, error) {
	switch x := s.(type) {
	case bool:
		return x, nil
	case *jsonv.Object:
		if x.Len() == 0 {
			return true, nil
		}
		if err := checkUnknown(x); err != nil {
			return false, err
		}
		return !hasRules(x), nil
	}
	return false, nil
}

func hasRef(s any) bool {
	switch x := s.(type) {
	case *jsonv.Object:
		for _, k := range x.Keys() {
			switch k {
			case "$ref", "$recursiveRef", "$recursiveAnchor", "$dynamicRef", "$dynamicAnchor":
				return true
			}
			if hasRef(x.Value(k)) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasRef(e) {
				return true
			}
		}
	}
	return false
}

func schemaTypes(o *jsonv.Object) ([]string, error) {
	var types []string
	switch t := o.Value("type").(type) {
	case nil:
	case string:
		types = []string{t}
	case []any:
		for _, e := range t {
			s, _ := e.(string)
			types = append(types, s)
		}
	}
	for _, t := range types {
		if !jsonTypes[t] {
			return nil, compileErr("type must be JSONType or JSONType[]: %s", strings.Join(types, ","))
		}
	}
	nullable, hasNullable := o.Get("nullable")
	if contains(types, "null") {
		if nullable == false {
			return nil, compileErr("type: null contradicts nullable: false")
		}
	} else {
		if len(types) == 0 && hasNullable {
			return nil, compileErr(`"nullable" cannot be used without "type"`)
		}
		if nullable == true {
			types = append(types, "null")
		}
	}
	return types, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ── URI resolution ───────────────────────────────────────────────────────────

func resolveURI(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

func splitFragment(u string) (string, string, bool) {
	doc, frag, ok := strings.Cut(u, "#")
	return doc, frag, ok
}

func normalizeID(id string) string { return strings.TrimSuffix(id, "#") }

// unescapeFragment is Ajv's: decodeURIComponent, then JSON-pointer unescape.
func unescapeFragment(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		s = d
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}

// ── compilation ──────────────────────────────────────────────────────────────

type strSet struct{ m map[string]bool }

func newStrSet() *strSet { return &strSet{m: map[string]bool{}} }

// cctx is Ajv's per-schema compilation context ("it"). Children are copies;
// defined is shared by reference until a data-changing child replaces it, as
// Ajv's definedProperties is.
type cctx struct {
	dataTypes []string
	defined   *strSet
	base      string
}

type compiler struct {
	v        *Validator
	envDone  map[string]bool
	visiting int
}

func newValidator(root *jsonv.Object) *Validator {
	v := &Validator{
		root:      root,
		info:      map[*jsonv.Object]*nodeInfo{},
		refs:      map[string]*refTarget{},
		patterns:  map[string]jsMatcher{},
		resources: map[string]any{},
		anchors:   map[string]any{},
	}
	if id, ok := root.Value("$id").(string); ok {
		v.rootBase = normalizeID(resolveURI("", id))
	}
	return v
}

// compileSchema compiles a schema whose meta-validation already passed.
func compileValidator(root *jsonv.Object) (*Validator, error) {
	v := newValidator(root)
	v.collectIDs(root, v.rootBase)
	c := &compiler{v: v, envDone: map[string]bool{}}
	if err := c.node(root, &cctx{defined: newStrSet(), base: v.rootBase}); err != nil {
		return nil, err
	}
	if err := v.checkRefCycles(); err != nil {
		return nil, err
	}
	return v, nil
}

// checkRefCycles refuses a schema in which a reference can lead back to a
// schema already being applied to the same data, such as
// {"$defs":{"a":{"allOf":[{"$ref":"#/$defs/a"}]}}} or a root "allOf"
// containing {"$ref":"#"}. Validating one would recurse forever without
// consuming data, and a goroutine stack overflow is fatal (recover cannot
// catch it). Recursive schemas that descend into the data (a tree whose
// "children" items refer back to the node) stay valid: "properties",
// "items" and the other data-changing keywords are not edges here.
//
// The edges are exactly the in-place applicators the validator follows
// (vstate.node and vstate.keyword), using the references compile resolved.
func (v *Validator) checkRefCycles() error {
	edges := func(o *jsonv.Object) []*jsonv.Object {
		ni := v.info[o]
		if ni == nil || !ni.hasRules {
			return nil
		}
		var out []*jsonv.Object
		add := func(s any) {
			if so, ok := s.(*jsonv.Object); ok {
				out = append(out, so)
			}
		}
		addRef := func(key string) {
			if t := v.refs[key]; t != nil {
				add(t.node)
			}
		}
		if ref, ok := o.Value("$ref").(string); ok {
			addRef(refKey(ni.base, ref))
		}
		if ni.refOnly {
			return out
		}
		for _, kw := range []string{"$dynamicRef", "$recursiveRef"} {
			if ref, ok := o.Value(kw).(string); ok {
				addRef(refKey(ni.base, "dyn:"+ref))
			}
		}
		for _, kw := range []string{"allOf", "anyOf", "oneOf"} {
			if arr, ok := o.Value(kw).([]any); ok {
				for _, e := range arr {
					add(e)
				}
			}
		}
		for _, kw := range []string{"not", "if", "then", "else"} {
			add(o.Value(kw))
		}
		for _, kw := range []string{"dependentSchemas", "dependencies"} {
			if m := asObject(o.Value(kw)); m != nil {
				for _, k := range m.Keys() {
					add(m.Value(k))
				}
			}
		}
		return out
	}
	// Iterative depth-first search with three colours, so a long chain of
	// schemas cannot itself exhaust the stack.
	const (
		white = iota
		grey
		black
	)
	colour := map[*jsonv.Object]int{}
	type frame struct {
		node *jsonv.Object
		next []*jsonv.Object
	}
	for start := range v.info {
		if colour[start] != white {
			continue
		}
		colour[start] = grey
		stack := []frame{{start, edges(start)}}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			if len(top.next) == 0 {
				colour[top.node] = black
				stack = stack[:len(stack)-1]
				continue
			}
			n := top.next[0]
			top.next = top.next[1:]
			switch colour[n] {
			case grey:
				return compileErr("reference cycle: a schema refers back to itself without descending into the data")
			case white:
				colour[n] = grey
				stack = append(stack, frame{n, edges(n)})
			}
		}
	}
	return nil
}

// collectIDs registers $id resources and $anchor/$dynamicAnchor names, as
// Ajv's getSchemaRefs does when a schema is added.
func (v *Validator) collectIDs(s any, base string) {
	o, ok := s.(*jsonv.Object)
	if !ok {
		return
	}
	if id, ok := o.Value("$id").(string); ok {
		base = normalizeID(resolveURI(base, id))
		v.resources[base] = o
	}
	for _, k := range []string{"$anchor", "$dynamicAnchor"} {
		if a, ok := o.Value(k).(string); ok {
			v.anchors[base+"#"+a] = o
		}
	}
	for _, k := range o.Keys() {
		val := o.Value(k)
		switch k {
		case "additionalItems", "contains", "additionalProperties", "propertyNames", "not", "if", "then", "else",
			"unevaluatedItems", "unevaluatedProperties", "contentSchema":
			v.collectIDs(val, base)
		case "items":
			if arr, ok := val.([]any); ok {
				for _, e := range arr {
					v.collectIDs(e, base)
				}
			} else {
				v.collectIDs(val, base)
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			if arr, ok := val.([]any); ok {
				for _, e := range arr {
					v.collectIDs(e, base)
				}
			}
		case "$defs", "definitions", "properties", "patternProperties", "dependencies", "dependentSchemas":
			if m, ok := val.(*jsonv.Object); ok {
				for _, pk := range m.Keys() {
					v.collectIDs(m.Value(pk), base)
				}
			}
		}
	}
}

func (v *Validator) nodeInfo(o *jsonv.Object, base string) (*nodeInfo, error) {
	if ni, ok := v.info[o]; ok {
		return ni, nil
	}
	types, err := schemaTypes(o)
	if err != nil {
		return nil, err
	}
	ni := &nodeInfo{base: base, hasRules: hasRules(o), types: types}
	_, hasRefKw := o.Get("$ref")
	ni.refOnly = hasRefKw && !hasRulesButRef(o)
	switch t := o.Value("type").(type) {
	case string:
		ni.typeMsg = t
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i], _ = e.(string)
		}
		ni.typeMsg = strings.Join(parts, ",")
	}
	ni.typeParam = o.Value("type")
	for g, rules := range groupRules {
		for _, k := range rules {
			if o.Has(k) {
				ni.groups[g] = true
				ni.keywords[g] = append(ni.keywords[g], k)
			}
		}
	}
	ni.propList = schemaProps(asObject(o.Value("properties")))
	ni.propSet = make(map[string]bool, len(ni.propList))
	for _, k := range ni.propList {
		ni.propSet[k] = true
	}
	ni.patternList = schemaProps(asObject(o.Value("patternProperties")))
	ni.additionalPatterns = ni.patternList
	if o.Has("unevaluatedProperties") || o.Has("unevaluatedItems") {
		v.unevaluated = true
	}
	// Ajv checks the type up front unless exactly one type is given and its
	// group has keywords to run, in which case the group's else-branch reports.
	ni.check = len(types) > 0
	if len(types) == 1 {
		for g := 1; g < 5; g++ {
			if groupType[g] == types[0] && ni.groups[g] {
				ni.check = false
			}
		}
	}
	v.info[o] = ni
	return ni, nil
}

func includesType(ts []string, t string) bool {
	return contains(ts, t) || (t == "integer" && contains(ts, "number"))
}

func hasApplicableType(ts []string, t string) bool {
	return contains(ts, t) || (t == "number" && contains(ts, "integer"))
}

func (c *compiler) child(ctx *cctx, dataChange bool, dataTypes []string) *cctx {
	ch := *ctx
	ch.dataTypes = append([]string(nil), ctx.dataTypes...)
	if dataChange {
		ctx.defined = newStrSet()
		ch.defined = ctx.defined
		ch.dataTypes = dataTypes
	}
	return &ch
}

func (c *compiler) sub(s any, ctx *cctx, dataChange bool, dataTypes []string) error {
	return c.node(s, c.child(ctx, dataChange, dataTypes))
}

func (c *compiler) node(s any, ctx *cctx) error {
	o, ok := s.(*jsonv.Object)
	if !ok {
		return nil
	}
	c.visiting++
	if c.visiting > 200000 {
		return compileErr("schema is too complex to compile")
	}
	if err := checkUnknown(o); err != nil {
		return err
	}
	if !hasRules(o) {
		return nil
	}
	if id, ok := o.Value("$id").(string); ok && o != c.v.root {
		ctx.base = normalizeID(resolveURI(ctx.base, id))
	}
	ni, err := c.v.nodeInfo(o, ctx.base)
	if err != nil {
		return err
	}
	if ni.refOnly {
		return c.ref(o, ctx)
	}
	if err := c.strictTypes(o, ni, ctx); err != nil {
		return err
	}
	for g, rules := range groupRules {
		if !ni.groups[g] {
			continue
		}
		for _, kw := range rules {
			if !o.Has(kw) {
				continue
			}
			if err := c.keyword(o, kw, ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *compiler) strictTypes(o *jsonv.Object, ni *nodeInfo, ctx *cctx) error {
	types := ni.types
	if len(types) > 0 {
		if len(ctx.dataTypes) == 0 {
			ctx.dataTypes = append([]string(nil), types...)
		} else {
			for _, t := range types {
				if !includesType(ctx.dataTypes, t) {
					return strictErr("type %q not allowed by context %q", t, strings.Join(ctx.dataTypes, ","))
				}
			}
			var narrowed []string
			for _, t := range ctx.dataTypes {
				if includesType(types, t) {
					narrowed = append(narrowed, t)
				} else if contains(types, "integer") && t == "number" {
					narrowed = append(narrowed, "integer")
				}
			}
			ctx.dataTypes = narrowed
		}
	}
	if len(types) > 1 && (len(types) != 2 || !contains(types, "null")) {
		return strictErr("use allowUnionTypes to allow union type keyword")
	}
	for _, kt := range keywordTypes {
		if !o.Has(kt.kw) {
			continue
		}
		ok := false
		for _, t := range kt.types {
			if hasApplicableType(ctx.dataTypes, t) {
				ok = true
			}
		}
		if !ok {
			return strictErr("missing type %q for keyword %q", strings.Join(kt.types, ","), kt.kw)
		}
	}
	return nil
}

func (c *compiler) pattern(p string) (jsMatcher, error) {
	if re, ok := c.v.patterns[p]; ok {
		return re, nil
	}
	re, err := compileJSRegex(p, "u")
	if err != nil {
		return nil, compileErr("invalid pattern %q: %v", p, err)
	}
	c.v.patterns[p] = re
	return re, nil
}

func asObject(v any) *jsonv.Object { o, _ := v.(*jsonv.Object); return o }

func numberValue(v any) (float64, bool) { f, ok := v.(float64); return f, ok }

func (c *compiler) keyword(o *jsonv.Object, kw string, ctx *cctx) error {
	val := o.Value(kw)
	switch kw {
	case "id":
		return compileErr(`NOT SUPPORTED: keyword "id", use "$id" for schema ID`)
	case "$ref":
		return c.ref(o, ctx)
	case "$recursiveRef", "$dynamicRef":
		ref, _ := val.(string)
		if !strings.HasPrefix(ref, "#") {
			return compileErr("%q only supports hash fragment reference", kw)
		}
		return c.dynamicTarget(ref, ctx)
	case "enum":
		if arr, _ := val.([]any); len(arr) == 0 {
			return compileErr("enum must have non-empty array")
		}
	case "not":
		av, err := alwaysValid(val)
		if err != nil || av {
			return err
		}
		return c.sub(val, ctx, false, nil)
	case "anyOf":
		for _, s := range val.([]any) {
			if _, err := alwaysValid(s); err != nil {
				return err
			}
		}
		for _, s := range val.([]any) {
			if err := c.sub(s, ctx, false, nil); err != nil {
				return err
			}
		}
	case "oneOf", "allOf":
		for _, s := range val.([]any) {
			av, err := alwaysValid(s)
			if err != nil {
				return err
			}
			if av {
				continue
			}
			if err := c.sub(s, ctx, false, nil); err != nil {
				return err
			}
		}
	case "if":
		_, hasThen := o.Get("then")
		_, hasElse := o.Get("else")
		if !hasThen && !hasElse {
			return strictErr(`"if" without "then" and "else" is ignored`)
		}
		useThen, err := c.useClause(o, "then")
		if err != nil {
			return err
		}
		useElse, err := c.useClause(o, "else")
		if err != nil {
			return err
		}
		if !useThen && !useElse {
			return nil
		}
		if err := c.sub(val, ctx, false, nil); err != nil {
			return err
		}
		if useThen {
			if err := c.sub(o.Value("then"), ctx, false, nil); err != nil {
				return err
			}
		}
		if useElse {
			return c.sub(o.Value("else"), ctx, false, nil)
		}
	case "then", "else":
		if !o.Has("if") {
			return strictErr("%q without \"if\" is ignored", kw)
		}
	case "format":
		name, _ := val.(string)
		if _, ok := formats[name]; !ok {
			return compileErr("unknown format %q ignored in schema", name)
		}
	case "formatMaximum", "formatMinimum", "formatExclusiveMaximum", "formatExclusiveMinimum":
		if !o.Has("format") {
			return compileErr("parent schema must have dependencies of %s: format", kw)
		}
		name, _ := o.Value("format").(string)
		f := formats[name]
		if f == nil || f.validate == nil {
			return nil
		}
		if f.compare == nil {
			return compileErr("%q: format %q does not define \"compare\" function", kw, name)
		}
	case "pattern":
		p, _ := val.(string)
		if _, err := c.pattern(p); err != nil {
			return err
		}
	case "prefixItems":
		arr, _ := val.([]any)
		l := float64(len(arr))
		minItems, _ := numberValue(o.Value("minItems"))
		maxItems, hasMax := numberValue(o.Value("maxItems"))
		full := o.Has("minItems") && l == minItems && ((hasMax && l == maxItems) || o.Value("items") == false)
		if !full {
			return strictErr(`"prefixItems" is %d-tuple, but minItems or maxItems/items are not specified or different`, len(arr))
		}
		for _, s := range arr {
			av, err := alwaysValid(s)
			if err != nil {
				return err
			}
			if av {
				continue
			}
			if err := c.sub(s, ctx, true, nil); err != nil {
				return err
			}
		}
	case "items", "unevaluatedItems", "unevaluatedProperties", "additionalProperties":
		av, err := alwaysValid(val)
		if err != nil || av {
			return err
		}
		if kw == "additionalProperties" {
			if err := c.patternsOf(o); err != nil {
				return err
			}
		}
		if _, isObj := val.(*jsonv.Object); isObj {
			return c.sub(val, ctx, true, nil)
		}
	case "contains":
		minC, hasMin := numberValue(o.Value("minContains"))
		maxC, hasMax := numberValue(o.Value("maxContains"))
		if !hasMin {
			minC = 1
		}
		if !hasMax && minC == 0 {
			return strictErr(`"minContains" == 0 without "maxContains": "contains" keyword ignored`)
		}
		if hasMax && minC > maxC {
			return strictErr(`"minContains" > "maxContains" is always invalid`)
		}
		av, err := alwaysValid(val)
		if err != nil || av {
			return err
		}
		return c.sub(val, ctx, true, nil)
	case "maxContains", "minContains":
		if !o.Has("contains") {
			return strictErr("%q without \"contains\" is ignored", kw)
		}
	case "required":
		props := asObject(o.Value("properties"))
		for _, r := range val.([]any) {
			name, _ := r.(string)
			if !props.Has(name) && !ctx.defined.m[name] {
				return strictErr("required property %q is not defined (strictRequired)", name)
			}
		}
	case "propertyNames":
		av, err := alwaysValid(val)
		if err != nil || av {
			return err
		}
		return c.sub(val, ctx, true, []string{"string"})
	case "dependencies", "dependentSchemas":
		deps := asObject(val)
		for _, k := range deps.Keys() {
			d := deps.Value(k)
			if _, isArr := d.([]any); isArr && kw == "dependencies" {
				continue
			}
			if k == "__proto__" {
				continue
			}
			av, err := alwaysValid(d)
			if err != nil {
				return err
			}
			if av {
				continue
			}
			if err := c.sub(d, ctx, false, nil); err != nil {
				return err
			}
		}
	case "properties":
		props := asObject(val)
		keys := schemaProps(props)
		for _, k := range keys {
			ctx.defined.m[k] = true
		}
		for _, k := range keys {
			av, err := alwaysValid(props.Value(k))
			if err != nil {
				return err
			}
			if av {
				continue
			}
			if err := c.sub(props.Value(k), ctx, true, nil); err != nil {
				return err
			}
			ctx.defined.m[k] = true
		}
	case "patternProperties":
		pats := asObject(val)
		keys := schemaProps(pats)
		if len(keys) == 0 {
			return nil
		}
		allTrivial := true
		for _, pat := range keys {
			av, err := alwaysValid(pats.Value(pat))
			if err != nil {
				return err
			}
			allTrivial = allTrivial && av
		}
		if allTrivial && o.Has("additionalProperties") {
			// Ajv returns early once every property is already evaluated.
			return nil
		}
		if props := asObject(o.Value("properties")); props != nil {
			for _, pat := range keys {
				re, err := compileJSRegex(pat, "")
				if err != nil {
					if !errorsIsUnsupported(err) {
						return compileErr("invalid pattern %q", pat)
					}
					continue
				}
				for _, p := range props.Keys() {
					if re.MatchString(p) {
						return strictErr("property %s matches pattern %s (use allowMatchingProperties)", p, pat)
					}
				}
			}
		}
		if err := c.patternsOf(o); err != nil {
			return err
		}
		for _, pat := range keys {
			av, err := alwaysValid(pats.Value(pat))
			if err != nil {
				return err
			}
			if av {
				continue
			}
			if err := c.sub(pats.Value(pat), ctx, true, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *compiler) patternsOf(o *jsonv.Object) error {
	pats := asObject(o.Value("patternProperties"))
	for _, p := range schemaProps(pats) {
		if _, err := c.pattern(p); err != nil {
			return err
		}
	}
	return nil
}

func (c *compiler) useClause(o *jsonv.Object, kw string) (bool, error) {
	v, ok := o.Get(kw)
	if !ok {
		return false, nil
	}
	av, err := alwaysValid(v)
	return !av, err
}

// schemaProps is Ajv's allSchemaProperties: keys without "__proto__".
func schemaProps(o *jsonv.Object) []string {
	var out []string
	for _, k := range o.Keys() {
		if k != "__proto__" {
			out = append(out, k)
		}
	}
	return out
}

func refKey(base, ref string) string { return base + "\x00" + ref }

func (c *compiler) ref(o *jsonv.Object, ctx *cctx) error {
	ref, _ := o.Value("$ref").(string)
	t, err := c.v.resolveRef(ctx.base, ref)
	if err != nil {
		return err
	}
	c.v.refs[refKey(ctx.base, ref)] = t
	if t.root {
		return nil
	}
	if t.inline {
		// Same data, dataTypes reset, definedProperties shared.
		ch := *ctx
		ch.dataTypes = nil
		ch.base = t.base
		return c.node(t.node, &ch)
	}
	return c.env(t)
}

// env compiles a separately compiled schema (Ajv's SchemaEnv) once.
func (c *compiler) env(t *refTarget) error {
	key := fmt.Sprintf("%p|%s", t.node, t.base)
	if c.envDone[key] {
		return nil
	}
	c.envDone[key] = true
	return c.node(t.node, &cctx{defined: newStrSet(), base: t.base})
}

func (c *compiler) dynamicTarget(ref string, ctx *cctx) error {
	t := c.v.dynamicRef(ctx.base, ref)
	c.v.refs[refKey(ctx.base, "dyn:"+ref)] = t
	if t.root {
		return nil
	}
	return c.env(t)
}

func (v *Validator) dynamicRef(base, ref string) *refTarget {
	if a, ok := v.anchors[base+ref]; ok {
		return &refTarget{node: a, base: base}
	}
	return &refTarget{node: v.root, base: v.rootBase, root: true}
}

// maxRefHops bounds how many reference-only schemas resolveRef follows in a
// chain, so a chain that loops ({"a":{"$ref":"#/$defs/b"}},
// {"b":{"$ref":"#/$defs/a"}}) ends instead of recursing until the stack
// overflows. checkRefCycles then refuses the schema.
const maxRefHops = 64

// resolveRef is Ajv's resolveRef for same-document references.
func (v *Validator) resolveRef(base, ref string) (*refTarget, error) {
	return v.resolveRefHops(base, ref, 0)
}

func (v *Validator) resolveRefHops(base, ref string, hops int) (*refTarget, error) {
	if hops > maxRefHops {
		return nil, compileErr("reference chain is too long or loops: %s", ref)
	}
	if (ref == "#" || ref == "#/" || ref == "") && base == v.rootBase {
		return &refTarget{node: v.root, base: v.rootBase, root: true}, nil
	}
	full := resolveURI(base, ref)
	doc, frag, _ := splitFragment(full)
	var resRoot any
	resBase := normalizeID(doc)
	if resBase == v.rootBase {
		resRoot = v.root
	} else if r, ok := v.resources[resBase]; ok {
		resRoot = r
	}
	if resRoot == nil {
		return nil, compileErr("can't resolve reference %s from id %s", ref, base)
	}
	var target any
	targetBase := resBase
	if strings.HasPrefix(frag, "/") {
		cur := resRoot
		for _, part := range strings.Split(frag[1:], "/") {
			o, ok := cur.(*jsonv.Object)
			if !ok {
				if arr, isArr := cur.([]any); isArr {
					i, err := strconv.Atoi(unescapeFragment(part))
					if err != nil || i < 0 || i >= len(arr) {
						cur = nil
						break
					}
					cur = arr[i]
					continue
				}
				cur = nil
				break
			}
			next, present := o.Get(unescapeFragment(part))
			if !present {
				cur = nil
				break
			}
			cur = next
			if no, ok := cur.(*jsonv.Object); ok {
				if id, ok := no.Value("$id").(string); ok && part != "properties" && part != "patternProperties" && part != "$defs" && part != "definitions" && part != "dependentSchemas" {
					targetBase = normalizeID(resolveURI(targetBase, id))
				}
			}
		}
		target = cur
		if o, ok := target.(*jsonv.Object); ok && o.Has("$ref") && !hasRulesButRef(o) {
			inner, _ := o.Value("$ref").(string)
			if t, err := v.resolveRefHops(targetBase, inner, hops+1); err == nil && !t.root {
				target, targetBase = t.node, t.base
			}
		}
		if target == resRoot && resRoot == any(v.root) {
			target = nil
		}
	} else if a, ok := v.anchors[resBase+"#"+frag]; ok && frag != "" {
		target = a
	} else if frag == "" && resRoot != any(v.root) {
		target = resRoot
	}
	if target == nil {
		return nil, compileErr("can't resolve reference %s from id %s", ref, base)
	}
	inline := true
	if _, isBool := target.(bool); !isBool {
		inline = !hasRef(target)
	}
	return &refTarget{node: target, base: targetBase, inline: inline}, nil
}
