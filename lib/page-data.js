// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Safe parsing and materialization for Pages-managed dashboard data. This
// module deliberately owns no database behavior: it turns immutable HTML into
// a validated contract plus source offsets, and can replace only the contents
// of the pages-data script. The version state machine owns locking/publishing.

const crypto = require("node:crypto");
const { parse, parseFragment } = require("parse5");
const Ajv2020 = require("ajv/dist/2020");
const addFormats = require("ajv-formats");
const { badRequest, conflict } = require("./apierror");

const SCHEMA_ID = "pages-data-schema";
const DATA_ID = "pages-data";
const CONFIG_SCHEMA_ID = "pages-config-schema";
const CONFIG_ID = "pages-config";
const EXAMPLE_ID = "pages-data-example";
const SCHEMA_TYPE = "application/schema+json";
const DATA_TYPE = "application/json";
const JSON_SCHEMA_2020_12 = new Set([
  "https://json-schema.org/draft/2020-12/schema",
  "https://json-schema.org/draft/2020-12/schema#",
]);

// ── managed block pairs ──────────────────────────────────────────────────────
// A managed pair is a schema block plus the payload block it validates. The
// payload is the only thing a write may replace; the schema is design-time and
// immutable for the life of the deployed bytes.
//
//   data   — the refresh payload (an envelope). Replaced by update_page_data.
//   config — per-instance deploy-time settings (a bare object). Replaced only
//            by update_page_config. Optional: a page with no config pair is an
//            ordinary managed page and behaves exactly as it always has.
//
// A spec entry adds `required` plus the error code to raise when the pair is
// absent entirely, so the same block reader serves pages and templates.
//
// One entry is not a pair: `example` is a lone payload with no schema of its
// own, because it is example DATA and so validates against the data schema
// already in the document. It exists only so the library can show a design
// populated instead of empty, and materializeBlocks always deletes it — see
// EXAMPLE_ENTRY below.

const DATA_PAIR = Object.freeze({
  key: "data",
  label: "data",
  schemaId: SCHEMA_ID,
  payloadId: DATA_ID,
});

const CONFIG_PAIR = Object.freeze({
  key: "config",
  label: "config",
  schemaId: CONFIG_SCHEMA_ID,
  payloadId: CONFIG_ID,
});

// A page: data required, config optional (present only on template-derived pages).
const PAGE_SPEC = Object.freeze([
  Object.freeze({ ...DATA_PAIR, required: true, missingCode: "page_not_data_managed" }),
  Object.freeze({ ...CONFIG_PAIR, required: false, missingCode: "page_not_template_managed" }),
]);

// Example data for PREVIEW ONLY. A template ships an empty #pages-data so no
// page built from it inherits rows; that also means previewing a design shows a
// skeleton, which is not much use in a library. This optional block carries a
// bare data object (not an envelope — the server stamps those) that satisfies
// the same data schema, and materializeBlocks deletes the block outright, so
// neither a page nor a rendered preview ever carries these bytes.
const EXAMPLE_ENTRY = Object.freeze({
  key: "example",
  label: "example data",
  payloadId: EXAMPLE_ID,
  payloadOnly: true,
  required: false,
  missingCode: "template_contract_invalid",
});

// A template: both pairs required. Its shipped payloads are the reference
// config and the empty-state envelope, which is what makes a template a real
// page that can be validated against its own schemas at registration.
const TEMPLATE_SPEC = Object.freeze([
  Object.freeze({ ...DATA_PAIR, required: true, missingCode: "template_contract_invalid" }),
  Object.freeze({ ...CONFIG_PAIR, required: true, missingCode: "template_contract_invalid" }),
  EXAMPLE_ENTRY,
]);

function envBytes(name, fallback) {
  const value = Number(process.env[name]);
  return Number.isSafeInteger(value) && value > 0 ? value : fallback;
}

const MAX_SCHEMA_BYTES = envBytes("PAGES_DATA_SCHEMA_MAX_BYTES", 256 * 1024);
const MAX_DATA_BYTES = envBytes("PAGES_DATA_MAX_BYTES", 1024 * 1024);
// Config is per-campaign identity, channels, and registries — kilobytes, not
// the megabyte of rows a refresh carries. A tighter ceiling keeps a runaway
// config from being smuggled in as "settings".
const MAX_CONFIG_BYTES = envBytes("PAGES_DATA_CONFIG_MAX_BYTES", 256 * 1024);
const MAX_TEMPLATE_BYTES = envBytes("PAGES_DATA_TEMPLATE_MAX_BYTES", 2 * 1024 * 1024);
const MAX_SCHEMA_NODES = envBytes("PAGES_DATA_SCHEMA_MAX_NODES", 10000);
const MAX_SCHEMA_DEPTH = envBytes("PAGES_DATA_SCHEMA_MAX_DEPTH", 100);
const FUTURE_TOLERANCE_MS = envBytes("PAGES_SOURCE_FUTURE_TOLERANCE_MS", 5 * 60 * 1000);
const MAX_PATTERN_CHARS = 1000;
const COMPILE_CACHE_MAX = 64;
const DATA_PLACEHOLDER = "__PAGES_DATA_CONTENT__";

// ── payload profiling (issue #102) ───────────────────────────────────────────
// A schema validates SHAPE. It cannot tell you that a refresh covers 7 days
// where the last one covered 31, or that a deal which had spend has vanished
// from the rows. A Vandelay dashboard shipped both of those to a client: the DSP
// series silently started 9 days late because only the newest fast.io daily
// folder was read (a trailing-7-day export, not a cumulative one), and SSP
// totals were understated ~7% because rows were filtered to the three
// configured deals, dropping a fourth that carried all of Jul 6's spend. Every
// payload satisfied the schema perfectly.
//
// So compute what the payload actually CONTAINS — row counts, date extents,
// numeric totals, and the distinct values of low-cardinality keys — and hand it
// back on both the write and the read. The write additionally has the previous
// payload in hand already (versions.updatePageData parses the published HTML to
// materialize against it), so comparing the two costs no extra I/O.
//
// Bounds exist because this lands in a model's context and runs on the request
// path. A pathological payload gets an honest partial profile, never a stall.
const PROFILE_MAX_ARRAYS = 8;
const PROFILE_MAX_FIELDS = 24;
const PROFILE_MAX_SCALARS = 24;
const PROFILE_MAX_DEPTH = 6;
// Above this a string field is a free-text column, not a dimension worth
// enumerating; report only that it overflowed.
const PROFILE_MAX_DISTINCT = 50;
// …and only this many are LISTED on the wire, so a 50-deal campaign cannot
// flood the response.
const PROFILE_LIST_DISTINCT = 20;
// A value longer than this is prose, not an identifier, so it is not echoed
// verbatim on the wire either. It is still TRACKED for membership (see
// MEMBERSHIP below) — dropping it from the comparison is how a single long
// placement name would silently switch off the dimension check for its field.
const PROFILE_MAX_VALUE_LEN = 120;
// How many values a warning names, in the message and in previous/current,
// before it summarizes, and how long each may be. Warnings land in a model's
// context too, and the ceiling is what makes that survivable: a pathological
// payload can produce a warning per field per array.
const PROFILE_NAME_IN_MESSAGE = 8;
const PROFILE_MESSAGE_VALUE_LEN = 60;
// …and the list itself is capped, with the count of what was dropped, because
// an honest partial answer beats an unbounded dump. A real refresh emits one to
// four; only a payload where most fields lost values approaches this.
const MAX_DATA_WARNINGS = 40;

// MEMBERSHIP — the complete tracked value set, attached NON-ENUMERABLY so it is
// never serialized. This is the split that lets the diff be exact and the report
// be small at the same time: `values` on the wire is a bounded top-N summary,
// while compareDataProfiles reasons over the whole set.
//
// Safe because there is exactly one production caller (versions.updatePageData),
// which profiles both payloads in-process and compares them immediately. A
// profile that has crossed the wire simply has no membership set, and the diff
// below refuses to claim a value is missing from a list it knows is partial —
// silence is wrong, but a false "this deal vanished" on every correct refresh is
// worse, and that is what diffing two top-20 slices produced.
const MEMBERSHIP = Symbol("pages.profile.members");

function attachMembers(entry, members) {
  Object.defineProperty(entry, MEMBERSHIP, { value: members, enumerable: false });
  return entry;
}
function membersOf(entry) {
  return entry && entry[MEMBERSHIP] ? entry[MEMBERSHIP] : null;
}

// A value quoted into a warning is for a human to recognise, not to re-key on.
// Bound it so one prose value cannot make a warning bigger than the payload.
function truncateForMessage(value) {
  const s = String(value);
  return s.length <= PROFILE_MESSAGE_VALUE_LEN ? s : `${s.slice(0, PROFILE_MESSAGE_VALUE_LEN)}…`;
}

