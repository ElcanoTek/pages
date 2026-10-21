// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package nodedefaults records the size settings the Node release ran with.
// The Go server deliberately raised them (see internal/datalimits,
// internal/uploads, internal/pagedata); golden tests recorded from the
// JavaScript at its defaults pin these values so they keep comparing like
// with like instead of being re-recorded.
package nodedefaults

import "testing"

// Env is every environment-backed size bound at its Node release default.
var Env = map[string]string{
	"MAX_HTML_BYTES":                  "2mb",
	"PAGES_UPLOAD_MAX_BYTES":          "2097152",
	"PAGE_UPLOAD_MAX_CHUNK_BYTES":     "49152",
	"PAGE_UPLOAD_TICKET_TTL_MINUTES":  "15",
	"PAGES_DATA_MAX_BYTES":            "1048576",
	"PAGES_MCP_MAX_INLINE_DATA_BYTES": "1500000",
	"PAGES_DATA_TEMPLATE_MAX_BYTES":   "2097152",
	"PAGES_DATA_SCHEMA_MAX_BYTES":     "262144",
	"PAGES_DATA_CONFIG_MAX_BYTES":     "262144",
	"PAGES_DATA_SCHEMA_MAX_NODES":     "10000",
}

// Setenv applies Env for the duration of t.
func Setenv(t testing.TB) {
	t.Helper()
	for k, v := range Env {
		t.Setenv(k, v)
	}
}
