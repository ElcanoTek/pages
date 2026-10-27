// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package uploads is durable staged content uploads for MCP. Real dashboard
// files routinely exceed a model/provider's safe tool-argument size even
// though Pages accepts a multi-megabyte HTTP body. Small base64 chunks plus an explicit
// server-minted handle keep every call bounded; PostgreSQL makes the flow work
// across server instances.
//
// An upload stages exact bytes (declared up front by total_bytes and
// content_sha256) for exactly one target: a page's HTML, a page's managed-data
// payload, or a template. Bytes arrive either as ordered base64 appends
// (Start, Append) or in one out-of-band PUT authenticated by a one-shot,
// write-only, content-pinned ticket (CreateTicket, PutTicketContent, and the
// HTTP Handler). Deploy hands the verified bytes to a consumer callback inside
// the upload's own transaction and stores the callback's result as an
// immutable commit receipt, so an ambiguous commit is safely retryable.
//
// Upload handles are bound to the agent token that opened them; every
// operation other than the ticket PUT takes a versions.Actor whose TokenID is
// that token.
package uploads

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ElcanoTek/pages/internal/apierror"
	"github.com/ElcanoTek/pages/internal/datalimits"
	"github.com/ElcanoTek/pages/internal/db"
	"github.com/ElcanoTek/pages/internal/env"
	"github.com/ElcanoTek/pages/internal/jsonv"
	"github.com/ElcanoTek/pages/internal/jsstr"
	"github.com/ElcanoTek/pages/internal/jsx"
	"github.com/ElcanoTek/pages/internal/tokens"
	"github.com/ElcanoTek/pages/internal/versions"
)

// Target kinds.
const (
	KindPage     = "page"
	KindTemplate = "template"
	KindData     = "data"
)

// State is an upload's progress as every staging call reports it. Field order
// is the wire order.
type State struct {
	UploadID   string `json:"upload_id"`
	TargetKind string `json:"target_kind"`
	// The row's one target column reads as a slug or a template name
	// depending on target_kind; report the one that is true and null the
	// other, so a caller can never act on a template name as though it were a
	// page.
	Slug          *string            `json:"slug"`
	Template      *string            `json:"template"`
	ContentSHA256 string             `json:"content_sha256"`
	TotalBytes    int                `json:"total_bytes"`
	BytesReceived int                `json:"bytes_received"`
	NextSequence  int                `json:"next_sequence"`
	MaxChunkBytes int                `json:"max_chunk_bytes"`
	DataLimits    *datalimits.Limits `json:"data_limits,omitempty"` // kind 'data' only
	Complete      bool               `json:"complete"`
	ExpiresAt     jsx.Time           `json:"expires_at"`
	NextStep      string             `json:"next_step"`
	// Ticket and TicketExpiresAt are set by CreateTicket only.
	Ticket          string    `json:"ticket,omitempty"`
	TicketExpiresAt *jsx.Time `json:"ticket_expires_at,omitempty"`
	// Deduped is set by Append and PutTicketContent only.
	Deduped *bool `json:"deduped,omitempty"`
}

// CancelResult is Cancel's answer.
type CancelResult struct {
	UploadID  string `json:"upload_id"`
	Cancelled bool   `json:"cancelled"`
	NextStep  string `json:"next_step"`
}

// Peeked is a verified staged upload read without consuming it.
type Peeked struct {
	UploadID      string  `json:"upload_id"`
	TargetKind    string  `json:"target_kind"`
	Slug          *string `json:"slug"`
	Template      *string `json:"template"`
	TotalBytes    int     `json:"total_bytes"`
	ContentSHA256 string  `json:"content_sha256"`
	HTML          string  `json:"html"`
}

// StartInput opens an upload. Supply exactly one of Slug or Template (nil =
// omitted). Kind (nil = omitted) applies to a slug target: "page" (default) or
// "data". DataSize is the advisory data_size: nil = omitted, versions.Null =
// explicit null, else a parsed JSON value.
type StartInput struct {
	Slug          *string
	Template      *string
	Kind          *string
	TotalBytes    int
	ContentSHA256 string
	DataSize      any
}

// AppendInput is one ordered chunk.
type AppendInput struct {
	UploadID    string
	Sequence    int
	ChunkBase64 string
}

type row struct {
	ID              string
	Slug            string
	TargetKind      string
	TotalBytes      int
	ContentSHA256   string
	BytesReceived   int
	NextSequence    int
	ExpiresAt       jsx.Time
	CommitKey       *string
	CommitResult    *string
	CommittedAt     *time.Time
	TicketUsedAt    *time.Time
	TicketExpiresAt *time.Time
}

func (r *row) kind() string {
	if r.TargetKind == "" {
		return KindPage
	}
	return r.TargetKind
}

// targets returns (slug, template) under the one-column, two-readings rule.
func (r *row) targets() (*string, *string) {
	name := r.Slug
	if r.kind() == KindTemplate {
		return nil, &name
	}
	return &name, nil
}

func ceilDiv(a, b int) int { return int(math.Ceil(float64(a) / float64(b))) }