// capWarnings — keep the list bounded, and SAY so rather than truncating
// silently. A dropped warning that nobody knows was dropped is the failure this
// whole module exists to prevent.
function capWarnings(warnings) {
  if (warnings.length <= MAX_DATA_WARNINGS) return warnings;
  const kept = warnings.slice(0, MAX_DATA_WARNINGS);
  const dropped = warnings.length - MAX_DATA_WARNINGS;
  kept.push({
    code: "warnings_truncated",
    path: "",
    message:
      `${dropped} further warning${dropped === 1 ? "" : "s"} were omitted to keep this response bounded. ` +
      `A payload that narrows this many fields at once should be reconciled against its source before publishing, ` +
      `not triaged warning by warning.`,
    previous: warnings.length,
    current: MAX_DATA_WARNINGS,
  });
  return kept;
}
// Relative slack on an `expect.totals` check. A caller summing the same column
// in a different order gets a different last float bit; 0.01% absorbs that and
// still catches the ~7% understatement that caused #102.
const DEFAULT_EXPECT_TOLERANCE = 0.0001;
const DATE_PREFIX_RE = /^\d{4}-\d{2}-\d{2}/;

function sha256(text) {
  return crypto.createHash("sha256").update(text, "utf8").digest("hex");
}

function contractInvalid(message, details) {
  return conflict(message, "data_contract_invalid", details);
}

function isPlainObject(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function canonicalJson(value) {
  if (value === null || typeof value === "boolean" || typeof value === "string") {
    return JSON.stringify(value);
  }
  if (typeof value === "number") {
    if (!Number.isFinite(value)) throw badRequest("data must contain only finite JSON numbers", "data_contract_invalid");
    return JSON.stringify(value);
  }
  if (Array.isArray(value)) return `[${value.map(canonicalJson).join(",")}]`;
  if (isPlainObject(value)) {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJson(value[key])}`)
      .join(",")}}`;
  }
  throw badRequest("data must contain only JSON values", "data_contract_invalid");
}

// Floats do not add associatively, so a sum reported as 24541.600000000006
// invites a caller to "fix" a number that is already right. Six decimals is
// past anything money or impressions need and short of float noise.
function roundSum(value) {
  if (!Number.isFinite(value)) return null;
  if (Number.isInteger(value)) return value;
  return Number(value.toFixed(6));
}

function isDateLike(value) {
  return typeof value === "string" && DATE_PREFIX_RE.test(value);
}

// profileRows — one pass over an array of objects, accumulating per field. Which
// KIND a field gets is decided by what is actually in it, not by the schema:
// the schema may call something a string when every value is a date, and it is
// the values a human reconciles against a source export.
function profileRows(rows) {
  const fields = new Map();
  const field = (name) => {
    let f = fields.get(name);
    if (!f) {
      if (fields.size >= PROFILE_MAX_FIELDS) return null;
      f = { present: 0, nulls: 0, numbers: 0, sum: 0, min: null, max: null, dates: 0, strings: 0, counts: new Map(), overflow: false };
      fields.set(name, f);
    }
    return f;
  };

  for (const row of rows) {
    if (!isPlainObject(row)) continue;
    for (const key of Object.keys(row)) {
      const f = field(key);
      if (!f) continue;
      const value = row[key];
      f.present += 1;
      if (value === null || value === undefined) {
        f.nulls += 1;
        continue;
      }
      if (typeof value === "number" && Number.isFinite(value)) {
        f.numbers += 1;
        f.sum += value;
        if (f.min === null || value < f.min) f.min = value;
        if (f.max === null || value > f.max) f.max = value;
        continue;
      }
      if (typeof value === "string") {
        if (isDateLike(value)) {
          f.dates += 1;
          // Lexicographic comparison is correct for ISO-8601 prefixes.
          if (f.min === null || value < f.min) f.min = value;
          if (f.max === null || value > f.max) f.max = value;
        } else {
          f.strings += 1;
        }
        if (f.counts.has(value)) {
          f.counts.set(value, f.counts.get(value) + 1);
        } else if (f.counts.size < PROFILE_MAX_DISTINCT) {
          f.counts.set(value, 1);
        } else {
          f.overflow = true;
        }
      }
    }
  }

  const out = {};
  for (const [name, f] of fields) {
    if (f.numbers > 0 && f.dates === 0 && f.strings === 0) {
      out[name] = { kind: "number", sum: roundSum(f.sum), min: f.min, max: f.max, nulls: f.nulls };
      continue;
    }
    if (f.dates > 0 && f.numbers === 0) {
      out[name] = { kind: "date", min: f.min, max: f.max, distinct: f.counts.size, nulls: f.nulls };
      if (f.overflow) out[name].distinct_overflow = true;
      continue;
    }
    // A dimension worth enumerating, or free text. The distinction is purely
    // cardinality: a dealId has a handful of values, a note has one per row.
    const entry = { kind: f.overflow ? "text" : "key", distinct: f.counts.size, nulls: f.nulls };
    if (f.overflow) {
      entry.distinct_overflow = true;
    } else {
      // Two different jobs, deliberately separated. `values` is the bounded
      // SUMMARY that goes on the wire: the most frequent few, none of them
      // prose. MEMBERSHIP is the complete set the diff reasons over, and never
      // leaves the process. Diffing the summary — which is what this used to do
      // — was wrong in both directions on any field with more than 20 values: a
      // value that merely moved down the ranking read as deleted, and a value
      // genuinely deleted from below the cut was never named. Both reproduced on
      // a 25-deal payload.
      const listed = [...f.counts.entries()].sort((a, b) => (b[1] - a[1]) || (a[0] < b[0] ? -1 : 1));
      const shown = listed.filter(([value]) => value.length <= PROFILE_MAX_VALUE_LEN).slice(0, PROFILE_LIST_DISTINCT);
      entry.values = Object.fromEntries(shown);
      if (listed.length > shown.length) entry.values_omitted = listed.length - shown.length;
      attachMembers(entry, new Set(f.counts.keys()));
    }
    out[name] = entry;
  }
  return out;
}

// profileData — what the payload contains, at the paths a caller can name.
// Arrays of objects are the rows a refresh is made of; the top-level scalars
// (dataThrough, lastRefreshed) are the stamps that were also wrong in #102.
function profileData(data) {
  const arrays = {};
  const scalars = {};
  let arraysProfiled = 0;
  let scalarsRecorded = 0;

  const walk = (value, path, depth) => {
    if (depth > PROFILE_MAX_DEPTH) return;
    if (Array.isArray(value)) {
      const objectRows = value.filter(isPlainObject).length;
      if (objectRows > 0 && arraysProfiled < PROFILE_MAX_ARRAYS) {
        arraysProfiled += 1;
        arrays[path] = { count: value.length, fields: profileRows(value) };
      } else if (arraysProfiled < PROFILE_MAX_ARRAYS) {
        // An array of scalars still has a length worth reconciling.
        arraysProfiled += 1;
        arrays[path] = { count: value.length, fields: {} };
      }
      return;
    }
    if (isPlainObject(value)) {
      for (const key of Object.keys(value)) {
        walk(value[key], path ? `${path}.${key}` : key, depth + 1);
      }
      return;
    }
    if (path && scalarsRecorded < PROFILE_MAX_SCALARS && (typeof value !== "object" || value === null)) {
      scalarsRecorded += 1;
      scalars[path] = value;
    }
  };

  walk(data, "", 0);
  return { arrays, scalars };
}

