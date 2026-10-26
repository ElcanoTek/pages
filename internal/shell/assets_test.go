// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package shell

import (
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	pages "github.com/ElcanoTek/pages"
	"github.com/ElcanoTek/pages/internal/assetver"
)

// These guard the browser assets the shells boot (public/shell-assets), ported
// from test/theme.test.js and the shell.css block of test/unit.test.js.

func readPublic(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(pages.Public(), name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ── theme ────────────────────────────────────────────────────────────────────
// The admin shell is meant to be entirely token-driven: every colour, space,
// radius, shadow and motion value resolves to a vendored Flag design token, or
// to one of a small set of deliberately declared Pages-local custom
// properties. That is what makes the light and dark themes work without a
// second stylesheet. The two ways this decays are both silent: a literal
// colour looks right in whichever theme its author had open, and a mistyped
// token name resolves to nothing at all.

var (
	colourLiteral = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\brgba?\(|\bhsla?\(`)
	blockComment  = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment   = regexp.MustCompile("(^|[^:\"'`\\\\])//.*$")
	declaredProp  = regexp.MustCompile(`(--[a-z0-9-]+)\s*:`)
	referenceProp = regexp.MustCompile(`var\(\s*(--[a-z0-9-]+)`)
)

var browserModules = []string{"admin.js", "welcome.js", "portals.js", "templates.js", "primitives.js", "page-switcher.js", "refresh-health.js"}

// stripComments drops /* */ and // comments: a hex or rgb() inside a comment
// is prose (a PR reference like "#125"), not a hard-coded colour.
func stripComments(source string) string {
	lines := strings.Split(blockComment.ReplaceAllString(source, ""), "\n")
	for i, line := range lines {
		lines[i] = lineComment.ReplaceAllString(line, "$1")
	}
	return strings.Join(lines, "\n")
}

func findLiterals(source string) []string {
	var found []string
	for i, line := range strings.Split(source, "\n") {
		if m := colourLiteral.FindAllString(line, -1); m != nil {
			found = append(found, fmt.Sprintf("line %d: %s %v", i+1, strings.TrimSpace(line), m))
		}
	}
	return found
}

func propSet(re *regexp.Regexp, source string) map[string]bool {
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(source, -1) {
		out[m[1]] = true
	}
	return out
}

func TestThemeStylesheetHardCodesNoColour(t *testing.T) {
	if found := findLiterals(stripComments(readPublic(t, "shell-assets/shell.css"))); len(found) > 0 {
		t.Errorf("shell.css must express colour through Flag tokens, not literals:\n%s", strings.Join(found, "\n"))
	}
}

func TestThemeEveryReferencedPropertyIsDefined(t *testing.T) {
	shellCSS := readPublic(t, "shell-assets/shell.css")
	tokens := propSet(declaredProp, readPublic(t, "assets/flag/tokens/design-tokens.css"))
	locals := propSet(declaredProp, shellCSS)
	var unresolved []string
	for name := range propSet(referenceProp, shellCSS) {
		if !tokens[name] && !locals[name] {
			unresolved = append(unresolved, name)
		}
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		t.Errorf("these var() references resolve to nothing — a mistyped token renders unstyled rather than failing:\n%s", strings.Join(unresolved, "\n"))
	}
}

func TestThemeLocalPropertiesStayFewAndNamespaced(t *testing.T) {
	var locals, misnamed []string
	for name := range propSet(declaredProp, readPublic(t, "shell-assets/shell.css")) {
		locals = append(locals, name)
		if !strings.HasPrefix(name, "--pages-") {
			misnamed = append(misnamed, name)
		}
	}
	sort.Strings(locals)
	sort.Strings(misnamed)
	if len(misnamed) > 0 {
		t.Errorf("a value Pages defines for itself must say so in its name:\n%s", strings.Join(misnamed, "\n"))
	}
	// Not a style rule — a pressure valve. Each of these is a value Flag does
	// not provide; if the list grows, the fix belongs upstream in the tokens.
	if len(locals) > 8 {
		t.Errorf("%d Pages-local properties (%s). Past a handful, these belong in the Flag tokens.", len(locals), strings.Join(locals, ", "))
	}
}

func TestThemeBrowserModulesSetNoColour(t *testing.T) {
	for _, name := range browserModules {
		if found := findLiterals(stripComments(readPublic(t, "shell-assets/"+name))); len(found) > 0 {
			t.Errorf("%s: colour belongs in shell.css against a token, never inline in a module:\n%s", name, strings.Join(found, "\n"))
		}
	}
}

func TestThemeBothThemesAreReachable(t *testing.T) {
	tokens := readPublic(t, "assets/flag/tokens/design-tokens.css")
	if !regexp.MustCompile(`\[data-theme="light"\]|\.theme-light`).MatchString(tokens) {
		t.Error("the Flag tokens no longer define a light theme")
	}
	if !strings.Contains(tokens, ":root") {
		t.Error("the Flag tokens no longer define a root palette")
	}
}

// ── the admin stylesheet ─────────────────────────────────────────────────────
// Two defects have shipped in shell.css that no API test could see, both of
// the same shape: a rule that does not exist, so the element silently falls
// back to user-agent styling. First a batch of invented class names; then a
// comment whose body contained a selector ending in a star-slash, which closed
// the comment early and made the parser swallow the rule that followed it.

func cssSelectors(css string) []string {
	var selectors []string
	// Good enough for a flat stylesheet with one level of @media nesting: take
	// the text before each "{" and drop at-rule preludes.
	for _, m := range regexp.MustCompile(`([^{}]+)\{`).FindAllStringSubmatch(blockComment.ReplaceAllString(css, ""), -1) {
		prelude := strings.TrimSpace(m[1])
		if prelude == "" || strings.HasPrefix(prelude, "@") {
			continue
		}
		selectors = append(selectors, prelude)
	}
	return selectors
}

func definedClasses(css string) map[string]bool {
	defined := map[string]bool{}
	re := regexp.MustCompile(`\.([A-Za-z0-9_-]+)`)
	for _, sel := range cssSelectors(css) {
		for _, m := range re.FindAllStringSubmatch(sel, -1) {
			defined[m[1]] = true
		}
	}
	return defined
}

func TestShellCSSNoCommentLeaksSelectorText(t *testing.T) {
	css := readPublic(t, "shell-assets/shell.css")
	// A selector can never contain a slash. One appearing here means a comment
	// terminated early and the parser is now reading prose as CSS.
	var leaked []string
	for _, sel := range cssSelectors(css) {
		if strings.Contains(sel, "/") {
			leaked = append(leaked, sel)
		}
	}
	if len(leaked) > 0 {
		t.Errorf(`selector text containing "/" means a comment ended early: %s`, strings.Join(leaked, " | "))
	}
	if strings.Contains(blockComment.ReplaceAllString(css, ""), "*/") {
		t.Error("an unbalanced comment terminator remains after stripping")
	}
}

func assertClassesDefined(t *testing.T, screen string, used []string) {
	t.Helper()
	defined := definedClasses(readPublic(t, "shell-assets/shell.css"))
	var missing []string
	for _, name := range used {
		if !defined[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%s renders classes shell.css never defines: %s", screen, strings.Join(missing, ", "))
	}
}

func TestShellCSSTemplateLibraryClassesDefined(t *testing.T) {
	assertClassesDefined(t, "templates.js", []string{
		"page-heading", "page-heading__row", "page-heading__copy", "page-heading__intro", "page-heading__actions",
		"stats", "stat", "panel", "section-block", "section-heading", "section-heading--row",
		"operation-table", "operation-table-wrap", "page-cell", "table-meta", "row-actions",
		"template-table__template", "template-table__num", "template-table__hash",
		"template-table__when", "template-table__actions",
		"row--selected", "template-detail", "template-kv", "code-block", "template-contract",
		"segmented", "segmented__button", "badge", "badge--live", "badge--pending",
		"btn", "btn-sm", "btn-primary", "form-stack", "field", "field-label", "field-help",
		"note", "note--warning",
		"cluster", "muted", "sr-only", "state-panel",
	})
}

func TestShellCSSPortalsClassesDefined(t *testing.T) {
	assertClassesDefined(t, "portals.js", []string{
		"code-block", "row--selected", "note", "note--warning",
		"field", "field-label", "field-help", "form-stack", "cluster",
		"operation-table", "operation-table-wrap", "table-meta", "row-actions",
		"section-block", "section-heading", "section-heading--row", "panel",
		"badge", "badge--live", "btn", "btn-sm", "btn-primary", "btn-danger",
		"state-panel",
		// The move-up/move-down controls borrow the dialog close button's square
		// icon control and the shared icon sizing; the two new ones keep their x
		// from changing between rows.
		"icon-action", "icon-inline",
		"row-actions--split", "row-actions__group", "operation-table__member-actions",
		"operation-table__page",
	})
}

func TestShellAssetsOnlyPrimitivesFetchEncodeOrFormat(t *testing.T) {
	// Once a screen can reach for fetch() or toLocaleString() itself, the fifth
	// variant of the behaviour is one session away.
	banned := []struct {
		re  *regexp.Regexp
		why string
	}{
		{regexp.MustCompile(`\bfetch\(`), "fetch( — use UI.request or UI.requestScope"},
		{regexp.MustCompile(`toLocale`), "toLocale… — use UI.formatWhen or UI.formatCount"},
		{regexp.MustCompile(`encodeURIComponent`), "encodeURIComponent — use UI.slugPath or UI.pathSegment"},
	}
	destructureRe := regexp.MustCompile(`const \{[^}]*\} = UI;`)
	bareConfirm := regexp.MustCompile(`\bconfirm\b`)
	for _, name := range []string{"welcome.js", "admin.js", "templates.js", "portals.js"} {
		source := readPublic(t, "shell-assets/"+name)
		for _, b := range banned {
			if b.re.MatchString(source) {
				t.Errorf("%s must not contain %s", name, b.why)
			}
		}
		// A bare `confirm` in the destructure shadows window.confirm, so a later
		// `if (confirm("…"))` would test a Promise and always pass. (Go regexp
		// has no lookahead: a match followed by "Dialog" is confirmDialog.)
		for _, line := range destructureRe.FindAllString(source, -1) {
			for _, loc := range bareConfirm.FindAllStringIndex(line, -1) {
				if !strings.HasPrefix(line[loc[1]:], "Dialog") {
					t.Errorf("%s destructures a bare confirm: %s", name, line)
				}
			}
		}
	}
}

func TestShellCSSCollapsedTableScopedToIndex(t *testing.T) {
	// The named grid areas exist only for the index's own six data-labels.
	css := blockComment.ReplaceAllString(readPublic(t, "shell-assets/shell.css"), "")
	for _, m := range regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`).FindAllStringSubmatch(css, -1) {
		prelude, body := m[1], m[2]
		if !strings.Contains(body, "grid-template-areas") || !strings.Contains(prelude, "operation-table") {
			continue
		}
		if !strings.Contains(prelude, "operation-table--index") {
			t.Errorf(`grid-template-areas on %q must be scoped to .operation-table--index`, strings.TrimSpace(prelude))
		}
	}
}

// The shells must reference only assets that ship in the binary.
func TestShellsReferenceEmbeddedAssets(t *testing.T) {
	re := regexp.MustCompile(`(?:src|href)="/shell-assets/([^"#]+)`)
	pub := pages.Public()
	var seen []string
	for _, html := range []string{
		Admin("northwind/q2", "t", "ops@example.com", ""), Templates("t", "ops@example.com", ""),
		Portals("t", "ops@example.com", ""), Welcome("ops@example.com", "", WelcomeOptions{}), NotFound(),
	} {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			if slices.Contains(seen, m[1]) {
				continue
			}
			seen = append(seen, m[1])
			// Every link carries the current asset version, so it is served
			// immutable; the file behind it is the unversioned path.
			rest, versioned, current := assetver.Strip("/" + m[1])
			if !versioned || !current {
				t.Errorf("shell references /shell-assets/%s, which is not under the current version %s", m[1], assetver.Shell())
			}
			rel := strings.TrimPrefix(rest, "/")
			// /shell-assets/flag is served from public/assets/flag.
			p := "shell-assets/" + rel
			if rest, ok := strings.CutPrefix(rel, "flag/"); ok {
				p = "assets/flag/" + rest
			}
			if _, err := fs.Stat(pub, p); err != nil {
				t.Errorf("shell references /shell-assets/%s, which is not embedded (%v)", m[1], err)
			}
		}
	}
}
