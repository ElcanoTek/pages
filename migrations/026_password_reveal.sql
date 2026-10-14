-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- 026_password_reveal.sql — a staff-readable copy of each client password
-- (lib/passwordvault.js).
--
-- password_hash stays the only thing the content-host gate checks. The new
-- column holds an AES-256-GCM copy, keyed from PAGE_COOKIE_SECRET and bound to
-- the password_hash it was sealed with, so the admin shell can show the current
-- password. NULL means Pages has no copy yet: the password predates this column,
-- or the page has none. A copy appears when the password is next set or when a
-- client next signs in with it.
--
-- Nullable columns only, so a predecessor release runs unchanged against this
-- schema. A password it sets leaves a copy bound to the old hash, which no
-- longer opens and is reported as unavailable rather than shown.
ALTER TABLE pages ADD COLUMN password_ciphertext TEXT;
ALTER TABLE page_portals ADD COLUMN password_ciphertext TEXT;
