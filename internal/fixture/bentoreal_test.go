// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealBentoDeckRoutes(t *testing.T) {
	app := New(3210)
	t.Setenv(bentoDeckEnv, "")
	if rec := do(t, app, "GET", "/bento/real", "", ""); rec.Code != 404 {
		t.Fatalf("without %s: %d, want 404", bentoDeckEnv, rec.Code)
	}

	notDeck := filepath.Join(t.TempDir(), "plain.html")
	if err := os.WriteFile(notDeck, []byte("<!doctype html><p>hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bentoDeckEnv, notDeck)
	if rec := do(t, app, "GET", "/bento/real", "", ""); rec.Code != 422 {
		t.Fatalf("not a deck: %d, want 422", rec.Code)
	}

	deck := filepath.Join(t.TempDir(), "Northwind.bento.html")
	if err := os.WriteFile(deck, []byte(bentoDeck), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bentoDeckEnv, deck)
	rec := do(t, app, "GET", "/bento/real", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "data-pages-deck-host") {
		t.Fatalf("deck: %d %.200s", rec.Code, rec.Body.String())
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "blob:") {
		t.Fatalf("raw headers missing the blob: grant: %q", csp)
	}
	edit := do(t, app, "GET", "/bento/real-edit", "", "")
	if edit.Code != 200 || !strings.Contains(edit.Body.String(), "/bento/real-save") {
		t.Fatalf("edit session: %d", edit.Code)
	}
	if csp := edit.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src http://127.0.0.1:3210") {
		t.Fatalf("edit headers do not open the save channel: %q", csp)
	}

	save := do(t, app, "POST", "/bento/real-save", "text/html", bentoDeck)
	if save.Code != 201 {
		t.Fatalf("save: %d %s", save.Code, save.Body.String())
	}
	events := do(t, app, "GET", "/__fixture/events", "", "").Body.String()
	for _, want := range []string{`"path":"/bento/real-save"`, `"is_deck":true`, `"title":"Synthetic deck"`, `"collab_keys":false`} {
		if !strings.Contains(events, want) {
			t.Errorf("events lack %s: %s", want, events)
		}
	}
}
