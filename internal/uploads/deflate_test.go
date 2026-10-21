// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package uploads

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
)

// HTTP Content-Encoding: deflate is the zlib format (RFC 1950), the only
// form body-parser's zlib.createInflate accepted. The first vector is the
// body an Express client sent (Node's zlib.deflateSync of {"a":"df"}).
func TestTicketBodyDeflateIsZlib(t *testing.T) {
	node, err := base64.StdEncoding.DecodeString("eJyrVkpUslJKSVOqBQAPiQLm")
	if err != nil {
		t.Fatal(err)
	}
	var big bytes.Buffer
	zw := zlib.NewWriter(&big)
	payload := strings.Repeat("Northwind ticket bytes\n", 4096)
	_, _ = zw.Write([]byte(payload))
	_ = zw.Close()

	put := func(body []byte) ([]byte, error) {
		r, _ := newRequest("PUT", "/x", bytes.NewReader(body))
		r.Header.Set("Content-Encoding", "deflate")
		return readRawBody(r, MaxUploadBytes())
	}
	if got, err := put(node); err != nil || string(got) != `{"a":"df"}` {
		t.Fatalf("node zlib body: %q %v", got, err)
	}
	if got, err := put(big.Bytes()); err != nil || string(got) != payload {
		t.Fatalf("zlib body: %d bytes, %v", len(got), err)
	}
	if got, err := put(nil); err != nil || len(got) != 0 {
		t.Fatalf("empty body: %q %v", got, err)
	}

	// Raw DEFLATE (no zlib header) was a decode error under Express too.
	var raw bytes.Buffer
	fw, _ := flate.NewWriter(&raw, flate.DefaultCompression)
	_, _ = fw.Write([]byte(`{"a":"df"}`))
	_ = fw.Close()
	_, err = put(raw.Bytes())
	if e, ok := apierror.As(err); !ok || e.Status != http.StatusBadRequest || e.Code != "upload_encoding_invalid" {
		t.Fatalf("raw deflate: %v", err)
	}
	// A truncated zlib stream is a decode error, not a short body.
	_, err = put(big.Bytes()[:big.Len()/2])
	if e, ok := apierror.As(err); !ok || e.Code != "upload_encoding_invalid" {
		t.Fatalf("truncated zlib: %v", err)
	}
}
