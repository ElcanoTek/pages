// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"strings"

	"golang.org/x/net/html"
)

// Block is one located <script> element: byte offsets into the HTML of its
// content and of the whole element (so a block can be deleted rather than
// emptied). Type is the element's type attribute ("" when absent).
type Block struct {
	Type         string
	ContentStart int
	ContentEnd   int
	ElementStart int
	ElementEnd   int
}

// The JS server found blocks with parse5's tree and source locations: only
// <script> elements in the HTML namespace, never one inside a <template>
// (parse5 keeps template content off childNodes), and never text that merely
// looks like a tag inside a raw-text element, a comment or an attribute. This
// walks golang.org/x/net/html's tokenizer — whose raw token bytes partition the
// input, so offsets are exact — with the slice of tree construction that
// decides those questions: foreign (SVG/MathML) content with its integration
// points and breakout tags, and template depth. Raw-text handling (script data
// escapes, <noscript> with scripting enabled, <textarea>, <plaintext>) is the
// tokenizer's, which implements the same specification states as parse5.

type foreignFrame struct {
	name        string
	ns          string // "svg" or "math"
	integration bool   // HTML or MathML text integration point
}

var breakoutTags = toSet("b big blockquote body br center code dd div dl dt em embed h1 h2 h3 h4 h5 h6 head hr i img li listing menu meta nobr ol p pre ruby s small span strong strike sub sup table tt u ul var")

func inForeign(stack []foreignFrame) bool {
	return len(stack) > 0 && !stack[len(stack)-1].integration
}

func isIntegration(ns, name string, attrs map[string]string) bool {
	switch ns {
	case "svg":
		return name == "foreignobject" || name == "desc" || name == "title"
	case "math":
		switch name {
		case "mi", "mo", "mn", "ms", "mtext":
			return true
		case "annotation-xml":
			enc := strings.ToLower(attrs["encoding"])
			return enc == "text/html" || enc == "application/xhtml+xml"
		}
	}
	return false
}

// walkHTML records, in document order, every HTML-namespace <script> whose id
// is a key of matches.
func walkHTML(src string, matches map[string][]*Block) error {
	z := html.NewTokenizer(strings.NewReader(src))
	offset := 0
	var stack []foreignFrame
	templateDepth := 0
	var pending *Block
	var pendingID string
	for {
		z.AllowCDATA(inForeign(stack))
		tt := z.Next()
		start := offset
		offset += len(z.Raw())
		if tt == html.ErrorToken {
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			nameB, hasAttr := z.TagName()
			name := string(nameB)
			attrs := map[string]string{}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if _, dup := attrs[string(k)]; !dup {
					attrs[string(k)] = string(v)
				}
			}
			if inForeign(stack) {
				_, hasColor := attrs["color"]
				_, hasFace := attrs["face"]
				_, hasSize := attrs["size"]
				if breakoutTags[name] || (name == "font" && (hasColor || hasFace || hasSize)) {
					for inForeign(stack) {
						stack = stack[:len(stack)-1]
					}
				}
			}
			if inForeign(stack) {
				if tt == html.StartTagToken {
					ns := stack[len(stack)-1].ns
					stack = append(stack, foreignFrame{name: name, ns: ns, integration: isIntegration(ns, name, attrs)})
				}
				z.NextIsNotRawText()
				continue
			}
			switch name {
			case "svg", "math":
				if tt == html.StartTagToken {
					stack = append(stack, foreignFrame{name: name, ns: name})
				}
			case "template":
				templateDepth++
			case "script":
				id, hasID := attrs["id"]
				if _, wanted := matches[id]; hasID && wanted && templateDepth == 0 {
					b := &Block{Type: attrs["type"], ElementStart: start, ContentStart: offset, ContentEnd: -1}
					matches[id] = append(matches[id], b)
					pending, pendingID = b, id
				}
			}
		case html.EndTagToken:
			nameB, _ := z.TagName()
			name := string(nameB)
			if pending != nil && name == "script" {
				pending.ContentEnd = start
				pending.ElementEnd = offset
				pending = nil
				continue
			}
			found := -1
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i].name == name {
					found = i
					break
				}
			}
			switch {
			case found >= 0:
				stack = stack[:found]
			case inForeign(stack):
				// An end tag for an HTML element closes the foreign subtree
				// inside it.
				stack = nil
			case name == "template" && templateDepth > 0:
				templateDepth--
			}
		}
	}
	if pending != nil {
		return contractInvalid("the "+pendingID+" block must have an explicit closing </script> tag", nil)
	}
	return nil
}
