// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/charmap"

	"github.com/ElcanoTek/pages/internal/httpx"
)

// The two body parsers the content host runs, as body-parser ran them:
// express.urlencoded for the password form and express.text for the deck save
// channel. A request whose Content-Type does not match leaves the body absent
// (Express 5's undefined); a body over the limit is a 413, an unsupported
// charset or encoding a 415 — all of which the error handler answers with the
// branded page at that status.

// hasBody is type-is's hasBody: a body is declared by Transfer-Encoding or a
// numeric Content-Length (even "0").
func hasBody(r *http.Request) bool {
	if r.Header.Get("Transfer-Encoding") != "" || len(r.TransferEncoding) > 0 || r.ContentLength != 0 {
		return true
	}
	cl := r.Header.Get("Content-Length")
	if cl == "" {
		return false
	}
	_, err := strconv.ParseFloat(strings.TrimSpace(cl), 64)
	return err == nil
}

// mediaType returns the request's media type (lowercased) and charset.
func mediaType(r *http.Request) (string, string, bool) {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return "", "", false
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return "", "", false
	}
	return strings.ToLower(mt), strings.ToLower(params["charset"]), true
}

func tooLarge() error {
	return &statusError{status: http.StatusRequestEntityTooLarge, msg: "request entity too large"}
}

func unsupported(msg string) error {
	return &statusError{status: http.StatusUnsupportedMediaType, msg: msg}
}

// readLimited reads the (possibly compressed) body, refusing more than limit
// bytes after decompression.
func readLimited(r *http.Request, limit int64) ([]byte, error) {
	if cl := r.ContentLength; cl > limit {
		return nil, tooLarge()
	}
	var src io.Reader = r.Body
	switch enc := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, &statusError{status: http.StatusBadRequest, msg: "invalid gzip body"}
		}
		defer zr.Close()
		src = zr
	case "deflate":
		// HTTP deflate is the zlib format (RFC 1950), which is all
		// body-parser's zlib.createInflate accepted; raw DEFLATE is refused.
		zr, err := zlib.NewReader(r.Body)
		if err != nil {
			return nil, &statusError{status: http.StatusBadRequest, msg: "invalid deflate body"}
		}
		defer zr.Close()
		src = zr
	default:
		return nil, unsupported(`unsupported content encoding "` + enc + `"`)
	}
	b, err := io.ReadAll(io.LimitReader(src, limit+1))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, tooLarge()
		}
		return nil, &statusError{status: http.StatusBadRequest, msg: "request aborted"}
	}
	if int64(len(b)) > limit {
		return nil, tooLarge()
	}
	return b, nil
}

func decodeCharset(b []byte, charset string) (string, error) {
	switch charset {
	case "", "utf-8", "utf8", "us-ascii":
		return string(b), nil
	case "iso-8859-1", "latin1":
		out, err := charmap.ISO8859_1.NewDecoder().Bytes(b)
		if err != nil {
			return "", unsupported("unsupported charset")
		}
		return string(out), nil
	default:
		return "", unsupported(`unsupported charset "` + strings.ToUpper(charset) + `"`)
	}
}

// readPassword is express.urlencoded({extended: false, limit: "1kb"}) then
// `(req.body && req.body.password) || ""`. A repeated field is a JS array,
// which no stored hash verifies, so it reads as "".
func readPassword(r *http.Request) (string, error) {
	mt, charset, ok := mediaType(r)
	if !ok || mt != "application/x-www-form-urlencoded" || !hasBody(r) {
		return "", nil
	}
	if charset != "" && charset != "utf-8" && charset != "utf8" && charset != "iso-8859-1" {
		return "", unsupported(`unsupported charset "` + strings.ToUpper(charset) + `"`)
	}
	b, err := readLimited(r, passwordFormLimit)
	if err != nil {
		return "", err
	}
	raw := string(b)
	if strings.Count(raw, "&")+1 > 1000 {
		return "", &statusError{status: http.StatusRequestEntityTooLarge, msg: "too many parameters"}
	}
	vals := httpx.ParseQuery(raw)["password"]
	if len(vals) != 1 {
		return "", nil
	}
	return vals[0], nil
}

// readText is express.text({type: ["text/html", "text/plain"], limit}). ok is
// false when the parser did not run (no body, or another media type).
func readText(r *http.Request, limit int64) (string, bool, error) {
	mt, charset, ok := mediaType(r)
	if !ok || (mt != "text/html" && mt != "text/plain") || !hasBody(r) {
		return "", false, nil
	}
	b, err := readLimited(r, limit)
	if err != nil {
		return "", false, err
	}
	s, err := decodeCharset(b, charset)
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}
