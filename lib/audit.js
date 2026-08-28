// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/audit.js — append a row to audit_log. Always called with an existing pg
// client inside the SAME transaction as the mutation it records (PLAN.md §5,
// §10): an action and its audit row commit or roll back together, so the log
// can never disagree with the data.

// write(client, { actor, actorType, action, pageId?, versionId?, tokenId?, ip?, metadata? })
//   actorType: 'user' | 'agent' | 'system'
//   action:    deploy | publish | approve | reject | rollback | create_page | ...
async function write(
  client,
  { actor, actorType, action, pageId = null, versionId = null, tokenId = null, ip = null, metadata = null }
) {
  await client.query(
    `INSERT INTO audit_log (actor, actor_type, action, page_id, version_id, token_id, ip, metadata)
     VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
    [actor, actorType, action, pageId, versionId, tokenId, ip, metadata ? JSON.stringify(metadata) : null]
  );
}

module.exports = { write };
