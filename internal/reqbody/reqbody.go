// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package reqbody is the dashboard host's broad body parsing, as server.js
// mounted it ahead of every dashboard router:
//
//	dashboardApp.use(express.urlencoded({ extended: false, limit: "64kb" }));
//	dashboardApp.use(express.json({ limit: MAX_REQUEST_BYTES }));
//
// with body-parser 2.x, type-is 2.1, content-type 2.1 and qs 6.16 semantics
// as far as a client can observe them: which requests are parsed, what the
// parsed value is, and which failures surface with which status. Parse
// failures precede every router handler (and so bearer auth), so a malformed
// body is a 400 even without a token; see Error.APIError for the
// machine-readable envelope server.js's dashboard error handler gave them.
//
// The parsed value uses the jsonv model (JSON objects are *jsonv.Object with
// JavaScript key order, numbers float64). A urlencoded body is an
// *jsonv.Object of string or []any-of-string values. A request neither parser
// accepted has no body at all (Express 5's req.body === undefined).
package reqbody

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

// Options are the two parsers' limits.
type Options struct {
	// JSONLimit is express.json's limit in bytes (MAX_REQUEST_BYTES).
	JSONLimit int64
	// FormLimit is express.urlencoded's limit in bytes ("64kb").
	FormLimit int64
}

// DashboardFormLimit is the dashboard's urlencoded limit ("64kb").
const DashboardFormLimit = 64 * 1024

// maxParams is body-parser's default parameterLimit.
const maxParams = 1000

// DashboardOptions are the dashboard host's limits, read now:
// MAX_REQUEST_BYTES for JSON (the per-version HTML ceiling, PLAN §7) and
// 64 KiB for forms.
func DashboardOptions() Options {
	return Options{JSONLimit: int64(datalimits.MaxRequestBytes()), FormLimit: DashboardFormLimit}
}

// Error is a body-parser failure: its HTTP status and body-parser's error
// type ("entity.too.large", "entity.parse.failed", "charset.unsupported",
// "encoding.unsupported", "parameters.too.many", "request.aborted").
type Error struct {
	Status int
	Type   string
	Err    error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s (%d): %v", e.Type, e.Status, e.Err)
	}
	return fmt.Sprintf("%s (%d)", e.Type, e.Status)
}

func (e *Error) Unwrap() error { return e.Err }

// APIError is server.js's dashboard error mapping for a parser failure:
// unparseable JSON → 400 bad_json, an oversized body → 413 body_too_large,
// any other client error → its status with "invalid request"/bad_request. The
// message is fixed, so a bounded error never echoes the submitted document.
func (e *Error) APIError() *apierror.Error {
	switch e.Type {
	case "entity.parse.failed":
		return apierror.New(http.StatusBadRequest, "invalid JSON request body", "bad_json", nil)
	case "entity.too.large":
		return apierror.New(http.StatusRequestEntityTooLarge, "request body too large", "body_too_large", nil)
	}
	return apierror.New(e.Status, "invalid request", "bad_request", nil)
}

func fail(status int, typ string, err error) *Error {
	return &Error{Status: status, Type: typ, Err: err}
}

// ── Context ────────────────────────────────────────────────────────────────

type ctxKey struct{}

type parsedBody struct {
	value   any
	present bool
}

// WithBody records a parsed body on ctx. present=false records that the
// parsers ran and left the body undefined.
func WithBody(ctx context.Context, value any, present bool) context.Context {
	return context.WithValue(ctx, ctxKey{}, parsedBody{value, present})
}

// From returns the body Middleware parsed: (value, true) when a parser
// produced one, (nil, false) when the request had none (JavaScript
// undefined) or the middleware did not run.
func From(r *http.Request) (any, bool) {
	p, _ := r.Context().Value(ctxKey{}).(parsedBody)
	return p.value, p.present
}

// Parsed reports whether Middleware (or WithBody) already ran for r, so a
// handler mounted both behind it and standalone parses at most once.
func Parsed(r *http.Request) bool {
	_, ok := r.Context().Value(ctxKey{}).(parsedBody)
	return ok
}