// compareDataProfiles — what got smaller. WARNINGS, never errors: narrowing is
// sometimes exactly right (a new flight, a deal that genuinely ended), so this
// must not wedge a legitimate refresh. But both halves of #102 are here —
// "31 days became 7" as a coverage regression, "deal 1442375 vanished" as a
// missing dimension value — and neither was visible anywhere before.
function compareDataProfiles(previous, next, { expected = null, previousDataSha = null, nextDataSha = null } = {}) {
  const warnings = [];
  if (!next) return warnings;

  // Reconstructing the Vandelay incident from its transcript showed the hole in
  // comparison-based detection: the DSP window was WRONG IN THE FIRST PAYLOAD.
  // Every write — three create_page_from_template attempts and the one
  // update_page_data — carried Jul 30–Aug 5, against a request for "7/6 and on".
  // There was never a wider baseline to regress from, so a diff finds nothing,
  // and the write that shipped wrong numbers to a client is exactly the one a
  // diff cannot see. That is precisely when the caller's own arithmetic is the
  // only available check — so when there is no baseline AND no expect naming
  // this array, say so rather than staying quiet.
  const expectNames = (path) => {
    if (!isPlainObject(expected)) return false;
    if (isPlainObject(expected.row_count) && path in expected.row_count) return true;
    const prefix = `${path}.`;
    for (const group of [expected.totals, expected.date_range]) {
      if (isPlainObject(group) && Object.keys(group).some((key) => key.startsWith(prefix))) return true;
    }
    return false;
  };

  for (const [path, after] of Object.entries(next.arrays)) {
    const before = previous && previous.arrays[path];
    if (!before || before.count === 0) {
      // Only a data SERIES earns this. An array of scalars — `unmapped.notes`
      // and friends are diagnostic strings — has no coverage to verify, and
      // warning about one is the kind of noise that trains a reader to skip the
      // field entirely.
      const isSeries = Object.keys(after.fields).length > 0;
      if (isSeries && after.count > 0 && !expectNames(path)) {
        const dateField = Object.entries(after.fields).find(([, f]) => f.kind === "date");
        warnings.push({
          code: "coverage_unverified",
          path,
          message:
            `${path} has ${after.count} rows and the payload it replaces had none, so there is nothing to compare ` +
            `this against and you supplied no expect for it. Its coverage` +
            (dateField ? ` (${dateField[1].min} to ${dateField[1].max})` : "") +
            ` and totals are UNVERIFIED. A first payload that is already truncated looks exactly like a correct one ` +
            `here. Reconcile data_profile against the source, or state the window and totals in expect so Pages can refuse a payload that disagrees.`,
          previous: before ? before.count : 0,
          current: after.count,
        });
      }
      continue;
    }

    if (after.count < before.count) {
      warnings.push({
        code: "row_count_dropped",
        path,
        message: `${path} went from ${before.count} rows to ${after.count}. If the source is cumulative, this refresh is missing data.`,
        previous: before.count,
        current: after.count,
      });
    }

    for (const [name, afterField] of Object.entries(after.fields)) {
      const beforeField = before.fields[name];
      if (!beforeField) continue;
      const fieldPath = `${path}.${name}`;

      if (afterField.kind === "date" && beforeField.kind === "date") {
        if (beforeField.min && afterField.min && afterField.min > beforeField.min) {
          warnings.push({
            code: "coverage_start_regressed",
            path: fieldPath,
            message:
              `${fieldPath} now starts at ${afterField.min}, later than the ${beforeField.min} already published. ` +
              `A trailing-window source export reads exactly like this — confirm the earlier days are genuinely absent and not merely unread.`,
            previous: beforeField.min,
            current: afterField.min,
          });
        }
        if (beforeField.max && afterField.max && afterField.max < beforeField.max) {
          warnings.push({
            code: "coverage_end_regressed",
            path: fieldPath,
            message: `${fieldPath} now ends at ${afterField.max}, earlier than the ${beforeField.max} already published.`,
            previous: beforeField.max,
            current: afterField.max,
          });
        }
      }

      // Only over COMPLETE sets, on both sides. An overflowed (free-text) field
      // has no dimension to lose, and a profile that crossed the wire carries
      // only the bounded `values` summary — claiming a value is gone from a list
      // known to be partial is how this warning cried wolf on correct refreshes.
      const beforeMembers = membersOf(beforeField);
      const afterMembers = membersOf(afterField);
      if (beforeMembers && afterMembers) {
        const missing = [...beforeMembers].filter((value) => !afterMembers.has(value));
        if (missing.length) {
          const named = missing.slice(0, PROFILE_NAME_IN_MESSAGE);
          warnings.push({
            code: "dimension_values_missing",
            path: fieldPath,
            message:
              `${fieldPath} no longer contains ${named.map((v) => JSON.stringify(truncateForMessage(v))).join(", ")}` +
              `${missing.length > PROFILE_NAME_IN_MESSAGE ? ` and ${missing.length - PROFILE_NAME_IN_MESSAGE} more` : ""}, ` +
              `which the published payload had. Rows filtered to a configured subset look exactly like this.`,
            // Bounded like the message: a warning lands in a model's context too,
            // and 50 values on each side of 24 fields of 8 arrays is a dump.
            previous: [...beforeMembers].slice(0, PROFILE_NAME_IN_MESSAGE).map(truncateForMessage),
            current: [...afterMembers].slice(0, PROFILE_NAME_IN_MESSAGE).map(truncateForMessage),
          });
        }
      }
    }
  }
  // Stale-refresh findings LEAD, so the cap can never drop the one that changes
  // what the caller should say — but only after loss findings have had their say
  // about the same field. "The window did not advance" is noise printed next to
  // "the window now starts 24 days later": the regression is the story, and the
  // first cut of this let the milder sentence take the headline on a refresh that
  // had lost 24 days and a deal.
  const regressed = new Set(
    warnings
      .filter((w) => w.code === "coverage_start_regressed" || w.code === "coverage_end_regressed")
      .map((w) => w.path)
  );
  const stale = staleRefreshWarnings({
    previousDataSha,
    nextDataSha,
    previousProfile: previous,
    nextProfile: next,
  }).filter((w) => !regressed.has(w.path));
  return capWarnings([...stale, ...warnings]);
}

// assertExpectedProfile — the caller's own arithmetic, enforced. The tool
// description already said "do not call when source data is missing, stale, or
// incomplete"; nothing made that checkable. An agent that has just summed a CSV
// can state the totals it believes it wrote, and Pages refuses the write if the
// payload disagrees. Paths are the same dotted paths the profile reports, so a
// caller reads one and writes the other.
// staleRefreshWarnings — "did this refresh actually add anything?"
//
// compareDataProfiles answers "what got smaller", which is a different question,
// and a real refresh slipped straight between them. A published dashboard was
// refreshed, reported "Version 205 published and live", listed "Data Warnings:
// None", and carried the SAME Aug 3-Aug 5 coverage as the version before it. The
// person who received that email replied: "This just ran but didn't update
// yesterday Aug 6th data?"
//
// Nothing was broken. The managed-data dedupe key is (data_sha256,
// data_template_sha256, source_as_of, render_mode), so a newer source_as_of over
// byte-identical data is deliberately NOT a dedupe — that is how a re-verified
// source gets recorded. But every field in the response then reads like new
// numbers landed: deduped false, version_is_live true, a fresh version id. The
// one fact that mattered — no metric moved — was the one thing nothing said.
//
// Two distinct answers, because they mean different things to whoever asks:
//   • data_unchanged        — the payload is byte-identical. Same source file.
//   • coverage_did_not_advance — numbers moved but the window did not. The source
//                            was updated and only restated days already covered.
function staleRefreshWarnings({ previousDataSha, nextDataSha, previousProfile, nextProfile }) {
  const warnings = [];
  if (!previousProfile || !nextProfile) return warnings;
  // Both hashes are required, not merely used when present. Without them the two
  // findings collapse into one ambiguous signal: an identical payload also has a
  // window that did not advance, so a caller with no hashes would be told the
  // weaker, vaguer thing. Distinguishing "same file" from "restated" IS the value
  // here, so a caller that cannot supply that fact gets neither warning.
  if (!previousDataSha || !nextDataSha) return warnings;

  if (previousDataSha === nextDataSha) {
    warnings.push({
      code: "data_unchanged",
      path: "",
      message:
        "This payload is byte-identical to the one already published, so no metric changed. A version was still " +
        "created because source_as_of advanced, which is how a re-verified source is recorded \u2014 but if you " +
        "expected new numbers, the source did not contain any. Do not report this as a data update.",
      previous: previousDataSha,
      current: nextDataSha,
    });
    return warnings; // "nothing moved" already says more than "the window did not"
  }

  for (const [path, after] of Object.entries(nextProfile.arrays)) {
    const before = previousProfile.arrays[path];
    if (!before || before.count === 0) continue;
    for (const [name, afterField] of Object.entries(after.fields)) {
      const beforeField = before.fields[name];
      if (!beforeField || afterField.kind !== "date" || beforeField.kind !== "date") continue;
      // A genuine RESTATEMENT and nothing else: same first day, same last day, same
      // number of rows, different figures. Anything that grew — an earlier start,
      // a later end, more rows — added data, and saying "the window did not
      // advance" about a refresh that just restored three weeks of history at the
      // front is worse than saying nothing. Anything that shrank is a loss finding
      // and gets reported as one, which is louder and more specific.
      const sameWindow =
        beforeField.max && afterField.max && afterField.max === beforeField.max &&
        beforeField.min && afterField.min && afterField.min === beforeField.min;
      if (sameWindow && after.count === before.count) {
        warnings.push({
          code: "coverage_did_not_advance",
          path: `${path}.${name}`,
          message:
            `${path}.${name} still ends at ${afterField.max}, the same day the published payload ended on. ` +
            `Figures changed but the window did not, so this refresh restated days that were already covered ` +
            `and added no new one. If a newer day was expected, the source did not have it \u2014 say that ` +
            `rather than reporting a refresh.`,
          previous: beforeField.max,
          current: afterField.max,
        });
      }
    }
  }
  return warnings;
}

