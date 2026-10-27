// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// ── coverage gaps ─────────────────────────────────────────────────────────────
// A payload can pass every check the profile makes — rows for every day, every
// entity present, the window advancing — and still be missing a large part of
// what it shows. A dashboard row often joins two sources: one source's fields
// and another's for the same entity and day. When a refresh reads the first
// source for only some of the entities (a filter scoped to one campaign, say),
// those entities' first-source fields stop while their second-source fields
// keep arriving. Every refresh succeeds and source_as_of keeps advancing,
// because the second source does. It happened: for four weeks a dashboard
// showed a small fraction of one campaign's revenue, and nothing said so.
//
// Only the rows show it. For those entities a group of number fields ends
// weeks before the entity's own rows do, while the same fields keep arriving
// for every other entity. DetectCoverageGaps looks for exactly that, in the
// payload alone. It does not depend on what the refresh reported, and it knows
// nothing about any particular template.
//
// The rule, for each array of objects (top level or nested, within the same
// bounds as ProfileData):
//
//   - Day: the row's date-like field (a value starting YYYY-MM-DD), compared
//     by calendar day. If several fields are date-like, the one with the most
//     distinct days is used: that is the row's own grain, not a flight's start
//     or end. Rows without a valid day are ignored.
//   - Number fields: fields that hold only numbers or null. A row "carries" f
//     when f is a number there. null or absent does not count.
//   - Identifier: the finest-grained entity. Candidates are string fields that
//     are never date-like and that behave like an entity over time: some day
//     holds two or more of their values (so the field is not a label for the
//     day, such as "Oct 26" or a week name), and some value spans two or more
//     days (so it is not a row id or free text). Of those, the one with the
//     most distinct values wins (at most gapMaxEntities). On a tie, the first
//     in row order wins. Only the first gapMaxCandidates string fields are
//     considered. An array with no such field, or with fewer than two number
//     fields, is not checked.
//   - For each entity v, and each number field f that v has carried at least
//     once:
//     last_f(v) is the latest day a row of v carries f;
//     last_any(v) is the latest day v carries a non-zero value of a number
//     field whose value changes across v's rows: the last day v delivered.
//     A row of nulls is not delivery, nor a row of zeros (one source still
//     listing a paused deal while the other has dropped it), nor a constant
//     repeated on every row (a budget or a flight goal). Each of those would
//     make a deal that stopped delivering look like one whose other fields
//     carry on;
//     global_last(f) is the latest day any row carries f;
//     newest is the array's latest day of delivery (the greatest last_any).
//     f is a gap for v when all three hold:
//     newest − last_any(v) < CoverageGapDays (v is still delivering, not a
//     deal that ended weeks ago with an old discrepancy between its sources,
//     nor one whose last stray row is a single impression);
//     last_any(v) − last_f(v) ≥ CoverageGapDays (v carried on without f);
//     global_last(f) − last_f(v) ≥ CoverageGapDays (f still arrives for
//     other entities).
//   - Fields that stop on the same day for the same set of entities form one
//     finding.
//
// It deliberately does not flag:
//   - an entity that stopped delivering altogether (a deal that ended or
//     paused, whatever rows of nulls, zeros or constants it still has);
//   - an entity that is no longer delivering, whatever happened to its fields
//     before it stopped: what a refresh misses today is what matters;
//   - a field an entity never carried;
//   - one source lagging another by a day or two;
//   - a field that stopped for every entity (the source went stale, which
//     refresh health's source_stale judges);
//   - an array with a single number field (there is nothing to compare it
//     with).
//
// Bounds: the arrays and depth ProfileData walks, gapMaxFields fields per
// array, gapMaxEntities entities, and gapMaxFindings findings, each naming at
// most gapListEntities entities and gapListFields fields. The work is a few
// linear passes over each array's rows, the same order of cost as profiling.

// CoverageGapDays is how many days a field must trail both its own entity's
// rows and the other entities before it counts as a gap. Sources legitimately
// lag one another by a day or two (one reports through yesterday, another
// through the day before), so two days is normal and three is not.
const CoverageGapDays = 3

// CoverageGapsVersion names the rule above. Stored results carry it as "v".
// When the rule or CoverageGapDays changes, bump it, and a result computed
// under the old rule is recomputed instead of trusted.
//
// 1: the first rule. 2: only entities still delivering are judged.
const CoverageGapsVersion = 2

