// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/uploadticket.js — the one endpoint that accepts page bytes over the wire
// instead of through a model's tool arguments.
//
// WHY. Everywhere else, an agent hands Pages HTML by emitting it: inline `html`
// or base64 `append_page_upload` chunks. That means a 65 KB dashboard costs
// ~25k output tokens the model must produce without a single slipped character,
// and a 300 KB one is effectively undeployable — we watched turns time out
// mid-upload and an agent shrink a client's dashboard to fit the chunk budget.
//
// An agent's sandbox already has outbound HTTP. So `create_upload_ticket` mints
// a one-shot credential, the sandbox PUTs the file here in a single curl, and
// the model only ever touches a URL and an opaque handle.
//
// THIS IS THE ONLY BEARER PATH ON THE DASHBOARD HOST THAT IS NOT AN AGENT TOKEN,
// so it is deliberately the weakest credential in the system (migrations/014):
//   • write-only  — stages bytes; cannot deploy, publish, read, or list.
//   • content-pinned — total_bytes and content_sha256 were fixed by the
//     authenticated agent when the ticket was minted, so the only byte string
//     this will ever accept is the one already committed to. A stolen ticket
//     cannot substitute different content; at worst it re-sends the same file.
//   • one upload, minutes-long TTL, spent on first use.
// Deploying the staged bytes still requires the real agent token.

const express = require("express");
const pageUploads = require("./page-uploads");
const { ApiError, unauthorized, fromDbError } = require("./apierror");
const { DASHBOARD_ORIGIN } = require("./csp");

const router = express.Router();

function bearer(req) {
  const m = /^Bearer\s+(.+)$/i.exec(req.get("authorization") || "");
  return m ? m[1].trim() : "";
}

// Authenticate on the HEADER before reading the body — an invalid ticket must
// not make the server buffer 2 MiB first. Same ordering rule as /mcp.
function requireTicket(req, res, next) {
  const ticket = bearer(req);
  if (!ticket) {
    res.set("WWW-Authenticate", 'Bearer realm="pages-upload"');
    return next(unauthorized("an upload ticket is required", "upload_ticket_missing"));
  }
  Promise.resolve(pageUploads.checkTicket(ticket, req.params.upload_id))
    .then(() => {
      req.uploadTicket = ticket;
      next();
    })
    .catch(next);
}

// Raw body, capped at the same ceiling as a staged upload. `type: () => true`
// because a sandbox curl sends whatever Content-Type it likes (or none) — the
// bytes are opaque to us and validated by length + SHA-256, not by header.
const rawBody = express.raw({ type: () => true, limit: pageUploads.MAX_UPLOAD_BYTES });

router.put("/:upload_id", requireTicket, rawBody, (req, res, next) => {
  Promise.resolve(pageUploads.putTicketContent(req.uploadTicket, req.params.upload_id, req.body))
    .then((state) => res.status(200).json(state))
    .catch(next);
});

// A ticket is for exactly one PUT of one document; nothing else is offered here.
router.all("/:upload_id", (_req, res) =>
  res.status(405).set("Allow", "PUT").json({ error: "use PUT to send the page bytes", code: "method_not_allowed" })
);

// eslint-disable-next-line no-unused-vars
router.use((err, req, res, _next) => {
  if (err && err.type === "entity.too.large") {
    return res.status(413).json({
      error: `page content must be at most ${pageUploads.MAX_UPLOAD_BYTES} bytes`,
      code: "upload_size_invalid",
    });
  }
  const mapped = err instanceof ApiError ? err : fromDbError(err);
  if (mapped) return res.status(mapped.status).json({ error: mapped.message, code: mapped.code });
  console.error("upload ticket error:", err.stack || err.message);
  res.status(500).json({ error: "internal error" });
});

// The absolute URL handed to the agent, so it never reconstructs one from a
// hostname and gets the zone wrong.
function uploadUrl(uploadId) {
  return `${DASHBOARD_ORIGIN}/upload/${uploadId}`;
}

module.exports = { router, uploadUrl };
