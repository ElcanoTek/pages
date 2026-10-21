// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/templates"
	"github.com/ElcanoTek/pages/internal/testdb"
)

// Ported from test/template-cli.integration.js: the file-backed template path,
// the one registration route that involves no agent and no model output.
// `pages template sync` is what an update runs, so its guarantees are
// load-bearing — unchanged bytes are a no-op, an edited design becomes
// revision N+1, a broken template is reported and skipped, and an explicit
// path + name works.

func cliTemplateHTML(marker string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><title>CLI</title></head><body>` +
		`<h1 id="t"></h1><div id="rev">` + marker + `</div>` +
		`<script type="application/schema+json" id="pages-config-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["campaign"],"properties":{"campaign":{"type":"string","minLength":1}}}</script>` +
		`<script type="application/json" id="pages-config">{"campaign":"Reference"}</script>` +
		`<script type="application/schema+json" id="pages-data-schema">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","additionalProperties":false,"required":["rows"],"properties":{"rows":{"type":"array","items":{"type":"object","additionalProperties":false}}}}</script>` +
		`<script type="application/json" id="pages-data">{"contract_version":1,"refreshed_at":"2026-08-01T00:00:00.000Z","source_as_of":"2026-08-01T00:00:00.000Z","data":{"rows":[]}}</script>` +
		"<script>\n" +
		"const CONFIG = JSON.parse(document.getElementById('pages-config').textContent);\n" +
		"const DATA = JSON.parse(document.getElementById('pages-data').textContent).data;\n" +
		"document.getElementById('t').textContent = CONFIG.campaign;\n" +
		"</script></body></html>"
}

type cliRun struct {
	code        int
	stdout, out string
}

func runTemplateCLI(t *testing.T, args ...string) cliRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runTemplate(context.Background(), args, &stdout, &stderr, db.OpenFromEnv)
	return cliRun{code: code, stdout: stdout.String(), out: stdout.String() + stderr.String()}
}

func TestTemplateFlags(t *testing.T) {
	flags, positional, err := parseTemplateFlags([]string{"file.html", "--name", "x", "--title", "T"})
	if err != nil || flags["name"] != "x" || flags["title"] != "T" || len(positional) != 1 || positional[0] != "file.html" {
		t.Fatal(flags, positional, err)
	}
	for _, args := range [][]string{{"--name", "--title", "T"}, {"--name"}} {
		if _, _, err := parseTemplateFlags(args); err == nil || err.Error() != "--name needs a value" {
			t.Error(args, err)
		}
	}
	if r := runTemplateCLI(t, "bogus"); r.code != 2 || !strings.Contains(r.out, "usage: pages template") {
		t.Error(r)
	}
}

func TestTemplateVerb(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "cli-smoke")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	htmlPath := filepath.Join(dir, "template.html")
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(htmlPath, cliTemplateHTML("DESIGN_V1"))
	write(filepath.Join(dir, "template.json"), `{"title":"CLI Smoke","description":"Fixture."}`)

	if r := runTemplateCLI(t, "list"); r.code != 0 || r.stdout != "no templates registered\n" {
		t.Fatal(r)
	}
	created := runTemplateCLI(t, "sync", root)
	if created.code != 0 || !regexp.MustCompile(`cli-smoke: created \(revision 1, [0-9a-f]{12}\)`).MatchString(created.out) {
		t.Fatal(created)
	}
	// An update re-runs sync every time; unchanged bytes must not invent revisions.
	if again := runTemplateCLI(t, "sync", root); again.code != 0 || !strings.Contains(again.out, "cli-smoke: unchanged (revision 1") {
		t.Fatal(again)
	}
	write(htmlPath, cliTemplateHTML("DESIGN_V2"))
	if bumped := runTemplateCLI(t, "sync", root); bumped.code != 0 || !strings.Contains(bumped.out, "cli-smoke: new revision (revision 2") {
		t.Fatal(bumped)
	}
	if listed := runTemplateCLI(t, "list"); listed.code != 0 || !regexp.MustCompile(`cli-smoke\s+revision 2\s+0 page\(s\)\s+— CLI Smoke`).MatchString(listed.out) {
		t.Fatal(listed)
	}
	shown := runTemplateCLI(t, "show", "cli-smoke")
	var parsed map[string]any
	if err := json.Unmarshal([]byte(shown.stdout), &parsed); err != nil || shown.code != 0 {
		t.Fatal(shown, err)
	}
	if _, ok := parsed["html"]; ok || parsed["template"].(map[string]any)["current_revision"] != 2.0 {
		t.Fatal("show must not dump the design", shown.stdout)
	}
	if rc, _ := json.Marshal(parsed["reference_config"]); string(rc) != `{"campaign":"Reference"}` {
		t.Fatal(string(rc))
	}
	if !strings.HasPrefix(shown.stdout, "{\n  \"template\": {\n    \"id\": \"") {
		t.Fatal("two-space JSON, as JSON.stringify(result, null, 2)", shown.stdout[:40])
	}
	if pinned := runTemplateCLI(t, "show", "cli-smoke", "--revision", "1"); pinned.code != 0 || !strings.Contains(pinned.stdout, `"revision": 1,`) {
		t.Fatal(pinned)
	}

	// A broken template must not take the good ones down with it.
	broken := filepath.Join(root, "broken")
	if err := os.Mkdir(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(broken, "template.html"), "<!doctype html><html><body>no blocks</body></html>")
	mixed := runTemplateCLI(t, "sync", root)
	if mixed.code != 1 || !strings.Contains(mixed.out, "broken: FAILED") || !strings.Contains(mixed.out, "cli-smoke: unchanged (revision 2") ||
		!strings.Contains(mixed.out, "synced 1/2") {
		t.Fatal(mixed)
	}

	// An explicit path + name: how a template from a client bundle is registered.
	byPath := runTemplateCLI(t, "register", htmlPath, "--name", "from-a-bundle", "--title", "Bundle Supplied")
	if byPath.code != 0 || !strings.Contains(byPath.out, "from-a-bundle: created (revision 1") {
		t.Fatal(byPath)
	}
	if r := runTemplateCLI(t, "register", filepath.Join(root, "nope.html")); r.code != 1 || !strings.Contains(r.out, "no such file: ") {
		t.Fatal(r)
	}

	// A CLI registration attributes itself as source "cli".
	got, err := templates.Get(ctx, d, "cli-smoke", nil, false)
	if err != nil || got.Revision.Source != "cli" || !strings.HasPrefix(got.Revision.Author, "pages-cli:") {
		t.Fatal(got, err)
	}

	// No dir: the templates built into the binary.
	shipped := runTemplateCLI(t, "sync")
	if shipped.code != 0 || !strings.Contains(shipped.out, "nwm-campaign-dashboard: created (revision 1") ||
		!strings.Contains(shipped.out, "synced 1/1 template(s) from the templates built into this release") {
		t.Fatal(shipped)
	}
	if again := runTemplateCLI(t, "sync"); again.code != 0 || !strings.Contains(again.out, "nwm-campaign-dashboard: unchanged (revision 1") {
		t.Fatal(again)
	}
	got, err = templates.Get(ctx, d, "nwm-campaign-dashboard", nil, false)
	if err != nil || got.Template.Title == "" {
		t.Fatal("template.json metadata is applied", got, err)
	}
}