function assertExpectedProfile(profile, expect, errorCode = "data_reconciliation_failed") {
  if (expect === undefined || expect === null) return;
  if (!isPlainObject(expect)) throw badRequest("expect must be an object", errorCode);

  const tolerance =
    expect.tolerance === undefined || expect.tolerance === null ? DEFAULT_EXPECT_TOLERANCE : Number(expect.tolerance);
  if (!Number.isFinite(tolerance) || tolerance < 0 || tolerance > 1) {
    throw badRequest("expect.tolerance must be a fraction between 0 and 1", errorCode);
  }
  const mismatches = [];
  const arrayAt = (path) => profile.arrays[path];
  const fieldAt = (path) => {
    const cut = path.lastIndexOf(".");
    if (cut < 0) return null;
    const array = profile.arrays[path.slice(0, cut)];
    return array ? array.fields[path.slice(cut + 1)] : null;
  };

  for (const [path, want] of Object.entries(expect.row_count || {})) {
    const array = arrayAt(path);
    if (!array) {
      mismatches.push({ check: "row_count", path, expected: want, actual: null, detail: "no array at that path" });
    } else if (array.count !== Number(want)) {
      mismatches.push({ check: "row_count", path, expected: Number(want), actual: array.count });
    }
  }

  for (const [path, want] of Object.entries(expect.totals || {})) {
    const field = fieldAt(path);
    const wanted = Number(want);
    if (!field || field.kind !== "number") {
      mismatches.push({ check: "total", path, expected: wanted, actual: null, detail: "no numeric field at that path" });
      continue;
    }
    // Relative where there is something to be relative to, absolute at zero.
    const slack = Math.abs(wanted) * tolerance || tolerance;
    if (Math.abs(field.sum - wanted) > slack) {
      mismatches.push({ check: "total", path, expected: wanted, actual: field.sum, tolerance: slack });
    }
  }

  for (const [path, want] of Object.entries(expect.date_range || {})) {
    const field = fieldAt(path);
    if (!Array.isArray(want) || want.length !== 2) {
      throw badRequest(`expect.date_range["${path}"] must be [min, max]`, errorCode);
    }
    if (!field || field.kind !== "date") {
      mismatches.push({ check: "date_range", path, expected: want, actual: null, detail: "no date field at that path" });
      continue;
    }
    if (field.min !== want[0] || field.max !== want[1]) {
      mismatches.push({ check: "date_range", path, expected: want, actual: [field.min, field.max] });
    }
  }

  if (mismatches.length) {
    throw badRequest(
      `the payload does not match what you said it would contain, so nothing was written: ` +
        mismatches
          .map((m) => `${m.check} at ${m.path} expected ${JSON.stringify(m.expected)}, payload has ${JSON.stringify(m.actual)}`)
          .join("; "),
      errorCode,
      { mismatches }
    );
  }
}

function semanticHash(value) {
  return sha256(canonicalJson(value));
}

function utf8Bytes(value) {
  return Buffer.byteLength(value, "utf8");
}

// ── pattern safety (ReDoS screen) ────────────────────────────────────────────
// Schema `pattern` keywords are compiled straight to RegExp and run against
// caller-supplied data on the shared event loop, where no statement_timeout
// applies. Two shapes are rejected at compile time:
//   • a quantifier nested inside another — (a+)+, (\d+)*, ([a-z]+){2,}
//   • a repeated group with two IDENTICAL alternatives — (a|a)+ — which
//     backtracks with no inner quantifier at all (14.5 seconds of blocked event
//     loop on a 29-character NON-matching input).
//
// A bounded outer repetition around an unbounded inner one — (.*a){20},
// (a{1,10})* — is equally explosive and is NOT screened. It was attempted and
// reverted: treating any {n,m} with max>1 as a repetition also files a BOUNDED
// inner quantifier as unbounded, which rejects the grouped-repetition idiom
// every real id format is built from — IPv4, IPv6, MAC, UUID, semver, cron,
// time ranges. Nineteen false positives, none of them catastrophic. Telling
// (.*a){20} from (\d{1,3}\.){3} needs the delimiter proof extended to trailing
// literals and complementary classes, which is a bigger change than a flag.
// docs/SECURITY.md records it as a follow-up rather than pretending it is
// covered. The delimited idiom (-[a-z0-9]+)* is NOT that shape: every pass
// through the group must start with the literal "-", which the inner
// repetition can never consume, so matches partition linearly. The screen
// keeps that idiom (the "literal delimiter" exemption) and errs conservative
// past one level of nesting.

function classMatchesChar(body, char) {
  const code = char.codePointAt(0);
  let negated = false;
  let i = 0;
  if (body.startsWith("^")) { negated = true; i = 1; }
  let matched = false;
  while (i < body.length) {
    let lo;
    if (body[i] === "\\") {
      const esc = body[i + 1] || "";
      if (esc === "d") { if (code >= 48 && code <= 57) matched = true; i += 2; continue; }
      if (esc === "w") { if (/[A-Za-z0-9_]/.test(char)) matched = true; i += 2; continue; }
      if (esc === "s") { if (/\s/.test(char)) matched = true; i += 2; continue; }
      if (esc === "S" || esc === "D" || esc === "W") return true; // complements: assume match
      lo = esc.codePointAt(0);
      i += 2;
    } else {
      lo = body.codePointAt(i);
      i += 1;
    }
    if (body[i] === "-" && i + 1 < body.length) {
      let hi;
      if (body[i + 1] === "\\") { hi = (body[i + 2] || "").codePointAt(0); i += 3; }
      else { hi = body.codePointAt(i + 1); i += 2; }
      if (lo <= code && code <= hi) matched = true;
    } else if (lo === code) {
      matched = true;
    }
  }
  return negated ? !matched : matched;
}

// A group is unsafe to quantify unboundedly when its body carries an inner
// unbounded quantifier that could also consume the group's own starting
// delimiter — that's what lets iterations overlap and backtrack explosively.
// Two IDENTICAL alternatives give the engine a real choice at every iteration,
// so a repeated group built from them backtracks exponentially with no inner
// quantifier at all: `^(a|a)+$` blocks this process for 14.5 seconds on a
// 29-character non-matching input.
//
// Only exact duplicates. A prefix relation was tried and reverted: `(a|ab)+`,
// `(GET|G)+`, `(https|http)+` and `(1|10|100)+` all parse UNIQUELY — every `b`
// must be consumed by an `ab` piece preceded by an `a`, so there is nothing to
// backtrack over — and all measured 0.1ms on 3000+ character hostile input.
// `(-|--)` is genuinely ambiguous, but because the longer branch DECOMPOSES into
// the shorter one, which is a different property from sharing a prefix and needs
// a real decomposition check rather than startsWith.
//
// This is a source-text comparison, so it deters an accident and not an author:
// `(a|[a])+`, `((a)|(a))+` and `(a|(b|b))+` are all still accepted and all still
// block for 14-21 seconds. See docs/SECURITY.md's follow-up.
function branchesOverlap(branches) {
  if (!branches || branches.length < 2) return false;
  for (let a = 0; a < branches.length; a++) {
    for (let b = a + 1; b < branches.length; b++) {
      if (branches[a] === branches[b]) return true;
    }
  }
  return false;
}

function groupIsAmbiguous(group) {
  if (group.hasAlternation && branchesOverlap(group.branches)) return true;
  if (group.innerUnbounded.length === 0) return false;
  const first = group.body[0];
  // The delimiter exemption is valid only when EVERY pass through the group
  // must consume the same leading literal. An optional delimiter or a
  // top-level alternative can bypass it, so neither proves a partition.
  if (!group.hasAlternation && first && first.type === "literal" && !first.optional) {
    return !group.innerUnbounded.every((atom) => {
      if (atom.type === "literal") return atom.char !== first.char;
      if (atom.type === "class") return !classMatchesChar(atom.body, first.char);
      return false; // nested-group atom: conservative → treat as ambiguous
    });
  }
  return true;
}

