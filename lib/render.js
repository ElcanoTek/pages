// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
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

const { escapedJson } = require("./page-data");

const FLAG_ASSETS_BASE = process.env.FLAG_ASSETS_BASE || "/assets/flag";

// The id Pages owns. A page switcher needs the CURRENT membership of whichever
// portal authorised this request, which is not knowable at deploy time and must
// not require re-deploying every page when it changes — so the list is injected
// here, as one more head tag, rather than substituted into a block the author
// declares. The author declares nothing at all: a template that wants a switcher
// reads this id, and a template that does not gets the BUILT-IN one below.
//
// That fallback is not a nicety. Shipping only the payload meant a design still
// had to opt in with code, so the switcher worked on pages authored after the
// feature and on nothing else — which, across a fleet of dashboards that already
// exist, is almost every page. A partner in a portal would reach a dashboard and
// have to navigate back to the portal index to move anywhere. Pages therefore
// renders the control itself unless the document already references this id, in
// which case the design owns it and Pages adds nothing but the data.
//
// Injection rather than substitution is what keeps this cheap and safe: no
// parser on the serve path (parse5 with source locations on a 2 MiB document is
// ~369 ms of blocked event loop; the head splice is ~0.006 ms), no offset cache
// to invalidate, no new throw site, and no amendment to "raw is served verbatim".
//
// It sits in <head>, which precedes <body>, so getElementById returns OURS even
// if a document somehow carries its own. stripInjectedNav removes any such copy
// at deploy time anyway, so the id means one thing.
const NAV_BLOCK_ID = "pages-nav";

// Anchored on `<script … id=pages-nav …>` with the three quoting forms spelled
// out, so `id=pages-nav-extra` is not a match. Every repetition is separated by a
// required literal, so there is no backtracking shape here to catch.
const NAV_BLOCK_RE =
  /<script\b[^>]*\sid\s*=\s*(?:"pages-nav"|'pages-nav'|pages-nav(?=[\s>]))[^>]*>[\s\S]*?<\/script\s*>/gi;

// stripInjectedNav — deploy-time normalisation. `#pages-nav` is Pages' id; a
// document that ships its own copy would be a second answer to "which dashboards
// can this viewer open", authored by whoever wrote the page. Removing it at
// deploy time (before the content hash, like the config-schema normaliser) means
// the served bytes can only ever contain the one Pages injects.
function stripInjectedNav(sourceHtml) {
  if (typeof sourceHtml !== "string" || !sourceHtml.includes(NAV_BLOCK_ID)) return sourceHtml;
  return sourceHtml.replace(NAV_BLOCK_RE, "");
}

// readsNavBlock — does this document RENDER its own switcher? Scans the SOURCE
// (before injection, or the block we are about to add would answer for it).
//
// This used to be a bare substring scan for the id, with "a mention in a
// comment also opts out" documented as intended. Production falsified that
// intent: dashboard-authoring boilerplate carries a CSS comment that mentions
// #pages-nav while the control it describes was never built, so on fresh pages
// (Lakeside / Hy-Vee, 2026-08-20) the comment suppressed the built-in menu and
// the page rendered nothing — a portal whose dashboards silently lost their
// nav, indistinguishable from a membership or cookie problem from the outside.
// The cost asymmetry decides the rule: a false "owns it" hides the nav and
// takes a debugging session to find; a false "doesn't own it" doubles a menu
// on screen where the design's author sees it immediately.
//
// So the scan now matches the ways a document actually CONSUMES the block — a
// DOM read of the id, or shipping an element that carries it — and a bare
// mention in a comment or prose no longer counts. Still no parse: includes()
// keeps the common no-mention page at one substring scan, and the regex runs
// only when the id appears at all.
const NAV_READ_RE = new RegExp(
  [
    String.raw`getElementById\s*\(\s*["'](?:#\s*)?pages-nav["']\s*\)`,
    String.raw`querySelector(?:All)?\s*\(\s*["']#?pages-nav["']\s*\)`,
    String.raw`\bid\s*=\s*["']pages-nav["']`,
  ].join("|")
);
function readsNavBlock(sourceHtml) {
  return (
    typeof sourceHtml === "string" && sourceHtml.includes(NAV_BLOCK_ID) && NAV_READ_RE.test(sourceHtml)
  );
}

