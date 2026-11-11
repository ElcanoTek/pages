// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package i18n

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRequestPreference(t *testing.T) {
	for _, tc := range []struct {
		name, path, cookie, accept, want string
		explicit                         bool
	}{
		{"query wins", "/?lang=es-MX", "elcano_language=pt", "en", "es", true},
		{"cookie wins", "/?lang=de", "elcano_language=pt-PT", "es", "pt", false},
		{"weighted browser", "/", "", "fr,pt;q=0.6,es;q=0.9", "es", false},
		{"unsupported browser", "/", "", "de,fr;q=0.9", "en", false},
		{"zero and invalid quality", "/", "", "es;q=0,pt;q=NaN,en;q=0.5", "en", false},
		{"malformed cookie", "/", "elcano_language=unsupported", "pt-BR", "pt", false},
		{"explicit English", "/?lang=en", "elcano_language=es", "pt", "en", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", tc.path, nil)
			r.Header.Set("Cookie", tc.cookie)
			r.Header.Set("Accept-Language", tc.accept)
			got := FromRequest(r)
			if got.Language != tc.want || got.Explicit != tc.explicit {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestTranslationDataAndNavigation(t *testing.T) {
	locale := Locale{Language: "es", Explicit: true}
	if got := locale.T("Loading {0}", "{0}</script>"); got != "Cargando {0}</script>" {
		t.Fatal(got)
	}
	if got := locale.T("Customer authored {0}", "text"); got != "Customer authored text" {
		t.Fatal(got)
	}
	if got := locale.URL("/admin?workspace=2#review"); got != "/admin?lang=es&workspace=2#review" {
		t.Fatal(got)
	}
	for _, href := range []string{"https://customer.example/a?x=1", "//customer.example/a"} {
		if locale.URL(href) != href {
			t.Fatal("changed foreign URL")
		}
	}
	if got := (Locale{Language: "pt"}).URL("/admin?x=1"); got != "/admin?x=1" {
		t.Fatal(got)
	}
	var boot struct {
		Language string
		Messages map[string]string
	}
	if err := json.Unmarshal([]byte(locale.Bootstrap()), &boot); err != nil {
		t.Fatal(err)
	}
	if boot.Language != "es" || boot.Messages["Client pages"] != "Páginas de clientes" { //nolint:misspell // This assertion checks Spanish interface copy.
		t.Fatal("wrong bootstrap language")
	}
	if strings.Contains(locale.Bootstrap(), "</script>") {
		t.Fatal("unsafe JSON island")
	}
}

func TestStaticServerMessagesHaveTranslations(t *testing.T) {
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "T" && selector.Sel.Name != "Source") {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			source, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			for _, language := range []string{"es", "pt"} {
				if _, exists := catalogs[language][source]; !exists {
					t.Errorf("%s: %s missing %q", path, language, source)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
