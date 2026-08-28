-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 020_page_refresh_checks.sql — let a page record that it was looked at and
-- deliberately not updated.
--
-- Everything Pages stores about freshness today is a by-product of a WRITE:
-- source_as_of and refreshed_at exist only because a version was created. So a
-- refresh that ran correctly and decided to publish nothing — the source had no
-- new day, the tool it needed was unavailable, the schema had drifted — leaves
-- no trace at all. On 2026-08-17, ten of seventeen managed pages were three
-- days to six weeks stale, and "we are checking daily and the upstream is dead"
-- was indistinguishable from "nobody has run this in three weeks". Those two
-- need different humans to act.
--
-- Last-check-wins rather than an append-only log: the question this answers is
-- "when did anyone last look, and what did they see", which is a single current
-- fact about the page. The audit_log already keeps the history, and a per-run
-- table would grow one row per page per day forever to serve a read that only
-- ever wants the newest.
--
-- These columns say nothing about whether a page is OVERDUE. Pages retired its
-- scheduler deliberately and does not know any page's expected cadence.

ALTER TABLE pages
  ADD COLUMN last_check_at TIMESTAMPTZ,
  ADD COLUMN last_check_outcome TEXT,
  ADD COLUMN last_check_detail TEXT,
  ADD COLUMN last_check_source_as_of TIMESTAMPTZ;

-- A stamp with no outcome cannot be interpreted, and an outcome with no stamp
-- cannot be aged — so the pair travels together or not at all. The vocabulary is
-- closed on purpose: these values are read by humans scanning a list and by
-- consumers deciding what to alert on, and a free-text field would become
-- neither. Mirrored in lib/mcp-tools.js (RefreshCheckOutcome).
ALTER TABLE pages
  ADD CONSTRAINT pages_last_check_all_or_none CHECK (
    (last_check_at IS NULL AND last_check_outcome IS NULL
      AND last_check_detail IS NULL AND last_check_source_as_of IS NULL)
    OR
    (last_check_at IS NOT NULL
      AND last_check_outcome IN ('updated', 'source_not_updated', 'source_unreachable', 'blocked', 'failed'))
  );

-- The estate-wide read this exists for: "which pages has nobody looked at?",
-- answered without scanning every row's versions. Partial, because a page that
-- has never recorded a check is found by the IS NULL side of the question.
CREATE INDEX pages_last_check_idx
  ON pages (last_check_at DESC)
  WHERE deleted_at IS NULL AND last_check_at IS NOT NULL;
