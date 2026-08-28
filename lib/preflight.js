// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/preflight.js — static "will this page actually work once we serve it?" check.
//
// WHY THIS EXISTS. Agents author these dashboards in a sandbox with no browser,
// no JS engine, and no read access to the live page — so their only way to judge
// a document was counting braces. A live campaign dashboard shipped with a dead
// date-range picker through five deploy attempts and two user complaints because
// nothing in the loop could execute the page. Pages can: it holds the bytes AND
// it owns the CSP they will be served under, so the check belongs here.
//
// Deliberately STATIC (parse5 + V8's own parser via node:vm). No browser is
// launched on the request path. Every failure mode we actually observed in the
// wild is detectable without running the page, and a static pass is fast enough
// to run inline on every deploy.
//
// Findings are advisory. Pages never blocks a deploy on them — humans own
// publish (PLAN.md §5) and a false positive must never wedge an agent.

const vm = require("node:vm");
const parse5 = require("parse5");
const pageData = require("./page-data");
const SHADOWED = require("./preflight-shadowed-names");
const { sandboxTokens } = require("./csp");

// Reports are read by a model inside a tool result. Cap them so a pathological
// document cannot blow out an agent's context; the counts still tell the truth.
const MAX_PER_RULE = 8;
// Compile-only syntax checking is linear in V8, but bound it anyway — this runs
// on the deploy path (cf. the managed-data CPU bound in lib/page-data.js).
const MAX_SCRIPT_BYTES = 1024 * 1024;

const EVENT_ATTR_RE = /^on[a-z]+$/;
// A BARE `foo(` in call position. The lookbehind (rather than a consuming
// alternation) matters twice: it keeps member calls out — `this.togglePopover()`,
// `window.togglePopover()` and `a?.togglePopover()` all resolve correctly and
// must not be flagged — and it does not eat the delimiter, so adjacent calls
// like `a(b())` are both seen.
const CALL_RE = /(?<![.\w$])([A-Za-z_$][A-Za-z0-9_$]*)\s*\(/g;
const ABSOLUTE_URL_RE = /^[a-z][a-z0-9+.-]*:/i;

function walk(node, visit) {
  if (!node || typeof node !== "object") return;
  visit(node);
  const kids = node.childNodes || node.content?.childNodes;
  if (kids) for (const child of kids) walk(child, visit);
}

function attr(node, name) {
  const found = (node.attrs || []).find((a) => a.name === name);
  return found ? found.value : undefined;
}

function textOf(node) {
  return (node.childNodes || [])
    .filter((c) => c.nodeName === "#text")
    .map((c) => c.value)
    .join("");
}

function finding(code, message, extra) {
  return { code, message, ...extra };
}

// Collapse a rule's raw hits into a bounded list plus an honest overflow count.
function cap(list) {
  if (list.length <= MAX_PER_RULE) return { items: list, omitted: 0 };
  return { items: list.slice(0, MAX_PER_RULE), omitted: list.length - MAX_PER_RULE };
}

function collectScripts(doc) {
  const scripts = [];
  walk(doc, (node) => {
    if (node.nodeName !== "script") return;
    const type = (attr(node, "type") || "").toLowerCase();
    // Data blocks (application/json, text/template) are not executed.
    if (type && !/^(text\/)?(javascript|module|ecmascript)$/.test(type)) return;
    if (attr(node, "src") !== undefined) return;
    const src = textOf(node);
    if (src.trim()) scripts.push({ node, src, module: type === "module" });
  });
  return scripts;
}

// ── R1: inline on*= handler shadowed by a DOM member ─────────────────────────
// The bug class that killed the live picker. `onclick="togglePopover()"` calls
// HTMLElement.prototype.togglePopover, never the author's global.
function checkInlineHandlers(doc, scriptText) {
  const errors = [];
  const warnings = [];
  const definedGlobals = new Set();
  for (const m of scriptText.matchAll(/(?:^|[\s;}])(?:function\s*\*?\s*|(?:const|let|var)\s+)([A-Za-z_$][A-Za-z0-9_$]*)/g)) {
    definedGlobals.add(m[1]);
  }

  walk(doc, (node) => {
    for (const a of node.attrs || []) {
      if (!EVENT_ATTR_RE.test(a.name)) continue;
      for (const m of String(a.value).matchAll(CALL_RE)) {
        const name = m[1];
        if (!SHADOWED.has(name)) continue;
        const where = `<${node.nodeName} ${a.name}="${String(a.value).slice(0, 60)}">`;
        const shared = {
          element: node.nodeName,
          attribute: a.name,
          identifier: name,
          snippet: where,
          fix: `Bind with addEventListener instead of ${a.name}=, or rename the global (e.g. "${name}" -> "dr_${name}").`,
        };
        if (definedGlobals.has(name)) {
          errors.push(
            finding(
              "inline_handler_shadowed",
              `${a.name}="${name}(...)" calls the built-in DOM member ${name}(), not your global function of the same name. ` +
                `Inline handlers resolve identifiers against the element, its form and the document before window, so this throws at click time and the control silently does nothing.`,
              shared
            )
          );
        } else {
          warnings.push(
            finding(
              "inline_handler_shadowed_maybe",
              `${a.name}="${name}(...)" resolves to the built-in DOM member ${name}() inside an inline handler, not to a global. ` +
                `If you meant a function you define elsewhere, this control is dead.`,
              shared
            )
          );
        }
      }
    }
  });
  return { errors, warnings };
}