const (
	gapMaxFields       = 48
	gapMaxCandidates   = 8
	gapMaxEntities     = 2000
	gapMaxFindings     = 10
	gapListEntities    = 8
	gapListFields      = 16
	gapMessageEntities = 3
	gapMessageFields   = 6
	noDay              = math.MinInt32
)

// CoverageGap is one finding: the number fields that stopped on one day for
// one set of entities whose rows carried on.
type CoverageGap struct {
	// Path is the array, named the way data_profile names it.
	Path      string `json:"path"`
	DateField string `json:"date_field"`
	KeyField  string `json:"key_field"`
	// Fields are the number fields that stopped, in row order (at most
	// gapListFields). FieldsOmitted counts the rest.
	Fields        []string `json:"fields"`
	FieldsOmitted int      `json:"fields_omitted"`
	// StoppedOn is the last day (YYYY-MM-DD) these entities carried these
	// fields.
	StoppedOn string `json:"stopped_on"`
	// RowsThrough is the latest day these entities still deliver (lastAny).
	RowsThrough string `json:"rows_through"`
	// FieldsThrough is the latest day any row carries these fields.
	FieldsThrough string `json:"fields_through"`
	// EntityCount is how many entities this finding covers. Entities lists at
	// most gapListEntities of them, sorted, each clipped.
	EntityCount int      `json:"entity_count"`
	Entities    []string `json:"entities"`
	// Message is one sentence for a human.
	Message string `json:"message"`
}

// CoverageGaps is what DetectCoverageGaps found in one payload, and what Pages
// stores beside each version (page_versions.coverage_gaps).
type CoverageGaps struct {
	// Version is CoverageGapsVersion at the time of computing.
	Version int           `json:"v"`
	Gaps    []CoverageGap `json:"gaps"`
	// Truncated is set when a bound cut the search or the findings short.
	Truncated bool `json:"truncated,omitempty"`
}

// NoCoverageGaps is the result for a document with no managed data.
func NoCoverageGaps() *CoverageGaps {
	return &CoverageGaps{Version: CoverageGapsVersion, Gaps: []CoverageGap{}}
}

// dayOf is the calendar day a date-like string starts with, as days since the
// Unix epoch. It is not ok for a string that does not start with a valid
// YYYY-MM-DD date.
func dayOf(s string) (int32, bool) {
	if len(s) < 10 || !datePrefixRe.MatchString(s) {
		return 0, false
	}
	t, err := time.Parse("2006-01-02", s[:10])
	if err != nil {
		return 0, false
	}
	// A four-digit year is within ±3.7 million days of the epoch; the check
	// is for the conversion's sake.
	days := t.Unix() / 86400
	if days <= math.MinInt32 || days > math.MaxInt32 {
		return 0, false
	}
	return int32(days), true
}

func dayString(d int32) string {
	return time.Unix(int64(d)*86400, 0).UTC().Format("2006-01-02")
}

// DetectCoverageGaps finds the coverage gaps in a payload (rule above). It
// never returns nil.
func DetectCoverageGaps(data any) *CoverageGaps {
	out := NoCoverageGaps()
	arrays := 0
	var walk func(value any, path string, depth int)
	walk = func(value any, path string, depth int) {
		if depth > profileMaxDepth {
			out.Truncated = true
			return
		}
		switch v := value.(type) {
		case []any:
			if arrays >= profileMaxArrays {
				out.Truncated = true
				return
			}
			arrays++
			gaps, truncated := arrayCoverageGaps(path, v)
			out.Gaps = append(out.Gaps, gaps...)
			out.Truncated = out.Truncated || truncated
		case *jsonv.Object:
			for _, k := range v.Keys() {
				next := k
				if path != "" {
					next = path + "." + k
				}
				walk(v.Value(k), next, depth+1)
			}
		}
	}
	walk(data, "", 0)
	sort.SliceStable(out.Gaps, func(i, j int) bool {
		a, b := out.Gaps[i], out.Gaps[j]
		if a.StoppedOn != b.StoppedOn {
			return a.StoppedOn < b.StoppedOn
		}
		if a.EntityCount != b.EntityCount {
			return a.EntityCount > b.EntityCount
		}
		return a.Path < b.Path
	})
	if len(out.Gaps) > gapMaxFindings {
		out.Gaps = out.Gaps[:gapMaxFindings]
		out.Truncated = true
	}
	return out
}

