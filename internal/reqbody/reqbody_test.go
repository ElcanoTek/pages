// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package reqbody

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

// express_bodies.json was recorded against Express 5.3 / body-parser 2.3 with
// server.js's dashboard parsers (urlencoded 64kb, json 2 MiB) and its error
// handler: each case POSTs a body and records status and response.
type bodyVector struct {
	Name    string            `json:"name"`
	Headers map[string]string `json:"headers"`
	BodyB64 *string           `json:"bodyB64"`
	NoAuth  bool              `json:"noAuth"`
	Status  int               `json:"status"`
	Body    string            `json:"body"`
}

// generated rebuilds the vectors too large to store.
func generated(name string) []byte {
	const mib2 = 2 * 1024 * 1024
	switch name {
	case "json oversized":
		return []byte(`{"html":"` + strings.Repeat("x", mib2) + `"}`)
	case "json exactly limit":
		return []byte(`{"h":"` + strings.Repeat("x", mib2-8) + `"}`)
	case "json gzip oversized inflated":
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		_, _ = zw.Write([]byte(`{"html":"` + strings.Repeat("x", mib2) + `"}`))
		_ = zw.Close()
		return b.Bytes()
	case "form oversized":
		return []byte("a=" + strings.Repeat("x", 64*1024))
	case "form 1000 params", "form 1001 params":
		n := 1000
		if strings.HasPrefix(name, "form 1001") {
			n = 1001
		}
		parts := make([]string, n)
		for i := range parts {
			parts[i] = fmt.Sprintf("k%d=v", i)
		}
		return []byte(strings.Join(parts, "&"))
	}
	panic("no generator for " + name)
}

func TestMatchesExpress(t *testing.T) {
	raw, err := os.ReadFile("testdata/express_bodies.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []bodyVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, present, perr := Parse(r, Options{JSONLimit: 2 * 1024 * 1024, FormLimit: DashboardFormLimit})
		if perr != nil {
			e := perr.APIError()
			httpx.SendJSONBytes(w, r, e.Status, []byte(jsonv.Stringify(jsonv.ObjectOf("error", e.Message, "code", e.Code))))
			return
		}
		var body any = "<undefined>"
		if present {
			body = v
		}
		httpx.SendJSONBytes(w, r, 200, []byte(jsonv.Stringify(jsonv.ObjectOf("route", "createPage", "body", body))))
	}))
	defer srv.Close()

	for _, vec := range vectors {
		if vec.NoAuth {
			continue // the API package covers parse-before-auth ordering
		}
		t.Run(vec.Name, func(t *testing.T) {
			var payload []byte
			if vec.BodyB64 == nil {
				payload = generated(vec.Name)
			} else if payload, err = base64.StdEncoding.DecodeString(*vec.BodyB64); err != nil {
				t.Fatal(err)
			}
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/pages", bytes.NewReader(payload))
			for k, v := range vec.Headers {
				if k == "Content-Length" {
					n, _ := strconv.Atoi(v)
					req.ContentLength = int64(n)
					continue
				}
				req.Header.Set(k, v)
			}
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			got := string(b)
			if len(got) > 300 {
				got = "<" + strconv.Itoa(len(got)) + " bytes>"
			}
			if res.StatusCode != vec.Status || got != vec.Body {
				t.Fatalf("got %d %s\nwant %d %s", res.StatusCode, got, vec.Status, vec.Body)
			}
		})
	}
}

func TestNoBodyIsUndefined(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/pages", nil)
	r.Header.Set("Content-Type", "application/json")
	if _, present, err := Parse(r, DashboardOptions()); present || err != nil {
		t.Fatalf("a GET without a body has none: present=%v err=%v", present, err)
	}
}

func TestMiddlewareParsesOnce(t *testing.T) {
	calls := 0
	h := Middleware(DashboardOptions(), func(w http.ResponseWriter, _ *http.Request, err *Error) {
		w.WriteHeader(err.APIError().Status)
	})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		v, ok := From(r)
		if !ok || jsonv.Stringify(v) != `{"a":1}` || !Parsed(r) {
			t.Errorf("From = %v %v", v, ok)
		}
	}))
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"a":1}`))
	r.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(httptest.NewRecorder(), r)
	// A second Middleware layer leaves the recorded body alone.
	r2 := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{bad`))
	r2 = r2.WithContext(WithBody(r2.Context(), jsonv.ObjectOf("a", 1.0), true))
	h.ServeHTTP(httptest.NewRecorder(), r2)
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
	bad := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{bad`))
	bad.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != 400 || calls != 2 {
		t.Fatalf("bad body: %d calls=%d", rec.Code, calls)
	}
}

func TestParseFormQsEdges(t *testing.T) {
	// Recorded from qs 6.16 with body-parser's extended:false options.
	cases := map[string]string{
		"[a]=1":                `{"a":"1"}`,
		"a=1&[a]=2":            `{"a":["1","2"]}`,
		"[]=1":                 `{"0":"1"}`,
		"[]=1&[]=2":            `{"0":"1","1":"2"}`,
		"[5]=x&b=1":            `{"5":"x","b":"1"}`,
		"[__proto__]=1":        `{}`,
		"a%5Bb%5D=1":           `{"a[b]":"1"}`,
		"[a]b=1":               `{"[a]b":"1"}`,
		"a]=b=c":               `{"a]":"b=c"}`,
		"[a=1":                 `{"[a":"1"}`,
		"[]":                   `{"0":""}`,
		"%5Ba%5D=z":            `{"a":"z"}`,
		"[0]=a&[0]=b":          `{"0":["a","b"]}`,
		"b=1&[0]=a":            `{"0":"a","b":"1"}`,
		"[01]=a":               `{"01":"a"}`,
		"a+%zz=b+%zz":          `{"a %zz":"b %zz"}`,
		"[ab]=1&ab=2&ab=3":     `{"ab":["1","2","3"]}`,
		"x[]=1&x[]=2&x=3":      `{"x[]":["1","2"],"x":"3"}`,
		"2=a&1=b&[0]=c":        `{"0":"c","1":"b","2":"a"}`,
		"__proto__=1&a=2":      `{"a":"2"}`,
		"t=%E2%82%AC&u=%E2%82": `{"t":"€","u":"%E2%82"}`,
	}
	for in, want := range cases {
		if got := jsonv.Stringify(ParseForm(in, false)); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
	if got := jsonv.Stringify(ParseForm("t=%E9%zz", true)); got != `{"t":"é%zz"}` {
		t.Errorf("latin1: %s", got)
	}
}

func TestParseContentType(t *testing.T) {
	typ, params := parseContentType(` Application/JSON ; Charset = "UT\F-8" ; charset=latin1;x`)
	if typ != "application/json" || params["charset"] != "UTF-8" || len(params) != 1 {
		t.Fatalf("%q %v", typ, params)
	}
	if mediaType("application/json;;") != "application/json" || mediaType("json") != "" || mediaType("") != "" {
		t.Fatal("mediaType")
	}
}
