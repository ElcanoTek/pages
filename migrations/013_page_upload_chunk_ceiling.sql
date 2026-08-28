-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 013_page_upload_chunk_ceiling.sql — raise the staged-upload chunk ceiling.
--
-- 009 pinned this at 12 KiB, matching lib/page-uploads.js at the time. Every
-- one of those bytes is base64 the calling MODEL emits token by token, so the
-- ceiling is a direct multiplier on the cost and failure rate of a deploy: a
-- routine 65 KB dashboard needed six flawless append calls, and in practice
-- turns timed out mid-upload and abandoned the staged bytes far more often
-- than they finished. lib/page-uploads.js now defaults to 48 KiB.
--
-- The app-side ceiling is operator-tunable (PAGE_UPLOAD_MAX_CHUNK_BYTES), so
-- this constraint is deliberately set to the top of that clamp rather than to
-- the current default — the DB guards against an absurd row, the application
-- owns the operational limit. Widening a CHECK needs no table rewrite and no
-- backfill: every existing chunk already satisfies it.

ALTER TABLE page_content_upload_chunks
  DROP CONSTRAINT page_content_upload_chunks_bytes_check;

ALTER TABLE page_content_upload_chunks
  ADD CONSTRAINT page_content_upload_chunks_bytes_check
  CHECK (octet_length(bytes) > 0 AND octet_length(bytes) <= 262144);
