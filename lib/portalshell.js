// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/portalshell.js — the partner portal screen (/admin/portals).
//
// Same shape as templateshell/adminshell: a server-rendered bootstrap that hands
// the browser a CSRF token and the content origin, then lets
// shell-assets/portals.js drive the admin JSON API.
//
// This screen is the ONLY way a portal is ever changed. lib/portals.js refuses any
// actor that is not a human admin and there is no MCP or bearer equivalent, so
// "who may see which dashboards" is a decision that exists nowhere but here.

const shell = require("./shell");

function render(csrfToken, email, contentOrigin) {
  return `<!doctype html>
<html lang="en">
<head>
${shell.head({
    title: shell.pageTitle("Partner portals"),
    bootstrap: { csrf: csrfToken, admin: email, contentOrigin: contentOrigin || "" },
    scripts: ["/shell-assets/portals.js"],
  })}
</head>
<body>
${shell.header(email)}
<main class="shell detail-shell">
  <div class="context-bar">
    ${shell.sectionNav("portals")}
  </div>
  <div id="app" class="screen-stack">
    ${shell.loading("Loading partner portals")}
  </div>
</main>
</body>
</html>`;
}

module.exports = { render };
