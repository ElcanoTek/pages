// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package nodebuf reproduces the Node.js Buffer behaviour the signed-credential
// formats depend on (lenient base64 decoding), so a value the Node
// implementation produced (or accepted) is read exactly the same way here.
// JavaScript string semantics (UTF-16 lengths and slices) live in jsstr.
package nodebuf

// Base64Decode is Node's Buffer.from(s, "base64") — and "base64url", which
// Node decodes identically. It never fails: both alphabets are accepted,
// characters outside them are skipped, decoding stops at the first '=', and a
// trailing partial group yields the bytes it completely encodes.
func Base64Decode(s string) []byte {
	clean := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
			clean = append(clean, c)
		case c == '-':
			clean = append(clean, '+')
		case c == '_':
			clean = append(clean, '/')
		case c == '=':
			i = len(s)
		}
	}
	return decode(clean)
}

// decode decodes the cleaned standard alphabet. Like Node it ignores the
// unused low bits of a trailing partial group, and a lone trailing character
// (six bits) completes no byte.
func decode(clean []byte) []byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var val [256]byte
	for i := 0; i < len(alphabet); i++ {
		val[alphabet[i]] = byte(i)
	}
	out := make([]byte, 0, len(clean)*3/4)
	var acc uint32
	bits := 0
	for _, c := range clean {
		acc = acc<<6 | uint32(val[c])
		bits += 6
		if bits >= 8 {
			bits -= 8
			out = append(out, byte(acc>>uint(bits)&0xff))
		}
	}
	return out
}
