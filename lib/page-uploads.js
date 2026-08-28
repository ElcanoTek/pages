// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Durable staged HTML uploads for MCP. Real dashboard files routinely exceed
// a model/provider's safe tool-argument size even though Pages accepts a 2 MiB
// HTTP body. Small base64 chunks plus an explicit server-minted handle keep
// every call bounded; PostgreSQL makes the flow work across server instances.

const crypto = require("node:crypto");
const { TextDecoder } = require("node:util");
const db = require("./db");
const versions = require("./versions");
const templates = require("./templates");
const { badRequest, conflict, notFound, unauthorized } = require("./apierror");
const { hashToken } = require("./tokens");

const MAX_UPLOAD_BYTES = 2 * 1024 * 1024;
// Every one of these bytes is base64 the calling MODEL has to emit token by
// token, so the ceiling is a direct multiplier on the cost and failure rate of
// a deploy. At the original 12 KiB a routine 65 KB dashboard took six perfect
// append calls; real conversations spent ~25k output tokens per attempt, hit
// turn timeouts mid-upload, and abandoned 13 of 14 uploads. 48 KiB (64 KiB of
// base64, ~16k tokens) is comfortably inside a modern model's single-argument
// budget and cuts that to two calls.
//
// This is a CEILING, not a required chunk size — `next_step` says "or fewer"
// and sequences are size-independent, so a client that can only emit small
// arguments keeps working unchanged. Tunable for ops without a redeploy.
const MAX_CHUNK_BYTES = clampChunkBytes(process.env.PAGE_UPLOAD_MAX_CHUNK_BYTES, 48 * 1024);

// Above this, a FRESH chunked upload is told about the ticket path before it emits
// any base64. Deliberately low: the cheap path is better at almost any size, and
// the advisory costs one sentence in a response the caller is already reading.
const CHUNKING_ADVISORY_BYTES = 20000;

// Appended to every chunk/hash failure. Observed sessions hit upload_chunk_invalid
// and upload_hash_mismatch four and five times in a row, cancelled, and started
// the same 64 KB base64 upload again — one of them said out loud it was "fighting
// base64 corruption" and went hunting through binaries for an HTTP endpoint, which
// is exactly what create_upload_ticket hands back. A failure is the one moment the
// caller is definitely reading, so it is where the cheaper path belongs.
const CHUNKING_HINT =
  "Hand-emitting base64 is the expensive way to move a file and the easy way to corrupt one: " +
  "cancel this upload and use create_upload_ticket instead — it returns a URL your shell PUTs the " +
  "file to, so the bytes never pass through your context.";
// The single most expensive response available to a sequence conflict is
// cancel-and-start-over, and it is the one every observed caller reached for —
// eight times in one conversation, re-emitting the whole document each time.
// Nothing in the error said the alternative existed, so it says it now: a failed
// append does not advance the sequence, and everything already accepted is still
// on the server.
const RESUME_HINT =
  "Do NOT cancel: a failed append changes nothing, and every chunk already accepted is still here. " +
  "Re-send from the expected sequence and continue.";
const MAX_CHUNK_BASE64_CHARS = Math.ceil(MAX_CHUNK_BYTES / 3) * 4;
const MAX_ACTIVE_UPLOADS = 5;
// How long a caller's chunked-upload attempts at one target stay counted. Long
// enough to span a session, short enough that a page someone uploads by hand
// once a month never accumulates into a permanent scolding.
const ATTEMPT_WINDOW_HOURS = 6;
// Past either of these, the caller is demonstrably in the loop that burns turns:
// ten starts and eight cancels for one page is the observed case. Advisory only.
const ATTEMPT_START_ALARM = 3;
const ATTEMPT_CANCEL_ALARM = 2;
const UPLOAD_TTL_HOURS = 24;
// An upload with no append for this long, from a caller that is at its cap, is
// abandoned rather than slow: appends within one turn land seconds apart. Only
// consulted when the cap would otherwise reject a start (see `start`).
const STALE_UPLOAD_MINUTES = 60;
// An upload ticket is minted and spent inside one agent turn. Keep the window
// tight: it is a bearer string that necessarily passes through a model context.
const TICKET_TTL_MINUTES = Number.parseInt(process.env.PAGE_UPLOAD_TICKET_TTL_MINUTES, 10) || 15;
const SHA256_RE = /^[0-9a-f]{64}$/;
const BASE64_RE = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;

