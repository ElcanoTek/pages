-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 029_page_version_coverage_gaps.sql — what Pages found missing inside each
-- version's managed data.
--
-- Refresh health (028) judges whether refreshes run and whether source_as_of
-- advances. A page can pass both and still be wrong. One trader dashboard
-- showed a small fraction of a campaign's revenue for four weeks: the refresh
-- read one source for only some of the deals, so those deals' fields from that
-- source stopped while the same deals' fields from another source kept
-- arriving. Every refresh succeeded and source_as_of advanced. Only the rows
-- themselves showed it (internal/pagedata/coveragegaps.go).
--
-- coverage_gaps holds that finding for the version: {"v": <rule version>,
-- "gaps": [...]}. It is derived from the version's immutable data, so it is
-- computed once, when the version is written, and then reused. It is a cache,
-- not content: deliberately outside page_versions_content_immutable's list. A
-- version written before this migration starts NULL and is filled the first
-- time its page's health is read while the page has a refresh schedule (only
-- those are judged). A value whose "v" is not the current rule version is
-- recomputed the same way. A page with no managed data stores an empty list.
--
-- A nullable column only, so a predecessor release runs unchanged against this
-- schema. It never reads the column, and versions it writes stay NULL until a
-- read fills them.

ALTER TABLE page_versions ADD COLUMN coverage_gaps JSONB;

ALTER TABLE page_versions
  ADD CONSTRAINT page_versions_coverage_gaps_shape
  CHECK (coverage_gaps IS NULL OR jsonb_typeof(coverage_gaps) = 'object');
