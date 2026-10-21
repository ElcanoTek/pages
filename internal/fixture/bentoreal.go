// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/render"
)

// ── a REAL Bento deck (test/browser/bento-deck-real.spec.js) ─────────────────
// The synthetic deck above proves the boot mechanism; it cannot prove that the
// real 650KB application boots, saves and exports under the same headers, and
// that application is not vendored here. PAGES_FIXTURE_BENTO_DECK names a
// .bento.html on disk; these routes serve it exactly as the synthetic deck is
// served, read on every request so a reused fixture server picks up a new
// file. Without the variable they answer 404 and the spec skips.

// bentoDeckEnv names the deck file the real-deck routes serve.
const bentoDeckEnv = "PAGES_FIXTURE_BENTO_DECK"

var bentoTitleRE = regexp.MustCompile(`"title"\s*:\s*"([^"]*)"`)
var bentoDocRE = regexp.MustCompile(`(?is)application/bento\+json[^>]*>(.*?)</script`)
var bentoCollabRE = regexp.MustCompile(`"collab"\s*:\s*\{`)

// realDeck reads the deck named by PAGES_FIXTURE_BENTO_DECK, answering the
// request itself (and returning ok=false) when there is none to serve.
func realDeck(w http.ResponseWriter, r *req) (string, bool) {
	file := os.Getenv(bentoDeckEnv)
	if file == "" {
		sendText(w, r.Request, http.StatusNotFound, bentoDeckEnv+" is not set")
		return "", false
	}
	b, err := os.ReadFile(file) //nolint:gosec // G703: the operator names the file in the test harness environment
	if err != nil {
		sendText(w, r.Request, http.StatusNotFound, err.Error())
		return "", false
	}
	if !render.IsBentoDeck(string(b)) {
		sendText(w, r.Request, http.StatusUnprocessableEntity, `not a Bento deck: no <script type="application/bento+json"> block`)
		return "", false
	}
	return string(b), true
}

func (a *App) realBentoRoutes() []route {
	serve := func(headers func() csp.Headers, body func(deck string) string) handler {
		return func(w http.ResponseWriter, r *req) {
			deck, ok := realDeck(w, r)
			if !ok {
				return
			}
			headers().Apply(w.Header())
			sendHTML(w, r.Request, http.StatusOK, body(deck))
		}
	}
	options := func(w http.ResponseWriter, r *req) {
		saveCors(w)
		w.WriteHeader(http.StatusNoContent)
	}
	return []route{
		{method: "GET", pattern: "/bento/real", h: serve(csp.RawHeaders, func(deck string) string {
			return renderPage(deck, "raw", nil, "")
		})},
		{method: "GET", pattern: "/bento/real-edit", h: serve(a.editHeaders, func(deck string) string {
			return render.EditSession(deck, render.EditSessionOptions{
				SaveURL:       a.origin + "/bento/real-save",
				Token:         "fixture-edit-token",
				VersionID:     "1",
				ContentOrigin: a.origin,
			})
		})},
		{method: "OPTIONS", pattern: "/bento/real-save", h: options},
		// What the real runtime's Save delivered through the edit channel.
		{method: "POST", pattern: "/bento/real-save", textOK: true, h: func(w http.ResponseWriter, r *req) {
			saveCors(w)
			text, _ := r.body.(string)
			doc := ""
			if m := bentoDocRE.FindStringSubmatch(text); m != nil {
				doc = m[1]
			}
			var title any
			if m := bentoTitleRE.FindStringSubmatch(text); m != nil {
				title = m[1]
			}
			a.mu.Lock()
			a.st.events.push(o("method", "POST", "path", "/bento/real-save", "slug", nil, "body", o(
				"authorization", headerOrNull(r.Request, "Authorization"),
				"origin", headerOrNull(r.Request, "Origin"),
				"cookie", headerOrNull(r.Request, "Cookie"),
				"bytes", len(text),
				"is_deck", isDeck(r.body),
				"title", title,
				"collab_keys", bentoCollabRE.MatchString(doc),
				"tags_carried", strings.Contains(text, "data-pages-deck-host"),
			)))
			a.mu.Unlock()
			sendJSON(w, r.Request, http.StatusCreated, o("version_id", "77", "status", "draft", "deduped", false, "collab_keys_removed", false))
		}},
	}
}
