-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- Pages-owned self-service refresh definitions and durable MOC dispatch runs.
-- Runtime secrets remain outside Pages: refreshes reference one pre-provisioned
-- data_update token by ID and automatically maintain its exact-slug grants.

CREATE TABLE page_refreshes (
  id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  page_id               BIGINT NOT NULL UNIQUE REFERENCES pages(id) ON DELETE CASCADE,
  status                TEXT NOT NULL CHECK (status IN ('active', 'paused')),
  daily_at_utc          TEXT NOT NULL CHECK (daily_at_utc ~ '^(?:[01][0-9]|2[0-3]):[0-5][0-9]$'),
  workflow              JSONB NOT NULL CHECK (jsonb_typeof(workflow) = 'object'),
  prompt                TEXT NOT NULL,
  schema_sha256         TEXT NOT NULL CHECK (schema_sha256 ~ '^[0-9a-f]{64}$'),
  publish               BOOLEAN NOT NULL DEFAULT true,
  target_node_name      TEXT NOT NULL,
  runtime_token_id      BIGINT NOT NULL REFERENCES api_tokens(id) ON DELETE RESTRICT,
  next_run_at           TIMESTAMPTZ NOT NULL,
  last_dispatched_at    TIMESTAMPTZ,
  last_error            TEXT,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX page_refreshes_due_idx
  ON page_refreshes (next_run_at, id)
  WHERE status = 'active';

CREATE TABLE page_refresh_runs (
  id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  refresh_id            BIGINT NOT NULL REFERENCES page_refreshes(id) ON DELETE CASCADE,
  kind                  TEXT NOT NULL CHECK (kind IN ('scheduled', 'manual')),
  scheduled_for         TIMESTAMPTZ NOT NULL,
  prompt                TEXT NOT NULL,
  target_node_name      TEXT NOT NULL,
  idempotency_key       TEXT NOT NULL UNIQUE,
  status                TEXT NOT NULL CHECK (status IN ('queued', 'dispatching', 'dispatched', 'error', 'cancelled')),
  attempts              INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  next_attempt_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  dispatch_lease_until  TIMESTAMPTZ,
  moc_task_id           UUID,
  last_error            TEXT,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (refresh_id, kind, scheduled_for)
);

CREATE INDEX page_refresh_runs_dispatch_idx
  ON page_refresh_runs (next_attempt_at, id)
  WHERE status IN ('queued', 'dispatching', 'error');
