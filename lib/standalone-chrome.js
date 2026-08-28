// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// One chrome for every standalone page either host renders around nothing of its
// own: the password gate, the portal index, and every 404/429/500. There were two
// of these — this and lib/errorshell.js — and this file's inline sheet
// re-implemented a dozen rules from public/shell-assets/shell.css with slightly
// different values (card padding space-7 vs space-card-padding, radius xl vs lg).
// Any brand fix had to be made twice and drifted the moment one was forgotten.
//
// It stays a self-contained inline <style> rather than a linked stylesheet. The
// content host serves these under the gate CSP with no script and no shared
// stylesheet, and a page that renders correctly before any other request
// completes is the point of them.
//
// Deliberately dark, and only dark. The Flag tokens flip on [data-theme] rather
// than on prefers-color-scheme, so following the OS would mean writing a second
// palette by hand for surfaces that carry no product UI. These pages are a brand
// moment; the dashboard behind them keeps its own theme.

const CSS = `  :root{--pages-action-fill:color-mix(in srgb,var(--color-primary) 96%,var(--color-black));--pages-action-hover:color-mix(in srgb,var(--color-primary) 98%,var(--color-black))}
  *,*::before,*::after{box-sizing:border-box}
  body{margin:0;min-width:0;min-height:100dvh;padding:var(--space-5);display:grid;place-items:center;color:var(--color-text-primary);background:var(--gradient-bg-home-signature);font-family:var(--font-body);font-size:var(--font-size-body);line-height:var(--line-height-body)}
  .gate-shell{width:min(100%,30rem);display:grid;gap:var(--space-5)}
  .brand{display:flex;align-items:center;gap:var(--space-3);width:fit-content;color:var(--color-text-primary);text-decoration:none}
  .brand img{width:2.25rem;height:2.25rem;object-fit:contain}
  .brand__text{display:grid;line-height:var(--line-height-caption)}
  .brand__eyebrow{color:var(--color-text-secondary);font-size:var(--font-size-overline);font-weight:var(--font-weight-bold);letter-spacing:.12em;text-transform:uppercase}
  .brand__name{font-size:var(--font-size-subtitle);font-weight:var(--font-weight-bold)}
  .card{width:100%;padding:var(--space-7);display:grid;gap:var(--space-4);border:1px solid var(--color-border-strong);border-radius:var(--radius-xl);background:var(--gradient-surface-elevated);box-shadow:var(--shadow-lg)}
  .kicker{margin:0;color:var(--color-text-secondary);font-size:var(--font-size-overline);font-weight:var(--font-weight-bold);letter-spacing:.12em;text-transform:uppercase}
  h1{margin:0;font-size:var(--font-size-title);line-height:var(--line-height-title);overflow-wrap:anywhere}
  p{margin:0;color:var(--color-text-secondary)}
  .sub{font-size:var(--font-size-body-lg);line-height:var(--line-height-body-lg)}
  form{display:grid;gap:var(--space-4);margin-block-start:var(--space-2)}
  .field{display:grid;gap:var(--space-2)}
  label{color:var(--color-text-secondary);font-size:var(--font-size-caption);font-weight:var(--font-weight-bold)}
  .help{color:var(--color-text-muted);font-size:var(--font-size-caption);font-weight:var(--font-weight-regular)}
  input{width:100%;min-height:3rem;padding:var(--space-3);border:1px solid var(--color-border-strong);border-radius:var(--radius-md);color:var(--color-text-primary);background:var(--color-surface-2);font:inherit;transition:border-color var(--transition-fast),box-shadow var(--transition-fast)}
  input:hover{border-color:var(--color-accent)}
  input:focus-visible{outline:none;border-color:var(--color-accent);box-shadow:var(--focus-ring)}
  button{width:100%;min-height:3rem;padding:var(--space-3);border:1px solid var(--pages-action-fill);border-radius:var(--radius-md);color:var(--color-white);background:var(--pages-action-fill);font:inherit;font-weight:var(--font-weight-bold);cursor:pointer;transition:background var(--transition-fast),border-color var(--transition-fast),transform var(--transition-fast),box-shadow var(--transition-fast)}
  button:hover{border-color:var(--pages-action-hover);background:var(--pages-action-hover);transform:translateY(-1px)}
  button:active{transform:translateY(0)}
  button:focus-visible,a:focus-visible{outline:none;box-shadow:var(--focus-ring)}
  .alert{padding:var(--space-3);display:grid;gap:var(--space-1);border:1px solid var(--color-status-error-border);border-radius:var(--radius-md);color:var(--color-status-error-fg);background:var(--color-status-error-bg);font-size:var(--font-size-caption)}
  .alert span{color:var(--color-status-error-fg)}
  .guidance{padding-block-start:var(--space-2);border-top:1px solid var(--color-border);color:var(--color-text-muted);font-size:var(--font-size-caption)}
  .guidance a{color:var(--color-accent)}
  .portal-section{margin:var(--space-2) 0 0;color:var(--color-text-secondary);font-size:var(--font-size-overline);font-weight:var(--font-weight-bold);letter-spacing:.12em;text-transform:uppercase}
  .portal-list{margin:0;padding:0;list-style:none;display:grid;gap:var(--space-2)}
  .portal-list a{min-width:0;overflow-wrap:anywhere;display:grid;gap:var(--space-1);padding:var(--space-3) var(--space-4);border:1px solid var(--color-border-strong);border-radius:var(--radius-md);color:var(--color-text-primary);background:var(--color-surface-2);text-decoration:none;transition:border-color var(--transition-fast),transform var(--transition-fast)}
  .portal-list a:hover{border-color:var(--color-accent);transform:translateY(-1px)}
  .portal-item__title{font-weight:var(--font-weight-bold)}
  /* Every dashboard in this list carries the date it was last current, one under
     the next. Nebula Sans's figures are proportional by default (digit advances
     407-625 per 1000em), so without tnum the column of dates reads ragged. */
  .portal-item__when{color:var(--color-text-muted);font-size:var(--font-size-caption);font-weight:var(--font-weight-regular);font-variant-numeric:tabular-nums}
  .portal-list--lead a{border-color:var(--color-accent);background:var(--gradient-surface-elevated)}
  .portal-footer{display:flex;flex-wrap:wrap;align-items:baseline;justify-content:space-between;gap:var(--space-2);padding-block-start:var(--space-2);border-top:1px solid var(--color-border)}
  .portal-footer .guidance{flex:1 1 14rem;padding-block-start:0;border-top:0}
  .portal-signout{margin:0;display:block}
  .portal-signout button{width:auto;min-height:2.25rem;padding:var(--space-2) var(--space-4);border-color:var(--color-border-strong);color:var(--color-text-secondary);background:none;font-size:var(--font-size-caption)}
  .portal-signout button:hover{border-color:var(--color-accent);color:var(--color-text-primary);background:var(--color-surface-2);transform:none}
  .credential-account{position:absolute;width:1px;height:1px;padding:0;margin:-1px;overflow:hidden;clip:rect(0,0,0,0);white-space:nowrap;border:0}
  @media(max-width:30rem){body{padding:var(--space-4)}.card{padding:var(--space-5)}}
  @media(prefers-reduced-motion:reduce){*,*::before,*::after{transition-duration:.01ms!important}}
  .cta{width:fit-content;min-height:3rem;padding:var(--space-3) var(--space-5);display:inline-flex;align-items:center;justify-content:center;border:1px solid var(--pages-action-fill);border-radius:var(--radius-md);color:var(--color-white);background:var(--pages-action-fill);text-decoration:none;font-weight:var(--font-weight-bold);transition:background var(--transition-fast),border-color var(--transition-fast)}
  .cta:hover{border-color:var(--pages-action-hover);background:var(--pages-action-hover)}
`;

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[char]);
}

