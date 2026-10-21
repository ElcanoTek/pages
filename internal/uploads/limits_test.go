// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package uploads

import (
	"bytes"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/pages/internal/apierror"
)

func codeOf(err error) string {
	if e, ok := apierror.As(err); ok {
		return e.Code
	}
	return ""
}

func TestDecodeBase64ChunkIsCanonicalAndBounded(t *testing.T) {
	t.Setenv("PAGE_UPLOAD_MAX_CHUNK_BYTES", "")
	t.Setenv("MAX_HTML_BYTES", "")
	original := []byte("dashboard ☀ data")
	got, err := DecodeBase64Chunk(base64.StdEncoding.EncodeToString(original))
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("decode = %q, %v", got, err)
	}
	maximum := bytes.Repeat([]byte{0xa5}, MaxChunkBytes())
	got, err = DecodeBase64Chunk(base64.StdEncoding.EncodeToString(maximum))
	if err != nil || !bytes.Equal(got, maximum) {
		t.Fatalf("max chunk: %v", err)
	}
	over := base64.StdEncoding.EncodeToString(make([]byte, MaxChunkBytes()+1))
	for _, bad := range []string{"", "not base64", "YQ", "YQ===", "YR==", over, "!"} {
		_, err := DecodeBase64Chunk(bad)
		if codeOf(err) != "upload_chunk_invalid" {
			t.Errorf("%.20q: err = %v", bad, err)
		}
	}
	// A failure is the one moment the caller is definitely reading, so it
	// points at the ticket path and asks for as few appends as possible.
	_, err = DecodeBase64Chunk("not!valid!base64")
	for _, want := range []string{"create_upload_ticket", "as few appends as max_chunk_bytes allows"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %s", want, err)
		}
	}
}

func TestDefaultChunkCeilingStaysInsideTheArgumentBudget(t *testing.T) {
	t.Setenv("PAGE_UPLOAD_MAX_CHUNK_BYTES", "")
	t.Setenv("MAX_HTML_BYTES", "")
	n := MaxChunkBytes()
	if n != 1024*1024 {
		t.Fatalf("default ceiling %d", n)
	}
	if MaxChunkBase64Chars() != (n+2)/3*4 || MaxChunkBase64Chars() != 1398104 {
		t.Fatalf("base64 chars %d", MaxChunkBase64Chars())
	}
	if c := ChunkCeiling(); c.Warning != "" {
		t.Fatalf("unexpected warning %q", c.Warning)
	}
}

func TestOperatorCanRaiseTheCeilingTo8MiBAndNoFurther(t *testing.T) {
	t.Setenv("MAX_HTML_BYTES", "")
	if MinChunkCeilingBytes != 4096 || MaxChunkCeilingBytes != 8388608 {
		t.Fatal("clamp bounds changed")
	}
	// The default 32 MB request body carries an 8 MiB chunk as base64 plus its
	// JSON-RPC envelope; otherwise the top of the clamp could never be used.
	if TransportMaxChunkBytes() < MaxChunkCeilingBytes {
		t.Fatalf("transport max %d", TransportMaxChunkBytes())
	}
	tm := TransportMaxChunkBytes()
	for _, n := range []string{"4096", "49152", "262144", "262145", "1048576", "1048577", "8388608"} {
		if got := ClampChunkBytes(n, 49152, tm); got != atoi(n) {
			t.Errorf("%s → %d", n, got)
		}
	}
	for _, raw := range []string{"8388609", "16777216", "4095", "0", "-1", "", "big"} {
		if got := ClampChunkBytes(raw, 49152, tm); got != 49152 {
			t.Errorf("%q → %d, want the default", raw, got)
		}
	}
	// A ceiling the configured request body cannot carry would be advertised
	// and then refused with a 413 before any tool ran.
	cases := []struct {
		raw              string
		transport, bytes int
	}{
		{"1048576", 786432, 49152},
		{"786432", 786432, 786432},
		// The default is held to the same bound…
		{"", 36864, 36864},
		{"49152", 36864, 36864},
		{"big", 36864, 36864},
		// …down to the 4 KiB floor.
		{"", -12288, 4096},
	}
	for _, c := range cases {
		if got := ClampChunkBytes(c.raw, 49152, c.transport); got != c.bytes {
			t.Errorf("clamp(%q, %d) = %d, want %d", c.raw, c.transport, got, c.bytes)
		}
	}
}

