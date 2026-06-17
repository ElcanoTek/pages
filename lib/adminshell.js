"use strict";
// lib/adminshell.js — the Flag-themed admin shell HTML (PLAN §8: shells use Flag
// tokens/components). Served on the dashboard host at GET /admin/:slug to an
// authenticated Elcano admin. The page is a thin bootstrap: it injects the CSRF
// token + slug, links the vendored Flag tokens/fonts, and loads admin.js, which
// drives everything through the admin JSON API (lib/adminapi.js). The iframe is
// a PREVIEW only (invariant #2) — sandboxed, pointed at the content host.

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

// render(slug, csrfToken, email) → full HTML document string.
function render(slug, csrfToken, email) {
  const s = esc(slug);
  return `<!doctype html>
<html lang="en" data-theme="dark">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Pages admin · ${s}</title>
<link rel="stylesheet" href="/shell-assets/flag/fonts/dubai-fonts.css">
<link rel="stylesheet" href="/shell-assets/flag/tokens/design-tokens.css">
<link rel="stylesheet" href="/shell-assets/shell.css">
<script src="/shell-assets/flag/theme/theme-controller.js" defer></script>
<!-- Bootstrap data as a NON-executed JSON island. An inline script element would
     be blocked by the shell CSP (script-src 'self', no unsafe-inline/nonce); a
     type=application/json block is data, not script, so admin.js reads it
     without violating CSP. -->
<script type="application/json" id="pages-bootstrap">${JSON.stringify({ slug, csrf: csrfToken, admin: email }).replace(/</g, "\\u003c")}</script>
<script src="/shell-assets/admin.js" defer></script>
</head>
<body>
<header class="app-header">
  <a class="brand" href="/admin" aria-label="Elcano Pages">
    <span class="brand__mark" aria-hidden="true">
      <svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M4 7.5 12 3l8 4.5"/><path d="M4 12l8 4.5 8-4.5"/><path d="M4 16.5 12 21l8-4.5"/></svg>
    </span>
    <span class="brand__name">Pages</span>
    <span class="crumb">/&nbsp;<b>${s}</b></span>
  </a>
  <div class="app-header__actions">
    <a class="btn btn-sm" href="/admin">All pages</a>
    <button class="btn btn-icon theme-toggle" type="button" data-flag-theme-toggle aria-label="Toggle light / dark theme" aria-pressed="true" title="Toggle theme">
      <svg class="icon-inline theme-toggle__icon theme-toggle__icon--sun" aria-hidden="true" width="17" height="17"><use href="/shell-assets/flag/icons/core-icons.svg#sun"></use></svg>
      <svg class="icon-inline theme-toggle__icon theme-toggle__icon--moon" aria-hidden="true" width="17" height="17"><use href="/shell-assets/flag/icons/core-icons.svg#moon"></use></svg>
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