func state(r *row) *State {
	maxChunk := MaxChunkBytes()
	complete := r.BytesReceived == r.TotalBytes
	remaining := r.TotalBytes - r.BytesReceived
	// Say the exact count: "at most N bytes" alone read as permission to send
	// many small chunks, and each append is a full model round trip.
	appendsLeft := ceilDiv(remaining, maxChunk)
	kind := r.kind()
	slug, template := r.targets()
	s := &State{
		UploadID: r.ID, TargetKind: kind, Slug: slug, Template: template,
		ContentSHA256: r.ContentSHA256, TotalBytes: r.TotalBytes, BytesReceived: r.BytesReceived,
		NextSequence: r.NextSequence, MaxChunkBytes: maxChunk, Complete: complete, ExpiresAt: r.ExpiresAt,
	}
	if kind == KindData {
		limits := datalimits.Current()
		s.DataLimits = &limits
	}
	switch {
	case complete:
		what := "HTML"
		if kind == KindData {
			what = "payload"
		}
		s.NextStep = fmt.Sprintf("Upload verified. Call %s with this upload_id; do not resend the %s.", ConsumerFor(kind), what)
	case r.BytesReceived == 0 && r.TotalBytes > ChunkingAdvisoryBytes:
		// On a FRESH upload of any size, say the cheaper thing before a single
		// byte of base64 is emitted. One observed session shrank its chunks
		// from 16 KB to 2 KB chasing corruption, which turned a 4-round-trip
		// upload into a 32-round-trip one, and the turn ended at 16,000 of
		// 64,291 bytes — never deployed. This line is the last moment before
		// that starts.
		plural := "s"
		if appendsLeft == 1 {
			plural = ""
		}
		s.NextStep = fmt.Sprintf("This file is %d bytes. Continue this upload with append_page_upload "+
			"using workspace_file references if your client advertises them, or exact programmatic base64. "+
			"Use the whole-file SHA-256, raw offset %d and sequence %d, "+
			"in as few appends as max_chunk_bytes (%d) allows: %d "+
			"append%s for this file. Never print or generate base64 through model output. "+
			"Do not cancel a usable upload to change transport. For a future upload without this capability, "+
			"create_upload_ticket supports direct file HTTP when permitted.",
			r.TotalBytes, r.BytesReceived, r.NextSequence, maxChunk, appendsLeft, plural)
	default:
		s.NextStep = fmt.Sprintf("Call append_page_upload with sequence %d for the next %d raw bytes at offset %d "+
			"(as few appends as max_chunk_bytes %d allows: %d left). Use a workspace_file reference if advertised by your client, otherwise exact programmatic base64; do not generate it through model output.",
			r.NextSequence, min(remaining, maxChunk), r.BytesReceived, maxChunk, appendsLeft)
	}
	return s
}

func boolPtr(b bool) *bool { return &b }

const stateColumns = `id::text, slug, target_kind, total_bytes, content_sha256, bytes_received, next_sequence, expires_at`

func scanState(rw pgx.Row, extra ...any) (*row, error) {
	r := &row{}
	dest := append([]any{&r.ID, &r.Slug, &r.TargetKind, &r.TotalBytes, &r.ContentSHA256,
		&r.BytesReceived, &r.NextSequence, &r.ExpiresAt}, extra...)
	if err := rw.Scan(dest...); err != nil {
		return nil, err
	}
	return r, nil
}

func actorTokenID(a versions.Actor) (jsx.ID, error) {
	if a.TokenID <= 0 {
		return 0, errors.New("authenticated MCP token id is missing")
	}
	return a.TokenID, nil
}

func cleanupExpired(ctx context.Context, q db.Querier) error {
	_, err := q.Exec(ctx, "DELETE FROM page_content_uploads WHERE expires_at <= now()")
	return err
}

