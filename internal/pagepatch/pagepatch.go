// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pagepatch is anchored edits and literal search over a stored
// version.
//
// WHY. Before this, the smallest possible change to a live dashboard cost a
// whole document. Fixing one CSS rule meant: read 65 KB back through
// get_version, hold it in context, re-emit all 65 KB, redeploy. We watched an
// agent spend 143k completion tokens in a single turn hand-reassembling a
// document it had just been handed, because a 65 KB tool argument had blown up
// on the way in.
//
// The bytes already live here. An edit is a few hundred tokens of anchors, and
// a search is a few hundred tokens of matches — neither needs the document to
// make a round trip through a model.
//
// Anchors are LITERAL strings, never regexes: an agent-supplied pattern is an
// unbounded-backtracking hazard on a 2 MiB document, and every real edit is a
// literal anyway (cf. the ReDoS screen in internal/pagedata). An anchor that
// does not match its expected occurrence count is an error, not a silent no-op
// — a patch that quietly did nothing is exactly how a "fixed" dashboard ships
// unfixed.
//
// Lengths, limits and reported offsets are in UTF-16 code units, as the
// JavaScript implementation (and therefore every client) counts them.
package pagepatch

import (
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// Limits.
const (
	MaxEdits         = 25
	MaxFindChars     = 8192
	MaxReplaceChars  = 128 * 1024
	MaxExpectedCount = 100
	// Search output is read by a model; keep it small and say what was left out.
	MaxMatches      = 20
	MaxContextChars = 240
)

// Edit is one literal replacement. Count is the number of occurrences the
// anchor must have; 0 means the default of 1.
type Edit struct {
	Find    string
	Replace string
	Count   int
}

// Applied reports one edit that landed.
type Applied struct {
	Index      int `json:"index"`
	Count      int `json:"count"`
	FirstLine  int `json:"first_line"`
	BytesDelta int `json:"bytes_delta"`
}

// Result is the edited document and what each edit did.
type Result struct {
	HTML    string    `json:"html"`
	Applied []Applied `json:"applied"`
}

func invalid(msg string) error { return apierror.BadRequest(msg, "patch_edits_invalid", nil) }

// ParseEdits validates a parsed JSON edits array (from jsonv.Parse) with the
// same messages the JS gave, and converts it.
func ParseEdits(v any) ([]Edit, error) {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil, invalid("edits must be a non-empty array")
	}
	if len(arr) > MaxEdits {
		return nil, invalid(fmt.Sprintf("at most %d edits per patch", MaxEdits))
	}
	out := make([]Edit, len(arr))
	for i, raw := range arr {
		obj, _ := raw.(*jsonv.Object)
		find, okF := obj.Value("find").(string)
		replace, okR := obj.Value("replace").(string)
		if obj == nil || !okF || !okR {
			return nil, invalid(fmt.Sprintf("edit %d: find and replace must both be strings", i))
		}
		if err := validateEdit(i, Edit{Find: find, Replace: replace, Count: 1}); err != nil {
			return nil, err
		}
		count := 1
		if c, present := obj.Get("count"); present {
			f, isNum := c.(float64)
			if !isNum || f != math.Trunc(f) || math.IsInf(f, 0) || f < 1 || f > MaxExpectedCount {
				return nil, invalid(fmt.Sprintf("edit %d: count must be an integer 1-%d", i, MaxExpectedCount))
			}
			count = int(f)
		}
		out[i] = Edit{Find: find, Replace: replace, Count: count}
	}
	return out, ValidateEdits(out)
}

func validateEdit(i int, e Edit) error {
	if n := jsstr.UTF16Len(e.Find); n == 0 || n > MaxFindChars {
		return invalid(fmt.Sprintf("edit %d: find must be 1-%d characters", i, MaxFindChars))
	}
	if jsstr.UTF16Len(e.Replace) > MaxReplaceChars {
		return invalid(fmt.Sprintf("edit %d: replace must be at most %d characters", i, MaxReplaceChars))
	}
	if e.Count < 0 || e.Count > MaxExpectedCount {
		return invalid(fmt.Sprintf("edit %d: count must be an integer 1-%d", i, MaxExpectedCount))
	}
	return nil
}

// ValidateEdits checks the edit list's bounds.
func ValidateEdits(edits []Edit) error {
	if len(edits) == 0 {
		return invalid("edits must be a non-empty array")
	}
	if len(edits) > MaxEdits {
		return invalid(fmt.Sprintf("at most %d edits per patch", MaxEdits))
	}
	for i, e := range edits {
		if err := validateEdit(i, e); err != nil {
			return err
		}
	}
	return nil
}

// CountOccurrences counts non-overlapping occurrences of needle.
func CountOccurrences(haystack, needle string) int {
	if needle == "" {
		return 0
	}
	return strings.Count(haystack, needle)
}

func lineOf(text string, byteOffset int) int {
	return 1 + strings.Count(text[:byteOffset], "\n")
}

// MismatchDetails is the detail object of a patch_anchor_mismatch conflict.
type MismatchDetails struct {
	EditIndex     int `json:"edit_index"`
	ExpectedCount int `json:"expected_count"`
	ActualCount   int `json:"actual_count"`
}

