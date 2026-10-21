// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package compose

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// Middleware wraps a handler (requireAdminJSON, requireCsrf).
type Middleware func(http.Handler) http.Handler

// MaxBodyBytes caps a POST body; 0 (the default) means the dashboard's
// request limit, MAX_HTML_BYTES (datalimits.MaxRequestBytes), read per request.
var MaxBodyBytes int64

func bodyLimit() int64 {
	if MaxBodyBytes > 0 {
		return MaxBodyBytes
	}
	return int64(datalimits.MaxRequestBytes())
}

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
		httpx.SendJSON(w, r, http.StatusNotFound, errorBody{"not found", "not_found"})
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
			httpx.SendJSON(w, r, http.StatusRequestEntityTooLarge, errorBody{"request body too large", "body_too_large"})
		} else {
			httpx.SendJSON(w, r, http.StatusBadRequest, errorBody{"invalid JSON request body", "bad_json"})
		}
		return
	}
	str := func(k string) (string, bool) { s, ok := b[k].(string); return s, ok }
	prompt, _ := str("prompt")
	prompt = jsstr.Trim(prompt)
	rawSlug, _ := str("slug")
	slug := strings.ToLower(jsstr.Trim(rawSlug))
	title, _ := str("title")
	title = firstUTF16(jsstr.Trim(title), 200)
	mode := "create"
	if m, _ := str("mode"); m == "revise" {
		mode = "revise"
	}

	if prompt == "" {
		msg := "prompt is required"
		if mode == "revise" {
			msg = "describe what to change"
		}
		httpx.SendJSON(w, r, http.StatusBadRequest, errorBody{msg, "no_prompt"})
		return
	}
	if jsstr.UTF16Len(prompt) > maxPromptUnits {
		httpx.SendJSON(w, r, http.StatusBadRequest, errorBody{"prompt too long (200k max)", "too_long"})
		return
	}
	if !slugRE.MatchString(slug) {
		httpx.SendJSON(w, r, http.StatusBadRequest, errorBody{"slug must be a lowercase page path", "bad_slug"})
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
	httpx.SendJSON(w, r, http.StatusAccepted, startResponse{JobID: id, Driver: drv})
}

// pollJob is GET /compose/{id} — job status and log tail (the panel polls it).
func pollJob(w http.ResponseWriter, r *http.Request) {
	jobsMu.Lock()
	j := jobs[r.PathValue("id")]
	jobsMu.Unlock()
	if j == nil {
		httpx.SendJSON(w, r, http.StatusNotFound, errorBody{"no such job", "no_job"})
		return
	}
	j.mu.Lock()
	out := pollResponse{Status: j.status, Code: j.code, Slug: j.slug, Title: j.title, Driver: j.driver, Log: strings.Join(j.log, "\n")}
	j.mu.Unlock()
	httpx.SendJSON(w, r, http.StatusOK, out)
}

// decodeBody is express.json(): a body whose Content-Type is not
// application/json is ignored (an empty object); a JSON body must be an
// object or array (strict mode); an array decodes to no fields.
func decodeBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/json" || r.Body == nil {
		return map[string]any{}, nil
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit()))
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