// Keep an operator override inside sane bounds: too small multiplies round
// trips, too large risks provider-side argument truncation mid-document.
function clampChunkBytes(raw, fallback) {
  const n = Number.parseInt(raw, 10);
  if (!Number.isInteger(n) || n < 4 * 1024 || n > 256 * 1024) return fallback;
  return n;
}

function sha256(bytes) {
  return crypto.createHash("sha256").update(bytes).digest("hex");
}

function decodeBase64Chunk(value) {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.length > MAX_CHUNK_BASE64_CHARS ||
    value.length % 4 !== 0 ||
    !BASE64_RE.test(value)
  ) {
    throw badRequest(
      "chunk_base64 must be canonical base64 for one non-empty chunk. " + CHUNKING_HINT,
      "upload_chunk_invalid"
    );
  }
  const bytes = Buffer.from(value, "base64");
  if (bytes.length === 0 || bytes.length > MAX_CHUNK_BYTES || bytes.toString("base64") !== value) {
    throw badRequest(`decoded chunk must be 1-${MAX_CHUNK_BYTES} bytes. ${CHUNKING_HINT}`, "upload_chunk_invalid");
  }
  return bytes;
}

function actorTokenId(ctx) {
  const tokenId = ctx && ctx.tokenId;
  if (tokenId === undefined || tokenId === null || !/^[1-9][0-9]*$/.test(String(tokenId))) {
    throw new Error("authenticated MCP token id is missing");
  }
  return String(tokenId);
}

function iso(value) {
  return value instanceof Date ? value.toISOString() : new Date(value).toISOString();
}

// The tool that consumes a verified upload, by what it was staged for. A staged
// upload is useless without the name of the one call that can spend it, and the
// three targets do not share one.
const CONSUMER_TOOL = {
  page: "deploy_page_upload",
  template: "register_template_upload",
  data: "update_page_data_upload",
};

function consumerFor(kind) {
  return CONSUMER_TOOL[kind || "page"] || CONSUMER_TOOL.page;
}

function state(row) {
  const complete = Number(row.bytes_received) === Number(row.total_bytes);
  const kind = row.target_kind || "page";
  const isTemplate = kind === "template";
  return {
    upload_id: row.id,
    // The row's one target column reads as a slug or a template name depending
    // on target_kind; report the one that is true and null the other, so a
    // caller can never act on a template name as though it were a page.
    target_kind: row.target_kind || "page",
    slug: isTemplate ? null : row.slug,
    template: isTemplate ? row.slug : null,
    content_sha256: row.content_sha256,
    total_bytes: Number(row.total_bytes),
    bytes_received: Number(row.bytes_received),
    next_sequence: Number(row.next_sequence),
    max_chunk_bytes: MAX_CHUNK_BYTES,
    complete,
    expires_at: iso(row.expires_at),
    next_step: complete
      ? `Upload verified. Call ${consumerFor(kind)} with this upload_id; do not resend the ${kind === "data" ? "payload" : "HTML"}.`
      // On a FRESH upload of any size, say the cheaper thing before a single byte
      // of base64 is emitted. One observed session shrank its chunks from 16 KB to
      // 2 KB chasing corruption, which turned a 4-round-trip upload into a
      // 32-round-trip one, and the turn ended at 16,000 of 64,291 bytes — never
      // deployed. This line is the last moment before that starts.
      : Number(row.bytes_received) === 0 && Number(row.total_bytes) > CHUNKING_ADVISORY_BYTES
        ? `This file is ${Number(row.total_bytes)} bytes. Prefer create_upload_ticket: it returns a URL your ` +
          `shell PUTs the file to, so the bytes never pass through your context and cannot be corrupted in ` +
          `transit. Cancel this upload and use it unless your environment cannot make outbound HTTP requests. ` +
          `To continue here anyway, base64-encode the next ${MAX_CHUNK_BYTES} or fewer raw bytes and call ` +
          `append_page_upload with sequence ${row.next_sequence} — use the full chunk size; smaller chunks ` +
          `mean more round trips, and every round trip re-sends everything before it.`
        : `Base64-encode the next ${MAX_CHUNK_BYTES} or fewer raw bytes and call append_page_upload with sequence ${row.next_sequence}.`,
  };
}

async function cleanupExpired(client) {
  await client.query("DELETE FROM page_content_uploads WHERE expires_at <= now()");
}

