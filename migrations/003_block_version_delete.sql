-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 003_block_version_delete.sql — finish the append-only guarantee (PLAN §5).
--
-- 001 installed only the BEFORE UPDATE half of the promised enforcement
-- ("block any UPDATE to html/content_sha256/page_id, and any DELETE"). The
-- only thing stopping a DELETE was the pages_pubver_fk RESTRICT constraint,
-- which protects exactly one row per page (the currently published one) —
-- drafts, the pending queue, rejected rows kept for audit, and every rollback
-- target were silently deletable.
--
-- This trigger blocks EVERY delete, including the ON DELETE CASCADE from a
-- pages hard-delete. That is deliberate: pages are only ever soft-deleted in
-- app code, and version history is kept forever (PLAN §13 retention).
-- Hard-purging a page requires dropping this trigger first, on purpose.

CREATE FUNCTION page_versions_no_delete() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'page_versions is append-only: DELETE is blocked (id=%). Drop trigger page_versions_no_delete to hard-purge deliberately.', OLD.id;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER page_versions_no_delete
  BEFORE DELETE ON page_versions
  FOR EACH ROW EXECUTE FUNCTION page_versions_no_delete();
