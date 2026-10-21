// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package jsx holds the JSON value shapes Pages' wire contract inherited from
// its first implementation (Node + node-postgres), so the Go server answers
// byte-for-byte as clients, the admin UI and the recorded MCP golden expect:
//
//   - BIGINT ids travel as decimal strings ("12"), never JSON numbers.
//   - Timestamps travel as UTC ISO-8601 with exactly three fractional digits
//     ("2026-07-22T09:30:00.000Z"), the shape of JavaScript's Date#toJSON.
//
// Use these types in any struct that is serialized onto the wire.
package jsx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TimeLayout is JavaScript's Date#toJSON layout (always UTC, milliseconds).
const TimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders t exactly as JavaScript's Date#toJSON would.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// Time is a nullable timestamp that marshals as Date#toJSON, or null when
// zero. It scans from timestamptz/timestamp columns, NULL included.
type Time struct{ time.Time }

// NewTime wraps t.
func NewTime(t time.Time) Time { return Time{t} }

// MarshalJSON implements json.Marshaler.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return []byte(`"` + FormatTime(t.Time) + `"`), nil
}

// UnmarshalJSON accepts null or any RFC 3339 string.
func (t *Time) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		t.Time = time.Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	t.Time = v
	return nil
}

// ScanTimestamptz implements pgtype.TimestamptzScanner.
func (t *Time) ScanTimestamptz(v pgtype.Timestamptz) error {
	if !v.Valid {
		t.Time = time.Time{}
		return nil
	}
	t.Time = v.Time
	return nil
}

// TimestamptzValue implements pgtype.TimestamptzValuer.
func (t Time) TimestamptzValue() (pgtype.Timestamptz, error) {
	if t.IsZero() {
		return pgtype.Timestamptz{}, nil
	}
	return pgtype.Timestamptz{Time: t.Time, Valid: true}, nil
}

// ID is a BIGINT key that marshals as a decimal string, the node-postgres
// convention every Pages client was built against. The zero value is "not
// set" and marshals as null — Pages ids start at 1.
type ID int64

// MarshalJSON implements json.Marshaler.
func (id ID) MarshalJSON() ([]byte, error) {
	if id == 0 {
		return []byte("null"), nil
	}
	return []byte(`"` + strconv.FormatInt(int64(id), 10) + `"`), nil
}

// UnmarshalJSON accepts a decimal string, a JSON integer, or null.
func (id *ID) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		*id = 0
		return nil
	}
	s := string(b)
	if len(b) > 1 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return fmt.Errorf("invalid id %s", b)
	}
	*id = ID(n)
	return nil
}

// String renders the id in decimal ("" when unset).
func (id ID) String() string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(int64(id), 10)
}

// ScanInt64 implements pgtype.Int64Scanner (NULL scans as 0).
func (id *ID) ScanInt64(v pgtype.Int8) error {
	if !v.Valid {
		*id = 0
		return nil
	}
	*id = ID(v.Int64)
	return nil
}

// Int64Value implements pgtype.Int64Valuer (0 binds as NULL).
func (id ID) Int64Value() (pgtype.Int8, error) {
	if id == 0 {
		return pgtype.Int8{}, nil
	}
	return pgtype.Int8{Int64: int64(id), Valid: true}, nil
}

// ParseID parses a decimal id as a client sends it ("12" or 12). It rejects
// anything that is not a positive base-10 integer that fits in a BIGINT.
func ParseID(v any) (ID, error) {
	switch x := v.(type) {
	case ID:
		if x > 0 {
			return x, nil
		}
	case string:
		if n, err := strconv.ParseInt(x, 10, 64); err == nil && n > 0 && strconv.FormatInt(n, 10) == x {
			return ID(n), nil
		}
	case float64:
		if x > 0 && x == float64(int64(x)) && x <= 9007199254740991 {
			return ID(int64(x)), nil
		}
	case json.Number:
		return ParseID(string(x))
	case int:
		if x > 0 {
			return ID(x), nil
		}
	case int64:
		if x > 0 {
			return ID(x), nil
		}
	}
	return 0, errors.New("not a valid id")
}
