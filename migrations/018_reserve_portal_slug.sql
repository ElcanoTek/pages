-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 018_reserve_portal_slug.sql — refuse to hand /portal/* to the router while a
-- live page still answers there.
--
-- <content-host>/portal/<portal-slug> is about to become the partner entry point,
-- matched inside contentview.serve()'s wildcard BEFORE the page lookup, so that a
-- page created later can never seize a partner's bookmarked URL. The mirror image
-- of that guarantee is that a page whose slug carries a `portal` segment stops
-- being reachable the moment the route lands.
--
-- lib/versions.js reserves the segment from here on, but reservation is enforced
-- at page CREATION, so it says nothing about a row that already exists — and
-- nothing in the application would ever notice one. This is the check that cannot
-- be skipped: it runs inside the migration transaction, on every environment, and
-- scripts/update.sh migrates staging ahead of the atomic swap, so it fires before
-- the route can ship. Nothing in this repo uses the word today; this exists so
-- "we checked production" is a property of the deploy rather than of someone's
-- memory.
--
-- A soft-deleted `portal/*` row is deliberately left alone: it serves nothing,
-- and versions.restorePage now refuses to bring a reserved slug back to life.

DO $$
DECLARE
  colliding TEXT;
BEGIN
  SELECT string_agg(slug, ', ' ORDER BY slug)
    INTO colliding
    FROM pages
   WHERE deleted_at IS NULL
     AND 'portal' = ANY (string_to_array(slug, '/'));

  IF colliding IS NOT NULL THEN
    RAISE EXCEPTION
      'live page(s) hold the reserved portal route and would become unreachable: %. Redeploy that content at a slug with no "portal" segment, confirm it serves, then soft-delete the old page.',
      colliding
      USING HINT = 'There is no slug-rename verb: deploy the same version at the new slug first, so the client is never without a page.';
  END IF;
END $$;
