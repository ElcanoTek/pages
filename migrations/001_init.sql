-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 001_init.sql — Elcano Pages initial schema (PLAN.md §5).
-- pointer-is-truth versioning: "live" = pages.published_version_id; status
-- tracks the review lifecycle; page_versions content is append-only (trigger).

CREATE TYPE version_status AS ENUM ('draft', 'pending', 'approved', 'rejected');
CREATE TYPE render_mode    AS ENUM ('themed', 'raw');

-- Themes are curated by Elcano (not agents): a small token override + logo.
CREATE TABLE themes (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name         TEXT UNIQUE NOT NULL,
  override_css TEXT NOT NULL DEFAULT '',
  logo_sha256  TEXT,
  default_mode TEXT NOT NULL DEFAULT 'system',   -- light | dark | system (flag contract)
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The default theme. pages.theme_id IS NULL also means "Flag default"; this
-- explicit row lets set_theme reference it by name and anchors the default.
INSERT INTO themes (name, override_css, default_mode) VALUES ('flag', '', 'system');

CREATE TABLE pages (
  id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  slug                 TEXT UNIQUE NOT NULL,
  title                TEXT NOT NULL DEFAULT '',
  client_id            TEXT,
  theme_id             BIGINT REFERENCES themes(id),   -- NULL ⇒ Flag default
  password_hash        TEXT,                            -- scrypt (lib/pagecookie); NULL = Elcano-only
  require_approval     BOOLEAN NOT NULL DEFAULT false,
  disabled             BOOLEAN NOT NULL DEFAULT false,  -- takedown kill switch
  published_version_id BIGINT,                          -- FK added below; the ONLY truth for "live"
  created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at           TIMESTAMPTZ
);

CREATE TABLE page_versions (
  id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  page_id        BIGINT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
  html           TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  status         version_status NOT NULL DEFAULT 'draft',
  render_mode    render_mode NOT NULL DEFAULT 'themed',
  author         TEXT NOT NULL,
  source         TEXT NOT NULL DEFAULT 'api',           -- api | mcp | admin
  note           TEXT,
  reviewed_by    TEXT,
  reviewed_at    TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX page_versions_page_created_idx ON page_versions (page_id, created_at DESC);

-- The pointer. Deferrable so a publish can move it within the same txn that
-- inserts a version; RESTRICT so a live version can't be deleted out from under it.
ALTER TABLE pages ADD CONSTRAINT pages_pubver_fk
  FOREIGN KEY (published_version_id) REFERENCES page_versions(id)
  ON DELETE RESTRICT DEFERRABLE INITIALLY IMMEDIATE;

-- Content immutability: a version's content identity never changes after insert.
-- status/reviewed_* may mutate (the review lifecycle); html/sha/page_id/render_mode
-- may not. Every edit is a NEW row, so the table is append-only on content.
CREATE FUNCTION page_versions_content_immutable() RETURNS trigger AS $$
BEGIN
  IF NEW.html           IS DISTINCT FROM OLD.html
  OR NEW.content_sha256 IS DISTINCT FROM OLD.content_sha256
  OR NEW.page_id        IS DISTINCT FROM OLD.page_id
  OR NEW.render_mode    IS DISTINCT FROM OLD.render_mode
  OR NEW.created_at     IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'page_versions content is immutable (id=%)', OLD.id;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
CREATE TRIGGER page_versions_content_immutable
  BEFORE UPDATE ON page_versions
  FOR EACH ROW EXECUTE FUNCTION page_versions_content_immutable();

CREATE TABLE assets (
  sha256       TEXT PRIMARY KEY,
  content_type TEXT NOT NULL,
  bytes        BIGINT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
  id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  label        TEXT NOT NULL,
  prefix       TEXT NOT NULL,
  token_hash   TEXT NOT NULL,           -- HMAC-SHA256(token, API_TOKEN_PEPPER)
  scope        TEXT NOT NULL DEFAULT 'deploy',
  last_used_at TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at   TIMESTAMPTZ
);

CREATE TABLE preview_links (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  page_id    BIGINT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
  version_id BIGINT NOT NULL REFERENCES page_versions(id) ON DELETE CASCADE,
  token_hash TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  revoked_at TIMESTAMPTZ,
  created_by TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE audit_log (
  id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor      TEXT NOT NULL,
  actor_type TEXT NOT NULL,             -- user | agent | system
  action     TEXT NOT NULL,
  page_id    BIGINT,
  version_id BIGINT,
  ip         TEXT,
  metadata   JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