func lockUpload(ctx context.Context, q db.Querier, uploadID string, a versions.Actor) (*row, error) {
	tokenID, err := actorTokenID(a)
	if err != nil {
		return nil, err
	}
	if err := cleanupExpired(ctx, q); err != nil {
		return nil, err
	}
	r := &row{}
	err = q.QueryRow(ctx,
		`SELECT id::text, slug, target_kind, total_bytes, content_sha256, bytes_received,
		        next_sequence, commit_key, commit_result::text, committed_at, expires_at
		   FROM page_content_uploads
		  WHERE id = $1 AND token_id = $2
		  FOR UPDATE`, uploadID, tokenID).
		Scan(&r.ID, &r.Slug, &r.TargetKind, &r.TotalBytes, &r.ContentSHA256, &r.BytesReceived,
			&r.NextSequence, &r.CommitKey, &r.CommitResult, &r.CommittedAt, &r.ExpiresAt)
	if db.IsNoRows(err) {
		return nil, apierror.NotFound("page upload not found, expired, or owned by another token", "page_upload_not_found", nil)
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

func assertDeclaredContent(totalBytes int, contentSHA256 string) error {
	if totalBytes < 1 || totalBytes > MaxUploadBytes() {
		return apierror.BadRequest(fmt.Sprintf("total_bytes must be 1-%d", MaxUploadBytes()), "upload_size_invalid", nil)
	}
	if !sha256RE.MatchString(contentSHA256) {
		return apierror.BadRequest("content_sha256 must be a lowercase SHA-256 hex digest", "upload_hash_invalid", nil)
	}
	return nil
}

type attemptCounts struct{ starts, cancels int }

// recordAttempt / attemptAlarm — the memory that makes a loop visible from
// inside it. Cancelling deletes the upload row, so without this every attempt
// looks like the first one; the conversation that spent 10M tokens started ten
// uploads for one page and cancelled eight, and nothing anywhere said so.
func recordAttempt(ctx context.Context, q db.Querier, tokenID jsx.ID, t target, field string) (attemptCounts, error) {
	var c attemptCounts
	if _, err := q.Exec(ctx,
		`DELETE FROM page_upload_attempts WHERE updated_at < now() - ($1 * interval '1 hour')`,
		AttemptWindowHours); err != nil {
		return c, err
	}
	// field is one of two constants, never caller input.
	sql := `INSERT INTO page_upload_attempts (token_id, target_kind, slug, ` + field + `)
	     VALUES ($1, $2, $3, 1)
	     ON CONFLICT (token_id, target_kind, slug) DO UPDATE
	        SET ` + field + ` = page_upload_attempts.` + field + ` + 1, updated_at = now()
	     RETURNING starts, cancels`
	err := q.QueryRow(ctx, sql, tokenID, t.kind, t.name).Scan(&c.starts, &c.cancels)
	return c, err
}

// attemptAlarm is the advisory itself. Not a refusal: an environment that
// genuinely cannot make outbound HTTP requests has to keep using this path,
// and blocking it there turns a bad turn into an impossible one.
func attemptAlarm(c attemptCounts) string {
	if c.starts < AttemptStartAlarm && c.cancels < AttemptCancelAlarm {
		return ""
	}
	plural := "s"
	if c.starts == 1 {
		plural = ""
	}
	return fmt.Sprintf("You have started %d chunked upload%s for this target and cancelled ", c.starts, plural) +
		fmt.Sprintf("%d. That loop does not converge — each restart re-emits the whole document, and the turn ends ", c.cancels) +
		"before the file lands. Resume a usable upload with file-reference appends or another permitted " +
		"programmatic transport. create_upload_ticket is available for direct file HTTP, but do not restart " +
		"an existing valid transfer or hand-emit base64."
}

// reserveUploadSlot is the per-token active-upload cap, plus the stale reaper
// that keeps it from becoming a dead end. Shared by both ways of opening an
// upload.
func reserveUploadSlot(ctx context.Context, q db.Querier, tokenID jsx.ID) error {
	// Serialize the count+insert guard per token; without this, concurrent
	// starts could both observe four active uploads and exceed the cap.
	if _, err := q.Exec(ctx, "SELECT pg_advisory_xact_lock($1::bigint)", tokenID); err != nil {
		return err
	}
	if err := cleanupExpired(ctx, q); err != nil {
		return err
	}
	countActive := func() (int, error) {
		var n int
		err := q.QueryRow(ctx,
			`SELECT count(*)::integer FROM page_content_uploads
			  WHERE token_id = $1 AND committed_at IS NULL AND expires_at > now()`, tokenID).Scan(&n)
		return n, err
	}
	n, err := countActive()
	if err != nil || n < MaxActiveUploads {
		return err
	}
	// Uploads are often abandoned rather than finished: a turn times out or
	// hits a token ceiling mid-append and nothing ever calls cancel. With a
	// 24h TTL those would hold the cap for a day, and the caller's only way
	// forward would be to find each upload_id and cancel it by hand.
	//
	// Reap only when the cap is actually in the way, and only uploads that
	// have been silent for StaleUploadMinutes, so an in-flight upload that
	// simply paused between appends is never destroyed underneath its caller.
	if _, err := q.Exec(ctx,
		`DELETE FROM page_content_uploads
		  WHERE token_id = $1
		    AND committed_at IS NULL
		    AND updated_at < now() - ($2 * interval '1 minute')`, tokenID, StaleUploadMinutes); err != nil {
		return err
	}
	if n, err = countActive(); err != nil {
		return err
	}
	if n >= MaxActiveUploads {
		return apierror.Conflict(
			fmt.Sprintf("this token already has %d uploads in flight and none are stale. ", MaxActiveUploads)+
				"Call cancel_page_upload on the ones you are no longer sending, or wait for them to go idle.",
			"page_upload_limit", nil)
	}
	return nil
}

type target struct{ kind, name string }

// normalizeTarget — an upload stages bytes for exactly one destination: a page
// slug or a template name, never both and never neither. The kind is recorded
// on the row so the commit step can refuse a mismatched consumer, which is
// what stops a template skeleton (whose data block is deliberately empty) from
// being published as a live client page.
//
// A slug can be staged for two different things — the page's HTML, or its
// managed-data payload — so kind disambiguates when a slug is given. It is
// meaningless with a template, which names its own kind, and passing it there
// is refused rather than ignored: silently accepting kind 'data' alongside a
// template would stage bytes that no tool can ever consume.
func normalizeTarget(slug, template, kind *string) (target, error) {
	if (slug != nil) == (template != nil) {
		return target{}, apierror.BadRequest("supply exactly one of slug or template", "upload_target_required", nil)
	}
	if template != nil {
		if kind != nil && *kind != KindTemplate {
			return target{}, apierror.BadRequest("kind applies to a slug target; a template upload is always kind template", "upload_target_kind_invalid", nil)
		}
		name, err := normalizeTemplateName(*template)
		if err != nil {
			return target{}, err
		}
		return target{KindTemplate, name}, nil
	}
	slugKind := KindPage
	if kind != nil {
		slugKind = *kind
	}
	if slugKind != KindPage && slugKind != KindData {
		return target{}, apierror.BadRequest("kind must be one of: page, data", "upload_target_kind_invalid", nil)
	}
	name, err := versions.NormalizeSlug(*slug)
	if err != nil {
		return target{}, err
	}
	return target{slugKind, name}, nil
}

// Template names (lib/templates.js NAME_RE / MAX_NAME_CHARS). Kept here so this
// package does not depend on internal/templates, which consumes uploads.
const maxTemplateNameChars = 64

var templateNameRE = regexp.MustCompile(`^[a-z0-9]+(?:[-_][a-z0-9]+)*$`)

func normalizeTemplateName(name string) (string, error) {
	n := jsstr.Trim(name)
	// JavaScript lowercases U+0130 to "i̇" (two code points), which the
	// pattern then rejects; Go's simple case mapping would make it a plain "i".
	valid := !strings.ContainsRune(n, 'İ')
	n = strings.Map(unicode.ToLower, n)
	if !valid || !templateNameRE.MatchString(n) || len(n) > maxTemplateNameChars {
		return "", apierror.BadRequest(
			fmt.Sprintf("template must be url-safe with no slashes (a-z 0-9 - _), at most %d characters", maxTemplateNameChars),
			"bad_template_name", nil)
	}
	return n, nil
}

func (in StartInput) prepare(a versions.Actor) (target, jsx.ID, error) {
	t, err := normalizeTarget(in.Slug, in.Template, in.Kind)
	if err != nil {
		return t, 0, err
	}
	if err := assertDeclaredContent(in.TotalBytes, in.ContentSHA256); err != nil {
		return t, 0, err
	}
	size, provided := in.DataSize, in.DataSize != nil
	if size == versions.Null {
		size = nil
	}
	if err := datalimits.AssertStagedDataSize(t.kind, size, provided); err != nil {
		return t, 0, err
	}
	tokenID, err := actorTokenID(a)
	return t, tokenID, err
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Start opens a chunked upload.
func Start(ctx context.Context, d *db.DB, in StartInput, a versions.Actor) (*State, error) {
	t, tokenID, err := in.prepare(a)
	if err != nil {
		return nil, err
	}
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*State, error) {
		if err := reserveUploadSlot(ctx, tx, tokenID); err != nil {
			return nil, err
		}
		counts, err := recordAttempt(ctx, tx, tokenID, t, "starts")
		if err != nil {
			return nil, err
		}
		r, err := scanState(tx.QueryRow(ctx,
			`INSERT INTO page_content_uploads
			   (id, token_id, slug, target_kind, total_bytes, content_sha256, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, now() + ($7 * interval '1 hour'))
			 RETURNING `+stateColumns,
			newUUID(), tokenID, t.name, t.kind, in.TotalBytes, in.ContentSHA256, UploadTTLHours))
		if err != nil {
			return nil, err
		}
		opened := state(r)
		// Front of next_step, not appended: by the time a caller is on its
		// fourth start it is skimming, and the sentence that matters has to be
		// first.
		if alarm := attemptAlarm(counts); alarm != "" {
			opened.NextStep = alarm + " " + opened.NextStep
		}
		return opened, nil
	})
}

// ── Out-of-band content upload ───────────────────────────────────────────────
// CreateTicket opens exactly the same staged upload as Start, then attaches a
// one-shot write-only credential the CALLER'S SANDBOX can use to send the file
// directly. The model handles a URL and an opaque handle; the bytes never enter
// its context. See migrations/014 for the threat model.

// CreateTicket opens an upload with a ticket for one direct PUT.
func CreateTicket(ctx context.Context, d *db.DB, in StartInput, a versions.Actor) (*State, error) {
	t, tokenID, err := in.prepare(a)
	if err != nil {
		return nil, err
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	ticket := "pgu_" + base64.RawURLEncoding.EncodeToString(raw[:])
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*State, error) {
		if err := reserveUploadSlot(ctx, tx, tokenID); err != nil {
			return nil, err
		}
		// Deliberately not counted as an attempt: this IS the path the
		// advisory steers to, and a caller who took it must not be scolded for
		// arriving.
		var ticketExpires jsx.Time
		r, err := scanState(tx.QueryRow(ctx,
			`INSERT INTO page_content_uploads
			   (id, token_id, slug, target_kind, total_bytes, content_sha256, expires_at,
			    ticket_hash, ticket_expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, now() + ($7 * interval '1 hour'),
			         $8, now() + ($9 * interval '1 minute'))
			 RETURNING `+stateColumns+`, ticket_expires_at`,
			newUUID(), tokenID, t.name, t.kind, in.TotalBytes, in.ContentSHA256, UploadTTLHours,
			tokens.HashToken(ticket), TicketTTLMinutes()), &ticketExpires)
		if err != nil {
			return nil, err
		}
		s := state(r)
		s.Ticket = ticket
		s.TicketExpiresAt = &ticketExpires
		return s, nil
	})
}