// Middleware parses the body once (Parse) and records it for From. A parser
// failure goes to onError instead of next — the caller answers it as the
// dashboard did (JSON for /api/v1 paths, the branded error page elsewhere).
// A request already parsed upstream passes straight through.
func Middleware(o Options, onError func(w http.ResponseWriter, r *http.Request, err *Error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if Parsed(r) {
				next.ServeHTTP(w, r)
				return
			}
			v, present, err := Parse(r, o)
			if err != nil {
				onError(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithBody(r.Context(), v, present)))
		})
	}
}

// ── Parsing ────────────────────────────────────────────────────────────────

// Parse runs urlencoded then json exactly as the two middlewares did: at
// most one of them matches a request's media type. present is false when
// neither ran (no body, or another media type).
func Parse(r *http.Request, o Options) (value any, present bool, err *Error) {
	if !hasBody(r) {
		return nil, false, nil
	}
	header := r.Header.Get("Content-Type")
	switch mediaType(header) {
	case "application/x-www-form-urlencoded":
		v, err := parseForm(r, header, o.FormLimit)
		if err != nil {
			return nil, false, err
		}
		return v, true, nil
	case "application/json":
		v, err := parseJSON(r, header, o.JSONLimit)
		if err != nil {
			return nil, false, err
		}
		return v, true, nil
	}
	return nil, false, nil
}

// hasBody is type-is's hasBody: Transfer-Encoding present, or a numeric
// Content-Length (even "0"). net/http moves Transfer-Encoding out of the
// header map.
func hasBody(r *http.Request) bool {
	if len(r.TransferEncoding) > 0 || r.Header.Get("Transfer-Encoding") != "" {
		return true
	}
	if vals, ok := r.Header["Content-Length"]; ok && len(vals) > 0 {
		s := strings.TrimSpace(vals[0])
		if s == "" {
			return true // isNaN("") is false
		}
		_, err := strconv.ParseFloat(s, 64)
		return err == nil
	}
	return r.ContentLength > 0
}

var mediaTyperRE = regexp.MustCompile(`^ *([A-Za-z0-9][A-Za-z0-9!#$&^_-]{0,126})/([A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}) *$`)

// mediaType is type-is's normalizeType: content-type 2.1's lenient type (up
// to the first ';', OWS-trimmed, lower-cased) when media-typer accepts it,
// else "".
func mediaType(header string) string {
	if header == "" {
		return ""
	}
	typ, _ := parseContentType(header)
	if !mediaTyperRE.MatchString(typ) {
		return ""
	}
	return typ
}

func isOWS(c byte) bool { return c == ' ' || c == '\t' }

// parseContentType is content-type 2.1.0's parse: never fails; the first
// occurrence of a parameter wins; names lower-cased, values verbatim (quoted
// strings unescaped).
func parseContentType(h string) (string, map[string]string) {
	n := len(h)
	i := 0
	for i < n && isOWS(h[i]) {
		i++
	}
	start := i
	for i < n && h[i] != ';' {
		i++
	}
	end := i
	for end > start && isOWS(h[end-1]) {
		end--
	}
	typ := strings.ToLower(h[start:end])
	params := map[string]string{}
	set := func(k, v string) {
		if _, ok := params[k]; !ok {
			params[k] = v
		}
	}
param:
	for i < n {
		i++ // skip ';'
		for i < n && isOWS(h[i]) {
			i++
		}
		keyStart := i
		for i < n {
			c := h[i]
			if c == ';' {
				continue param
			}
			if c == '=' {
				keyEnd := i
				for keyEnd > keyStart && isOWS(h[keyEnd-1]) {
					keyEnd--
				}
				key := strings.ToLower(h[keyStart:keyEnd])
				i++
				for i < n && isOWS(h[i]) {
					i++
				}
				if i < n && h[i] == '"' {
					i++
					var v strings.Builder
					for i < n {
						c := h[i]
						i++
						if c == '"' {
							for i < n && h[i] != ';' {
								i++
							}
							set(key, v.String())
							break
						}
						if c == '\\' && i < n {
							v.WriteByte(h[i])
							i++
							continue
						}
						v.WriteByte(c)
					}
					continue param
				}
				valueStart := i
				for i < n && h[i] != ';' {
					i++
				}
				valueEnd := i
				for valueEnd > valueStart && isOWS(h[valueEnd-1]) {
					valueEnd--
				}
				set(key, h[valueStart:valueEnd])
				continue param
			}
			i++
		}
	}
	return typ, params
}

