// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/render"
)

// The server halves of the browser/server pins in test/frontend.test.js. The
// browser module's value is read from the embedded asset (the bytes the
// binary serves) and checked against the server's own.

var maxSortOrderRE = regexp.MustCompile(`(?m)^const MAX_SORT_ORDER = (\d+);`)

func constIn(t *testing.T, src, file string) string {
	t.Helper()
	m := maxSortOrderRE.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("%s declares no `const MAX_SORT_ORDER = N;`", file)
	}
	return m[1]
}

// TestBrowserMaxSortOrderMatchesServer: the portal screen appends members
// without proposing a sort_order the write would reject, so its ceiling must
// be the API's.
//
// TODO(internal/portals): the Go portals package does not exist yet, so the
// server value is read from lib/portals.js. Once it does, compare against its
// exported constant instead and drop the lib/ read.
func TestBrowserMaxSortOrderMatchesServer(t *testing.T) {
	browser, err := fs.ReadFile(pages.Public(), "shell-assets/portals.js")
	if err != nil {
		t.Fatal(err)
	}
	server, err := os.ReadFile("../../lib/portals.js")
	if err != nil {
		t.Skipf("lib/portals.js is gone; pin against internal/portals instead: %v", err)
	}
	b := constIn(t, string(browser), "public/shell-assets/portals.js")
	s := constIn(t, string(server), "lib/portals.js")
	if b != s {
		t.Fatalf("browser MAX_SORT_ORDER %s != server %s", b, s)
	}
	if b != "9999" {
		t.Fatalf("MAX_SORT_ORDER is %s; test/frontend.test.js pins 9999 — update both", b)
	}
}

func bentoDeckHTML() string {
	return `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><title>Deck</title>
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'unsafe-inline' blob:; connect-src 'none'">
<script id="fleet-offline-deck">(function(){try{localStorage.setItem("bento-offline","on")}catch(e){}})();</script>
<script type="application/bento+json" id="bento-doc">{"format":"bento/slides","version":1,"slides":[]}</script>
<script id="bento-rt" type="bento/deflate-b64">c8vPz1EoLSjOSczJyU9V0FIoLSktKSpJUUhJVEgQMAAA</script>
</head><body><div id="bento-splash"></div><script>(async()=>{var js="";var url=URL.createObjectURL(new Blob([js],{type:"text/javascript"}));await import(url)})()</script></body></html>`
}

// TestBentoRecogniserTable: the admin picks Raw for a deck before the server
// would refuse Themed, so the browser's looksLikeBentoDeck and the server's
// IsBentoDeck must answer alike. test/frontend.test.js checks the browser
// against this same table.
func TestBentoRecogniserTable(t *testing.T) {
	deck := bentoDeckHTML()
	samples := []struct {
		html string
		want bool
	}{
		{deck, true},
		{strings.Replace(deck, `"application/bento+json"`, `'application/bento+json'`, 1), true},
		{strings.Replace(deck, `type="application/bento+json" id="bento-doc"`, `id="bento-doc" type=application/bento+json`, 1), true},
		{"<p>a page that mentions application/bento+json in prose</p>", false},
		{`<script type="application/json" id="bento-doc">{}</script>`, false},
		{"<!doctype html><html><body><h1>Plain</h1></body></html>", false},
		{"", false},
	}
	for i, s := range samples {
		if got := render.IsBentoDeck(s.html); got != s.want {
			t.Errorf("sample %d: IsBentoDeck = %v, want %v", i, got, s.want)
		}
	}
}
