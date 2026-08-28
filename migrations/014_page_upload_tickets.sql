-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 014_page_upload_tickets.sql — out-of-band content upload for staged uploads.
--
-- Until now the ONLY way bytes reached Pages was through an agent's own tool
-- arguments, which means every byte of a dashboard was base64 the model emitted
-- token by token. A 65 KB page cost ~25k output tokens per attempt and turns
-- routinely died mid-upload; a 300 KB page was simply not deployable.
--
-- A ticket lets the agent's SANDBOX send the file directly (one curl) while the
-- model only ever handles a URL and an opaque handle. The ticket is deliberately
-- the weakest credential in the system:
--
--   * write-only  — it can stage bytes and nothing else. It cannot deploy,
--                   publish, read, list, or touch any live page.
--   * content-pinned — the row already carries total_bytes + content_sha256, so
--                   the ONLY byte string it will ever accept is the one the
--                   authenticated agent already committed to. A stolen ticket
--                   cannot substitute different content.
--   * single upload, minutes-long TTL, and revoked the moment it is used.
--
-- Stored as HMAC-SHA256(ticket, API_TOKEN_PEPPER) exactly like an api_tokens
-- row, so a database dump does not yield usable tickets.

ALTER TABLE page_content_uploads
  ADD COLUMN ticket_hash       TEXT,
  ADD COLUMN ticket_expires_at TIMESTAMPTZ,
  ADD COLUMN ticket_used_at    TIMESTAMPTZ;

-- Lookup is by hash on every PUT; partial because most uploads have no ticket.
CREATE UNIQUE INDEX page_content_uploads_ticket_hash_idx
  ON page_content_uploads (ticket_hash)
  WHERE ticket_hash IS NOT NULL;
