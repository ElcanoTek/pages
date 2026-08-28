-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 002_soft_delete.sql — make soft-delete actually usable (PLAN.md §5).
--
-- pages.deleted_at existed from 001 and every read/serve query already filters
-- `deleted_at IS NULL`, but nothing ever set it and the table-level UNIQUE(slug)
-- constraint would have permanently stranded a slug after delete. Swap the full
-- UNIQUE for a PARTIAL unique index scoped to live rows, so a deleted page keeps
-- its historical row (and audit trail) while its slug is freed for reuse.

-- `slug TEXT UNIQUE` created the implicit constraint pages_slug_key.
ALTER TABLE pages DROP CONSTRAINT IF EXISTS pages_slug_key;

-- Uniqueness now applies only among live (non-deleted) pages.
CREATE UNIQUE INDEX IF NOT EXISTS pages_slug_live_uniq
  ON pages (slug) WHERE deleted_at IS NULL;
