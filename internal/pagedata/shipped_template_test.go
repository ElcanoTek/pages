// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
		`Jun 30 (partial) 'q'`:         true,
		"Jun 30 &amp;":                 false,
	} {
		d := jsonv.ObjectOf()
		data.Range(func(k string, v any) bool { d.Set(k, v); return true })
		d.Set("lastRefreshed", stamp)
		if got := m.Validate.Valid(d); got != ok {
			t.Errorf("lastRefreshed %q: valid=%v, want %v", stamp, got, ok)
		}
	}
}

// legacyCampaignDashboard rebuilds the campaign dashboard as revisions before
// the fix shipped it: the innerHTML sink and a lastRefreshed schema with no
// pattern. Pages built from those revisions keep that source.
func legacyCampaignDashboard(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "templates", "nwm-campaign-dashboard", "template.html"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, r := range [][2]string{
		{`if(CONFIG.lastRefreshed){$("#stampRefresh").textContent=String(CONFIG.lastRefreshed);}else{$("#stampRefresh").innerHTML='<span class="pending">never</span>';}`,
			`$("#stampRefresh").innerHTML=CONFIG.lastRefreshed?CONFIG.lastRefreshed:'<span class="pending">never</span>';`},
		{`,"pattern":"^[^<>&]*$"}`, `}`},
		{`CONFIG.channels.map(c=>esc(c.name)+" \u2014 "+esc(c.kpiLabel)+" at "`, `CONFIG.channels.map(c=>c.name+" \u2014 "+c.kpiLabel+" at "`},
	} {
		if !strings.Contains(s, r[0]) {
			t.Fatalf("template no longer contains %q", r[0])
		}
		s = strings.Replace(s, r[0], r[1], 1)
	}
	return s
}

// A page built from a pre-fix revision still renders lastRefreshed with
// innerHTML, so data writes to it refuse a stamp that could form markup,
// while ordinary stamps keep refreshing.
func TestLegacyCampaignDashboardRefusesMarkupStamps(t *testing.T) {
	m, err := ParseManaged(legacyCampaignDashboard(t), TemplateSpec)
	if err != nil {
		t.Fatal(err)
	}
	withStamp := func(stamp string) *jsonv.Object {
		d := jsonv.ObjectOf()
		m.Example.Range(func(k string, v any) bool { d.Set(k, v); return true })
		d.Set("lastRefreshed", stamp)
		return d
	}
	for stamp, ok := range map[string]bool{
		"Aug 2, 2026":                  true,
		"2026-09-21T06:02:11.000Z":     true,
		"Aug 2 (partial)":              true,
		"<img src=x onerror=alert(1)>": false,
		"Aug 2 &lt;b&gt;":              false,
	} {
		d := withStamp(stamp)
		if !m.Validate.Valid(d) {
			t.Fatalf("%q: the legacy schema itself should accept it", stamp)
		}
		_, err := MaterializeBlocks(m, BlockUpdate{Data: d, SetData: true}, MaterializeOptions{SourceAsOf: "2026-01-01T00:00:00Z"})
		perr := ValidateProposedData(m, d)
		if ok && (err != nil || perr != nil) {
			t.Fatalf("%q refused: %v / %v", stamp, err, perr)
		}
		if !ok && (code(err) != "data_validation_failed" || code(perr) != "data_validation_failed") {
			t.Fatalf("%q accepted on a legacy page: %v / %v", stamp, err, perr)
		}
	}
}

// The same revisions concatenated channel names and KPI labels into the
// header's KPI note with innerHTML, so config writes to such a page refuse
// labels that could form markup. The current revision escapes them and
// takes any label.
func TestLegacyCampaignDashboardRefusesMarkupChannelLabels(t *testing.T) {
	legacy, err := ParseManaged(legacyCampaignDashboard(t), TemplateSpec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "templates", "nwm-campaign-dashboard", "template.html"))
	if err != nil {
		t.Fatal(err)
	}
	current, err := ParseManaged(string(raw), TemplateSpec)
	if err != nil {
		t.Fatal(err)
	}
	withLabel := func(m *Managed, field, label string) *jsonv.Object {
		c := mustParse(t, jsonv.Stringify(m.Config)).(*jsonv.Object)
		c.Value("channels").([]any)[0].(*jsonv.Object).Set(field, label)
		return c
	}
	for _, field := range []string{"name", "kpiLabel"} {
		for label, ok := range map[string]bool{
			"Display (O&O)":                true,
			"a<b":                          false,
			"Display (open web)":           true,
			"CPM 'net'":                    true,
			"<img src=x onerror=alert(1)>": false,
		} {
			_, err := MaterializeBlocks(legacy, BlockUpdate{Config: withLabel(legacy, field, label), SetConfig: true}, MaterializeOptions{})
			if ok && err != nil {
				t.Fatalf("legacy %s %q refused: %v", field, label, err)
			}
			if !ok && code(err) != "config_validation_failed" {
				t.Fatalf("legacy %s %q accepted: %v", field, label, err)
			}
			if _, err := MaterializeBlocks(current, BlockUpdate{Config: withLabel(current, field, label), SetConfig: true}, MaterializeOptions{}); err != nil {
				t.Fatalf("current %s %q refused: %v", field, label, err)
			}
		}
	}
}
