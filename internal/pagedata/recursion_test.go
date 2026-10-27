// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

const draft2020 = `"$schema":"https://json-schema.org/draft/2020-12/schema"`

// A schema whose references lead back to a schema already being applied to
// the same data used to compile and then recurse at validation (or, for a
// chain of reference-only schemas, at compilation) until the goroutine stack
// overflowed: a fatal error recover() cannot catch, so one stored schema took
// the whole process down. They are refused when the schema is accepted.
func TestReferenceCyclesAreRefusedNotFatal(t *testing.T) {
	for name, s := range map[string]string{
		"ref-only self":       `{` + draft2020 + `,"type":"object","$defs":{"a":{"$ref":"#/$defs/a"}},"properties":{"x":{"$ref":"#/$defs/a"}}}`,
		"ref-only pair":       `{` + draft2020 + `,"type":"object","$defs":{"a":{"$ref":"#/$defs/b"},"b":{"$ref":"#/$defs/a"}},"properties":{"x":{"$ref":"#/$defs/a"}}}`,
		"allOf self":          `{` + draft2020 + `,"type":"object","$defs":{"a":{"allOf":[{"$ref":"#/$defs/a"}]}},"properties":{"x":{"$ref":"#/$defs/a"}}}`,
		"anyOf pair":          `{` + draft2020 + `,"type":"object","$defs":{"a":{"anyOf":[{"type":"string"},{"$ref":"#/$defs/b"}]},"b":{"not":{"$ref":"#/$defs/a"}}},"properties":{"x":{"$ref":"#/$defs/a"}}}`,
		"root allOf":          `{` + draft2020 + `,"type":"object","allOf":[{"$ref":"#"}]}`,
		"root if/then":        `{` + draft2020 + `,"type":"object","if":{"$ref":"#"},"then":{"required":[]}}`,
		"dependentSchemas":    `{` + draft2020 + `,"type":"object","properties":{"k":{"type":"string"}},"dependentSchemas":{"k":{"$ref":"#"}}}`,
		"long ref-only chain": refChain(200),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileSchema(mustParse(t, s), "data")
			if code(err) != "data_contract_invalid" {
				t.Fatalf("want data_contract_invalid, got %v", err)
			}
		})
	}
}

func refChain(n int) string {
	var defs []string
	for i := range n {
		defs = append(defs, fmt.Sprintf(`"d%d":{"$ref":"#/$defs/d%d"}`, i, (i+1)%n))
	}
	return `{` + draft2020 + `,"type":"object","$defs":{` + strings.Join(defs, ",") + `},"properties":{"x":{"$ref":"#/$defs/d0"}}}`
}

// Recursion that descends into the data is how trees are described, and
// stays valid: each step consumes a level of the document.
func TestRecursiveSchemasThatDescendStillValidate(t *testing.T) {
	v, err := CompileSchema(mustParse(t, `{`+draft2020+`,"type":"object","$defs":{"node":{"type":"object","properties":{"name":{"type":"string"},"children":{"type":"array","items":{"$ref":"#/$defs/node"}}},"required":["name"]}},"properties":{"root":{"$ref":"#/$defs/node"}}}`), "data")
	if err != nil {
		t.Fatalf("tree schema: %v", err)
	}
	deep := `{"name":"leaf"}`
	for range 2000 {
		deep = `{"name":"n","children":[` + deep + `]}`
	}
	if errs := v.Validate(mustParse(t, `{"root":`+deep+`}`)); len(errs) != 0 {
		t.Fatalf("deep valid tree refused: %v", errs[0])
	}
	bad := strings.Replace(deep, `{"name":"leaf"}`, `{"name":1}`, 1)
	if errs := v.Validate(mustParse(t, `{"root":`+bad+`}`)); len(errs) == 0 {
		t.Fatal("a wrong leaf deep in the tree must still be reported")
	}
	// A root self-reference under "properties" descends too.
	if _, err := CompileSchema(mustParse(t, `{`+draft2020+`,"type":"object","properties":{"next":{"$ref":"#"}}}`), "data"); err != nil {
		t.Fatalf("linked-list schema: %v", err)
	}
}

