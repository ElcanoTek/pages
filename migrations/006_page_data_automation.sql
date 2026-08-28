-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- Immutable structured-data metadata, exact-slug automation grants, and
-- durable token attribution for Pages-owned dashboard refreshes.

ALTER TABLE page_versions
  ADD COLUMN data_sha256 TEXT,
  ADD COLUMN data_template_sha256 TEXT,
  ADD COLUMN source_as_of TIMESTAMPTZ,
  ADD COLUMN refreshed_at TIMESTAMPTZ;

ALTER TABLE page_versions
  ADD CONSTRAINT page_versions_data_metadata_all_or_none CHECK (
    (data_sha256 IS NULL AND data_template_sha256 IS NULL AND source_as_of IS NULL AND refreshed_at IS NULL)
    OR
    (data_sha256 ~ '^[0-9a-f]{64}$'
      AND data_template_sha256 ~ '^[0-9a-f]{64}$'
      AND source_as_of IS NOT NULL
      AND refreshed_at IS NOT NULL)
  );

CREATE INDEX page_versions_data_dedupe_idx
  ON page_versions (page_id, data_template_sha256, data_sha256, source_as_of DESC, id DESC)
  WHERE data_sha256 IS NOT NULL;

CREATE TABLE api_token_page_grants (
  token_id BIGINT NOT NULL REFERENCES api_tokens(id) ON DELETE CASCADE,
  slug TEXT NOT NULL CHECK (
    slug ~ '^[a-z0-9]+([-_][a-z0-9]+)*(/[a-z0-9]+([-_][a-z0-9]+)*)*$'
  ),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (token_id, slug)
);

CREATE INDEX api_token_page_grants_slug_idx ON api_token_page_grants (slug, token_id);

ALTER TABLE audit_log
  ADD COLUMN token_id BIGINT REFERENCES api_tokens(id) ON DELETE SET NULL;

CREATE INDEX audit_log_token_created_idx ON audit_log (token_id, created_at DESC)
  WHERE token_id IS NOT NULL;

-- Extend the existing content-immutability trigger to cover structured-data
-- identity. Status/review fields remain the only mutable version fields.
CREATE OR REPLACE FUNCTION page_versions_content_immutable() RETURNS trigger AS $$
BEGIN
  IF NEW.html                 IS DISTINCT FROM OLD.html
  OR NEW.content_sha256       IS DISTINCT FROM OLD.content_sha256
  OR NEW.page_id              IS DISTINCT FROM OLD.page_id
  OR NEW.render_mode          IS DISTINCT FROM OLD.render_mode
  OR NEW.data_sha256          IS DISTINCT FROM OLD.data_sha256
  OR NEW.data_template_sha256 IS DISTINCT FROM OLD.data_template_sha256
  OR NEW.source_as_of         IS DISTINCT FROM OLD.source_as_of
  OR NEW.refreshed_at         IS DISTINCT FROM OLD.refreshed_at
  OR NEW.created_at           IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'page_versions content is immutable (id=%)', OLD.id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
