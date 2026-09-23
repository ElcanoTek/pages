-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 024_page_upload_chunk_ceiling_1mib.sql — widen the staged-upload chunk CHECK
-- to the new top of the PAGE_UPLOAD_MAX_CHUNK_BYTES clamp (1 MiB).
--
-- 013 set this to 256 KiB because every chunk byte was base64 the calling model
-- emitted. A client that reads a workspace_file reference host-side never puts
-- the bytes in model output, but each append is still one model tool call that
-- re-sends the whole conversation: a 990 KB data file was 21 appends at the
-- 48 KiB default. lib/page-uploads.js now lets an operator raise the ceiling to
-- 1 MiB (≈1.4 MB of base64, inside the default 2 MB request body); the default
-- stays 48 KiB.
--
-- Same rule as 013: the constraint sits at the top of the clamp, not at the
-- default. The DB guards against an absurd row; the application owns the
-- operational limit. Widening a CHECK needs no table rewrite and no backfill:
-- every existing chunk already satisfies it, and a predecessor release (clamped
-- at 256 KiB) only ever writes rows that satisfy it too.

ALTER TABLE page_content_upload_chunks
  DROP CONSTRAINT page_content_upload_chunks_bytes_check;

ALTER TABLE page_content_upload_chunks
  ADD CONSTRAINT page_content_upload_chunks_bytes_check
  CHECK (octet_length(bytes) > 0 AND octet_length(bytes) <= 1048576);