func ticketMissing() error {
	return apierror.Unauthorized("an upload ticket is required", "upload_ticket_missing", nil)
}

func ticketInvalid() error {
	return apierror.Unauthorized("unknown or expired upload ticket", "upload_ticket_invalid", nil)
}

func committedConflict() error {
	return apierror.Conflict("this upload has already been deployed", "page_upload_committed", nil)
}

func ticketExpired() error {
	return apierror.Unauthorized("this upload ticket has expired; create a new one", "upload_ticket_expired", nil)
}

// CheckTicket is a cheap pre-flight on the TICKET ALONE, so a bad one is
// rejected before the server buffers a whole staged body (the same
// auth-before-parse ordering the MCP boundary uses). Advisory:
// PutTicketContent re-checks everything under a row lock, so a race here can
// only cost a wasted read, never a bad write. It returns the declared size.
func CheckTicket(ctx context.Context, q db.Querier, rawTicket, uploadID string) (int, error) {
	if rawTicket == "" {
		return 0, ticketMissing()
	}
	if err := db.CheckWriteAdmission(db.WithDrainBypass(ctx)); err != nil {
		return 0, err
	}
	var (
		id            string
		total         int
		ticketExpires *time.Time
		committedAt   *time.Time
	)
	err := q.QueryRow(ctx,
		`SELECT id::text, total_bytes, ticket_expires_at, committed_at
		   FROM page_content_uploads
		  WHERE ticket_hash = $1 AND expires_at > now()`, tokens.HashToken(rawTicket)).
		Scan(&id, &total, &ticketExpires, &committedAt)
	if db.IsNoRows(err) || (err == nil && id != uploadID) {
		return 0, ticketInvalid()
	}
	if err != nil {
		return 0, err
	}
	if committedAt != nil {
		return 0, committedConflict()
	}
	if ticketExpires == nil || !ticketExpires.After(time.Now()) {
		return 0, ticketExpired()
	}
	return total, nil
}

