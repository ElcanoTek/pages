// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/bento.js — what Pages knows about a Bento deck's DOCUMENT, and the edit
// session that lets a staff member save one back.
//
// A deck (bento.page; the .bento.html fleet's bento-slides skill produces) is one
// file that is its own viewer and editor: a compressed runtime around one JSON
// document block, <script type="application/bento+json" id="bento-doc">. Recognition
// and the serve-time host adaptation live in lib/render.js; this module reads
// and writes the document block, and builds the edit session.
//
// Why a save channel at all: a deck opens in Bento's full editor on the live
// page, but the content host forbids every request back to Pages (connect-src
// 'none', no cookies, opaque origin) — so Save could only ever produce a file.
// The edit session is the ONE response that may talk back: it is served for a
// signed, staff-minted, page-bound token; its CSP names Pages' own origin and no
// other; and Bento's Save — every entry point of it, since they all end in a
// blob download — is intercepted and POSTed to Pages instead, where it becomes a
// DRAFT version attributed to the staff member. Nothing here reaches the stored
// bytes or the vendored shell: it is all serve-time, and the deploy path strips
// every tag this module adds.

const render = require("./render");
const { escapedJson } = require("./page-data");

// The document block, with the offsets needed to write it back in place.
const DOC_BLOCK_RE = /(<script\b[^>]*\btype\s*=\s*(?:"application\/bento\+json"|'application\/bento\+json'|application\/bento\+json(?=[\s>]))[^>]*>)([\s\S]*?)(<\/script\s*>)/i;

// readDoc — the parsed document, or null when there is no block or it does not
// parse. Never throws: callers use it to decide, not to fail.
function readDoc(html) {
  const match = typeof html === "string" ? html.match(DOC_BLOCK_RE) : null;
  if (!match) return null;
  try {
    return JSON.parse(match[2]);
  } catch {
    return null;
  }
}

// stripCollab — remove the live-collaboration block from a deck's document.
//
// Bento writes a `collab` block — a room URL and a private key — into any file
// saved from its own UI, and a file saved from the hosted editor is exactly what
// the save channel receives. On Pages that block can never be used (both CSPs
// forbid connections), so it is key material in a shareable document and nothing
// else; fleet's own bento_doc.py drops it for the same reason. The rest of the
// document is re-serialised through escapedJson, which is the escaping the block
// requires (`<` as <) and the same encoding fleet's helper uses. docId and
// every other key are carried across untouched. A block that does not parse is
// left alone rather than risked.
function stripCollab(html) {
  const match = typeof html === "string" ? html.match(DOC_BLOCK_RE) : null;
  if (!match) return { html, stripped: false };
  let doc;
  try {
    doc = JSON.parse(match[2]);
  } catch {
    return { html, stripped: false };
  }
  if (!doc || typeof doc !== "object" || !("collab" in doc)) return { html, stripped: false };
  delete doc.collab;
  const rebuilt = html.slice(0, match.index) + match[1] + escapedJson(doc) + match[3] + html.slice(match.index + match[0].length);
  return { html: rebuilt, stripped: true };
}

