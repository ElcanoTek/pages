// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package audit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The documented list had drifted by six actions (every template and config
// action) before the Node release grew a test for it, which is a security doc
// going quietly stale: the audit vocabulary is how anyone reading the log
// knows what they are looking at. This pins docs/SECURITY.md's "Audit
// vocabulary" bullets against the actions the server writes, both ways.

var actionName = regexp.MustCompile(`^[a-z][a-z_]*$`)

// writtenActions collects, from every non-test Go file under internal/, the
// string literals that become an audit action:
//
//   - the value of an `Action:` field in a composite literal (audit.Entry,
//     versions.DataMetadata);
//   - a literal assigned to a variable named `action`
//     (`action := "deploy"`, `action = "disable"`);
//   - a literal passed in the position of a parameter named `action` of any
//     function or method declared in the same package (entry, auditEntry,
//     movePointer), which is how the pointer move receives publish,
//     rollback and approve.
func writtenActions(t *testing.T, root string) map[string][]string {
	t.Helper()
	written := map[string][]string{}
	add := func(fset *token.FileSet, lit ast.Expr) {
		bl, ok := lit.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return
		}
		v, err := strconv.Unquote(bl.Value)
		if err != nil || !actionName.MatchString(v) {
			return
		}
		pos := fset.Position(bl.Pos())
		rel, _ := filepath.Rel(root, pos.Filename)
		written[v] = append(written[v], rel+":"+strconv.Itoa(pos.Line))
	}
	pkgs := map[string][]*ast.File{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkgs[filepath.Dir(path)] = append(pkgs[filepath.Dir(path)], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, files := range pkgs {
		// Functions (and methods) of this package with a parameter named
		// action, and that parameter's index.
		actionParam := map[string]int{}
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				i := 0
				for _, field := range fn.Type.Params.List {
					names := field.Names
					if len(names) == 0 {
						i++
						continue
					}
					for _, n := range names {
						if n.Name == "action" {
							actionParam[fn.Name.Name] = i
						}
						i++
					}
				}
			}
		}
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.KeyValueExpr:
					if k, ok := n.Key.(*ast.Ident); ok && k.Name == "Action" {
						add(fset, n.Value)
					}
				case *ast.AssignStmt:
					for i, lhs := range n.Lhs {
						if id, ok := lhs.(*ast.Ident); ok && id.Name == "action" && i < len(n.Rhs) {
							add(fset, n.Rhs[i])
						}
					}
				case *ast.CallExpr:
					name := ""
					switch fun := n.Fun.(type) {
					case *ast.Ident:
						name = fun.Name
					case *ast.SelectorExpr:
						name = fun.Sel.Name
					}
					if i, ok := actionParam[name]; ok && i < len(n.Args) {
						add(fset, n.Args[i])
					}
				}
				return true
			})
		}
	}
	return written
}

// documentedActions reads the backticked names in the bulleted list under
// "## Audit vocabulary" — the `- **Surface** — …` bullets and their indented
// continuations. Prose above and below may name retired actions without
// being read as vocabulary; the doc keeps prose out of the bullets for that.
func documentedActions(t *testing.T, doc string) map[string]bool {
	t.Helper()
	const heading = "## Audit vocabulary"
	at := strings.Index(doc, heading)
	if at < 0 {
		t.Fatal("docs/SECURITY.md must keep an Audit vocabulary section")
	}
	lines := strings.Split(doc[at:], "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "- **") })
	if start < 0 {
		t.Fatal("the vocabulary must stay a bulleted list, one bullet per surface")
	}
	end := start
	for end < len(lines) && (strings.HasPrefix(lines[end], "- ") || strings.HasPrefix(lines[end], "  ")) {
		end++
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("`([a-z][a-z_]*)`").FindAllStringSubmatch(strings.Join(lines[start:end], "\n"), -1) {
		documented[m[1]] = true
	}
	return documented
}

func TestAuditVocabularyIsDocumented(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	written := writtenActions(t, root)
	// The real vocabulary is about forty actions; far fewer means the scan
	// stopped seeing the call sites, not that the server stopped auditing.
	if len(written) < 30 {
		t.Fatalf("expected the real vocabulary, found %d: %v", len(written), written)
	}
	for _, want := range []string{"publish", "rollback", "approve", "deploy", "data_update", "template_build", "delete_portal"} {
		if _, ok := written[want]; !ok {
			t.Errorf("the scan no longer sees %q being written", want)
		}
	}
	doc, err := os.ReadFile(filepath.Join(root, "docs", "SECURITY.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := documentedActions(t, string(doc))

	var undocumented, stale []string
	for a, where := range written {
		if !documented[a] {
			undocumented = append(undocumented, a+" ("+strings.Join(where, ", ")+")")
		}
	}
	for a := range documented {
		if _, ok := written[a]; !ok {
			stale = append(stale, a)
		}
	}
	slices.Sort(undocumented)
	slices.Sort(stale)
	if len(undocumented) > 0 {
		t.Errorf("a new audit action must be documented in docs/SECURITY.md in the same change: %v", undocumented)
	}
	// The reverse direction: a documented action nothing writes any more is
	// equally misleading to whoever is reading a log.
	if len(stale) > 0 {
		t.Errorf("docs/SECURITY.md names an action nothing writes: %v", stale)
	}
}