func charsetOf(header, def string) string {
	_, params := parseContentType(header)
	if c, ok := params["charset"]; ok {
		return strings.ToLower(c)
	}
	return def
}

func unsupportedCharset(charset string) *Error {
	return fail(http.StatusUnsupportedMediaType, "charset.unsupported", fmt.Errorf("unsupported charset %q", strings.ToUpper(charset)))
}

// read is body-parser's read + raw-body: inflate gzip/deflate, refuse a body
// over limit (by Content-Length up front when not encoded, by decoded size
// otherwise).
func read(r *http.Request, limit int64) ([]byte, *Error) {
	enc := strings.ToLower(r.Header.Get("Content-Encoding"))
	if enc == "" {
		enc = "identity"
	}
	var src io.Reader = r.Body
	switch enc {
	case "identity":
		if r.ContentLength > limit {
			return nil, fail(http.StatusRequestEntityTooLarge, "entity.too.large", errors.New("request entity too large"))
		}
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fail(http.StatusBadRequest, "request.aborted", err)
		}
		defer zr.Close()
		src = zr
	case "deflate":
		zr, err := zlib.NewReader(r.Body)
		if err != nil {
			return nil, fail(http.StatusBadRequest, "request.aborted", err)
		}
		defer zr.Close()
		src = zr
	default:
		// body-parser also inflated "br"; the standard library has no Brotli
		// decoder, so it is refused like any other unknown coding.
		return nil, fail(http.StatusUnsupportedMediaType, "encoding.unsupported", fmt.Errorf("unsupported content encoding %q", enc))
	}
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, fail(http.StatusRequestEntityTooLarge, "entity.too.large", err)
		}
		return nil, fail(http.StatusBadRequest, "request.aborted", err)
	}
	if int64(len(b)) > limit {
		return nil, fail(http.StatusRequestEntityTooLarge, "entity.too.large", errors.New("request entity too large"))
	}
	return b, nil
}

// ── JSON ───────────────────────────────────────────────────────────────────

func parseJSON(r *http.Request, header string, limit int64) (any, *Error) {
	charset := charsetOf(header, "utf-8")
	// RFC 7159 §8.1: only utf-* charsets; then iconv-lite must know it.
	if !strings.HasPrefix(charset, "utf-") {
		return nil, unsupportedCharset(charset)
	}
	decode, ok := decoders[charset]
	if !ok {
		// utf-32 and utf-7 were decodable by iconv-lite; nothing sends them.
		return nil, unsupportedCharset(charset)
	}
	raw, err := read(r, limit)
	if err != nil {
		return nil, err
	}
	text := decode(raw)
	if text == "" {
		// body-parser special-cases an empty body as a common client mistake.
		return jsonv.NewObject(), nil
	}
	// strict: the first non-whitespace character must open an object or array.
	first := strings.TrimLeft(text, " \t\n\r")
	if first == "" || (first[0] != '{' && first[0] != '[') {
		return nil, fail(http.StatusBadRequest, "entity.parse.failed", errors.New("strict violation"))
	}
	v, perr := jsonv.ParseString(text)
	if perr != nil {
		return nil, fail(http.StatusBadRequest, "entity.parse.failed", perr)
	}
	return v, nil
}

// decoders are iconv-lite's decoders for the charsets the JSON parser takes,
// BOM stripped (iconv-lite's default).
var decoders = map[string]func([]byte) string{
	"utf-8":    func(b []byte) string { return jsonv.DecodeUTF8(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))) },
	"utf-16le": func(b []byte) string { return decodeUTF16(b, false, false) },
	"utf-16be": func(b []byte) string { return decodeUTF16(b, true, false) },
	"utf-16":   func(b []byte) string { return decodeUTF16(b, false, true) },
}

// decodeUTF16 decodes UTF-16 code units (a lone surrogate becomes U+FFFD).
// detect follows iconv-lite's "utf-16": a BOM picks the byte order, little
// endian otherwise.
func decodeUTF16(b []byte, bigEndian, detect bool) string {
	if detect && len(b) >= 2 {
		switch {
		case b[0] == 0xfe && b[1] == 0xff:
			bigEndian = true
		case b[0] == 0xff && b[1] == 0xfe:
			bigEndian = false
		}
	}
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if bigEndian {
			units = append(units, uint16(b[i])<<8|uint16(b[i+1]))
		} else {
			units = append(units, uint16(b[i+1])<<8|uint16(b[i]))
		}
	}
	if len(units) > 0 && units[0] == 0xfeff {
		units = units[1:]
	}
	s := string(utf16.Decode(units))
	if len(b)%2 == 1 {
		s += "�"
	}
	return s
}