// PutTicketContent accepts the whole document in one request, authenticated
// by the ticket alone. content has already been length-capped by the caller.
func PutTicketContent(ctx context.Context, d *db.DB, rawTicket, uploadID string, content []byte) (*State, error) {
	if rawTicket == "" {
		return nil, ticketMissing()
	}
	if len(content) == 0 {
		return nil, apierror.BadRequest("request body must be the raw page bytes", "upload_body_empty", nil)
	}
	ticketHash := tokens.HashToken(rawTicket)
	ctx = db.WithDrainBypass(ctx)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*State, error) {
		if err := cleanupExpired(ctx, tx); err != nil {
			return nil, err
		}
		r := &row{}
		err := tx.QueryRow(ctx,
			`SELECT `+stateColumns+`, ticket_used_at, ticket_expires_at, committed_at
			   FROM page_content_uploads
			  WHERE ticket_hash = $1
			  FOR UPDATE`, ticketHash).
			Scan(&r.ID, &r.Slug, &r.TargetKind, &r.TotalBytes, &r.ContentSHA256, &r.BytesReceived,
				&r.NextSequence, &r.ExpiresAt, &r.TicketUsedAt, &r.TicketExpiresAt, &r.CommittedAt)
		// One non-revealing answer for a wrong ticket, a ticket for another
		// upload, and an upload that no longer exists.
		if db.IsNoRows(err) || (err == nil && r.ID != uploadID) {
			return nil, ticketInvalid()
		}
		if err != nil {
			return nil, err
		}
		if r.CommittedAt != nil {
			return nil, committedConflict()
		}
		if r.TicketExpiresAt == nil || !r.TicketExpiresAt.After(time.Now()) {
			return nil, ticketExpired()
		}
		// Re-sending the identical document is a safe no-op: the content is
		// pinned by content_sha256, so a retried curl cannot mean anything
		// else.
		if r.TicketUsedAt != nil {
			if r.BytesReceived == r.TotalBytes && SHA256(content) == r.ContentSHA256 {
				s := state(r)
				s.Deduped = boolPtr(true)
				return s, nil
			}
			return nil, apierror.Conflict("this upload ticket was already used; create a new one", "upload_ticket_used", nil)
		}
		if len(content) != r.TotalBytes {
			return nil, apierror.BadRequest(
				fmt.Sprintf("expected exactly %d bytes, received %d", r.TotalBytes, len(content)),
				"upload_size_mismatch", nil)
		}
		if SHA256(content) != r.ContentSHA256 {
			// No ChunkingHint here: this IS the ticket path, so "use
			// create_upload_ticket instead" is advice the caller already took.
			// The useful thing to say is that the ticket is content-pinned and
			// still usable, and that the bytes changed between hashing and
			// sending.
			return nil, apierror.Conflict(
				"uploaded bytes do not match content_sha256. The ticket is pinned to the hash you declared, so "+
					"the file changed between hashing and sending, or the transfer altered it — re-hash the exact file "+
					"you are sending (`sha256sum <file>`) and PUT it again with --data-binary; the ticket is still valid.",
				"upload_hash_mismatch", nil)
		}
		// Reuse the chunk store so Deploy reassembles and re-verifies exactly
		// as it does for an appended upload — one commit path, not two. The
		// split is a storage detail the caller never sees.
		maxChunk := MaxChunkBytes()
		sequence := 0
		for offset := 0; offset < len(content); offset += maxChunk {
			slice := content[offset:min(offset+maxChunk, len(content))]
			if _, err := tx.Exec(ctx,
				`INSERT INTO page_content_upload_chunks (upload_id, sequence, bytes, content_sha256)
				 VALUES ($1, $2, $3, $4)`, r.ID, sequence, slice, SHA256(slice)); err != nil {
				return nil, err
			}
			sequence++
		}
		updated, err := scanState(tx.QueryRow(ctx,
			`UPDATE page_content_uploads
			    SET bytes_received = $2, next_sequence = $3, ticket_used_at = now(),
			        expires_at = now() + ($4 * interval '1 hour'), updated_at = now()
			  WHERE id = $1
			  RETURNING `+stateColumns, r.ID, len(content), sequence, UploadTTLHours))
		if err != nil {
			return nil, err
		}
		s := state(updated)
		s.Deduped = boolPtr(false)
		return s, nil
	})
}

