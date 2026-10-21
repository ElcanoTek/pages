// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"io"
	"net/http"
	"strings"

	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/shell"
)

// req is one request as an Express handler saw it.
type req struct {
	*http.Request
	// params are the decoded route parameters; a splat is its segments
	// joined with "/".
	params map[string]string
	// body is req.body: the parsed JSON or urlencoded body, the text body
	// for a text route, or undef when no parser applied.
	body any
	// rest is the mount-relative raw path for a mounted handler (req.path
	// inside app.use(prefix, fn)).
	rest string
}

// bodyOr is `req.body || {}`.
func (r *req) bodyOr() any {
	if truthy(r.body) {
		return r.body
	}
	return o()
}

type handler func(w http.ResponseWriter, r *req)

// route is one Express registration. A pattern segment ":name" matches one
// segment and "*name" the rest of the path (one or more segments); a route
// with mount set is app.use(prefix, fn), which matches the prefix and
// everything under it. method "" matches every method, "GET" also HEAD.
type route struct {
	method  string
	pattern string
	mount   bool
	textOK  bool // the route takes express.text for text/html and text/plain
	h       handler
	// mw is set instead of h for a middleware that may call next(): it
	// returns true to fall through to the next route.
	mw func(w http.ResponseWriter, r *req) bool
}

func (rt route) methodMatches(m string) bool {
	switch rt.method {
	case "":
		return true
	case http.MethodGet:
		return m == http.MethodGet || m == http.MethodHead
	}
	return m == rt.method
}

// match is path-to-regexp's non-strict, case-insensitive match against the
// raw path: a trailing slash is tolerated and parameters are decoded.
func (rt route) match(raw string) (map[string]string, string, bool, bool) {
	if rt.mount {
		p := strings.TrimSuffix(rt.pattern, "/")
		if len(raw) < len(p) || !strings.EqualFold(raw[:len(p)], p) {
			return nil, "", false, false
		}
		rest := raw[len(p):]
		if rest != "" && rest[0] != '/' {
			return nil, "", false, false
		}
		if rest == "" {
			rest = "/"
		}
		return nil, rest, true, true
	}
	path := raw
	if len(path) > 1 {
		path = strings.TrimSuffix(path, "/")
	}
	want := strings.Split(strings.TrimPrefix(rt.pattern, "/"), "/")
	got := strings.Split(strings.TrimPrefix(path, "/"), "/")
	params := map[string]string{}
	for i, seg := range want {
		if strings.HasPrefix(seg, "*") {
			if i >= len(got) || (len(got) == i+1 && got[i] == "") {
				return nil, "", false, false
			}
			parts := make([]string, 0, len(got)-i)
			for _, g := range got[i:] {
				d, err := httpx.DecodeURIComponent(g)
				if err != nil {
					return nil, "", true, false
				}
				parts = append(parts, d)
			}
			params[seg[1:]] = strings.Join(parts, "/")
			return params, "", true, true
		}
		if i >= len(got) {
			return nil, "", false, false
		}
		if strings.HasPrefix(seg, ":") {
			if got[i] == "" {
				return nil, "", false, false
			}
			d, err := httpx.DecodeURIComponent(got[i])
			if err != nil {
				return nil, "", true, false
			}
			params[seg[1:]] = d
			continue
		}
		if !strings.EqualFold(seg, got[i]) {
			return nil, "", false, false
		}
	}
	if len(got) != len(want) {
		return nil, "", false, false
	}
	return params, "", true, true
}

// jsonLimit is express.json({ limit: "3mb" }) (and the text parser's limit).
const jsonLimit = 3 * 1024 * 1024