// ApplyEdits applies literal, occurrence-checked edits in order. Each edit must
// match exactly Count times (default 1) in the text as it stands when that edit
// runs — so edits that overlap earlier replacements fail loudly instead of
// landing somewhere unintended.
func ApplyEdits(source string, edits []Edit) (*Result, error) {
	if err := ValidateEdits(edits); err != nil {
		return nil, err
	}
	html := source
	applied := make([]Applied, 0, len(edits))
	for index, e := range edits {
		expected := e.Count
		if expected == 0 {
			expected = 1
		}
		found := CountOccurrences(html, e.Find)
		if found != expected {
			hint := fmt.Sprintf("Widen the anchor until it is unique, or set count:%d if every occurrence should change.", found)
			if found == 0 {
				hint = "The anchor is not present. Locate it with find_in_version and copy the exact bytes, including whitespace."
			}
			return nil, apierror.Conflict(
				fmt.Sprintf("edit %d: expected %d occurrence(s) of the anchor, found %d. %s", index, expected, found, hint),
				"patch_anchor_mismatch",
				MismatchDetails{EditIndex: index, ExpectedCount: expected, ActualCount: found},
			)
		}
		before := len(html)
		first := strings.Index(html, e.Find)
		line := lineOf(html, first)
		html = strings.ReplaceAll(html, e.Find, e.Replace)
		applied = append(applied, Applied{Index: index, Count: found, FirstLine: line, BytesDelta: len(html) - before})
	}
	return &Result{HTML: html, Applied: applied}, nil
}

// Match is one located occurrence: its UTF-16 offset, 1-based line, and a
// short single-line excerpt.
type Match struct {
	Offset  int    `json:"offset"`
	Line    int    `json:"line"`
	Excerpt string `json:"excerpt"`
}

// FindResult is a bounded search answer.
type FindResult struct {
	TotalMatches   int     `json:"total_matches"`
	Matches        []Match `json:"matches"`
	MatchesOmitted int     `json:"matches_omitted"`
}

// FindOptions tunes FindMatches. MaxMatches 0 means the default (20); a
// caller-supplied value is clamped to 1–20, so pass -1 for a client's 0.
type FindOptions struct {
	MaxMatches int
	IgnoreCase bool
}

// FindMatches is a bounded literal search, so an agent can locate an anchor
// without reading the document back. With IgnoreCase both sides are lowercased
// rune by rune (which keeps offsets aligned with the source; JavaScript's
// toLowerCase could shift them for a handful of characters such as U+0130).
func FindMatches(source, query string, opts FindOptions) (*FindResult, error) {
	if n := jsstr.UTF16Len(query); n == 0 || n > MaxFindChars {
		return nil, apierror.BadRequest(fmt.Sprintf("query must be 1-%d characters", MaxFindChars), "search_query_invalid", nil)
	}
	limit := opts.MaxMatches
	if limit == 0 {
		limit = MaxMatches
	}
	limit = min(max(1, limit), MaxMatches)

	haystack, needle := source, query
	if opts.IgnoreCase {
		haystack, needle = strings.Map(unicode.ToLower, source), strings.Map(unicode.ToLower, query)
	}
	needleUnits := jsstr.UTF16Len(needle)

	res := &FindResult{Matches: []Match{}}
	var units []uint16
	cur := cursor{hay: haystack, src: source}
	at := strings.Index(haystack, needle)
	for at != -1 {
		res.TotalMatches++
		if len(res.Matches) < limit {
			srcByte, off16 := cur.advance(at)
			if units == nil {
				units = utf16.Encode([]rune(source))
			}
			res.Matches = append(res.Matches, Match{
				Offset:  off16,
				Line:    lineOf(source, srcByte),
				Excerpt: excerpt(units, off16, needleUnits),
			})
		}
		next := strings.Index(haystack[at+len(needle):], needle)
		if next == -1 {
			break
		}
		at += len(needle) + next
	}
	res.MatchesOmitted = max(0, res.TotalMatches-len(res.Matches))
	return res, nil
}

// cursor walks the (possibly lowercased) haystack and the source in step,
// mapping a haystack byte offset to the source byte offset and UTF-16 offset
// of the same rune. Offsets only move forward.
type cursor struct {
	hay, src        string
	hb, sb, units16 int
}

func (c *cursor) advance(hayByte int) (int, int) {
	for c.hb < hayByte {
		_, hn := utf8.DecodeRuneInString(c.hay[c.hb:])
		r, sn := utf8.DecodeRuneInString(c.src[c.sb:])
		c.hb += hn
		c.sb += sn
		if r >= 0x10000 {
			c.units16 += 2
		} else {
			c.units16++
		}
	}
	return c.sb, c.units16
}

// isJSSpace is JavaScript's \s (and String.prototype.trim) set.
func isJSSpace(u uint16) bool {
	switch u {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return u >= 0x2000 && u <= 0x200a
}

// excerpt is a short, single-line excerpt around a UTF-16 offset — enough for
// a human or a model to recognise the site without shipping the surrounding
// document.
func excerpt(text []uint16, offset, length int) string {
	pad := max(0, (MaxContextChars-length)/2)
	start := max(0, offset-pad)
	end := min(len(text), offset+length+pad)
	// .replace(/\s+/g, " ").trim()
	body := make([]uint16, 0, end-start)
	inSpace := false
	for _, u := range text[start:end] {
		if isJSSpace(u) {
			if !inSpace {
				body = append(body, ' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		body = append(body, u)
	}
	for len(body) > 0 && body[0] == ' ' {
		body = body[1:]
	}
	for len(body) > 0 && body[len(body)-1] == ' ' {
		body = body[:len(body)-1]
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	b.WriteString(string(utf16.Decode(body)))
	if end < len(text) {
		b.WriteString("…")
	}
	return b.String()
}