func resumableDetails(expected int, r *row) *jsonv.Object {
	return jsonv.ObjectOf(
		"expected_sequence", expected,
		"bytes_received", r.BytesReceived,
		"total_bytes", r.TotalBytes,
		"resumable", true)
}

// Append stores one ordered chunk. Replaying an accepted sequence with the
// same bytes is a no-op (Deduped); every failure leaves the upload unchanged.
func Append(ctx context.Context, d *db.DB, in AppendInput, a versions.Actor) (*State, error) {
	if in.Sequence < 0 {
		return nil, apierror.BadRequest("sequence must be a non-negative integer", "upload_sequence_invalid", nil)
	}
	chunk, err := DecodeBase64Chunk(in.ChunkBase64)
	if err != nil {
		return nil, err
	}
	chunkSHA := SHA256(chunk)
	ctx = db.WithDrainBypass(ctx)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*State, error) {
		up, err := lockUpload(ctx, tx, in.UploadID, a)
		if err != nil {
			return nil, err
		}
		if up.CommitResult != nil {
			return nil, committedConflict()
		}
		next := up.NextSequence
		if in.Sequence < next {
			var priorBytes []byte
			var priorSHA string
			err := tx.QueryRow(ctx,
				`SELECT bytes, content_sha256 FROM page_content_upload_chunks
				  WHERE upload_id = $1 AND sequence = $2`, up.ID, in.Sequence).Scan(&priorBytes, &priorSHA)
			if err != nil && !db.IsNoRows(err) {
				return nil, err
			}
			if err == nil && priorSHA == chunkSHA && bytes.Equal(priorBytes, chunk) {
				s := state(up)
				s.Deduped = boolPtr(true)
				return s, nil
			}
			return nil, apierror.Conflict(
				fmt.Sprintf("sequence %d was already accepted with DIFFERENT bytes; %d of ", in.Sequence, up.BytesReceived)+
					fmt.Sprintf("%d bytes are stored and the next chunk expected is %d. ", up.TotalBytes, next)+
					"You are re-sending a chunk you already sent, with a different encoding of it. "+ResumeHint,
				"upload_sequence_conflict", resumableDetails(next, up))
		}
		if in.Sequence > next {
			return nil, apierror.Conflict(
				fmt.Sprintf("expected upload sequence %d, received %d: %d of ", next, in.Sequence, up.BytesReceived)+
					fmt.Sprintf("%d bytes are stored. A FAILED append does not advance the sequence, so ", up.TotalBytes)+
					fmt.Sprintf("whatever went wrong at %d still has to be sent. ", next)+ResumeHint,
				"upload_sequence_conflict", resumableDetails(next, up))
		}

		received := up.BytesReceived + len(chunk)
		if received > up.TotalBytes {
			overflow := received - up.TotalBytes
			return nil, apierror.BadRequest(
				fmt.Sprintf("chunk exceeds the declared total_bytes by %d bytes ", overflow)+
					fmt.Sprintf("(declared %d, already stored %d). ", up.TotalBytes, up.BytesReceived)+
					"Do NOT trim the payload to fit: the declaration describes a DIFFERENT document from the one you "+
					"are sending, so a trimmed one would fail content_sha256 anyway — and if it did not, you would "+
					"publish a truncated page. Recompute total_bytes and content_sha256 from the exact bytes you "+
					"intend to send and start a new upload. "+ChunkingHint,
				"upload_size_mismatch",
				jsonv.ObjectOf(
					"declared_total_bytes", up.TotalBytes,
					"bytes_received", up.BytesReceived,
					"overflow_bytes", overflow))
		}

		// Validate the complete byte stream before accepting its last chunk.
		// A mismatch rolls this transaction back, preserving the same sequence
		// for a corrected retry (or the caller can start a fresh upload).
		if received == up.TotalBytes {
			complete, err := storedBytes(ctx, tx, up.ID, received)
			if err != nil {
				return nil, err
			}
			complete = append(complete, chunk...)
			if SHA256(complete) != up.ContentSHA256 {
				return nil, apierror.Conflict(
					fmt.Sprintf("complete upload does not match content_sha256. Sequence %d was NOT accepted, so the ", in.Sequence)+
						fmt.Sprintf("upload still holds %d of %d bytes and this exact ", up.BytesReceived, up.TotalBytes)+
						"sequence is still the one expected — re-send it if that chunk was wrong. Only if the earlier "+
						"chunks are wrong does this need a new upload. "+ChunkingHint,
					"upload_hash_mismatch", resumableDetails(in.Sequence, up))
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO page_content_upload_chunks (upload_id, sequence, bytes, content_sha256)
			 VALUES ($1, $2, $3, $4)`, up.ID, in.Sequence, chunk, chunkSHA); err != nil {
			return nil, err
		}
		updated, err := scanState(tx.QueryRow(ctx,
			`UPDATE page_content_uploads
			    SET bytes_received = $2, next_sequence = $3,
			        expires_at = now() + ($4 * interval '1 hour'), updated_at = now()
			  WHERE id = $1
			  RETURNING `+stateColumns, up.ID, received, in.Sequence+1, UploadTTLHours))
		if err != nil {
			return nil, err
		}
		s := state(updated)
		s.Deduped = boolPtr(false)
		return s, nil
	})
}

// storedChunks concatenates an upload's chunks in sequence order, and reports
// how many chunk rows there were.
func storedChunks(ctx context.Context, q db.Querier, uploadID string, capacity int) ([]byte, int, error) {
	rows, err := q.Query(ctx,
		`SELECT bytes FROM page_content_upload_chunks WHERE upload_id = $1 ORDER BY sequence`, uploadID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]byte, 0, capacity)
	n := 0
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, 0, err
		}
		out = append(out, b...)
		n++
	}
	return out, n, rows.Err()
}

func storedBytes(ctx context.Context, q db.Querier, uploadID string, capacity int) ([]byte, error) {
	b, _, err := storedChunks(ctx, q, uploadID, capacity)
	return b, err
}

// Cancel discards an uncommitted upload. Unknown, expired, already-cancelled
// and cross-token handles are all the same non-revealing idempotent no-op; a
// committed upload is a 409.
func Cancel(ctx context.Context, d *db.DB, uploadID string, a versions.Actor) (*CancelResult, error) {
	tokenID, err := actorTokenID(a)
	if err != nil {
		return nil, err
	}
	ctx = db.WithDrainBypass(ctx)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*CancelResult, error) {
		if err := cleanupExpired(ctx, tx); err != nil {
			return nil, err
		}
		var t target
		err := tx.QueryRow(ctx,
			`DELETE FROM page_content_uploads
			  WHERE id = $1 AND token_id = $2 AND committed_at IS NULL
			  RETURNING slug, target_kind`, uploadID, tokenID).Scan(&t.name, &t.kind)
		if err == nil {
			if t.kind == "" {
				t.kind = KindPage
			}
			counts, err := recordAttempt(ctx, tx, tokenID, t, "cancels")
			if err != nil {
				return nil, err
			}
			// The old next_step read as an invitation to start another one,
			// which is how a caller cancels eight times. Say what a restart
			// actually costs.
			next := attemptAlarm(counts)
			if next == "" {
				next = "Staged bytes were deleted. A new chunked upload re-emits the whole document from sequence 0 — " +
					"if you cancelled to recover from a sequence or hash error, that was not necessary. " +
					"create_upload_ticket avoids both."
			}
			return &CancelResult{UploadID: uploadID, Cancelled: true, NextStep: next}, nil
		}
		if !db.IsNoRows(err) {
			return nil, err
		}
		var committed bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM page_content_uploads
			                 WHERE id = $1 AND token_id = $2 AND committed_at IS NOT NULL)`,
			uploadID, tokenID).Scan(&committed); err != nil {
			return nil, err
		}
		if committed {
			return nil, apierror.Conflict("a deployed upload cannot be cancelled", "page_upload_committed", nil)
		}
		return &CancelResult{
			UploadID: uploadID, Cancelled: false,
			NextStep: "No active upload was found; no page or staged content changed.",
		}, nil
	})
}

