// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// One transport contract for initialization, tool schemas and prepared prompts.
// Uploading stages bytes only; the consumer still owns validation/publication.
function fileUploadGuidance(kind = "page", prefix = "") {
  const target = kind === "data" ? " kind='data'" : "";
  const consumer = kind === "data" ? "update_page_data_upload" : "deploy_page_upload";
  return `Compute the exact file byte count and lowercase SHA-256. Prefer ${prefix}create_upload_ticket${target}: ` +
    `PUT the file to the returned URL from your shell, then call ${prefix}${consumer} with upload_id. ` +
    `If outbound HTTP is unavailable, use ${prefix}start_page_upload${target} followed by ordered ` +
    `${prefix}append_page_upload base64 chunks within returned max_chunk_bytes and next_sequence, then the same consumer. ` +
    `Retry an interrupted append with the same sequence and bytes; do not cancel and restart unchanged content. ` +
    `Verify the returned byte count/hash before consuming the completed upload.`;
}

module.exports = { fileUploadGuidance };
