-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 022_page_upload_attempts.sql — remember that a caller has been round this
-- loop before, so the third attempt can say something the first could not.
--
-- One conversation (Training-materials-page-creation-options-9fa1f766) started
-- ten chunked uploads for the same page, CANCELLED EIGHT, spent over 10 million
-- tokens, hit the turn ceiling twice, and shipped nothing; seven partial HTML
-- files were recovered off disk instead, four of them missing their closing
-- </html>. In 452 history entries create_upload_ticket — allowlisted, and the
-- path that keeps the bytes out of the model's context entirely — was never
-- called once.
--
-- Cancelling deletes the upload row, so nothing survived to notice the pattern
-- with. Each attempt looked like the first one. This is the smallest state that
-- makes the loop visible from inside it.
--
-- Advisory, never a refusal: an environment that genuinely cannot make outbound
-- HTTP requests has to keep using the chunked path, and blocking it there would
-- turn a bad turn into an impossible one.

CREATE TABLE page_upload_attempts (
  token_id     BIGINT      NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
  target_kind  TEXT        NOT NULL CHECK (target_kind IN ('page', 'template', 'data')),
  -- Same dual reading as page_content_uploads.slug: a slug, or a template name.
  slug         TEXT        NOT NULL,
  starts       INTEGER     NOT NULL DEFAULT 0,
  cancels      INTEGER     NOT NULL DEFAULT 0,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (token_id, target_kind, slug)
);

-- Counts are about the session in front of you, not the account's history: a
-- page uploaded by hand once a month must not accumulate into a permanent
-- scolding. Rows older than the window are reaped on the next start.
CREATE INDEX page_upload_attempts_stale_idx ON page_upload_attempts (updated_at);
