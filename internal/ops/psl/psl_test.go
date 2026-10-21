// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package psl

import "testing"

func TestRegistrableDomain(t *testing.T) {
	for host, want := range map[string]string{
		"pages.contoso.example":     "contoso.example",
		"cdn.pages.contoso.example": "contoso.example",
		"pages.contoso.co.uk":       "contoso.co.uk",
		"cdn.contoso.co.uk":         "contoso.co.uk",
		// com.ng is a two-level public suffix: a shortlist would say com.ng.
		"pages.contoso.com.ng":   "contoso.com.ng",
		"pages.northwind.com.ng": "northwind.com.ng",
		// *.ck wildcard, with the !www.ck exception winning the tie.
		"foo.www.ck":         "www.ck",
		"foo.northwind.ck":   "foo.northwind.ck",
		"Pages.Contoso.COM.": "contoso.com",
		"co.uk":              "co.uk",
		"localhost":          "localhost",
		"":                   "",
	} {
		if got := RegistrableDomain(host); got != want {
			t.Errorf("RegistrableDomain(%q) = %q, want %q", host, got, want)
		}
	}
	if !SameSite("pages.contoso.co.uk", "cdn.contoso.co.uk") || SameSite("pages.contoso.com.ng", "pages.northwind.com.ng") {
		t.Fatal("SameSite")
	}
}
