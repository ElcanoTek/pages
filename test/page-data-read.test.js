// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// get_page_data's three read sizes, the managed prompt that chooses between
// them, and the export credential. No database: versions.getPageData is stubbed
// with what it returns for a real published page, built through the same parser.

const { test } = require("node:test");
const assert = require("node:assert/strict");
const crypto = require("node:crypto");

process.env.RAW_TOKEN_SECRET = "test-secret-do-not-use-in-prod";
process.env.PAGE_COOKIE_SECRET = process.env.PAGE_COOKIE_SECRET || "unit-test-secret";
delete process.env.DASHBOARD_ORIGIN;
delete process.env.CONTENT_ORIGIN;
process.env.DASHBOARD_HOST = "pages.elcanotek.com";
process.env.CONTENT_HOST = "elcano-pages.com";

const versions = require("../lib/versions");
const pageData = require("../lib/page-data");
const updatePrompts = require("../lib/update-prompts");
const rawtoken = require("../lib/rawtoken");
const dataExport = require("../lib/data-export");
const { TOOLS } = require("../lib/mcp-tools");
const { pageUrls } = require("../lib/mcp");

const SOURCE_AS_OF = "2026-09-21T00:00:00.000Z";
const block = (id, value, type = "application/json") => `<script id="${id}" type="${type}">${JSON.stringify(value)}</script>`;

// A tuple-row payload shaped like the largest production data page: a deal
// registry and ~990 KB of [dealId, date, revenue, margin, fee, impressions, clicks].
function tupleFixture() {
  const deals = Array.from({ length: 40 }, (_, i) => [`d${i}`, `Northwind_SSP_Contoso_${i}_Fabrikam_Display_Q3_2026_Open_Web_US_Standard`]);
  const rows = [];
  for (let i = 0, size = 0; size < 985_000; i++) {
    const day = new Date(Date.UTC(2025, 0, 1) + (i % 600) * 86400000).toISOString().slice(0, 10);
    const row = [`d${i % 40}`, day, 1000.25 + (i % 997), 310.5 + (i % 89), 12.75, 150000 + i, 300 + (i % 700)];
    rows.push(row);
    size += Buffer.byteLength(JSON.stringify(row)) + 1;
  }
  const schema = {
    $schema: "https://json-schema.org/draft/2020-12/schema",
    type: "object",
    additionalProperties: false,
    required: ["dataThrough", "lastRefreshed", "sourceDetail", "deals", "rows"],
    properties: {
      dataThrough: { type: "string" },
      lastRefreshed: { type: "string" },
      sourceDetail: { type: "string" },
      deals: { type: "array", items: { type: "array", items: { type: "string" } } },
      rows: { type: "array", items: { type: "array", minItems: 7, maxItems: 7 } },
    },
  };
  return {
    schema,
    data: {
      dataThrough: "2026-09-20",
      lastRefreshed: "2026-09-21T06:02:11.000Z",
      sourceDetail: "Canonical SSP history from the Northwind storage export, Index Exchange and Magnite, normalized to ET.",
      deals,
      rows,
    },
  };
}

