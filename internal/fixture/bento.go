// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/csp"
	"github.com/ElcanoTek/pages/internal/httpx"
	"github.com/ElcanoTek/pages/internal/render"
)

// ── Bento decks (test/browser/bento-deck.spec.js) ────────────────────────────
// A stand-in with the REAL boot mechanism and a tiny payload. A Bento deck
// keeps its stylesheet and 650KB runtime as deflate-raw + base64 blocks that a
// bootstrap inflates with DecompressionStream and import()s from a blob URL;
// the bootstrap below is that code, copied from an upstream v1.0.18 deck. That
// mechanism is the part Pages has to get right — the blob: grant in the raw
// headers and the verbatim, no-switcher render — and it is testable without
// vendoring the app. test/manual/bento-deck-check.js runs the same probes
// against a real deck.
const bentoBootstrap = `(async () => {
  var fail = function (msg) {
    var d = document.createElement('div')
    d.style.cssText = 'position:fixed;inset:0;display:flex;align-items:center;justify-content:center;background:#0D1B2E;color:#F2F0EA;font:16px/1.6 sans-serif;text-align:center;padding:40px;z-index:99999'
    d.innerHTML = msg
    document.body.appendChild(d)
    var s = document.getElementById('bento-splash'); if (s) s.remove()
  }
  if (typeof DecompressionStream === 'undefined') {
    fail('This file needs a browser from 2023 or later.')
    return
  }
  var inflate = async function (id) {
    var b64 = document.getElementById(id).textContent.trim()
    var bytes = Uint8Array.from(atob(b64), function (c) { return c.charCodeAt(0) })
    var ds = new DecompressionStream('deflate-raw')
    var stream = new Blob([bytes]).stream().pipeThrough(ds)
    return await new Response(stream).text()
  }
  try {
    var css = await inflate('bento-rt-css')
    var st = document.createElement('style')
    st.id = 'bento-rt-style'
    st.textContent = css
    document.head.appendChild(st)
    var js = await inflate('bento-rt')
    var url = URL.createObjectURL(new Blob([js], { type: 'text/javascript' }))
    await import(url)
  } catch (e) {
    fail('This file could not start: ' + (e && e.message ? e.message : e))
  }
})()`

// The deck's stylesheet and runtime, and their deflate-raw + base64 blocks.
// The blocks are the bytes Node's zlib.deflateRawSync produced for them (Go's
// compress/flate picks different, equally valid, bytes), so the deck is
// byte-for-byte the one the JavaScript fixture served; TestBentoBlocksInflate
// pins that each still inflates to its source.
const (
	bentoCSS = "body{margin:0;background:#0D1B2E;color:#F2F0EA;font:20px/1.4 sans-serif}#booted{padding:48px}"
	// Save mirrors Bento's own path exactly: serialise the LIVE DOM into a
	// text/html Blob, make an <a download> for its object URL, click it. That
	// is the one step the edit session intercepts, so this is what exercises
	// it.
	bentoRuntime = "window.__bentoBooted = true;\n" +
		"const s = document.getElementById('bento-splash'); if (s) s.remove();\n" +
		"const h = document.createElement('h1'); h.id = 'booted'; h.textContent = 'Synthetic deck booted'; document.body.appendChild(h);\n" +
		"const b = document.createElement('button'); b.id = 'save'; b.type = 'button'; b.textContent = 'Save';\n" +
		"b.addEventListener('click', () => {\n" +
		"  const html = '<!DOCTYPE html>' + document.documentElement.outerHTML;\n" +
		"  const a = document.createElement('a'); a.href = URL.createObjectURL(new Blob([html], { type: 'text/html' })); a.download = 'Synthetic.bento.html'; a.click();\n" +
		"});\n" +
		"document.body.appendChild(b);"
	bentoCSSBlock     = "BcFBCoAgEADArwSeK5MOsZ6K7B/alki1K1pQRH9vxjE+72GTDwRSOztvPvFFCEKOzaCMnnnnBGJSkzS9XplOUDLedVO1RbaUy7yksH7CMZ8LvtEiBvLQdvH+fg=="
	bentoRuntimeBlock = "fVFdT8IwFH3nV1yf2kWs8dU5HkASTWYwCA/GGNKuFzcZ7bLdMRfCfzetY34k8tbenns+eprMaNuI1UqhITu2llBDBFTWGA4SayqCCiLQNqm3aEi8IU1zdMdxe68582sXVZHLKmVBCNkaeBVAJUrc2h3y4MiS/mRJSpSEHRFn6ZVbTUXmpJnyJpgbEH7QxBpCQ+7lqTWUImUJaEw20AN7XmV1K2RRoNGTNMs1T3t9dUJf1UTWOA+q81DJHTJ3pbZAb+oL4kd/THnoQAmp9XSHhuKsIjRYcpbkWbJhQ+ABRCPYDwC6v6Bt7lZvzm5nk8Xz49RPRgzOvy0eD51JYWvC8m7xEIc9jTwRSbo0UqQlriGC5TzuADP1jgkt5zE32MA4t4q/OPHXIezBhb0G5gJeuiGDQ+BptG1MbqX+VYLw3QsPdCCf1hV+CMLB/5WoIPwE"
)

// bentoDeck has the same head order as a fleet-produced deck: CSP meta,
// guard, document, blocks.
var bentoDeck = `<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1.0" />
    <title>Synthetic deck</title>
    <meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline' blob:; style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; object-src 'none'; frame-src 'none'; base-uri 'none'; form-action 'none'">
    <script id="fleet-offline-deck">(function () { try { localStorage.setItem("bento-offline", "on"); } catch (e) {} })();</script>
    <script type="application/bento+json" id="bento-doc">{"format":"bento/slides","version":1,"title":"Synthetic deck","slides":[]}</script>
    <script id="bento-rt-css" type="bento/deflate-b64">` + bentoCSSBlock + `</script>
    <script id="bento-rt" type="bento/deflate-b64">` + bentoRuntimeBlock + `</script>
  </head>
  <body>
    <div id="bento-splash">Loading…</div>
    <script>` + bentoBootstrap + `</script>
  </body>
</html>`

