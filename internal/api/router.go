// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/httpx"
)

// Express routing, as far as a client can observe it (router 2.2 +
// path-to-regexp 8): paths match case-insensitively against the raw
// (percent-encoded) path, a trailing slash is optional, a `*slug` splat is
// greedy across segments and `:id` is one segment. Matched parameters are
// percent-decoded; a malformed escape is an error for that request. Routes
// are tried in registration order and the first whose path AND method match
// handles the request; a GET route also answers HEAD. An OPTIONS request no
// route handles is answered with the Allow list of every route whose path
// matched. Anything else falls through (to the dashboard's 404).

type params struct {
	slug string
	id   string
}

type route struct {
	method  string
	re      *regexp.Regexp
	handler func(h *handler, w http.ResponseWriter, r *http.Request, p params) error
}

// pattern compiles an Express path (only the literal, `*slug` and `:id`
// forms this router uses) to path-to-regexp's anchored, case-insensitive,
// non-strict expression.
func pattern(path string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(?is)^`)
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		b.WriteString("/")
		switch seg {
		case "*slug":
			b.WriteString(`(?P<slug>.+)`)
		case ":id":
			b.WriteString(`(?P<id>[^/]+)`)
		default:
			b.WriteString(regexp.QuoteMeta(seg))
		}
	}
	b.WriteString(`/?$`)
	return regexp.MustCompile(b.String())
}

// errBadParam is a parameter whose percent-encoding does not decode.
var errBadParam = errors.New("failed to decode param")

// match decodes re's captures from path, or reports no match.
func (rt *route) match(path string) (params, bool, error) {
	m := rt.re.FindStringSubmatch(path)
	if m == nil {
		return params{}, false, nil
	}
	var p params
	for i, name := range rt.re.SubexpNames() {
		if name == "" {
			continue
		}
		// Express decodes each "/"-separated piece of a splat and joins
		// them back with "/": the same as decoding the whole, since an
		// escape never spans a "/".
		v, err := httpx.DecodeURIComponent(m[i])
		if err != nil {
			return params{}, true, errBadParam
		}
		switch name {
		case "slug":
			p.slug = v
		case "id":
			p.id = v
		}
	}
	return p, true, nil
}

func (rt *route) handles(method string) bool {
	return rt.method == method || (method == http.MethodHead && rt.method == http.MethodGet)
}

func (rt *route) methods() []string {
	if rt.method == http.MethodGet {
		return []string{http.MethodGet, http.MethodHead}
	}
	return []string{rt.method}
}

// serveOptions is router 2.2's automatic OPTIONS answer.
func serveOptions(w http.ResponseWriter, methods []string) {
	seen := map[string]bool{}
	var list []string
	for _, m := range methods {
		if !seen[m] {
			seen[m] = true
			list = append(list, m)
		}
	}
	sort.Strings(list)
	allow := strings.Join(list, ", ")
	h := w.Header()
	h.Set("Allow", allow)
	h.Set("Content-Length", strconv.Itoa(len(allow)))
	h.Set("Content-Type", "text/plain")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(allow))
}

// mountRE is app.use("/api/v1", …): case-insensitive, a whole segment.
var mountRE = regexp.MustCompile(`(?i)^/api/v1(?:/|$)`)

// Prefix is where the server mounts Handler.
const Prefix = "/api/v1"

// routerPath is the path the router sees below the mount (Express's req.url
// with the mount stripped), or ok=false when r is not under /api/v1.
func routerPath(r *http.Request) (string, bool) {
	p := r.URL.EscapedPath()
	if !mountRE.MatchString(p) {
		return "", false
	}
	rest := p[len(Prefix):]
	if rest == "" {
		rest = "/"
	}
	return rest, true
}
