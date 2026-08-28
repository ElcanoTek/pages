-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 015_page_templates.sql — templates: one stored design, many pages.
--
-- Until now the only way to build a second dashboard from an existing design was
-- to send the whole design again. A 62 KB campaign dashboard in which ~1.4 KB
-- (the CONFIG object) is all that differs per campaign cost either ~21k output
-- tokens of base64 or a hand-edited copy of the file, and every page stored its
-- own copy of bytes that were meant to be identical. A design fix then had to be
-- repeated per page, so the copies drifted.
--
-- A template is the design half, stored once and versioned. A page built from it
-- carries only its own config + data. Serving does not change at all: the page
-- still stores its complete materialized HTML in page_versions, so
-- pointer-is-truth, rollback, /raw and the content host are untouched, and a
-- template can never break a page that is already live.

CREATE TABLE page_templates (
  id                 BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name               TEXT NOT NULL CHECK (
    name ~ '^[a-z0-9]+([-_][a-z0-9]+)*$'
  ),
  title              TEXT NOT NULL DEFAULT '',
  description        TEXT NOT NULL DEFAULT '',
  current_version_id BIGINT,                      -- FK below; what a create uses
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at         TIMESTAMPTZ
);

-- Same partial-unique trick as pages: a soft-deleted template releases its name.
CREATE UNIQUE INDEX page_templates_live_name_idx
  ON page_templates (name) WHERE deleted_at IS NULL;

-- Append-only revisions. The HTML is the truth for a template's contract; the
-- two schema columns are an extracted cache so listing templates and answering
-- "what does this design need?" never re-parses a 62 KB document.
CREATE TABLE page_template_versions (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  template_id          BIGINT NOT NULL REFERENCES page_templates(id) ON DELETE CASCADE,
  revision             INTEGER NOT NULL CHECK (revision > 0),
  html                 TEXT NOT NULL,
  content_sha256       TEXT NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
  config_schema        JSONB NOT NULL CHECK (jsonb_typeof(config_schema) = 'object'),
  data_schema          JSONB NOT NULL CHECK (jsonb_typeof(data_schema) = 'object'),
  config_schema_sha256 TEXT NOT NULL CHECK (config_schema_sha256 ~ '^[0-9a-f]{64}$'),
  data_schema_sha256   TEXT NOT NULL CHECK (data_schema_sha256 ~ '^[0-9a-f]{64}$'),
  reference_config     JSONB NOT NULL CHECK (jsonb_typeof(reference_config) = 'object'),
  author               TEXT NOT NULL,
  source               TEXT NOT NULL DEFAULT 'mcp',   -- api | mcp | admin
  note                 TEXT,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (template_id, revision)
);

CREATE INDEX page_template_versions_template_idx
  ON page_template_versions (template_id, revision DESC);

ALTER TABLE page_templates ADD CONSTRAINT page_templates_current_fk
  FOREIGN KEY (current_version_id) REFERENCES page_template_versions(id)
  ON DELETE RESTRICT DEFERRABLE INITIALLY IMMEDIATE;

-- A revision's content identity never changes after insert; a design fix is a
-- NEW revision. Mirrors page_versions_content_immutable so the two halves of the
-- system have the same guarantee, and so a page's pinned revision can never be
-- rewritten under it.
CREATE FUNCTION page_template_versions_immutable() RETURNS trigger AS $$
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
  OR NEW.created_at           IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'page_template_versions content is immutable (id=%)', OLD.id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER page_template_versions_immutable
  BEFORE UPDATE ON page_template_versions
  FOR EACH ROW EXECUTE FUNCTION page_template_versions_immutable();

-- Provenance on the page side: which template revision produced these bytes, and
-- the identity of the config they were produced with. This is what makes "who is
-- still on revision 1" answerable without parsing every page's HTML. Nullable —
-- pages authored directly have no template — and all-or-none so a half-written
-- binding cannot exist.
ALTER TABLE page_versions
  ADD COLUMN template_version_id BIGINT REFERENCES page_template_versions(id),
  ADD COLUMN config_sha256 TEXT;

ALTER TABLE page_versions
  ADD CONSTRAINT page_versions_template_binding_all_or_none CHECK (
    (template_version_id IS NULL AND config_sha256 IS NULL)
    OR
    (template_version_id IS NOT NULL AND config_sha256 ~ '^[0-9a-f]{64}$')
  );

CREATE INDEX page_versions_template_idx
  ON page_versions (template_version_id, page_id)
  WHERE template_version_id IS NOT NULL;

-- Extend content immutability to the template binding. A version's bytes and the
-- revision that produced them are one fact; status/review stay the only mutable
-- fields (cf. migrations/006, which added the structured-data identity).
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
  OR NEW.template_version_id  IS DISTINCT FROM OLD.template_version_id
  OR NEW.config_sha256        IS DISTINCT FROM OLD.config_sha256
  OR NEW.created_at           IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'page_versions content is immutable (id=%)', OLD.id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- Staged uploads can now target a template instead of a page. The existing slug
-- column carries the template name in that case; target_kind is what keeps the
-- two apart, so deploy_page_upload can never publish a template skeleton as a
-- live client page and register_template_upload can never consume page bytes.
ALTER TABLE page_content_uploads
  ADD COLUMN target_kind TEXT NOT NULL DEFAULT 'page'
    CHECK (target_kind IN ('page', 'template'));
