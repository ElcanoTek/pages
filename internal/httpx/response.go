// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package httpx

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // G505: Express's etag is a sha1 content hash, not a security primitive
	"encoding/base64"
	"encoding/json"
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
func SendHTML(w http.ResponseWriter, r *http.Request, status int, html string) {
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

func escapeHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;").Replace(s)
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
	var body string
	switch negotiate(Header(r, "Accept"), []string{"text/plain", "text/html"}) {
	case "text/plain":
		h.Set("Content-Type", "text/plain; charset=utf-8")
		body = msg + ". Redirecting to " + address
	case "text/html":
		h.Set("Content-Type", "text/html; charset=utf-8")
		u := escapeHTML(address)
		body = "<!DOCTYPE html><head><title>" + msg + "</title></head><body><p>" + msg + ". Redirecting to " + u + "</p></body>"
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write([]byte(body))
	}
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
