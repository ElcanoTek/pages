// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const { badRequest } = require("./apierror");

// Namespace contract for content routes, admin screens and page API actions.
// Any segment is reserved so nested slugs cannot be interpreted as actions.
// Creation and restoration enforce this; legacy reads retain their URLs.
// New reservations need a rollout collision check before their routes ship.
const RESERVED_SLUG_SEGMENTS = Object.freeze([
  "raw", "raw-template", "assets", "healthz", "readyz", "welcome",
  "versions", "publish", "rollback", "approve", "reject", "approval",
  "password", "title", "theme", "disable", "enable", "delete", "restore",
  "preview-token", "edit-token", "deploy-source", "preflight", "workspace",
  "templates", "portal", "portals",
]);
const reserved = new Set(RESERVED_SLUG_SEGMENTS);

function assertSlugNotReserved(slug) {
  for (const segment of slug.split("/")) {
    if (reserved.has(segment)) {
      throw badRequest(`slug segment '${segment}' is reserved (collides with a route)`, "reserved_slug");
    }
  }
}

module.exports = { RESERVED_SLUG_SEGMENTS, assertSlugNotReserved };