async function lockUpload(client, uploadId, ctx) {
  await cleanupExpired(client);
  const { rows } = await client.query(
    `SELECT id, token_id, slug, target_kind, total_bytes, content_sha256, bytes_received,
            next_sequence, commit_key, commit_result, committed_at, expires_at
       FROM page_content_uploads
      WHERE id = $1 AND token_id = $2
      FOR UPDATE`,
    [uploadId, actorTokenId(ctx)]
  );
  if (!rows[0]) {
    throw notFound("page upload not found, expired, or owned by another token", "page_upload_not_found");
  }
  return rows[0];
}

function assertDeclaredContent(totalBytes, contentSha256) {
  if (!Number.isInteger(totalBytes) || totalBytes < 1 || totalBytes > MAX_UPLOAD_BYTES) {
    throw badRequest(`total_bytes must be 1-${MAX_UPLOAD_BYTES}`, "upload_size_invalid");
  }
  if (typeof contentSha256 !== "string" || !SHA256_RE.test(contentSha256)) {
    throw badRequest("content_sha256 must be a lowercase SHA-256 hex digest", "upload_hash_invalid");
  }
}

// recordAttempt / attemptAlarm — the memory that makes a loop visible from
// inside it. Cancelling deletes the upload row, so without this every attempt
// looks like the first one; the conversation that spent 10M tokens started ten
// uploads for one page and cancelled eight, and nothing anywhere said so.
async function recordAttempt(client, tokenId, target, field) {
  await client.query(
    `DELETE FROM page_upload_attempts WHERE updated_at < now() - ($1 * interval '1 hour')`,
    [ATTEMPT_WINDOW_HOURS]
  );
  const { rows } = await client.query(
    `INSERT INTO page_upload_attempts (token_id, target_kind, slug, ${field})
     VALUES ($1, $2, $3, 1)
     ON CONFLICT (token_id, target_kind, slug) DO UPDATE
        SET ${field} = page_upload_attempts.${field} + 1, updated_at = now()
     RETURNING starts, cancels`,
    [tokenId, target.kind, target.name]
  );
  return rows[0];
}

// The advisory itself. Not a refusal: an environment that genuinely cannot make
// outbound HTTP requests has to keep using this path, and blocking it there
// turns a bad turn into an impossible one.
function attemptAlarm(counts) {
  if (!counts) return null;
  const starts = Number(counts.starts) || 0;
  const cancels = Number(counts.cancels) || 0;
  if (starts < ATTEMPT_START_ALARM && cancels < ATTEMPT_CANCEL_ALARM) return null;
  return (
    `You have started ${starts} chunked upload${starts === 1 ? "" : "s"} for this target and cancelled ` +
    `${cancels}. That loop does not converge — each restart re-emits the whole document, and the turn ends ` +
    `before the file lands. Stop and use create_upload_ticket: it returns a URL your shell PUTs the file to, ` +
    `so the bytes never pass through your context and there is no sequence to keep straight.`
  );
}

// The per-token active-upload cap, plus the stale reaper that keeps it from
// becoming a dead end. Shared by both ways of opening an upload.
async function reserveUploadSlot(client, tokenId) {
  // Serialize the count+insert guard per token; without this, concurrent
  // starts could both observe four active uploads and exceed the cap.
  await client.query("SELECT pg_advisory_xact_lock($1::bigint)", [tokenId]);
  await cleanupExpired(client);
  const countActive = async () =>
    (
      await client.query(
        `SELECT count(*)::integer AS count
           FROM page_content_uploads
          WHERE token_id = $1 AND committed_at IS NULL AND expires_at > now()`,
        [tokenId]
      )
    ).rows[0].count;

  if ((await countActive()) < MAX_ACTIVE_UPLOADS) return;
  // Uploads are abandoned far more often than they are finished: a turn
  // times out or hits a token ceiling mid-append and nothing ever calls
  // cancel. With a 24h TTL those corpses held the cap for a day and the
  // caller's only way forward was to guess five upload_ids and cancel each
  // by hand — which is exactly what happened in production.
  //
  // Reap only when the cap is actually in the way, and only uploads that
  // have been silent for STALE_UPLOAD_MINUTES, so an in-flight upload that
  // simply paused between appends is never destroyed underneath its caller.
  await client.query(
    `DELETE FROM page_content_uploads
      WHERE token_id = $1
        AND committed_at IS NULL
        AND updated_at < now() - ($2 * interval '1 minute')`,
    [tokenId, STALE_UPLOAD_MINUTES]
  );
  if ((await countActive()) >= MAX_ACTIVE_UPLOADS) {
    throw conflict(
      `this token already has ${MAX_ACTIVE_UPLOADS} uploads in flight and none are stale. ` +
        `Call cancel_page_upload on the ones you are no longer sending, or wait for them to go idle.`,
      "page_upload_limit"
    );
  }
}