// ── R2: subresources the served CSP will block ───────────────────────────────
const SUBRESOURCE_RULES = [
  { tag: "script", attr: "src", directive: "script-src", kind: "script" },
  { tag: "img", attr: "src", directive: "img-src", kind: "image" },
  { tag: "image", attr: "href", directive: "img-src", kind: "image" },
  { tag: "iframe", attr: "src", directive: "frame-src", kind: "iframe" },
  { tag: "object", attr: "data", directive: "object-src", kind: "object" },
  { tag: "embed", attr: "src", directive: "object-src", kind: "embed" },
  { tag: "video", attr: "src", directive: "media-src", kind: "media" },
  { tag: "audio", attr: "src", directive: "media-src", kind: "media" },
];

function remoteHost(value) {
  if (typeof value !== "string") return null;
  const v = value.trim();
  if (!v || !ABSOLUTE_URL_RE.test(v)) return null; // relative == same-origin, fine
  if (/^(data|blob):/i.test(v)) return null;
  try {
    return new URL(v).host || null;
  } catch {
    return null;
  }
}

function checkSubresources(doc, cssText) {
  const errors = [];
  walk(doc, (node) => {
    for (const rule of SUBRESOURCE_RULES) {
      if (node.nodeName !== rule.tag) continue;
      const host = remoteHost(attr(node, rule.attr));
      if (!host) continue;
      errors.push(
        finding("remote_subresource_blocked", `<${rule.tag}> loads a ${rule.kind} from ${host}, which the content host's "${rule.directive}" blocks. It will not render.`, {
          element: rule.tag,
          host,
          directive: rule.directive,
          fix:
            rule.kind === "image"
              ? "Inline it as a data: URI (logos are usually a few KB as an optimised SVG or PNG)."
              : "Inline the code/asset in the document; the content host allows no remote origins.",
        })
      );
    }
    if (node.nodeName === "link") {
      const rel = (attr(node, "rel") || "").toLowerCase();
      const host = remoteHost(attr(node, "href"));
      if (host && /stylesheet|preload|font/.test(rel)) {
        errors.push(
          finding("remote_subresource_blocked", `<link rel="${rel}"> points at ${host}; the content host blocks every remote origin. It will not load.`, {
            element: "link", host, directive: "style-src/font-src",
            fix: "Inline the CSS in a <style> block and use a system font stack or a data: @font-face.",
          })
        );
      }
    }
  });

  for (const m of cssText.matchAll(/url\(\s*['"]?([^'")]+)['"]?\s*\)/gi)) {
    const host = remoteHost(m[1]);
    if (host) {
      errors.push(
        finding("remote_subresource_blocked", `CSS url() references ${host}; the content host blocks remote origins, so this asset will not load.`, {
          element: "css", host, directive: "img-src/font-src",
          fix: "Inline the asset as a data: URI.",
        })
      );
    }
  }
  return errors;
}

