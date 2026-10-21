// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// ── payload profiling (issue #102) ───────────────────────────────────────────
// A schema validates SHAPE. It cannot tell you that a refresh covers 7 days
// where the last one covered 31, or that a deal which had spend has vanished
// from the rows. Both happen in practice: a series starts late because only the
// newest file of a trailing-window export was read, and totals are understated
// because rows were filtered to the configured deals, dropping one that carried
// real spend. Every such payload satisfies the schema perfectly.
//
// So compute what the payload actually CONTAINS — row counts, date extents,
// numeric totals, and the distinct values of low-cardinality keys — and hand it
// back on both the write and the read. The write additionally has the previous
// payload in hand already (the data update parses the published HTML to
// materialize against it), so comparing the two costs no extra I/O.
//
// Bounds exist because this lands in a model's context and runs on the request
// path. A pathological payload gets an honest partial profile, never a stall.
const (
	profileMaxArrays   = 8
	profileMaxFields   = 24
	profileMaxScalars  = 24
	profileMaxDepth    = 6
	profileMaxDistinct = 50 // above this a string field is free text
	// …and only this many are LISTED on the wire, so a 50-deal campaign cannot
	// flood the response.
	profileListDistinct = 20
	// A value longer than this is prose, not an identifier, so it is not echoed
	// verbatim on the wire either. It is still TRACKED for membership —
	// dropping it from the comparison is how a single long placement name would
	// silently switch off the dimension check for its field.
	profileMaxValueLen = 120
	// How many values a warning names, and how long each may be.
	profileNameInMessage   = 8
	profileMessageValueLen = 60
	maxDataWarnings        = 40
	coverageValueLen       = 32
	coverageNameLen        = 80
	coverageMaxDateFields  = 6
	coverageMaxScalars     = 8
	// CoverageMaxBytes bounds the serialized coverage profile.
	CoverageMaxBytes = 3072
)

// DefaultExpectTolerance is the relative slack on an expect.totals check. A
// caller summing the same column in a different order gets a different last
// float bit; 0.01% absorbs that and still catches the ~7% understatement that
// caused #102.
const DefaultExpectTolerance = 0.0001

var datePrefixRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}`)

func isDateLike(v any) bool {
	s, ok := v.(string)
	return ok && datePrefixRe.MatchString(s)
}

// FieldProfile is one field of a profiled array. Its wire shape depends on
// Kind:
//
//	number: {kind, sum, min, max, nulls}
//	date:   {kind, min, max, distinct, nulls, distinct_overflow?}
//	key/text: {kind, distinct, nulls, distinct_overflow?, values?, values_omitted?}
//
// The complete tracked value set (members) is never serialized: values is the
// bounded top-N summary that goes on the wire, while CompareDataProfiles
// reasons over the whole set. A profile that has crossed the wire has no
// members, and the diff then refuses to claim a value is missing from a list
// it knows is partial.
type FieldProfile struct {
	Kind             string
	Sum              any // number kind: float64, or nil when the sum overflowed
	Min, Max         any // float64 (number) or string (date)
	Distinct         int
	Nulls            int
	DistinctOverflow bool
	Values           *jsonv.Object // value → count, most frequent first (key kind)
	ValuesOmitted    int

	members   []string // insertion order, as a JS Set iterates
	memberSet map[string]bool
}

// WriteJSON writes the field in its wire shape.
func (f *FieldProfile) WriteJSON(b *strings.Builder) error {
	o := jsonv.ObjectOf("kind", f.Kind)
	switch f.Kind {
	case "number":
		o.Set("sum", f.Sum)
		o.Set("min", f.Min)
		o.Set("max", f.Max)
		o.Set("nulls", float64(f.Nulls))
	case "date":
		o.Set("min", f.Min)
		o.Set("max", f.Max)
		o.Set("distinct", float64(f.Distinct))
		o.Set("nulls", float64(f.Nulls))
		if f.DistinctOverflow {
			o.Set("distinct_overflow", true)
		}
	default:
		o.Set("distinct", float64(f.Distinct))
		o.Set("nulls", float64(f.Nulls))
		if f.DistinctOverflow {
			o.Set("distinct_overflow", true)
		} else {
			if f.Values != nil {
				o.Set("values", f.Values)
			}
			if f.ValuesOmitted > 0 {
				o.Set("values_omitted", float64(f.ValuesOmitted))
			}
		}
	}
	s, err := jsonv.Marshal(o)
	b.WriteString(s)
	return err
}

// MarshalJSON writes the field in its wire shape.
func (f *FieldProfile) MarshalJSON() ([]byte, error) { return marshalWriter(f) }

// ArrayProfile is one profiled array: its length and per-field profiles.
type ArrayProfile struct {
	Count  int
	Fields *Ordered[*FieldProfile]
}

// WriteJSON writes {count, fields}.
func (a *ArrayProfile) WriteJSON(b *strings.Builder) error {
	fmt.Fprintf(b, `{"count":%d,"fields":`, a.Count)
	if err := a.Fields.WriteJSON(b); err != nil {
		return err
	}
	b.WriteByte('}')
	return nil
}

// MarshalJSON writes {count, fields}.
func (a *ArrayProfile) MarshalJSON() ([]byte, error) { return marshalWriter(a) }

// Profile is what a payload contains, at the dotted paths a caller can name.
type Profile struct {
	Arrays  *Ordered[*ArrayProfile]
	Scalars *jsonv.Object
	// capped records that a cap cut the profile short. It is kept off the wire
	// so the full read's data_profile stays byte-identical, while the summary
	// read can still say its coverage is partial.
	capped bool
}

// WriteJSON writes {arrays, scalars}.
func (p *Profile) WriteJSON(b *strings.Builder) error {
	b.WriteString(`{"arrays":`)
	if err := p.Arrays.WriteJSON(b); err != nil {
		return err
	}
	b.WriteString(`,"scalars":`)
	s, err := jsonv.Marshal(p.Scalars)
	if err != nil {
		return err
	}
	b.WriteString(s)
	b.WriteByte('}')
	return nil
}

// MarshalJSON writes {arrays, scalars}.
func (p *Profile) MarshalJSON() ([]byte, error) { return marshalWriter(p) }

// Capped reports whether a profile cap (arrays, fields, scalars, depth) cut
// this profile short.
func (p *Profile) Capped() bool { return p != nil && p.capped }

func marshalWriter(w jsonv.Writer) ([]byte, error) {
	var b strings.Builder
	err := w.WriteJSON(&b)
	return []byte(b.String()), err
}

// Ordered is a string-keyed map with JavaScript property order (array-index
// keys first), the order the JS profile objects serialized in.
type Ordered[V any] struct{ o jsonv.Object }

// NewOrdered returns an empty ordered map.
func NewOrdered[V any]() *Ordered[V] { return &Ordered[V]{} }

// Set stores v at k.
func (m *Ordered[V]) Set(k string, v V) { m.o.Set(k, v) }

// Get returns the value at k.
func (m *Ordered[V]) Get(k string) (V, bool) {
	v, ok := m.o.Get(k)
	if !ok {
		var zero V
		return zero, false
	}
	return v.(V), true
}

// Delete removes k.
func (m *Ordered[V]) Delete(k string) { m.o.Delete(k) }

// Keys returns the keys in JavaScript order.
func (m *Ordered[V]) Keys() []string { return m.o.Keys() }

// Len is the number of entries.
func (m *Ordered[V]) Len() int { return m.o.Len() }

// WriteJSON writes the map as a JSON object.
func (m *Ordered[V]) WriteJSON(b *strings.Builder) error {
	s, err := jsonv.Marshal(&m.o)
	b.WriteString(s)
	return err
}

// MarshalJSON writes the map as a JSON object.
func (m *Ordered[V]) MarshalJSON() ([]byte, error) { return marshalWriter(m) }

// roundSum: floats do not add associatively, so a sum reported as
// 20044.102999999992 invites a caller to "fix" a number that is already right.
// Six decimals is past anything money or impressions need and short of float
// noise. It is Number(value.toFixed(6)).
func roundSum(v float64) any {
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return nil
	}
	if v == math.Trunc(v) {
		return v
	}
	f, _ := strconv.ParseFloat(jsToFixed(v, 6), 64)
	return f
}

// jsToFixed is Number.prototype.toFixed: the exact decimal value rounded half
// away from zero ("pick the larger n" on a tie), exponential form at 1e21.
func jsToFixed(v float64, digits int) string {
	if math.Abs(v) >= 1e21 {
		return jsonv.FormatNumber(v)
	}
	exact := new(big.Float).SetPrec(0).SetFloat64(v).Text('f', 1100)
	neg := strings.HasPrefix(exact, "-")
	exact = strings.TrimPrefix(exact, "-")
	intPart, frac, _ := strings.Cut(exact, ".")
	frac += strings.Repeat("0", digits+1)
	keep := intPart + frac[:digits]
	roundUp := frac[digits] >= '5'
	n := new(big.Int)
	n.SetString(keep, 10)
	if roundUp {
		n.Add(n, big.NewInt(1))
	}
	s := n.String()
	for len(s) <= digits {
		s = "0" + s
	}
	out := s[:len(s)-digits]
	if digits > 0 {
		out += "." + s[len(s)-digits:]
	}
	if neg {
		out = "-" + out
	}
	return out
}

type fieldAcc struct {
	present, nulls, numbers, dates, strings int
	sum                                     float64
	numMin, numMax                          float64
	hasNum                                  bool
	dateMin, dateMax                        string
	hasDate                                 bool
	counts                                  map[string]int
	order                                   []string
	overflow                                bool
}

func profileRows(rows []any, onCapped func()) *Ordered[*FieldProfile] {
	fields := NewOrdered[*fieldAcc]()
	field := func(name string) *fieldAcc {
		if f, ok := fields.Get(name); ok {
			return f
		}
		if fields.Len() >= profileMaxFields {
			onCapped()
			return nil
		}
		f := &fieldAcc{counts: map[string]int{}}
		fields.Set(name, f)
		return f
	}
	for _, r := range rows {
		row, ok := r.(*jsonv.Object)
		if !ok {
			continue
		}
		for _, key := range row.Keys() {
			f := field(key)
			if f == nil {
				continue
			}
			value := row.Value(key)
			f.present++
			switch v := value.(type) {
			case nil:
				f.nulls++
			case float64:
				if math.IsInf(v, 0) || math.IsNaN(v) {
					continue
				}
				f.numbers++
				f.sum += v
				if !f.hasNum || v < f.numMin {
					f.numMin = v
				}
				if !f.hasNum || v > f.numMax {
					f.numMax = v
				}
				f.hasNum = true
			case string:
				if isDateLike(v) {
					f.dates++
					// Lexicographic comparison is correct for ISO-8601 prefixes.
					if !f.hasDate || jsstr.Less(v, f.dateMin) {
						f.dateMin = v
					}
					if !f.hasDate || jsstr.Less(f.dateMax, v) {
						f.dateMax = v
					}
					f.hasDate = true
				} else {
					f.strings++
				}
				if _, seen := f.counts[v]; seen {
					f.counts[v]++
				} else if len(f.counts) < profileMaxDistinct {
					f.counts[v] = 1
					f.order = append(f.order, v)
				} else {
					f.overflow = true
				}
			}
		}
	}

	out := NewOrdered[*FieldProfile]()
	for _, name := range fields.Keys() {
		f, _ := fields.Get(name)
		switch {
		case f.numbers > 0 && f.dates == 0 && f.strings == 0:
			out.Set(name, &FieldProfile{Kind: "number", Sum: roundSum(f.sum), Min: f.numMin, Max: f.numMax, Nulls: f.nulls})
			continue
		case f.dates > 0 && f.numbers == 0:
			out.Set(name, &FieldProfile{Kind: "date", Min: f.dateMin, Max: f.dateMax, Distinct: len(f.counts), Nulls: f.nulls, DistinctOverflow: f.overflow})
			continue
		}
		// A dimension worth enumerating, or free text. The distinction is
		// purely cardinality: a dealId has a handful of values, a note has one
		// per row.
		entry := &FieldProfile{Kind: "key", Distinct: len(f.counts), Nulls: f.nulls}
		if f.overflow {
			entry.Kind = "text"
			entry.DistinctOverflow = true
		} else {
			listed := append([]string(nil), f.order...)
			sort.SliceStable(listed, func(i, j int) bool {
				ci, cj := f.counts[listed[i]], f.counts[listed[j]]
				if ci != cj {
					return ci > cj
				}
				return jsstr.Less(listed[i], listed[j])
			})
			values := jsonv.NewObject()
			shown := 0
			for _, v := range listed {
				if jsstr.UTF16Len(v) > profileMaxValueLen || shown >= profileListDistinct {
					continue
				}
				values.Set(v, float64(f.counts[v]))
				shown++
			}
			entry.Values = values
			if len(listed) > shown {
				entry.ValuesOmitted = len(listed) - shown
			}
			entry.members = f.order
			entry.memberSet = make(map[string]bool, len(f.order))
			for _, v := range f.order {
				entry.memberSet[v] = true
			}
		}
		out.Set(name, entry)
	}
	return out
}

// ProfileData profiles a payload: arrays of objects are the rows a refresh is
// made of; the top-level scalars (dataThrough, lastRefreshed) are the stamps
// that were also wrong in #102.
func ProfileData(data any) *Profile {
	p := &Profile{Arrays: NewOrdered[*ArrayProfile](), Scalars: jsonv.NewObject()}
	arraysProfiled, scalarsRecorded := 0, 0
	var walk func(value any, path string, depth int)
	walk = func(value any, path string, depth int) {
		if depth > profileMaxDepth {
			p.capped = true
			return
		}
		switch v := value.(type) {
		case []any:
			objectRows := 0
			for _, e := range v {
				if _, ok := e.(*jsonv.Object); ok {
					objectRows++
				}
			}
			switch {
			case objectRows > 0 && arraysProfiled < profileMaxArrays:
				arraysProfiled++
				p.Arrays.Set(path, &ArrayProfile{Count: len(v), Fields: profileRows(v, func() { p.capped = true })})
			case arraysProfiled < profileMaxArrays:
				// An array of scalars still has a length worth reconciling.
				arraysProfiled++
				p.Arrays.Set(path, &ArrayProfile{Count: len(v), Fields: NewOrdered[*FieldProfile]()})
			default:
				p.capped = true
			}
			return
		case *jsonv.Object:
			for _, k := range v.Keys() {
				next := k
				if path != "" {
					next = path + "." + k
				}
				walk(v.Value(k), next, depth+1)
			}
			return
		}
		if path != "" {
			if scalarsRecorded < profileMaxScalars {
				scalarsRecorded++
				p.Scalars.Set(path, value)
			} else {
				p.capped = true
			}
		}
	}
	walk(data, "", 0)
	return p
}

func clipCoverageValue(v any) (any, bool) {
	s, ok := v.(string)
	if ok && jsstr.UTF16Len(s) > coverageValueLen {
		return jsstr.Slice(s, 0, coverageValueLen-1) + "…", true
	}
	return v, false
}

func profileBytes(p *Profile) int {
	var b strings.Builder
	_ = p.WriteJSON(&b)
	return b.Len()
}

// BoundedCoverageProfile is the part of a profile that decides whether a
// refresh is due: how many rows each array holds and which dates they span,
// plus date-stamped scalars. Numeric sums, key values and free-text scalars are
// dropped; a refresh run reads this on every invocation, and the full profile
// of a wide payload is tens of kilobytes. It is also BOUNDED: retained values
// are clipped, long names skipped, each array keeps at most six date fields,
// and the serialized result never exceeds maxBytes (CoverageMaxBytes when <=
// 0). truncated is true when ANY coverage is missing — cut by ProfileData's own
// caps, clipped here, or left out to stay within the bound.
func BoundedCoverageProfile(profile *Profile, maxBytes int) (*Profile, bool) {
	if maxBytes <= 0 {
		maxBytes = CoverageMaxBytes
	}
	out := &Profile{Arrays: NewOrdered[*ArrayProfile](), Scalars: jsonv.NewObject()}
	truncated := profile.Capped()
	fits := func() bool { return profileBytes(out) <= maxBytes }
	for _, path := range profile.Arrays.Keys() {
		entry, _ := profile.Arrays.Get(path)
		if jsstr.UTF16Len(path) > coverageNameLen {
			truncated = true
			continue
		}
		fields := NewOrdered[*FieldProfile]()
		kept := 0
		for _, name := range entry.Fields.Keys() {
			field, _ := entry.Fields.Get(name)
			if field.Kind != "date" {
				continue
			}
			if jsstr.UTF16Len(name) > coverageNameLen || kept >= coverageMaxDateFields {
				truncated = true
				continue
			}
			c := *field
			var clipped bool
			if c.Min, clipped = clipCoverageValue(field.Min); clipped {
				truncated = true
			}
			if c.Max, clipped = clipCoverageValue(field.Max); clipped {
				truncated = true
			}
			fields.Set(name, &c)
			kept++
		}
		out.Arrays.Set(path, &ArrayProfile{Count: entry.Count, Fields: fields})
		if fits() {
			continue
		}
		truncated = true
		// Keep what fits of this array: its count, then its fields one by one.
		partial := &ArrayProfile{Count: entry.Count, Fields: NewOrdered[*FieldProfile]()}
		out.Arrays.Set(path, partial)
		if !fits() {
			out.Arrays.Delete(path)
			continue
		}
		for _, name := range fields.Keys() {
			f, _ := fields.Get(name)
			partial.Fields.Set(name, f)
			if !fits() {
				partial.Fields.Delete(name)
				break
			}
		}
	}
	scalarsKept := 0
	for _, path := range profile.Scalars.Keys() {
		value := profile.Scalars.Value(path)
		if !isDateLike(value) {
			continue
		}
		if jsstr.UTF16Len(path) > coverageNameLen || jsstr.UTF16Len(value.(string)) > profileMessageValueLen || scalarsKept >= coverageMaxScalars {
			truncated = true
			continue
		}
		out.Scalars.Set(path, value)
		if fits() {
			scalarsKept++
			continue
		}
		out.Scalars.Delete(path)
		truncated = true
	}
	return out, truncated
}

// CoverageProfile is BoundedCoverageProfile's profile alone.
func CoverageProfile(profile *Profile) *Profile {
	p, _ := BoundedCoverageProfile(profile, CoverageMaxBytes)
	return p
}

// ── comparing profiles ───────────────────────────────────────────────────────

// Warning is one data-profile finding. Previous and Current are a count, a
// date string, a hash, or a bounded list of values, depending on Code.
type Warning struct {
	Code     string `json:"code"`
	Path     string `json:"path"`
	Message  string `json:"message"`
	Previous any    `json:"previous"`
	Current  any    `json:"current"`
}

// WriteJSON writes the warning with JavaScript number formatting.
func (w Warning) WriteJSON(b *strings.Builder) error {
	s, err := jsonv.Marshal(jsonv.ObjectOf("code", w.Code, "path", w.Path, "message", w.Message, "previous", w.Previous, "current", w.Current))
	b.WriteString(s)
	return err
}

// MarshalJSON writes the warning.
func (w Warning) MarshalJSON() ([]byte, error) { return marshalWriter(w) }

func truncateForMessage(v string) string {
	if jsstr.UTF16Len(v) <= profileMessageValueLen {
		return v
	}
	return jsstr.Slice(v, 0, profileMessageValueLen) + "…"
}

// capWarnings keeps the list bounded, and SAYS so rather than truncating
// silently: a dropped warning nobody knows was dropped is the failure this
// module exists to prevent.
func capWarnings(w []Warning) []Warning {
	if len(w) <= maxDataWarnings {
		return w
	}
	kept := append([]Warning(nil), w[:maxDataWarnings]...)
	dropped := len(w) - maxDataWarnings
	plural := "s"
	if dropped == 1 {
		plural = ""
	}
	return append(kept, Warning{
		Code: "warnings_truncated",
		Path: "",
		Message: fmt.Sprintf("%d further warning%s were omitted to keep this response bounded. ", dropped, plural) +
			"A payload that narrows this many fields at once should be reconciled against its source before publishing, " +
			"not triaged warning by warning.",
		Previous: float64(len(w)),
		Current:  float64(maxDataWarnings),
	})
}

// CompareOptions carries what CompareDataProfiles may use beyond the two
// profiles: the caller's expect object (parsed JSON) and both data hashes.
type CompareOptions struct {
	Expected        any
	PreviousDataSHA string
	NextDataSHA     string
}

func dateString(v any) string { s, _ := v.(string); return s }

// CompareDataProfiles reports what got smaller. WARNINGS, never errors:
// narrowing is sometimes exactly right (a new flight, a deal that genuinely
// ended), so this must not wedge a legitimate refresh. But both halves of #102
// are here — "31 days became 7" as a coverage regression, "a deal vanished" as
// a missing dimension value.
func CompareDataProfiles(previous, next *Profile, opts CompareOptions) []Warning {
	warnings := []Warning{}
	if next == nil {
		return warnings
	}
	expected, _ := opts.Expected.(*jsonv.Object)
	// The hole in comparison-based detection: a window can be WRONG IN THE
	// FIRST PAYLOAD. With no baseline to regress from a diff finds nothing, so
	// when there is no baseline AND no expect naming this array, say so.
	expectNames := func(path string) bool {
		if expected == nil {
			return false
		}
		if rc, ok := expected.Value("row_count").(*jsonv.Object); ok && rc.Has(path) {
			return true
		}
		prefix := path + "."
		for _, group := range []string{"totals", "date_range"} {
			if g, ok := expected.Value(group).(*jsonv.Object); ok {
				for _, k := range g.Keys() {
					if strings.HasPrefix(k, prefix) {
						return true
					}
				}
			}
		}
		return false
	}

	for _, path := range next.Arrays.Keys() {
		after, _ := next.Arrays.Get(path)
		var before *ArrayProfile
		if previous != nil {
			before, _ = previous.Arrays.Get(path)
		}
		if before == nil || before.Count == 0 {
			// Only a data SERIES earns this. An array of scalars has no
			// coverage to verify, and warning about one is the noise that
			// trains a reader to skip the field.
			if after.Fields.Len() > 0 && after.Count > 0 && !expectNames(path) {
				window := ""
				for _, name := range after.Fields.Keys() {
					if f, _ := after.Fields.Get(name); f.Kind == "date" {
						window = fmt.Sprintf(" (%s to %s)", jsString(f.Min), jsString(f.Max))
						break
					}
				}
				prev := 0
				if before != nil {
					prev = before.Count
				}
				warnings = append(warnings, Warning{
					Code: "coverage_unverified",
					Path: path,
					Message: fmt.Sprintf("%s has %d rows and the payload it replaces had none, so there is nothing to compare ", path, after.Count) +
						"this against and you supplied no expect for it. Its coverage" + window +
						" and totals are UNVERIFIED. A first payload that is already truncated looks exactly like a correct one " +
						"here. Reconcile data_profile against the source, or state the window and totals in expect so Pages can refuse a payload that disagrees.",
					Previous: float64(prev),
					Current:  float64(after.Count),
				})
			}
			continue
		}
		if after.Count < before.Count {
			warnings = append(warnings, Warning{
				Code:     "row_count_dropped",
				Path:     path,
				Message:  fmt.Sprintf("%s went from %d rows to %d. If the source is cumulative, this refresh is missing data.", path, before.Count, after.Count),
				Previous: float64(before.Count),
				Current:  float64(after.Count),
			})
		}
		for _, name := range after.Fields.Keys() {
			afterField, _ := after.Fields.Get(name)
			beforeField, ok := before.Fields.Get(name)
			if !ok {
				continue
			}
			fieldPath := path + "." + name
			if afterField.Kind == "date" && beforeField.Kind == "date" {
				bMin, aMin := dateString(beforeField.Min), dateString(afterField.Min)
				if bMin != "" && aMin != "" && jsstr.Less(bMin, aMin) {
					warnings = append(warnings, Warning{
						Code: "coverage_start_regressed",
						Path: fieldPath,
						Message: fmt.Sprintf("%s now starts at %s, later than the %s already published. ", fieldPath, aMin, bMin) +
							"A trailing-window source export reads exactly like this — confirm the earlier days are genuinely absent and not merely unread.",
						Previous: bMin,
						Current:  aMin,
					})
				}
				bMax, aMax := dateString(beforeField.Max), dateString(afterField.Max)
				if bMax != "" && aMax != "" && jsstr.Less(aMax, bMax) {
					warnings = append(warnings, Warning{
						Code:     "coverage_end_regressed",
						Path:     fieldPath,
						Message:  fmt.Sprintf("%s now ends at %s, earlier than the %s already published.", fieldPath, aMax, bMax),
						Previous: bMax,
						Current:  aMax,
					})
				}
			}
			// Only over COMPLETE sets, on both sides.
			if beforeField.memberSet != nil && afterField.memberSet != nil {
				var missing []string
				for _, v := range beforeField.members {
					if !afterField.memberSet[v] {
						missing = append(missing, v)
					}
				}
				if len(missing) > 0 {
					named := missing
					if len(named) > profileNameInMessage {
						named = named[:profileNameInMessage]
					}
					quoted := make([]string, len(named))
					for i, v := range named {
						quoted[i] = jsonv.Quote(truncateForMessage(v))
					}
					more := ""
					if len(missing) > profileNameInMessage {
						more = fmt.Sprintf(" and %d more", len(missing)-profileNameInMessage)
					}
					warnings = append(warnings, Warning{
						Code: "dimension_values_missing",
						Path: fieldPath,
						Message: fmt.Sprintf("%s no longer contains %s%s, ", fieldPath, strings.Join(quoted, ", "), more) +
							"which the published payload had. Rows filtered to a configured subset look exactly like this.",
						Previous: boundedValues(beforeField.members),
						Current:  boundedValues(afterField.members),
					})
				}
			}
		}
	}
	// Stale-refresh findings LEAD, so the cap can never drop the one that
	// changes what the caller should say — but only after loss findings have
	// had their say about the same field: "the window did not advance" is noise
	// next to "the window now starts 24 days later".
	regressed := map[string]bool{}
	for _, w := range warnings {
		if w.Code == "coverage_start_regressed" || w.Code == "coverage_end_regressed" {
			regressed[w.Path] = true
		}
	}
	lead := []Warning{}
	for _, w := range staleRefreshWarnings(opts.PreviousDataSHA, opts.NextDataSHA, previous, next) {
		if !regressed[w.Path] {
			lead = append(lead, w)
		}
	}
	return capWarnings(append(lead, warnings...))
}

func boundedValues(members []string) []any {
	n := min(len(members), profileNameInMessage)
	out := make([]any, n)
	for i := 0; i < n; i++ {
		out[i] = truncateForMessage(members[i])
	}
	return out
}

func jsString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return jsonv.FormatNumber(x)
	case nil:
		return "null"
	}
	return fmt.Sprint(v)
}

// staleRefreshWarnings answers "did this refresh actually add anything?" — a
// different question from "what got smaller", which a refresh can slip
// between: a new version, no warnings, and exactly the coverage of the version
// before it. Two distinct answers, because they mean different things:
//   - data_unchanged: the payload is byte-identical (same source file);
//   - coverage_did_not_advance: numbers moved but the window did not (the
//     source restated days already covered).
//
// Both hashes are required: without them the two findings collapse into one
// ambiguous signal.
func staleRefreshWarnings(prevSHA, nextSHA string, previous, next *Profile) []Warning {
	var warnings []Warning
	if previous == nil || next == nil || prevSHA == "" || nextSHA == "" {
		return warnings
	}
	if prevSHA == nextSHA {
		return append(warnings, Warning{
			Code: "data_unchanged",
			Path: "",
			Message: "This payload is byte-identical to the one already published, so no metric changed. A version was still " +
				"created because source_as_of advanced, which is how a re-verified source is recorded — but if you " +
				"expected new numbers, the source did not contain any. Do not report this as a data update.",
			Previous: prevSHA,
			Current:  nextSHA,
		})
	}
	for _, path := range next.Arrays.Keys() {
		after, _ := next.Arrays.Get(path)
		before, ok := previous.Arrays.Get(path)
		if !ok || before.Count == 0 {
			continue
		}
		for _, name := range after.Fields.Keys() {
			afterField, _ := after.Fields.Get(name)
			beforeField, ok := before.Fields.Get(name)
			if !ok || afterField.Kind != "date" || beforeField.Kind != "date" {
				continue
			}
			// A genuine RESTATEMENT and nothing else: same first day, same
			// last day, same number of rows, different figures.
			aMax, bMax := dateString(afterField.Max), dateString(beforeField.Max)
			aMin, bMin := dateString(afterField.Min), dateString(beforeField.Min)
			if bMax != "" && aMax != "" && aMax == bMax && bMin != "" && aMin != "" && aMin == bMin && after.Count == before.Count {
				warnings = append(warnings, Warning{
					Code: "coverage_did_not_advance",
					Path: path + "." + name,
					Message: fmt.Sprintf("%s.%s still ends at %s, the same day the published payload ended on. ", path, name, aMax) +
						"Figures changed but the window did not, so this refresh restated days that were already covered " +
						"and added no new one. If a newer day was expected, the source did not have it — say that " +
						"rather than reporting a refresh.",
					Previous: bMax,
					Current:  aMax,
				})
			}
		}
	}
	return warnings
}

// ── expect ───────────────────────────────────────────────────────────────────

// jsNumber is Number(v) for a parsed JSON value.
func jsNumber(v any) float64 {
	switch x := v.(type) {
	case nil:
		return 0
	case bool:
		if x {
			return 1
		}
		return 0
	case float64:
		return x
	case string:
		return jsstr.Number(x)
	case []any:
		switch len(x) {
		case 0:
			return 0
		case 1:
			return jsstr.Number(jsArrayString(x[0]))
		}
	}
	return math.NaN()
}

func jsArrayString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = jsArrayString(e)
		}
		return strings.Join(parts, ",")
	case *jsonv.Object:
		return "[object Object]"
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return jsString(v)
}

// entries is Object.entries for a parsed JSON value.
func entries(v any) ([]string, []any) {
	switch x := v.(type) {
	case *jsonv.Object:
		keys := x.Keys()
		vals := make([]any, len(keys))
		for i, k := range keys {
			vals[i] = x.Value(k)
		}
		return keys, vals
	case []any:
		keys := make([]string, len(x))
		for i := range x {
			keys[i] = strconv.Itoa(i)
		}
		return keys, x
	case string:
		var keys []string
		var vals []any
		for i, r := range []rune(x) {
			keys = append(keys, strconv.Itoa(i))
			vals = append(vals, string(r))
		}
		return keys, vals
	}
	return nil, nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	}
	return true
}

// ReconciliationDetails is the detail object of a refused expect.
type ReconciliationDetails struct {
	Mismatches []*jsonv.Object `json:"mismatches"`
}

// AssertExpectedProfile enforces the caller's own arithmetic. An agent that
// has just summed a CSV can state the totals it believes it wrote, and Pages
// refuses the write if the payload disagrees. Paths are the dotted paths the
// profile reports. expect is the parsed JSON value (nil when absent or null);
// errorCode defaults to data_reconciliation_failed.
func AssertExpectedProfile(profile *Profile, expect any, errorCode string) error {
	if errorCode == "" {
		errorCode = "data_reconciliation_failed"
	}
	if expect == nil {
		return nil
	}
	exp, ok := expect.(*jsonv.Object)
	if !ok {
		return apierror.BadRequest("expect must be an object", errorCode, nil)
	}
	tolerance := DefaultExpectTolerance
	if t, present := exp.Get("tolerance"); present && t != nil {
		tolerance = jsNumber(t)
	}
	if math.IsNaN(tolerance) || math.IsInf(tolerance, 0) || tolerance < 0 || tolerance > 1 {
		return apierror.BadRequest("expect.tolerance must be a fraction between 0 and 1", errorCode, nil)
	}
	var mismatches []*jsonv.Object
	fieldAt := func(path string) *FieldProfile {
		cut := strings.LastIndex(path, ".")
		if cut < 0 {
			return nil
		}
		arr, ok := profile.Arrays.Get(path[:cut])
		if !ok {
			return nil
		}
		f, _ := arr.Fields.Get(path[cut+1:])
		return f
	}
	group := func(name string) any {
		v := exp.Value(name)
		if !truthy(v) {
			return jsonv.NewObject()
		}
		return v
	}

	keys, vals := entries(group("row_count"))
	for i, path := range keys {
		want := vals[i]
		arr, ok := profile.Arrays.Get(path)
		if !ok {
			mismatches = append(mismatches, jsonv.ObjectOf("check", "row_count", "path", path, "expected", want, "actual", nil, "detail", "no array at that path"))
		} else if float64(arr.Count) != jsNumber(want) {
			mismatches = append(mismatches, jsonv.ObjectOf("check", "row_count", "path", path, "expected", jsNumber(want), "actual", float64(arr.Count)))
		}
	}

	keys, vals = entries(group("totals"))
	for i, path := range keys {
		wanted := jsNumber(vals[i])
		f := fieldAt(path)
		if f == nil || f.Kind != "number" {
			mismatches = append(mismatches, jsonv.ObjectOf("check", "total", "path", path, "expected", wanted, "actual", nil, "detail", "no numeric field at that path"))
			continue
		}
		// Relative where there is something to be relative to, absolute at zero.
		slack := math.Abs(wanted) * tolerance
		if slack == 0 || math.IsNaN(slack) {
			slack = tolerance
		}
		sum := 0.0 // null - wanted is -wanted in JavaScript
		if s, ok := f.Sum.(float64); ok {
			sum = s
		}
		if math.Abs(sum-wanted) > slack {
			mismatches = append(mismatches, jsonv.ObjectOf("check", "total", "path", path, "expected", wanted, "actual", f.Sum, "tolerance", slack))
		}
	}

	keys, vals = entries(group("date_range"))
	for i, path := range keys {
		want := vals[i]
		f := fieldAt(path)
		arr, ok := want.([]any)
		if !ok || len(arr) != 2 {
			return apierror.BadRequest(fmt.Sprintf(`expect.date_range["%s"] must be [min, max]`, path), errorCode, nil)
		}
		if f == nil || f.Kind != "date" {
			mismatches = append(mismatches, jsonv.ObjectOf("check", "date_range", "path", path, "expected", want, "actual", nil, "detail", "no date field at that path"))
			continue
		}
		if !strictEqual(f.Min, arr[0]) || !strictEqual(f.Max, arr[1]) {
			mismatches = append(mismatches, jsonv.ObjectOf("check", "date_range", "path", path, "expected", want, "actual", []any{f.Min, f.Max}))
		}
	}

	if len(mismatches) == 0 {
		return nil
	}
	parts := make([]string, len(mismatches))
	for i, m := range mismatches {
		parts[i] = fmt.Sprintf("%s at %s expected %s, payload has %s", m.Value("check"), m.Value("path"),
			jsonStringify(m.Value("expected")), jsonStringify(m.Value("actual")))
	}
	return apierror.BadRequest(
		"the payload does not match what you said it would contain, so nothing was written: "+strings.Join(parts, "; "),
		errorCode,
		ReconciliationDetails{Mismatches: mismatches},
	)
}

// jsonStringify is JSON.stringify for a value that may be undefined-free.
func jsonStringify(v any) string { return jsonv.Stringify(v) }

func strictEqual(a, b any) bool {
	switch x := a.(type) {
	case string:
		y, ok := b.(string)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && x == y
	case nil:
		return b == nil
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	}
	return false
}