// normalizeTarget — an upload stages bytes for exactly one destination: a page
// slug or a template name, never both and never neither. The kind is recorded on
// the row so the commit step can refuse a mismatched consumer, which is what
// stops a template skeleton (whose data block is deliberately empty) from being
// published as a live client page.
// A slug can be staged for two different things — the page's HTML, or its
// managed-data payload — so `kind` disambiguates when a slug is given. It is
// meaningless with `template`, which names its own kind, and passing it there is
// refused rather than ignored: silently accepting kind:'data' alongside a
// template would stage bytes that no tool can ever consume.
const SLUG_TARGET_KINDS = new Set(["page", "data"]);

function normalizeTarget({ slug, template, kind }) {
  const hasSlug = slug !== undefined && slug !== null;
  const hasTemplate = template !== undefined && template !== null;
  if (hasSlug === hasTemplate) {
    throw badRequest("supply exactly one of slug or template", "upload_target_required");
  }
  if (hasTemplate) {
    if (kind !== undefined && kind !== null && kind !== "template") {
      throw badRequest("kind applies to a slug target; a template upload is always kind template", "upload_target_kind_invalid");
    }
    return { kind: "template", name: templates.normalizeTemplateName(template) };
  }
  const slugKind = kind === undefined || kind === null ? "page" : kind;
  if (!SLUG_TARGET_KINDS.has(slugKind)) {
    throw badRequest(`kind must be one of: ${[...SLUG_TARGET_KINDS].join(", ")}`, "upload_target_kind_invalid");
  }
  return { kind: slugKind, name: versions.normalizeSlug(slug) };
}

async function start({ slug, template, kind, totalBytes, contentSha256 }, ctx) {
  const target = normalizeTarget({ slug, template, kind });
  assertDeclaredContent(totalBytes, contentSha256);
  const tokenId = actorTokenId(ctx);
  return db.withTransaction(async (client) => {
    await reserveUploadSlot(client, tokenId);
    const id = crypto.randomUUID();
    const counts = await recordAttempt(client, tokenId, target, "starts");
    const { rows } = await client.query(
      `INSERT INTO page_content_uploads
         (id, token_id, slug, target_kind, total_bytes, content_sha256, expires_at)
       VALUES ($1, $2, $3, $4, $5, $6, now() + ($7 * interval '1 hour'))
       RETURNING id, slug, target_kind, total_bytes, content_sha256, bytes_received,
                 next_sequence, expires_at`,
      [id, tokenId, target.name, target.kind, totalBytes, contentSha256, UPLOAD_TTL_HOURS]
    );
    const opened = state(rows[0]);
    const alarm = attemptAlarm(counts);
    // Front of next_step, not appended: by the time a caller is on its fourth
    // start it is skimming, and the sentence that matters has to be first.
    return alarm ? { ...opened, next_step: `${alarm} ${opened.next_step}` } : opened;
  });
}

// ── Out-of-band content upload ───────────────────────────────────────────────
// createTicket opens exactly the same staged upload as start(), then attaches a
// one-shot write-only credential the CALLER'S SANDBOX can use to send the file
// directly. The model handles a URL and an opaque handle; the bytes never enter
// its context. See migrations/014 for the threat model.

async function createTicket({ slug, template, kind, totalBytes, contentSha256 }, ctx) {
  const target = normalizeTarget({ slug, template, kind });
  assertDeclaredContent(totalBytes, contentSha256);
  const tokenId = actorTokenId(ctx);
  const ticket = `pgu_${crypto.randomBytes(24).toString("base64url")}`;
  return db.withTransaction(async (client) => {
    await reserveUploadSlot(client, tokenId);
    const id = crypto.randomUUID();
    // Deliberately not counted as an attempt: this IS the path the advisory
    // steers to, and a caller who took it must not be scolded for arriving.
    const { rows } = await client.query(
      `INSERT INTO page_content_uploads
         (id, token_id, slug, target_kind, total_bytes, content_sha256, expires_at,
          ticket_hash, ticket_expires_at)
       VALUES ($1, $2, $3, $4, $5, $6, now() + ($7 * interval '1 hour'),
               $8, now() + ($9 * interval '1 minute'))
       RETURNING id, slug, target_kind, total_bytes, content_sha256, bytes_received,
                 next_sequence, expires_at, ticket_expires_at`,
      [
        id,
        tokenId,
        target.name,
        target.kind,
        totalBytes,
        contentSha256,
        UPLOAD_TTL_HOURS,
        hashToken(ticket),
        TICKET_TTL_MINUTES,
      ]
    );
    return { ...state(rows[0]), ticket, ticket_expires_at: iso(rows[0].ticket_expires_at) };
  });
}

