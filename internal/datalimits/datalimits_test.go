// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package datalimits

// Ported from test/data-limits.test.js (the parts that need only this
// package; the inline-transport checks belong to the MCP layer).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

func small(t *testing.T) {
	t.Setenv("PAGES_DATA_MAX_BYTES", "2048")
	t.Setenv("PAGES_MCP_MAX_INLINE_DATA_BYTES", "1024")
	t.Setenv("MAX_HTML_BYTES", "4kb")
}

func TestLimitsDistinguishPayloadEnvelopeAndTransport(t *testing.T) {
	small(t)
	l, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := Limits{MaxPayloadBytes: 2048, MaxEnvelopeBytes: 2048, MaxInlineBytes: 1024, MaxStagedBytes: 2097152, MaxRequestBytes: 4096, MinEnvelopeOverheadBytes: MinEnvelopeOverheadBytes}
	if l != want {
		t.Errorf("%+v", l)
	}
	if got := jsonv.Stringify(l); got != `{"max_payload_bytes":2048,"max_envelope_bytes":2048,"max_inline_bytes":1024,"max_staged_bytes":2097152,"max_request_bytes":4096,"min_envelope_overhead_bytes":114}` {
		t.Errorf("wire %s", got)
	}
	for _, boundary := range []string{"payload", "envelope"} {
		for _, n := range []int{2047, 2048} {
			if err := AssertDomainSize(n, boundary, ""); err != nil {
				t.Error(err)
			}
		}
		err := AssertDomainSize(2049, boundary, "")
		e, ok := apierror.As(err)
		if !ok || e.Code != "data_validation_failed" || e.Status != 400 {
			t.Fatalf("%v", err)
		}
		d := e.Details.(DomainSizeDetails)
		if d.SizeLimit != boundary || d.ChangingTransportCanHelp || d.Bytes != 2049 || d.MaxBytes != 2048 {
			t.Errorf("%+v", d)
		}
	}
}

func TestEscapingAndUTF8CountAgainstTheEnvelope(t *testing.T) {
	data := jsonv.ObjectOf("label", "Northwind <&>  é")
	m := MeasureData(data)
	if m.PayloadBytes != len(jsonv.Stringify(data)) || m.EscapedPayloadBytes != m.PayloadBytes+21 {
		t.Errorf("%+v", m)
	}
}

func TestMinimumEnvelopeOverhead(t *testing.T) {
	// {"contract_version":1,"refreshed_at":"…","source_as_of":"…","data":{}} minus the data braces.
	if MinEnvelopeOverheadBytes != 114 {
		t.Errorf("overhead %d", MinEnvelopeOverheadBytes)
	}
	if EnvelopeOverhead("-000001-12-31T23:00:00.000Z", "2000-01-01T00:00:00.000Z") != 117 {
		t.Error("extended years are longer")
	}
}

func TestAdvisoryMeasurements(t *testing.T) {
	small(t)
	size := func(s string) any { v, _ := jsonv.ParseString(s); return v }
	if err := AssertStagedDataSize("data", nil, false); err != nil {
		t.Error(err)
	}
	if err := AssertStagedDataSize("data", size(`{"payload_bytes":2,"escaped_payload_bytes":2}`), true); err != nil {
		t.Error(err)
	}
	if err := AssertStagedDataSize("page", size(`{"payload_bytes":2,"escaped_payload_bytes":2}`), true); err == nil || !regexp.MustCompile(`(?i)only.*data`).MatchString(err.Error()) {
		t.Errorf("%v", err)
	}
	for _, s := range []string{`{"payload_bytes":-1,"escaped_payload_bytes":2}`, `{"payload_bytes":2,"escaped_payload_bytes":1}`,
		`{"payload_bytes":2.5,"escaped_payload_bytes":3}`, `{"payload_bytes":2,"escaped_payload_bytes":9007199254740992}`, `null`, `[]`,
		`{"payload_bytes":2,"escaped_payload_bytes":2,"x":1}`} {
		if err := AssertStagedDataSize("data", size(s), true); err == nil || !strings.Contains(err.Error(), "data_size") {
			t.Errorf("%s: %v", s, err)
		}
	}
	err := AssertStagedDataSize("data", size(`{"payload_bytes":2049,"escaped_payload_bytes":2049}`), true)
	if e, _ := apierror.As(err); e == nil || e.Details.(DomainSizeDetails).SizeLimit != "payload" {
		t.Errorf("%v", err)
	}
	err = AssertStagedDataSize("data", size(`{"payload_bytes":350,"escaped_payload_bytes":2100}`), true)
	if e, _ := apierror.As(err); e == nil || e.Details.(DomainSizeDetails).SizeLimit != "envelope" {
		t.Errorf("%v", err)
	}
}

