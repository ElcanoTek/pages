// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package httpx

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // G505: Express's etag is a sha1 content hash, not a security primitive
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"strings"
)

// ── Responses (Express semantics) ─────────────────────────────────────────

// ETag is Express's default weak ETag for a body.
func ETag(body []byte) string {
	sum := sha1.Sum(body) //nolint:gosec // content hash for caching, as Express computes it
	hash := base64.StdEncoding.EncodeToString(sum[:])[:27]
	return `W/"` + strconv.FormatInt(int64(len(body)), 16) + "-" + hash + `"`
}

var noCache = func(cc string) bool {
	for _, part := range strings.Split(cc, ",") {
		if strings.EqualFold(strings.TrimSpace(part), "no-cache") {
			return true
		}
	}
	return false
}

// fresh is the `fresh` package against an ETag (Pages never sets
// Last-Modified on these responses).
func fresh(r *http.Request, etag string) bool {
	ims := r.Header.Get("If-Modified-Since")
	inm := Header(r, "If-None-Match")
	if ims == "" && inm == "" {
		return false
	}
	if cc := Header(r, "Cache-Control"); cc != "" && noCache(cc) {
		return false
	}
	if inm != "" {
		if inm == "*" {
			return true
		}
		for _, m := range strings.Split(inm, ",") {
			m = strings.TrimSpace(m)
			if m == etag || "W/"+m == etag || m == "W/"+etag {
				return true
			}
		}
		return false
	}
	// If-Modified-Since without a Last-Modified is always stale.
	return false
}

