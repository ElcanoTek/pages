// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package httpx is the one home for the Node/Express request and response
// behaviour Pages has to reproduce byte for byte (every surface uses it; do
// not write another res.json):
//
//   - responses (response.go): res.send / res.json / res.redirect with
//     Express's weak ETag and freshness check — Send, SendHTML, SendJSON,
//     WriteJSON, Redirect, ETag, Marshal;
//   - requests (this file): the cookie jar parser lib/auth.js shipped, Node's
//     querystring parser (Express 5's "simple" query parser), req.get-style
//     header joining, req.originalUrl;
//   - URLs: encodeURIComponent / decodeURIComponent, URLSearchParams
//     serialization, and encodeurl for Location headers.
//
// It depends on nothing heavier than jsstr, so packages that only need
// ParseCookies (the content host's page sessions, the rate limiter's key) can
// import it without pulling in the database.
package httpx

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/pages/internal/jsstr"
)

const upperHex = "0123456789ABCDEF"

func ishex(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func percentEncode(b *strings.Builder, s string, keep func(byte) bool) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if keep(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&15])
	}
}

func isAlnum(c byte) bool {
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9')
}

// EncodeURIComponent is JavaScript's encodeURIComponent (invalid UTF-8 is
// first replaced with U+FFFD, the closest a Go string gets to a lone
// surrogate, which JavaScript would refuse).
func EncodeURIComponent(s string) string {
	s = strings.ToValidUTF8(s, "�")
	var b strings.Builder
	percentEncode(&b, s, func(c byte) bool {
		return isAlnum(c) || strings.IndexByte("-_.!~*'()", c) >= 0
	})
	return b.String()
}

func encodeURI(s string) string {
	var b strings.Builder
	percentEncode(&b, s, func(c byte) bool {
		return isAlnum(c) || strings.IndexByte(";,/?:@&=+$-_.!~*'()#", c) >= 0
	})
	return b.String()
}

// DecodeURIComponent is JavaScript's decodeURIComponent: an error for a
// malformed escape or escaped bytes that are not UTF-8 (URIError).
func DecodeURIComponent(s string) (string, error) {
	if strings.IndexByte(s, '%') < 0 {
		return s, nil
	}
	readByte := func(i int) (byte, bool) {
		if i+2 >= len(s) || s[i] != '%' || !ishex(s[i+1]) || !ishex(s[i+2]) {
			return 0, false
		}
		return unhex(s[i+1])<<4 | unhex(s[i+2]), true
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); {
		if s[i] != '%' {
			out = append(out, s[i])
			i++
			continue
		}
		b, ok := readByte(i)
		if !ok {
			return "", errURIMalformed
		}
		i += 3
		if b < 0x80 {
			out = append(out, b)
			continue
		}
		var n int
		switch {
		case b&0xe0 == 0xc0:
			n = 2
		case b&0xf0 == 0xe0:
			n = 3
		case b&0xf8 == 0xf0:
			n = 4
		default:
			return "", errURIMalformed
		}
		seq := []byte{b}
		for k := 1; k < n; k++ {
			c, ok := readByte(i)
			if !ok || c&0xc0 != 0x80 {
				return "", errURIMalformed
			}
			seq = append(seq, c)
			i += 3
		}
		if r, size := utf8.DecodeRune(seq); r == utf8.RuneError || size != n {
			return "", errURIMalformed
		}
		out = append(out, seq...)
	}
	return string(out), nil
}

var errURIMalformed = errors.New("URI malformed")

// unescapeLoose is Node's querystring.unescapeBuffer: valid %XX sequences
// decode, anything else stays literal, and the bytes become a string with
// invalid UTF-8 replaced by U+FFFD.
func unescapeLoose(s string) string {
	if v, err := DecodeURIComponent(s); err == nil {
		return v
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && ishex(s[i+1]) && ishex(s[i+2]) {
			out = append(out, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		out = append(out, s[i])
	}
	return strings.ToValidUTF8(string(out), "�")
}

// FormEncode serializes pairs in order exactly as URLSearchParams.toString
// does (application/x-www-form-urlencoded: space → "+", only *-._ and
// alphanumerics left bare). url.Values.Encode differs: it sorts keys and
// treats '~' and '*' the other way round.
func FormEncode(pairs ...[2]string) string {
	var b strings.Builder
	keep := func(c byte) bool { return isAlnum(c) || c == '*' || c == '-' || c == '.' || c == '_' }
	enc := func(s string) {
		s = strings.ToValidUTF8(s, "�")
		for i := 0; i < len(s); i++ {
			if s[i] == ' ' {
				b.WriteByte('+')
				continue
			}
			percentEncode(&b, s[i:i+1], keep)
		}
	}
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}
		enc(p[0])
		b.WriteByte('=')
		enc(p[1])
	}
	return b.String()
}