// Cheap pre-flight on the TICKET ALONE, so a bad one is rejected before the
// server buffers a 2 MiB body (the same auth-before-parse ordering the MCP
// boundary uses). Advisory: putTicketContent re-checks everything under a row
// lock, so a race here can only cost a wasted read, never a bad write.
async function checkTicket(rawTicket, uploadId) {
  if (typeof rawTicket !== "string" || !rawTicket) {
    throw unauthorized("an upload ticket is required", "upload_ticket_missing");
  }
  const { rows } = await db.query(
    `SELECT id, total_bytes, ticket_expires_at, committed_at
       FROM page_content_uploads
      WHERE ticket_hash = $1 AND expires_at > now()`,
    [hashToken(rawTicket)]
  );
  const upload = rows[0];
  if (!upload || String(upload.id) !== String(uploadId)) {
    throw unauthorized("unknown or expired upload ticket", "upload_ticket_invalid");
  }
  if (upload.committed_at) throw conflict("this upload has already been deployed", "page_upload_committed");
  if (new Date(upload.ticket_expires_at).getTime() <= Date.now()) {
    throw unauthorized("this upload ticket has expired; create a new one", "upload_ticket_expired");
  }
  return { totalBytes: Number(upload.total_bytes) };
}

// Accept the whole document in one request, authenticated by the ticket alone.
// `content` has already been length-capped by the body parser.
async function putTicketContent(rawTicket, uploadId, content) {
  if (typeof rawTicket !== "string" || !rawTicket) {
    throw unauthorized("an upload ticket is required", "upload_ticket_missing");
  }
  if (!Buffer.isBuffer(content) || content.length === 0) {
    throw badRequest("request body must be the raw page bytes", "upload_body_empty");
  }
  const ticketHash = hashToken(rawTicket);
  return db.withTransaction(async (client) => {
    await cleanupExpired(client);
    const { rows } = await client.query(
      `SELECT id, slug, target_kind, total_bytes, content_sha256, bytes_received,
              next_sequence, expires_at, ticket_used_at, ticket_expires_at, committed_at
         FROM page_content_uploads
        WHERE ticket_hash = $1
        FOR UPDATE`,
      [ticketHash]
    );
    const upload = rows[0];
    // One non-revealing answer for a wrong ticket, a ticket for another upload,
    // and an upload that no longer exists.
    if (!upload || String(upload.id) !== String(uploadId)) {
      throw unauthorized("unknown or expired upload ticket", "upload_ticket_invalid");
    }
    if (upload.committed_at) throw conflict("this upload has already been deployed", "page_upload_committed");
    if (new Date(upload.ticket_expires_at).getTime() <= Date.now()) {
      throw unauthorized("this upload ticket has expired; create a new one", "upload_ticket_expired");
    }

    const declaredBytes = Number(upload.total_bytes);
    // Re-sending the identical document is a safe no-op: the content is pinned
    // by content_sha256, so a retried curl cannot mean anything else.
    if (upload.ticket_used_at) {
      if (Number(upload.bytes_received) === declaredBytes && sha256(content) === upload.content_sha256) {
        return { ...state(upload), deduped: true };
      }
      throw conflict("this upload ticket was already used; create a new one", "upload_ticket_used");
    }
    if (content.length !== declaredBytes) {
      throw badRequest(
        `expected exactly ${declaredBytes} bytes, received ${content.length}`,
        "upload_size_mismatch"
      );
    }
    if (sha256(content) !== upload.content_sha256) {
      // No CHUNKING_HINT here: this IS the ticket path, so "use create_upload_ticket
      // instead" is advice the caller already took. The useful thing to say is that
      // the ticket is content-pinned and still usable, and that the bytes changed
      // between hashing and sending.
      throw conflict(
        "uploaded bytes do not match content_sha256. The ticket is pinned to the hash you declared, so " +
          "the file changed between hashing and sending, or the transfer altered it — re-hash the exact file " +
          "you are sending (`sha256sum <file>`) and PUT it again with --data-binary; the ticket is still valid.",
        "upload_hash_mismatch"
      );
    }

    // Reuse the chunk store so deploy() reassembles and re-verifies exactly as
    // it does for an appended upload — one commit path, not two. The split is a
    // storage detail the caller never sees.
    let sequence = 0;
    for (let offset = 0; offset < content.length; offset += MAX_CHUNK_BYTES) {
      const slice = content.subarray(offset, offset + MAX_CHUNK_BYTES);
      await client.query(
        `INSERT INTO page_content_upload_chunks (upload_id, sequence, bytes, content_sha256)
         VALUES ($1, $2, $3, $4)`,
        [upload.id, sequence, slice, sha256(slice)]
      );
      sequence += 1;
    }
    const updated = await client.query(
      `UPDATE page_content_uploads
          SET bytes_received = $2, next_sequence = $3, ticket_used_at = now(),
              expires_at = now() + ($4 * interval '1 hour'), updated_at = now()
        WHERE id = $1
        RETURNING id, slug, target_kind, total_bytes, content_sha256, bytes_received,
                  next_sequence, expires_at`,
      [upload.id, content.length, sequence, UPLOAD_TTL_HOURS]
    );
    return { ...state(updated.rows[0]), deduped: false };
  });
}

