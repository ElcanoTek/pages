// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pageurls

import (
	"encoding/json"
	"testing"
)

func TestFor(t *testing.T) {
	t.Setenv("DASHBOARD_HOST", "pages.northwind.test")
	t.Setenv("CONTENT_HOST", "content.northwind.test")
	t.Setenv("DASHBOARD_ORIGIN", "")
	t.Setenv("CONTENT_ORIGIN", "")
	got, err := json.Marshal(For("northwind/q2"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"admin":"https://pages.northwind.test/admin/northwind/q2","view":"https://pages.northwind.test/view/northwind/q2","live":"https://content.northwind.test/northwind/q2"}`
	if string(got) != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}

	t.Setenv("DASHBOARD_ORIGIN", "http://localhost:3000")
	t.Setenv("CONTENT_ORIGIN", "http://content.localhost:3000")
	u := For("acme")
	if u.Admin != "http://localhost:3000/admin/acme" || u.View != "http://localhost:3000/view/acme" || u.Live != "http://content.localhost:3000/acme" {
		t.Fatalf("explicit origins: %+v", u)
	}
}