function isSafePattern(pattern) {
  if (typeof pattern !== "string" || pattern.length > MAX_PATTERN_CHARS) return false;
  const stack = [];
  const current = () => stack[stack.length - 1];
  let prevAtom = null;
  const pushAtom = (atom) => {
    const c = current();
    if (c) c.body.push(atom);
  };
  let i = 0;
  while (i < pattern.length) {
    const ch = pattern[i];
    if (ch === "\\") {
      const next = pattern[i + 1] || "";
      prevAtom = /[dwsbSDWpP]/.test(next)
        ? { type: "class", body: `\\${next}` }
        : { type: "literal", char: next };
      pushAtom(prevAtom);
      i += 2;
      continue;
    }
    if (ch === "[") {
      let j = i + 1;
      let body = "";
      if (pattern[j] === "^") { body += "^"; j++; }
      if (pattern[j] === "]") { body += "]"; j++; } // a leading ] is literal
      while (j < pattern.length && pattern[j] !== "]") {
        if (pattern[j] === "\\") { body += pattern[j] + (pattern[j + 1] || ""); j += 2; continue; }
        body += pattern[j];
        j++;
      }
      if (j >= pattern.length) return false; // unterminated class (invalid regex anyway)
      prevAtom = { type: "class", body };
      pushAtom(prevAtom);
      i = j + 1;
      continue;
    }
    if (ch === "(") {
      let j = i + 1;
      let zeroWidth = false;
      if (pattern[j] === "?") {
        j++;
        if (pattern[j] === ":") {
          j++;
        } else if (pattern[j] === "=" || pattern[j] === "!") {
          zeroWidth = true;
          j++;
        } else if (pattern[j] === "<" && (pattern[j + 1] === "=" || pattern[j + 1] === "!")) {
          zeroWidth = true;
          j += 2;
        } else if (pattern[j] === "<") {
          // Named capture: (?<name>...). The name is syntax, not a literal
          // prefix of the group's language, so skip it before parsing the body.
          const end = pattern.indexOf(">", j + 1);
          if (end < 0 || end === j + 1) return false;
          j = end + 1;
        } else {
          // Unknown/future group syntax cannot safely participate in the
          // delimiter proof. Invalid syntax would be rejected by RegExp later,
          // but failing closed here also covers newly introduced constructs.
          return false;
        }
      }
      stack.push({ body: [], innerUnbounded: [], hasAlternation: false, zeroWidth, branchStarts: [j] });
      prevAtom = null;
      i = j;
      continue;
    }
    if (ch === ")") {
      const g = stack.pop();
      if (!g) return false; // unbalanced (invalid regex anyway)
      g.branches = g.branchStarts.map((start, n) => {
        const end = n + 1 < g.branchStarts.length ? g.branchStarts[n + 1] - 1 : i;
        return pattern.slice(start, end);
      });
      prevAtom = g.zeroWidth ? { type: "assertion" } : { type: "group", group: g };
      pushAtom(prevAtom);
      // Parentheses do not erase repetition depth. Propagate an unbounded
      // descendant through an otherwise-unquantified wrapper so ((\d+))* is
      // screened exactly like (\d+)*.
      const parent = current();
      if (parent && g.innerUnbounded.length > 0) parent.innerUnbounded.push(prevAtom);
      i++;
      continue;
    }
    if (ch === "+" || ch === "*" || ch === "?" || ch === "{") {
      let unbounded = ch === "+" || ch === "*";
      let optional = ch === "*" || ch === "?";
      let span = 1;
      if (ch === "{") {
        const m = /^\{(\d+)(?:,(\d*))?\}/.exec(pattern.slice(i));
        if (m) {
          optional = Number(m[1]) === 0;
          unbounded = m[2] !== undefined && m[2] === ""; // {n,} is unbounded
          span = m[0].length;
        } else {
          prevAtom = { type: "literal", char: "{" }; // a lone { is a literal
          pushAtom(prevAtom);
          i++;
          continue;
        }
      }
      if (prevAtom && optional) prevAtom.optional = true;
      if (unbounded) {
        if (prevAtom && prevAtom.type === "group" && groupIsAmbiguous(prevAtom.group)) return false;
        const c = current();
        if (c && prevAtom) c.innerUnbounded.push(prevAtom);
      }
      prevAtom = null;
      i += span;
      continue;
    }
    if (ch === "|") {
      const c = current();
      if (c) {
        c.hasAlternation = true;
        c.branchStarts.push(i + 1);
      }
      prevAtom = null;
      i++;
      continue;
    }
    if (ch === "^" || ch === "$") {
      prevAtom = { type: "assertion" };
      pushAtom(prevAtom);
      i++;
      continue;
    }
    if (ch === ".") { prevAtom = { type: "class", body: "\\S" }; pushAtom(prevAtom); i++; continue; }
    prevAtom = { type: "literal", char: ch };
    pushAtom(prevAtom);
    i++;
  }
  return stack.length === 0;
}

// ── compiled-schema cache ────────────────────────────────────────────────────
// parseManagedHtml runs on every managed-data read/write; compiling a
// near-max-size schema per call is free CPU for a loop-calling agent. Ajv
// validators are stateless between runs, so compile once per schema content
// and reuse, keyed by the deterministic schema hash, bounded LRU.
const compileCache = new Map();
const compileCacheCounters = { hits: 0, misses: 0 };
function compileCacheStats() {
  return { size: compileCache.size, ...compileCacheCounters };
}
// Keyed by schema content only: `label` changes error wording, never validity,
// so the same schema used as both a config and a data schema shares one
// compiled validator.
function compileSchemaCached(schema, label = "data") {
  const key = semanticHash(schema);
  const hit = compileCache.get(key);
  if (hit) {
    compileCacheCounters.hits++;
    compileCache.delete(key);
    compileCache.set(key, hit); // refresh recency
    return hit;
  }
  compileCacheCounters.misses++;
  const validate = compileSchema(schema, label);
  compileCache.set(key, validate);
  while (compileCache.size > COMPILE_CACHE_MAX) {
    compileCache.delete(compileCache.keys().next().value);
  }
  return validate;
}

function walkHtml(node, matches) {
  if (node && node.tagName === "script" && node.namespaceURI === "http://www.w3.org/1999/xhtml") {
    const attributes = new Map((node.attrs || []).map((attribute) => [attribute.name, attribute.value]));
    const id = attributes.get("id");
    if (id !== undefined && Object.prototype.hasOwnProperty.call(matches, id)) {
      const location = node.sourceCodeLocation;
      if (!location || !location.startTag || !location.endTag) {
        throw contractInvalid(`the ${id} block must have an explicit closing </script> tag`);
      }
      matches[id].push({
        type: attributes.get("type"),
        contentStart: location.startTag.endOffset,
        contentEnd: location.endTag.startOffset,
        // The whole element, so a block can be DELETED rather than emptied.
        elementStart: location.startOffset,
        elementEnd: location.endOffset,
      });
    }
  }
  for (const child of (node && node.childNodes) || []) walkHtml(child, matches);
}

// findBlocks — locate every pair in `spec`. A pair that is absent ENTIRELY is
// either the "not managed" case (required → the caller can add the blocks) or
// simply not in use (optional → null). Any other shape — one block without its
// partner, a duplicate, a wrong type — is a broken contract and fails closed,
// because a page whose schema and payload disagree is exactly the state where a
// silent write would be worst.
function findBlocks(html, spec = PAGE_SPEC) {
  if (typeof html !== "string" || html.length === 0) {
    throw contractInvalid("published HTML is empty");
  }
  if (utf8Bytes(html) > MAX_TEMPLATE_BYTES) {
    throw contractInvalid(`published HTML exceeds the ${MAX_TEMPLATE_BYTES}-byte managed-data limit`);
  }

  const fullDocument = /<!doctype\s|<html(?:\s|>)|<head(?:\s|>)|<body(?:\s|>)/i.test(html);
  const document = fullDocument
    ? parse(html, { sourceCodeLocationInfo: true })
    : parseFragment(html, { sourceCodeLocationInfo: true });
  const matches = {};
  for (const entry of spec) {
    if (entry.schemaId) matches[entry.schemaId] = [];
    matches[entry.payloadId] = [];
  }
  walkHtml(document, matches);

  const found = {};
  for (const entry of spec) {
    const payloadMatches = matches[entry.payloadId];

    // A payload-only entry has no schema of its own; it borrows one already in
    // the document. Zero or one, never two, and the same wrong-type check.
    if (entry.payloadOnly) {
      if (payloadMatches.length === 0) {
        if (entry.required) {
          throw conflict(`page is missing its #${entry.payloadId} block`, entry.missingCode);
        }
        found[entry.key] = null;
        continue;
      }
      if (payloadMatches.length !== 1) {
        throw contractInvalid(`a managed page allows at most one #${entry.payloadId} block`);
      }
      if (payloadMatches[0].type !== DATA_TYPE) {
        throw contractInvalid(`#${entry.payloadId} must use type="${DATA_TYPE}"`);
      }
      found[entry.key] = { entry, schemaBlock: null, payloadBlock: payloadMatches[0] };
      continue;
    }

    const schemaMatches = matches[entry.schemaId];

    if (schemaMatches.length === 0 && payloadMatches.length === 0) {
      if (entry.required) {
        throw conflict(
          `page is not ${entry.label}-managed; add exactly one #${entry.schemaId} block and one #${entry.payloadId} block`,
          entry.missingCode
        );
      }
      found[entry.key] = null;
      continue;
    }
    if (schemaMatches.length !== 1 || payloadMatches.length !== 1) {
      throw contractInvalid(
        `managed pages require exactly one #${entry.schemaId} block and one #${entry.payloadId} block`
      );
    }

    const schemaBlock = schemaMatches[0];
    const payloadBlock = payloadMatches[0];
    if (schemaBlock.type !== SCHEMA_TYPE) {
      throw contractInvalid(`#${entry.schemaId} must use type="${SCHEMA_TYPE}"`);
    }
    if (payloadBlock.type !== DATA_TYPE) {
      throw contractInvalid(`#${entry.payloadId} must use type="${DATA_TYPE}"`);
    }
    found[entry.key] = { entry, schemaBlock, payloadBlock };
  }
  return found;
}

