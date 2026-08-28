-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 004_workspaces.sql — one-level admin workspaces for organizing pages.
-- Existing pages intentionally remain NULL ("Ungrouped"), so upgrades need no
-- data migration and every page stays visible on the admin index.

CREATE TABLE workspaces (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT workspaces_name_valid CHECK (
    name = btrim(name) AND char_length(name) BETWEEN 1 AND 100
  )
);

-- Names are human-facing identifiers in the admin UI. Treat case-only
-- variants as duplicates so "Acme" and "acme" cannot become two folders.
CREATE UNIQUE INDEX workspaces_name_lower_uidx ON workspaces ((lower(name)));

ALTER TABLE pages
  ADD COLUMN workspace_id BIGINT REFERENCES workspaces(id) ON DELETE SET NULL;

CREATE INDEX pages_workspace_idx ON pages (workspace_id) WHERE deleted_at IS NULL;
