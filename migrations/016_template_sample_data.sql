-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 016 — preview-only example data for a template revision.
--
-- A template ships an EMPTY #pages-data so that no page built from it inherits
-- rows. The consequence is that previewing a design shows a skeleton, which is
-- close to useless when the point of the library is deciding whether a design
-- is the right one. A revision may therefore carry an example dataset, extracted
-- from an optional #pages-data-example block at registration.
--
-- It is preview-only by construction, not by convention: page-data.js deletes
-- that block from every materialization, so neither a page nor a rendered
-- preview can carry these bytes as page data. Nullable — most templates will not
-- have one, and every revision registered before this migration has none.

ALTER TABLE page_template_versions
  ADD COLUMN sample_data JSONB;

-- Same reasoning as every other content column on this table: a revision's
-- bytes never change after insert, so an example dataset edit is a NEW revision.
-- Extending the existing function rather than adding a second trigger keeps the
-- whole immutability rule readable in one place (see migrations/006 for the
-- same CREATE OR REPLACE pattern on page_versions).
CREATE OR REPLACE FUNCTION page_template_versions_immutable() RETURNS trigger AS $$
BEGIN
  IF NEW.html                 IS DISTINCT FROM OLD.html
  OR NEW.content_sha256       IS DISTINCT FROM OLD.content_sha256
  OR NEW.template_id          IS DISTINCT FROM OLD.template_id
  OR NEW.revision             IS DISTINCT FROM OLD.revision
  OR NEW.config_schema        IS DISTINCT FROM OLD.config_schema
  OR NEW.data_schema          IS DISTINCT FROM OLD.data_schema
  OR NEW.config_schema_sha256 IS DISTINCT FROM OLD.config_schema_sha256
  OR NEW.data_schema_sha256   IS DISTINCT FROM OLD.data_schema_sha256
  OR NEW.reference_config     IS DISTINCT FROM OLD.reference_config
  OR NEW.sample_data          IS DISTINCT FROM OLD.sample_data
  OR NEW.created_at           IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'page_template_versions content is immutable (id=%)', OLD.id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