// gapField is what the first pass learns about one field of an array.
type gapField struct {
	name                         string
	numbers, dates, strs, others int
	days                         map[int32]struct{}
}

// gapCandidate is what the second pass learns about one identifier candidate.
type gapCandidate struct {
	field     int
	values    map[string]int32 // value → the first day it was seen
	overflow  bool             // more than gapMaxEntities values
	multiDay  bool             // some value spans two or more days
	dayFirst  map[int32]string // day → the first value seen on it
	sharesDay bool             // some day holds two or more values
}

type gapEntity struct {
	name string
	// lastF is the last day the entity carried each number field;
	// lastNonZero the last day it carried a non-zero value of it.
	lastF, lastNonZero []int32
	// first is each field's first value for this entity, and varies whether
	// a later one differed: only a field that varies is evidence of delivery.
	first  []float64
	varies []bool
}

// lastAny is the last day the entity delivered: its latest non-zero value of
// a number field that varies.
func (e *gapEntity) lastAny() int32 {
	last := int32(noDay)
	for j, varies := range e.varies {
		if varies {
			last = max(last, e.lastNonZero[j])
		}
	}
	return last
}

func arrayCoverageGaps(path string, rows []any) ([]CoverageGap, bool) {
	truncated := false
	// Pass 1: what kind of value each field holds.
	var fields []*gapField
	index := map[string]int{}
	for _, r := range rows {
		row, ok := r.(*jsonv.Object)
		if !ok {
			continue
		}
		row.Range(func(k string, v any) bool {
			i, seen := index[k]
			if !seen {
				if len(fields) >= gapMaxFields {
					truncated = true
					return true
				}
				i = len(fields)
				index[k] = i
				fields = append(fields, &gapField{name: k})
			}
			f := fields[i]
			switch x := v.(type) {
			case nil:
			case float64:
				f.numbers++
			case string:
				if !isDateLike(x) {
					f.strs++
					break
				}
				f.dates++
				if d, ok := dayOf(x); ok {
					if f.days == nil {
						f.days = map[int32]struct{}{}
					}
					f.days[d] = struct{}{}
				}
			default:
				f.others++
			}
			return true
		})
	}
	dateField := -1
	var numberFields, candidates []int
	for i, f := range fields {
		switch {
		case f.dates > 0 && f.numbers == 0 && f.others == 0:
			if dateField < 0 || len(f.days) > len(fields[dateField].days) {
				dateField = i
			}
		case f.numbers > 0 && f.dates == 0 && f.strs == 0 && f.others == 0:
			numberFields = append(numberFields, i)
		case f.strs > 0 && f.dates == 0 && f.numbers == 0 && f.others == 0 && len(candidates) < gapMaxCandidates:
			candidates = append(candidates, i)
		}
	}
	if dateField < 0 || len(fields[dateField].days) < 2 || len(numberFields) < 2 || len(candidates) == 0 {
		return nil, truncated
	}
	dateName := fields[dateField].name
	rowDay := func(row *jsonv.Object) (int32, bool) {
		s, ok := row.Value(dateName).(string)
		if !ok {
			return 0, false
		}
		return dayOf(s)
	}

	// Pass 2: which candidate behaves like an entity over time.
	cands := make([]*gapCandidate, len(candidates))
	candOf := map[string]*gapCandidate{}
	for i, fi := range candidates {
		cands[i] = &gapCandidate{field: fi, values: map[string]int32{}, dayFirst: map[int32]string{}}
		candOf[fields[fi].name] = cands[i]
	}
	for _, r := range rows {
		row, ok := r.(*jsonv.Object)
		if !ok {
			continue
		}
		day, ok := rowDay(row)
		if !ok {
			continue
		}
		row.Range(func(k string, v any) bool {
			c := candOf[k]
			s, isString := v.(string)
			if c == nil || !isString || c.overflow {
				return true
			}
			if first, seen := c.values[s]; !seen {
				if len(c.values) >= gapMaxEntities {
					c.overflow = true
					c.values, c.dayFirst = nil, nil
					return true
				}
				c.values[s] = day
			} else if first != day {
				c.multiDay = true
			}
			if !c.sharesDay {
				if first, seen := c.dayFirst[day]; !seen {
					c.dayFirst[day] = s
				} else if first != s {
					c.sharesDay = true
					c.dayFirst = nil
				}
			}
			return true
		})
	}
	var key *gapCandidate
	for _, c := range cands {
		if c.overflow || !c.multiDay || !c.sharesDay {
			continue
		}
		if key == nil || len(c.values) > len(key.values) {
			key = c
		}
	}
	if key == nil {
		return nil, truncated
	}
	keyName := fields[key.field].name

	// Pass 3: per entity, the last day each number field arrived.
	numberOf := map[string]int{}
	for j, fi := range numberFields {
		numberOf[fields[fi].name] = j
	}
	global := make([]int32, len(numberFields))
	for j := range global {
		global[j] = noDay
	}
	entityIndex := map[string]int{}
	var entities []*gapEntity
	carried := make([]bool, len(numberFields))
	values := make([]float64, len(numberFields))
	for _, r := range rows {
		row, ok := r.(*jsonv.Object)
		if !ok {
			continue
		}
		day, ok := rowDay(row)
		if !ok {
			continue
		}
		var name string
		hasKey := false
		row.Range(func(k string, v any) bool {
			if j, isNumber := numberOf[k]; isNumber {
				values[j], carried[j] = v.(float64)
			} else if k == keyName {
				name, hasKey = v.(string)
			}
			return true
		})
		var e *gapEntity
		if hasKey {
			i, seen := entityIndex[name]
			if !seen {
				i = len(entities)
				entityIndex[name] = i
				n := len(numberFields)
				e = &gapEntity{name: name, lastF: make([]int32, n), lastNonZero: make([]int32, n),
					first: make([]float64, n), varies: make([]bool, n)}
				for j := range e.lastF {
					e.lastF[j], e.lastNonZero[j] = noDay, noDay
				}
				entities = append(entities, e)
			}
			e = entities[i]
		}
		for j, c := range carried {
			if !c {
				continue
			}
			carried[j] = false
			global[j] = max(global[j], day)
			if e == nil {
				continue
			}
			if e.lastF[j] == noDay {
				e.first[j] = values[j]
			} else if values[j] != e.first[j] {
				e.varies[j] = true
			}
			e.lastF[j] = max(e.lastF[j], day)
			if values[j] != 0 {
				e.lastNonZero[j] = max(e.lastNonZero[j], day)
			}
		}
	}

	// Which fields stopped for which entities, grouped by (field, day).
	type stop struct {
		field int
		day   int32
	}
	// newest is the array's last day of delivery. Only an entity still
	// delivering near it is judged: one that stopped weeks ago is history.
	newest := int32(noDay)
	for _, e := range entities {
		newest = max(newest, e.lastAny())
	}
	byStop := map[stop][]int{}
	var stops []stop
	for ei, e := range entities {
		lastAny := e.lastAny()
		if lastAny == noDay || newest-lastAny >= CoverageGapDays {
			continue
		}
		for j, last := range e.lastF {
			if last == noDay {
				continue
			}
			if lastAny-last >= CoverageGapDays && global[j]-last >= CoverageGapDays {
				s := stop{j, last}
				if _, seen := byStop[s]; !seen {
					stops = append(stops, s)
				}
				byStop[s] = append(byStop[s], ei)
			}
		}
	}
	if len(stops) == 0 {
		return nil, truncated
	}
	// One finding per (day, entity set), its fields in row order.
	type group struct {
		day      int32
		entities []int
		fields   []int
	}
	groupOf := map[string]*group{}
	var groups []*group
	sort.SliceStable(stops, func(i, j int) bool { return stops[i].field < stops[j].field })
	for _, s := range stops {
		ents := byStop[s]
		var sig strings.Builder
		fmt.Fprintf(&sig, "%d", s.day)
		for _, ei := range ents {
			fmt.Fprintf(&sig, ",%d", ei)
		}
		g := groupOf[sig.String()]
		if g == nil {
			g = &group{day: s.day, entities: ents}
			groupOf[sig.String()] = g
			groups = append(groups, g)
		}
		g.fields = append(g.fields, s.field)
	}
	out := make([]CoverageGap, 0, len(groups))
	for _, g := range groups {
		rowsThrough, fieldsThrough := int32(noDay), int32(noDay)
		names := make([]string, len(g.entities))
		for i, ei := range g.entities {
			names[i] = entities[ei].name
			rowsThrough = max(rowsThrough, entities[ei].lastAny())
		}
		sort.Strings(names)
		listed := names[:min(len(names), gapListEntities)]
		clipped := make([]string, len(listed))
		for i, n := range listed {
			clipped[i] = truncateForMessage(gapText(n))
		}
		var stopped []string
		for _, j := range g.fields {
			fieldsThrough = max(fieldsThrough, global[j])
			stopped = append(stopped, gapText(fields[numberFields[j]].name))
		}
		gap := CoverageGap{
			Path: gapText(path), DateField: gapText(dateName), KeyField: gapText(keyName),
			Fields: stopped[:min(len(stopped), gapListFields)], FieldsOmitted: max(0, len(stopped)-gapListFields),
			StoppedOn: dayString(g.day), RowsThrough: dayString(rowsThrough), FieldsThrough: dayString(fieldsThrough),
			EntityCount: len(names), Entities: clipped,
		}
		gap.Message = coverageGapMessage(gap)
		out = append(out, gap)
	}
	return out, truncated
}