func TestGuidanceAdvertisesResolvedLimits(t *testing.T) {
	small(t)
	g := DataLimitGuidance()
	for _, n := range []string{"2048", "1024", "4096", "2097152", "114 metadata bytes"} {
		if !strings.Contains(g, n) {
			t.Errorf("guidance lacks %s", n)
		}
	}
	for _, kind := range []string{"page", "data"} {
		if !strings.Contains(FileUploadGuidance(kind, ""), "as few appends as returned max_chunk_bytes allows") {
			t.Error(kind)
		}
	}
	if !strings.HasPrefix(FileUploadGuidance("data", "mcp_pages_"), "Managed-data limits") || !strings.Contains(FileUploadGuidance("data", "mcp_pages_"), "mcp_pages_update_page_data_upload with upload_id") {
		t.Error("data guidance")
	}
	if strings.Contains(FileUploadGuidance("page", ""), "kind='data'") {
		t.Error("page guidance")
	}
}

func TestStartupOverridesRetainValidatedDefaults(t *testing.T) {
	t.Setenv("PAGES_DATA_MAX_BYTES", "invalid")
	t.Setenv("PAGES_MCP_MAX_INLINE_DATA_BYTES", "0")
	t.Setenv("MAX_HTML_BYTES", "1.5mb")
	l, err := Load()
	if err != nil || l.MaxPayloadBytes != 1048576 || l.MaxInlineBytes != 1500000 || l.MaxRequestBytes != 1572864 {
		t.Errorf("%+v %v", l, err)
	}
	t.Setenv("MAX_HTML_BYTES", "2 megabytes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), `MAX_HTML_BYTES="2 megabytes" is not a positive byte size`) {
		t.Errorf("%v", err)
	}
	if MaxRequestBytes() != 2*1024*1024 {
		t.Error("fallback")
	}
	t.Setenv("MAX_HTML_BYTES", "")
	if n, err := RequestBytes(); err != nil || n != 2*1024*1024 {
		t.Error("empty means unset: the 2mb default")
	}
}

func TestParseByteSize(t *testing.T) {
	for text, want := range map[string]int{"999": 999, "999b": 999, "4kb": 4096, "4KB": 4096, "2mb": 2 << 20, "2 Mb": 2 << 20,
		" 3mb ": 3 << 20, "1.5mb": 1572864, "0.5kb": 512, "1.0001kb": 1024, "1gb": 1 << 30} {
		if got, err := ParseByteSize(text, "MAX_HTML_BYTES"); err != nil || got != want {
			t.Errorf("%q: %d %v", text, got, err)
		}
	}
	msg := regexp.MustCompile(`^MAX_HTML_BYTES=".*" is not a positive byte size; use a number of bytes or a value such as 512kb, 2mb or 1gb$`)
	for _, text := range []string{"", "0", "0.1b", "garbage", "2mbx", "2MB2", "1e6", "-1mb", "+2mb", "1tb", "2 m b", ".5mb", "1.", "0x10", "Infinity", "99999999999gb"} {
		if _, err := ParseByteSize(text, "MAX_HTML_BYTES"); err == nil || !msg.MatchString(err.Error()) {
			t.Errorf("%q: %v", text, err)
		}
	}
}

// TestGuidanceMatchesJavaScript compares the prose with what lib/data-limits.js
// and lib/upload-guidance.js produce at their defaults (testdata/guidance.json,
// regenerated by internal/pagedata/testdata/generate.js).
func TestGuidanceMatchesJavaScript(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "guidance.json"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := jsonv.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	g := v.(*jsonv.Object)
	for key, got := range map[string]string{
		"data": DataLimitGuidance(), "page": FileUploadGuidance("page", ""), "page_prefixed": FileUploadGuidance("page", "mcp_pages_"),
		"data_upload": FileUploadGuidance("data", ""), "data_prefixed": FileUploadGuidance("data", "mcp_pages_"),
	} {
		if want := g.Value(key); got != want {
			t.Errorf("%s:\n got %s\nwant %s", key, got, want)
		}
	}
	err = AssertDomainSize(1<<21, "envelope", "")
	e, _ := apierror.As(err)
	want := g.Value("domain_error").(*jsonv.Object)
	if e == nil || e.Message != want.Value("message") || jsonv.Stringify(e.Details) != jsonv.Stringify(want.Value("details")) {
		t.Errorf("domain error:\n got %v %s\nwant %s", err, jsonv.Stringify(e.Details), jsonv.Stringify(want))
	}
}

func TestUsageReportsHeadroom(t *testing.T) {
	small(t)
	u := MeasureUsage(jsonv.ObjectOf("rows", []any{1.0, 2.0}))
	if u.PayloadBytes != 14 || u.EnvelopeBytes != 14+MinEnvelopeOverheadBytes || u.LimitBytes != 2048 || u.HeadroomBytes != 2048-u.EnvelopeBytes {
		t.Errorf("%+v", u)
	}
	if got := jsonv.Stringify(u); got != `{"payload_bytes":14,"envelope_bytes":128,"limit_bytes":2048,"headroom_bytes":1920}` {
		t.Errorf("wire %s", got)
	}
}
