// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package render

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/env"
	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

const emptyDoc = `{"format":"bento/slides","version":1,"slides":[]}`

func bentoDeckHTML() string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><title>Deck</title>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline' blob:; connect-src 'none'">
<script id="fleet-offline-deck">(function(){try{localStorage.setItem("bento-offline","on")}catch(e){}})();</script>
<script type="application/bento+json" id="bento-doc">` + emptyDoc + `</script>
<script id="bento-rt" type="bento/deflate-b64">c8vPz1EoLSjOSczJyU9V0FIoLSktKSpJUUhJVEgQMAAA</script>
</head><body><div id="bento-splash"></div><script>(async()=>{var js="";var url=URL.createObjectURL(new Blob([js],{type:"text/javascript"}));await import(url)})()</script></body></html>`
}

func TestIsBentoDeck(t *testing.T) {
	deck := bentoDeckHTML()
	for i, c := range []struct {
		html string
		want bool
	}{
		{deck, true},
		{strings.Replace(deck, `"application/bento+json"`, `'application/bento+json'`, 1), true},
		{strings.Replace(deck, `type="application/bento+json" id="bento-doc"`, `id="bento-doc" type=application/bento+json`, 1), true},
		{"<p>a page that mentions application/bento+json in prose</p>", false},
		{`<script type="application/json" id="bento-doc">{}</script>`, false},
		{"", false},
	} {
		if got := IsBentoDeck(c.html); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

func TestDeckServedAsStoredPlusAdaptation(t *testing.T) {
	html := bentoDeckHTML()
	nav := &navPayload{
		Portal: navPortal{Slug: "p", Name: "P", URL: "https://x/portal/p"},
		Pages:  []navPage{{Slug: "a", Title: "A", URL: "https://x/a", Current: true}, {Slug: "b", Title: "B", URL: "https://x/b"}},
	}
	served := RenderVersion(Renderable{RenderMode: "raw", HTML: html})
	if strings.Count(served, "data-pages-deck-host") != 1 {
		t.Error("exactly one adaptation")
	}
	if strings.Index(served, "data-pages-deck-host") > strings.Index(served, "bento-doc") {
		t.Error("ahead of the document and the bootstrap")
	}
	mustContain(t, served, "showSaveFilePicker", "picker removal")
	if StripDeckHostAdaptation(served) != html {
		t.Error("strip(served) != stored")
	}
	if RenderVersion(Renderable{RenderMode: "raw", HTML: html, Nav: nav}) != served {
		t.Error("no Page menu over the deck's toolbar")
	}
	if RenderVersion(Renderable{RenderMode: "themed", HTML: html, OverrideCSS: ":root{--x:1}", Nav: nav}) != served {
		t.Error("no Flag head ahead of the deck's own CSP")
	}
	if RenderVersion(Renderable{RenderMode: "raw", HTML: served}) != served {
		t.Error("idempotent")
	}
	reserialised := strings.Replace(served, "<script data-pages-deck-host>", `<script data-pages-deck-host="">`, 1)
	if StripDeckHostAdaptation(reserialised) != html {
		t.Error("browser-serialised form stripped")
	}
	if strings.Count(RenderVersion(Renderable{RenderMode: "raw", HTML: reserialised}), "data-pages-deck-host") != 1 {
		t.Error("never doubled")
	}
	mustContain(t, RenderVersion(Renderable{RenderMode: "raw", HTML: "<html><head></head><body><p>dash</p></body></html>", Nav: nav}), "pgnav", "ordinary raw page keeps its menu")
}

var docBlockText = regexp.MustCompile(`(?i)<script\b[^>]*bento\+json[^>]*>([\s\S]*?)</script>`)

func TestStripCollab(t *testing.T) {
	doc := `{"format":"bento/slides","version":1,"docId":"d-1","title":"T \u003cb>x\u003c/b>","slides":[{"id":"s1","elements":[]}],"collab":{"room":"wss://sync.bento.page/d/abc","key":"secret"}}`
	html := strings.Replace(bentoDeckHTML(), emptyDoc, doc, 1)
	out, stripped := StripCollab(html)
	if !stripped {
		t.Fatal("not stripped")
	}
	afterAny, ok := ReadDoc(out)
	if !ok {
		t.Fatal("unreadable after strip")
	}
	after := afterAny.(map[string]any)
	if _, has := after["collab"]; has {
		t.Error("collab survived")
	}
	if after["docId"] != "d-1" {
		t.Error("docId is never regenerated")
	}
	if !reflect.DeepEqual(after["slides"], []any{map[string]any{"id": "s1", "elements": []any{}}}) {
		t.Errorf("slides: %#v", after["slides"])
	}
	if after["title"] != "T <b>x</b>" {
		t.Errorf("title: %q", after["title"])
	}
	block := docBlockText.FindStringSubmatch(out)[1]
	if strings.Contains(block, "<") {
		t.Error("raw < inside the document block")
	}
	if strings.Replace(out, block, "", 1) != strings.Replace(html, docBlockText.FindStringSubmatch(html)[1], "", 1) {
		t.Error("bytes outside the block changed")
	}
	plain := bentoDeckHTML()
	if o, s := StripCollab(plain); o != plain || s {
		t.Error("no collab: untouched")
	}
	broken := strings.Replace(plain, emptyDoc, "{not json", 1)
	if o, s := StripCollab(broken); o != broken || s {
		t.Error("not JSON: left alone")
	}
}

func TestEditSession(t *testing.T) {
	origin := env.ContentOrigin()
	html := bentoDeckHTML()
	session := EditSession(html, EditSessionOptions{SaveURL: origin + "/raw/team/guide/versions", Token: "T.sig", VersionID: "41", ContentOrigin: origin})
	meta := regexp.MustCompile(`<meta[^>]*Content-Security-Policy[^>]*>`).FindString(session)
	mustContain(t, meta, "connect-src "+origin, "widened guard")
	mustContain(t, meta, "default-src 'none'", "rest of fleet's guard")
	if strings.Count(session, "data-pages-deck-host") != 2 {
		t.Error("two Pages tags")
	}
	if !regexp.MustCompile(`Authorization[^;]*Bearer`).MatchString(session) {
		t.Error("the save carries the token")
	}
	if !regexp.MustCompile(`(?i)publish it from the admin`).MatchString(session) {
		t.Error("says a save is a draft")
	}
	if StripDeckHostAdaptation(session) != html {
		t.Error("strip(edit session) != stored bytes")
	}
	body := regexp.MustCompile(`(?i)^<script[^>]*>|</script>$`).ReplaceAllString(EditSessionScript("u", "t<x", "1"), "")
	if _, err := js.Parse(parse.NewInputString(body), js.Options{}); err != nil {
		t.Errorf("save channel does not parse: %v", err)
	}
	if regexp.MustCompile(`(?i)</script`).MatchString(body) {
		t.Error("raw </script in the script body")
	}
}
