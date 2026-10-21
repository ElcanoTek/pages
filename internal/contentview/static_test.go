// SPDX-License-Identifier: BUSL-1.1

package contentview

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseRangeSuffix(t *testing.T) {
	cases := []struct {
		header string
		want   []byteRange // empty ⇒ unsatisfiable
	}{
		{"bytes=-100", []byteRange{{400, 499}}},
		{"bytes=-500", []byteRange{{0, 499}}},  // suffix equal to the size
		{"bytes=-1000", []byteRange{{0, 499}}}, // longer suffix: the whole file (RFC 9110 §14.1.2)
		{"bytes=-0", nil},                      // zero-length suffix: unsatisfiable
		{"bytes=-000", nil},
		{"bytes=-0,-1000", []byteRange{{0, 499}}},
		{"bytes=0-9,-1000", []byteRange{{0, 499}}},
		{"bytes=600-", nil},
	}
	for _, c := range cases {
		got, ok := parseRange(500, c.header)
		if !ok {
			t.Fatalf("%s: treated as malformed", c.header)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %v, want %v", c.header, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: got %v, want %v", c.header, got, c.want)
			}
		}
	}
	if got, ok := parseRange(0, "bytes=-5"); !ok || len(got) != 0 {
		t.Fatalf("a suffix of an empty file is unsatisfiable: %v %v", got, ok)
	}
}

func TestServeStaticOversizedSuffixRange(t *testing.T) {
	body := bytes.Repeat([]byte("abcde"), 100)       // 500 bytes
	fsys := &fstest.MapFS{"asset.txt": {Data: body}} // a pointer: the file cache keys on the FS
	serve := func(rng string) (*httptest.ResponseRecorder, error) {
		r := httptest.NewRequest(http.MethodGet, "/asset.txt", nil)
		r.Header.Set("Range", rng)
		w := httptest.NewRecorder()
		handled, err := serveStatic(w, r, fsys, "asset.txt", func(http.Header) {})
		if !handled {
			t.Fatalf("%s: fell through", rng)
		}
		return w, err
	}
	for _, rng := range []string{"bytes=-1000", "bytes=-500"} {
		w, err := serve(rng)
		if err != nil {
			t.Fatalf("%s: %v", rng, err)
		}
		if w.Code != http.StatusPartialContent || w.Header().Get("Content-Range") != "bytes 0-499/500" || !bytes.Equal(w.Body.Bytes(), body) {
			t.Fatalf("%s: %d %q %d bytes", rng, w.Code, w.Header().Get("Content-Range"), w.Body.Len())
		}
	}
	w, err := serve("bytes=-10")
	if err != nil || w.Code != http.StatusPartialContent || w.Body.String() != strings.Repeat("abcde", 2) {
		t.Fatalf("bytes=-10: %v %d %q", err, w.Code, w.Body.String())
	}
	w, err = serve("bytes=-0")
	var se *statusError
	if !errors.As(err, &se) || se.status != http.StatusRequestedRangeNotSatisfiable || w.Header().Get("Content-Range") != "bytes */500" {
		t.Fatalf("bytes=-0 must be 416: %v %q", err, w.Header().Get("Content-Range"))
	}
}
