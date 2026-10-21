// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package compose

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // G505: the Express-compatible ETag is a cache validator, not a security boundary
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf16"
)

// Middleware wraps a handler (requireAdminJSON, requireCsrf).
type Middleware func(http.Handler) http.Handler

// MaxBodyBytes caps a POST body; the server should set it to the dashboard's
// request limit (MAX_HTML_BYTES, 2 MB by default).
var MaxBodyBytes int64 = 2 * 1024 * 1024

// maxPromptUnits is the prompt cap in UTF-16 code units (JS string length).
// Generous: a pasted MOC protocol YAML is tens of KB. Still bounded.
const maxPromptUnits = 200000

var (
	jobsMu sync.Mutex
	jobs   = map[string]*job{}
	seq    int
)

// Handler serves POST {Prefix} (start a run) and GET {Prefix}/{id} (poll a
// job). Mount it at both Prefix and Prefix+"/" — and only when Enabled():
//
//	h := compose.Handler(csrf.RequireAdminJSON, csrf.RequireCSRF)
//	mux.Handle(compose.Prefix, h)
//	mux.Handle(compose.Prefix+"/", h)
//
// Every request passes requireAdminJSON; POST also passes requireCSRF (which
// must run after it). The handler decodes the JSON body itself.
func Handler(requireAdminJSON, requireCSRF Middleware) http.Handler {
	mux := http.NewServeMux()
	start := requireCSRF(http.HandlerFunc(startJob))
	mux.Handle("POST "+Prefix, start)
	mux.Handle("POST "+Prefix+"/{$}", start)
	mux.HandleFunc("GET "+Prefix+"/{id}", pollJob)
	mux.HandleFunc("GET "+Prefix+"/{id}/{$}", pollJob)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, r, http.StatusNotFound, errorBody{"not found", "not_found"})
	})
	return requireAdminJSON(mux)
}

type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

type startResponse struct {
	JobID  string `json:"jobId"`
	Driver string `json:"driver"`
}

type pollResponse struct {
	Status string `json:"status"`
	Code   *int   `json:"code"`
	Slug   string `json:"slug"`
	Title  string `json:"title"`
	Driver string `json:"driver"`
	Log    string `json:"log"`
}

// startJob is POST /compose — start a run via the configured driver.
func startJob(w http.ResponseWriter, r *http.Request) {
	b, err := decodeBody(w, r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, r, http.StatusRequestEntityTooLarge, errorBody{"request body too large", "body_too_large"})
		} else {
			writeJSON(w, r, http.StatusBadRequest, errorBody{"invalid JSON request body", "bad_json"})
		}
		return
	}
	str := func(k string) (string, bool) { s, ok := b[k].(string); return s, ok }
	prompt, _ := str("prompt")
	prompt = jsTrim(prompt)
	rawSlug, _ := str("slug")
	slug := strings.ToLower(jsTrim(rawSlug))
	title, _ := str("title")
	title = firstUTF16(jsTrim(title), 200)
	mode := "create"
	if m, _ := str("mode"); m == "revise" {
		mode = "revise"
	}

	if prompt == "" {
		msg := "prompt is required"
		if mode == "revise" {
			msg = "describe what to change"
		}
		writeJSON(w, r, http.StatusBadRequest, errorBody{msg, "no_prompt"})
		return
	}
	if utf16Len(prompt) > maxPromptUnits {
		writeJSON(w, r, http.StatusBadRequest, errorBody{"prompt too long (200k max)", "too_long"})
		return
	}
	if !slugRE.MatchString(slug) {
		writeJSON(w, r, http.StatusBadRequest, errorBody{"slug must be a lowercase page path", "bad_slug"})
		return
	}

	drv := driver()
	jobsMu.Lock()
	seq++
	id := strconv.Itoa(seq)
	j := &job{id: id, driver: drv, mode: mode, status: "running", slug: slug, title: title}
	jobs[id] = j
	jobsMu.Unlock()

	task := buildTask(prompt, slug, title, mode)
	if drv == "moc" {
		startMoc(j, task)
	} else {
		startSpawn(j, task)
	}
	writeJSON(w, r, http.StatusAccepted, startResponse{JobID: id, Driver: drv})
}

// pollJob is GET /compose/{id} — job status and log tail (the panel polls it).
func pollJob(w http.ResponseWriter, r *http.Request) {
	jobsMu.Lock()
	j := jobs[r.PathValue("id")]
	jobsMu.Unlock()
	if j == nil {
		writeJSON(w, r, http.StatusNotFound, errorBody{"no such job", "no_job"})
		return
	}
	j.mu.Lock()
	out := pollResponse{Status: j.status, Code: j.code, Slug: j.slug, Title: j.title, Driver: j.driver, Log: strings.Join(j.log, "\n")}
	j.mu.Unlock()
	writeJSON(w, r, http.StatusOK, out)
}

// decodeBody is express.json(): a body whose Content-Type is not
// application/json is ignored (an empty object); a JSON body must be an
// object or array (strict mode); an array decodes to no fields.
func decodeBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" || r.Body == nil {
		return map[string]any{}, nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return nil, errors.New("strict JSON body must be an object or array")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// writeJSON is Express's res.status(n).json(v): JSON without HTML escaping,
// Content-Type application/json; charset=utf-8, and the weak ETag Express
// derives from the body (W/"<len hex>-<base64 sha1, 27 chars>"), answering a
// matching If-None-Match on a 2xx GET/HEAD with 304.
func writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	sum := sha1.Sum(body) //nolint:gosec // G401: see the import
	etag := `W/"` + strconv.FormatInt(int64(len(body)), 16) + "-" + base64.StdEncoding.EncodeToString(sum[:])[:27] + `"`
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("ETag", etag)
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && status >= 200 && status < 300 && etagMatches(r.Header.Get("If-None-Match"), etag) {
		h.Del("Content-Type")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func etagMatches(header, etag string) bool {
	if header == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	strong := strings.TrimPrefix(etag, "W/")
	for _, tag := range strings.Split(header, ",") {
		tag = strings.TrimSpace(tag)
		if tag == etag || strings.TrimPrefix(tag, "W/") == strong {
			return true
		}
	}
	return false
}

// jsTrim is String.prototype.trim: Unicode white space plus U+FEFF.
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF })
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// firstUTF16 is s.slice(0, n) in UTF-16 code units. A surrogate pair the cut
// would split is dropped whole (Go strings cannot hold a lone surrogate).
func firstUTF16(s string, n int) string {
	units := 0
	for i, r := range s {
		u := utf16.RuneLen(r)
		if u < 0 {
			u = 1
		}
		if units+u > n {
			return s[:i]
		}
		units += u
	}
	return s
}
