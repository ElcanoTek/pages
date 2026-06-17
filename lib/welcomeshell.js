"use strict";
// lib/welcomeshell.js — the Flag-themed admin LANDING page (GET /admin and
// /admin/welcome). Unlike adminshell.js (which is per-slug), this is the index:
// a thin bootstrap that links Flag tokens/fonts, exposes the content origin for
// "view live" links, and loads welcome.js, which lists every page via the
// read-only admin API (GET /api/v1/admin/pages). No mutations here, so no CSRF.

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// render(email, contentOrigin, opts) → full HTML document string.
// opts: { csrf, compose } — csrf token + whether the (dev-only) compose panel is on.
function render(email, contentOrigin, opts = {}) {
  const sprite = "/shell-assets/flag/icons/core-icons.svg";
  return `<!doctype html>
<html lang="en" data-theme="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pages admin · welcome</title>
<link rel="stylesheet" href="/shell-assets/flag/fonts/dubai-fonts.css">
<link rel="stylesheet" href="/shell-assets/flag/tokens/design-tokens.css">
<link rel="stylesheet" href="/shell-assets/shell.css">
<script src="/shell-assets/flag/theme/theme-controller.js" defer></script>
<!-- Bootstrap data as a NON-executed JSON island. An inline script element would
     be blocked by the shell CSP (script-src 'self', no unsafe-inline/nonce); a
     type=application/json block is data, not script, so it's served and read
     by welcome.js without violating CSP. -->
<script type="application/json" id="pages-bootstrap">${JSON.stringify({ admin: email, contentOrigin: contentOrigin || "", csrf: opts.csrf || "", compose: !!opts.compose }).replace(/</g, "\\u003c")}</script>
<script src="/shell-assets/welcome.js" defer></script>
</head>
<body>
<header class="app-header">
  <a class="brand" href="/admin" aria-label="Elcano Pages">
    <span class="brand__mark" aria-hidden="true">
      <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7.5 12 3l8 4.5"/><path d="M4 12l8 4.5 8-4.5"/><path d="M4 16.5 12 21l8-4.5"/></svg>
    </span>
    <span class="brand__name">Pages</span>
    <span class="brand__tag">Elcano</span>
  </a>
  <div class="app-header__actions">
    <button class="btn btn-icon theme-toggle" type="button" data-flag-theme-toggle aria-label="Toggle light / dark theme" aria-pressed="true" title="Toggle theme">
      <svg class="icon-inline theme-toggle__icon theme-toggle__icon--sun" aria-hidden="true" width="17" height="17"><use href="${sprite}#sun"></use></svg>
      <svg class="icon-inline theme-toggle__icon theme-toggle__icon--moon" aria-hidden="true" width="17" height="17"><use href="${sprite}#moon"></use></svg>
    </button>
    <span class="user"><span class="user__email">${esc(email)}</span><span class="user__sep">·</span><a href="/logout">Sign out</a></span>
  </div>
</header>
<main id="app" class="wrap">
  <p class="muted" id="loading">Loading…</p>
</main>
</body>
</html>`;
}

module.exports = { render };
