// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/page-patch.js — anchored edits and literal search over a stored version.
//
// WHY. Before this, the smallest possible change to a live dashboard cost a
// whole document. Fixing one CSS rule meant: read 65 KB back through
// get_version, hold it in context, re-emit all 65 KB, redeploy. We watched an
// agent spend 143k completion tokens in a single turn hand-reassembling a
// document it had just been handed, because a 65 KB tool argument had blown up
// on the way in.
//
// The bytes already live here. An edit is a few hundred tokens of anchors, and
// a search is a few hundred tokens of matches — neither needs the document to
// make a round trip through a model.
//
// Anchors are LITERAL strings, never regexes: an agent-supplied pattern is an
// unbounded-backtracking hazard on a 2 MiB document, and every real edit is a
// literal anyway (cf. the ReDoS screen in lib/page-data.js). An anchor that does
// not match its expected occurrence count is an error, not a silent no-op — a
// patch that quietly did nothing is exactly how a "fixed" dashboard ships
// unfixed.

const { badRequest, conflict } = require("./apierror");

const MAX_EDITS = 25;
const MAX_FIND_CHARS = 8192;
const MAX_REPLACE_CHARS = 128 * 1024;
const MAX_EXPECTED_COUNT = 100;
// Search output is read by a model; keep it small and say what was left out.
const MAX_MATCHES = 20;
const MAX_CONTEXT_CHARS = 240;

function countOccurrences(haystack, needle) {
  let n = 0;
  let at = haystack.indexOf(needle);
  while (at !== -1) {
    n += 1;
    at = haystack.indexOf(needle, at + needle.length);
  }
  return n;
}

function lineOf(text, offset) {
  let line = 1;
  for (let i = 0; i < offset; i += 1) if (text.charCodeAt(i) === 10) line += 1;
  return line;
}

// A short, single-line excerpt around an offset — enough for a human or a model
// to recognise the site without shipping the surrounding document.
function excerpt(text, offset, length) {
  const pad = Math.max(0, Math.floor((MAX_CONTEXT_CHARS - length) / 2));
  const start = Math.max(0, offset - pad);
  const end = Math.min(text.length, offset + length + pad);
  const body = text.slice(start, end).replace(/\s+/g, " ").trim();
  return `${start > 0 ? "…" : ""}${body}${end < text.length ? "…" : ""}`;
}

function validateEdits(edits) {
  if (!Array.isArray(edits) || edits.length === 0) {
    throw badRequest("edits must be a non-empty array", "patch_edits_invalid");
  }
  if (edits.length > MAX_EDITS) {
    throw badRequest(`at most ${MAX_EDITS} edits per patch`, "patch_edits_invalid");
  }
  edits.forEach((edit, i) => {
    if (!edit || typeof edit.find !== "string" || typeof edit.replace !== "string") {
      throw badRequest(`edit ${i}: find and replace must both be strings`, "patch_edits_invalid");
    }
    if (edit.find.length === 0 || edit.find.length > MAX_FIND_CHARS) {
      throw badRequest(`edit ${i}: find must be 1-${MAX_FIND_CHARS} characters`, "patch_edits_invalid");
    }
    if (edit.replace.length > MAX_REPLACE_CHARS) {
      throw badRequest(`edit ${i}: replace must be at most ${MAX_REPLACE_CHARS} characters`, "patch_edits_invalid");
    }
    const count = edit.count === undefined ? 1 : edit.count;
    if (!Number.isInteger(count) || count < 1 || count > MAX_EXPECTED_COUNT) {
      throw badRequest(`edit ${i}: count must be an integer 1-${MAX_EXPECTED_COUNT}`, "patch_edits_invalid");
    }
  });
}

/**
 * Apply literal, occurrence-checked edits in order.
 *
 * Each edit must match exactly `count` times (default 1) in the text as it
 * stands when that edit runs — so edits that overlap earlier replacements fail
 * loudly instead of landing somewhere unintended.
 *
 * @returns {{html: string, applied: Array<{index:number, count:number, first_line:number, bytes_delta:number}>}}
 */
function applyEdits(sourceHtml, edits) {
  validateEdits(edits);
  let html = sourceHtml;
  const applied = [];

  edits.forEach((edit, index) => {
    const expected = edit.count === undefined ? 1 : edit.count;
    const found = countOccurrences(html, edit.find);
    if (found !== expected) {
      const hint =
        found === 0
          ? "The anchor is not present. Locate it with find_in_version and copy the exact bytes, including whitespace."
          : `Widen the anchor until it is unique, or set count:${found} if every occurrence should change.`;
      throw conflict(
        `edit ${index}: expected ${expected} occurrence(s) of the anchor, found ${found}. ${hint}`,
        "patch_anchor_mismatch",
        { edit_index: index, expected_count: expected, actual_count: found }
      );
    }
    const before = Buffer.byteLength(html);
    const firstAt = html.indexOf(edit.find);
    applied.push({
      index,
      count: found,
      first_line: lineOf(html, firstAt),
      bytes_delta: 0, // filled in below, once the replacement is done
    });
    html = html.split(edit.find).join(edit.replace);
    applied[applied.length - 1].bytes_delta = Buffer.byteLength(html) - before;
  });

  return { html, applied };
}

/**
 * Bounded literal search, so an agent can locate an anchor without reading the
 * document back.
 */
function findMatches(sourceHtml, query, { maxMatches = MAX_MATCHES, ignoreCase = false } = {}) {
  if (typeof query !== "string" || query.length === 0 || query.length > MAX_FIND_CHARS) {
    throw badRequest(`query must be 1-${MAX_FIND_CHARS} characters`, "search_query_invalid");
  }
  const limit = Math.min(Math.max(1, maxMatches), MAX_MATCHES);
  const haystack = ignoreCase ? sourceHtml.toLowerCase() : sourceHtml;
  const needle = ignoreCase ? query.toLowerCase() : query;

  const matches = [];
  let total = 0;
  let at = haystack.indexOf(needle);
  while (at !== -1) {
    total += 1;
    if (matches.length < limit) {
      matches.push({
        offset: at,
        line: lineOf(sourceHtml, at),
        excerpt: excerpt(sourceHtml, at, needle.length),
      });
    }
    at = haystack.indexOf(needle, at + needle.length);
  }
  return { total_matches: total, matches, matches_omitted: Math.max(0, total - matches.length) };
}

module.exports = {
  applyEdits,
  findMatches,
  countOccurrences,
  MAX_EDITS,
  MAX_FIND_CHARS,
  MAX_REPLACE_CHARS,
  MAX_MATCHES,
};