function parseJson(text, blockName, maxBytes) {
  if (utf8Bytes(text) > maxBytes) {
    throw contractInvalid(`#${blockName} exceeds its ${maxBytes}-byte limit`);
  }
  try {
    return JSON.parse(text);
  } catch {
    throw contractInvalid(`#${blockName} must contain valid JSON`);
  }
}

function assertSelfContainedSchema(schema, label = "data") {
  if (!isPlainObject(schema)) throw contractInvalid(`the ${label} schema root must be an object`);
  if (!JSON_SCHEMA_2020_12.has(schema.$schema)) {
    throw contractInvalid(`the ${label} schema must declare JSON Schema draft 2020-12 in $schema`);
  }
  if (schema.type !== "object") {
    throw contractInvalid(`the ${label} schema root must declare type "object"`);
  }

  let nodes = 0;
  const seen = new Set();
  function visit(value, depth) {
    if (value === null || typeof value !== "object") return;
    if (depth > MAX_SCHEMA_DEPTH) throw contractInvalid(`the ${label} schema is too deeply nested`);
    nodes++;
    if (nodes > MAX_SCHEMA_NODES) throw contractInvalid(`the ${label} schema is too complex`);
    if (seen.has(value)) throw contractInvalid(`the ${label} schema must be acyclic JSON`);
    seen.add(value);
    if (Array.isArray(value)) {
      for (const child of value) visit(child, depth + 1);
    } else {
      for (const [key, child] of Object.entries(value)) {
        if ((key === "$ref" || key === "$dynamicRef" || key === "$recursiveRef") && typeof child === "string") {
          if (child !== "" && !child.startsWith("#")) {
            throw contractInvalid("external schema references are not allowed", { keyword: key });
          }
        }
        if (key === "pattern" && typeof child === "string" && !isSafePattern(child)) {
          throw contractInvalid(
            `the ${label} schema uses a pattern that could backtrack catastrophically; simplify the regex`,
            { keyword: "pattern" }
          );
        }
        visit(child, depth + 1);
      }
    }
    seen.delete(value);
  }
  visit(schema, 0);
}

function boundedValidationErrors(errors) {
  const all = Array.isArray(errors) ? errors : [];
  return {
    validation_errors: all.slice(0, 12).map((error) => ({
      instance_path: String(error.instancePath || "").slice(0, 300),
      schema_path: String(error.schemaPath || "").slice(0, 300),
      keyword: String(error.keyword || "").slice(0, 100),
      message: String(error.message || "validation failed").slice(0, 300),
    })),
    total_errors: all.length,
  };
}

function compileSchema(schema, label = "data") {
  assertSelfContainedSchema(schema, label);
  try {
    const ajv = new Ajv2020({ allErrors: true, strict: true, validateFormats: true });
    addFormats(ajv);
    return ajv.compile(schema);
  } catch {
    throw contractInvalid(`the ${label} schema is not a valid self-contained JSON Schema 2020-12 document`);
  }
}

const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

function normalizeRfc3339(value, field, errorCode, currentContract = false) {
  if (typeof value !== "string" || !RFC3339.test(value)) {
    const message = `${field} must be an RFC3339 timestamp with a timezone`;
    throw currentContract ? contractInvalid(message) : badRequest(message, errorCode);
  }
  const timestamp = Date.parse(value);
  if (!Number.isFinite(timestamp)) {
    const message = `${field} must be a valid RFC3339 timestamp`;
    throw currentContract ? contractInvalid(message) : badRequest(message, errorCode);
  }
  return new Date(timestamp).toISOString();
}

function normalizeSourceAsOf(value, now = Date.now()) {
  const normalized = normalizeRfc3339(value, "source_as_of", "source_as_of_invalid");
  if (Date.parse(normalized) > Number(now) + FUTURE_TOLERANCE_MS) {
    throw badRequest(
      `source_as_of cannot be more than ${FUTURE_TOLERANCE_MS}ms in the future`,
      "source_in_future"
    );
  }
  return normalized;
}

function assertEnvelope(envelope) {
  if (!isPlainObject(envelope)) throw contractInvalid("#pages-data must contain an object envelope");
  const expectedKeys = ["contract_version", "data", "refreshed_at", "source_as_of"];
  const keys = Object.keys(envelope).sort();
  if (keys.length !== expectedKeys.length || keys.some((key, index) => key !== expectedKeys[index])) {
    throw contractInvalid(
      "#pages-data must contain exactly contract_version, refreshed_at, source_as_of, and data"
    );
  }
  if (envelope.contract_version !== 1) {
    throw contractInvalid("#pages-data contract_version must be 1");
  }
  normalizeRfc3339(envelope.refreshed_at, "refreshed_at", "data_contract_invalid", true);
  normalizeRfc3339(envelope.source_as_of, "source_as_of", "data_contract_invalid", true);
  if (!isPlainObject(envelope.data)) throw contractInvalid("#pages-data data must be an object");
}

// JSON has arbitrary-precision number literals; every JSON parser in this stack
// turns them into IEEE-754 doubles. So a payload carrying an integer above 2^53
// is quietly rewritten before Pages ever sees the digits — 9007199254740993 is
// stored as ...992, and 1.49e69 as "1.493135176553913e+69" — and then hashed and
// served as if it were what the caller sent. No schema catches it either:
// {"type":"integer"} is satisfied by 1.49e69, because Number.isInteger of a huge
// double is true, and the NWM-family schemas set no maximum.
//
// That is not hypothetical. A run summing a CSV column that pandas had typed as
// object produced a 70-digit "impression count" by string concatenation; the
// agent happened to notice before deploying, but nothing in the contract would
// have stopped it. Refuse the write instead, and say what to do about the one
// legitimate reason a payload holds a number that big: a large identifier, which
// belongs in a string.
//
// Called from materializeBlocks only, deliberately: that is the one seam every
// write passes through and no read does. parseManaged revalidates the stored
// payload on every read, so putting this in validatePayload would make any page
// already carrying such a number unreadable — and the corruption this catches
// leaves exactly that behind (9007199254740993 is stored as ...992, still over
// the limit). A serving page is not the place to discover a historical payload
// we would now refuse to accept.
function assertRepresentableNumbers(value, errorCode, label, path = "") {
  if (typeof value === "number") {
    if (Number.isInteger(value) && Math.abs(value) > Number.MAX_SAFE_INTEGER) {
      throw badRequest(
        `${label} carries an integer at ${path || "the root"} beyond IEEE-754 exact range ` +
          `(|value| > ${Number.MAX_SAFE_INTEGER}), so it cannot round-trip without silently changing. ` +
          "If this is a count, it is almost certainly a summing bug — check for string concatenation. " +
          "If it is an identifier, send it as a string.",
        errorCode
      );
    }
    return;
  }
  if (Array.isArray(value)) {
    value.forEach((item, index) => assertRepresentableNumbers(item, errorCode, label, `${path}/${index}`));
    return;
  }
  if (isPlainObject(value)) {
    for (const key of Object.keys(value)) {
      assertRepresentableNumbers(value[key], errorCode, label, `${path}/${key}`);
    }
  }
}

function validatePayload(validate, value, { errorCode, label = "data", maxBytes = MAX_DATA_BYTES }) {
  if (!isPlainObject(value)) {
    throw badRequest(`${label} must be an object`, errorCode, {
      validation_errors: [{ instance_path: "", schema_path: "#/type", keyword: "type", message: "must be object" }],
      total_errors: 1,
    });
  }
  let serialized;
  try {
    serialized = canonicalJson(value);
  } catch (error) {
    if (error && error.code) throw error;
    throw badRequest(`${label} must contain only JSON values`, errorCode);
  }
  if (utf8Bytes(serialized) > maxBytes) {
    throw badRequest(`${label} exceeds the ${maxBytes}-byte limit`, errorCode);
  }
  if (!validate(value)) {
    throw badRequest(
      `${label} does not satisfy the page's JSON Schema`,
      errorCode,
      boundedValidationErrors(validate.errors)
    );
  }
  return serialized;
}

function validateData(validate, data, errorCode) {
  return validatePayload(validate, data, { errorCode, label: "data", maxBytes: MAX_DATA_BYTES });
}

function validateConfigPayload(validate, config, errorCode) {
  return validatePayload(validate, config, { errorCode, label: "config", maxBytes: MAX_CONFIG_BYTES });
}