// escapeStyleText — a theme's override_css was the one stored string that reached
// a parser unescaped. The HTML tokenizer ends a <style> element at the first
// `</style`, wherever it appears — inside a CSS string, inside a comment, it does
// not matter — and everything after it is then parsed as markup in the page's own
// document. `content: "</style><script>…"` in a curated theme was enough.
//
// Escaping at render time rather than rejecting at write time, because this also
// covers every theme already in the database. `\/` is CSS's escape for a solidus
// and means exactly `/` in a string or an identifier; in a comment the backslash
// is inert; and outside those there is no legal CSS in which `</` appears at all.
// So the rendered sheet is the same stylesheet, and there is no sequence a theme
// can spell that closes the element.
//
// Deliberately every `</`, not just `</style`: the tokenizer is case-insensitive
// and accepts whitespace or `/` after the name, and enumerating those is how one
// spelling gets missed.
function escapeStyleText(css) {
  return String(css).replace(/<\//g, "<\\/");
}

// escapedJson, not JSON.stringify: the first dashboard title containing
// `</script` would otherwise end the block and put the rest of the payload into
// the document as markup.
function navBlockTag(nav) {
  return `<script type="application/json" id="${NAV_BLOCK_ID}" data-flag-injected>${escapedJson(nav)}</script>`;
}

// The built-in switcher: a self-contained control for every dashboard that does
// not render its own. Constraints it has to satisfy, since it lands inside an
// arbitrary client design Pages did not write:
//
//   * It must not move anything. `position: fixed`, so it cannot reflow a chart
//     or widen the document at any viewport.
//   * It must be legible over an unknown background. Flag tokens are injected on
//     this same render, so it uses them, with literal fallbacks for a design that
//     overrides them.
//   * Its names must not collide. Everything is prefixed `pgnav`, and the styles
//     are scoped to that class rather than to element selectors.
//   * A native <details> for the disclosure, real <a> elements for the links: the
//     sandbox withholds allow-popups, so a scripted window.open would silently do
//     nothing, and anchors bring keyboard and middle-click for free.
//   * Titles via textContent. A sibling's title is set by whoever owns that page.
//   * Print and reduced-motion are respected, because this is somebody's report.
function defaultSwitcherTags() {
  // Inside a shadow root, so: no token fallbacks to get half-applied, no
  // specificity contest with the dashboard's own `a:visited`, and a focus ring
  // that a design's `*:focus{outline:none}` cannot remove. The palette comes from
  // the page's own background rather than from Flag tokens, because a `raw`
  // dashboard is served without them and every fallback fired at once — a white
  // pill parked on a dark bespoke report.
  const css = `
:host{all:initial;position:fixed;z-index:10000;font:600 13px/1.35 system-ui,-apple-system,"Segoe UI",sans-serif}
*{box-sizing:border-box}
.pgnav{color:var(--fg)}
.pgnav>summary{list-style:none;cursor:pointer;display:inline-flex;align-items:center;gap:7px;padding:7px 11px;border:1px solid var(--edge);border-radius:8px;color:var(--fg);background:var(--bg);box-shadow:0 2px 10px rgba(16,24,40,.18)}
.pgnav>summary::-webkit-details-marker{display:none}
.pgnav>summary::marker{content:""}
.pgnav>summary:hover{border-color:var(--accent)}
.pgnav>summary:focus-visible{outline:2px solid var(--accent);outline-offset:2px}
.pgnav__caret{font-size:10px;opacity:.7}
.pgnav__menu{position:absolute;top:calc(100% + 6px);right:0;min-width:15rem;max-width:min(22rem,calc(100vw - 32px));max-height:min(70vh,32rem);overflow:auto;padding:7px;border:1px solid var(--edge);border-radius:10px;background:var(--bg);box-shadow:0 14px 38px rgba(16,24,40,.28)}
.pgnav__head{padding:4px 8px 7px;font-size:10.5px;font-weight:700;letter-spacing:.1em;text-transform:uppercase;color:var(--muted)}
.pgnav__item{display:block;padding:8px;border-radius:6px;color:var(--fg);text-decoration:none;font-weight:500;overflow-wrap:anywhere}
.pgnav__item:hover{background:var(--hover)}
.pgnav__item:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}
.pgnav__item--current{background:var(--hover);font-weight:700}
.pgnav__head--link{display:block;text-decoration:none;border-radius:6px;color:var(--muted)}
.pgnav__head--link:hover{background:var(--hover);text-decoration:underline}
.pgnav__head--link:focus-visible{outline:2px solid var(--accent);outline-offset:-2px}
.pgnav__home{margin-left:6px;font-size:9.5px;font-weight:700;letter-spacing:.08em;text-transform:uppercase;opacity:.7}
@media print{:host{display:none}}
@media(prefers-reduced-motion:reduce){*{transition:none!important}}`.trim();

  // No inline handlers: they are evaluated in a scope that cannot see anything
  // declared here, which is this codebase's single most repeated authoring bug.
  const js = `
(function(){
  function build(){
    var block=document.getElementById(${JSON.stringify(NAV_BLOCK_ID)});
    if(!block||document.querySelector(".pgnav-host"))return;
    var nav; try{nav=JSON.parse(block.textContent);}catch(e){return;}
    var pages=(nav&&Array.isArray(nav.pages))?nav.pages:[];
    if(pages.length<2)return;

    // The page decides where this sits. A dashboard's own brand row, export
    // button or "last updated" badge lives in the top-right corner too, and on a
    // phone there is no spare corner at all — so below 40rem it goes to the
    // bottom, where a thumb is anyway and designs rarely put anything.
    var root=document.documentElement;
    var rs=getComputedStyle(root);
    function authored(name,fallback){var v=rs.getPropertyValue(name); return v&&v.trim()?v.trim():fallback;}
    var narrow=(window.innerWidth||root.clientWidth||0)<640;
    var host=document.createElement("div"); host.className="pgnav-host";
    // Inline, so a dashboard's own rule on div cannot move it. The two custom
    // properties are the sanctioned way for a design to relocate it.
    host.style.cssText="position:fixed;z-index:10000;"
      +(narrow?("bottom:"+authored("--pages-nav-bottom","12px")+";")
              :("top:"+authored("--pages-nav-top","12px")+";"))
      +"right:"+authored("--pages-nav-right","12px")+";";
    var shadow=host.attachShadow?host.attachShadow({mode:"open"}):null;
    if(!shadow)return;

    // A raw dashboard is served without Flag tokens, so a token-with-fallback
    // palette fired every fallback at once and painted a white pill on a dark
    // report. Read what the page actually is instead.
    var lum=0.85;
    try{
      var probe=getComputedStyle(document.body||root).backgroundColor||"";
      var m=probe.match(/rgba?[(]([^)]+)[)]/);
      if(m){
        var parts=m[1].split(",").map(function(x){return parseFloat(x);});
        if(parts.length<4||parts[3]>0.1){
          lum=(0.2126*parts[0]+0.7152*parts[1]+0.0722*parts[2])/255;
        }
      }
    }catch(e){}
    var dark=lum<0.45;
    var palette=dark
      ? "--bg:#1b2130;--fg:#f2f4f8;--muted:#aab3c4;--edge:rgba(226,232,240,.28);--hover:rgba(226,232,240,.12);--accent:#8fb3e8"
      : "--bg:#fff;--fg:#1a2233;--muted:#5a6577;--edge:rgba(120,130,150,.45);--hover:rgba(120,130,150,.12);--accent:#3f6aa6";
    var style=document.createElement("style");
    // The palette goes AFTER the sheet: :host{all:initial} resets custom properties
    // too, so declaring it first left every var() undefined and painted the light
    // fallback on a dark report — which is the bug this is fixing.
    style.textContent=${JSON.stringify(css)}+" :host{"+palette+"}";
    shadow.appendChild(style);

    var d=document.createElement("details"); d.className="pgnav";
    var s=document.createElement("summary");
    s.appendChild(document.createTextNode("Page"));
    var c=document.createElement("span"); c.className="pgnav__caret"; c.setAttribute("aria-hidden","true");
    c.textContent="\\u25be"; s.appendChild(c); d.appendChild(s);
    var m=document.createElement("nav"); m.className="pgnav__menu";
    // "Dashboard pages" was our filing label, and appending " dashboards" to it
    // made the accessible name read "Dashboard pages dashboards". A partner with
    // a portal gets its name; one without gets the plain possessive.
    var portalName=(nav.portal&&nav.portal.name)?nav.portal.name:"";
    var label=portalName||"Your dashboards";
    m.setAttribute("aria-label",portalName?portalName+" dashboards":"Your dashboards");
    var portalUrl=(nav.portal&&nav.portal.url)?nav.portal.url:"";
    var h;
    if(portalUrl){h=document.createElement("a"); h.className="pgnav__head pgnav__head--link"; h.setAttribute("href",portalUrl);}
    else{h=document.createElement("div"); h.className="pgnav__head";}
    h.textContent=label; m.appendChild(h);
    pages.forEach(function(p){
      var node=document.createElement(p.current?"span":"a");
      node.className="pgnav__item"+(p.current?" pgnav__item--current":"");
      if(p.current){node.setAttribute("aria-current","page");}else{node.setAttribute("href",p.url);}
      node.textContent=p.title||p.slug;
      if(p.home){var hb=document.createElement("span"); hb.className="pgnav__home"; hb.textContent="Start here"; node.appendChild(hb);}
      m.appendChild(node);
    });
    if(nav.truncated){
      var more;
      if(portalUrl){more=document.createElement("a"); more.className="pgnav__head pgnav__head--link"; more.setAttribute("href",portalUrl);}
      else{more=document.createElement("div"); more.className="pgnav__head";}
      more.textContent="See all in your portal"; m.appendChild(more);
    }
    d.appendChild(m);
    // The click lands on the HOST from the document's point of view, so the
    // outside-click test has to ask the host, not the details inside it.
    document.addEventListener("click",function(e){ if(d.open&&!host.contains(e.target))d.open=false; });
    d.addEventListener("keydown",function(e){ if(e.key==="Escape"){d.open=false;s.focus();} });
    shadow.appendChild(d);
    (document.body||document.documentElement).appendChild(host);
  }
  if(document.readyState==="loading")document.addEventListener("DOMContentLoaded",build);
  else build();
})();`.trim();

  return [`<style data-flag-injected>\n${css}\n</style>`, `<script data-flag-injected>\n${js}\n</script>`];
}

function flagHead(overrideCss, nav, withDefaultSwitcher) {
  const tags = [
    `<link rel="stylesheet" href="${FLAG_ASSETS_BASE}/fonts/fonts.css" data-flag-injected>`,
    `<link rel="stylesheet" href="${FLAG_ASSETS_BASE}/tokens/design-tokens.css" data-flag-injected>`,
  ];
  if (overrideCss && overrideCss.trim()) {
    // override_css is curated by Elcano (not agents); still sandboxed regardless.
    tags.push(`<style data-flag-injected>\n${escapeStyleText(overrideCss)}\n</style>`);
  }
  tags.push(
    `<script src="${FLAG_ASSETS_BASE}/theme/theme-controller.js" data-flag-injected defer></script>`
  );
  if (nav) {
    tags.push(navBlockTag(nav));
    // …and the control itself, unless the design renders its own.
    if (withDefaultSwitcher) tags.push(...defaultSwitcherTags());
  }
  return tags.join("\n");
}

// `<head(?=[\s>])` and not `<head[^>]*>`: the looser form also matches `<header
// class="…">`, so a fragment with a header and no head would have had the whole
// Flag payload spliced into its banner.
const HEAD_OPEN_RE = /<head(?=[\s>])[^>]*>/i;

function injectThemed(sourceHtml, { overrideCss = "", nav = null } = {}) {
  // Read the SOURCE, before the block we are about to add would answer for it.
  const head = flagHead(overrideCss, nav, nav ? !readsNavBlock(sourceHtml) : false);
  if (HEAD_OPEN_RE.test(sourceHtml)) {
    return sourceHtml.replace(HEAD_OPEN_RE, (match) => `${match}\n${head}`);
  }
  if (/<html[^>]*>/i.test(sourceHtml)) {
    return sourceHtml.replace(/(<html[^>]*>)/i, `$1\n<head>\n${head}\n</head>`);
  }
  return `<!doctype html>\n<html>\n<head>\n${head}\n</head>\n<body>\n${sourceHtml}\n</body>\n</html>`;
}

// injectRawNav — the switcher, and ONLY the switcher, into a `raw` document.
//
// `raw` exists so an agent can push a fully self-contained design and have Pages
// not restyle it. That is what it protects, and this keeps protecting it: no Flag
// tokens, no fonts, no theme controller ever reach a raw page. What it stopped
// protecting is navigation, and the cost of that turned out to be the whole point
// of portals — 18 of 31 live dashboards are raw, so "raw gets no Page menu" meant
// most of a partner's set was a dead end. Redeploying them as themed is not the
// answer either: that WOULD restyle a bespoke design, which is the one thing raw
// is for.
//
// Placement matters. The payload goes as early as the document allows, because a
// design that reads it can run its script anywhere; the control goes last, so
// nothing of the author's is displaced.
function injectRawNav(sourceHtml, nav) {
  const payload = navBlockTag(nav);
  const control = readsNavBlock(sourceHtml) ? [] : defaultSwitcherTags();

  if (!HEAD_OPEN_RE.test(sourceHtml)) {
    // Nothing to hang the data on early. Everything goes at the end rather than
    // before the doctype, which would put the document into quirks mode. A design
    // reading the block from an inline script cannot see it in this shape — no raw
    // page does today, and the built-in control runs after the document either way.
    return appendToBody(sourceHtml, [payload, ...control].join("\n"));
  }
  const withPayload = sourceHtml.replace(HEAD_OPEN_RE, (match) => `${match}\n${payload}`);
  return control.length ? appendToBody(withPayload, control.join("\n")) : withPayload;
}

function appendToBody(html, extra) {
  if (/<\/body\s*>/i.test(html)) return html.replace(/<\/body\s*>/i, (match) => `${extra}\n${match}`);
  return `${html}\n${extra}`;
}

// renderable: row from db.getRenderable ({ html, render_mode, override_css }),
// plus an optional `nav` payload for the page switcher.
//
// `raw` is returned byte-for-byte UNLESS a portal authorised the view, in which
// case it gains the switcher and nothing else — no theming, no fonts, no theme
// controller. See injectRawNav: what raw protects is "do not restyle my design",
// and that is untouched; what it was also costing was navigation, on more than
// half the live fleet.
function renderVersion(renderable) {
  if (renderable.render_mode === "raw") {
    return renderable.nav ? injectRawNav(renderable.html, renderable.nav) : renderable.html;
  }
  return injectThemed(renderable.html, {
    overrideCss: renderable.override_css || "",
    nav: renderable.nav || null,
  });
}

module.exports = {
  renderVersion,
  injectThemed,
  injectRawNav,
  stripInjectedNav,
  readsNavBlock,
  NAV_BLOCK_ID,
  escapeStyleText,
};