async function append({ uploadId, sequence, chunkBase64 }, ctx) {
  if (!Number.isInteger(sequence) || sequence < 0) {
    throw badRequest("sequence must be a non-negative integer", "upload_sequence_invalid");
  }
  const bytes = decodeBase64Chunk(chunkBase64);
  const chunkSha = sha256(bytes);

  return db.withTransaction(async (client) => {
    const upload = await lockUpload(client, uploadId, ctx);
    if (upload.commit_result) {
      throw conflict("this upload has already been deployed", "page_upload_committed");
    }

    const nextSequence = Number(upload.next_sequence);
    if (sequence < nextSequence) {
      const prior = await client.query(
        `SELECT bytes, content_sha256
           FROM page_content_upload_chunks
          WHERE upload_id = $1 AND sequence = $2`,
        [upload.id, sequence]
      );
      if (
        prior.rows[0] &&
        prior.rows[0].content_sha256 === chunkSha &&
        Buffer.isBuffer(prior.rows[0].bytes) &&
        prior.rows[0].bytes.equals(bytes)
      ) {
        return { ...state(upload), deduped: true };
      }
      throw conflict(
        `sequence ${sequence} was already accepted with DIFFERENT bytes; ${upload.bytes_received} of ` +
          `${upload.total_bytes} bytes are stored and the next chunk expected is ${nextSequence}. ` +
          "You are re-sending a chunk you already sent, with a different encoding of it. " +
          RESUME_HINT,
        "upload_sequence_conflict",
        {
          expected_sequence: nextSequence,
          bytes_received: Number(upload.bytes_received),
          total_bytes: Number(upload.total_bytes),
          resumable: true,
        }
      );
    }
    if (sequence > nextSequence) {
      throw conflict(
        `expected upload sequence ${nextSequence}, received ${sequence}: ${upload.bytes_received} of ` +
          `${upload.total_bytes} bytes are stored. A FAILED append does not advance the sequence, so ` +
          `whatever went wrong at ${nextSequence} still has to be sent. ` +
          RESUME_HINT,
        "upload_sequence_conflict",
        {
          expected_sequence: nextSequence,
          bytes_received: Number(upload.bytes_received),
          total_bytes: Number(upload.total_bytes),
          resumable: true,
        }
      );
    }

    const bytesReceived = Number(upload.bytes_received) + bytes.length;
    if (bytesReceived > Number(upload.total_bytes)) {
      throw badRequest(
        `chunk exceeds the declared total_bytes by ${bytesReceived - Number(upload.total_bytes)} bytes ` +
          `(declared ${upload.total_bytes}, already stored ${upload.bytes_received}). ` +
          "Do NOT trim the payload to fit: the declaration describes a DIFFERENT document from the one you " +
          "are sending, so a trimmed one would fail content_sha256 anyway — and if it did not, you would " +
          "publish a truncated page. Recompute total_bytes and content_sha256 from the exact bytes you " +
          "intend to send and start a new upload. " +
          CHUNKING_HINT,
        "upload_size_mismatch",
        {
          declared_total_bytes: Number(upload.total_bytes),
          bytes_received: Number(upload.bytes_received),
          overflow_bytes: bytesReceived - Number(upload.total_bytes),
        }
      );
    }

    // Validate the complete byte stream before accepting its last chunk. A
    // mismatch rolls this transaction back, preserving the same sequence for
    // a corrected retry (or the caller can start a fresh upload).
    if (bytesReceived === Number(upload.total_bytes)) {
      const prior = await client.query(
        `SELECT bytes FROM page_content_upload_chunks
          WHERE upload_id = $1 ORDER BY sequence`,
        [upload.id]
      );
      const complete = Buffer.concat([...prior.rows.map((row) => row.bytes), bytes], bytesReceived);
      if (sha256(complete) !== upload.content_sha256) {
        throw conflict(
          `complete upload does not match content_sha256. Sequence ${sequence} was NOT accepted, so the ` +
            `upload still holds ${upload.bytes_received} of ${upload.total_bytes} bytes and this exact ` +
            "sequence is still the one expected — re-send it if that chunk was wrong. Only if the earlier " +
            "chunks are wrong does this need a new upload. " +
            CHUNKING_HINT,
          "upload_hash_mismatch",
          {
            expected_sequence: sequence,
            bytes_received: Number(upload.bytes_received),
            total_bytes: Number(upload.total_bytes),
            resumable: true,
          }
        );
      }
    }

    await client.query(
      `INSERT INTO page_content_upload_chunks (upload_id, sequence, bytes, content_sha256)
       VALUES ($1, $2, $3, $4)`,
      [upload.id, sequence, bytes, chunkSha]
    );
    const { rows } = await client.query(
      `UPDATE page_content_uploads
          SET bytes_received = $2, next_sequence = $3,
              expires_at = now() + ($4 * interval '1 hour'), updated_at = now()
        WHERE id = $1
        RETURNING id, slug, target_kind, total_bytes, content_sha256, bytes_received,
                  next_sequence, expires_at`,
      [upload.id, bytesReceived, sequence + 1, UPLOAD_TTL_HOURS]
    );
    return { ...state(rows[0]), deduped: false };
  });
}

