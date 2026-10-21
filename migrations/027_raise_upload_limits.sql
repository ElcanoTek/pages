-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 027_raise_upload_limits.sql — widen the staged-upload CHECKs to the Go
-- server's ceilings: a staged file of up to 64 MiB (the hard cap on
-- PAGES_UPLOAD_MAX_BYTES, default 32 MiB) and a chunk of up to 8 MiB (the top
-- of the PAGE_UPLOAD_MAX_CHUNK_BYTES range, default 1 MiB).
--
-- 009 sized total_bytes at the Node release's 2 MiB staged-file constant and
-- 024 the chunk at its 1 MiB clamp. Those numbers came from clients whose
-- model emitted every byte as base64; a client that reads workspace_file
-- references host-side (or PUTs to an upload ticket) is not bound by them, and
-- ordinary multi-megabyte dashboards and data files were refused.
--
-- Same rule as 013 and 024: each constraint sits at the top of what the
-- application can be configured to allow, not at the default. The DB guards
-- against an absurd row; the application owns the operational limit.
--
-- Backward-compatible: widening a CHECK needs no rewrite and no backfill. Every
-- existing row already satisfies the wider bound, and a predecessor release
-- (2 MiB files, 1 MiB chunks) only ever writes rows that satisfy it too. The
-- new constraints are added NOT VALID and validated separately so the scan of
-- existing rows does not hold an exclusive lock.

ALTER TABLE page_content_uploads
  DROP CONSTRAINT page_content_uploads_total_bytes_check;

ALTER TABLE page_content_uploads
  ADD CONSTRAINT page_content_uploads_total_bytes_check
  CHECK (total_bytes > 0 AND total_bytes <= 67108864) NOT VALID;

ALTER TABLE page_content_uploads
  VALIDATE CONSTRAINT page_content_uploads_total_bytes_check;

ALTER TABLE page_content_upload_chunks
  DROP CONSTRAINT page_content_upload_chunks_bytes_check;

ALTER TABLE page_content_upload_chunks
  ADD CONSTRAINT page_content_upload_chunks_bytes_check
  CHECK (octet_length(bytes) > 0 AND octet_length(bytes) <= 8388608) NOT VALID;

ALTER TABLE page_content_upload_chunks
  VALIDATE CONSTRAINT page_content_upload_chunks_bytes_check;
