// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// gapDay is 2026-09-01 plus n days: day 29 is 2026-09-30, day 55 2026-10-26.
func gapDay(n int) string {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, n).Format("2006-01-02")
}

// Two sources joined per (deal, day): an SSP field group and a DSP one.
var (
	gapSSPFields = []string{"revenue", "grossMargin", "platformFee", "sspImpressions", "clicks"}
	gapDSPFields = []string{"dspSpend", "dspImpressions", "conversions"}
)

// gapDeal is one entity of a synthetic trader payload. last* are day indexes
// (gapDay); a negative lastSSP means the deal never had SSP fields.
type gapDeal struct {
	id               string
	campaign         string
	lastRow          int
	lastSSP, lastDSP int
	// nullAfter writes the stopped SSP fields as null instead of leaving
	// them out; zeroAfter writes the stopped DSP fields as 0.
	nullAfter, zeroAfter bool
	// budget, when set, is a constant on every row.
	budget float64
	// lastField overrides one field's last day.
	lastField map[string]int
}

func fullDeal(id, campaign string, last int) gapDeal {
	return gapDeal{id: id, campaign: campaign, lastRow: last, lastSSP: last, lastDSP: last}
}

// traderPayload is one row per (dealId, date) over days days, with the
// distractions a real payload carries: a campaign (coarser than the deal), a
// day label (one value per day) and a row id (one value per row).
func traderPayload(deals []gapDeal, days int) *jsonv.Object {
	rows := []any{}
	n := 0
	for d := 0; d < days; d++ {
		for _, deal := range deals {
			if d > deal.lastRow {
				continue
			}
			n++
			row := jsonv.ObjectOf(
				"rowId", fmt.Sprintf("r-%05d", n),
				"date", gapDay(d),
				"dateLabel", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, d).Format("Jan 2"),
				"campaign", deal.campaign,
				"dealId", deal.id,
			)
			last := func(field string, group int) int {
				if v, ok := deal.lastField[field]; ok {
					return v
				}
				return group
			}
			// Metrics move from day to day, as real ones do.
			for i, f := range gapSSPFields {
				switch {
				case d <= last(f, deal.lastSSP):
					row.Set(f, float64(100+i+d%7))
				case deal.nullAfter && deal.lastSSP >= 0:
					row.Set(f, nil)
				}
			}
			for i, f := range gapDSPFields {
				switch {
				case d <= last(f, deal.lastDSP):
					row.Set(f, float64(10+i+d%5))
				case deal.zeroAfter:
					row.Set(f, float64(0))
				}
			}
			if deal.budget != 0 {
				row.Set("budget", deal.budget)
			}
			rows = append(rows, row)
		}
	}
	return jsonv.ObjectOf("dataThrough", gapDay(days-1), "rows", rows)
}

// eightDeals is the incident's shape: the SSP fields of the first four deals
// stop on 2026-09-30 while all their DSP fields carry on to 2026-10-26.
func eightDeals(nullAfter bool) []gapDeal {
	var deals []gapDeal
	for i := 1; i <= 8; i++ {
		d := fullDeal(fmt.Sprintf("D-%02d", i), "Northwind", 55)
		if i > 4 {
			d.campaign = "Contoso"
		} else {
			d.lastSSP, d.nullAfter = 29, nullAfter
		}
		deals = append(deals, d)
	}
	return deals
}

func TestCoverageGapsFlagsFieldsThatStopForSomeEntities(t *testing.T) {
	for _, nullAfter := range []bool{false, true} {
		t.Run(fmt.Sprintf("null=%v", nullAfter), func(t *testing.T) {
			got := DetectCoverageGaps(traderPayload(eightDeals(nullAfter), 56))
			if got.Version != CoverageGapsVersion || got.Truncated || len(got.Gaps) != 1 {
				t.Fatalf("want one finding, got %+v", got)
			}
			g := got.Gaps[0]
			want := CoverageGap{
				Path: "rows", DateField: "date", KeyField: "dealId",
				Fields:    []string{"revenue", "grossMargin", "platformFee", "sspImpressions", "clicks"},
				StoppedOn: "2026-09-30", RowsThrough: "2026-10-26", FieldsThrough: "2026-10-26",
				EntityCount: 4, Entities: []string{"D-01", "D-02", "D-03", "D-04"},
				Message: `In rows, revenue, grossMargin, platformFee, sspImpressions and clicks end on 2026-09-30 for 4 dealIds ("D-01", "D-02", "D-03" and 1 more), whose rows continue to 2026-10-26.`,
			}
			if fmt.Sprintf("%+v", g) != fmt.Sprintf("%+v", want) {
				t.Fatalf("finding\n got %+v\nwant %+v", g, want)
			}
		})
	}
}

