// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package passwordvault

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/testdb"
)

const hash = "scrypt$16384$8$1$c2FsdA==$aGFzaA=="

func TestSealOpenRoundTrip(t *testing.T) {
	t.Setenv("PAGE_COOKIE_SECRET", "reveal-test-secret")
	s := Seal("Fabrikam-Q3!", hash)
	if !strings.HasPrefix(s, "v1.") || strings.Count(s, ".") != 3 {
		t.Fatalf("format %q", s)
	}
	if got, ok := Open(s, hash); !ok || got != "Fabrikam-Q3!" {
		t.Fatalf("open %q %v", got, ok)
	}
	if _, ok := Open(s, "scrypt$16384$8$1$b3RoZXI=$aGFzaA=="); ok {
		t.Fatal("bound to the hash it was sealed with")
	}
	tampered := s[:len(s)-2] + "AA"
	if _, ok := Open(tampered, hash); ok {
		t.Fatal("tampered tag")
	}
	for _, bad := range []string{"", "v2.a.b.c", "v1.a.b", "garbage"} {
		if _, ok := Open(bad, hash); ok {
			t.Errorf("%q opened", bad)
		}
	}
	t.Setenv("PAGE_COOKIE_SECRET", "another-secret")
	if _, ok := Open(s, hash); ok {
		t.Fatal("a changed key opens nothing")
	}
}

func TestNoKeyNoCopy(t *testing.T) {
	t.Setenv("PAGE_COOKIE_SECRET", "")
	if Seal("x", hash) != "" {
		t.Fatal("no key → no copy")
	}
	t.Setenv("PAGE_COOKIE_SECRET", "k")
	if Seal("", hash) != "" || Seal("x", "") != "" {
		t.Fatal("empty inputs → no copy")
	}
}

// A copy sealed by lib/passwordvault.js opens here.
func TestOpensNodeSealedCopy(t *testing.T) {
	t.Setenv("PAGE_COOKIE_SECRET", "fixture-secret")
	const node = "v1.s6jjzVEr3ZU6UvZG.xt9h1RKbm2GD3cG3uPLWuVAcA4J7.WUSMFxT0-Ue_QaJYYGrpSw"
	if got, ok := Open(node, hash); !ok || got != "Northwind-pass-é😀" {
		t.Fatalf("open node copy: %q %v", got, ok)
	}
}

func TestRememberOnSignIn(t *testing.T) {
	t.Setenv("PAGE_COOKIE_SECRET", "reveal-test-secret")
	d := testdb.New(t)
	ctx := context.Background()
	var pageID, portalID int64
	if err := d.QueryRow(ctx, `INSERT INTO pages (slug, password_hash) VALUES ('fabrikam-q3', $1) RETURNING id`, hash).Scan(&pageID); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(ctx, `INSERT INTO page_portals (slug, name, password_hash) VALUES ('fabrikam', 'Fabrikam', $1) RETURNING id`, hash).Scan(&portalID); err != nil {
		t.Fatal(err)
	}
	read := func(table string, id int64) *string {
		var s *string
		if err := d.QueryRow(ctx, `SELECT password_ciphertext FROM `+table+` WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	RememberOnSignIn(ctx, d, Page, pageID, hash, "pw-one")
	first := read("pages", pageID)
	if first == nil {
		t.Fatal("copy kept on sign-in")
	}
	if got, ok := Open(*first, hash); !ok || got != "pw-one" {
		t.Fatalf("copy opens: %q", got)
	}
	RememberOnSignIn(ctx, d, Page, pageID, hash, "pw-one")
	if again := read("pages", pageID); *again != *first {
		t.Fatal("an opening copy is left alone")
	}
	// A rotation that committed in between is never overwritten.
	RememberOnSignIn(ctx, d, Page, pageID, "scrypt$16384$8$1$c3RhbGU=$aGFzaA==", "stale")
	if again := read("pages", pageID); *again != *first {
		t.Fatal("stale hash must not write")
	}

	RememberOnSignIn(ctx, d, Portal, portalID, hash, "portal-pw")
	if got, ok := Open(*read("page_portals", portalID), hash); !ok || got != "portal-pw" {
		t.Fatal("portal copy")
	}
	RememberOnSignIn(ctx, d, Kind("bogus"), portalID, hash, "x") // no table, no write, no panic
}