func atoi(s string) int {
	n, _ := parseInt(s)
	return n
}

func TestUnhonouredChunkSettingSaysSo(t *testing.T) {
	t.Setenv("MAX_HTML_BYTES", "")
	tm := TransportMaxChunkBytes()
	silent := []struct {
		raw   string
		bytes int
	}{{"1048576", 1048576}, {"", 49152}, {"  ", 49152}, {" 262144 ", 262144}}
	for _, c := range silent {
		if got := ResolveChunkCeiling(c.raw, 49152, tm); got != (Ceiling{Bytes: c.bytes}) {
			t.Errorf("%q → %+v", c.raw, got)
		}
	}
	cases := []struct {
		raw       string
		transport int
		want      string
	}{
		{"16777216", tm, `PAGE_UPLOAD_MAX_CHUNK_BYTES="16777216" is not used because it is above the 8388608-byte maximum; staged-upload chunks are limited to 49152 bytes\.`},
		{"1024", tm, `PAGE_UPLOAD_MAX_CHUNK_BYTES="1024" is not used because it is below the 4096-byte minimum; staged-upload chunks are limited to 49152 bytes\.`},
		{"big", tm, `PAGE_UPLOAD_MAX_CHUNK_BYTES="big" is not used because it is not a whole number of bytes; staged-upload chunks are limited to 49152 bytes\.`},
		// parseInt would read these as 1 MiB and raise the ceiling silently.
		{"1048576oops", tm, `PAGE_UPLOAD_MAX_CHUNK_BYTES="1048576oops" is not used because it is not a whole number of bytes; staged-upload chunks are limited to 49152 bytes\.`},
		{"1048576.5", tm, `"1048576\.5" is not used because it is not a whole number of bytes`},
		{"1e6", tm, `"1e6" is not used because it is not a whole number of bytes`},
		{"-65536", tm, `"-65536" is not used because it is not a whole number of bytes`},
		{"99999999999999999999999", tm, `is not used because it is above the 8388608-byte maximum`},
		{"1048576", 774144, `PAGE_UPLOAD_MAX_CHUNK_BYTES="1048576" is not used because its base64 does not fit one \d+-byte request \(MAX_HTML_BYTES carries at most 774144 raw bytes per chunk\); staged-upload chunks are limited to 49152 bytes\.`},
		{"", 36864, `MAX_HTML_BYTES \(\d+ bytes per request\) cannot carry the default 49152-byte upload chunk as base64; staged-upload chunks are limited to 36864 bytes\.$`},
		{"", -12288, `limited to 4096 bytes\. Even that does not fit one request; raise MAX_HTML_BYTES to at least 21848 bytes\.$`},
	}
	for _, c := range cases {
		w := ResolveChunkCeiling(c.raw, 49152, c.transport).Warning
		if !strings.HasPrefix(w, "WARNING: ") || !regexp.MustCompile(c.want).MatchString(w) {
			t.Errorf("%q/%d: %q", c.raw, c.transport, w)
		}
	}
	if MinTransportRequestBytes != 21848 {
		t.Fatal(MinTransportRequestBytes)
	}
}

