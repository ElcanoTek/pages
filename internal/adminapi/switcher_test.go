// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package adminapi

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// switcherScript loads the browser module and runs its pure model over the
// admin index payload on stdin.
const switcherScript = `
const sw = require(process.argv[1]);
let input = "";
process.stdin.on("data", (d) => (input += d));
process.stdin.on("end", () => {
  const m = sw.model(JSON.parse(input).pages, "switch-15");
  process.stdout.write(JSON.stringify({
    total: m.total,
    current: m.current, previous: m.previous, next: m.next,
    nested: m.items.find((i) => i.slug === "adm/nest") || null,
  }));
});`

// checkPageSwitcher flows the REAL /api/v1/admin/pages response through the
// same pure model admin.js uses (public/shell-assets/page-switcher.js). A
// 35-page run covers the long native select case while also proving active
// state, deterministic neighbours, status context, and nested-slug-safe
// direct navigation.
//
// The payload contract the module relies on is asserted in Go regardless;
// running the module itself needs node and is skipped without it.
func checkPageSwitcher(t *testing.T, body string) {
	t.Helper()
	var payload struct {
		Pages []map[string]any `json:"pages"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Pages) < 39 {
		t.Fatalf("long list includes all seeded admin pages: %d", len(payload.Pages))
	}
	for _, p := range payload.Pages {
		if s, _ := p["slug"].(string); s == "" {
			t.Fatalf("every row has a slug: %v", p)
		}
		for _, k := range []string{"title", "disabled", "is_live", "published_version_id", "require_approval", "workspace_id", "workspace_name"} {
			if _, ok := p[k]; !ok {
				t.Fatalf("row %v lacks %s, which page-switcher.js reads", p["slug"], k)
			}
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Log("node not found; page-switcher.js model run skipped (payload contract asserted)")
		return
	}
	module, err := filepath.Abs(filepath.Join("..", "..", "public", "shell-assets", "page-switcher.js"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "-e", switcherScript, module)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node page-switcher: %v", err)
	}
	type item struct {
		Slug        string `json:"slug"`
		Href        string `json:"href"`
		OptionLabel string `json:"optionLabel"`
	}
	var m struct {
		Total    int   `json:"total"`
		Current  *item `json:"current"`
		Previous *item `json:"previous"`
		Next     *item `json:"next"`
		Nested   *item `json:"nested"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if m.Total < 39 || m.Current == nil || m.Previous == nil || m.Next == nil || m.Nested == nil {
		t.Fatalf("switcher model: %s", out)
	}
	if m.Current.Slug != "switch-15" || m.Previous.Slug != "switch-14" || m.Next.Slug != "switch-16" {
		t.Fatalf("active page and deterministic neighbours: %s", out)
	}
	if m.Next.Href != "/admin/switch-16" {
		t.Fatalf("selection targets the existing admin route: %s", m.Next.Href)
	}
	if !regexp.MustCompile(`Client 15 — /switch-15 · draft$`).MatchString(m.Current.OptionLabel) {
		t.Fatalf("title, slug, and status are selection context: %q", m.Current.OptionLabel)
	}
	if m.Nested.Href != "/admin/adm/nest" {
		t.Fatalf("nested selection preserves route segments: %q", m.Nested.Href)
	}
}
