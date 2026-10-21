// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// Ajv validates every schema against the draft 2020-12 meta-schema before it
// compiles it ("schema is invalid: data/... must be number"). That check is
// delegated to santhosh-tekuri/jsonschema, validating the schema document as an
// instance of the official meta-schema. Formats in the meta-schema stay
// annotations, as they are for Ajv (it accepts "$ref": "#/$defs/a b").

const metaSchemaURL = "https://json-schema.org/draft/2020-12/schema"

var (
	metaOnce   sync.Once
	metaSchema *jsonschema.Schema
	errMeta    error
)

func loadMeta() (*jsonschema.Schema, error) {
	metaOnce.Do(func() {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		metaSchema, errMeta = c.Compile(metaSchemaURL)
	})
	return metaSchema, errMeta
}

// toPlain converts the value model into the shapes jsonschema/v6 reads.
func toPlain(v any) any {
	switch x := v.(type) {
	case *jsonv.Object:
		m := make(map[string]any, x.Len())
		for _, k := range x.Keys() {
			m[k] = toPlain(x.Value(k))
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = toPlain(e)
		}
		return out
	case float64:
		return json.Number(jsonv.FormatNumber(x))
	}
	return v
}

// validateMeta reports whether schema is a valid draft 2020-12 document.
func validateMeta(schema *jsonv.Object) error {
	meta, err := loadMeta()
	if err != nil {
		return fmt.Errorf("pagedata: loading the 2020-12 meta-schema: %w", err)
	}
	if err := meta.Validate(toPlain(schema)); err != nil {
		return fmt.Errorf("%w: schema is invalid: %w", errCompile, err)
	}
	return nil
}
