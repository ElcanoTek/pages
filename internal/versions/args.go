// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package versions

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
)

// Loosely typed inputs.
//
// The REST and admin surfaces hand the state machine whatever a client put in
// a JSON body, and the JavaScript implementation validated those values here —
// so the 400s ("publish must be a boolean", "bad render_mode: 5", "note must be
// a string or null") are part of this package's contract. Fields typed `any`
// accept the raw decoded JSON value (string, bool, float64, nil, …) or an
// ordinary Go value (string, bool, int, int64, jsx.ID).
//
// JavaScript also told an omitted key from an explicit null in a few places (a
// null boolean is a 400; an omitted one takes its default). Go nil means
// "omitted"; pass Null for an explicit JSON null. Arg builds the right value
// from a decoded object lookup.

type nullValue struct{}

// MarshalJSON renders the sentinel as JSON null.
func (nullValue) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// Null is an explicit JSON null (JavaScript null, as opposed to undefined).
var Null any = nullValue{}

// Arg converts an object lookup (value, present) into this package's input
// convention: absent ⇒ nil, present null ⇒ Null, else the value.
func Arg(v any, present bool) any {
	if !present {
		return nil
	}
	if v == nil {
		return Null
	}
	return v
}

func isNullish(v any) bool { return v == nil || v == Null }

// jsString is JavaScript's String(v) for the values a caller can send.
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "undefined"
	case nullValue:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return jsonv.FormatNumber(x)
	case float32:
		return jsonv.FormatNumber(float64(x))
	case int:
		return strconv.Itoa(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case jsx.ID:
		if x == 0 {
			return "null"
		}
		return x.String()
	case *jsx.ID:
		if x == nil {
			return "null"
		}
		return jsString(*x)
	case json.Number:
		f, err := strconv.ParseFloat(x.String(), 64)
		if err != nil {
			return x.String()
		}
		return jsonv.FormatNumber(f)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if !isNullish(e) {
				parts[i] = jsString(e)
			}
		}
		return strings.Join(parts, ",")
	case []string:
		return strings.Join(x, ",")
	}
	return "[object Object]"
}

// sameDBID is the JavaScript String(a) === String(b) id comparison, with
// null/undefined never equal to anything.
func sameDBID(a any, b jsx.ID) bool {
	return !isNullish(a) && b != 0 && jsString(a) == b.String()
}

// pgText converts a loosely typed value to the text node-postgres would have
// bound for it. ok=false means SQL NULL.
func pgText(v any) (string, bool) {
	switch x := v.(type) {
	case nil, nullValue:
		return "", false
	case string:
		return x, true
	case bool, float64, float32, int, int32, int64, uint64, jsx.ID, json.Number:
		return jsString(x), true
	}
	b, err := jsonv.Marshal(v)
	if err != nil {
		return jsString(v), true
	}
	return b, true
}

// jsTruthy is JavaScript truthiness for decoded values.
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil, nullValue:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0 && !math.IsNaN(x)
	case int:
		return x != 0
	case int64:
		return x != 0
	case jsx.ID:
		return x != 0
	}
	return true
}

func boolArg(v any, def bool, field string) (bool, error) {
	if v == nil {
		return def, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, apierror.BadRequest(field+" must be a boolean", "bad_boolean", nil)
	}
	return b, nil
}

// ── slugs, notes, ids ───────────────────────────────────────────────────────

var slugRE = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*(?:/[a-z0-9]+(?:[-_][a-z0-9]+)*)*$`)

// isJSSpace is JavaScript's String#trim whitespace set.
func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func jsTrim(s string) string { return strings.TrimFunc(s, isJSSpace) }

// NormalizeSlug validates and canonicalizes a page slug: flat ('northwind') or
// nested ('northwind/q2-report'), url-safe segments only, trimmed and
// lowercased. A non-string is "slug is required" (400 slug_required).
func NormalizeSlug(slug any) (string, error) {
	s, ok := slug.(string)
	if !ok {
		return "", apierror.BadRequest("slug is required", "slug_required", nil)
	}
	s = jsTrim(s)
	// JavaScript lowercases U+0130 to "i̇" (two code points), which the pattern
	// then rejects; Go's simple case mapping would make it a plain "i".
	if strings.ContainsRune(s, 'İ') {
		return "", badSlug()
	}
	s = strings.Map(unicode.ToLower, s)
	if !slugRE.MatchString(s) {
		return "", badSlug()
	}
	return s, nil
}

func badSlug() error {
	return apierror.BadRequest("slug must be url-safe (a-z 0-9 - _ /)", "bad_slug", nil)
}

const maxSafeInteger = 1<<53 - 1

// WorkspaceRef is a normalized workspace id. JavaScript normalized a workspace
// id to a Number when it was a safe integer and kept the decimal string
// otherwise, and that value reached responses (assignPage) and audit metadata,
// so it marshals the same way: a JSON number up to 2^53-1, else a string.
type WorkspaceRef int64

// MarshalJSON implements json.Marshaler (0 ⇒ null).
func (w WorkspaceRef) MarshalJSON() ([]byte, error) {
	if w == 0 {
		return []byte("null"), nil
	}
	s := strconv.FormatInt(int64(w), 10)
	if w <= maxSafeInteger {
		return []byte(s), nil
	}
	return []byte(`"` + s + `"`), nil
}

// ID is the reference as a jsx.ID.
func (w WorkspaceRef) ID() jsx.ID { return jsx.ID(w) }

var decimalIDRE = regexp.MustCompile(`^[1-9][0-9]*$`)

// parsePositiveID accepts a positive safe-integer number or a decimal string
// within BIGINT range, as normalizeWorkspaceId did.
func parsePositiveID(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		if x > 0 && x == math.Trunc(x) && x <= maxSafeInteger {
			return int64(x), true
		}
	case int:
		if x > 0 && int64(x) <= maxSafeInteger {
			return int64(x), true
		}
	case int64:
		if x > 0 && x <= maxSafeInteger {
			return x, true
		}
	case jsx.ID:
		if x > 0 {
			return int64(x), true
		}
	case WorkspaceRef:
		if x > 0 {
			return int64(x), true
		}
	case string:
		if decimalIDRE.MatchString(x) {
			n, err := strconv.ParseInt(x, 10, 64)
			if err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// NormalizeWorkspaceID validates a workspace id (a positive safe integer or a
// decimal string within BIGINT range): 400 bad_workspace_id otherwise.
func NormalizeWorkspaceID(v any) (WorkspaceRef, error) {
	if id, ok := parsePositiveID(v); ok {
		return WorkspaceRef(id), nil
	}
	return 0, apierror.BadRequest("workspace_id must be a positive database id", "bad_workspace_id", nil)
}

// normalizeNote bounds a note: nil/Null ⇒ none, else a string of at most 500
// UTF-16 units.
func normalizeNote(v any) (*string, error) {
	if isNullish(v) {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, apierror.BadRequest("note must be a string or null", "bad_note", nil)
	}
	if jsstr.UTF16Len(s) > 500 {
		return nil, apierror.BadRequest("note must be 500 characters or fewer", "note_too_long", nil)
	}
	return &s, nil
}
