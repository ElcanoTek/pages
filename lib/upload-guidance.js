// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { dataLimitGuidance } = require("./data-limits");

// One transport contract for initialization, tool schemas and prepared prompts.
// Uploading stages bytes only; the consumer still owns validation/publication.
function fileUploadGuidance(kind = "page", prefix = "") {
  const target = kind === "data" ? " kind='data'" : "";
  const consumer = kind === "data" ? "update_page_data_upload" : "deploy_page_upload";
  return (kind === "data" ? dataLimitGuidance() : "") + `Compute the exact file byte count and lowercase SHA-256. If the client's append_page_upload schema accepts a workspace file reference, prefer ${prefix}start_page_upload${target} then ordered ${prefix}append_page_upload calls with chunk_base64={workspace_file: relative_path, sha256: whole_file_sha256, offset: accepted_raw_bytes, length: next_raw_chunk_bytes}. The client reads and encodes exact bytes; do not print or transcribe base64. Use returned max_chunk_bytes and next_sequence, then call ${prefix}${consumer}. Otherwise prefer ${prefix}create_upload_ticket${target}: ` +
    `PUT the file to the returned URL from your shell, then call ${prefix}${consumer} with upload_id. ` +
    `If outbound HTTP is unavailable, use ${prefix}start_page_upload${target} followed by ordered ` +
    `${prefix}append_page_upload base64 chunks within returned max_chunk_bytes and next_sequence, then the same consumer. ` +
    `Retry an interrupted append with the same sequence and bytes; do not cancel and restart unchanged content. ` +
    `Verify the returned byte count/hash before consuming the completed upload.`;
}

module.exports = { fileUploadGuidance };