func TestCoverageGapsWireShape(t *testing.T) {
	b, err := json.Marshal(DetectCoverageGaps(traderPayload(eightDeals(false), 56)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`{"v":1,"gaps":[{"path":"rows","date_field":"date","key_field":"dealId",`,
		`"fields_omitted":0,"stopped_on":"2026-09-30","rows_through":"2026-10-26","fields_through":"2026-10-26","entity_count":4,`,
	} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("%s\nmissing %s", b, want)
		}
	}
	if strings.Contains(string(b), "truncated") {
		t.Fatalf("truncated is only written when set: %s", b)
	}
	none, _ := json.Marshal(NoCoverageGaps())
	if string(none) != `{"v":1,"gaps":[]}` {
		t.Fatalf("no gaps = %s", none)
	}
}

func TestCoverageGapsLeaveLegitimateShapesAlone(t *testing.T) {
	cases := map[string]func() []gapDeal{
		"an entity that ended entirely": func() []gapDeal {
			deals := eightDeals(false)
			for i := range deals[:4] {
				deals[i].lastSSP, deals[i].lastDSP, deals[i].lastRow = 29, 29, 29
			}
			return deals
		},
		"fields an entity never had": func() []gapDeal {
			deals := eightDeals(false)
			for i := range deals[:4] {
				deals[i].lastSSP = -1
			}
			return deals
		},
		"a two-day lag between the groups": func() []gapDeal {
			deals := eightDeals(false)
			for i := range deals[:4] {
				deals[i].lastSSP = 53
			}
			return deals
		},
		"one source two days behind for every entity": func() []gapDeal {
			deals := eightDeals(false)
			for i := range deals {
				deals[i].lastSSP = 53
			}
			return deals
		},
		"a source that stopped for every entity": func() []gapDeal {
			deals := eightDeals(false)
			for i := range deals {
				deals[i].lastSSP = 29
			}
			return deals
		},
		"rows of nulls after an entity stopped delivering": func() []gapDeal {
			deals := eightDeals(true)
			for i := range deals[:4] {
				deals[i].lastDSP = 29
			}
			return deals
		},
		"a paused deal one source still lists with zeros": func() []gapDeal {
			deals := eightDeals(false)
			for i := range deals[:4] {
				deals[i].lastDSP, deals[i].zeroAfter = 29, true
			}
			return deals
		},
		"a constant budget on every row after the deal stopped": func() []gapDeal {
			deals := eightDeals(false)
			for i := range deals {
				deals[i].budget = 5000
			}
			for i := range deals[:4] {
				deals[i].lastDSP = 29
			}
			return deals
		},
	}
	for name, deals := range cases {
		t.Run(name, func(t *testing.T) {
			if got := DetectCoverageGaps(traderPayload(deals(), 56)); len(got.Gaps) != 0 || got.Truncated {
				t.Fatalf("flagged: %+v", got)
			}
		})
	}
}

func TestCoverageGapsSeeThroughConstants(t *testing.T) {
	// A budget on every row does not hide a real gap either.
	deals := eightDeals(false)
	for i := range deals {
		deals[i].budget = 5000
	}
	got := DetectCoverageGaps(traderPayload(deals, 56))
	if len(got.Gaps) != 1 || got.Gaps[0].EntityCount != 4 || got.Gaps[0].RowsThrough != "2026-10-26" {
		t.Fatalf("%+v", got)
	}
}