// Send is Express's res.status(status).send(body) for a body whose
// Content-Type is already chosen: Content-Length, weak ETag, 304 when the
// request is fresh, no body for HEAD.
func Send(w http.ResponseWriter, r *http.Request, status int, contentType string, body []byte) {
	h := w.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", contentType)
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	etag := h.Get("ETag")
	if etag == "" {
		etag = ETag(body)
		h.Set("ETag", etag)
	}
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && status >= 200 && status < 300 && fresh(r, etag) {
		status = http.StatusNotModified
	}
	if status == http.StatusNoContent || status == http.StatusNotModified {
		h.Del("Content-Type")
		h.Del("Content-Length")
		h.Del("Transfer-Encoding")
		body = nil
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead && len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// SendHTML is res.status(status).send(html) (or .type("html").send).
//
// Every HTML response is Cache-Control: no-store. The dashboard's shells carry
// the signed-in email and a CSRF token, and an error page answering an asset
// request must not inherit that asset's long-lived cache policy; no HTML Pages
// serves is meant to be stored. Static assets go through their own handler.
func SendHTML(w http.ResponseWriter, r *http.Request, status int, html string) {
	w.Header().Set("Cache-Control", "no-store")
	Send(w, r, status, "text/html; charset=utf-8", []byte(html))
}

// Marshal is JSON.stringify for the plain values Pages sends: no HTML
// escaping (encoding/json escapes <>& by default; JavaScript does not) and no
// trailing newline.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// SendJSON is res.status(status).json(v).
func SendJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := Marshal(v)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	Send(w, r, status, "application/json; charset=utf-8", body)
}

// SendJSONBytes is SendJSON for a body already serialized (JSON.stringify
// output built by hand or with jsonv).
func SendJSONBytes(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	Send(w, r, status, "application/json; charset=utf-8", body)
}

// SendCredentialJSON is SendJSONBytes for a body that carries a credential
// or answers a request that set one (a revealed, generated or just-set
// password): Cache-Control: no-store and NO ETag. Express would hash the
// body into a weak ETag, and a SHA-1 of a body holding a password, seen in a
// proxy log or a cache, lets a guess be confirmed offline. With no validator
// there is no 304 either; the body is always sent (except to HEAD).
func SendCredentialJSON(w http.ResponseWriter, r *http.Request, status int, body []byte) {
	w.Header().Set("Cache-Control", "no-store")
	SendNoValidator(w, r, status, "application/json; charset=utf-8", body)
}

// SendNoValidator is Send without the ETag: Content-Type (unless already
// set), Content-Length, no body for HEAD (or 204). The body is never hashed
// and never answered with a 304. For answers that must not carry a hash of
// their body (credentials) or that a validator can never serve (a 429:
// freshness only ever turns a 2xx into a 304).
func SendNoValidator(w http.ResponseWriter, r *http.Request, status int, contentType string, body []byte) {
	h := w.Header()
	if h.Get("Content-Type") == "" {
		h.Set("Content-Type", contentType)
	}
	h.Del("ETag")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if status == http.StatusNoContent {
		h.Del("Content-Type")
		h.Del("Content-Length")
		body = nil
	}
	w.WriteHeader(status)
	if r.Method != http.MethodHead && len(body) > 0 {
		_, _ = w.Write(body)
	}
}

// WriteJSON is res.status(status).json(...) where no request is at hand to
// check freshness against (a POST answer, an error envelope): JSON content
// type with charset, weak ETag, Content-Length, the body always written.
func WriteJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("ETag", ETag(body))
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

var statusText = map[int]string{
	301: "Moved Permanently", 302: "Found", 303: "See Other", 307: "Temporary Redirect", 308: "Permanent Redirect",
}

// escapeHTML is the escape-html package: html.EscapeString (the sanitizer
// static analysis recognizes) with its &#34; spelled &quot;.
func escapeHTML(s string) string {
	out := html.EscapeString(s)
	if strings.Contains(out, "&#34;") {
		out = strings.ReplaceAll(out, "&#34;", "&quot;")
	}
	return out
}

// Redirect is Express 5's res.redirect(status, url): Location through
// encodeurl, a text or HTML body chosen by Accept, Vary: Accept.
func Redirect(w http.ResponseWriter, r *http.Request, status int, url string) {
	address := EncodeURL(url)
	h := w.Header()
	h.Set("Location", address)
	h.Add("Vary", "Accept")
	msg := statusText[status]
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch negotiate(Header(r, "Accept"), []string{"text/plain", "text/html"}) {
	case "text/plain":
		redirectText(w, r, status, msg+". Redirecting to "+address)
	case "text/html":
		redirectHTML(w, r, status, msg, address)
	default:
		redirectEmpty(w, status)
	}
}

// redirectText writes Redirect's text/plain body. The address is not
// escaped (Express does not), which is safe only because this body is never
// served as HTML: the content type is fixed here, in the one function that
// writes it.
func redirectText(w http.ResponseWriter, r *http.Request, status int, body string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// redirectHTML writes Redirect's HTML body with the address HTML-escaped.
func redirectHTML(w http.ResponseWriter, r *http.Request, status int, msg, address string) {
	body := "<!DOCTYPE html><head><title>" + msg + "</title></head><body><p>" + msg + ". Redirecting to " + escapeHTML(address) + "</p></body>"
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
}

// redirectEmpty answers an Accept that takes neither offer: no body, as
// Express sends.
func redirectEmpty(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// negotiate is the negotiator package's preferred media type among offers
// (in offer order on ties), "" when Accept excludes them all. No Accept
// header accepts anything, so the first offer wins.
func negotiate(accept string, offers []string) string {
	if strings.TrimSpace(accept) == "" {
		return offers[0]
	}
	type rng struct {
		typ, sub string
		q        float64
		idx      int
	}
	var ranges []rng
	for i, part := range strings.Split(accept, ",") {
		fields := strings.Split(part, ";")
		mt := strings.ToLower(strings.TrimSpace(fields[0]))
		typ, sub, ok := strings.Cut(mt, "/")
		if !ok {
			continue
		}
		q := 1.0
		for _, p := range fields[1:] {
			k, v, _ := strings.Cut(strings.TrimSpace(p), "=")
			if strings.EqualFold(strings.TrimSpace(k), "q") {
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
					q = f
				}
			}
		}
		ranges = append(ranges, rng{typ, sub, q, i})
	}
	best, bestQ, bestS, bestO := "", 0.0, -1, 0
	for _, offer := range offers {
		typ, sub, _ := strings.Cut(offer, "/")
		q, s, o := 0.0, -1, -1
		for _, rg := range ranges {
			spec := 0
			switch {
			case rg.typ == typ && rg.sub == sub:
				spec = 6
			case rg.typ == typ && rg.sub == "*":
				spec = 4
			case rg.typ == "*" && rg.sub == "*":
				spec = 0
			default:
				continue
			}
			if spec > s || (spec == s && rg.q > q) {
				q, s, o = rg.q, spec, rg.idx
			}
		}
		if q <= 0 {
			continue
		}
		if best == "" || q > bestQ || (q == bestQ && s > bestS) || (q == bestQ && s == bestS && o < bestO) {
			best, bestQ, bestS, bestO = offer, q, s, o
		}
	}
	return best
}

// IsLocalPath reports whether s is a same-origin path a redirect may carry:
// one leading "/" that is not "//" or "/\" (browsers read both as a
// protocol-relative URL to another host), and no control character
// (browsers drop tab and newline from a URL, so "/\t/evil.example" would
// become "//evil.example").
func IsLocalPath(s string) bool {
	if !strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") || strings.HasPrefix(s, `/\`) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	return true
}