// An object-row payload: several arrays, date and key fields, lots of numbers.
function objectFixture() {
  const daily = [];
  for (let i = 0, size = 0; size < 900_000; i++) {
    const row = {
      date: new Date(Date.UTC(2026, 0, 1) + (i % 260) * 86400000).toISOString().slice(0, 10),
      campaign: `Contoso ${i % 12}`, placement: `Fabrikam placement ${i % 300}`, dsp: ["Northwind", "Contoso"][i % 2],
      impressions: 1000 + i, clicks: i % 97, spend: 12.5 + (i % 31), conversions: i % 7,
    };
    daily.push(row);
    size += Buffer.byteLength(JSON.stringify(row)) + 1;
  }
  const weekly = Array.from({ length: 40 }, (_, i) => ({ week_start: `2026-${String(1 + (i % 9)).padStart(2, "0")}-0${1 + (i % 7)}`, spend: i * 10.5 }));
  const flights = Array.from({ length: 12 }, (_, i) => ({ flight: `F${i}`, starts: "2026-01-05", ends: "2026-12-20", budget: 5000 + i }));
  const schema = { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object" };
  return { schema, data: { dataThrough: "2026-09-18", note: "Northwind all-channel delivery", daily, weekly, flights, totals: { spend: 99.5 } } };
}

const FRESHNESS = Object.freeze({
  last_check_at: "2026-09-21T06:05:00.000Z",
  latest_outcome: "source_not_updated",
  latest_detail: "upstream max date still 2026-09-20; the storage export was re-uploaded without a new day",
  latest_source_as_of: "2026-09-20T00:00:00.000Z",
  source_as_of: SOURCE_AS_OF,
  refreshed_at: "2026-09-20T06:02:11.000Z",
  checked_at: "2026-09-21T06:05:00.000Z",
  last_check_outcome: "source_not_updated",
  last_check_detail: "upstream max date still 2026-09-20; the storage export was re-uploaded without a new day",
  last_check_source_as_of: "2026-09-20T00:00:00.000Z",
  days_since_source: 1,
  days_since_refresh: 2,
  days_since_check: 1,
});

// Exactly what versions.getPageData returns for a published managed page.
function publishedResult({ schema, data }, freshness = FRESHNESS) {
  const envelope = { contract_version: 1, refreshed_at: "2026-09-20T06:02:11.000Z", source_as_of: SOURCE_AS_OF, data };
  const html = "<!doctype html><html><head><title>Northwind</title></head><body><h1>Northwind</h1>" +
    block("pages-data-schema", schema, "application/schema+json") + block("pages-data", envelope) + "</body></html>";
  const managed = pageData.parseManagedHtml(html);
  return {
    page: {
      id: "17", slug: "northwind/overview", title: "Northwind Client Overview", client_id: null,
      workspace_id: "3", workspace_name: "Northwind", theme_id: null, theme_name: "flag",
      require_approval: false, disabled: false, published_version_id: "42",
      created_at: "2026-01-02T00:00:00.000Z", updated_at: "2026-09-20T06:02:11.000Z", has_password: true,
    },
    version: {
      id: "42", page_id: "17", content_sha256: crypto.createHash("sha256").update(html).digest("hex"),
      status: "approved", render_mode: "raw", author: "refresh-agent", source: "mcp", note: null,
      reviewed_by: null, reviewed_at: null, created_at: "2026-09-20T06:02:11.000Z",
    },
    schema: managed.schema,
    envelope: managed.envelope,
    data_sha256: managed.data_sha256,
    schema_sha256: managed.schema_sha256,
    template_sha256: managed.template_sha256,
    data_profile: pageData.profileData(managed.envelope.data),
    freshness,
  };
}

async function read(result, args, ctx = { tokenId: "9" }) {
  const original = versions.getPageData;
  versions.getPageData = async () => result;
  try {
    const tool = TOOLS.get_page_data;
    const out = JSON.parse(JSON.stringify(await tool.handler(tool.inputSchema.parse({ slug: result.page.slug, ...args }), ctx)));
    tool.outputSchema.parse(out);
    return out;
  } finally {
    versions.getPageData = original;
  }
}

const bytes = (value) => Buffer.byteLength(JSON.stringify(value), "utf8");

test("get_page_data summary: a ~1 MB payload's contract fits in 2 KB and carries no schema or rows", async (t) => {
  const tuple = publishedResult(tupleFixture());
  assert.ok(bytes(tuple.envelope.data) > 980_000, "the fixture is the size of the largest production payload");
  const full = await read(tuple, {});
  const summary = await read(tuple, { detail: "summary" });
  assert.ok(bytes(summary) <= 2048, `summary is ${bytes(summary)} bytes`);
  assert.ok(bytes(full) > 500 * bytes(summary), "and a small fraction of the full read");
  t.diagnostic(`tuple rows: full ${bytes(full)} bytes, summary ${bytes(summary)} bytes`);

  assert.equal(summary.detail, "summary");
  assert.deepEqual(summary.page, { slug: "northwind/overview", title: "Northwind Client Overview", disabled: false, require_approval: false });
  assert.equal(summary.live_version_id, "42");
  for (const hash of ["data_sha256", "schema_sha256", "template_sha256"]) assert.equal(summary[hash], full[hash]);
  assert.deepEqual(summary.envelope, { contract_version: 1, refreshed_at: "2026-09-20T06:02:11.000Z", source_as_of: SOURCE_AS_OF });
  assert.deepEqual(summary.freshness, FRESHNESS);
  assert.deepEqual(summary.coverage_profile, {
    arrays: { deals: { count: 40, fields: {} }, rows: { count: tuple.envelope.data.rows.length, fields: {} } },
    scalars: { dataThrough: "2026-09-20", lastRefreshed: "2026-09-21T06:02:11.000Z" },
  });
  for (const absent of ["schema", "version", "data_profile", "urls", "exports", "data_omitted"]) {
    assert.equal(Object.hasOwn(summary, absent), false, `summary omits ${absent}`);
  }
  assert.equal(Object.hasOwn(summary.envelope, "data"), false);
  assert.match(summary.next_step, /detail='export'/);

  // Object rows: date extents only — no sums, no key values, no prose scalars.
  const objects = publishedResult(objectFixture());
  const objectSummary = await read(objects, { detail: "summary" });
  assert.ok(bytes(objectSummary) <= 2048, `object-row summary is ${bytes(objectSummary)} bytes`);
  t.diagnostic(`object rows: summary ${bytes(objectSummary)} bytes`);
  const daily = objectSummary.coverage_profile.arrays.daily;
  assert.equal(daily.count, objects.envelope.data.daily.length);
  assert.deepEqual(Object.keys(daily.fields), ["date"]);
  assert.equal(daily.fields.date.kind, "date");
  assert.equal(daily.fields.date.min, "2026-01-01");
  assert.equal(typeof daily.fields.date.distinct, "number");
  assert.deepEqual(Object.keys(objectSummary.coverage_profile.arrays.flights.fields), ["starts", "ends"]);
  assert.deepEqual(objectSummary.coverage_profile.scalars, { dataThrough: "2026-09-18" });
  assert.doesNotMatch(JSON.stringify(objectSummary), /"sum"|"values"|Fabrikam placement/);
});

test("get_page_data summary: long recorded-check details are clipped, not the stamps beside them", async () => {
  const detail = "x".repeat(500);
  const result = publishedResult(tupleFixture(), { ...FRESHNESS, latest_detail: detail, last_check_detail: detail });
  const summary = await read(result, { detail: "summary" });
  assert.ok(bytes(summary) <= 2048, `worst-case summary is ${bytes(summary)} bytes`);
  assert.equal(summary.freshness.latest_detail.length, 160);
  assert.ok(summary.freshness.latest_detail.endsWith("…"));
  assert.equal(summary.freshness.checked_at, FRESHNESS.checked_at);
  assert.equal((await read(result, {})).freshness.latest_detail, detail, "a full read keeps the complete line");
});

test("get_page_data full: the default read is byte-for-byte what it was before detail existed", async () => {
  const result = publishedResult(tupleFixture());
  const urls = pageUrls(result.page.slug);
  // The pre-detail handler's exact construction, key order included.
  const snapshot = (omitData) => {
    const { data, ...coverage } = result.envelope;
    return JSON.stringify({
      page: result.page,
      version: result.version,
      schema: result.schema,
      data_sha256: result.data_sha256,
      schema_sha256: result.schema_sha256,
      template_sha256: result.template_sha256,
      data_profile: result.data_profile,
      freshness: result.freshness,
      version_is_live: true,
      page_is_live: true,
      live_version_id: "42",
      urls,
      ...(omitData ? { data_omitted: true } : {}),
      envelope: omitData ? coverage : { ...coverage, data },
    });
  };
  assert.equal(JSON.stringify(await read(result, {})), snapshot(false));
  assert.equal(JSON.stringify(await read(result, { detail: "full" })), snapshot(false));
  assert.equal(JSON.stringify(await read(result, { include_data: false })), snapshot(true));
  assert.equal(JSON.stringify(await read(result, { detail: "full", include_data: false })), snapshot(true));
});

test("get_page_data output schema holds each read mode to its own shape", async () => {
  const result = publishedResult(objectFixture());
  const schema = TOOLS.get_page_data.outputSchema;
  const full = await read(result, {});
  const summary = await read(result, { detail: "summary" });
  const exported = await read(result, { detail: "export" });
  const { data_profile: _profile, ...fullWithoutProfile } = full;
  assert.equal(schema.safeParse(fullWithoutProfile).success, false, "a full read cannot drop data_profile");
  assert.equal(schema.safeParse({ ...full, next_step: "x" }).success, false);
  assert.equal(schema.safeParse({ ...summary, schema: result.schema }).success, false, "a summary never carries the schema");
  assert.equal(schema.safeParse({ ...summary, envelope: result.envelope }).success, false, "…or the rows");
  assert.equal(schema.safeParse({ ...summary, page: result.page }).success, false);
  assert.equal(schema.safeParse({ ...exported, data_profile: result.data_profile }).success, false);
  const { exports: _exports, ...exportWithoutUrls } = exported;
  assert.equal(schema.safeParse(exportWithoutUrls).success, false);
  // The advertised MCP schema stays object-rooted, which the protocol requires.
  const advertised = require("zod").toJSONSchema(schema, { io: "output" });
  assert.equal(advertised.type, "object");
  assert.deepEqual(advertised.required.sort(), ["data_sha256", "envelope", "live_version_id", "page", "schema_sha256", "template_sha256"]);
});

test("get_page_data export: URLs for the live version, bound to the calling token", async () => {
  const result = publishedResult(objectFixture());
  const exported = await read(result, { detail: "export" }, { tokenId: "9" });
  assert.equal(exported.detail, "export");
  assert.equal(exported.exports.version_id, "42");
  const token = new URL(exported.exports.data_url).pathname.split("/")[2];
  for (const [part, url] of Object.entries({ schema: exported.exports.schema_url, data: exported.exports.data_url, envelope: exported.exports.envelope_url })) {
    assert.equal(url, `https://pages.elcanotek.com/export/${token}/${part}.json`, "served from the dashboard host only");
  }
  const claims = dataExport.verify(token);
  assert.deepEqual({ pid: claims.pid, vid: claims.vid, tid: claims.tid }, { pid: "17", vid: "42", tid: "9" });
  assert.ok(Date.parse(exported.exports.expires_at) - Date.now() <= dataExport.EXPORT_TTL_SECONDS * 1000);
  assert.equal(Object.hasOwn(exported.envelope, "data"), false);
  assert.match(exported.next_step, /download_url/);
  assert.match(exported.next_step, /Download schema_url and data_url once each/);
  assert.match(exported.next_step, /never share/i);
  await assert.rejects(read(result, { detail: "export" }, {}), /tokenId is missing/, "no token, no URL");
});

test("data export tokens: forged, expired and cross-audience credentials are refused", () => {
  const { token } = dataExport.mint({ pageId: "17", versionId: "42", tokenId: "9" });
  assert.ok(dataExport.verify(token));
  const [body, sig] = token.split(".");
  const flipped = `${body}.${sig.slice(0, -2)}${sig.slice(-2) === "AA" ? "BB" : "AA"}`;
  assert.equal(dataExport.verify(flipped), null, "signature tampering");
  const otherClaims = Buffer.from(JSON.stringify({ ...JSON.parse(Buffer.from(body, "base64url")), vid: "43" })).toString("base64url");
  assert.equal(dataExport.verify(`${otherClaims}.${sig}`), null, "claims cannot be moved to another version");
  assert.equal(dataExport.verify(dataExport.mint({ pageId: "17", versionId: "42", tokenId: "9" }, -1).token), null, "expired");
  assert.equal(dataExport.verify(`${token}.extra`), null);
  // Disjoint keys: a /raw render token never opens an export, and an export
  // token never renders at /raw, even though both derive from one secret.
  const view = rawtoken.mint({ pageId: 17, versionId: 42, purpose: "view", renderMode: "raw" });
  assert.equal(dataExport.verify(view), null);
  assert.equal(rawtoken.verify(token), null);
});

test("coverageProfile keeps counts and dates, and drops sums, key values and prose", () => {
  const profile = pageData.profileData({
    through: "2026-09-20",
    label: "Contoso",
    count: 4,
    rows: [{ day: "2026-09-01", deal: "a", spend: 1.5 }, { day: "2026-09-20", deal: "b", spend: 2 }],
    ids: ["a", "b", "c"],
  });
  assert.deepEqual(pageData.coverageProfile(profile), {
    arrays: {
      rows: { count: 2, fields: { day: { kind: "date", min: "2026-09-01", max: "2026-09-20", distinct: 2, nulls: 0 } } },
      ids: { count: 3, fields: {} },
    },
    scalars: { through: "2026-09-20" },
  });
});

test("managed prompts read the summary, fetch the contract once by reference, and forbid re-reads", () => {
  for (const recurring of [true, false]) {
    const prompt = updatePrompts.managedPrompt({
      slug: "northwind/overview", instructions: "Refresh from the Northwind export.", schemaSha256: "a".repeat(64), publish: true, recurring,
    });
    const step1 = prompt.split("\n").find((line) => line.startsWith("1. "));
    assert.match(step1, /^1\. Call mcp_pages_get_page_data for exactly northwind\/overview with detail="summary"/);
    // Exactly one instruction fetches the schema and rows: by export, or one
    // full read where the client cannot download URLs. Nothing else asks for it.
    const fullReads = prompt.split(/(?<=\.) /).filter((sentence) => /detail="(export|full)"/.test(sentence));
    assert.equal(fullReads.length, 1, fullReads.join("\n"));
    assert.match(fullReads[0], /ONCE into workspace files/);
    assert.match(fullReads[0], /download_url/);
    // The hash check names data_sha256, so the run must download the file that
    // hash is over (data_url) and hash its bytes, not re-canonicalize an
    // envelope in Python and chase false mismatches back into re-reads.
    assert.match(fullReads[0], /download its schema_url and data_url/);
    assert.match(fullReads[0], /exact bytes against schema_sha256 and data_sha256/);
    assert.equal(prompt.match(/detail="export"/g).length, 1);
    assert.equal(prompt.match(/detail="full"/g).length, 1);
    assert.doesNotMatch(prompt, /include_data/);
    assert.match(prompt, /That is this run's only full contract read/);
    assert.match(prompt, /Do not call mcp_pages_get_page_data or mcp_pages_get_page_config again to re-verify or after a context compaction/);
    assert.match(prompt, /stale_version or an ambiguous write response, reread once with detail="summary"/);
    assert.match(prompt, /refresh the contract files once as in step 1/);
    // A concurrent update_page_config is one cause of stale_version, and CONFIG
    // is in neither export: a retry must not build on superseded mappings.
    assert.match(prompt.split("\n").find((line) => line.startsWith("11. ")), /if this run used CONFIG registries in step 2, read them again once with mcp_pages_get_page_config/);
    assert.match(prompt.split("\n").find((line) => line.startsWith("11. ")), /reconcile against the new live version and its current CONFIG, and retry once/);
  }
});

test("a hostile payload cannot push the summary past its bound", async () => {
  // Eight arrays of 24 date-like fields with long names, a date-prefixed string
  // of 100 KB per array (it would be max verbatim), and 24 date-stamped scalars.
  const huge = `2026-01-01${"x".repeat(100_000)}`;
  const data = {};
  for (let a = 0; a < 8; a++) {
    const row = {};
    for (let f = 0; f < 24; f++) row[`date_field_with_a_rather_long_descriptive_name_${String(f).padStart(2, "0")}`] = f === 0 ? huge : "2026-09-20";
    data[`array_with_a_long_descriptive_name_${a}`] = [row, { ...row, date_field_with_a_rather_long_descriptive_name_00: "2025-01-01" }];
  }
  for (let s = 0; s < 24; s++) data[`stamp${s}`] = "2026-09-20T00:00:00.000Z";
  const result = publishedResult({ schema: { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object" }, data });
  const full = pageData.profileData(data);
  assert.ok(bytes(full) > 300_000, "the full profile really is hostile");

  const summary = await read(result, { detail: "summary" });
  assert.ok(bytes(summary.coverage_profile) <= pageData.COVERAGE_MAX_BYTES, `${bytes(summary.coverage_profile)} bytes`);
  assert.ok(bytes(summary) <= 5 * 1024, `summary is ${bytes(summary)} bytes`);
  assert.equal(summary.coverage_truncated, true);
  assert.match(summary.next_step, /coverage_truncated/);
  for (const entry of Object.values(summary.coverage_profile.arrays)) {
    for (const field of Object.values(entry.fields)) {
      assert.ok(field.min.length <= 32 && field.max.length <= 32, "extents are clipped");
    }
  }
  // Only whole entries are left out: every kept array still says its count.
  const first = Object.values(summary.coverage_profile.arrays)[0];
  assert.equal(first.count, 2);

  // A payload inside the bound is not marked, and its values are untouched.
  const normal = await read(publishedResult(tupleFixture()), { detail: "summary" });
  assert.equal(Object.hasOwn(normal, "coverage_truncated"), false);
  // Full reads never carry the marker.
  const schema = TOOLS.get_page_data.outputSchema;
  const fullRead = await read(publishedResult(tupleFixture()), {});
  assert.equal(schema.safeParse({ ...fullRead, coverage_truncated: true }).success, false);
});

test("coverage_truncated is set whenever any coverage is missing, not only when the byte bound cuts", async () => {
  const schema = { $schema: "https://json-schema.org/draft/2020-12/schema", type: "object" };
  const summaryOf = (data) => read(publishedResult({ schema, data }), { detail: "summary" });
  const small = { dataThrough: "2026-09-20", rows: [{ day: "2026-09-19" }, { day: "2026-09-20" }] };
  assert.equal(Object.hasOwn(await summaryOf(small), "coverage_truncated"), false, "a complete small page is not marked");

  // profileData's own caps: a ninth array, the 25th scalar, the 25th field, too deep.
  const nine = {};
  for (let i = 0; i < 9; i++) nine[`a${i}`] = [{ day: "2026-09-20" }];
  const ninth = await summaryOf(nine);
  assert.equal(ninth.coverage_truncated, true, "a ninth array");
  assert.equal(Object.keys(ninth.coverage_profile.arrays).length, 8);

  const scalars = {};
  for (let i = 0; i < 25; i++) scalars[`s${String(i).padStart(2, "0")}`] = "2026-09-20";
  assert.equal((await summaryOf(scalars)).coverage_truncated, true, "a 25th scalar");

  const wide = {};
  for (let i = 0; i < 24; i++) wide[`n${String(i).padStart(2, "0")}`] = i;
  wide.day = "2026-09-20";
  assert.equal((await summaryOf({ rows: [wide] })).coverage_truncated, true, "a 25th field");

  let deep = { day: "2026-09-20" };
  for (let i = 0; i < 8; i++) deep = { nested: deep };
  assert.equal((await summaryOf(deep)).coverage_truncated, true, "a level past the depth cap");

  // A clipped extent on an otherwise small page.
  const long = await summaryOf({ rows: [{ day: "2026-09-20T00:00:00.000Z and then a long note" }] });
  assert.equal(long.coverage_truncated, true, "a clipped extent");
  assert.ok(long.coverage_profile.arrays.rows.fields.day.max.length <= 32);

  // The full read's data_profile is unaffected: same bytes, and it still validates.
  const fullRead = await read(publishedResult({ schema, data: nine }), {});
  assert.equal(JSON.stringify(fullRead.data_profile), JSON.stringify(pageData.profileData(nine)));
  // …and the handler's own objects (not a JSON round trip) validate, so nothing
  // that marks a capped profile rides on the profile itself.
  const original = versions.getPageData;
  versions.getPageData = async () => publishedResult({ schema, data: { rows: [wide] } });
  try {
    for (const detail of ["full", "summary"]) {
      const raw = await TOOLS.get_page_data.handler({ slug: "northwind/overview", detail }, { tokenId: "9" });
      assert.ok(TOOLS.get_page_data.outputSchema.safeParse(raw).success, detail);
    }
  } finally {
    versions.getPageData = original;
  }
});

test("the compact reads clip an oversize legacy title; the full read keeps it", async () => {
  // POST /api/v1/pages takes a title without the 200-character MCP limit.
  const result = publishedResult(tupleFixture());
  result.page = { ...result.page, title: "Northwind ".repeat(50_000) };
  for (const detail of ["summary", "export"]) {
    const out = await read(result, { detail });
    assert.equal(out.page.title.length, 200, detail);
    assert.ok(out.page.title.endsWith("…"));
    if (detail === "summary") assert.ok(bytes(out) <= 5 * 1024, `summary is ${bytes(out)} bytes`);
  }
  assert.equal((await read(result, {})).page.title, result.page.title, "a full read is unchanged");
  const normal = publishedResult(tupleFixture());
  assert.equal((await read(normal, { detail: "summary" })).page.title, normal.page.title, "a normal title is untouched");
});