// editHeaders: the raw edit headers name CONTENT_ORIGIN, which is the
// production host; the fixture is its own origin, so it applies the same
// one-directive change to the raw headers itself. Everything else about the
// response is the real thing.
func (a *App) editHeaders() csp.Headers {
	return withCSP(csp.RawHeaders(), func(v string) string {
		return replaceFirst(v, "connect-src 'none'", "connect-src "+a.origin)
	})
}

// withoutBlob is the header as it was before blob: was granted. Pinned so the
// grant cannot be "tidied away" without this route saying exactly what
// breaks.
var scriptBlobRE = regexp.MustCompile(`script-src ([^;]*) blob:`)

func withoutBlob() csp.Headers {
	return withCSP(csp.RawHeaders(), func(v string) string {
		v = replaceFirst(v, " blob:;", ";")
		if loc := scriptBlobRE.FindStringSubmatchIndex(v); loc != nil {
			v = v[:loc[0]] + "script-src " + v[loc[2]:loc[3]] + v[loc[1]:]
		}
		return v
	})
}

// saveCors is the save channel's CORS, as the server answers it: the opaque
// origin may read the reply.
func saveCors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "null")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Pages-Base-Version")
	h.Set("Vary", "Origin")
}

func headerOrNull(r *http.Request, name string) any {
	if v := httpx.Header(r, name); v != "" {
		return v
	}
	return nil
}

func (a *App) bentoRoutes() []route {
	raw := func(nav func() obj) handler {
		return withHeaders(csp.RawHeaders, html(http.StatusOK, func(*req) string {
			var n obj
			if nav != nil {
				n = nav()
			}
			return renderPage(bentoDeck, "raw", n, "")
		}))
	}
	options := func(w http.ResponseWriter, r *req) {
		saveCors(w)
		w.WriteHeader(http.StatusNoContent)
	}
	// Unescaped, as new RegExp(`connect-src ${FIXTURE_ORIGIN}`) was.
	guardWidened := regexp.MustCompile("connect-src " + a.origin)
	return []route{
		// The deck's bytes as a file a human would have saved from the editor,
		// for the admin upload spec to hand to the source editor's file input.
		{method: "GET", pattern: "/bento/deck-source", h: func(w http.ResponseWriter, r *req) {
			sendText(w, r.Request, http.StatusOK, bentoDeck)
		}},
		// Served exactly as the content host would: the deck's stored bytes
		// through RenderVersion, under the real raw headers.
		{method: "GET", pattern: "/bento/deck", h: raw(nil)},
		// In a portal. A raw dashboard gets the Page menu here; a deck must not.
		{method: "GET", pattern: "/bento/deck-portal", h: raw(func() obj { return fixtureNav(navPages(), false) })},
		// ── the edit session and its save channel ──────────────────────────
		{method: "GET", pattern: "/bento/edit", h: withHeaders(a.editHeaders, html(http.StatusOK, func(r *req) string {
			save := "/bento/save"
			if q := httpx.QueryParam(r.Request, "fail"); q.Present && (len(q.Values) > 1 || q.Values[0] != "") {
				save = "/bento/save-fail"
			}
			return render.EditSession(bentoDeck, render.EditSessionOptions{
				SaveURL:       a.origin + save,
				Token:         "fixture-edit-token",
				VersionID:     "41",
				ContentOrigin: a.origin,
			})
		}))},
		{method: "OPTIONS", pattern: "/bento/save", h: options},
		{method: "OPTIONS", pattern: "/bento/save-fail", h: options},
		// The save channel, and a record of exactly what arrived — the token,
		// the Origin the browser sent, whether any cookie rode along, and
		// whether the body is a deck — so the spec can assert the channel's
		// shape, not just its outcome.
		{method: "POST", pattern: "/bento/save", textOK: true, h: func(w http.ResponseWriter, r *req) {
			saveCors(w)
			text := ""
			if s, ok := r.body.(string); ok {
				text = s
			}
			a.mu.Lock()
			a.st.events.push(o("method", "POST", "path", "/bento/save", "slug", nil, "body", o(
				"authorization", headerOrNull(r.Request, "Authorization"),
				"origin", headerOrNull(r.Request, "Origin"),
				"cookie", headerOrNull(r.Request, "Cookie"),
				"base_version", headerOrNull(r.Request, "X-Pages-Base-Version"),
				"bytes", len(text),
				"is_deck", isDeck(r.body),
				"guard_widened", guardWidened.MatchString(text),
				"tags_carried", strings.Contains(text, "data-pages-deck-host"),
			)))
			a.mu.Unlock()
			sendJSON(w, r.Request, http.StatusCreated, o("version_id", "42", "status", "draft", "deduped", false, "collab_keys_removed", false))
		}},
		{method: "POST", pattern: "/bento/save-fail", textOK: true, h: func(w http.ResponseWriter, r *req) {
			saveCors(w)
			sendJSON(w, r.Request, http.StatusInternalServerError, errorBody("fixture: Pages could not store the version"))
		}},
		{method: "GET", pattern: "/bento/deck-without-blob", h: withHeaders(withoutBlob, html(http.StatusOK, func(*req) string {
			return renderPage(bentoDeck, "raw", nil, "")
		}))},
	}
}
