// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const bytes = require("bytes");
const { badRequest } = require("./apierror");

function positiveBytes(value, fallback) {
  const number = Number(value);
  return Number.isSafeInteger(number) && number > 0 ? number : fallback;
}

// Resolve once at startup, and pass this same value to Express. HTTP includes
// the complete request, whereas the other budgets count specific JSON values.
const MAX_REQUEST_BYTES = positiveBytes(bytes.parse(process.env.MAX_HTML_BYTES || "2mb"), 2 * 1024 * 1024);
const MAX_DATA_BYTES = positiveBytes(process.env.PAGES_DATA_MAX_BYTES, 1024 * 1024);
const MAX_INLINE_DATA_BYTES = positiveBytes(process.env.PAGES_MCP_MAX_INLINE_DATA_BYTES, 1500000);
const MAX_UPLOAD_BYTES = 2 * 1024 * 1024;

function escapedJson(value) {
  return JSON.stringify(value)
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/&/g, "\\u0026")
    .replace(/\u2028/g, "\\u2028")
    .replace(/\u2029/g, "\\u2029");
}

const ORDINARY_TIMESTAMP = "2000-01-01T00:00:00.000Z";
function envelopeOverhead(sourceAsOf = ORDINARY_TIMESTAMP, refreshedAt = ORDINARY_TIMESTAMP) {
  return Buffer.byteLength(escapedJson({
    contract_version: 1, refreshed_at: refreshedAt, source_as_of: sourceAsOf, data: {},
  }), "utf8") - 2;
}

// New normalized timestamps are at least 24 characters; extended ISO years
// can be longer. Early staging checks use a minimum, actual writes count all bytes.
const MIN_ENVELOPE_OVERHEAD_BYTES = envelopeOverhead();
const DATA_LIMITS = Object.freeze({
  max_payload_bytes: MAX_DATA_BYTES,
  max_envelope_bytes: MAX_DATA_BYTES,
  max_inline_bytes: MAX_INLINE_DATA_BYTES,
  max_staged_bytes: MAX_UPLOAD_BYTES,
  max_request_bytes: MAX_REQUEST_BYTES,
  min_envelope_overhead_bytes: MIN_ENVELOPE_OVERHEAD_BYTES,
});

function assertDomainSize(size, boundary, errorCode = "data_validation_failed") {
  if (size <= MAX_DATA_BYTES) return;
  throw badRequest(
    `Managed data ${boundary} is ${size} bytes; its limit is ${MAX_DATA_BYTES} bytes. ` +
    "Changing upload transport cannot help. Keep the complete source file and correct the data contract " +
    "or ask the operator to review capacity. Do NOT split, sample or summarize the payload to fit.",
    errorCode,
    { size_limit: boundary, bytes: size, max_bytes: MAX_DATA_BYTES, changing_transport_can_help: false, limits: DATA_LIMITS }
  );
}

function measureData(data) {
  return {
    payload_bytes: Buffer.byteLength(JSON.stringify(data), "utf8"),
    escaped_payload_bytes: Buffer.byteLength(escapedJson(data), "utf8"),
  };
}

function assertStagedDataSize(kind, size) {
  if (size === undefined) return;
  if (kind !== "data") throw badRequest("data_size is only valid for kind 'data'", "data_size_invalid");
  if (!size || typeof size !== "object" || Array.isArray(size) ||
      Object.keys(size).length !== 2 ||
      !Number.isSafeInteger(size.payload_bytes) || size.payload_bytes < 0 ||
      !Number.isSafeInteger(size.escaped_payload_bytes) || size.escaped_payload_bytes < size.payload_bytes) {
    throw badRequest("data_size requires nonnegative safe integer payload_bytes and escaped_payload_bytes >= payload_bytes", "data_size_invalid");
  }
  assertDomainSize(size.payload_bytes, "payload");
  assertDomainSize(size.escaped_payload_bytes + MIN_ENVELOPE_OVERHEAD_BYTES, "envelope");
}

function dataLimitGuidance() {
  return `Managed-data limits in UTF-8 bytes: compact payload ${MAX_DATA_BYTES}; stored HTML-escaped ` +
    `envelope ${MAX_DATA_BYTES} including timestamps and at least ${MIN_ENVELOPE_OVERHEAD_BYTES} metadata bytes; ` +
    `inline payload transport ${MAX_INLINE_DATA_BYTES}; staged raw file ${MAX_UPLOAD_BYTES}; complete JSON HTTP request ` +
    `${MAX_REQUEST_BYTES} including RPC fields and expect. All applicable limits must fit. ` +
    "Staging bypasses only the inline transport limit, never the data/envelope limits. " +
    "Before staging kind='data', optionally supply advisory data_size: payload_bytes is UTF-8 byteLength(JSON.stringify(data)); " +
    "escaped_payload_bytes counts the same JSON after replacing <, >, &, U+2028 and U+2029 with six-character Unicode escapes. " +
    "Measure the parsed object, not pretty-printed file bytes; total_bytes and SHA-256 still describe the exact file. " +
    "The server validates received content independently; measurements do not guarantee acceptance. ";
}

module.exports = {
  MAX_REQUEST_BYTES, MAX_DATA_BYTES, MAX_INLINE_DATA_BYTES, MAX_UPLOAD_BYTES,
  MIN_ENVELOPE_OVERHEAD_BYTES, DATA_LIMITS, escapedJson, envelopeOverhead,
  assertDomainSize, measureData, assertStagedDataSize, dataLimitGuidance,
};
