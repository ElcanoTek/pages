// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package mcpproto

// The JSON body parser of the endpoint: express.json({limit, strict: false})
// from body-parser 2.x, as far as a client can observe it.

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

var (
	// ErrBodyParse is a body that is not JSON (body-parser's
	// entity.parse.failed): 400 Parse error.
	ErrBodyParse = errors.New("request body is not valid JSON")
	// ErrBodyTooLarge is a body over the limit (entity.too.large): 413.
	ErrBodyTooLarge = errors.New("request body too large")
)

// errUnsupported marks body-parser failures that are not parse or size
// errors (415 charset/encoding, malformed Content-Type, aborted reads).
// lib/mcp.js's error middleware did not recognize them, so they answer 500
// Internal error.
var errUnsupported = errors.New("unsupported request body")

// ReadJSONBody reads and parses r's body as express.json did: only when the
// request has a body and its Content-Type is application/json; UTF-8 (a BOM
// is stripped); gzip/deflate inflated; an empty body is {}; any JSON value
// is accepted. It returns nil when there was nothing to parse.
func ReadJSONBody(r *http.Request, limit int64) (any, error) {
	_, hasLength := r.Header["Content-Length"]
	if !hasLength && len(r.TransferEncoding) == 0 && r.ContentLength <= 0 {
		return nil, nil
	}
	ctHeader := r.Header.Get("Content-Type")
	if ctHeader == "" {
		return nil, nil
	}
	ct, err := ParseContentType(ctHeader)
	if err != nil {
		// type-is let content-type's parse error escape the middleware.
		return nil, fmt.Errorf("%w: %w", errUnsupported, err)
	}
	if !mediaTyperTest(ct.Type) || ct.Type != "application/json" {
		return nil, nil
	}
	charset := "utf-8"
	if c, ok := ct.Params["charset"]; ok {
		charset = strings.ToLower(c)
	}
	if !strings.HasPrefix(charset, "utf-") || iconvCanonical(charset) != "utf8" {
		// Other utf-* charsets iconv-lite would decode are not supported
		// here; the rest were a 415 that the error middleware made a 500.
		return nil, fmt.Errorf("%w: charset %q", errUnsupported, charset)
	}
	encoding := strings.ToLower(strings.Join(r.Header.Values("Content-Encoding"), ", "))
	if encoding == "" {
		encoding = "identity"
	}
	defer httpx.ArmBodyReadDeadline(r, limit)()
	var stream io.Reader = r.Body
	switch encoding {
	case "identity":
		if r.ContentLength > limit {
			return nil, ErrBodyTooLarge
		}
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errUnsupported, err)
		}
		defer zr.Close()
		stream = zr
	case "deflate":
		zr, err := zlib.NewReader(r.Body)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errUnsupported, err)
		}
		defer zr.Close()
		stream = zr
	default:
		// body-parser also inflated "br"; the standard library has no
		// Brotli decoder.
		return nil, fmt.Errorf("%w: content encoding %q", errUnsupported, encoding)
	}
	raw, err := io.ReadAll(io.LimitReader(stream, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUnsupported, err)
	}
	if int64(len(raw)) > limit {
		return nil, ErrBodyTooLarge
	}
	text := []byte(jsonv.DecodeUTF8(raw))
	text = bytes.TrimPrefix(text, []byte("\xef\xbb\xbf"))
	if len(text) == 0 {
		return jsonv.NewObject(), nil
	}
	v, err := jsonv.ParseLossless(text)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBodyParse, err)
	}
	return v, nil
}

// iconvCanonical is iconv-lite's encoding-name canonicalization.
func iconvCanonical(name string) string {
	name = strings.ToLower(name)
	if m := regexp.MustCompile(`:\d{4}$`).FindStringIndex(name); m != nil {
		name = name[:m[0]]
	}
	var b strings.Builder
	for _, c := range []byte(name) {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ContentType is a parsed Content-Type header (content-type 1.0.5).
type ContentType struct {
	Type   string // lower-cased type/subtype
	Params map[string]string
}

const tchar = "!#$%&'*+.^_`|~0-9A-Za-z-"

var (
	ctTypeRegexp  = regexp.MustCompile(`^[` + tchar + `]+/[` + tchar + `]+$`)
	ctParamRegexp = regexp.MustCompile(`^; *([` + tchar + `]+) *= *("(?:[\x{0b}\x{20}\x{21}\x{23}-\x{5b}\x{5d}-\x{7e}\x{80}-\x{ff}]|\\[\x{0b}\x{20}-\x{ff}])*"|[` + tchar + `]+) *`)
	ctQescRegexp  = regexp.MustCompile(`\\([\x{0b}\x{20}-\x{ff}])`)
	mediaTyper    = regexp.MustCompile(`^ *([A-Za-z0-9][A-Za-z0-9!#$&^_-]{0,126})/([A-Za-z0-9][A-Za-z0-9!#$&^_.+-]{0,126}) *$`)
)

// ParseContentType is the content-type package's parse, strict as it is:
// a malformed parameter list is an error.
func ParseContentType(header string) (ContentType, error) {
	s := latin1(header)
	index := strings.IndexByte(s, ';')
	typ := s
	if index >= 0 {
		typ = s[:index]
	}
	typ = jsstr.Trim(typ)
	if !ctTypeRegexp.MatchString(typ) {
		return ContentType{}, errors.New("invalid media type")
	}
	ct := ContentType{Type: strings.ToLower(typ), Params: map[string]string{}}
	if index < 0 {
		return ct, nil
	}
	for index < len(s) {
		m := ctParamRegexp.FindStringSubmatch(s[index:])
		if m == nil {
			return ContentType{}, errors.New("invalid parameter format")
		}
		index += len(m[0])
		value := m[2]
		if strings.HasPrefix(value, `"`) {
			value = ctQescRegexp.ReplaceAllString(value[1:len(value)-1], "$1")
		}
		ct.Params[strings.ToLower(m[1])] = value
	}
	return ct, nil
}

func mediaTyperTest(typ string) bool { return mediaTyper.MatchString(strings.ToLower(typ)) }

// latin1 turns header bytes into the string Node sees: one code point per
// byte.
func latin1(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}
