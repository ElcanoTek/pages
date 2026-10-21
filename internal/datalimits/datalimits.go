// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package datalimits holds the managed-data size budgets and the one place
// they are measured. Several budgets apply at once and they count different
// things: the compact payload, the stored HTML-escaped envelope, the inline
// transport argument, a staged raw file, and the complete HTTP request. A
// refusal names the budget it hit and whether changing transport could help,
// because "split the payload to fit" is exactly the wrong advice for a domain
// limit.
//
// Every value is read from the environment at call time (see internal/env), so
// tests can t.Setenv. The server should call Load once at startup so an
// unreadable MAX_HTML_BYTES stops it with the setting named.
package datalimits

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/jsonv"
)

// MaxUploadBytes is the staged raw-file budget.
const MaxUploadBytes = 2 * 1024 * 1024

const (
	defaultMaxDataBytes       = 1024 * 1024
	defaultMaxInlineDataBytes = 1500000
	defaultMaxRequest         = "2mb"
	ordinaryTimestamp         = "2000-01-01T00:00:00.000Z"
)

// positiveBytes mirrors the JS Number(value) + isSafeInteger check: the value
// must be a positive safe integer (blanks around it allowed, as Number allows);
// anything else is the default.
func positiveBytes(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsInf(f, 0) || f != math.Trunc(f) || f <= 0 || f > 9007199254740991 {
		return fallback
	}
	// Number() accepts forms ParseFloat does not (hex) and vice versa
	// ("1_000" is rejected by both). Hex settings are not a real use; the
	// common forms agree.
	return int(f)
}

// MaxDataBytes is PAGES_DATA_MAX_BYTES (default 1 MiB): the compact payload
// and the stored envelope budget.
func MaxDataBytes() int { return positiveBytes("PAGES_DATA_MAX_BYTES", defaultMaxDataBytes) }

// MaxInlineDataBytes is PAGES_MCP_MAX_INLINE_DATA_BYTES (default 1500000).
func MaxInlineDataBytes() int {
	return positiveBytes("PAGES_MCP_MAX_INLINE_DATA_BYTES", defaultMaxInlineDataBytes)
}

// RequestBytes resolves MAX_HTML_BYTES (default "2mb"), the complete JSON HTTP
// request budget, and reports an unreadable value. Pass the same value to the
// HTTP body limit.
func RequestBytes() (int, error) {
	v := os.Getenv("MAX_HTML_BYTES")
	if v == "" {
		v = defaultMaxRequest
	}
	return ParseByteSize(v, "MAX_HTML_BYTES")
}

// MaxRequestBytes is RequestBytes with an unreadable value replaced by the
// 2 MiB default. Startup validates the setting through Load first, so this
// fallback is never reached on a running server.
func MaxRequestBytes() int {
	n, err := RequestBytes()
	if err != nil {
		return 2 * 1024 * 1024
	}
	return n
}

// Limits is the resolved budget set, serialized as the JS DATA_LIMITS object.
type Limits struct {
	MaxPayloadBytes          int `json:"max_payload_bytes"`
	MaxEnvelopeBytes         int `json:"max_envelope_bytes"`
	MaxInlineBytes           int `json:"max_inline_bytes"`
	MaxStagedBytes           int `json:"max_staged_bytes"`
	MaxRequestBytes          int `json:"max_request_bytes"`
	MinEnvelopeOverheadBytes int `json:"min_envelope_overhead_bytes"`
}

// Load resolves every budget now, failing on an unreadable MAX_HTML_BYTES.
func Load() (Limits, error) {
	req, err := RequestBytes()
	if err != nil {
		return Limits{}, err
	}
	return limitsWith(req), nil
}

// Current is Load with an unreadable request budget replaced by its default.
func Current() Limits { return limitsWith(MaxRequestBytes()) }

func limitsWith(req int) Limits {
	data := MaxDataBytes()
	return Limits{
		MaxPayloadBytes:          data,
		MaxEnvelopeBytes:         data,
		MaxInlineBytes:           MaxInlineDataBytes(),
		MaxStagedBytes:           MaxUploadBytes,
		MaxRequestBytes:          req,
		MinEnvelopeOverheadBytes: MinEnvelopeOverheadBytes,
	}
}

// byteSizeRe: a decimal number with an optional unit b, kb, mb or gb
// (1024-based, case-insensitive), blanks allowed around the number and before
// the unit. JS \s also matches Unicode spaces; settings are ASCII in practice.
var byteSizeRe = regexp.MustCompile(`^[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*(\d+(?:\.\d+)?)[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*((?i:b|kb|mb|gb))?[\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]*$`)

var byteUnits = map[string]float64{"b": 1, "kb": 1024, "mb": 1024 * 1024, "gb": 1024 * 1024 * 1024}