async function cancel(uploadId, ctx) {
  const tokenId = actorTokenId(ctx);
  return db.withTransaction(async (client) => {
    await cleanupExpired(client);
    const removed = await client.query(
      `DELETE FROM page_content_uploads
        WHERE id = $1 AND token_id = $2 AND committed_at IS NULL
        RETURNING id, slug, target_kind`,
      [uploadId, tokenId]
    );
    if (removed.rows[0]) {
      const counts = await recordAttempt(
        client,
        tokenId,
        { kind: removed.rows[0].target_kind || "page", name: removed.rows[0].slug },
        "cancels"
      );
      // The old next_step read as an invitation to start another one, which is
      // how a caller cancels eight times. Say what a restart actually costs.
      return {
        upload_id: uploadId,
        cancelled: true,
        next_step:
          attemptAlarm(counts) ||
          "Staged bytes were deleted. A new chunked upload re-emits the whole document from sequence 0 — " +
            "if you cancelled to recover from a sequence or hash error, that was not necessary. " +
            "create_upload_ticket avoids both.",
      };
    }
    const committed = await client.query(
      "SELECT 1 FROM page_content_uploads WHERE id = $1 AND token_id = $2 AND committed_at IS NOT NULL",
      [uploadId, tokenId]
    );
    if (committed.rows[0]) {
      throw conflict("a deployed upload cannot be cancelled", "page_upload_committed");
    }
    // Unknown, expired, already-cancelled, and cross-token handles are all the
    // same non-revealing idempotent no-op.
    return {
      upload_id: uploadId,
      cancelled: false,
      next_step: "No active upload was found; no page or staged content changed.",
    };
  });
}

function canonicalJson(value) {
  return JSON.parse(JSON.stringify(value));
}

// assertTarget / readVerified — the checks between "an upload exists" and "these
// are its bytes". Split out of deploy() so a read-only caller runs the identical
// gauntlet: right target kind, complete, byte count and SHA-256 as declared,
// valid UTF-8. A validator that skipped any of these would be reporting on
// something other than what registration is going to see.
function assertTarget(upload, expectKind) {
  const kind = upload.target_kind || "page";
  if (kind !== expectKind) {
    throw conflict(
      `this upload was staged for a ${kind}, not a ${expectKind}; use ${consumerFor(kind)} instead`,
      "page_upload_target_mismatch",
      { target_kind: kind }
    );
  }
  return kind;
}

