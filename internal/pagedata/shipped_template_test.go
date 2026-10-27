// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// The campaign dashboard printed DATA.lastRefreshed into its header with
// innerHTML, and its schema accepted any string up to 64 characters, so a data
// refresh could carry markup that ran in every viewer's browser. The stamp is
// now written as text and the schema admits only plain date-like text.
func TestCampaignDashboardRefreshStampIsText(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "templates", "nwm-campaign-dashboard", "template.html"))
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`innerHTML\s*=\s*CONFIG\.lastRefreshed`).Match(raw) {
		t.Fatal("lastRefreshed is written with innerHTML again")
	}
	if !regexp.MustCompile(`\$\("#stampRefresh"\)\.textContent\s*=\s*String\(CONFIG\.lastRefreshed\)`).Match(raw) {
		t.Fatal("lastRefreshed should be written with textContent")
	}
	m, err := ParseManaged(string(raw), TemplateSpec)
	if err != nil {
		t.Fatal(err)
	}
	data := m.Example
	for stamp, ok := range map[string]bool{
		"Jun 30, 2026":                 true,
		"2026-06-30 06:00":             true,
		"<img src=x onerror=alert(1)>": false,
		"Jun 30</span><b>":             false,
		`Jun 30 "quoted"`:              false,
	} {
		d := jsonv.ObjectOf()
		data.Range(func(k string, v any) bool { d.Set(k, v); return true })
		d.Set("lastRefreshed", stamp)
		if got := m.Validate.Valid(d); got != ok {
			t.Errorf("lastRefreshed %q: valid=%v, want %v", stamp, got, ok)
		}
	}
}
