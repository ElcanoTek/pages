// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package render

// What Pages knows about a Bento deck's DOCUMENT, and the edit session that
// lets a staff member save one back (lib/bento.js).
//
// A deck (bento.page; the .bento.html fleet's bento-slides skill produces) is
// one file that is its own viewer and editor: a compressed runtime around one
// JSON document block, <script type="application/bento+json" id="bento-doc">.
//
// Why a save channel at all: a deck opens in Bento's full editor on the live
// page, but the content host forbids every request back to Pages (connect-src
// 'none', no cookies, opaque origin) — so Save could only ever produce a file.
// The edit session is the ONE response that may talk back: it is served for a
// signed, staff-minted, page-bound token; its CSP names Pages' own origin and no
// other; and Bento's Save — every entry point of it, since they all end in a
// blob download — is intercepted and POSTed to Pages instead, where it becomes a
// DRAFT version attributed to the staff member. Nothing here reaches the stored
// bytes or the vendored shell: it is all serve-time, and the deploy path strips
// every tag this file adds.

import (
	"regexp"
	"strings"

	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

// DocBlockRE matches the document block. Submatches: 1 the opening tag, 2 the
// JSON text, 3 the closing tag — the offsets needed to write it back in place.
var DocBlockRE = regexp.MustCompile(`(<` + fold("script") + `\b[^>]*\b` + fold("type") + sp + `*=` + sp + `*(?:(?:"` + bentoTypeValue + `"|'` + bentoTypeValue + `')[^>]*>|` + bentoTypeValue + `(?:>|` + sp + `[^>]*>)))([\s\S]*?)(</` + fold("script") + sp + `*>)`)

// ReadDoc returns the parsed document (encoding/json shapes: map[string]any,
// []any, float64, …) and true, or false when there is no block or it does not
// parse. Never fails: callers use it to decide, not to fail. A block holding
// JSON null reads as (nil, false), as it did in the JS.
func ReadDoc(html string) (any, bool) {
	m := DocBlockRE.FindStringSubmatch(html)
	if m == nil {
		return nil, false
	}
	v, err := jsonv.ParseString(m[2])
	if err != nil || v == nil {
		return nil, false
	}
	return toGo(v), true
}

// toGo converts a jsonv value to the encoding/json shapes ReadDoc promises.
func toGo(v any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, el := range x {
			out[i] = toGo(el)
		}
		return out
	case *jsonv.Object:
		out := make(map[string]any, x.Len())
		x.Range(func(k string, el any) bool {
			out[k] = toGo(el)
			return true
		})
		return out
	}
	return v
}

// StripCollab removes the live-collaboration block from a deck's document.
//
// Bento writes a `collab` block — a room URL and a private key — into any file
// saved from its own UI, and a file saved from the hosted editor is exactly what
// the save channel receives. On Pages that block can never be used (both CSPs
// forbid connections), so it is key material in a shareable document and
// nothing else; fleet's own bento_doc.py drops it for the same reason. The rest
// of the document is re-serialised as JSON.stringify would, then escaped (`<` as
// <) — the escaping the block requires and the encoding fleet's helper
// uses. docId and every other key are carried across untouched. A block that
// does not parse is left alone rather than risked.
func StripCollab(html string) (out string, stripped bool) {
	loc := DocBlockRE.FindStringSubmatchIndex(html)
	if loc == nil {
		return html, false
	}
	// Lossless: a lone surrogate escape in the document is written back as
	// the same escape, as JSON.stringify(JSON.parse(...)) would.
	parsed, err := jsonv.ParseLossless([]byte(html[loc[4]:loc[5]]))
	doc, ok := parsed.(*jsonv.Object)
	if err != nil || !ok || !doc.Has("collab") {
		return html, false
	}
	doc.Delete("collab")
	return html[:loc[0]] + html[loc[2]:loc[3]] + datalimits.EscapedJSON(doc) + html[loc[6]:loc[7]] + html[loc[1]:], true
}

// EditSessionOptions configure an edit session.
type EditSessionOptions struct {
	// SaveURL is where the save channel POSTs the edited deck.
	SaveURL string
	// Token is the signed edit token, sent as a Bearer credential.
	Token string
	// VersionID is the base version the edit started from; "" means none (null).
	VersionID string
	// ContentOrigin is this host's origin, the one connect-src is widened to.
	ContentOrigin string
}

