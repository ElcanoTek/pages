// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package uploads

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/jsstr"
)

// When the calling MODEL writes the base64 itself, every byte is emitted token
// by token, so the chunk ceiling is a direct multiplier on the cost and failure
// rate of a deploy. At the original 12 KiB a routine 65 KB dashboard took six
// perfect append calls; real conversations spent ~25k output tokens per
// attempt, hit turn timeouts mid-upload, and abandoned 13 of 14 uploads. 48 KiB
// (64 KiB of base64, ~16k tokens) is comfortably inside a modern model's
// single-argument budget and cuts that to two calls. That is why it stays the
// default.
//
// A client that reads a workspace_file reference host-side never puts those
// bytes in model output, but each append is still one model tool call — and
// every call re-sends the whole conversation. At 48 KiB a 990 KB data file is
// 21 appends. An operator whose clients all read files host-side can raise the
// ceiling (up to 1 MiB, which fits the default 2 MB request body as base64)
// and make that one append.
//
// This is a CEILING, not a required chunk size — sequences are
// size-independent, so a client that can only emit small arguments keeps
// working unchanged. Tunable for ops without a redeploy
// (PAGE_UPLOAD_MAX_CHUNK_BYTES).
const (
	// MinChunkCeilingBytes is the smallest chunk ceiling an operator may set.
	MinChunkCeilingBytes = 4 * 1024
	// MaxChunkCeilingBytes is the largest; it matches the
	// page_content_upload_chunks CHECK (migration 024).
	MaxChunkCeilingBytes = 1024 * 1024
	// DefaultChunkBytes is the ceiling when PAGE_UPLOAD_MAX_CHUNK_BYTES is unset.
	DefaultChunkBytes = 48 * 1024
	// RPCEnvelopeReserveBytes: a chunk travels as base64 inside one JSON-RPC
	// request, so a ceiling the request body cannot carry would be advertised
	// and then refused with a 413 before any tool runs. Reserve room for the
	// envelope (id, method, upload_id, sequence, a client's _meta).
	RPCEnvelopeReserveBytes = 16 * 1024
	// MinTransportRequestBytes is the smallest MAX_HTML_BYTES that can carry
	// even the minimum chunk.
	MinTransportRequestBytes = (MinChunkCeilingBytes+2)/3*4 + RPCEnvelopeReserveBytes

	// MaxUploadBytes is the staged raw-file budget (2 MiB).
	MaxUploadBytes = datalimits.MaxUploadBytes

	// ChunkingAdvisoryBytes: above this, a FRESH chunked upload is told about
	// the ticket path before it emits any base64. Deliberately low: the cheap
	// path is better at almost any size, and the advisory costs one sentence in
	// a response the caller is already reading.
	ChunkingAdvisoryBytes = 20000

	// MaxActiveUploads is the per-token cap on uncommitted uploads.
	MaxActiveUploads = 5
	// AttemptWindowHours is how long a caller's chunked-upload attempts at one
	// target stay counted. Long enough to span a session, short enough that a
	// page someone uploads by hand once a month never accumulates into a
	// permanent scolding.
	AttemptWindowHours = 6
	// AttemptStartAlarm / AttemptCancelAlarm: past either, the caller is
	// demonstrably in the loop that burns turns (ten starts and eight cancels
	// for one page is the observed case). Advisory only.
	AttemptStartAlarm  = 3
	AttemptCancelAlarm = 2
	// UploadTTLHours is how long an upload (and a commit receipt) lives after
	// its last change.
	UploadTTLHours = 24
	// StaleUploadMinutes: an upload with no append for this long, from a caller
	// that is at its cap, is abandoned rather than slow — appends within one
	// turn land seconds apart. Only consulted when the cap would otherwise
	// reject a start.
	StaleUploadMinutes = 60
	// DefaultTicketTTLMinutes is PAGE_UPLOAD_TICKET_TTL_MINUTES's default.
	DefaultTicketTTLMinutes = 15
)

