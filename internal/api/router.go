// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package api

import (
	"net/http"
	"regexp"

	"github.com/ElcanoTek/pages/internal/route"
)

// Express routing (package route): case-insensitive against the raw path,
// optional trailing slash, greedy `*slug`, decoded params, first match wins,
// GET answers HEAD, automatic OPTIONS. Anything else falls through (to the
// dashboard's 404).

type params = route.Params

type handlerFunc = func(h *handler, w http.ResponseWriter, r *http.Request, p params) error

type apiRoute = route.Route[handlerFunc]

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
