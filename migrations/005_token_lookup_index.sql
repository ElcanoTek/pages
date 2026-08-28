-- SPDX-License-Identifier: BUSL-1.1
-- Copyright (c) 2026 ElcanoTek, Inc.
-- Bearer verification is on every REST/MCP request. Make the HMAC token lookup
-- indexed at the database layer as the token table grows; uniqueness is
-- also a defense-in-depth guarantee against ambiguous credential rows.
CREATE UNIQUE INDEX api_tokens_token_hash_uidx ON api_tokens (token_hash);