// Names come from the payload, and the result is stored as jsonb, which
// refuses U+0000 and invalid UTF-8.
func TestCoverageGapsStoreSafely(t *testing.T) {
	deals := eightDeals(false)
	deals[0].id = "D-\x00-\xff"
	got := DetectCoverageGaps(traderPayload(deals, 56))
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Gaps) != 1 || strings.Contains(string(b), `\u0000`) || !strings.Contains(string(b), "D-\uFFFD-\uFFFD") {
		t.Fatalf("%s", b)
	}
}

func TestCoverageGapsThresholdIsThreeDays(t *testing.T) {
	deals := eightDeals(false)
	deals[0].lastSSP = 55 - CoverageGapDays
	for i := range deals[1:4] {
		deals[i+1].lastSSP = 55
	}
	got := DetectCoverageGaps(traderPayload(deals, 56))
	if len(got.Gaps) != 1 || got.Gaps[0].StoppedOn != gapDay(52) || got.Gaps[0].EntityCount != 1 {
		t.Fatalf("a three-day gap is a gap: %+v", got)
	}
	if want := `In rows, revenue, grossMargin, platformFee, sspImpressions and clicks end on 2026-10-23 for dealId "D-01", whose rows continue to 2026-10-26.`; got.Gaps[0].Message != want {
		t.Fatalf("message\n got %s\nwant %s", got.Gaps[0].Message, want)
	}
}

func TestCoverageGapsSkipUnallocatedStyleArrays(t *testing.T) {
	// One number field per row: a source whose revenue goes null while its
	// rows carry on has nothing else to be compared with.
	var rows []any
	for d := 0; d < 56; d++ {
		for _, source := range []string{"alpha", "beta", "gamma"} {
			var revenue any = float64(d)
			if source == "beta" && d > 29 {
				revenue = nil
			}
			rows = append(rows, jsonv.ObjectOf("date", gapDay(d), "source", source, "revenue", revenue))
		}
	}
	data := jsonv.ObjectOf("unallocated", rows, "rows", traderPayload(eightDeals(false), 56).Value("rows"))
	got := DetectCoverageGaps(data)
	if len(got.Gaps) != 1 || got.Gaps[0].Path != "rows" {
		t.Fatalf("only the trader rows have a gap: %+v", got)
	}
}

func TestCoverageGapsIdentifierRule(t *testing.T) {
	t.Run("the finest entity, not a day label, a row id or a coarser grouping", func(t *testing.T) {
		got := DetectCoverageGaps(traderPayload(eightDeals(false), 56))
		if len(got.Gaps) != 1 || got.Gaps[0].KeyField != "dealId" {
			t.Fatalf("key = %+v", got)
		}
	})
	t.Run("a coarser grouping when it is the only entity", func(t *testing.T) {
		data := traderPayload(eightDeals(false), 56)
		for _, r := range data.Value("rows").([]any) {
			r.(*jsonv.Object).Delete("dealId")
		}
		// Grouped by campaign, Northwind's SSP fields still stop while
		// Contoso's carry on.
		got := DetectCoverageGaps(data)
		if len(got.Gaps) != 1 || got.Gaps[0].KeyField != "campaign" || got.Gaps[0].Entities[0] != "Northwind" {
			t.Fatalf("key = %+v", got)
		}
	})
	t.Run("only labels of the day and row ids", func(t *testing.T) {
		data := traderPayload(eightDeals(false), 56)
		for _, r := range data.Value("rows").([]any) {
			r.(*jsonv.Object).Delete("dealId")
			r.(*jsonv.Object).Delete("campaign")
		}
		if got := DetectCoverageGaps(data); len(got.Gaps) != 0 {
			t.Fatalf("no entity, no finding: %+v", got)
		}
	})
	t.Run("no date field", func(t *testing.T) {
		data := traderPayload(eightDeals(false), 56)
		for _, r := range data.Value("rows").([]any) {
			r.(*jsonv.Object).Delete("date")
		}
		if got := DetectCoverageGaps(data); len(got.Gaps) != 0 {
			t.Fatalf("no day, no finding: %+v", got)
		}
	})
	t.Run("the finest of several date fields", func(t *testing.T) {
		data := traderPayload(eightDeals(false), 56)
		for _, r := range data.Value("rows").([]any) {
			// A flight start is date-like too, but constant per deal.
			r.(*jsonv.Object).Set("flightStart", "2026-08-15")
		}
		got := DetectCoverageGaps(data)
		if len(got.Gaps) != 1 || got.Gaps[0].DateField != "date" {
			t.Fatalf("date field = %+v", got)
		}
	})
}