async function readVerified(client, upload) {
  if (Number(upload.bytes_received) !== Number(upload.total_bytes)) {
    throw conflict(
      `upload is incomplete: ${upload.bytes_received} of ${upload.total_bytes} bytes received`,
      "page_upload_incomplete",
      { bytes_received: Number(upload.bytes_received), total_bytes: Number(upload.total_bytes) }
    );
  }
  const chunks = await client.query(
    `SELECT sequence, bytes FROM page_content_upload_chunks
      WHERE upload_id = $1 ORDER BY sequence`,
    [upload.id]
  );
  if (chunks.rows.length !== Number(upload.next_sequence)) {
    throw conflict("upload chunks are incomplete", "page_upload_incomplete");
  }
  const content = Buffer.concat(chunks.rows.map((row) => row.bytes), Number(upload.total_bytes));
  if (content.length !== Number(upload.total_bytes) || sha256(content) !== upload.content_sha256) {
    throw conflict("stored upload failed byte-count or SHA-256 verification", "upload_hash_mismatch");
  }
  try {
    return new TextDecoder("utf-8", { fatal: true }).decode(content);
  } catch {
    throw badRequest("uploaded page content must be valid UTF-8", "upload_encoding_invalid");
  }
}

// peek — read a staged upload's bytes WITHOUT consuming it. This is what lets a
// dry run cost nothing: the bytes were already PUT once through a ticket, so
// validating them must not force a re-upload, and must not spend the upload
// either. Nothing is written; the same upload_id can still be registered after.
//
// An already-committed upload is refused rather than re-read: its chunks are
// deleted on commit, so there would be nothing to verify against.
async function peek(uploadId, ctx, { expectKind = "page" } = {}) {
  return db.withTransaction(async (client) => {
    const upload = await lockUpload(client, uploadId, ctx);
    assertTarget(upload, expectKind);
    if (upload.commit_result) {
      throw conflict(
        "this upload was already committed; its staged bytes are gone",
        "page_upload_already_committed"
      );
    }
    const html = await readVerified(client, upload);
    return {
      upload_id: String(upload.id),
      target_kind: upload.target_kind || "page",
      // One column, two readings — same rule as state().
      slug: (upload.target_kind || "page") === "template" ? null : upload.slug,
      template: (upload.target_kind || "page") === "template" ? upload.slug : null,
      total_bytes: Number(upload.total_bytes),
      content_sha256: upload.content_sha256,
      html,
    };
  });
}

async function deploy(uploadId, ctx, commitKey, deployVerified, { expectKind = "page" } = {}) {
  if (typeof commitKey !== "string" || !SHA256_RE.test(commitKey)) throw new Error("commit key is required");
  if (typeof deployVerified !== "function") throw new Error("deploy callback is required");
  return db.withTransaction(async (client) => {
    const upload = await lockUpload(client, uploadId, ctx);
    // Checked under the row lock, before any bytes are read: an upload staged
    // for a template is not deployable as a page, and vice versa. The target was
    // fixed when the upload started, so this cannot be argued with here.
    const kind = assertTarget(upload, expectKind);
    if (upload.commit_result) {
      if (upload.commit_key !== commitKey) {
        throw conflict(
          "this upload_id was already deployed with different options; use the original options or start a new upload",
          "page_upload_commit_conflict"
        );
      }
      return upload.commit_result;
    }
    const html = await readVerified(client, upload);
    const result = canonicalJson(
      await deployVerified(client, {
        uploadId: upload.id,
        // One target column, two readings — same rule as state().
        slug: kind === "template" ? null : upload.slug,
        template: kind === "template" ? upload.slug : null,
        targetKind: kind,
        html,
      })
    );
    await client.query(
      `UPDATE page_content_uploads
          SET commit_key = $2, commit_result = $3::jsonb, committed_at = now(),
              expires_at = now() + ($4 * interval '1 hour'), updated_at = now()
        WHERE id = $1`,
      [upload.id, commitKey, JSON.stringify(result), UPLOAD_TTL_HOURS]
    );
    await client.query("DELETE FROM page_content_upload_chunks WHERE upload_id = $1", [upload.id]);
    return result;
  });
}

module.exports = {
  consumerFor,
  MAX_UPLOAD_BYTES,
  MAX_CHUNK_BYTES,
  MAX_CHUNK_BASE64_CHARS,
  MAX_ACTIVE_UPLOADS,
  STALE_UPLOAD_MINUTES,
  TICKET_TTL_MINUTES,
  UPLOAD_TTL_HOURS,
  decodeBase64Chunk,
  sha256,
  start,
  createTicket,
  checkTicket,
  putTicketContent,
  append,
  cancel,
  peek,
  deploy,
};
