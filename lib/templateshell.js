// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/templateshell.js — the template library shell (/admin/templates).
//
// Same shape as adminshell: a server-rendered bootstrap that hands the browser a
// CSRF token and the content origin, then lets shell-assets/templates.js drive
// the CSRF-protected admin JSON API. No template bytes are rendered here — a
// design is untrusted HTML and previews only on the content host, under the
// sandbox+CSP, through a signed short-TTL URL.

const shell = require("./shell");

function render(csrfToken, email, contentOrigin) {
  return `<!doctype html>
<html lang="en">
<head>
${shell.head({
    title: shell.pageTitle("Template library"),
    bootstrap: { csrf: csrfToken, admin: email, contentOrigin: contentOrigin || "" },
    scripts: ["/shell-assets/templates.js"],
  })}
</head>
<body>
${shell.header(email)}
<!-- Named for the same reason as the page detail's: the sandboxed template
     preview renders its own <main> inside this document. -->
<main class="shell detail-shell" aria-label="Template library">
  <div class="context-bar">
    ${shell.sectionNav("templates")}
  </div>
  <div id="app" class="screen-stack">
    ${shell.loading("Loading templates")}
  </div>
</main>
</body>
</html>`;
}

module.exports = { render };
