// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package nodebuf

import (
	"strings"
	"testing"
)

// Expectations recorded from Node 22: Buffer.from(s, "base64").toString("hex").
func TestBase64Decode(t *testing.T) {
	for in, want := range map[string]string{
		"aGFzaA==":    "68617368",
		"aGFzaA":      "68617368",
		"aGFz aA==":   "68617368",
		"-_-_":        "fbffbf",
		"+/+/":        "fbffbf",
		"aGF=zaA":     "6861",
		"YQ":          "61",
		"Y":           "",
		"":            "",
		"!!aGFzaA!!=": "68617368",
		"YR":          "61", // non-zero trailing bits are ignored
	} {
		if got := hexOf(Base64Decode(in)); got != want {
			t.Errorf("Base64Decode(%q) = %s, want %s", in, got, want)
		}
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	var s strings.Builder
	for _, c := range b {
		s.WriteByte(digits[c>>4])
		s.WriteByte(digits[c&15])
	}
	return s.String()
}
