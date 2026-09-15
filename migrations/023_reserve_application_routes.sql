-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- Refuse rollout when an existing active page holds a newly reserved segment.
-- This never renames, deletes or changes an immutable version. Soft-deleted
-- rows remain historical and cannot be restored into a reserved namespace.
DO $$
DECLARE
  collisions TEXT;
BEGIN
  SELECT string_agg(slug, ', ' ORDER BY slug) INTO collisions
    FROM (SELECT slug FROM pages
           WHERE deleted_at IS NULL
             AND string_to_array(slug, '/') && ARRAY['raw-template', 'preflight', 'edit-token', 'readyz']
           ORDER BY slug LIMIT 20) AS conflicting_pages;
  IF collisions IS NOT NULL THEN
    RAISE EXCEPTION 'active page slugs conflict with application routes: %', collisions
      USING HINT = 'Keep the current release running. Copy the affected source to an ordinary new slug, verify its published content and access settings, update shared links, then soft-delete the old page and retry. No page has been changed by this check; restore the old page using the old release if you need to undo the move.';
  END IF;
END $$;
