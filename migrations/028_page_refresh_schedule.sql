-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 028_page_refresh_schedule.sql — tell Pages how often a page is MEANT to
-- refresh, and who answers for it.
--
-- 020 taught Pages when a page was last looked at, and said nothing about
-- whether that was late: Pages did not know any page's expected cadence. So a
-- dashboard whose daily refresh stopped read exactly like one refreshed on
-- time, and the people trading on it found out by looking at every dashboard.
-- In two weeks of production runs, a third did not publish — a quarter found
-- no new source data (some sources frozen for weeks) and the rest were stopped
-- by the refresh agent's own rules — and nothing in Pages said which pages
-- those were.
--
-- Refreshes are still scheduled elsewhere; Pages runs nothing on this. It is
-- the expectation a page is judged against (internal/versions/refreshhealth.go):
-- a standard five-field cron, interpreted in UTC, validated by internal/cron
-- before it is written. NULL means no expectation, and no judgement — the
-- neutral behaviour every page had before this migration.
--
-- refresh_owner is the email of the person a missed refresh should reach.
--
-- Nullable columns only, so a predecessor release runs unchanged against this
-- schema and simply never reads them.

ALTER TABLE pages
  ADD COLUMN refresh_schedule TEXT,
  ADD COLUMN refresh_owner TEXT;

-- The application owns the grammar; the database only refuses rows no
-- version of it could have written.
ALTER TABLE pages
  ADD CONSTRAINT pages_refresh_schedule_shape CHECK (
    refresh_schedule IS NULL
    OR (length(refresh_schedule) BETWEEN 9 AND 100 AND refresh_schedule !~ '[^0-9*/, -]')
  ),
  ADD CONSTRAINT pages_refresh_owner_shape CHECK (
    refresh_owner IS NULL OR (length(refresh_owner) BETWEEN 3 AND 254 AND position('@' IN refresh_owner) > 1)
  );