// parseManaged — read a managed document against `spec`. Returns the data pair's
// fields under their historical names (schema/envelope/dataBlock/validate) so
// every existing caller is untouched, plus the config pair when present.
function parseManaged(html, spec = PAGE_SPEC) {
  const found = findBlocks(html, spec);
  const { schemaBlock, payloadBlock: dataBlock } = found.data;
  const schema = parseJson(html.slice(schemaBlock.contentStart, schemaBlock.contentEnd), SCHEMA_ID, MAX_SCHEMA_BYTES);
  const envelope = parseJson(html.slice(dataBlock.contentStart, dataBlock.contentEnd), DATA_ID, MAX_DATA_BYTES);
  const validate = compileSchemaCached(schema, "data");
  assertEnvelope(envelope);
  assertCurrentPayload(
    () => validateData(validate, envelope.data, "data_contract_invalid"),
    `the current #${DATA_ID} payload does not satisfy its schema`
  );

  let configBlock = null;
  let configSchema = null;
  let config = null;
  let validateConfig = null;
  if (found.config) {
    const configSchemaBlock = found.config.schemaBlock;
    configBlock = found.config.payloadBlock;
    configSchema = parseJson(
      html.slice(configSchemaBlock.contentStart, configSchemaBlock.contentEnd),
      CONFIG_SCHEMA_ID,
      MAX_SCHEMA_BYTES
    );
    config = parseJson(html.slice(configBlock.contentStart, configBlock.contentEnd), CONFIG_ID, MAX_CONFIG_BYTES);
    validateConfig = compileSchemaCached(configSchema, "config");
    assertCurrentPayload(
      () => validateConfigPayload(validateConfig, config, "data_contract_invalid"),
      `the current #${CONFIG_ID} payload does not satisfy its schema`
    );
  }

  // Example data is preview-only, but it is still validated here: a design whose
  // example does not satisfy its own data schema would render a preview that no
  // real refresh could reproduce, which is worse than no preview at all.
  let exampleBlock = null;
  let example = null;
  if (found.example) {
    exampleBlock = found.example.payloadBlock;
    example = parseJson(html.slice(exampleBlock.contentStart, exampleBlock.contentEnd), EXAMPLE_ID, MAX_DATA_BYTES);
    assertCurrentPayload(
      () => validateData(validate, example, "data_contract_invalid"),
      `the #${EXAMPLE_ID} payload does not satisfy the #${SCHEMA_ID} schema`
    );
  }

  return {
    html,
    schema,
    envelope,
    config,
    configSchema,
    example,
    data_sha256: semanticHash(envelope.data),
    schema_sha256: semanticHash(schema),
    config_sha256: config === null ? null : semanticHash(config),
    config_schema_sha256: configSchema === null ? null : semanticHash(configSchema),
    template_sha256: sha256(templateIdentity(html, dataBlock)),
    dataBlock,
    configBlock,
    exampleBlock,
    validate,
    validateConfig,
  };
}

function parseManagedHtml(html) {
  return parseManaged(html, PAGE_SPEC);
}

// Re-label a schema failure on content ALREADY in the document: that is a broken
// contract (nothing the caller sent), not a rejected proposal.
function assertCurrentPayload(check, message) {
  try {
    check();
  } catch (error) {
    if (error && error.code === "data_contract_invalid") throw contractInvalid(message, error.details);
    throw error;
  }
}

// The template identity — "everything about these bytes except the refresh
// payload". Only the DATA block is elided. Config contents stay inline on
// purpose: to a data update, config is part of the immutable template it is
// pouring rows into, so editing config MUST change this hash. If it did not,
// update_page_data's (data, template, source) dedupe could match a version from
// before the config change and silently republish the old config.
//
// For a page with no config block this is byte-for-byte the original formula,
// so template_sha256 is stable for every page already deployed.
function templateIdentity(html, dataBlock) {
  return `${html.slice(0, dataBlock.contentStart)}${DATA_PLACEHOLDER}${html.slice(dataBlock.contentEnd)}`;
}

// ── inferring a config schema ────────────────────────────────────────────────
// A page and a template are the same artifact used two ways, and the only thing
// that used to stand between them was 4 KB of hand-written JSON Schema. Nobody
// separates design from data if separating costs that, so a page may ship
// #pages-config with no schema block and get one derived from the values.
//
// This is a real contract, not a rubber stamp: it pins types and rejects unknown
// keys, which is what catches a typo'd config key or a number sent as a string.
// It is deliberately NOT clever — no formats, no enums, no patterns — because a
// guess that rejects a legitimate future value is worse than a loose schema. A
// design that becomes a family should have its schema tightened by hand; the
// point here is that nothing is blocked on doing so first.
//
// Deterministic: the same config always yields the same schema, so redeploying
// the same file dedupes instead of minting a version.
const INFER_MAX_DEPTH = 12;

function inferSchema(value, depth = 0) {
  if (depth >= INFER_MAX_DEPTH) return {};
  if (value === null) return {}; // unknowable — never constrain it to null
  if (typeof value === "boolean") return { type: "boolean" };
  if (typeof value === "number") return Number.isInteger(value) ? { type: "integer" } : { type: "number" };
  if (typeof value === "string") return { type: "string" };
  if (Array.isArray(value)) {
    if (value.length === 0) return { type: "array" };
    // One items schema only when every element agrees. Inferring from the first
    // element of a heterogeneous array would reject the rest.
    const shapes = new Map();
    for (const entry of value) {
      const shape = inferSchema(entry, depth + 1);
      shapes.set(JSON.stringify(shape), shape);
    }
    return shapes.size === 1 ? { type: "array", items: [...shapes.values()][0] } : { type: "array" };
  }
  if (isPlainObject(value)) {
    const keys = Object.keys(value);
    // An empty object cannot be constrained: additionalProperties:false with no
    // properties would reject everything.
    if (keys.length === 0) return { type: "object" };

    const shapes = keys.map((key) => inferSchema(value[key], depth + 1));

    // A DICTIONARY, not a record. The campaign config's revshareMap is keyed by
    // deal code, so pinning its current keys with additionalProperties:false
    // would reject the next campaign's codes — the exact "guess that rejects a
    // legitimate future value" this is supposed to avoid. Recognised narrowly:
    // two or more keys whose values are all objects of the identical shape.
    // Scalars are excluded, so {sspLabel, dspLabel} stays a record and keeps its
    // typo protection.
    const distinct = new Set(shapes.map((shape) => JSON.stringify(shape)));
    const allObjects = keys.every((key) => isPlainObject(value[key]) && Object.keys(value[key]).length > 0);
    if (keys.length >= 2 && allObjects && distinct.size === 1) {
      return { type: "object", additionalProperties: shapes[0] };
    }

    const properties = {};
    // A null value reads as "not set", so those keys are optional. That is how a
    // campaign config expresses an open-ended flight end date.
    const required = [];
    keys.forEach((key, index) => {
      properties[key] = shapes[index];
      if (value[key] !== null) required.push(key);
    });
    const schema = { type: "object", additionalProperties: false, properties };
    if (required.length > 0) schema.required = required;
    return schema;
  }
  return {};
}

function inferConfigSchema(config) {
  const schema = { $schema: "https://json-schema.org/draft/2020-12/schema", ...inferSchema(config) };
  if (schema.type !== "object") {
    throw contractInvalid(`#${CONFIG_ID} must contain a JSON object so its schema can be derived`);
  }
  // Self-check. A generated schema that does not accept the values it was
  // generated from would fail the deploy it is supposed to enable, so catch it
  // here where the message can say what actually happened.
  const validate = compileSchemaCached(schema, "config");
  if (!validate(config)) {
    throw contractInvalid("could not derive a #pages-config-schema for this config", {
      errors: (validate.errors || []).slice(0, 5),
    });
  }
  return schema;
}

