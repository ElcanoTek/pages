-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 025_central_auth.sql — state for PAGES_AUTH_MODE=central (lib/central-auth.js):
-- sign-in through a password-mode Auth deployment's application handoff.
--
-- New tables only, so a predecessor release runs unchanged against this
-- schema; the default auth mode never reads them. Times are Unix seconds,
-- matching the claims they are compared with.

-- Who may use this Pages deployment. Maintained by Auth's signed access
-- events and by `pages access grant|revoke`. Disabling a row ends its
-- sessions in the same transaction (lib/central-auth.js revokeAccess).
CREATE TABLE central_access (
  email      TEXT PRIMARY KEY,
  enabled    BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Opaque Pages sessions, stored only as the SHA-256 of the cookie value.
CREATE TABLE central_sessions (
  token_hash          TEXT PRIMARY KEY,
  subject             TEXT NOT NULL,
  email               TEXT NOT NULL,
  created_at          BIGINT NOT NULL,
  last_seen_at        BIGINT NOT NULL,
  idle_expires_at     BIGINT NOT NULL,
  absolute_expires_at BIGINT NOT NULL,
  revoked_at          BIGINT,
  CHECK (idle_expires_at <= absolute_expires_at)
);
CREATE INDEX central_sessions_subject_idx ON central_sessions (subject);
CREATE INDEX central_sessions_email_idx ON central_sessions (email);

-- Back-channel logout replay protection, keyed by the token's jti.
-- received_ms orders a logout against a sign-in in flight (createSession).
CREATE TABLE central_revocation_events (
  event_id    TEXT PRIMARY KEY,
  issuer      TEXT NOT NULL,
  subject     TEXT NOT NULL,
  issued_at   BIGINT NOT NULL,
  received_at BIGINT NOT NULL,
  received_ms BIGINT NOT NULL
);
CREATE INDEX central_revocation_events_subject_idx ON central_revocation_events (subject, received_ms);

-- The last access event applied per Auth subject; older versions are ignored.
CREATE TABLE central_access_provisioning (
  issuer      TEXT NOT NULL,
  subject     TEXT NOT NULL,
  email       TEXT NOT NULL,
  version     BIGINT NOT NULL CHECK (version > 0),
  allowed     BOOLEAN NOT NULL,
  event_id    TEXT NOT NULL,
  issued_at   BIGINT NOT NULL,
  received_at BIGINT NOT NULL,
  PRIMARY KEY (issuer, subject)
);
