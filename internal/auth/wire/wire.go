// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package wire holds the small pieces of Node/Express request and response
// behaviour the sign-in surface has to reproduce byte for byte: the cookie
// jar parser lib/auth.js shipped, encodeURIComponent and URLSearchParams
// serialization, Node's querystring parser (Express 5's "simple" query
// parser), encodeurl for Location headers, and res.send / res.json /
// res.redirect with Express's weak ETag and freshness check.
//
// It is deliberately dependency-free so packages that only need ParseCookies
// (the content host's page sessions, the rate limiter's key) can import it
// without pulling in the database.
package wire

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // Express's etag is a sha1 content hash, not a security primitive
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ── JavaScript string helpers ──────────────────────────────────────────────

// IsJSSpace reports whether r is JavaScript whitespace or a line terminator:
// what String.prototype.trim strips and what /\s/u matches.
func IsJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// TrimJS is String.prototype.trim.
func TrimJS(s string) string { return strings.TrimFunc(s, IsJSSpace) }

// UTF16Len is a JavaScript string's .length for s.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

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
		pair := TrimJS(entry)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq < 1 {
			continue
		}
		key := TrimJS(pair[:eq])
		value := TrimJS(pair[eq+1:])
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

// ── Responses (Express semantics) ─────────────────────────────────────────

// ETag is Express's default weak ETag for a body.
func ETag(body []byte) string {
	sum := sha1.Sum(body) //nolint:gosec // content hash for caching, as Express computes it
	hash := base64.StdEncoding.EncodeToString(sum[:])[:27]
	return `W/"` + strconv.FormatInt(int64(len(body)), 16) + "-" + hash + `"`
}

var noCache = func(cc string) bool {
	for _, part := range strings.Split(cc, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "no-cache") {
			return true
		}
	}
	return false
}

// fresh is the `fresh` package against an ETag (Pages never sets
// Last-Modified on these responses).
func fresh(r *http.Request, etag string) bool {
	ims := r.Header.Get("If-Modified-Since")
	inm := Header(r, "If-None-Match")
	if ims == "" && inm == "" {
		return false
	}
	if cc := Header(r, "Cache-Control"); cc != "" && noCache(cc) {
		return false
	}
	if inm != "" {
		if inm == "*" {
			return true
		}
		for _, m := range strings.Split(inm, ",") {
			m = strings.TrimSpace(m)
			if m == etag || "W/"+m == etag || m == "W/"+etag {
				return true
			}
		}
		return false
	}
	// If-Modified-Since without a Last-Modified is always stale.
	return false
}

// Send is Express's res.status(status).send(body) for a body whose
// Content-Type is already chosen: Content-Length, weak ETag, 304 when the
// request is fresh, no body for HEAD.
func Send(w http.ResponseWriter, r *http.Request, status int, contentType string, body []byte) {
	h := w.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", contentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	etag := h.Get("ETag")
	if etag == "" {
		etag = ETag(body)
		h.Set("ETag", etag)
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && status >= 200 && status < 300 && fresh(r, etag) {
		status = http.StatusNotModified
	}
	if status == http.StatusNoContent || status == http.StatusNotModified {
		h.Del("Content-Type")
		h.Del("Content-Length")
		h.Del("Transfer-Encoding")
		body = nil
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead && len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// SendHTML is res.status(status).send(html) (or .type("html").send).
func SendHTML(w http.ResponseWriter, r *http.Request, status int, html string) {
	Send(w, r, status, "text/html; charset=utf-8", []byte(html))
}

// Marshal is JSON.stringify for the plain values Pages sends: no HTML
// escaping (encoding/json escapes <>& by default; JavaScript does not) and no
// trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// SendJSON is res.status(status).json(v).
func SendJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := Marshal(v)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	Send(w, r, status, "application/json; charset=utf-8", body)
}

// JSONString is JSON.stringify(s) for a string: like encoding/json, but
// U+2028/U+2029 and <>& stay literal, as V8 writes them.
func JSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
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

var statusText = map[int]string{
	301: "Moved Permanently", 302: "Found", 303: "See Other", 307: "Temporary Redirect", 308: "Permanent Redirect",
}

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
}

// Redirect is Express 5's res.redirect(status, url): Location through
// encodeurl, a text or HTML body chosen by Accept, Vary: Accept.
func Redirect(w http.ResponseWriter, r *http.Request, status int, url string) {
	address := EncodeURL(url)
	h := w.Header()
	h.Set("Location", address)
	h.Add("Vary", "Accept")
	msg := statusText[status]
	if msg == "" {
		msg = http.StatusText(status)
	}
	var body string
	switch negotiate(Header(r, "Accept"), []string{"text/plain", "text/html"}) {
	case "text/plain":
		h.Set("Content-Type", "text/plain; charset=utf-8")
		body = msg + ". Redirecting to " + address
	case "text/html":
		h.Set("Content-Type", "text/html; charset=utf-8")
		u := escapeHTML(address)
		body = "<!DOCTYPE html><head><title>" + msg + "</title></head><body><p>" + msg + ". Redirecting to " + u + "</p></body>"
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// negotiate is the negotiator package's preferred media type among offers
// (in offer order on ties), "" when Accept excludes them all. No Accept
// header accepts anything, so the first offer wins.
func negotiate(accept string, offers []string) string {
	if strings.TrimSpace(accept) == "" {
		return offers[0]
	}
	type rng struct {
		typ, sub string
		q        float64
		idx      int
	}
	var ranges []rng
	for i, part := range strings.Split(accept, ",") {
		fields := strings.Split(part, ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		typ, sub, ok := strings.Cut(mt, "/")
		if !ok {
			continue
		}
		q := 1.0
		for _, p := range fields[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		ranges = append(ranges, rng{typ, sub, q, i})
	}
	best, bestQ, bestS, bestO := "", 0.0, -1, 0
	for _, offer := range offers {
		typ, sub, _ := strings.Cut(offer, "/")
		q, s, o := 0.0, -1, -1
		for _, rg := range ranges {
			spec := 0
			switch {
			case rg.typ == typ && rg.sub == sub:
				spec = 6
			case rg.typ == typ && rg.sub == "*":
				spec = 4
			case rg.typ == "*" && rg.sub == "*":
				spec = 0
			default:
				continue
			}
			if spec > s || (spec == s && rg.q > q) {
				q, s, o = rg.q, spec, rg.idx
			}
		}
		if q <= 0 {
			continue
		}
		if best == "" || q > bestQ || (q == bestQ && s > bestS) || (q == bestQ && s == bestS && o < bestO) {
			best, bestQ, bestS, bestO = offer, q, s, o
		}
	}
	return best
}