// ── urlencoded ─────────────────────────────────────────────────────────────

func parseForm(r *http.Request, header string, limit int64) (any, *Error) {
	charset := charsetOf(header, "utf-8")
	if charset != "utf-8" && charset != "iso-8859-1" {
		return nil, unsupportedCharset(charset)
	}
	raw, err := read(r, limit)
	if err != nil {
		return nil, err
	}
	var body string
	if charset == "iso-8859-1" {
		runes := make([]rune, len(raw))
		for i, c := range raw {
			runes[i] = rune(c)
		}
		body = string(runes)
	} else {
		body = jsonv.DecodeUTF8(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	}
	if body == "" {
		return jsonv.NewObject(), nil
	}
	if strings.Count(body, "&")+1 > maxParams {
		return nil, fail(http.StatusRequestEntityTooLarge, "parameters.too.many", errors.New("too many parameters"))
	}
	return ParseForm(body, charset == "iso-8859-1"), nil
}

var bracketEscRE = regexp.MustCompile(`(?i)%5[bd]`)

// ParseForm is qs.parse as body-parser 2.x calls it for extended:false
// (depth 0, allowPrototypes, arrayLimit = parameter count): '&'-separated
// pairs, the split at "]=" or else the first '='; '+' is a space and a value
// whose percent-decoding fails stays literal (as a whole); a repeated key
// collects its values into an array; a wholly bracketed key "[k]" is k; "[]"
// keys become array indices; "__proto__" is dropped. Keys enumerate in
// JavaScript order (array indices first).
func ParseForm(s string, latin1 bool) *jsonv.Object {
	s = bracketEscRE.ReplaceAllStringFunc(s, func(m string) string {
		if m[2] == 'b' || m[2] == 'B' {
			return "["
		}
		return "]"
	})
	tmp := jsonv.NewObject()
	for _, part := range strings.Split(s, "&") {
		pos := strings.Index(part, "]=")
		if pos >= 0 {
			pos++
		} else {
			pos = strings.Index(part, "=")
		}
		var key, val string
		if pos == -1 {
			key, val = qsDecode(part, latin1), ""
		} else {
			key, val = qsDecode(part[:pos], latin1), qsDecode(part[pos+1:], latin1)
		}
		if prev, ok := tmp.Get(key); ok {
			tmp.Set(key, combine(prev, val))
		} else {
			tmp.Set(key, val)
		}
	}
	out := jsonv.NewObject()
	tmp.Range(func(key string, v any) bool {
		switch key {
		case "":
		case "[]":
			for i, item := range asArray(v) {
				merge(out, strconv.Itoa(i), item)
			}
		default:
			root := key
			if len(root) >= 2 && root[0] == '[' && root[len(root)-1] == ']' {
				root = root[1 : len(root)-1]
			}
			if root != "__proto__" {
				merge(out, root, v)
			}
		}
		return true
	})
	return out
}

func asArray(v any) []any {
	if a, ok := v.([]any); ok {
		return append([]any(nil), a...)
	}
	return []any{v}
}

func combine(a, b any) any { return append(asArray(a), asArray(b)...) }

// merge is qs's top-level merge of one parsed key: a collision concatenates
// (qs merges two arrays index-wise; no form a client sends produces that).
func merge(out *jsonv.Object, key string, v any) {
	if prev, ok := out.Get(key); ok {
		out.Set(key, combine(prev, v))
		return
	}
	out.Set(key, v)
}

var latin1EscRE = regexp.MustCompile(`%[0-9a-fA-F]{2}`)

// qsDecode is qs's utils.decode.
func qsDecode(s string, latin1 bool) string {
	s = strings.ReplaceAll(s, "+", " ")
	if !strings.Contains(s, "%") {
		return s
	}
	if latin1 {
		return latin1EscRE.ReplaceAllStringFunc(s, func(m string) string {
			n, _ := strconv.ParseUint(m[1:], 16, 8)
			return string(rune(n))
		})
	}
	if d, err := httpx.DecodeURIComponent(s); err == nil {
		return d
	}
	return s
}