// assertTarget / readVerified — the checks between "an upload exists" and
// "these are its bytes". Split out of Deploy so a read-only caller (Peek) runs
// the identical gauntlet: right target kind, complete, byte count and SHA-256
// as declared, valid UTF-8. A validator that skipped any of these would be
// reporting on something other than what registration is going to see.
func assertTarget(r *row, expectKind string) (string, error) {
	kind := r.kind()
	if kind != expectKind {
		return "", apierror.Conflict(
			fmt.Sprintf("this upload was staged for a %s, not a %s; use %s instead", kind, expectKind, ConsumerFor(kind)),
			"page_upload_target_mismatch", jsonv.ObjectOf("target_kind", kind))
	}
	return kind, nil
}

var utf8BOM = []byte{0xef, 0xbb, 0xbf}

func readVerified(ctx context.Context, q db.Querier, r *row) (string, error) {
	if r.BytesReceived != r.TotalBytes {
		return "", apierror.Conflict(
			fmt.Sprintf("upload is incomplete: %d of %d bytes received", r.BytesReceived, r.TotalBytes),
			"page_upload_incomplete",
			jsonv.ObjectOf("bytes_received", r.BytesReceived, "total_bytes", r.TotalBytes))
	}
	content, n, err := storedChunks(ctx, q, r.ID, r.TotalBytes)
	if err != nil {
		return "", err
	}
	if n != r.NextSequence {
		return "", apierror.Conflict("upload chunks are incomplete", "page_upload_incomplete", nil)
	}
	// Buffer.concat(list, totalLength) truncates to the declared length.
	if len(content) > r.TotalBytes {
		content = content[:r.TotalBytes]
	}
	if len(content) != r.TotalBytes || SHA256(content) != r.ContentSHA256 {
		return "", apierror.Conflict("stored upload failed byte-count or SHA-256 verification", "upload_hash_mismatch", nil)
	}
	if !utf8.Valid(content) {
		return "", apierror.BadRequest("uploaded page content must be valid UTF-8", "upload_encoding_invalid", nil)
	}
	// TextDecoder("utf-8") drops a leading byte-order mark.
	return string(bytes.TrimPrefix(content, utf8BOM)), nil
}

// PeekOptions: ExpectKind defaults to "page".
type PeekOptions struct {
	ExpectKind string
}

