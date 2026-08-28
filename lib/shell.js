// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Shared server-rendered shell fragments for Pages-owned admin screens. The
// browser behavior stays in public/shell-assets; these helpers keep the Flag
// header, approved branding, asset order, and loading state identical.

const SPRITE = "/shell-assets/flag/icons/core-icons.svg";
const MARK = "/shell-assets/flag/logos/elcano-mark-primary.svg";
const FAVICON = "/shell-assets/flag/logos/elcano-mark-favicon.svg";

function esc(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;",
    "<": "&lt;",
    ">": "&gt;",
    '"': "&quot;",
    "'": "&#39;",
  })[char]);
}

function jsonIsland(value) {
  return JSON.stringify(value).replace(/</g, "\\u003c");
}

function head({ title, bootstrap, scripts = [] }) {
  return `<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="icon" href="${FAVICON}" type="image/svg+xml">
<title>${esc(title)}</title>
<link rel="stylesheet" href="/shell-assets/flag/fonts/fonts.css">
<link rel="stylesheet" href="/shell-assets/flag/tokens/design-tokens.css">
<link rel="stylesheet" href="/shell-assets/shell.css">
<script src="/shell-assets/flag/theme/theme-controller.js"></script>
<script type="application/json" id="pages-bootstrap">${jsonIsland(bootstrap)}</script>
<script src="/shell-assets/primitives.js" defer></script>
${scripts.map((src) => `<script src="${esc(src)}" defer></script>`).join("\n")}`;
}

function icon(name, className = "icon-inline") {
  return `<svg class="${esc(className)}" aria-hidden="true"><use href="${SPRITE}#${esc(name)}"></use></svg>`;
}

function header(email) {
  return `<header class="page-header">
  <div class="ds-app-header shell-header">
    <a class="ds-app-header__brand brand-link" href="/admin" aria-label="Pages home">
      <img class="ds-app-header__mark" src="${MARK}" alt="">
      <span class="ds-app-header__text">
        <span class="ds-app-header__title">Pages</span>
      </span>
    </a>
    <div class="ds-app-header__actions">
      <span class="session-user" title="${esc(email)}">${esc(email)}</span>
      <button class="icon-action theme-toggle" type="button" data-flag-theme-toggle aria-label="Switch theme" aria-pressed="false" title="Change color theme">
        ${icon("sun", "icon-inline theme-toggle__icon theme-toggle__icon--sun")}
        ${icon("moon", "icon-inline theme-toggle__icon theme-toggle__icon--moon")}
      </button>
      <!-- The label is hidden on narrow viewports, so the accessible name has to
           come from aria-label or the link becomes an unnamed icon. -->
      <a class="btn btn-ghost header-signout" href="/logout" aria-label="Sign out">${icon("logout")}<span>Sign out</span></a>
    </div>
  </div>
</header>`;
}

// Every admin screen's document title is "<what this screen is> · Pages", so a
// row of browser tabs is scannable by its first word and the product name never
// leads. The screen name is the same string its section tab and its <h1> use.
function pageTitle(screen) {
  return `${screen} · Pages`;
}

// Top-level section navigation, shared so both sections render the identical
// markup. Each tab names its destination the way that page's own <h1> names
// itself — a tab whose label is echoed by an overline above a different heading
// reads as a second header rather than as navigation.
const SECTIONS = [
  { key: "pages", href: "/admin", label: "Client pages" },
  { key: "templates", href: "/admin/templates", label: "Template library" },
  { key: "portals", href: "/admin/portals", label: "Partner portals" },
];

function sectionNav(current) {
  const tabs = SECTIONS.map((section) => {
    const active = section.key === current ? ' aria-current="page"' : "";
    return `    <a class="context-tab" href="${section.href}"${active}>${esc(section.label)}</a>`;
  });
  return `<nav class="context-nav" aria-label="Pages sections">
${tabs.join("\n")}
  </nav>`;
}

// The detail screen's second row. The trail is server-rendered from the slug so
// there is something correct before the payload lands; admin.js then upgrades the
// workspace and the human title in place. Marking the last crumb aria-current
// makes the trail a location, not a set of links.
function breadcrumb(slug) {
  return `<nav class="breadcrumb" aria-label="Breadcrumb">
    <ol class="breadcrumb__list">
      <li class="breadcrumb__item"><a href="/admin">Client pages</a></li>
      <li class="breadcrumb__item breadcrumb__item--workspace" id="breadcrumb-workspace" hidden></li>
      <li class="breadcrumb__item"><span id="breadcrumb-current" aria-current="page">/${esc(slug)}</span></li>
    </ol>
  </nav>`;
}

function loading(label = "Loading pages") {
  return `<div class="state-panel state-panel--loading" role="status" aria-live="polite">
  <span class="spinner spinner--panel" aria-hidden="true"></span>
  <p>${esc(label)}…</p>
</div>`;
}

module.exports = { esc, head, header, icon, loading, sectionNav, pageTitle, breadcrumb };