// gapText makes a name from the payload safe to store: Postgres jsonb refuses
// U+0000 and invalid UTF-8, and a finding must never be what fails a write.
func gapText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
}

// listWords is "a, b and c", naming at most limit items and counting the rest
// ("a, b, c and 2 more").
func listWords(items []string, limit, extra int) string {
	shown := items
	if len(shown) > limit {
		extra += len(shown) - limit
		shown = shown[:limit]
	}
	switch {
	case extra > 0:
		return strings.Join(shown, ", ") + fmt.Sprintf(" and %d more", extra)
	case len(shown) == 1:
		return shown[0]
	default:
		return strings.Join(shown[:len(shown)-1], ", ") + " and " + shown[len(shown)-1]
	}
}

// coverageGapMessage is the finding as one sentence: "In rows, revenue and
// clicks end on 2026-09-30 for 4 dealIds ("D-1", "D-2", "D-3" and 1 more),
// whose rows continue to 2026-10-26."
func coverageGapMessage(g CoverageGap) string {
	where := g.Path
	if where == "" {
		where = "the payload"
	}
	where = truncateForMessage(where)
	fields := make([]string, len(g.Fields))
	for i, f := range g.Fields {
		fields[i] = truncateForMessage(f)
	}
	verb := "end"
	if len(g.Fields)+g.FieldsOmitted == 1 {
		verb = "ends"
	}
	keyField := truncateForMessage(g.KeyField)
	quoted := make([]string, len(g.Entities))
	for i, e := range g.Entities {
		quoted[i] = jsonv.Quote(e)
	}
	var who string
	if g.EntityCount == 1 && len(quoted) == 1 {
		who = keyField + " " + quoted[0]
	} else {
		who = fmt.Sprintf("%d %ss (%s)", g.EntityCount, keyField,
			listWords(quoted, gapMessageEntities, g.EntityCount-len(quoted)))
	}
	return fmt.Sprintf("In %s, %s %s on %s for %s, whose rows continue to %s.",
		where, listWords(fields, gapMessageFields, g.FieldsOmitted), verb, g.StoppedOn, who, g.RowsThrough)
}