// The validator's depth backstop: even a cycle that slipped past compilation
// ends in a refusal, never a stack overflow, and an applicator that discards
// its branches' errors ("not") cannot turn the abandoned walk into a pass.
func TestValidationDepthBackstop(t *testing.T) {
	for _, s := range []string{
		`{` + draft2020 + `,"type":"object","allOf":[{"$ref":"#"}]}`,
		`{` + draft2020 + `,"type":"object","not":{"not":{"allOf":[{"$ref":"#"}]}}}`,
	} {
		root := mustParse(t, s).(*jsonv.Object)
		// Compile without checkRefCycles, as a schema accepted before the
		// check existed would have been.
		v := newValidator(root)
		v.collectIDs(root, v.rootBase)
		c := &compiler{v: v, envDone: map[string]bool{}}
		if err := c.node(root, &cctx{defined: newStrSet(), base: v.rootBase}); err != nil {
			t.Fatalf("compile: %v", err)
		}
		errs := v.Validate(mustParse(t, `{}`))
		if len(errs) != 1 || errs[0].Message != "schema recursion is too deep" {
			t.Fatalf("want the depth refusal, got %v", errs)
		}
	}
}

// uniqueItemsQuadratic is Ajv's general-case comparison, kept as the
// reference the linear check must agree with.
func uniqueItemsQuadratic(arr []any) (int, int, bool) {
	for i := len(arr) - 1; i > 0; i-- {
		for j := i - 1; j >= 0; j-- {
			if jsonv.Equal(arr[i], arr[j]) {
				return i, j, true
			}
		}
	}
	return 0, 0, false
}

// uniqueItems on arrays of objects used to compare every pair inside the
// managed-data write. It is linear now and must report exactly the pair Ajv
// reported: the largest i with an equal earlier item, and the largest such j.
func TestUniqueItemsIsLinearAndReportsAjvsPair(t *testing.T) {
	v, err := CompileSchema(mustParse(t, `{`+draft2020+`,"type":"object","properties":{"a":{"type":"array","uniqueItems":true}}}`), "data")
	if err != nil {
		t.Fatal(err)
	}
	for _, arr := range []string{
		`[]`, `[1]`, `[1,2,3]`, `[1,"1",true,null,[1],{"1":1}]`,
		`[1,2,1]`, `[1,1,2,2]`, `[2,1,2,1,3]`, `[0,-0]`, `[1e0,1]`,
		`[{"a":1,"b":2},{"b":2,"a":1}]`, `[{"a":1,"b":2},{"a":2,"b":1}]`,
		`[{"a":[1,{"x":null}]},{"a":[1,{"x":false}]},{"a":[1,{"x":null}]}]`,
		`[[1,2],[2,1],[1,2],[2,1]]`, `[{},[],{},[]]`, `["a","b","c","b","a"]`,
		`[{"k":"a"},{"k":"b"},{"k":"a"},{"k":"c"},{"k":"b"}]`,
	} {
		data := mustParse(t, `{"a":`+arr+`}`)
		errs := v.Validate(data)
		i, j, dup := uniqueItemsQuadratic(data.(*jsonv.Object).Value("a").([]any))
		if !dup {
			if len(errs) != 0 {
				t.Fatalf("%s: unexpected %v", arr, errs)
			}
			continue
		}
		want := fmt.Sprintf("must NOT have duplicate items (items ## %d and %d are identical)", j, i)
		if len(errs) != 1 || errs[0].Message != want {
			t.Fatalf("%s: want %q, got %v", arr, want, errs)
		}
	}

	// 200k distinct objects: quadratic pairwise comparison would be ~2e10
	// Equal calls; the linear check finishes in well under a second.
	var b strings.Builder
	b.WriteString(`{"a":[`)
	for i := range 200000 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"v":"x"}`, i)
	}
	b.WriteString(`,{"v":"x","id":7}]}`)
	errs := v.Validate(mustParse(t, b.String()))
	if len(errs) != 1 || !strings.Contains(errs[0].Message, "items ## 7 and 200000 are identical") {
		t.Fatalf("large array: %v", errs)
	}
}
