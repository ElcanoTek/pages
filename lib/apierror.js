"use strict";
// lib/apierror.js — a thrown error that carries the HTTP status the API layer
// should return. Business logic (lib/versions.js, lib/tokens.js) throws these;
// the /api/v1 error handler turns them into `{ error }` JSON with `.status`.
// Anything that is NOT an ApiError is an unexpected bug → 500 (never leaked).

class ApiError extends Error {
  constructor(status, message, code) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code || undefined;
  }
}

// Convenience constructors for the statuses we use.
const badRequest = (msg, code) => new ApiError(400, msg, code);
const unauthorized = (msg, code) => new ApiError(401, msg, code);
const forbidden = (msg, code) => new ApiError(403, msg, code);
const notFound = (msg, code) => new ApiError(404, msg, code);
const conflict = (msg, code) => new ApiError(409, msg, code);

module.exports = { ApiError, badRequest, unauthorized, forbidden, notFound, conflict };
