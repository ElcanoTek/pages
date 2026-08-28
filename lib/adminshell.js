// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

const shell = require("./shell");

// render(slug, csrfToken, email, contentOrigin) → full HTML document string.
// contentOrigin (e.g. https://elcano-pages.com) lets admin.js show the live
// client URL in the password / client-access card.
function render(slug, csrfToken, email, contentOrigin) {
  const s = shell.esc(slug);
  return `<!doctype html>
<html lang="en">
<head>
${shell.head({
    title: shell.pageTitle(`/${slug}`),
    bootstrap: { slug, csrf: csrfToken, admin: email, contentOrigin: contentOrigin || "" },
    scripts: ["/shell-assets/page-switcher.js", "/shell-assets/admin.js"],
  })}
</head>
<body>
${shell.header(email)}
<!-- Named because the sandboxed preview renders a whole document inside this one,
     and two unnamed <main> landmarks are indistinguishable in the accessibility
     tree (axe: landmark-unique). -->
<main class="shell detail-shell" aria-label="Page review">
  <div class="context-bar">
    ${shell.sectionNav("pages")}
  </div>
  <div class="detail-bar">
    ${shell.breadcrumb(slug)}
    <nav class="page-switcher" id="page-switcher" aria-label="Page navigation" aria-busy="true">
      <button class="icon-action page-switcher__step" id="page-switcher-prev" type="button" aria-label="Previous page" disabled><span aria-hidden="true">←</span></button>
      <label class="sr-only" for="page-switcher-select">Switch admin page</label>
      <select class="page-switcher__select" id="page-switcher-select" aria-describedby="page-switcher-count" disabled>
        <option>Loading pages…</option>
      </select>
      <span class="page-switcher__count" id="page-switcher-count" role="status" aria-live="polite">Loading…</span>
      <button class="icon-action page-switcher__step" id="page-switcher-next" type="button" aria-label="Next page" disabled><span aria-hidden="true">→</span></button>
    </nav>
  </div>
  <div id="app" class="screen-stack">
    ${shell.loading(`Loading /${s}`)}
  </div>
</main>
</body>
</html>`;
}

module.exports = { render };
