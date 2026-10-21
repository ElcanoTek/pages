// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package preflight

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tdewolff/parse/v2"
	"github.com/tdewolff/parse/v2/js"
)

// The JS checked classic scripts with V8 (vm.Script, compile only) and modules
// with Acorn. The Go port parses both with github.com/tdewolff/parse/v2/js:
//
//   - It is pure Go, parse-only (no evaluation path exists to misuse), and
//     covers the grammar dashboards are written in: ES2022+ classes with
//     private and static members and static blocks, optional chaining, ??=,
//     BigInt, numeric separators, regexp v flag, async iteration, dynamic
//     import, import.meta, and module top-level await.
//   - It bounds its own recursion (1000 nested expressions / statements) and
//     reports that as an error rather than overflowing. That is decisive here:
//     preflight runs inside the server on deploy, a Go stack overflow is a fatal
//     process crash rather than a recoverable error, and the obvious
//     alternative (esbuild's parser) recursed a 1 MiB `((((…` straight into the
//     1 GB stack limit and took the process down.
//   - goja's parser has no module grammar at all.
//
// The parser has one grammar for both goals, so the script/module split V8 and
// Acorn drew is drawn here from the AST: a classic script that contains an
// import/export declaration, import.meta or a top-level await is reported with
// the message V8 gives. Known differences from V8, all on legacy sloppy-mode
// syntax no modern dashboard uses: legacy octal literals (010), `await` as an
// identifier in a classic script, and Annex B function declarations as an if
// body are reported as syntax errors; strict-mode-only early errors in modules
// (e.g. `with`) are not. Messages are the parser's own, with Acorn's
// "(line:column)" suffix, not V8's wording.

// maxNesting bounds bracket depth before the parser runs: the parser's own
// limits cover expressions and statements but not destructuring patterns, which
// it parses recursively without a bound.
const maxNesting = 2000

type syntaxVerdict struct {
	err   string // a syntax error message, or ""
	limit bool   // the analysis limit was reached
}

func checkScriptSyntax(src string, module bool) (v syntaxVerdict) {
	defer func() {
		if r := recover(); r != nil {
			v = syntaxVerdict{limit: true}
		}
	}()
	if bracketDepth(src) > maxNesting {
		return syntaxVerdict{limit: true}
	}
	ast, err := js.Parse(parse.NewInputString(src), js.Options{})
	if err != nil {
		var pe *parse.Error
		if errors.As(err, &pe) {
			if strings.HasPrefix(pe.Message, "too many nested") {
				return syntaxVerdict{limit: true}
			}
			return syntaxVerdict{err: fmt.Sprintf("%s (%d:%d)", upperFirst(pe.Message), pe.Line, pe.Column)}
		}
		return syntaxVerdict{err: err.Error()}
	}
	if !module {
		if msg := moduleOnlySyntax(ast); msg != "" {
			return syntaxVerdict{err: msg}
		}
	}
	return syntaxVerdict{}
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// moduleOnlySyntax returns V8's message for the first construct a classic
// script may not contain, or "".
func moduleOnlySyntax(ast *js.AST) string {
	for _, stmt := range ast.List {
		switch stmt.(type) {
		case *js.ImportStmt:
			return "Cannot use import statement outside a module"
		case *js.ExportStmt:
			return "Unexpected token 'export'"
		}
	}
	v := &moduleVisitor{}
	js.Walk(v, ast)
	return v.msg
}

type moduleVisitor struct {
	funcDepth int
	msg       string
}

func isFunction(n js.INode) bool {
	switch n.(type) {
	case *js.FuncDecl, *js.MethodDecl, *js.ArrowFunc:
		return true
	}
	return false
}

func (v *moduleVisitor) Enter(n js.INode) js.IVisitor {
	if v.msg != "" {
		return nil
	}
	switch n := n.(type) {
	case *js.ImportMetaExpr:
		v.msg = "Cannot use 'import.meta' outside a module"
		return nil
	case *js.UnaryExpr:
		if n.Op == js.AwaitToken && v.funcDepth == 0 {
			v.msg = "await is only valid in async functions and the top level bodies of modules"
			return nil
		}
	case *js.ForOfStmt:
		if n.Await && v.funcDepth == 0 {
			v.msg = "Unexpected reserved word"
			return nil
		}
	}
	if isFunction(n) {
		v.funcDepth++
	}
	return v
}

func (v *moduleVisitor) Exit(n js.INode) {
	if isFunction(n) {
		v.funcDepth--
	}
}

// bracketDepth is the deepest nesting of (), [], {} and template
// substitutions, from the lexer (strings, comments and templates are tokens,
// so brackets inside them do not count). A `/` is read as a regular expression
// wherever the previous token cannot end an expression — the usual lexer
// heuristic; misreading one only skews the count, never the verdict.
func bracketDepth(src string) int {
	l := js.NewLexer(parse.NewInputString(src))
	depth, deepest := 0, 0
	prev := js.ErrorToken
	for {
		tt, _ := l.Next()
		switch tt {
		case js.ErrorToken:
			return deepest
		case js.WhitespaceToken, js.LineTerminatorToken, js.CommentToken, js.CommentLineTerminatorToken:
			continue
		case js.DivToken, js.DivEqToken:
			if regexpAllowed(prev) {
				tt, _ = l.RegExp()
				if tt == js.ErrorToken {
					return deepest
				}
			}
		case js.OpenParenToken, js.OpenBracketToken, js.OpenBraceToken, js.TemplateStartToken:
			depth++
			deepest = max(deepest, depth)
		case js.CloseParenToken, js.CloseBracketToken, js.CloseBraceToken, js.TemplateEndToken:
			depth = max(0, depth-1)
		}
		prev = tt
	}
}

func regexpAllowed(prev js.TokenType) bool {
	switch prev {
	case js.IdentifierToken, js.StringToken, js.TemplateToken, js.TemplateEndToken, js.RegExpToken,
		js.CloseParenToken, js.CloseBracketToken, js.CloseBraceToken,
		js.ThisToken, js.SuperToken, js.TrueToken, js.FalseToken, js.NullToken,
		js.IncrToken, js.DecrToken, js.PrivateIdentifierToken:
		return false
	}
	return !js.IsNumeric(prev)
}