// ensureConfigSchema — the ONE place that tolerates a page carrying a config
// payload without its schema. Stored HTML always has the complete pair, so
// findBlocks/parseManaged keep their strict both-or-neither rule and every
// downstream reader is unchanged.
function ensureConfigSchema(html) {
  const unchanged = { html, generated: false, schema: null };
  if (typeof html !== "string" || !html.includes(CONFIG_ID)) return unchanged;
  // A fast negative: if the schema id appears anywhere there is nothing to add.
  if (html.includes(CONFIG_SCHEMA_ID)) return unchanged;

  const document = /<!doctype\s|<html(?:\s|>)|<head(?:\s|>)|<body(?:\s|>)/i.test(html)
    ? parse(html, { sourceCodeLocationInfo: true })
    : parseFragment(html, { sourceCodeLocationInfo: true });
  const matches = { [CONFIG_ID]: [], [CONFIG_SCHEMA_ID]: [] };
  walkHtml(document, matches);
  if (matches[CONFIG_SCHEMA_ID].length > 0) return unchanged;
  if (matches[CONFIG_ID].length === 0) return unchanged;
  if (matches[CONFIG_ID].length !== 1) {
    throw contractInvalid(`managed pages allow exactly one #${CONFIG_ID} block`);
  }

  const block = matches[CONFIG_ID][0];
  if (block.type !== DATA_TYPE) throw contractInvalid(`#${CONFIG_ID} must use type="${DATA_TYPE}"`);
  const config = parseJson(html.slice(block.contentStart, block.contentEnd), CONFIG_ID, MAX_CONFIG_BYTES);
  const schema = inferConfigSchema(config);
  const encoded = escapedJson(schema);
  if (utf8Bytes(encoded) > MAX_SCHEMA_BYTES) {
    throw contractInvalid(`the derived #${CONFIG_SCHEMA_ID} exceeds its ${MAX_SCHEMA_BYTES}-byte limit`);
  }
  // Immediately before the config block it describes, so the stored document
  // reads in the same order a hand-authored one would.
  const inserted = `<script type="${SCHEMA_TYPE}" id="${CONFIG_SCHEMA_ID}">${encoded}</script>\n`;
  return {
    html: html.slice(0, block.elementStart) + inserted + html.slice(block.elementStart),
    generated: true,
    schema,
  };
}

function escapedJson(value) {
  return JSON.stringify(value)
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/&/g, "\\u0026")
    .replace(/\u2028/g, "\\u2028")
    .replace(/\u2029/g, "\\u2029");
}

const escapedEnvelopeJson = escapedJson;

// Replace block contents in DESCENDING offset order so that every not-yet-applied
// block's offsets \u2014 computed against the original html \u2014 stay valid.
function spliceBlocks(html, replacements) {
  let out = html;
  for (const { block, text } of [...replacements].sort((a, b) => b.block.contentStart - a.block.contentStart)) {
    out = out.slice(0, block.contentStart) + text + out.slice(block.contentEnd);
  }
  return out;
}

// materializeBlocks \u2014 write a new payload into one or both managed blocks. An
// omitted key leaves that block's bytes untouched, which is what makes a config
// update provably data-preserving and a data update provably config-preserving.
function materializeBlocks(managed, { data, config } = {}, { sourceAsOf, now = Date.now() } = {}) {
  const replacements = [];
  let envelope = managed.envelope;
  let dataSha = managed.data_sha256;
  let configOut = managed.config;
  let configSha = managed.config_sha256;

  if (data !== undefined) {
    assertRepresentableNumbers(data, "data_validation_failed", "data");
    validateData(managed.validate, data, "data_validation_failed");
    envelope = {
      contract_version: 1,
      refreshed_at: new Date(Number(now)).toISOString(),
      source_as_of: normalizeSourceAsOf(sourceAsOf, now),
      data,
    };
    const encoded = escapedJson(envelope);
    if (utf8Bytes(encoded) > MAX_DATA_BYTES) {
      throw badRequest(`materialized data exceeds the ${MAX_DATA_BYTES}-byte limit`, "data_validation_failed");
    }
    replacements.push({ block: managed.dataBlock, text: encoded });
    dataSha = semanticHash(data);
  }

  if (config !== undefined) {
    if (!managed.configBlock || !managed.validateConfig) {
      throw conflict(
        `page has no #${CONFIG_ID} block to update; it was not built from a template`,
        "page_not_template_managed"
      );
    }
    assertRepresentableNumbers(config, "config_validation_failed", "config");
    validateConfigPayload(managed.validateConfig, config, "config_validation_failed");
    const encoded = escapedJson(config);
    if (utf8Bytes(encoded) > MAX_CONFIG_BYTES) {
      throw badRequest(`materialized config exceeds the ${MAX_CONFIG_BYTES}-byte limit`, "config_validation_failed");
    }
    replacements.push({ block: managed.configBlock, text: encoded });
    configOut = config;
    configSha = semanticHash(config);
  }

  // Example data is a property of the TEMPLATE, never of what the template
  // produces. Delete the whole element — not just its contents — from every
  // materialization, so a page cannot carry example rows and a rendered preview
  // shows exactly the bytes a page would have. Expressed as a splice over the
  // element's own offsets, so it rides the same descending-order machinery.
  if (managed.exampleBlock) {
    replacements.push({
      block: { contentStart: managed.exampleBlock.elementStart, contentEnd: managed.exampleBlock.elementEnd },
      text: "",
    });
  }

  const html = spliceBlocks(managed.html, replacements);
  if (utf8Bytes(html) > MAX_TEMPLATE_BYTES) {
    throw badRequest(`materialized page exceeds the ${MAX_TEMPLATE_BYTES}-byte limit`, "data_validation_failed");
  }

  // The new template identity, without any offset arithmetic: splice the same
  // replacements but put the placeholder where the data content goes. A config
  // change therefore changes template_sha256, and a data-only change cannot
  // (see templateIdentity for why that distinction is load-bearing).
  const identity = spliceBlocks(managed.html, [
    ...replacements.filter((entry) => entry.block !== managed.dataBlock),
    { block: managed.dataBlock, text: DATA_PLACEHOLDER },
  ]);

  return {
    html,
    envelope,
    config: configOut,
    data_sha256: dataSha,
    schema_sha256: managed.schema_sha256,
    config_sha256: configSha,
    config_schema_sha256: managed.config_schema_sha256,
    template_sha256: sha256(identity),
  };
}

function materialize(managed, data, sourceAsOf, now = Date.now()) {
  return materializeBlocks(managed, { data }, { sourceAsOf, now });
}

// assembleTemplate — turn a PAGE that already separates design from data into
// the template form, without moving the bytes.
//
// A page that carries #pages-config is already the whole design; promoting it is
// two edits, not a rewrite: replace its live data with the empty state a template
// must ship, and optionally keep a copy of that live data as preview-only example
// rows. Both are expressed as one descending splice, so no offset can go stale.
//
// This cannot go through materializeBlocks, which DELETES the example block —
// correct for a page or a preview, wrong here, because the example block is the
// one thing a template is supposed to keep.
function assembleTemplate(pageHtml, { emptyData, exampleData = null } = {}) {
  const managed = parseManaged(pageHtml, PAGE_SPEC);
  if (!managed.configBlock) {
    throw conflict(
      `this page has no #${CONFIG_ID} block, so there is nothing to vary per instance; ` +
        `separate its per-instance values into #${CONFIG_ID} first and redeploy`,
      "page_not_template_managed"
    );
  }
  validateData(managed.validate, emptyData, "data_validation_failed");
  if (exampleData !== null) validateData(managed.validate, exampleData, "data_validation_failed");

  // The empty state templates ship: epoch timestamps, so the first real ingest
  // into any page built from this can never be rejected as a regression.
  const envelope = {
    contract_version: 1,
    refreshed_at: new Date(0).toISOString(),
    source_as_of: new Date(0).toISOString(),
    data: emptyData,
  };
  const replacements = [{ block: managed.dataBlock, text: escapedJson(envelope) }];
  if (exampleData !== null) {
    const element = `\n<script type="${DATA_TYPE}" id="${EXAMPLE_ID}">${escapedJson(exampleData)}</script>`;
    // A zero-length range immediately after the data element: an insertion
    // through the same machinery as a replacement.
    replacements.push({
      block: { contentStart: managed.dataBlock.elementEnd, contentEnd: managed.dataBlock.elementEnd },
      text: element,
    });
  }

  const html = spliceBlocks(pageHtml, replacements);
  if (utf8Bytes(html) > MAX_TEMPLATE_BYTES) {
    throw badRequest(`the assembled template exceeds the ${MAX_TEMPLATE_BYTES}-byte limit`, "data_validation_failed");
  }
  // Prove it before returning it: the result has to satisfy the template contract
  // on its own, or registration would reject bytes this function produced.
  parseManaged(html, TEMPLATE_SPEC);
  return { html, config: managed.config, configSchema: managed.configSchema };
}

module.exports = {
  SCHEMA_ID,
  DATA_ID,
  CONFIG_SCHEMA_ID,
  CONFIG_ID,
  EXAMPLE_ID,
  SCHEMA_TYPE,
  DATA_TYPE,
  PAGE_SPEC,
  TEMPLATE_SPEC,
  MAX_SCHEMA_BYTES,
  MAX_DATA_BYTES,
  MAX_CONFIG_BYTES,
  MAX_TEMPLATE_BYTES,
  FUTURE_TOLERANCE_MS,
  canonicalJson,
  semanticHash,
  profileData,
  compareDataProfiles,
  assertExpectedProfile,
  DEFAULT_EXPECT_TOLERANCE,
  inferConfigSchema,
  ensureConfigSchema,
  assembleTemplate,
  normalizeSourceAsOf,
  parseManaged,
  parseManagedHtml,
  materialize,
  materializeBlocks,
  escapedJson,
  escapedEnvelopeJson,
  isSafePattern,
  compileCacheStats,
};
