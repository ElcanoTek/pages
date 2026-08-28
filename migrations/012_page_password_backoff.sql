-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 012_page_password_backoff.sql — per-page brute-force backoff state.
--
-- The content host's password form has a per-IP rate limit, but a distributed
-- guessing run gets a fresh budget per address. This table carries one shared
-- failure counter per page (lib/passwordgate.js) so the progressive 401 delay
-- applies to the page no matter how many IPs attack it. It is NOT a lockout:
-- a hard per-page lock would let an attacker deny the page to its real
-- viewers. Rows clear on a successful unlock and reset after a quiet window,
-- and die with their page (pages are only ever soft-deleted, but a deliberate
-- hard purge must not strand state).

CREATE TABLE page_password_failures (
  page_id      BIGINT PRIMARY KEY REFERENCES pages(id) ON DELETE CASCADE,
  fail_count   INTEGER NOT NULL CHECK (fail_count > 0),
  window_start TIMESTAMPTZ NOT NULL,
  last_fail_at TIMESTAMPTZ NOT NULL
);