// ParseByteSize reads a size setting such as "2mb": a decimal number with an
// optional unit b, kb, mb or gb (1024-based, case-insensitive). The result is
// floored to whole bytes and must be positive. Anything else is an error
// naming the setting: the previous parser (the `bytes` package) read "2MB " or
// "1e6" as 2 or 1 byte and silently replaced garbage with the default, so a
// typo changed the limit without a word.
func ParseByteSize(value, name string) (int, error) {
	if name == "" {
		name = "byte size"
	}
	size := math.NaN()
	if m := byteSizeRe.FindStringSubmatch(value); m != nil {
		n, err := strconv.ParseFloat(m[1], 64)
		unit := strings.ToLower(m[2])
		if unit == "" {
			unit = "b"
		}
		if err == nil {
			size = math.Floor(n * byteUnits[unit])
		}
	}
	if math.IsNaN(size) || math.IsInf(size, 0) || size <= 0 || size > 9007199254740991 {
		return 0, fmt.Errorf("%s=%s is not a positive byte size; use a number of bytes or a value such as 512kb, 2mb or 1gb",
			name, jsonv.Quote(value))
	}
	return int(size), nil
}

// EscapedJSON is JSON.stringify(value) made safe inside a <script> element:
// <, >, & and U+2028/2029 become six-character Unicode escapes. It is the
// stored form of every managed block.
func EscapedJSON(value any) string {
	return escape(jsonv.Stringify(value))
}

var escaper = strings.NewReplacer("<", `\u003c`, ">", `\u003e`, "&", `\u0026`, "\u2028", `\u2028`, "\u2029", `\u2029`)

func escape(s string) string { return escaper.Replace(s) }

// EnvelopeOverhead is the escaped envelope's bytes beyond its data payload for
// the given timestamps (the data object itself counted as its two braces).
func EnvelopeOverhead(sourceAsOf, refreshedAt string) int {
	env := jsonv.ObjectOf("contract_version", 1.0, "refreshed_at", refreshedAt, "source_as_of", sourceAsOf, "data", jsonv.NewObject())
	return len(EscapedJSON(env)) - 2
}

// MinEnvelopeOverheadBytes is the overhead with ordinary 24-character
// timestamps. New normalized timestamps are at least that long (extended ISO
// years are longer), so early staging checks use this minimum while actual
// writes count every byte.
var MinEnvelopeOverheadBytes = EnvelopeOverhead(ordinaryTimestamp, ordinaryTimestamp)

// DomainSizeDetails is the detail object of a domain-size refusal.
type DomainSizeDetails struct {
	SizeLimit                string `json:"size_limit"`
	Bytes                    int    `json:"bytes"`
	MaxBytes                 int    `json:"max_bytes"`
	ChangingTransportCanHelp bool   `json:"changing_transport_can_help"`
	Limits                   Limits `json:"limits"`
}

// AssertDomainSize refuses a payload or envelope ("boundary") larger than the
// managed-data budget. errorCode defaults to data_validation_failed.
func AssertDomainSize(size int, boundary, errorCode string) error {
	limit := MaxDataBytes()
	if size <= limit {
		return nil
	}
	if errorCode == "" {
		errorCode = "data_validation_failed"
	}
	return apierror.BadRequest(
		fmt.Sprintf("Managed data %s is %d bytes; its limit is %d bytes. ", boundary, size, limit)+
			"Changing upload transport cannot help. Keep the complete source file and correct the data contract "+
			"or ask the operator to review capacity. Do NOT split, sample or summarize the payload to fit.",
		errorCode,
		DomainSizeDetails{SizeLimit: boundary, Bytes: size, MaxBytes: limit, ChangingTransportCanHelp: false, Limits: Current()},
	)
}

// DataSize is the measured size of a payload, the shape clients send as the
// advisory data_size.
type DataSize struct {
	PayloadBytes        int `json:"payload_bytes"`
	EscapedPayloadBytes int `json:"escaped_payload_bytes"`
}

// MeasureData measures a payload: UTF-8 bytes of JSON.stringify(data), and of
// the same JSON after script-safe escaping.
func MeasureData(data any) DataSize {
	s := jsonv.Stringify(data)
	return DataSize{PayloadBytes: len(s), EscapedPayloadBytes: len(escape(s))}
}

// Usage is how close a payload is to the managed-data limit (issue #109), so a
// refresh can report its headroom instead of discovering the wall. The binding
// size is the larger of the compact payload and the stored envelope.
type Usage struct {
	PayloadBytes  int `json:"payload_bytes"`
	EnvelopeBytes int `json:"envelope_bytes"`
	LimitBytes    int `json:"limit_bytes"`
	HeadroomBytes int `json:"headroom_bytes"`
}

// UsageOf builds a Usage from already-measured sizes (envelopeBytes is the
// exact stored envelope, as materialization counts it).
func UsageOf(payloadBytes, envelopeBytes int) Usage {
	limit := MaxDataBytes()
	used := payloadBytes
	if envelopeBytes > used {
		used = envelopeBytes
	}
	return Usage{PayloadBytes: payloadBytes, EnvelopeBytes: envelopeBytes, LimitBytes: limit, HeadroomBytes: limit - used}
}

