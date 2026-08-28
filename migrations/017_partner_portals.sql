-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 017_partner_portals.sql — partner portals: one credential, a curated set of
-- dashboards, and the membership a page switcher reads.
--
-- Today a client credential is per PAGE (pages.password_hash) and the session
-- cookie is page-scoped three independent ways (cookie name, payload `pid`, and
-- the single name serve() reads), so a partner with six dashboards needs six
-- passwords and six links, and no page can name its siblings. A portal is the
-- missing noun: a named SET of pages behind ONE shared password, from which the
-- sibling list can be derived per request.
--
-- Three properties here are deliberate and load-bearing:
--
--   * password_hash is NOT NULL and non-empty. lib/pagecookie.credentialDigest
--     digests `cred:` || (hash || ''), so a portal with a NULL — or empty —
--     hash would share its credential digest with every staff-only page, and a
--     staff /view session cookie would then verify as a portal session. The
--     column constraints make that unrepresentable rather than merely unlikely,
--     and are why this table has no "clear the password" path at all.
--   * Membership is a join table from day one. A page may sit in several
--     portals (co-branded work is visible to both audiences), which a nullable
--     portals column on pages could not express and could not be widened to
--     later without a breaking migration.
--   * Membership carries its own label and order. pages.title is agent-settable
--     (versions.setTitle takes a bearer token), so a portal that displayed
--     pages.title would let an agent rewrite what a partner reads; and without
--     an explicit order the macro view a partner should land on is buried
--     alphabetically among the campaign dashboards.
--
-- Nothing here changes serving. No route reads either table yet: the portal
-- session, the serve() predicate, and the in-page switcher land in later PRs.

CREATE TABLE page_portals (
  id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  -- ONE url-safe segment, because the partner URL is <content-host>/portal/<slug>
  -- and a nested slug would change the shape of that route. Same charset as a
  -- pages.slug segment and page_templates.name.
  slug          TEXT NOT NULL CHECK (
    slug ~ '^[a-z0-9]+([-_][a-z0-9]+)*$' AND char_length(slug) BETWEEN 1 AND 64
  ),
  name          TEXT NOT NULL CHECK (
    name = btrim(name) AND char_length(name) BETWEEN 1 AND 100
  ),
  -- scrypt, via lib/pagecookie.hashPassword. Never NULL, never '' — see above.
  password_hash TEXT NOT NULL CHECK (password_hash <> ''),
  -- The macro view a partner should land on. Advisory by design: reads resolve
  -- it THROUGH the membership rows, so a home page that is later removed from
  -- the portal or soft-deleted is inert rather than wrong. ON DELETE SET NULL
  -- covers a future hard purge.
  home_page_id  BIGINT REFERENCES pages(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at    TIMESTAMPTZ
);

-- Same partial-unique pattern as pages (migrations/002) and page_templates: a
-- retired portal keeps its row, its members, and its audit trail, and frees its
-- slug for reuse. Retiring is also how a portal credential is taken out of
-- service — every read filters `deleted_at IS NULL`.
CREATE UNIQUE INDEX page_portals_slug_live_uidx
  ON page_portals (slug) WHERE deleted_at IS NULL;

-- The name is what an admin picks a portal by in a list ("Fabrikam — All" next to
-- "Fabrikam — West"), so case-only variants are duplicates, as with workspaces.
CREATE UNIQUE INDEX page_portals_name_live_uidx
  ON page_portals ((lower(name))) WHERE deleted_at IS NULL;

CREATE TABLE page_portal_members (
  portal_id  BIGINT NOT NULL REFERENCES page_portals(id) ON DELETE CASCADE,
  page_id    BIGINT NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
  -- The partner-facing title. NULL means "fall back to pages.title".
  label      TEXT CHECK (
    label IS NULL OR (label = btrim(label) AND char_length(label) BETWEEN 1 AND 200)
  ),
  sort_order INTEGER NOT NULL DEFAULT 0 CHECK (sort_order BETWEEN 0 AND 9999),
  added_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (portal_id, page_id)
);

-- The serving-side question is "which portals contain this page?", asked once
-- per client page view as soon as the serve() predicate lands, so the page side
-- is indexed too. (portal_id is covered by the primary key.)
CREATE INDEX page_portal_members_page_idx ON page_portal_members (page_id);
