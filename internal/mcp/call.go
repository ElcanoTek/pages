// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// What a tool handler works with: the authenticated actor, the database, and
// small helpers that read validated arguments and build results with
// JavaScript object semantics.

import (
	"fmt"
	"log/slog"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

// call is one tool invocation's context (lib/mcp.js actorContext): the
// authenticated agent, its audit identity, and the request id for logs.
type call struct {
	d         *db.DB
	agent     *tokens.Agent
	actor     versions.Actor
	requestID string
	logger    *slog.Logger
}

// ── arguments ───────────────────────────────────────────────────────────────
//
// Arguments have passed the tool's input schema, so their types are known;
// these helpers only distinguish present from absent.

// argStr returns a string argument and whether it was present.
func argStr(a *jsonv.Object, key string) (string, bool) {
	v, ok := a.Get(key)
	if !ok {
		return "", false
	}
	s, isStr := v.(string)
	return s, isStr
}

// argStrPtr is a present string argument, or nil.
func argStrPtr(a *jsonv.Object, key string) *string {
	if s, ok := argStr(a, key); ok {
		return &s
	}
	return nil
}

// argBool returns a boolean argument and whether it was present.
func argBool(a *jsonv.Object, key string) (bool, bool) {
	v, ok := a.Get(key)
	if !ok {
		return false, false
	}
	b, isBool := v.(bool)
	return b, isBool
}

// argBoolOr is a boolean argument, or def when absent.
func argBoolOr(a *jsonv.Object, key string, def bool) bool {
	if b, ok := argBool(a, key); ok {
		return b
	}
	return def
}

// argInt is an integer argument (schema-validated as a safe integer).
func argInt(a *jsonv.Object, key string) (int, bool) {
	v, ok := a.Get(key)
	if !ok {
		return 0, false
	}
	n, isNum := v.(float64)
	return int(n), isNum
}

func argIntPtr(a *jsonv.Object, key string) *int {
	if n, ok := argInt(a, key); ok {
		return &n
	}
	return nil
}

// argAny is the versions input convention for an argument: absent ⇒ nil,
// null ⇒ versions.Null, else the value.
func argAny(a *jsonv.Object, key string) any {
	v, ok := a.Get(key)
	return versions.Arg(v, ok)
}

// argID is an id argument (the schema transformed it to a decimal string).
func argID(a *jsonv.Object, key string) (jsx.ID, bool) {
	s, ok := argStr(a, key)
	if !ok {
		return 0, false
	}
	id, err := jsx.ParseID(s)
	if err != nil {
		return 0, false
	}
	return id, true
}

// truthy is JavaScript truthiness for a parsed argument value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	}
	return true
}

// ── results ─────────────────────────────────────────────────────────────────

// toObject converts a result (a struct, *jsonv.Object…) to an ordered
// object: JSON.parse(JSON.stringify(v)).
func toObject(v any) (*jsonv.Object, error) {
	s, err := jsonv.Marshal(v)
	if err != nil {
		return nil, err
	}
	parsed, err := jsonv.ParseLossless([]byte(s))
	if err != nil {
		return nil, err
	}
	o, ok := parsed.(*jsonv.Object)
	if !ok {
		return nil, fmt.Errorf("mcp: result is %s, not an object", jsonv.TypeName(parsed))
	}
	return o, nil
}

// toValue is JSON.parse(JSON.stringify(v)) for any value.
func toValue(v any) (any, error) {
	s, err := jsonv.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jsonv.ParseLossless([]byte(s))
}

// spread is JavaScript's {...a, ...b, ...}: later keys overwrite earlier
// values in the earlier position; new keys are appended.
func spread(parts ...any) (*jsonv.Object, error) {
	out := jsonv.NewObject()
	for _, p := range parts {
		if p == nil {
			continue
		}
		o, ok := p.(*jsonv.Object)
		if !ok {
			var err error
			if o, err = toObject(p); err != nil {
				return nil, err
			}
		}
		o.Range(func(k string, v any) bool {
			out.Set(k, v)
			return true
		})
	}
	return out, nil
}

// without is {key: _, ...rest} = o.
func without(o *jsonv.Object, keys ...string) *jsonv.Object {
	c := o.Clone()
	for _, k := range keys {
		c.Delete(k)
	}
	return c
}

// sameID is lib/mcp-tools.js compareIds: both present and String(a) ===
// String(b).
func sameID(a, b jsx.ID) bool { return a != 0 && b != 0 && a == b }

// idOrNull renders a nullable id the way results carry it.
func idOrNull(id jsx.ID) any {
	if id == 0 {
		return nil
	}
	return id.String()
}