// parseBody is the global express.json + express.urlencoded pair, plus
// express.text for the routes that mount it. ok is false when a parser
// refused the body; the error page has then been written.
func parseBody(w http.ResponseWriter, r *http.Request, text bool) (any, bool) {
	if r.Body == nil || (r.ContentLength == 0 && len(r.TransferEncoding) == 0) {
		if r.ContentLength == 0 && r.Header.Get("Content-Length") != "" && isJSON(r) {
			return o(), true // body-parser answers an empty JSON body with {}
		}
		return undef, true
	}
	kind := ""
	switch {
	case isJSON(r):
		kind = "json"
	case mediaType(r) == "application/x-www-form-urlencoded":
		kind = "form"
	case text && (mediaType(r) == "text/html" || mediaType(r) == "text/plain"):
		kind = "text"
	default:
		return undef, true
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, jsonLimit+1))
	if err != nil {
		sendError(w, r, http.StatusBadRequest, "BadRequestError: request aborted")
		return nil, false
	}
	if len(raw) > jsonLimit {
		sendError(w, r, http.StatusRequestEntityTooLarge, "PayloadTooLargeError: request entity too large")
		return nil, false
	}
	switch kind {
	case "text":
		return jsonv.DecodeUTF8(raw), true
	case "form":
		return parseForm(string(raw)), true
	}
	if len(raw) == 0 {
		return o(), true
	}
	// strict: only an object or an array is accepted.
	trimmed := strings.TrimLeft(string(raw), " \t\n\r")
	if trimmed == "" || (trimmed[0] != '{' && trimmed[0] != '[') {
		sendError(w, r, http.StatusBadRequest, "SyntaxError: Unexpected token in JSON")
		return nil, false
	}
	v, err := jsonv.ParseLossless(raw)
	if err != nil {
		sendError(w, r, http.StatusBadRequest, "SyntaxError: "+err.Error())
		return nil, false
	}
	return fromParsed(v), true
}

// fromParsed turns parsed arrays into shared lists, so a stored body behaves
// as the JavaScript array it was.
func fromParsed(v any) any {
	switch x := v.(type) {
	case []any:
		out := newList()
		for _, item := range x {
			out.push(fromParsed(item))
		}
		return out
	case *jsonv.Object:
		for _, k := range x.Keys() {
			val, _ := x.Get(k)
			x.Set(k, fromParsed(val))
		}
		return x
	}
	return v
}

func mediaType(r *http.Request) string {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return strings.ToLower(strings.TrimSpace(ct))
}

func isJSON(r *http.Request) bool { return mediaType(r) == "application/json" }

// parseForm is querystring.parse, as express.urlencoded({ extended: false })
// uses it: repeated keys become arrays, key order is first appearance.
func parseForm(raw string) obj {
	out := o()
	if raw == "" {
		return out
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		k = formDecode(k)
		v = formDecode(v)
		prev, ok := out.Get(k)
		switch {
		case !ok:
			out.Set(k, v)
		case isList(prev):
			prev.(*list).push(v)
		default:
			out.Set(k, newList(prev, v))
		}
	}
	return out
}

func isList(v any) bool { _, ok := v.(*list); return ok }

func formDecode(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if d, err := httpx.DecodeURIComponent(s); err == nil {
		return d
	}
	return s // querystring.unescape falls back to a lenient decode
}

// sendError is Express's default error handler outside production, without
// the stack trace.
func sendError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	doc := "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n<title>Error</title>\n</head>\n<body>\n<pre>" +
		shell.Esc(msg) + "</pre>\n</body>\n</html>\n"
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, doc)
	}
}

// sendJSON is res.status(status).json(v), minus Express's ETag. The fixture
// is a test double for the browser suite: no spec revalidates a JSON answer
// (fetch hides a 304 from page code anyway), and its canned rows carry
// password fields, so it does not hash its JSON bodies.
func sendJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	sendJSONBytes(w, r, status, marshal(v))
}

func sendJSONBytes(w http.ResponseWriter, r *http.Request, status int, payload []byte) {
	httpx.SendNoValidator(w, r, status, "application/json; charset=utf-8", payload)
}

// sendCredential is sendJSON for an answer that carries or sets a password,
// sent as the real server sends it: no-store, no ETag.
func sendCredential(w http.ResponseWriter, r *http.Request, status int, v any) {
	sendCredentialBytes(w, r, status, marshal(v))
}

func sendCredentialBytes(w http.ResponseWriter, r *http.Request, status int, payload []byte) {
	httpx.SendCredentialJSON(w, r, status, payload)
}

func sendHTML(w http.ResponseWriter, r *http.Request, status int, html string) {
	httpx.SendHTML(w, r, status, html)
}

func sendText(w http.ResponseWriter, r *http.Request, status int, text string) {
	httpx.Send(w, r, status, "text/plain; charset=utf-8", []byte(text))
}