// ChunkingHint is appended to every chunk/hash failure. Observed sessions hit
// upload_chunk_invalid and upload_hash_mismatch four and five times in a row,
// cancelled, and started the same 64 KB base64 upload again — one of them said
// out loud it was "fighting base64 corruption" and went hunting through
// binaries for an HTTP endpoint, which is exactly what create_upload_ticket
// hands back. A failure is the one moment the caller is definitely reading, so
// it is where the cheaper path belongs.
const ChunkingHint = "Do not hand-emit base64. If your client advertises workspace_file references on append_page_upload, " +
	"use a whole-file SHA-256 and exact byte ranges, as few appends as max_chunk_bytes allows; the client " +
	"encodes the file. Otherwise use a permitted programmatic transport (create_upload_ticket for direct file " +
	"HTTP). Keep accepted bytes and resume this upload."

// ResumeHint: the single most expensive response available to a sequence
// conflict is cancel-and-start-over, and it is the one every observed caller
// reached for — eight times in one conversation, re-emitting the whole
// document each time. Nothing in the error said the alternative existed, so it
// says it now: a failed append does not advance the sequence, and everything
// already accepted is still on the server.
const ResumeHint = "Do NOT cancel: a failed append changes nothing, and every chunk already accepted is still here. " +
	"Re-send from the expected sequence and continue."

// TransportMaxChunkBytes is the largest raw chunk whose base64 plus the
// JSON-RPC envelope fits one MAX_HTML_BYTES request. It may be negative when
// the request budget is tiny.
func TransportMaxChunkBytes() int {
	return floorDiv(datalimits.MaxRequestBytes()-RPCEnvelopeReserveBytes, 4) * 3
}

func floorDiv(a, b int) int {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// Ceiling is a resolved chunk ceiling. Warning is "" when nothing needs saying.
type Ceiling struct {
	Bytes   int
	Warning string
}

var digitsRE = regexp.MustCompile(`^[0-9]+$`)

// ResolveChunkCeiling keeps an operator override inside sane bounds: too
// small multiplies round trips, too large cannot fit one request body (and,
// for a client whose model emits the base64, risks provider-side argument
// truncation mid-document). A value outside them falls back to the default,
// and the default is held to the same transport bound, so the advertised
// ceiling is always one a request can carry — down to the 4 KiB floor, below
// which no MAX_HTML_BYTES is sensible.
//
// The value is SAID when it is not what took effect (Warning): raising it is a
// production env change, and a silent fallback looks exactly like a rollout
// that never happened. raw is the setting as read ("" when unset).
func ResolveChunkCeiling(raw string, fallback, transportMax int) Ceiling {
	usable := max(MinChunkCeilingBytes, min(fallback, transportMax))
	cramped := ""
	if transportMax < MinChunkCeilingBytes {
		cramped = fmt.Sprintf(" Even that does not fit one request; raise MAX_HTML_BYTES to at least %d bytes.", MinTransportRequestBytes)
	}
	configured := jsstr.Trim(raw)
	if configured == "" {
		if usable == fallback {
			return Ceiling{Bytes: usable}
		}
		return Ceiling{Bytes: usable, Warning: fmt.Sprintf(
			"WARNING: MAX_HTML_BYTES (%d bytes per request) cannot carry the default %d-byte "+
				"upload chunk as base64; staged-upload chunks are limited to %d bytes.%s",
			datalimits.MaxRequestBytes(), fallback, usable, cramped)}
	}
	// The whole value must be decimal digits: parseInt would read
	// "1048576oops" or "1048576.5" as 1 MiB and raise the ceiling without the
	// warning.
	reason := ""
	n, err := 0, error(nil)
	if digitsRE.MatchString(configured) {
		n, err = strconv.Atoi(configured)
	}
	switch {
	case !digitsRE.MatchString(configured) || err != nil:
		if err != nil {
			// More digits than an int holds: above the maximum, as Number() reads it.
			reason = fmt.Sprintf("it is above the %d-byte maximum", MaxChunkCeilingBytes)
		} else {
			reason = "it is not a whole number of bytes"
		}
	case n < MinChunkCeilingBytes:
		reason = fmt.Sprintf("it is below the %d-byte minimum", MinChunkCeilingBytes)
	case n > MaxChunkCeilingBytes:
		reason = fmt.Sprintf("it is above the %d-byte maximum", MaxChunkCeilingBytes)
	case n > transportMax:
		reason = fmt.Sprintf("its base64 does not fit one %d-byte request (MAX_HTML_BYTES carries at most "+
			"%d raw bytes per chunk)", datalimits.MaxRequestBytes(), max(transportMax, 0))
	}
	if reason == "" {
		return Ceiling{Bytes: n}
	}
	return Ceiling{Bytes: usable, Warning: fmt.Sprintf(
		"WARNING: PAGE_UPLOAD_MAX_CHUNK_BYTES=\"%s\" is not used because %s; "+
			"staged-upload chunks are limited to %d bytes.%s", configured, reason, usable, cramped)}
}

// ClampChunkBytes is ResolveChunkCeiling without the warning.
func ClampChunkBytes(raw string, fallback, transportMax int) int {
	return ResolveChunkCeiling(raw, fallback, transportMax).Bytes
}

// ChunkCeiling resolves PAGE_UPLOAD_MAX_CHUNK_BYTES against the current
// MAX_HTML_BYTES. The server should call it once at startup and log a
// non-empty Warning (the Node server printed it with console.warn); every
// other read is the same pure computation, so tests can t.Setenv.
func ChunkCeiling() Ceiling {
	return ResolveChunkCeiling(os.Getenv("PAGE_UPLOAD_MAX_CHUNK_BYTES"), DefaultChunkBytes, TransportMaxChunkBytes())
}

// MaxChunkBytes is the resolved raw chunk ceiling (max_chunk_bytes).
func MaxChunkBytes() int { return ChunkCeiling().Bytes }

// MaxChunkBase64Chars is the base64 length of a full chunk: the advertised
// chunk_base64 maxLength.
func MaxChunkBase64Chars() int { return (MaxChunkBytes() + 2) / 3 * 4 }

// TicketTTLMinutes is PAGE_UPLOAD_TICKET_TTL_MINUTES read as JS parseInt (a
// leading integer; 0 or unreadable ⇒ 15). An upload ticket is minted and
// spent inside one agent turn; keep the window tight: it is a bearer string
// that necessarily passes through a model context.
func TicketTTLMinutes() int {
	n, ok := parseInt(os.Getenv("PAGE_UPLOAD_TICKET_TTL_MINUTES"))
	if !ok || n == 0 {
		return DefaultTicketTTLMinutes
	}
	return n
}

// parseInt is Number.parseInt(s, 10) for values that fit an int.
func parseInt(s string) (int, bool) {
	s = strings.TrimLeftFunc(s, jsstr.IsSpace)
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0, false
	}
	if neg {
		n = -n
	}
	return n, true
}