function escapeAttr(value) {
  return escapeHtml(value);
}

// assetsBase — where this host serves the vendored Flag files from. The content
// host mounts them at /assets/flag, the dashboard host at /shell-assets/flag.
function render({ title, kicker, heading, bodyHtml = "", assetsBase, cta }) {
  const base = assetsBase || "/assets/flag";
  const action = cta
    ? `<a class="cta" href="${escapeAttr(cta.href)}">${escapeHtml(cta.label)}</a>`
    : "";
  return `<!doctype html><html lang="en" data-theme="dark"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="color-scheme" content="dark">
<title>${escapeHtml(title)}</title>
<link rel="icon" href="${base}/logos/elcano-mark-favicon.svg" type="image/svg+xml">
<link rel="stylesheet" href="${base}/fonts/fonts.css">
<link rel="stylesheet" href="${base}/tokens/design-tokens.css">
<style>
${CSS}</style></head>
<body><div class="gate-shell">
  <div class="brand"><img src="${base}/logos/elcano-mark-primary.svg" alt=""><span class="brand__text"><span class="brand__eyebrow">Elcano</span><span class="brand__name">Pages</span></span></div>
  <main class="card" aria-labelledby="gate-title">
    ${kicker ? `<p class="kicker">${escapeHtml(kicker)}</p>` : ""}
    <h1 id="gate-title">${escapeHtml(heading)}</h1>
    ${bodyHtml}
    ${action}
  </main>
</div></body></html>`;
}

module.exports = { render, CSS };
