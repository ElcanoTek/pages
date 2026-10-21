// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package wire

import (
	"net/http/httptest"
	"testing"
)

// Expected values were recorded from express 5 (res.redirect, res.send,
// res.json).
func TestRedirectMatchesExpress(t *testing.T) {
	const loc = "/a%20b?x=%25zz&y=%C3%A9%25%5B|"
	cases := []struct {
		method, accept, ctype, body string
	}{
		{"GET", "", "text/plain; charset=utf-8", "See Other. Redirecting to " + loc},
		{"GET", "text/html", "text/html; charset=utf-8", "<!DOCTYPE html><head><title>See Other</title></head><body><p>See Other. Redirecting to /a%20b?x=%25zz&amp;y=%C3%A9%25%5B|</p></body>"},
		{"GET", "application/json", "", ""},
		{"HEAD", "", "text/plain; charset=utf-8", ""},
		{"GET", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", "text/html; charset=utf-8", "<!DOCTYPE html><head><title>See Other</title></head><body><p>See Other. Redirecting to /a%20b?x=%25zz&amp;y=%C3%A9%25%5B|</p></body>"},
		{"GET", "*/*", "text/plain; charset=utf-8", "See Other. Redirecting to " + loc},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/r", nil)
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		w := httptest.NewRecorder()
		Redirect(w, r, 303, "/a b?x=%zz&y=é%[|")
		if w.Code != 303 || w.Header().Get("Location") != loc || w.Header().Get("Vary") != "Accept" ||
			w.Header().Get("Content-Type") != c.ctype || w.Body.String() != c.body {
			t.Errorf("%s %q: %d %v %q", c.method, c.accept, w.Code, w.Header(), w.Body.String())
		}
	}
}

func TestSendJSONMatchesExpress(t *testing.T) {
	w := httptest.NewRecorder()
	SendJSON(w, httptest.NewRequest("GET", "/j", nil), 401, map[string]string{"error": "not signed in", "code": "no_session"})
	// map keys marshal sorted; the wire body Express sent was
	// {"error":…,"code":…} — callers that care about order pass a struct.
	if w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Content-Length") != "45" {
		t.Fatalf("headers %v", w.Header())
	}
}

func TestSendFreshness(t *testing.T) {
	body := "Forbidden — Elcano staff only."
	etag := `W/"20-Ta4d1GioLHOsrqHSRMh/U9OUaP8"`
	if got := ETag([]byte(body)); got != etag {
		t.Fatalf("etag = %s", got)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	SendHTML(w, r, 200, body)
	if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("Content-Type") != "" {
		t.Fatalf("fresh = %d %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	SendHTML(w, r, 403, body)
	if w.Code != 403 || w.Body.String() != body {
		t.Fatalf("non-2xx is never fresh: %d", w.Code)
	}
	r.Header.Set("Cache-Control", "no-cache")
	w = httptest.NewRecorder()
	SendHTML(w, r, 200, body)
	if w.Code != 200 {
		t.Fatalf("no-cache: %d", w.Code)
	}
}

func TestJSONString(t *testing.T) {
	for in, want := range map[string]string{
		"a":             `"a"`,
		"<&>":           `"<&>"`,
		"\u2028":        "\"\u2028\"",
		"q\"\\\n\t\x01": `"q\"\\\n\t\u0001"`,
	} {
		if got := JSONString(in); got != want {
			t.Errorf("JSONString(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestUTF16Len(t *testing.T) {
	if UTF16Len("a😀é") != 4 {
		t.Fatal(UTF16Len("a😀é"))
	}
}
