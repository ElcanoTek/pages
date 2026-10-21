// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package contentview

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func deflated(t *testing.T, s string, raw bool) []byte {
	t.Helper()
	var b bytes.Buffer
	if raw {
		w, _ := flate.NewWriter(&b, flate.DefaultCompression)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
	} else {
		w := zlib.NewWriter(&b)
		_, _ = w.Write([]byte(s))
		_ = w.Close()
	}
	return b.Bytes()
}

// HTTP Content-Encoding: deflate is zlib-wrapped (RFC 1950), the only form
// express.text/express.urlencoded (zlib.createInflate) inflated.
func TestDeflateBodiesAreZlib(t *testing.T) {
	req := func(ct string, body []byte) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/p", bytes.NewReader(body))
		r.Header.Set("Content-Type", ct)
		r.Header.Set("Content-Encoding", "deflate")
		return r
	}
	const html = "<h1>Northwind quarterly</h1>"
	if s, ok, err := readText(req("text/html", deflated(t, html, false)), 1<<20); err != nil || !ok || s != html {
		t.Fatalf("zlib text: %q %v %v", s, ok, err)
	}
	if pw, err := readPassword(req("application/x-www-form-urlencoded", deflated(t, "password=contoso-secret", false))); err != nil || pw != "contoso-secret" {
		t.Fatalf("zlib form: %q %v", pw, err)
	}
	var se *statusError
	if _, _, err := readText(req("text/html", deflated(t, html, true)), 1<<20); !errors.As(err, &se) || se.status != http.StatusBadRequest {
		t.Fatalf("raw deflate must be refused like body-parser did: %v", err)
	}
}