func TestCoverageGapsGroupFieldsBySameDayAndEntities(t *testing.T) {
	deals := eightDeals(false)
	// D-05's clicks alone stop on 2026-10-11; D-06's DSP spend on 2026-10-01.
	deals[4].lastField = map[string]int{"clicks": 40}
	deals[5].lastField = map[string]int{"dspSpend": 30}
	got := DetectCoverageGaps(traderPayload(deals, 56))
	var summary []string
	for _, g := range got.Gaps {
		summary = append(summary, fmt.Sprintf("%s %s %v", g.StoppedOn, strings.Join(g.Fields, "+"), g.Entities))
	}
	want := []string{
		"2026-09-30 revenue+grossMargin+platformFee+sspImpressions+clicks [D-01 D-02 D-03 D-04]",
		"2026-10-01 dspSpend [D-06]",
		"2026-10-11 clicks [D-05]",
	}
	if strings.Join(summary, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings\n got %q\nwant %q", summary, want)
	}
	if got.Gaps[2].Message != `In rows, clicks ends on 2026-10-11 for dealId "D-05", whose rows continue to 2026-10-26.` {
		t.Fatalf("message %s", got.Gaps[2].Message)
	}
	if s := CoverageGapsSummary(got.Gaps); !strings.HasSuffix(s, " (+2 more gaps)") || !strings.HasPrefix(s, got.Gaps[0].Message) {
		t.Fatalf("summary %s", s)
	}
	if CoverageGapsSummary(got.Gaps[:2]) != got.Gaps[0].Message+" (+1 more gap)" || CoverageGapsSummary(nil) != "" {
		t.Fatal("summary pluralization")
	}
}

func TestCoverageGapsNestedArrays(t *testing.T) {
	data := jsonv.ObjectOf("sections", jsonv.ObjectOf("deals", traderPayload(eightDeals(false), 56).Value("rows")))
	got := DetectCoverageGaps(data)
	if len(got.Gaps) != 1 || got.Gaps[0].Path != "sections.deals" || !strings.HasPrefix(got.Gaps[0].Message, "In sections.deals, ") {
		t.Fatalf("nested: %+v", got)
	}
}

