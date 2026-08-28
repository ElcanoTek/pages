-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- Durable, token-bound staging for dashboard HTML that is too large to fit
-- safely in one model-generated MCP tool call. The upload handle is explicit;
-- no transport session affinity or client-specific file access is required.

CREATE TABLE page_content_uploads (
  id                    UUID PRIMARY KEY,
  token_id              BIGINT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
  slug                  TEXT NOT NULL,
  total_bytes           INTEGER NOT NULL CHECK (total_bytes > 0 AND total_bytes <= 2097152),
  content_sha256        TEXT NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
  bytes_received        INTEGER NOT NULL DEFAULT 0 CHECK (bytes_received >= 0 AND bytes_received <= total_bytes),
  next_sequence         INTEGER NOT NULL DEFAULT 0 CHECK (next_sequence >= 0),
  commit_key            TEXT CHECK (commit_key IS NULL OR commit_key ~ '^[0-9a-f]{64}$'),
  commit_result         JSONB CHECK (commit_result IS NULL OR jsonb_typeof(commit_result) = 'object'),
  committed_at          TIMESTAMPTZ,
  expires_at            TIMESTAMPTZ NOT NULL,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (
    (commit_key IS NULL) = (commit_result IS NULL)
    AND (commit_result IS NULL) = (committed_at IS NULL)
  )
);

CREATE INDEX page_content_uploads_expiry_idx ON page_content_uploads (expires_at);
CREATE INDEX page_content_uploads_active_token_idx
  ON page_content_uploads (token_id, created_at)
  WHERE committed_at IS NULL;

CREATE TABLE page_content_upload_chunks (
  upload_id             UUID NOT NULL REFERENCES page_content_uploads(id) ON DELETE CASCADE,
  sequence              INTEGER NOT NULL CHECK (sequence >= 0),
  bytes                 BYTEA NOT NULL CHECK (octet_length(bytes) > 0 AND octet_length(bytes) <= 12288),
  content_sha256        TEXT NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (upload_id, sequence)
);
