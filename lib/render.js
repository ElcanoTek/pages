"use strict";
// lib/render.js — turn a stored version into the bytes served at /raw.
//
//   render_mode 'raw'    → the agent's HTML, verbatim (the "bespoke" path).
//   render_mode 'themed' → inject the Flag design system (tokens + fonts +
//                          theme controller) and the page's client theme
//                          override into <head>, WITHOUT touching the rest of
//                          the document. Injected nodes are tagged
//                          [data-flag-injected] so the source editor can strip
//                          them — we persist source, never the rendered frame.
//
// We inject by string-splicing after the opening <head> (or synthesizing one).
// This is deliberately NOT a full parse/serialize: we never rewrite the agent's
// markup, so chart/script structure is preserved byte-for-byte.

const FLAG_ASSETS_BASE = process.env.FLAG_ASSETS_BASE || "/assets/flag";

function flagHead(overrideCss) {
  const tags = [
    `<link rel="stylesheet" href="${FLAG_ASSETS_BASE}/fonts/dubai-fonts.css" data-flag-injected>`,
    `<link rel="stylesheet" href="${FLAG_ASSETS_BASE}/tokens/design-tokens.css" data-flag-injected>`,
  ];
  if (overrideCss && overrideCss.trim()) {
    // override_css is curated by Elcano (not agents); still sandboxed regardless.
    tags.push(`<style data-flag-injected>\n${overrideCss}\n</style>`);
  }
  tags.push(
    `<script src="${FLAG_ASSETS_BASE}/theme/theme-controller.js" data-flag-injected defer></script>`
  );
  return tags.join("\n");
}

function injectThemed(sourceHtml, { overrideCss = "" } = {}) {
  const head = flagHead(overrideCss);
  if (/<head[^>]*>/i.test(sourceHtml)) {
    return sourceHtml.replace(/(<head[^>]*>)/i, `$1\n${head}`);
  }
  if (/<html[^>]*>/i.test(sourceHtml)) {
    return sourceHtml.replace(/(<html[^>]*>)/i, `$1\n<head>\n${head}\n</head>`);
  }
  return `<!doctype html>\n<html>\n<head>\n${head}\n</head>\n<body>\n${sourceHtml}\n</body>\n</html>`;
}

// renderable: row from db.getRenderable ({ html, render_mode, override_css }).
function renderVersion(renderable) {
  if (renderable.render_mode === "raw") return renderable.html;
  return injectThemed(renderable.html, { overrideCss: renderable.override_css || "" });
}

module.exports = { renderVersion, injectThemed };
