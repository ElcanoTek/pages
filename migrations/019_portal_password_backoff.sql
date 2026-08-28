-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 019_portal_password_backoff.sql — per-portal brute-force backoff state.
--
-- The mirror of page_password_failures (migrations/012), for the same reason: the
-- per-IP rate limit stops a single-source guessing run, but a distributed one gets
-- a fresh budget per address, so the shared counter is what bounds the total guess
-- rate against one secret. Same curve (lib/passwordgate.js), same "delay, never
-- lock out" rule — a hard lock would let an attacker deny a partner their portal.
--
-- A portal needs its OWN counter rather than borrowing its members': a portal
-- password is worth more than any single page password (it opens the whole set),
-- and its arrival URL is not the pages' URLs. The counter is keyed by portal so
-- that every door which tests a portal password charges the same budget —
-- otherwise an attacker who can submit at the portal index AND at each member
-- page's form would get N parallel budgets against a secret worth N times as much.
-- The member-page door lands with the serve predicate; the table is keyed for it
-- from the start.

CREATE TABLE portal_password_failures (
  portal_id    BIGINT PRIMARY KEY REFERENCES page_portals(id) ON DELETE CASCADE,
  fail_count   INTEGER NOT NULL CHECK (fail_count > 0),
  window_start TIMESTAMPTZ NOT NULL,
  last_fail_at TIMESTAMPTZ NOT NULL
);