// CoverageGapsSummary is a staff sentence for a list of findings: the first
// finding's message, and how many more there are. "" for none.
func CoverageGapsSummary(gaps []CoverageGap) string {
	if len(gaps) == 0 {
		return ""
	}
	s := gaps[0].Message
	if more := len(gaps) - 1; more > 0 {
		unit := "gaps"
		if more == 1 {
			unit = "gap"
		}
		s += fmt.Sprintf(" (+%d more %s)", more, unit)
	}
	return s
}

// CoverageGapsOfHTML is DetectCoverageGaps over a stored document's #pages-data
// payload, for a version written before Pages computed gaps at write time. It
// reads the data block alone and does not validate it against the schema again:
// the version was validated when it was written, it is immutable, and
// revalidating a large payload would triple the cost of reading it. A document
// without a readable data block has none.
func CoverageGapsOfHTML(html string) *CoverageGaps {
	found, err := findBlocks(html, PageSpec)
	if err != nil || found["data"] == nil {
		return NoCoverageGaps()
	}
	block := found["data"].payloadBlock
	env, err := parseJSONBlock(html[block.ContentStart:block.ContentEnd], DataID, MaxDataBytes())
	if err != nil {
		return NoCoverageGaps()
	}
	envelope, ok := env.(*jsonv.Object)
	if !ok {
		return NoCoverageGaps()
	}
	data, ok := envelope.Value("data").(*jsonv.Object)
	if !ok {
		return NoCoverageGaps()
	}
	return DetectCoverageGaps(data)
}