// Peek reads a staged upload's bytes WITHOUT consuming it. This is what lets a
// dry run cost nothing: the bytes were already PUT once through a ticket, so
// validating them must not force a re-upload, and must not spend the upload
// either. Nothing is written; the same upload_id can still be registered
// after.
//
// An already-committed upload is refused rather than re-read: its chunks are
// deleted on commit, so there would be nothing to verify against.
func Peek(ctx context.Context, d *db.DB, uploadID string, a versions.Actor, opts PeekOptions) (*Peeked, error) {
	expect := opts.ExpectKind
	if expect == "" {
		expect = KindPage
	}
	ctx = db.WithDrainBypass(ctx)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (*Peeked, error) {
		up, err := lockUpload(ctx, tx, uploadID, a)
		if err != nil {
			return nil, err
		}
		if _, err := assertTarget(up, expect); err != nil {
			return nil, err
		}
		if up.CommitResult != nil {
			return nil, apierror.Conflict("this upload was already committed; its staged bytes are gone", "page_upload_already_committed", nil)
		}
		html, err := readVerified(ctx, tx, up)
		if err != nil {
			return nil, err
		}
		slug, template := up.targets()
		return &Peeked{
			UploadID: up.ID, TargetKind: up.kind(), Slug: slug, Template: template,
			TotalBytes: up.TotalBytes, ContentSHA256: up.ContentSHA256, HTML: html,
		}, nil
	})
}

// Staged is what a consumer receives: the verified bytes and their target.
// Slug is "" for a template upload and Template is "" otherwise (one target
// column, two readings).
type Staged struct {
	UploadID   string
	Slug       string
	Template   string
	TargetKind string
	HTML       string
}

// DeployFunc consumes verified bytes inside the upload's transaction. Its
// result (anything jsonv.Marshal accepts) becomes the immutable commit
// receipt; an error rolls everything back and leaves the upload deployable.
type DeployFunc func(ctx context.Context, tx pgx.Tx, staged Staged) (any, error)

// ReplayFunc refreshes read-only guidance on a saved receipt (a jsonv value,
// read back from jsonb) for a retried commit. It must never run the mutation
// again.
type ReplayFunc func(ctx context.Context, tx pgx.Tx, receipt any) (any, error)

// DeployOptions: ExpectKind defaults to "page"; ReplayCommitted nil returns
// the saved receipt as stored.
type DeployOptions struct {
	ExpectKind      string
	ReplayCommitted ReplayFunc
}

// canonical is JSON.parse(JSON.stringify(v)), returning the JSON text too.
func canonical(v any) (any, string, error) {
	s, err := jsonv.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	out, err := jsonv.ParseString(s)
	return out, s, err
}

// Deploy commits a verified upload exactly once. commitKey (a lowercase
// SHA-256 of the consumer's options) binds a retry to the original options: a
// retry with the same key replays the saved receipt, a different key is 409
// page_upload_commit_conflict. The result is a jsonv value.
//
// The target kind is checked under the row lock, before any bytes are read:
// an upload staged for a template is not deployable as a page, and vice
// versa. The target was fixed when the upload started, so it cannot be argued
// with here.
func Deploy(ctx context.Context, d *db.DB, uploadID string, a versions.Actor, commitKey string, deployVerified DeployFunc, opts DeployOptions) (any, error) {
	if !sha256RE.MatchString(commitKey) {
		return nil, errors.New("commit key is required")
	}
	if deployVerified == nil {
		return nil, errors.New("deploy callback is required")
	}
	expect := opts.ExpectKind
	if expect == "" {
		expect = KindPage
	}
	ctx = db.WithDrainBypass(ctx)
	return db.WithTxResult(ctx, d, func(tx pgx.Tx) (any, error) {
		up, err := lockUpload(ctx, tx, uploadID, a)
		if err != nil {
			return nil, err
		}
		kind, err := assertTarget(up, expect)
		if err != nil {
			return nil, err
		}
		if up.CommitResult != nil {
			if up.CommitKey == nil || *up.CommitKey != commitKey {
				return nil, apierror.Conflict(
					"this upload_id was already deployed with different options; use the original options or start a new upload",
					"page_upload_commit_conflict", nil)
			}
			receipt, err := jsonv.ParseString(*up.CommitResult)
			if err != nil {
				return nil, err
			}
			// The saved receipt is immutable. A consumer may refresh read-only
			// guidance for this reply, but must never run the mutation again.
			if opts.ReplayCommitted == nil {
				return receipt, nil
			}
			replayed, err := opts.ReplayCommitted(ctx, tx, receipt)
			if err != nil {
				return nil, err
			}
			out, _, err := canonical(replayed)
			return out, err
		}
		html, err := readVerified(ctx, tx, up)
		if err != nil {
			return nil, err
		}
		staged := Staged{UploadID: up.ID, TargetKind: kind, HTML: html}
		if kind == KindTemplate {
			staged.Template = up.Slug
		} else {
			staged.Slug = up.Slug
		}
		produced, err := deployVerified(ctx, tx, staged)
		if err != nil {
			return nil, err
		}
		result, text, err := canonical(produced)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE page_content_uploads
			    SET commit_key = $2, commit_result = $3::jsonb, committed_at = now(),
			        expires_at = now() + ($4 * interval '1 hour'), updated_at = now()
			  WHERE id = $1`, up.ID, commitKey, text, UploadTTLHours); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM page_content_upload_chunks WHERE upload_id = $1", up.ID); err != nil {
			return nil, err
		}
		return result, nil
	})
}

// UploadURL is the absolute URL handed to the agent, so it never reconstructs
// one from a hostname and gets the zone wrong.
func UploadURL(uploadID string) string {
	return env.DashboardOrigin() + "/upload/" + uploadID
}
