// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const shell = require("./shell");

// render(email, contentOrigin, opts) → full HTML document string.
// opts: { csrf, compose } — csrf token + whether the (dev-only) compose panel is on.
function render(email, contentOrigin, opts = {}) {
  return `<!doctype html>
<html lang="en">
<head>
${shell.head({
    title: shell.pageTitle("Client pages"),
    bootstrap: { admin: email, contentOrigin: contentOrigin || "", csrf: opts.csrf || "", compose: !!opts.compose },
    scripts: ["/shell-assets/welcome.js"],
  })}
</head>
<body>
${shell.header(email)}
<main class="shell">
  <div class="context-bar">
    ${shell.sectionNav("pages")}
  </div>
  <div id="app" class="screen-stack">
    ${shell.loading("Loading page operations")}
  </div>
</main>
</body>
</html>`;
}

module.exports = { render };