func TestCoverageGapsAreBounded(t *testing.T) {
	t.Run("entities listed", func(t *testing.T) {
		var deals []gapDeal
		for i := 1; i <= 30; i++ {
			d := fullDeal(fmt.Sprintf("D-%02d", i), "Northwind", 55)
			if i <= 12 {
				d.lastSSP = 29
			}
			deals = append(deals, d)
		}
		deals[0].id = strings.Repeat("x", 200)
		got := DetectCoverageGaps(traderPayload(deals, 56))
		g := got.Gaps[0]
		if g.EntityCount != 12 || len(g.Entities) != gapListEntities {
			t.Fatalf("entities %d listed of %d", len(g.Entities), g.EntityCount)
		}
		if !strings.Contains(g.Message, "for 12 dealIds (") || !strings.Contains(g.Message, " and 9 more)") {
			t.Fatalf("message %s", g.Message)
		}
		for _, e := range g.Entities {
			if len(e) > 3*profileMessageValueLen+3 {
				t.Fatalf("entity not clipped: %d", len(e))
			}
		}
	})
	t.Run("findings", func(t *testing.T) {
		deals := eightDeals(false)
		// Every deal's fields stop on its own day: many findings.
		var many []gapDeal
		for i := 0; i < 40; i++ {
			d := fullDeal(fmt.Sprintf("D-%02d", i), "Northwind", 55)
			d.lastField = map[string]int{"clicks": 10 + i%30, "conversions": 11 + i%30}
			many = append(many, d)
		}
		got := DetectCoverageGaps(traderPayload(append(deals, many...), 56))
		if len(got.Gaps) != gapMaxFindings || !got.Truncated {
			t.Fatalf("%d findings, truncated=%v", len(got.Gaps), got.Truncated)
		}
		for i := 1; i < len(got.Gaps); i++ {
			if got.Gaps[i-1].StoppedOn > got.Gaps[i].StoppedOn {
				t.Fatalf("not oldest first: %s before %s", got.Gaps[i-1].StoppedOn, got.Gaps[i].StoppedOn)
			}
		}
	})
	t.Run("too many entities is free text, not an entity", func(t *testing.T) {
		var rows []any
		for d := 0; d < 10; d++ {
			for e := 0; e < gapMaxEntities/5+1; e++ {
				row := jsonv.ObjectOf("date", gapDay(d), "id", fmt.Sprintf("e-%d", e), "a", float64(d+1))
				if d < 3 || e%2 == 0 {
					row.Set("b", float64(d+2))
				}
				rows = append(rows, row)
			}
		}
		// Within the cap: flagged.
		if got := DetectCoverageGaps(jsonv.ObjectOf("rows", rows)); len(got.Gaps) != 1 {
			t.Fatalf("within the cap: %+v", got.Gaps)
		}
		rows = rows[:0]
		for d := 0; d < 10; d++ {
			for e := 0; e < gapMaxEntities+1; e++ {
				row := jsonv.ObjectOf("date", gapDay(d), "id", fmt.Sprintf("e-%d", e), "a", float64(d+1))
				if d < 3 || e%2 == 0 {
					row.Set("b", float64(d+2))
				}
				rows = append(rows, row)
			}
		}
		if got := DetectCoverageGaps(jsonv.ObjectOf("rows", rows)); len(got.Gaps) != 0 {
			t.Fatalf("over the cap: %+v", got.Gaps)
		}
	})
	t.Run("not an array of rows", func(t *testing.T) {
		for _, data := range []any{nil, "x", float64(1), []any{}, []any{"a", "b"}, jsonv.NewObject()} {
			if got := DetectCoverageGaps(data); len(got.Gaps) != 0 || got.Truncated || got.Version != CoverageGapsVersion {
				t.Fatalf("%v: %+v", data, got)
			}
		}
	})
}

// gapPage is a managed document carrying data.
func gapPage(data *jsonv.Object) string {
	env := jsonv.ObjectOf("contract_version", float64(1), "refreshed_at", "2026-10-27T06:00:00.000Z",
		"source_as_of", "2026-10-26T00:00:00.000Z", "data", data)
	return `<!doctype html><html><body><script id="pages-data-schema" type="application/schema+json">{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}</script>` +
		`<script id="pages-data" type="application/json">` + EscapedEnvelopeJSON(env) + `</script></body></html>`
}

func TestCoverageGapsOfHTML(t *testing.T) {
	data := traderPayload(eightDeals(false), 56)
	html := gapPage(data)
	if _, err := ParseManagedHTML(html); err != nil {
		t.Fatalf("fixture is not a managed page: %v", err)
	}
	want, _ := json.Marshal(DetectCoverageGaps(data))
	got, _ := json.Marshal(CoverageGapsOfHTML(html))
	if string(got) != string(want) {
		t.Fatalf("from HTML\n got %s\nwant %s", got, want)
	}
	for name, doc := range map[string]string{
		"plain page":      "<!doctype html><p>Hello, Northwind</p>",
		"empty":           "",
		"broken payload":  strings.Replace(html, `"data":{`, `"data":{,`, 1),
		"data not object": `<script id="pages-data-schema" type="application/schema+json">{}</script><script id="pages-data" type="application/json">{"data":[1]}</script>`,
	} {
		if got := CoverageGapsOfHTML(doc); len(got.Gaps) != 0 || got.Version != CoverageGapsVersion {
			t.Fatalf("%s: %+v", name, got)
		}
	}
}
