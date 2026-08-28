-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- MOC task creation has no receiver-side idempotency contract. Keep the
-- durable key local to Pages and claim each queued run at most once.

ALTER TABLE page_refresh_runs
  RENAME COLUMN idempotency_key TO run_key;

ALTER TABLE page_refresh_runs
  RENAME CONSTRAINT page_refresh_runs_idempotency_key_key TO page_refresh_runs_run_key_key;

DROP INDEX page_refresh_runs_dispatch_idx;

CREATE INDEX page_refresh_runs_dispatch_idx
  ON page_refresh_runs (next_attempt_at, id)
  WHERE status = 'queued';
