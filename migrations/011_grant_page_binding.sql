-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 011_grant_page_binding.sql — bind exact-slug token grants to page identity.
--
-- Grants were slug-text only: after a soft delete freed a slug and a new page
-- took it, the old grant silently authorized the scoped data_update token
-- against the NEW page — a principal the operator never granted (the same
-- delete→recreate slug-reuse hazard the disabled takedown already blocks).
--
-- A grant now records the page row it is bound to:
--   • existing grants bind to the live page currently holding their slug;
--   • grants minted for a not-yet-existing page (page_id NULL) bind lazily to
--     the first live page that holds the slug (verified in lib/tokens.js);
--   • once bound, a grant NEVER re-binds: delete→recreate yields a new page
--     id and the grant goes stale until an operator re-grants, while
--     delete→restore keeps the same row id and keeps working.
ALTER TABLE api_token_page_grants
  ADD COLUMN page_id BIGINT REFERENCES pages(id) ON DELETE CASCADE;

UPDATE api_token_page_grants g
   SET page_id = p.id
  FROM pages p
 WHERE p.slug = g.slug
   AND p.deleted_at IS NULL;

CREATE INDEX api_token_page_grants_page_idx ON api_token_page_grants (page_id);
