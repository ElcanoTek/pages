// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pagedata

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/pages/internal/jsonv"
)

// nearLimitPage is an object-row payload near the Node release's 1 MiB data limit, like the
// fixture in test/page-data-read.test.js.
func nearLimitPage(tb testing.TB) string {
	tb.Helper()
	var rows []string
	size := 0
	for i := 0; size < 900_000; i++ {
		row := fmt.Sprintf(`{"date":"2026-%02d-%02d","campaign":"Contoso %d","placement":"Fabrikam placement %d","dsp":"Northwind","impressions":%d,"clicks":%d,"spend":%g,"conversions":%d}`,
			1+i%12, 1+i%28, i%12, i%300, 1000+i, i%97, 12.5+float64(i%31), i%7)
		rows = append(rows, row)
		size += len(row) + 1
	}
	schema := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","required":["daily"],"properties":{"daily":{"type":"array","items":{"type":"object","required":["date","spend"],"properties":{"date":{"type":"string","pattern":"^\\d{4}-\\d{2}-\\d{2}$"},"campaign":{"type":"string"},"placement":{"type":"string"},"dsp":{"type":"string","enum":["Northwind","Contoso"]},"impressions":{"type":"integer","minimum":0},"clicks":{"type":"integer"},"spend":{"type":"number"},"conversions":{"type":"integer"}},"additionalProperties":false}}}}`
	env := `{"contract_version":1,"refreshed_at":"2026-09-20T06:02:11.000Z","source_as_of":"2026-09-21T00:00:00.000Z","data":{"daily":[` + strings.Join(rows, ",") + `]}}`
	return `<!doctype html><html><body><script id="pages-data-schema" type="application/schema+json">` + schema +
		`</script><script id="pages-data" type="application/json">` + env + `</script></body></html>`
}

func BenchmarkParseManagedNearLimit(b *testing.B) {
	html := nearLimitPage(b)
	b.SetBytes(int64(len(html)))
	for i := 0; i < b.N; i++ {
		if _, err := ParseManagedHTML(html); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMaterializeAndProfileNearLimit(b *testing.B) {
	html := nearLimitPage(b)
	m, err := ParseManagedHTML(html)
	if err != nil {
		b.Fatal(err)
	}
	data := m.Data()
	now := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	for i := 0; i < b.N; i++ {
		if _, err := Materialize(m, data, "2026-09-21T00:00:00Z", now); err != nil {
			b.Fatal(err)
		}
		_ = jsonv.Stringify(ProfileData(data))
	}
}
