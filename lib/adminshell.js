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
<script>
  window.__PAGES__ = { slug: ${JSON.stringify(slug)}, csrf: ${JSON.stringify(csrfToken)}, admin: ${JSON.stringify(email)} };
</script>
<script src="/shell-assets/admin.js" defer></script>
</head>
<body>
<header class="app-header">
  <div class="app-header__title">Pages admin · <code>${s}</code></div>
  <div class="app-header__who">${esc(email)} · <a href="/logout">sign out</a></div>
</header>
<main id="app" class="wrap">
  <p class="muted" id="loading">Loading…</p>
</main>
</body>
</html>`;
}

module.exports = { render };