// ParseQuery is Node's querystring.parse (Express 5's default query parser):
// '&'-separated, the first '=' splits key from value, '+' is a space,
// malformed escapes stay literal, at most 1000 pairs. A repeated key keeps
// every value in order.
func ParseQuery(raw string) map[string][]string {
	out := map[string][]string{}
	if raw == "" {
		return out
	}
	parts := strings.Split(raw, "&")
	if len(parts) > 1000 {
		parts = parts[:1000]
	}
	for _, part := range parts {
		if part == "" {
			continue
		}
		k, v, _ := strings.Cut(part, "=")
		k = unescapeLoose(strings.ReplaceAll(k, "+", " "))
		v = unescapeLoose(strings.ReplaceAll(v, "+", " "))
		out[k] = append(out[k], v)
	}
	return out
}

// Query is one parsed query parameter as Express hands it to a route.
type Query struct {
	// Present is false when the parameter is absent (JS undefined).
	Present bool
	// Values are every value given; more than one is a JS array.
	Values []string
}

// String is the value when the parameter is a single string
// (typeof === "string"), else ("", false).
func (q Query) String() (string, bool) {
	if len(q.Values) == 1 {
		return q.Values[0], true
	}
	return "", false
}

// QueryParam returns one parameter of r's query string.
func QueryParam(r *http.Request, name string) Query {
	vals, ok := ParseQuery(r.URL.RawQuery)[name]
	return Query{Present: ok, Values: vals}
}

// ── Cookies and headers ───────────────────────────────────────────────────

// ParseCookies is lib/auth.js parseCookies. decodeURIComponent throws on
// malformed percent-encoding (a bare '%'); anything on the domain can set such
// a cookie, and a throw there would 500 every cookie-reading route for that
// browser, so the raw value is kept instead: downstream verification treats
// an undecodable cookie as opaque/invalid, which fails closed. A later
// duplicate name wins, as the JS reduce did.
func ParseCookies(header string) map[string]string {
	out := map[string]string{}
	if header == "" {
		return out
	}
	for _, entry := range strings.Split(header, ";") {
		pair := jsstr.Trim(entry)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq < 1 {
			continue
		}
		key := jsstr.Trim(pair[:eq])
		value := jsstr.Trim(pair[eq+1:])
		if dec, err := DecodeURIComponent(value); err == nil {
			out[key] = dec
		} else {
			out[key] = value
		}
	}
	return out
}

// CookieHeader is the request's Cookie header as Node presents it: several
// Cookie fields joined with "; ".
func CookieHeader(r *http.Request) string { return strings.Join(r.Header.Values("Cookie"), "; ") }

// RequestCookies is ParseCookies(CookieHeader(r)).
func RequestCookies(r *http.Request) map[string]string { return ParseCookies(CookieHeader(r)) }

// Header is a request header as Node's req.headers / Express's req.get
// return it: repeated fields joined with ", ".
func Header(r *http.Request, name string) string { return strings.Join(r.Header.Values(name), ", ") }

// OriginalURL is Express's req.originalUrl: the request target as received.
func OriginalURL(r *http.Request) string {
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

// EncodeURL is the encodeurl package Express runs every Location through:
// characters a URL cannot carry are percent-encoded, existing valid %XX
// escapes are kept, and a '%' that starts no valid escape becomes %25.
func EncodeURL(s string) string {
	s = strings.ToValidUTF8(s, "�")
	allowed := func(c byte) bool {
		return c == 0x21 || (c >= 0x23 && c <= 0x3b) || c == 0x3d || (c >= 0x3f && c <= 0x5f) ||
			(c >= 0x61 && c <= 0x7a) || c == 0x7c || c == 0x7e
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c == '%' {
			switch {
			case i+2 < len(s) && ishex(s[i+1]) && ishex(s[i+2]):
				b.WriteString(s[i : i+3])
				i += 3
			case i+1 >= len(s):
				b.WriteString("%25")
				i++
			case !ishex(s[i+1]):
				_, size := utf8.DecodeRuneInString(s[i+1:])
				b.WriteString(encodeURI(s[i : i+1+size]))
				i += 1 + size
			case i+2 >= len(s):
				// "%X" at the very end matches none of encodeurl's bad-escape
				// forms, so it passes through.
				b.WriteByte('%')
				i++
			default: // hex then non-hex
				_, size := utf8.DecodeRuneInString(s[i+2:])
				b.WriteString(encodeURI(s[i : i+2+size]))
				i += 2 + size
			}
			continue
		}
		if c < utf8.RuneSelf && allowed(c) {
			b.WriteByte(c)
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(encodeURI(s[i : i+size]))
		i += size
	}
	return b.String()
}
