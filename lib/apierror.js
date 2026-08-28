// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/apierror.js — a thrown error that carries the HTTP status the API layer
// should return. Business logic (lib/versions.js, lib/tokens.js) throws these;
// the /api/v1 error handler turns them into `{ error }` JSON with `.status`.
// Anything that is NOT an ApiError is an unexpected bug → 500 (never leaked).

class ApiError extends Error {
  constructor(status, message, code, details) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code || undefined;
    this.details = details === undefined ? undefined : details;
  }
}

// Convenience constructors for the statuses we use.
const badRequest = (msg, code, details) => new ApiError(400, msg, code, details);
const unauthorized = (msg, code, details) => new ApiError(401, msg, code, details);
const forbidden = (msg, code, details) => new ApiError(403, msg, code, details);
const notFound = (msg, code, details) => new ApiError(404, msg, code, details);
const conflict = (msg, code, details) => new ApiError(409, msg, code, details);

// fromDbError — translate the bounded-wait Postgres failures (lib/db.js pool
// timeouts, issue #10) into clean ApiErrors instead of opaque 500s. Returns
// null for anything else (a real bug stays a 500 and gets logged).
function fromDbError(err) {
  if (!err) return null;
  switch (err.code) {
    case "55P03": // lock_not_available (lock_timeout while waiting on FOR UPDATE)
      return new ApiError(503, "the page is busy with another update — retry shortly", "db_lock_timeout");
    case "57014": // query_canceled (statement_timeout)
      return new ApiError(503, "database query timed out — retry shortly", "db_statement_timeout");
    case "57P05": // idle_in_transaction_session_timeout killed the session
    case "ECONNREFUSED":
    case "ETIMEDOUT":
      return new ApiError(503, "database unavailable — retry shortly", "db_unavailable");
    default:
      // connectionTimeoutMillis rejections are plain Errors with no code; pg
      // words them two ways depending on the acquisition path.
      if (/timeout exceeded when trying to connect|connection terminated due to connection timeout/i.test(err.message || "")) {
        return new ApiError(503, "database unavailable — retry shortly", "db_unavailable");
      }
      return null;
  }
}

module.exports = { ApiError, badRequest, unauthorized, forbidden, notFound, conflict, fromDbError };
