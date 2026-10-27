// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/tokens"
)

// gzipJSON is a small gzip body that inflates to a JSON document of about n
// bytes.
func gzipJSON(t *testing.T, n int) string {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = zw.Write([]byte(`{"slug":"northwind-bomb","title":"T","pad":"`))
	_, _ = zw.Write(bytes.Repeat([]byte("0"), n))
	_, _ = zw.Write([]byte(`"}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// The dashboard inflated and parsed request bodies up to MAX_REQUEST_BYTES
// before anything checked who sent them, so an anonymous client could send a
// small gzip body that grew to the full limit and was then parsed. Without a
// verified credential the decompressed body is now capped at
// reqbody.UnauthenticatedLimit, decided before any of it is read.
func TestUnauthenticatedCompressedBodyIsCappedBeforeParsing(t *testing.T) {
	e := setup(t, nil)
	bomb := gzipJSON(t, 30<<20) // ~30 KiB on the wire, 30 MiB inflated
	if len(bomb) > 1<<20 {
		t.Fatalf("test body is not small on the wire: %d", len(bomb))
	}
	gz := map[string]string{"Content-Encoding": "gzip"}
	for _, c := range []struct{ name, path, cookie, auth string }{
		{"no credential, REST", "/api/v1/pages", "", ""},
		{"bad bearer token", "/api/v1/pages", "", "Bearer pgs_not-a-real-token"},
		{"non-admin session, admin API", "/api/v1/admin/pages", e.outCk, ""},
		{"non-API path", "/admin", e.adminCk, ""},
	} {
		headers := map[string]string{"Content-Encoding": "gzip"}
		if c.auth != "" {
			headers["Authorization"] = c.auth
		}
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		r := e.do("POST", c.path, opt{body: bomb, cookie: c.cookie, headers: headers})
		runtime.ReadMemStats(&after)
		if r.status != 413 {
			t.Fatalf("%s: status = %d %.200s", c.name, r.status, r.body)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 8<<20 {
			t.Fatalf("%s: refusing the body allocated %d bytes", c.name, grew)
		}
	}

	// A verified agent token keeps the full limit.
	m, err := tokens.Mint(context.Background(), e.d, tokens.MintParams{Label: "northwind-body", Scope: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	big := gzipJSON(t, 200<<10)
	r := e.do("POST", "/api/v1/pages", opt{body: big, headers: map[string]string{
		"Content-Encoding": "gzip", "Authorization": "Bearer " + m.Token,
	}})
	if r.status == 413 || strings.Contains(r.body, "body_too_large") {
		t.Fatalf("an authenticated 200 KiB body was refused: %d %s", r.status, r.body)
	}
	// So does an admin session on the admin API (it then needs CSRF).
	r = e.do("POST", "/api/v1/admin/pages", opt{body: big, cookie: e.adminCk, headers: gz})
	if r.status == 413 {
		t.Fatalf("an admin's 200 KiB body was refused: %d %s", r.status, r.body)
	}
	// A malformed small body is still the parser's 400 before authentication.
	if r := e.do("POST", "/api/v1/pages", opt{body: "{not json"}); r.status != 400 {
		t.Fatalf("malformed = %d", r.status)
	}
}