// EditSessionScript is the save channel, injected into an edit-session
// response only.
//
// Every one of Bento's save entry points — the Save button, ⌘S, Save as → Save
// a copy — ends the same way on this host: the serialised deck goes into a
// text/html Blob, an <a download> is created for its object URL, and click() is
// called on it (the host adaptation already made the file-picker path
// unreachable). Intercepting that one step therefore catches all of them
// without touching Bento's code: the Blob's text is POSTed to Pages with the
// edit token; on success the download is suppressed; on failure a fresh
// download is started from the captured bytes, even if Bento has already
// revoked its own URL. Same attribute as the host adaptation, so the
// deploy-time strip removes it, and it removes itself from the DOM so a file
// that leaves carries nothing of Pages.
func EditSessionScript(saveURL, token, versionID string) string {
	version := "null"
	if versionID != "" {
		version = jsonv.Quote(versionID)
	}
	config := strings.ReplaceAll(`{"saveUrl":`+jsonv.Quote(saveURL)+`,"token":`+jsonv.Quote(token)+`,"versionId":`+version+`}`, "<", `\u003c`)
	return `<script data-pages-deck-host>(function(){var s=document.currentScript;var cfg=` + config + `;var saves=Promise.resolve();var saveBlocked=false;` +
		`if(!(window.origin==="null"&&/^https?:$/.test(location.protocol))){if(s&&s.parentNode)s.parentNode.removeChild(s);return}` +
		// toast: a shadow root, so the deck's own styles cannot reach it and vice versa
		`var host=null;function say(text,kind){if(!host){host=document.createElement("div");host.setAttribute("data-pages-save-toast","");` +
		`host.style.cssText="position:fixed;left:50%;bottom:24px;transform:translateX(-50%);z-index:2147483647";var r=host.attachShadow({mode:"open"});` +
		`r.innerHTML="<style>p{margin:0;padding:10px 16px;border-radius:8px;font:14px/1.4 system-ui,sans-serif;color:#fff;background:#1c2b3a;box-shadow:0 8px 24px rgba(0,0,0,.35);max-width:min(36rem,90vw)}p.ok{background:#1f6b3a}p.err{background:#8a2b2b}</style><p></p>";` +
		`document.body.appendChild(host)}var p=host.shadowRoot.querySelector("p");p.className=kind||"";p.textContent=text;host.hidden=false;clearTimeout(host._t);host._t=setTimeout(function(){host.hidden=true},kind==="err"?12000:6000)}` +
		// remember every text/html blob by its object URL
		`var blobs=new Map();var cou=URL.createObjectURL;URL.createObjectURL=function(b){var u=cou.apply(this,arguments);if(b instanceof Blob&&/^text\/html/.test(b.type))blobs.set(u,b);return u};` +
		`var rou=URL.revokeObjectURL;URL.revokeObjectURL=function(u){blobs.delete(String(u));return rou.apply(this,arguments)};` +
		`var clickA=HTMLAnchorElement.prototype.click;` +
		// Use the original create/click functions so fallback never re-enters Save.
		// Give the browser time to consume the download URL before releasing it.
		`function download(blob,name){var a=document.createElement("a");var u=cou.call(URL,blob);a.href=u;a.download=name;` +
		`try{document.body.appendChild(a);clickA.call(a)}finally{a.remove();setTimeout(function(){rou.call(URL,u)},1000)}}` +
		`HTMLAnchorElement.prototype.click=function(){var a=this;if(!a.download||!blobs.has(a.href)){return clickA.apply(a,arguments)}` +
		`var url=a.href;var blob=blobs.get(url);var filename=a.download;blobs.delete(url);say("Saving to Pages\u2026");` +
		`saves=saves.then(function(){if(saveBlocked)throw new Error("Reopen the editor from the admin to continue saving to Pages");return blob.text().then(function(text){return fetch(cfg.saveUrl,{method:"POST",headers:{"Authorization":"Bearer "+cfg.token,"Content-Type":"text/html; charset=utf-8","X-Pages-Base-Version":cfg.versionId||""},body:text,credentials:"omit"})})` +
		`.then(function(res){return res.text().then(function(body){var data={};try{data=JSON.parse(body)}catch(e){}if(!res.ok){if(res.status===409||res.status===403)saveBlocked=true;throw new Error(data.error||("Pages answered "+res.status))}return data})})` +
		`.then(function(data){cfg.versionId=String(data.version_id||cfg.versionId||"");say(data.deduped?"Nothing changed since the last save.":"Saved to Pages as a new draft version. Publish it from the admin when it is ready.","ok")})` +
		`}).catch(function(err){var reason="Couldn\u2019t save to Pages ("+(err&&err.message?err.message:err)+"). ";` +
		`try{download(blob,filename);say(reason+"Starting a download of your file instead \u2014 keep it before closing the editor, then upload it as a new version.","err")}` +
		`catch(e){say(reason+"Couldn\u2019t start the download. Keep this editor open and try Save again.","err")}})` +
		`.finally(function(){rou.call(URL,url)});` +
		`return undefined};` +
		`if(s&&s.parentNode)s.parentNode.removeChild(s)})();</script>`
}

// EditSession is the bytes served for an edit token: the stored deck, the host
// adaptation AdaptDeckToHost always adds, the deck's OWN guard CSP widened from
// connect-src 'none' to Pages' origin (the response header is widened the same
// way by the csp package's raw edit headers; both must agree or the save is
// refused by whichever is stricter), and the save channel. Everything added
// carries data-pages-deck-host and is stripped at deploy.
func EditSession(html string, opts EditSessionOptions) string {
	adapted := AdaptDeckToHost(html)
	// The guard meta's content is double-quoted and full of single-quoted
	// keywords, so match the whole tag and edit inside it.
	widened := replaceFirst(guardMetaRE, adapted, func(tag string) string {
		return strings.Replace(tag, "connect-src 'none'", "connect-src "+opts.ContentOrigin, 1)
	})
	script := EditSessionScript(opts.SaveURL, opts.Token, opts.VersionID)
	// Directly after the host adaptation, which sits right after <head>.
	from := strings.Index(widened, "data-pages-deck-host")
	at := -1
	if from == -1 {
		// indexOf(x, -1) searches from 0.
		from = 0
	}
	if i := strings.Index(widened[from:], "</script>"); i != -1 {
		at = from + i
	}
	if at == -1 {
		return script + widened
	}
	return widened[:at+9] + script + widened[at+9:]
}