// The advertised chunk_base64 maxLength (MaxChunkBase64Chars) follows the
// configured ceiling and the request budget, and the startup warning says
// when the setting did not take effect.
func TestAdvertisedCeilingFollowsConfiguration(t *testing.T) {
	b64 := func(n int) int { return (n + 2) / 3 * 4 }
	cases := []struct {
		chunk, request string
		bytes          int
		warning        string
	}{
		{"8388608", "", 8388608, ""},
		{"8388609", "", 1048576, `PAGE_UPLOAD_MAX_CHUNK_BYTES="8388609" is not used because it is above`},
		{"", "", 1048576, ""},
		{"", "2mb", 1048576, ""},
		{"49152", "2mb", 49152, ""},
		{"8388608", "2mb", 1048576, `"8388608" is not used because its base64 does not fit one 2097152-byte request .*limited to 1048576 bytes`},
		{"1048576", "1mb", 774144, `"1048576" is not used because its base64 does not fit one 1048576-byte request .*limited to 774144 bytes`},
		{"524288", "1mb", 524288, ""},
		{"", "1mb", 774144, `cannot carry the default 1048576-byte upload chunk as base64; staged-upload chunks are limited to 774144 bytes`},
		{"", "64kb", 36864, `cannot carry the default 1048576-byte upload chunk as base64; staged-upload chunks are limited to 36864 bytes`},
	}
	for _, c := range cases {
		t.Setenv("PAGE_UPLOAD_MAX_CHUNK_BYTES", c.chunk)
		t.Setenv("MAX_HTML_BYTES", c.request)
		got := ChunkCeiling()
		if got.Bytes != c.bytes || MaxChunkBytes() != c.bytes || MaxChunkBase64Chars() != b64(c.bytes) {
			t.Errorf("%q/%q: %+v", c.chunk, c.request, got)
		}
		if (c.warning == "") != (got.Warning == "") || (c.warning != "" && !regexp.MustCompile(c.warning).MatchString(got.Warning)) {
			t.Errorf("%q/%q warning %q", c.chunk, c.request, got.Warning)
		}
	}
	// A full-size chunk plus its envelope fits the shrunken request.
	if b64(36864)+RPCEnvelopeReserveBytes > 64*1024 {
		t.Fatal("envelope reserve does not fit")
	}
}

func TestConsumerAndTicketSettings(t *testing.T) {
	for kind, want := range map[string]string{
		"page": "deploy_page_upload", "": "deploy_page_upload", "bogus": "deploy_page_upload",
		"template": "register_template_upload", "data": "update_page_data_upload",
	} {
		if got := ConsumerFor(kind); got != want {
			t.Errorf("ConsumerFor(%q) = %q", kind, got)
		}
	}
	for _, c := range []struct {
		raw  string
		want int
	}{{"", 60}, {"0", 60}, {"abc", 60}, {"20", 20}, {"\t7min", 7}, {"-5", -5}} {
		t.Setenv("PAGE_UPLOAD_TICKET_TTL_MINUTES", c.raw)
		if got := TicketTTLMinutes(); got != c.want {
			t.Errorf("ttl %q = %d", c.raw, got)
		}
	}
	t.Setenv("DASHBOARD_ORIGIN", "https://pages.northwind.test")
	if got := UploadURL("abc"); got != "https://pages.northwind.test/upload/abc" {
		t.Fatal(got)
	}
}

func TestTemplateNameNormalization(t *testing.T) {
	if got, err := normalizeTemplateName(" NWM-Campaign_Dashboard "); err != nil || got != "nwm-campaign_dashboard" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []string{"nwm/campaign", "nwm campaign", "-leading", "trailing-", "double--dash", "", strings.Repeat("x", 65), "İndex"} {
		if _, err := normalizeTemplateName(bad); codeOf(err) != "bad_template_name" {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := normalizeTemplateName(strings.Repeat("x", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestBearer(t *testing.T) {
	for header, want := range map[string]string{
		"Bearer pgu_abc": "pgu_abc", "bearer   pgu_abc  ": "pgu_abc", "Bearer ": "", "Bearer  ": "",
		"Bearerpgu": "", "Basic x": "", "": "",
	} {
		r, _ := newRequest("PUT", "/x", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		if got := bearer(r); got != want {
			t.Errorf("%q → %q", header, got)
		}
	}
}
