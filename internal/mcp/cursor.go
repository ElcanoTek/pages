// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcp

// Keyset pagination cursors. A cursor is base64url JSON naming the list it
// belongs to, the row to continue after, and a hash of the filters it was
// issued under, so it cannot be replayed against a different query. The
// format is the JavaScript's, so a cursor issued before the Go migration
// still decodes.

import (
	"crypto/sha256"
	"encoding/base64"
	"regexp"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/nodebuf"
)

func cursorHash(filters *jsonv.Object) string {
	sum := sha256.Sum256([]byte(jsonv.Stringify(filters)))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:22]
}

// EncodeCursor issues a cursor for kind continuing after the given row.
func encodeCursor(kind string, after *jsonv.Object, filters *jsonv.Object) string {
	payload := jsonv.ObjectOf("v", float64(1), "kind", kind, "after", after, "filters", cursorHash(filters))
	return base64.RawURLEncoding.EncodeToString([]byte(jsonv.Stringify(payload)))
}

var cursorRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// decodeCursor returns the cursor's row (nil for no cursor), or 400
// invalid_cursor when it is malformed, for another list, or for other
// filters.
func decodeCursor(encoded, kind string, filters *jsonv.Object) (*jsonv.Object, error) {
	if encoded == "" {
		return nil, nil
	}
	bad := apierror.BadRequest("invalid or stale pagination cursor", "invalid_cursor", nil)
	if !cursorRE.MatchString(encoded) || len(encoded) > 2048 {
		return nil, bad
	}
	parsed, err := jsonv.ParseLossless([]byte(jsonv.DecodeUTF8(nodebuf.Base64Decode(encoded))))
	if err != nil {
		return nil, bad
	}
	o, ok := parsed.(*jsonv.Object)
	if !ok || o.Value("v") != float64(1) || o.Value("kind") != kind || o.Value("filters") != cursorHash(filters) {
		return nil, bad
	}
	// typeof after === "object": an array passes too, as in JavaScript, and
	// is then refused by the caller's shape check.
	switch after := o.Value("after").(type) {
	case *jsonv.Object:
		return after, nil
	case []any:
		return jsonv.NewObject(), nil
	}
	return nil, bad
}