// ── R3: APIs the sandbox response header turns into silent no-ops ────────────
// Read the tokens from lib/csp.js so this can never drift from what we serve.
const SANDBOX_APIS = [
  { token: "allow-modals", re: /(?:^|[^.\w$])(?:window\s*\.\s*)?print\s*\(/, label: "window.print()",
    consequence: 'Chromium logs "Ignored call to print()" and nothing happens — a "Download PDF" button does nothing at all.' },
  { token: "allow-modals", re: /(?:^|[^.\w$])(?:window\s*\.\s*)?(?:alert|confirm|prompt)\s*\(/, label: "alert()/confirm()/prompt()",
    consequence: "The call is ignored; confirm() returns false, so any flow gated on it silently takes the negative branch." },
  { token: "allow-downloads", re: /\.\s*download\s*=|<a[^>]+\sdownload[\s=>]/i, label: "a[download] / programmatic download",
    consequence: "The click is swallowed — an Export CSV/Excel button appears to work and produces no file." },
  { token: "allow-popups", re: /(?:^|[^.\w$])window\s*\.\s*open\s*\(/, label: "window.open()",
    consequence: "Returns null and no window opens." },
];

function checkSandboxCapabilities(text) {
  const tokens = new Set(sandboxTokens());
  const errors = [];
  for (const api of SANDBOX_APIS) {
    if (tokens.has(api.token)) continue;
    if (!api.re.test(text)) continue;
    errors.push(
      finding("sandbox_capability_missing", `${api.label} does not work on the content host: its sandbox does not include "${api.token}". ${api.consequence}`, {
        api: api.label,
        required_token: api.token,
        fix: `Remove the control, or ask for "${api.token}" to be added to the content-host sandbox in lib/csp.js.`,
      })
    );
  }
  return errors;
}

// ── R4: opaque-origin APIs that throw rather than no-op ──────────────────────
const OPAQUE_ORIGIN_APIS = [
  { re: /(?:^|[^.\w$])(?:window\s*\.\s*)?(localStorage|sessionStorage)\b/, label: "localStorage/sessionStorage" },
  { re: /document\s*\.\s*cookie/, label: "document.cookie" },
  { re: /(?:^|[^.\w$])indexedDB\b/, label: "indexedDB" },
];

function checkOpaqueOrigin(text) {
  const warnings = [];
  for (const api of OPAQUE_ORIGIN_APIS) {
    if (!api.re.test(text)) continue;
    warnings.push(
      finding("opaque_origin_api", `${api.label} throws SecurityError on the content host — pages are served from a sandboxed, cookieless opaque origin with no storage access.`, {
        api: api.label,
        fix: "Keep the state in a module-level variable, or wrap every access in try/catch so an unhandled throw cannot take the rest of the script down with it.",
      })
    );
  }
  return warnings;
}

// ── R5: network calls the CSP forbids outright ───────────────────────────────
function checkNetwork(text) {
  const hits = [];
  if (/(?:^|[^.\w$])fetch\s*\(/.test(text)) hits.push("fetch()");
  if (/new\s+XMLHttpRequest\b/.test(text)) hits.push("XMLHttpRequest");
  if (/new\s+(?:WebSocket|EventSource)\b/.test(text)) hits.push("WebSocket/EventSource");
  if (/(?:^|[^.\w$])navigator\s*\.\s*sendBeacon\s*\(/.test(text)) hits.push("navigator.sendBeacon");
  if (!hits.length) return [];
  return [
    finding("network_blocked", `${hits.join(", ")} cannot reach anything: the content host sets "connect-src 'none'". Every request fails.`, {
      apis: hits,
      fix: "Embed the data in the document at deploy time, or move the page to the managed-data path (update_page_data) so Pages refreshes the numbers for you.",
    }),
  ];
}

// ── R7: the Pages-managed blocks do not parse ────────────────────────────────
// The most consequential thing preflight can catch, and the last one it learned.
// A managed page reads its own numbers with
// JSON.parse(document.getElementById('pages-data').textContent) — so a malformed
// #pages-data block is not a data problem, it is an uncaught exception on line one
// of the render layer and a blank page. Every other rule here already parses the
// scripts; none of them looked inside the JSON those scripts consume, so preflight
// answered ok:true for a document that cannot render at all. That is worse than
// staying silent: the whole point of the field is "trust this before you share the
// link".
//
// page_not_data_managed is not a finding. Most pages are ordinary documents with
// no managed blocks and nothing to check.
// The page-switcher payload is INJECTED into <head> at render time, and only on a
// themed render — `raw` is returned byte-for-byte, so nothing is added to it. A
// document that reads #pages-nav and is deployed raw has therefore written
// switcher code that can never run, and would fail silently in front of a partner.
// This is the one authoring trap the inject-don't-substitute design creates, and
// it is catchable from the bytes alone.
function checkNavBlock(html, renderMode) {
  if (renderMode !== "raw" || !html.includes("pages-nav")) return [];
  return [
    finding(
      "nav_block_ignored",
      'This document reads the #pages-nav page-switcher block, but render_mode is "raw", which is served byte-for-byte — Pages injects the switcher payload only into a themed render.',
      {
        fix:
          "Deploy this page as `themed` (which only adds tags to <head> and leaves your markup untouched), " +
          "or drop the switcher code. A raw page cannot show a Page menu.",
      }
    ),
  ];
}

function checkManagedBlocks(html) {
  try {
    // Judge the document as Pages will STORE it, not as it arrived. Shipping
    // #pages-config with no #pages-config-schema is a supported path —
    // prepareDeploy derives the missing block into the stored bytes on every
    // deploy — so checking the submitted bytes would report a contract failure
    // the served page does not have. Normalizing here also keeps this rule and
    // the deploy-side refusal looking at identical bytes, so they can never
    // disagree about whether a page is publishable.
    pageData.parseManagedHtml(pageData.ensureConfigSchema(html).html);
    return [];
  } catch (error) {
    const code = error && error.code;
    if (!code || code === "page_not_data_managed" || code === "page_not_template_managed") return [];
    return [
      finding("managed_block_invalid", `The Pages-managed blocks do not satisfy their own contract: ${error.message}`, {
        contract_code: code,
        fix:
          "The render layer JSON.parses these blocks, so this throws before anything draws and the page serves blank. " +
          "Fix the block contents and redeploy — or, if the numbers are what changed, use update_page_data instead of " +
          "editing the block by hand.",
      }),
    ];
  }
}

// ── R6: the script does not even parse ───────────────────────────────────────
// V8's own parser, compile-only. Catches the truncation/corruption that a
// chunked upload or a hand-reassembled document can leave behind.
function checkSyntax(scripts) {
  const errors = [];
  for (const [i, script] of scripts.entries()) {
    if (Buffer.byteLength(script.src) > MAX_SCRIPT_BYTES) continue;
    try {
      // eslint-disable-next-line no-new
      new vm.Script(script.src, { filename: `inline-script-${i}.js` });
    } catch (err) {
      if (!(err instanceof SyntaxError)) throw err;
      errors.push(
        finding("script_syntax_error", `Inline <script> #${i + 1} does not parse: ${err.message}. Nothing in that block runs, so every function it declares is undefined and every control wired to one is dead.`, {
          script_index: i + 1,
          fix: "Re-check the document was uploaded whole — a truncated or hand-reassembled script is the usual cause.",
        })
      );
    }
  }
  return errors;
}

/**
 * Analyse a page document the way the content host will serve it.
 *
 * @param {string} html    the stored source (pre-theme-injection)
 * @param {{renderMode?: string}} opts
 * @returns {{ok: boolean, errors: object[], warnings: object[], summary: string, checks: string[]}}
 */
function analyze(html, { renderMode = "raw" } = {}) {
  const source = typeof html === "string" ? html : "";
  const doc = parse5.parse(source);

  const scripts = collectScripts(doc);
  const scriptText = scripts.map((s) => s.src).join("\n;\n");

  let cssText = "";
  walk(doc, (node) => {
    if (node.nodeName === "style") cssText += `${textOf(node)}\n`;
    for (const a of node.attrs || []) if (a.name === "style") cssText += `${a.value}\n`;
  });

  const handlers = checkInlineHandlers(doc, scriptText);
  const errors = [
    ...checkSyntax(scripts),
    ...handlers.errors,
    ...checkSubresources(doc, cssText),
    ...checkSandboxCapabilities(`${scriptText}\n${source}`),
    ...checkNetwork(scriptText),
    ...checkManagedBlocks(source),
  ];
  const warnings = [...handlers.warnings, ...checkOpaqueOrigin(scriptText), ...checkNavBlock(source, renderMode)];

  const e = cap(errors);
  const w = cap(warnings);
  const parts = [];
  if (e.items.length) parts.push(`${errors.length} error${errors.length === 1 ? "" : "s"}`);
  if (w.items.length) parts.push(`${warnings.length} warning${warnings.length === 1 ? "" : "s"}`);

  return {
    ok: errors.length === 0,
    render_mode: renderMode,
    errors: e.items,
    warnings: w.items,
    errors_omitted: e.omitted,
    warnings_omitted: w.omitted,
    checks: [
      "script_syntax",
      "inline_handler_shadowed",
      "remote_subresource",
      "sandbox_capability",
      "network",
      "opaque_origin_api",
      "managed_block_contract",
      "page_switcher_block",
    ],
    summary: parts.length
      ? `Preflight found ${parts.join(" and ")}. These are advisory — nothing was blocked.`
      : "Preflight found no problems: scripts parse, managed blocks satisfy their contract, no shadowed inline handlers, no blocked subresources or APIs.",
  };
}

module.exports = { analyze, MAX_PER_RULE };
