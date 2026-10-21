// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package fixture is the deterministic browser fixture the Playwright suite
// (test/browser/*.spec.js) runs against: real server-rendered shells and real
// browser assets, backed by an in-memory implementation of the unchanged admin
// payloads. Database and state-machine truth stay covered by the integration
// suites; this package exists so a browser can drive every screen with no
// Postgres, no SSO and no clock.
//
// It is a port of the Node release's Express fixture and answers byte-for-byte as
// that Express app did: the same routes, the same seeded pages, portals,
// templates and versions, the same fixed NOW, and the same mutation rules.
// `pages browser-fixture` serves it.
package fixture

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// NOW is the fixture's fixed clock: every seeded timestamp is computed from it
// so copy such as "3 days ago" is the same string on every run.
const NOW = "2026-07-22T09:30:00.000Z"

var nowTime = mustTime(NOW)

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

// isoTime is Date#toISOString.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// iso is NOW minus minutesAgo, as an ISO string.
func iso(minutesAgo float64) string {
	return isoTime(nowTime.Add(-time.Duration(minutesAgo * float64(time.Minute))))
}

// App is the fixture server. Its state is process-wide and shared by every
// spec, exactly as the JavaScript module state was; specs reset it through
// the /__fixture/* routes.
type App struct {
	port   int
	origin string
	routes []route

	mu sync.Mutex
	st *state
	// Portal fixtures live outside state: /__fixture/reset leaves them alone
	// and /__fixture/reset-portals restores them.
	portals           *list
	portalSeed        *list
	membersByPortal   map[string]*list
	portalDetailDelay map[string]float64
	portalListDelay   float64
}

// New returns the fixture app for a server listening on 127.0.0.1:port. The
// port is part of the payloads (absolute preview, portal and live URLs).
func New(port int) *App {
	a := &App{port: port, origin: "http://127.0.0.1:" + strconv.Itoa(port)}
	a.st = initialState()
	a.portals = a.initialPortals()
	a.portalSeed = clone(a.portals).(*list)
	a.resetPortalMembers()
	a.portalDetailDelay = map[string]float64{}
	a.routes = a.buildRoutes()
	return a
}

// ServeHTTP dispatches in registration order, as Express does.
func (a *App) ServeHTTP(w http.ResponseWriter, hr *http.Request) {
	body, ok := parseBody(w, hr, false)
	if !ok {
		return
	}
	raw := hr.URL.EscapedPath()
	r := &req{Request: hr, body: body}
	for _, rt := range a.routes {
		if !rt.methodMatches(hr.Method) {
			continue
		}
		params, rest, shape, ok := rt.match(raw)
		if !shape {
			continue
		}
		if !ok {
			sendError(w, hr, http.StatusBadRequest, "URIError: Failed to decode param")
			return
		}
		if rt.textOK {
			if _, none := r.body.(undefined); none {
				if r.body, ok = parseBody(w, hr, true); !ok {
					return
				}
			}
		}
		r.params, r.rest = params, rest
		if rt.mw != nil {
			if rt.mw(w, r) {
				continue // next()
			}
			return
		}
		rt.h(w, r)
		return
	}
	sendHTML(w, hr, http.StatusNotFound, notFoundShell())
}

// sleep is setTimeout's delay; NaN or a non-positive delay is immediate.
func sleep(ms float64) {
	if ms > 0 {
		time.Sleep(time.Duration(ms * float64(time.Millisecond)))
	}
}