// The save channel, injected into an edit-session response only.
//
// Every one of Bento's save entry points — the Save button, ⌘S, Save as → Save
// a copy — ends the same way on this host: the serialised deck goes into a
// text/html Blob, an <a download> is created for its object URL, and click() is
// called on it (lib/render.js already made the file-picker path unreachable).
// Intercepting that one step therefore catches all of them without touching
// Bento's code: the Blob's text is POSTed to Pages with the edit token; on
// success the download is suppressed and the deck says so; on ANY failure the
// download proceeds exactly as it would have, so a save can never be lost to a
// network error. Same attribute as the host adaptation, so the deploy-time strip
// removes it, and it removes itself from the DOM so a file that leaves carries
// nothing of Pages.
function editSessionScript({ saveUrl, token, versionId }) {
  const config = JSON.stringify({ saveUrl, token, versionId: versionId == null ? null : String(versionId) })
    .replace(/</g, "\\u003c");
  return (
    `<script data-pages-deck-host>(function(){var s=document.currentScript;var cfg=${config};var saves=Promise.resolve();var saveBlocked=false;` +
    `if(!(window.origin==="null"&&/^https?:$/.test(location.protocol))){if(s&&s.parentNode)s.parentNode.removeChild(s);return}` +
    // toast: a shadow root, so the deck's own styles cannot reach it and vice versa
    `var host=null;function say(text,kind){if(!host){host=document.createElement("div");host.setAttribute("data-pages-save-toast","");` +
    `host.style.cssText="position:fixed;left:50%;bottom:24px;transform:translateX(-50%);z-index:2147483647";var r=host.attachShadow({mode:"open"});` +
    `r.innerHTML="<style>p{margin:0;padding:10px 16px;border-radius:8px;font:14px/1.4 system-ui,sans-serif;color:#fff;background:#1c2b3a;box-shadow:0 8px 24px rgba(0,0,0,.35);max-width:min(36rem,90vw)}p.ok{background:#1f6b3a}p.err{background:#8a2b2b}</style><p></p>";` +
    `document.body.appendChild(host)}var p=host.shadowRoot.querySelector("p");p.className=kind||"";p.textContent=text;host.hidden=false;clearTimeout(host._t);host._t=setTimeout(function(){host.hidden=true},kind==="err"?12000:6000)}` +
    // remember every text/html blob by its object URL
    `var blobs=new Map();var cou=URL.createObjectURL;URL.createObjectURL=function(b){var u=cou.apply(this,arguments);if(b instanceof Blob&&/^text\\/html/.test(b.type))blobs.set(u,b);return u};` +
    `var clickA=HTMLAnchorElement.prototype.click;var passthrough=false;` +
    `HTMLAnchorElement.prototype.click=function(){var a=this;if(passthrough||!a.download||!blobs.has(a.href)){return clickA.apply(a,arguments)}` +
    `var blob=blobs.get(a.href);say("Saving to Pages\\u2026");` +
    `saves=saves.then(function(){if(saveBlocked)throw new Error("Reopen the editor from the admin to continue saving to Pages");return blob.text().then(function(text){return fetch(cfg.saveUrl,{method:"POST",headers:{"Authorization":"Bearer "+cfg.token,"Content-Type":"text/html; charset=utf-8","X-Pages-Base-Version":cfg.versionId||""},body:text,credentials:"omit"})})` +
    `.then(function(res){return res.text().then(function(body){var data={};try{data=JSON.parse(body)}catch(e){}if(!res.ok){if(res.status===409||res.status===403)saveBlocked=true;throw new Error(data.error||("Pages answered "+res.status))}return data})})` +
    `.then(function(data){cfg.versionId=String(data.version_id||cfg.versionId||"");say(data.deduped?"Nothing changed since the last save.":"Saved to Pages as a new draft version. Publish it from the admin when it is ready.","ok")})` +
    `}).catch(function(err){say("Couldn\\u2019t save to Pages ("+(err&&err.message?err.message:err)+"). Your file downloaded instead \\u2014 upload it as a new version.","err");passthrough=true;try{clickA.apply(a,arguments)}finally{passthrough=false}});` +
    `return undefined};` +
    `if(s&&s.parentNode)s.parentNode.removeChild(s)})();</script>`
  );
}

// editSession — the bytes served for an edit token: the stored deck, the host
// adaptation lib/render.js always adds, the deck's OWN guard CSP widened from
// connect-src 'none' to Pages' origin (the response header is widened the same
// way in lib/csp.js rawEditHeaders; both must agree or the save is refused by
// whichever is stricter), and the save channel. Everything added carries
// data-pages-deck-host and is stripped at deploy.
function editSession(html, { saveUrl, token, versionId, contentOrigin }) {
  const adapted = render.adaptDeckToHost(html);
  // The guard meta's content is double-quoted and full of single-quoted
  // keywords, so match the whole tag and edit inside it.
  const widened = adapted.replace(
    /<meta\b[^>]*http-equiv\s*=\s*["']Content-Security-Policy["'][^>]*>/i,
    (tag) => tag.replace("connect-src 'none'", `connect-src ${contentOrigin}`)
  );
  const script = editSessionScript({ saveUrl, token, versionId });
  // Directly after the host adaptation, which sits right after <head>.
  const at = widened.indexOf("</script>", widened.indexOf("data-pages-deck-host"));
  return at === -1 ? script + widened : widened.slice(0, at + 9) + script + widened.slice(at + 9);
}

module.exports = { readDoc, stripCollab, editSession, editSessionScript, DOC_BLOCK_RE };
