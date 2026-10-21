// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package route is Express routing as far as a client can observe it (router
// 2.2 + path-to-regexp 8), shared by the REST API (internal/api) and the admin
// API (internal/adminapi): paths match case-insensitively against the raw
// (percent-encoded) path, a trailing slash is optional, a `*slug` splat is
// greedy across segments and `:id` / `:name` are one segment. Matched
// parameters are percent-decoded (so an encoded %2F inside one segment
// decodes to "/"); a malformed escape is an error for that request. Callers
// try routes in registration order and the first whose path AND method match
// handles the request; a GET route also answers HEAD. An OPTIONS request no
// route handles is answered with the Allow list of every route whose path
// matched (ServeOptions).
package route

import (
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/httpx"
)

// Params are a matched route's decoded parameters ("" when the path has no
// such parameter).
type Params struct {
	Slug string // *slug: every remaining segment, joined by "/"
	ID   string // :id
	Name string // :name
}

// Route is one registered route. H is the caller's handler type.
type Route[H any] struct {
	Method  string
	Re      *regexp.Regexp
	Handler H
}

// New compiles an Express path (literal segments, `*slug`, `:id`, `:name`)
// into a route.
func New[H any](method, path string, handler H) Route[H] {
	return Route[H]{Method: method, Re: Pattern(path), Handler: handler}
}

// Pattern compiles an Express path to path-to-regexp's anchored,
// case-insensitive, non-strict expression.
func Pattern(path string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(?is)^`)
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		b.WriteString("/")
		switch {
		case seg == "*slug":
			b.WriteString(`(?P<slug>.+)`)
		case strings.HasPrefix(seg, ":"):
			b.WriteString(`(?P<` + seg[1:] + `>[^/]+)`)
		default:
			b.WriteString(regexp.QuoteMeta(seg))
		}
	}
	b.WriteString(`/?$`)
	return regexp.MustCompile(b.String())
}

// ErrBadParam is a parameter whose percent-encoding does not decode.
var ErrBadParam = errors.New("failed to decode param")

// Match decodes the route's captures from path (the router-relative escaped
// path), or reports no match. matched is true with ErrBadParam when the path
// matched but a parameter does not decode.
func (rt *Route[H]) Match(path string) (p Params, matched bool, err error) {
	m := rt.Re.FindStringSubmatch(path)
	if m == nil {
		return Params{}, false, nil
	}
	for i, name := range rt.Re.SubexpNames() {
		if name == "" {
			continue
		}
		// Express decodes each "/"-separated piece of a splat and joins
		// them back with "/": the same as decoding the whole, since an
		// escape never spans a "/".
		v, err := httpx.DecodeURIComponent(m[i])
		if err != nil {
			return Params{}, true, ErrBadParam
		}
		switch name {
		case "slug":
			p.Slug = v
		case "id":
			p.ID = v
		case "name":
			p.Name = v
		}
	}
	return p, true, nil
}

// Handles reports whether the route answers method (GET also answers HEAD).
func (rt *Route[H]) Handles(method string) bool {
	return rt.Method == method || (method == http.MethodHead && rt.Method == http.MethodGet)
}

// Methods are the methods the route contributes to an OPTIONS Allow list.
func (rt *Route[H]) Methods() []string {
	if rt.Method == http.MethodGet {
		return []string{http.MethodGet, http.MethodHead}
	}
	return []string{rt.Method}
}

// ServeOptions is router 2.2's automatic OPTIONS answer: the sorted, de-duped
// Allow list as a text/plain body.
func ServeOptions(w http.ResponseWriter, methods []string) {
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