// MeasureUsage measures data and estimates its envelope with the minimum
// metadata overhead (actual writes count their exact timestamps).
func MeasureUsage(data any) Usage {
	m := MeasureData(data)
	return UsageOf(m.PayloadBytes, m.EscapedPayloadBytes+MinEnvelopeOverheadBytes)
}

func isSafeNonNegInt(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != math.Trunc(f) || math.IsInf(f, 0) || math.Abs(f) > 9007199254740991 {
		return 0, false
	}
	return int(f), true
}

// AssertStagedDataSize checks a client's advisory data_size before staging.
// provided is false when the field was absent (JS undefined); a present null
// is an error, as in the JS. size is a parsed JSON value (*jsonv.Object).
func AssertStagedDataSize(kind string, size any, provided bool) error {
	if !provided {
		return nil
	}
	if kind != "data" {
		return apierror.BadRequest("data_size is only valid for kind 'data'", "data_size_invalid", nil)
	}
	obj, ok := size.(*jsonv.Object)
	invalid := !ok || obj.Len() != 2
	var payload, escaped int
	if !invalid {
		var okP, okE bool
		payload, okP = isSafeNonNegInt(obj.Value("payload_bytes"))
		escaped, okE = isSafeNonNegInt(obj.Value("escaped_payload_bytes"))
		invalid = !okP || !okE || payload < 0 || escaped < payload
	}
	if invalid {
		return apierror.BadRequest("data_size requires nonnegative safe integer payload_bytes and escaped_payload_bytes >= payload_bytes", "data_size_invalid", nil)
	}
	if err := AssertDomainSize(payload, "payload", ""); err != nil {
		return err
	}
	return AssertDomainSize(escaped+MinEnvelopeOverheadBytes, "envelope", "")
}

// DataLimitGuidance is the limits paragraph tool descriptions and prompts
// carry, with the resolved numbers.
func DataLimitGuidance() string {
	data := MaxDataBytes()
	return fmt.Sprintf("Managed-data limits in UTF-8 bytes: compact payload %d; stored HTML-escaped ", data) +
		fmt.Sprintf("envelope %d including timestamps and at least %d metadata bytes; ", data, MinEnvelopeOverheadBytes) +
		fmt.Sprintf("inline payload transport %d; staged raw file %d; complete JSON HTTP request ", MaxInlineDataBytes(), MaxUploadBytes) +
		fmt.Sprintf("%d including RPC fields and expect. All applicable limits must fit. ", MaxRequestBytes()) +
		"Direct file staging removes the file from inline/JSON-RPC transport budgets, never the data/envelope limits. " +
		"Before staging kind='data', optionally supply advisory data_size: payload_bytes is UTF-8 byteLength(JSON.stringify(data)); " +
		"escaped_payload_bytes counts the same JSON after replacing <, >, &, U+2028 and U+2029 with six-character Unicode escapes. " +
		"Measure the parsed object, not pretty-printed file bytes; total_bytes and SHA-256 still describe the exact file. " +
		"The server validates received content independently; measurements do not guarantee acceptance. "
}

// FileUploadGuidance is the one transport contract for initialization, tool
// schemas and prepared prompts (lib/upload-guidance.js). kind is "page" or
// "data"; prefix is the tool-name prefix ("" or "mcp_pages_"). Uploading
// stages bytes only; the consumer still owns validation and publication.
func FileUploadGuidance(kind, prefix string) string {
	if kind == "" {
		kind = "page"
	}
	target, consumer, lead := "", "deploy_page_upload", ""
	if kind == "data" {
		target, consumer, lead = " kind='data'", "update_page_data_upload", DataLimitGuidance()
	}
	return lead + "Compute the exact file byte count and lowercase SHA-256. If the client's append_page_upload schema accepts a workspace file reference, prefer " +
		prefix + "start_page_upload" + target + " then ordered " + prefix + "append_page_upload calls with chunk_base64={workspace_file: relative_path, sha256: whole_file_sha256, offset: accepted_raw_bytes, length: next_raw_chunk_bytes}. The client reads and encodes exact bytes; do not print or transcribe base64. Use returned next_sequence and as few appends as returned max_chunk_bytes allows (make each length max_chunk_bytes until the last), then call " +
		prefix + consumer + ". Otherwise prefer " + prefix + "create_upload_ticket" + target + ": " +
		"PUT the file to the returned URL from your shell, then call " + prefix + consumer + " with upload_id. " +
		"If outbound HTTP is unavailable, use " + prefix + "start_page_upload" + target + " followed by ordered " +
		prefix + "append_page_upload base64 chunks at returned next_sequence, as few appends as returned max_chunk_bytes allows, then the same consumer. " +
		"Retry an interrupted append with the same sequence and bytes; do not cancel and restart unchanged content. " +
		"Verify the returned byte count/hash before consuming the completed upload."
}