// SHA256 is the lowercase hex SHA-256 of b.
func SHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var (
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	base64RE = regexp.MustCompile(`^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$`)
)

// DecodeBase64Chunk decodes one canonical base64 chunk of 1..MaxChunkBytes
// bytes, or fails 400 upload_chunk_invalid with the chunking hint.
func DecodeBase64Chunk(value string) ([]byte, error) {
	maxBytes := MaxChunkBytes()
	if value == "" || len(value) > (maxBytes+2)/3*4 || len(value)%4 != 0 || !base64RE.MatchString(value) {
		return nil, apierror.BadRequest(
			"chunk_base64 must be canonical base64 for one non-empty chunk. "+ChunkingHint,
			"upload_chunk_invalid", nil)
	}
	b, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(b) == 0 || len(b) > maxBytes || base64.StdEncoding.EncodeToString(b) != value {
		return nil, apierror.BadRequest(
			fmt.Sprintf("decoded chunk must be 1-%d bytes. %s", maxBytes, ChunkingHint),
			"upload_chunk_invalid", nil)
	}
	return b, nil
}

// consumerTool is the tool that consumes a verified upload, by what it was
// staged for. A staged upload is useless without the name of the one call
// that can spend it, and the three targets do not share one.
var consumerTool = map[string]string{
	KindPage:     "deploy_page_upload",
	KindTemplate: "register_template_upload",
	KindData:     "update_page_data_upload",
}

// ConsumerFor names the MCP tool that consumes an upload of this target kind
// ("" and unknown kinds read as page).
func ConsumerFor(kind string) string {
	if c, ok := consumerTool[kind]; ok {
		return c
	}
	return consumerTool[KindPage]
}
