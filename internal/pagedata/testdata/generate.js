// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Regenerates the golden files the Go port of lib/page-data.js is tested
// against, by running the JavaScript implementation itself (plus Ajv, parse5
// and RegExp) over a fixed corpus. Run from this directory with the Node
// dependencies installed:
//
//   NODE_PATH=/path/to/node_modules node generate.js
//
// The outputs are committed; the Go tests never need Node.

const fs = require("node:fs");
const path = require("node:path");
const { parse, parseFragment } = require("parse5");
const Ajv2020 = require("ajv/dist/2020");
const addFormats = require("ajv-formats");
const pageData = require("../../../lib/page-data");

const out = (name, rows) => fs.writeFileSync(path.join(__dirname, name), rows.map((r) => JSON.stringify(r)).join("\n") + "\n");

// Deterministic PRNG (mulberry32).
function rng(seed) {
  return () => {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
const rand = rng(1009);
const pick = (arr) => arr[Math.floor(rand() * arr.length)];

function caught(fn) {
  try {
    return { ok: true, value: fn() };
  } catch (e) {
    return { ok: false, error: { status: e.status || null, code: e.code || null, message: e.message, details: e.details === undefined ? null : e.details } };
  }
}

// ── numbers ────────────────────────────────────────────────────────────────
{
  const rows = [];
  const values = [0, -0, 1, -1, 0.1, 0.2, 0.30000000000000004, 1e21, 1e-7, 1e-6, 999999999999999900000, 5e-324, 1.7976931348623157e308,
    0.0078125, 1.0000005, 2.5e-7, -0.0000005, 20044.102999999992, 9007199254740993, 123.456, 1 / 3, 2 / 3, 1e300, 1.5e-300];
  for (let k = 0; k < 64; k++) values.push(k / 128, -k / 1024, (2 * k + 1) / 2e6);
  for (let i = 0; i < 1500; i++) {
    const e = Math.floor(rand() * 60) - 30;
    values.push((rand() - 0.5) * 10 ** e);
    values.push(Math.round(rand() * 10 ** Math.floor(rand() * 18)) / 10 ** Math.floor(rand() * 8));
  }
  for (const v of values) rows.push({ text: JSON.stringify(v), str: String(v), fixed6: Math.abs(v) < 1e21 ? v.toFixed(6) : null, isNegZero: Object.is(v, -0) });
  out("numbers.jsonl", rows);
}

// ── canonical JSON / semantic hash ─────────────────────────────────────────
{
  const keys = ["a", "b", "B", "10", "2", "01", "é", "😀", "～", "ｚ", "", " ", "a b", "__proto__", "z\u2028", "<script>", "&"];
  const scalar = () => pick([null, true, false, 0, -0, 1.5, 1e21, 1e-7, "x", "<\u2028>", "é😀", 12345678901234567890, 0.1 + 0.2]);
  const gen = (d) => {
    const r = rand();
    if (d > 3 || r < 0.4) return scalar();
    if (r < 0.7) return Array.from({ length: Math.floor(rand() * 4) }, () => gen(d + 1));
    const o = {};
    for (let i = 0; i < Math.floor(rand() * 5); i++) Object.defineProperty(o, pick(keys), { value: gen(d + 1), enumerable: true, writable: true, configurable: true });
    return o;
  };
  const rows = [];
  for (let i = 0; i < 400; i++) {
    const v = gen(0);
    rows.push({ json: JSON.stringify(v), canonical: pageData.canonicalJson(v), sha: pageData.semanticHash(v), escaped: pageData.escapedJson(v) });
  }
  out("canonical.jsonl", rows);
}

// ── isSafePattern + RegExp semantics ───────────────────────────────────────
{
  const atoms = ["a", "b", "\\d", "\\w", "\\s", "\\S", ".", "[a-z]", "[^\\s]", "(a)", "(?:ab)", "(a|b)", "(a|a)", "-", "\\-", "x", "[0-9]", "(?<n>a)", "\\.", "\\p{L}", "é", "😀"];
  const quants = ["", "", "", "+", "*", "?", "{2}", "{1,3}", "{2,}", "+?"];
  const patterns = new Set(["^(a+)+$", "^(a|a)+$", "^[a-z0-9]+(?:-[a-z0-9]+)*$", "^(\\d{1,3}\\.){3}\\d{1,3}$", "(?=a)", "a{", "a{1", "\\1(a)", "(a)\\1", "[\\d-z]", "[a-\\d]", "\\c", "\\cA", "[\\cA]", "\\u{1F600}", "\\uD83D\\uDE00", "\\x4", "\\x41", "[]", "[^]", "\\0", "\\00", "a**", "^*", "(?:)", "", "|", "a|", "\\k<n>(?<n>a)", "\\p{Lu}", "\\P{Script=Greek}", "\\p{Letter}", "[\\s\\S]", "\\/", "\\a", "}", "]", "a{2,1}", "a{1001}", "\\b\\B", "(?<!a)b", "(?<=a)b", "[z-a]", "\\", "(", ")", "(?i:a)", "^(?=.*\\d)(?=.*[a-z]).{3,}$", "(a)\\1", "(?<x>ab)\\k<x>", "^(?!abc).*$", "^(a*)*b$", "^(?:(a)|b)+\\1$", "(?<=ab)c", "(?<!^)a", "^(?=(a+))\\1b", "\\b\\w+(?=-)", "(a{2})\\1{1,2}", "^(\\d)(?!\\1)\\d$", "a{1500}", "^(?:a|ab)(?=b)"]);
  for (let i = 0; i < 1500; i++) {
    let p = rand() < 0.5 ? "^" : "";
    const n = 1 + Math.floor(rand() * 4);
    for (let j = 0; j < n; j++) {
      let a = pick(atoms) + pick(quants);
      if (rand() < 0.2) a = `(${a}${pick(["", "|" + pick(atoms)])})${pick(quants)}`;
      p += a;
    }
    if (rand() < 0.5) p += "$";
    patterns.add(p);
  }
  const inputs = ["", "a", "aa", "ab", "abc", "abab", "a1b2", "xb", "aab", "ba", "12", "11", "aaaa", "ababc", "abb", "a-b", "1", "123", " ", "\u00a0", "\u2028", "\n", "é", "😀", "aaaaaaab", "A", "_", "x-y-z", "a.b", "-", "\t1a"];
  const rows = [];
  for (const p of patterns) {
    const row = { pattern: p, safe: pageData.isSafePattern(p) };
    for (const flags of ["u", ""]) {
      let re = null;
      try { re = new RegExp(p, flags); } catch (e) { row[`err_${flags || "none"}`] = true; }
      if (re) row[`match_${flags || "none"}`] = inputs.map((s) => re.test(s));
    }
    rows.push(row);
  }
  out("patterns.jsonl", [{ inputs }, ...rows]);
}

// ── HTML block location (parse5, as lib/page-data.js walks it) ──────────────
{
  const ids = ["pages-data", "pages-data-schema", "pages-config"];
  function walk(node, matches) {
    if (node && node.tagName === "script" && node.namespaceURI === "http://www.w3.org/1999/xhtml") {
      const attributes = new Map((node.attrs || []).map((a) => [a.name, a.value]));
      const id = attributes.get("id");
      if (id !== undefined && Object.prototype.hasOwnProperty.call(matches, id)) {
        const l = node.sourceCodeLocation;
        if (!l || !l.startTag || !l.endTag) throw new Error(`explicit:${id}`);
        matches[id].push({ type: attributes.get("type") ?? null, contentStart: l.startTag.endOffset, contentEnd: l.endTag.startOffset, elementStart: l.startOffset, elementEnd: l.endOffset });
      }
    }
    for (const c of (node && node.childNodes) || []) walk(c, matches);
  }
  const s = (id, extra = "") => `<script id="${id}" type="application/json"${extra}>{"x":1}</script>`;
  const docs = [
    s("pages-data"), `<p>é😀</p>${s("pages-data")}`, `<!doctype html><html><head>${s("pages-data")}</head><body>${s("pages-config")}</body></html>`,
    `<svg>${s("pages-data")}</svg>${s("pages-data")}`, `<svg><foreignObject>${s("pages-data")}</foreignObject></svg>`,
    `<math><mi>${s("pages-data")}</mi></math>`, `<svg><g>${s("pages-data")}</g><p>${s("pages-config")}</p>`,
    `<template>${s("pages-data")}</template>${s("pages-config")}`, `<!-- ${s("pages-data")} -->`, `<textarea>${s("pages-data")}</textarea>`,
    `<noscript>${s("pages-data")}</noscript>`, `<title>${s("pages-data")}</title>`, `<style>${s("pages-data")}</style>`,
    `<script>var a = "${s("pages-data").replace("</script>", "<\\/script>")}";</script>`, `<div data-x='${s("pages-data")}'></div>`,
    `<script id="pages-data" type="application/json">{"a":"<!--<script>"}</script>-->"}</script>`,
    `<script id="pages-data" type="application/json">{"a":1}`, `<SCRIPT ID="pages-data" TYPE="application/json">{}</SCRIPT >`,
    `<script id="pages-data" id="other" type="application/json" type="text/plain">{}</script>`, `<script id=pages-data>{}</script>`,
    `<script id="pages-data" type="application/json"/>{}</script>`, `<table><tr><td>${s("pages-data")}</td></tr>${s("pages-config")}</table>`,
    `<select>${s("pages-data")}</select>`, `<svg><desc>${s("pages-data")}</desc><title>x</title></svg>`, `<svg><script id="pages-data">x</script></svg><b>${s("pages-config")}</b>`,
    `\r\n${s("pages-data")}\r\n`, `\ufeff${s("pages-data")}`, `<plaintext>${s("pages-data")}`, `<iframe>${s("pages-data")}</iframe>${s("pages-config")}`,
    `<math><annotation-xml encoding="text/html">${s("pages-data")}</annotation-xml></math>`, `<svg><font color=red>${s("pages-data")}</font></svg>`,
    `<p>${s("pages-data")}${s("pages-data")}</p>`, `<script id="pages-data" type="application/json">a</script ><script id="pages-config" type="application/json">b</script\t>`,
  ];
  const rows = [];
  for (const html of docs) {
    for (const full of [false, true]) {
      const doc = full ? parse(html, { sourceCodeLocationInfo: true }) : parseFragment(html, { sourceCodeLocationInfo: true });
      const matches = Object.fromEntries(ids.map((id) => [id, []]));
      let error = null;
      try { walk(doc, matches); } catch (e) { error = e.message; }
      rows.push({ html, full, matches: error ? null : matches, error });
    }
  }
  out("html.jsonl", rows);
}

// ── Ajv ────────────────────────────────────────────────────────────────────
{
  const corpus = require("./ajv-corpus.json");
  const rows = [];
  for (const { schema, data } of corpus) {
    const row = {};
    try {
      const ajv = new Ajv2020({ allErrors: true, strict: true, validateFormats: true });
      addFormats(ajv);
      const validate = ajv.compile(schema);
      const valid = validate(data);
      Object.assign(row, { compile: true, valid, errors: (validate.errors || []).map((e) => [e.instancePath, e.schemaPath, e.keyword, e.message]) });
    } catch (e) {
      Object.assign(row, { compile: false, reason: e.message.slice(0, 160) });
    }
    rows.push(row);
  }
  out("ajv.jsonl", rows);
}

// ── profiles, coverage, comparison and expect ───────────────────────────────
{
  const days = (from, to) => Array.from({ length: to - from + 1 }, (_, i) => `2026-03-${String(from + i).padStart(2, "0")}`);
  const series = (dayList, deals, extra = {}) => dayList.flatMap((date, i) => deals.map((dealId, j) => ({ date, dealId, spend: (i + 1) * 10.1 + j * 0.37, imps: 1000 + i * j, ...extra })));
  const payloads = [
    { rows: series(days(1, 31), ["a", "b", "c", "d"]) },
    { rows: series(days(25, 31), ["b", "c", "d"]), dataThrough: "2026-03-31" },
    { rows: series(days(1, 31), ["a", "b", "c", "d"]).map((r) => ({ ...r, spend: r.spend + 1 })) },
    { rows: [], dataThrough: null, note: "x" },
    { a: { b: { c: { d: { e: { f: { g: [1, 2] } } } } } }, list: [1, 2, 3], when: "2026-01-01T00:00:00Z" },
    { rows: Array.from({ length: 80 }, (_, i) => ({ note: `unique note ${i}`, n: i % 3 === 0 ? null : i, flag: i % 2 === 0, mixed: i % 2 ? "2026-01-0" + (i % 9 + 1) : i })) },
    { rows: Array.from({ length: 30 }, (_, i) => ({ k: i < 25 ? `deal${i % 25}` : "Contoso | " + "x".repeat(130), v: 1e21 * i, w: 0.1 * i })) },
    Object.fromEntries(Array.from({ length: 12 }, (_, a) => [`arr${a}`, [{ x: a, d: "2026-02-0" + ((a % 9) + 1) }]])),
    Object.fromEntries(Array.from({ length: 30 }, (_, s) => [`s${s}`, s % 2 ? "2026-09-20" : s])),
    { rows: [{ ...Object.fromEntries(Array.from({ length: 30 }, (_, f) => [`f${f}`, `2026-01-${String((f % 28) + 1).padStart(2, "0")}${"z".repeat(f)}`])) }] },
    { rows: [{ "10": "a", "2": "b", z: "😀", "～": "2026-01-01", "😀": "2026-01-02" }, { "10": "😀", "2": "～" }] },
    (() => {
      const huge = `2026-01-01${"x".repeat(300)}`;
      const data = {};
      for (let a = 0; a < 8; a++) {
        const row = {};
        for (let f = 0; f < 24; f++) row[`date_field_with_a_rather_long_descriptive_name_${String(f).padStart(2, "0")}`] = f === 0 ? huge : "2026-09-20";
        data[`array_with_a_long_descriptive_name_${a}`] = [row, { ...row, date_field_with_a_rather_long_descriptive_name_00: "2025-01-01" }];
      }
      for (let s = 0; s < 24; s++) data[`stamp${s}`] = "2026-09-20T00:00:00.000Z";
      return data;
    })(),
  ];
  const rows = [];
  for (const p of payloads) {
    const profile = pageData.profileData(p);
    const bounded = pageData.boundedCoverageProfile(profile);
    const small = pageData.boundedCoverageProfile(profile, 300);
    rows.push({ kind: "profile", data: p, profile: JSON.stringify(profile), coverage: JSON.stringify(bounded.profile), truncated: bounded.truncated, small: JSON.stringify(small.profile), smallTruncated: small.truncated });
  }
  const expects = [undefined, { row_count: { rows: 124 } }, { totals: { "rows.spend": 1 } }, { date_range: { "rows.date": ["2026-03-01", "2026-03-31"] } }, { row_count: { other: 1 } }];
  for (let i = 0; i < payloads.length; i++) {
    for (let j = 0; j < payloads.length; j++) {
      if (i > 4 && j > 4 && i !== j) continue;
      const prev = pageData.profileData(payloads[i]);
      const next = pageData.profileData(payloads[j]);
      for (const expected of expects) {
        for (const shas of [false, true]) {
          const opts = { expected: expected === undefined ? null : expected };
          if (shas) Object.assign(opts, { previousDataSha: pageData.semanticHash(payloads[i]), nextDataSha: pageData.semanticHash(payloads[j]) });
          rows.push({ kind: "compare", prev: i, next: j, expected: expected === undefined ? null : expected, shas, warnings: JSON.stringify(pageData.compareDataProfiles(prev, next, opts)) });
        }
      }
    }
  }
  const expectCases = [null, 5, "x", [], { tolerance: 5 }, { tolerance: "0.5", totals: { "rows.spend": "17000" } }, { row_count: { rows: "124", nope: 1 } },
    { totals: { "rows.spend": 0, "rows.dealId": 1, "rows.imps": 125000 } }, { date_range: { "rows.date": ["2026-03-01", "2026-03-31"], "rows.spend": ["a", "b"] } },
    { date_range: { "rows.date": ["2026-03-01"] } }, { row_count: "ab" }, { row_count: [124] }, { totals: { "rows.spend": [5] } }, { tolerance: null, row_count: { rows: true } }];
  for (let i = 0; i < 3; i++) {
    for (const e of expectCases) {
      const r = caught(() => pageData.assertExpectedProfile(pageData.profileData(payloads[i]), e));
      rows.push({ kind: "expect", data: i, expect: e, error: r.ok ? null : r.error });
    }
  }
  out("profiles.jsonl", rows);
}

// ── managed documents: parse, materialize, infer, ensure, assemble ──────────
{
  const S = { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object", additionalProperties: false, required: ["count", "label"], properties: { count: { type: "integer", minimum: 0 }, label: { type: "string" }, rows: { type: "array" } } };
  const C = { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object", additionalProperties: false, required: ["campaign"], properties: { campaign: { type: "string", minLength: 1 }, deals: { type: "array", items: { type: "string" } } } };
  const env = (data, extra = {}) => ({ contract_version: 1, refreshed_at: "2026-07-17T00:00:00.000Z", source_as_of: "2026-07-16T23:00:00.000Z", data, ...extra });
  const blk = (id, v, type = "application/json") => `<script type="${type}" id="${id}">${typeof v === "string" ? v : JSON.stringify(v)}</script>`;
  const page = (parts, full = true) => (full ? `<!doctype html><html><head><title>Northwind</title></head><body>\n${parts.join("\n")}\n<footer>é😀</footer></body></html>` : parts.join("\n"));
  const dataPair = (data = { count: 3, label: "ready" }, schema = S, e = env(data)) => [blk("pages-data-schema", schema, "application/schema+json"), blk("pages-data", e)];
  const cfgPair = (cfg = { campaign: "Reference", deals: ["d1"] }, schema = C) => [blk("pages-config-schema", schema, "application/schema+json"), blk("pages-config", cfg)];
  const docs = [
    page(dataPair()), page(dataPair(), false), page([...cfgPair(), ...dataPair()]), page([...dataPair(), ...cfgPair()]),
    page([...dataPair(), ...cfgPair(), blk("pages-data-example", { count: 1, label: "example" })]),
    page([...dataPair({ count: 3, label: "ready" }, S, env({ count: 3, label: "ready" }, { refreshed_at: "2026-07-17T00:00:00Z" }))]),
    page(["<main>ordinary</main>"]), "", page([blk("pages-data", env({}))]), page([...dataPair(), blk("pages-data", env({}))]),
    page(dataPair().map((b) => b.replace("application/schema+json", "application/json"))),
    page([blk("pages-data-schema", "{", "application/schema+json"), blk("pages-data", env({ count: 1, label: "x" }))]),
    page(dataPair({ count: -1, label: "bad" })), page(dataPair({ count: 1.5, label: 4, extra: true })),
    page(dataPair({ count: 1, label: "x" }, { type: "object" })), page(dataPair({ count: 1, label: "x" }, { ...S, type: "array" })),
    page(dataPair({ count: 1, label: "x" }, { ...S, properties: { ...S.properties, label: { $ref: "https://example.com/x.json" } } })),
    page(dataPair({ code: "abc" }, { $schema: S.$schema, type: "object", required: ["code"], properties: { code: { type: "string", pattern: "^(a+)+$" } } })),
    page(dataPair({ code: "abc" }, { $schema: S.$schema, type: "object", required: ["code"], properties: { code: { type: "string", pattern: "^[a-z]+(?:-[a-z]+)*$" } } })),
    page(dataPair({ code: "abc" }, { $schema: S.$schema, type: "object", required: ["code"], properties: { code: { type: "string", pattern: "^(?=a)abc$" } } })),
    page(dataPair({ n: 1 }, { $schema: S.$schema, type: "object", required: ["n"] })),
    page(dataPair({ n: 1 }, { $schema: S.$schema, type: "object", properties: { n: { minimum: 0 } } })),
    page(dataPair({ n: 1 }, { $schema: S.$schema, type: "object", properties: { n: { type: "integer" } }, $defs: { x: { foo: 1 } } })),
    page(dataPair({ n: 1 }, { $schema: S.$schema, type: "object", properties: { n: { $ref: "#/$defs/n" } }, $defs: { n: { type: "integer", maximum: 0 } } })),
    page([...dataPair({ count: 1, label: "x" }, S, { ...env({ count: 1, label: "x" }), contract_version: 2 })]),
    page([...dataPair({ count: 1, label: "x" }, S, { ...env({ count: 1, label: "x" }), source_as_of: "2026-07-16" })]),
    page([...dataPair({ count: 1, label: "x" }, S, { ...env({ count: 1, label: "x" }), source_as_of: "2026-02-30T00:00:00Z" })]),
    page([...dataPair({ count: 1, label: "x" }, S, { ...env({ count: 1, label: "x" }), extra: 1 })]),
    page([...dataPair(), blk("pages-config", { campaign: "x" })]), page([...dataPair(), ...cfgPair({ campaign: 42 })]),
    page([...dataPair(), ...cfgPair(undefined, { type: "object" })]), page([...dataPair(), ...cfgPair(), blk("pages-data-example", { count: "x" })]),
    page([...dataPair(), ...cfgPair(), blk("pages-data-example", {}), blk("pages-data-example", {})]),
    page(dataPair({ count: 3, label: "</script><img>\u2028&" }).map((b) => b.replace("</script><img>", "\\u003c/script\\u003e\\u003cimg\\u003e"))),
    page([...dataPair({ count: 1, label: "x".repeat(3000) })]),
    `<script id="pages-data" type="application/json">{}`,
  ];
  const rows = [];
  const summarize = (m) => ({
    schema: m.schema, envelope: m.envelope, config: m.config, configSchema: m.configSchema, example: m.example,
    data_sha256: m.data_sha256, schema_sha256: m.schema_sha256, config_sha256: m.config_sha256, config_schema_sha256: m.config_schema_sha256,
    template_sha256: m.template_sha256, dataBlock: m.dataBlock && [m.dataBlock.contentStart, m.dataBlock.contentEnd],
  });
  const matSum = (r) => ({ html: r.html, envelope: r.envelope, config: r.config, data_sha256: r.data_sha256, schema_sha256: r.schema_sha256, config_sha256: r.config_sha256, config_schema_sha256: r.config_schema_sha256, template_sha256: r.template_sha256 });
  const now = Date.parse("2026-07-17T00:02:00Z");
  const writes = [
    { data: { count: 4, label: '</script><img src=x onerror="alert(1)">&\u2028' }, sourceAsOf: "2026-07-17T00:01:00Z" },
    { data: { count: 4, label: "x" }, sourceAsOf: "2026-07-17T01:00:00+01:00" },
    { data: { count: -2, label: 4 }, sourceAsOf: "2026-07-17T00:01:00Z" },
    { data: { count: 1, label: "x" }, sourceAsOf: "2026-07-17" },
    { data: { count: 1, label: "x" }, sourceAsOf: "2026-07-17T01:00:00Z" },
    { data: { count: 9007199254740993, label: "x" }, sourceAsOf: "2026-07-17T00:01:00Z" },
    { data: { count: 1, label: "x", rows: [{ impressions: 1e300 }] }, sourceAsOf: "2026-07-17T00:01:00Z" },
    { data: { count: 1, label: "x".repeat(2000) }, sourceAsOf: "2026-07-17T00:01:00Z" },
    { data: [1], sourceAsOf: "2026-07-17T00:01:00Z" },
    { data: { count: 2, label: "y" }, sourceAsOf: "2026-07-17T00:01:00.123456Z" },
    { config: { campaign: "Second", deals: ["a", "b"] } },
    { config: { campaign: "" } },
    { config: { campaign: "ok", unexpected: true } },
    { config: { campaign: "Third" }, data: { count: 5, label: "both" }, sourceAsOf: "2026-07-17T00:01:00Z" },
    { envelope: { contract_version: 1, refreshed_at: "2026-07-01T10:00:00.000Z", source_as_of: "2026-07-01T00:00:00.000Z", data: { count: 9, label: "Northwind" } } },
    { envelope: { contract_version: 1, refreshed_at: "2026-07-01T10:00:00.000Z", source_as_of: "2026-07-01T00:00:00.000Z", data: { count: "bad" } } },
    { envelope: { data: {} }, data: { count: 1, label: "x" } },
  ];
  for (const html of docs) {
    for (const spec of ["page", "template"]) {
      const r = caught(() => pageData.parseManaged(html, spec === "page" ? pageData.PAGE_SPEC : pageData.TEMPLATE_SPEC));
      const row = { kind: "parse", html, spec, result: r.ok ? summarize(r.value) : null, error: r.ok ? null : r.error };
      if (r.ok) {
        row.writes = writes.map((w) => {
          const blocks = {};
          if (w.data !== undefined) blocks.data = w.data;
          if (w.config !== undefined) blocks.config = w.config;
          if (w.envelope !== undefined) blocks.envelope = w.envelope;
          const m = caught(() => pageData.materializeBlocks(r.value, blocks, { sourceAsOf: w.sourceAsOf, now }));
          return { write: w, result: m.ok ? matSum(m.value) : null, error: m.ok ? null : m.error };
        });
      }
      rows.push(row);
    }
  }
  const configs = [{ client: "Acme", kpiTarget: 3.5, live: true, tags: ["a", "b"] }, { revshareMap: { X1: { pct: 50, type: "margin" }, X2: { pct: 40, type: "margin" } } },
    { sspLabel: "Index", dspLabel: "Amazon" }, { campaign: "x", flightEnd: null }, { mix: [{ a: 1 }, { b: 2 }] }, {}, [], "x", { "10": 1, a: { b: [[]] }, e: {} },
    JSON.parse('{"deep":' + '{"a":'.repeat(13) + "1" + "}".repeat(13) + "}"), { one: { a: 1 } }, { n: 1e21, m: -0, f: 1.5 }];
  for (const c of configs) {
    const r = caught(() => pageData.inferConfigSchema(c));
    rows.push({ kind: "infer", config: c, schema: r.ok ? JSON.stringify(r.value) : null, error: r.ok ? null : r.error });
  }
  const ensureDocs = [
    page([blk("pages-config", { client: "Acme" }), ...dataPair({ count: 1, label: "x" })]),
    page([blk("pages-config", { client: "Acme" }, "text/json"), ...dataPair()]),
    page([blk("pages-config", "{oops}"), ...dataPair()]),
    page([blk("pages-config", { a: 1 }), blk("pages-config", { a: 2 }), ...dataPair()]),
    page([...cfgPair(), ...dataPair()]), page(["<p>pages-config mentioned</p>"]), page([blk("pages-config", [1])]),
    page([`<!-- pages-config -->`, blk("pages-config", { "<b>": "&" })]),
  ];
  for (const html of ensureDocs) {
    const r = caught(() => pageData.ensureConfigSchema(html));
    rows.push({ kind: "ensure", html, result: r.ok ? { html: r.value.html, generated: r.value.generated, schema: r.value.schema } : null, error: r.ok ? null : r.error });
  }
  const promote = pageData.ensureConfigSchema(page([blk("pages-config", { client: "Acme Corp" }), blk("pages-data-schema", { $schema: S.$schema, type: "object", required: ["rows"], properties: { rows: { type: "array" } } }, "application/schema+json"), blk("pages-data", env({ rows: [{ secret: "acme-only" }] }))])).html;
  for (const [emptyData, exampleData] of [[{ rows: [] }, null], [{ rows: [] }, { rows: [{ secret: "acme-only" }] }], [{}, null], [undefined, null], [{ rows: [] }, { rows: 5 }]]) {
    const r = caught(() => pageData.assembleTemplate(promote, { emptyData, exampleData }));
    rows.push({ kind: "assemble", html: promote, emptyData: emptyData === undefined ? null : emptyData, exampleData, result: r.ok ? { html: r.value.html, config: r.value.config, configSchema: r.value.configSchema } : null, error: r.ok ? null : r.error });
  }
  const noCfg = page(dataPair());
  const r = caught(() => pageData.assembleTemplate(noCfg, { emptyData: { count: 0, label: "" } }));
  rows.push({ kind: "assemble", html: noCfg, emptyData: { count: 0, label: "" }, exampleData: null, result: null, error: r.error });
  out("managed.jsonl", rows);
}

// ── data-limit guidance text (internal/datalimits) ──────────────────────────
{
  const limits = require("../../../lib/data-limits");
  const { fileUploadGuidance } = require("../../../lib/upload-guidance");
  fs.mkdirSync(path.join(__dirname, "../../datalimits/testdata"), { recursive: true });
  fs.writeFileSync(path.join(__dirname, "../../datalimits/testdata/guidance.json"), JSON.stringify({
    data: limits.dataLimitGuidance(), page: fileUploadGuidance("page"), page_prefixed: fileUploadGuidance("page", "mcp_pages_"),
    data_upload: fileUploadGuidance("data"), data_prefixed: fileUploadGuidance("data", "mcp_pages_"),
    domain_error: (() => { try { limits.assertDomainSize(2 ** 21, "envelope"); } catch (e) { return { message: e.message, details: e.details }; } })(),
  }, null, 1) + "\n");
}

// ── page-patch (internal/pagepatch) ─────────────────────────────────────────
{
  const patch = require("../../../lib/page-patch");
  const docs = [
    "<style>.ctl-group{display:flex;}</style><button onclick='go()'>x</button>",
    "line one\n<div class=\"dr-popover\" id=\"drPopover\">\n" + "x".repeat(500) + "\n",
    "héllo 😀 world\n😀😀 tab\tnew line  spaced nbsp HELLO Hello",
    "hit ".repeat(30),
    "<b>x</b><b>x</b>",
    "a".repeat(300) + "NEEDLE" + "b".repeat(300),
    "\n\n\n   NEEDLE   \n\n",
  ];
  const rows = [];
  const edits = [
    [{ find: ".ctl-group{display:flex;", replace: ".ctl-group{position:relative;display:flex;" }],
    [{ find: "x", replace: "yy", count: 2 }], [{ find: "😀", replace: "☃", count: 3 }], [{ find: "hit", replace: "miss" }],
    [{ find: "<b>x</b>", replace: "<i>x</i>" }], [{ find: "one", replace: "two" }, { find: "two", replace: "three" }],
    [{ find: "NEEDLE", replace: "é".repeat(10) }], [], [{ find: "", replace: "x" }], [{ find: "a", replace: 5 }],
    [{ find: "a", replace: "b", count: 0 }], [{ find: "a", replace: "b", count: 1.5 }], [{ find: "a", replace: "b", count: null }],
    [{ find: "x".repeat(8193), replace: "" }], [{ find: "a", replace: "x".repeat(131073) }], null, [null], Array.from({ length: 26 }, () => ({ find: "a", replace: "b" })),
    [{ find: "a", replace: "b" }, { find: "", replace: "x" }], [{ find: "\n", replace: "\r\n", count: 2 }],
  ];
  for (const src of docs) {
    for (const [i, e] of edits.entries()) {
      const r = caught(() => patch.applyEdits(src, e));
      rows.push({ kind: "edit", src, edits: i, result: r.ok ? r.value : null, error: r.ok ? null : r.error });
    }
    for (const [query, opts] of [["x", {}], ["hello", { ignoreCase: true }], ["😀", {}], ["hit", { maxMatches: 5 }], ["NEEDLE", {}], ["b", { maxMatches: 0 }], ["", {}], ["dr-popover", {}], ["  ", {}], ["l", { maxMatches: 100 }]]) {
      const r = caught(() => patch.findMatches(src, query, opts));
      rows.push({ kind: "find", src, query, opts, result: r.ok ? r.value : null, error: r.ok ? null : r.error });
    }
  }
  fs.mkdirSync(path.join(__dirname, "../../pagepatch/testdata"), { recursive: true });
  fs.writeFileSync(path.join(__dirname, "../../pagepatch/testdata/patch.jsonl"), [{ edits }, ...rows].map((r) => JSON.stringify(r)).join("\n") + "\n");
}

// ── the shipped templates, end to end (hashes only; the HTML is in the repo) ─
{
  const crypto = require("node:crypto");
  const dir = path.join(__dirname, "../../../templates");
  const rows = [];
  for (const name of fs.readdirSync(dir).sort()) {
    const file = path.join(dir, name, "template.html");
    if (!fs.existsSync(file)) continue;
    const html = fs.readFileSync(file, "utf8");
    const parsed = pageData.parseManaged(html, pageData.TEMPLATE_SPEC);
    const data = parsed.example || parsed.envelope.data;
    const page = pageData.materializeBlocks(parsed, { config: parsed.config, data }, { sourceAsOf: "2026-01-01T00:00:00Z", now: Date.parse("2026-01-02T00:00:00Z") });
    const profile = pageData.profileData(data);
    rows.push({
      name, data_sha256: parsed.data_sha256, schema_sha256: parsed.schema_sha256, config_sha256: parsed.config_sha256,
      config_schema_sha256: parsed.config_schema_sha256, template_sha256: parsed.template_sha256,
      page_sha256: crypto.createHash("sha256").update(page.html).digest("hex"), page_template_sha256: page.template_sha256,
      profile: JSON.stringify(profile), coverage: JSON.stringify(pageData.coverageProfile(profile)),
    });
  }
  out("templates.jsonl", rows);
}
