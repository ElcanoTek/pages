// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// Records how the JavaScript tool registry's zod input schemas answered a
// generated corpus of arguments (at the Node release's size defaults), for
// the Go port's ParseInput test: for each case either the parsed arguments
// (defaults applied, ids transformed) or the SDK's rendering of the issues.
//
//   node internal/mcp/testdata/dump-parse-corpus.js > internal/mcp/testdata/js-parse-corpus.json

const path = require("node:path");

const root = path.resolve(__dirname, "../../..");
for (const name of Object.keys(process.env)) {
  if (/^(PAGES_|MAX_HTML_BYTES|PAGE_UPLOAD_|DASHBOARD_|CONTENT_|MCP_)/.test(name)) delete process.env[name];
}
const { TOOLS } = require(path.join(root, "lib/mcp-tools.js"));
const catalog = require("./js-catalog.json");

// The SDK's issue rendering (lib/mcp-protocol.js parseErrorMessage).
function dotPath(p) {
  return p.reduce((acc, seg, i) => (i === 0 ? String(seg) : typeof seg === "number" ? `${acc}[${seg}]` : `${acc}.${seg}`), "");
}
function render(error) {
  return error.issues.map((is) => (is.path && is.path.length ? `${is.message} at ${dotPath(is.path)}` : is.message)).join("\n");
}

const SAMPLES = {
  "^[1-9][0-9]*$": "12",
  "^[0-9a-f]{64}$": "a".repeat(64),
  "^(?:[01][0-9]|2[0-3]):[0-5][0-9]$": "06:30",
  "^[a-z0-9]+(?:-[a-z0-9]+)*$": "first-steps",
};

function sample(schema) {
  if (!schema || typeof schema !== "object") return null;
  if (schema.default !== undefined) return schema.default;
  if (schema.const !== undefined) return schema.const;
  if (schema.enum) return schema.enum[0];
  if (schema.anyOf) return sample(schema.anyOf[schema.anyOf.length > 1 && schema.anyOf[0].type === "integer" ? 1 : 0]);
  const type = Array.isArray(schema.type) ? schema.type[0] : schema.type;
  switch (type) {
    case "string":
      if (schema.format === "date-time") return "2026-07-01T00:00:00Z";
      if (schema.format === "uuid") return "123e4567-e89b-42d3-a456-426614174000";
      if (schema.pattern && SAMPLES[schema.pattern]) return SAMPLES[schema.pattern];
      return "x".repeat(Math.max(schema.minLength || 1, 4));
    case "integer":
      return schema.minimum !== undefined ? schema.minimum : 1;
    case "number":
      return schema.minimum !== undefined ? schema.minimum : 0.5;
    case "boolean":
      return true;
    case "null":
      return null;
    case "array": {
      const n = schema.minItems || 1;
      return Array.from({ length: n }, () => sample(schema.items));
    }
    case "object": {
      if (schema.properties) {
        const out = {};
        for (const key of schema.required || []) out[key] = sample(schema.properties[key]);
        return out;
      }
      return { k: 1 };
    }
  }
  return null;
}

function full(schema) {
  // Every property, not only the required ones.
  if (schema && schema.type === "object" && schema.properties) {
    const out = {};
    for (const [key, prop] of Object.entries(schema.properties)) out[key] = prop.type === "object" && prop.properties ? full(prop) : sample(prop);
    return out;
  }
  return sample(schema);
}

const BAD = [null, true, false, 0, -1, 1.5, 1e21, "", "x", "12", "0", "99999999999999999999", "abc", [], [1], {}, { a: 1 }];

function variants(schema) {
  const out = [...BAD];
  if (schema.maxLength !== undefined) out.push("y".repeat(schema.maxLength), "y".repeat(schema.maxLength + 1));
  if (schema.minLength !== undefined && schema.minLength > 1) out.push("y".repeat(schema.minLength - 1));
  if (schema.maximum !== undefined) out.push(schema.maximum, schema.maximum + 1);
  if (schema.minimum !== undefined) out.push(schema.minimum - 1);
  if (schema.exclusiveMinimum !== undefined) out.push(schema.exclusiveMinimum);
  if (schema.maxItems !== undefined && schema.items) out.push(Array.from({ length: schema.maxItems + 1 }, () => sample(schema.items)));
  if (schema.minItems !== undefined && schema.items) out.push([]);
  if (schema.format === "date-time") out.push("2026-07-01", "2026-07-01T00:00:00+02:00", "2026-02-30T00:00:00Z", "2026-07-01T00:00:00.123456Z");
  if (schema.format === "uuid") out.push("123E4567-E89B-42D3-A456-426614174000", "123e4567-e89b-02d3-a456-426614174000");
  if (schema.type === "integer") out.push(9007199254740991, 9007199254740992, -9007199254740992);
  if (schema.anyOf) out.push(9223372036854775807, "9223372036854775807", "9223372036854775808");
  return out;
}

const cases = [];
function record(tool, args) {
  const parsed = TOOLS[tool].inputSchema.safeParse(args);
  cases.push(parsed.success ? { tool, args, data: parsed.data } : { tool, args, error: render(parsed.error) });
}

for (const entry of catalog.tools) {
  const name = entry.name;
  const schema = entry.inputSchema;
  const base = sample(schema);
  record(name, base);
  record(name, full(schema));
  record(name, {});
  record(name, { ...base, bogus: 1 });
  record(name, { ...base, bogus: 1, zz: 2 });
  for (const [key, prop] of Object.entries(schema.properties || {})) {
    const without = { ...base };
    delete without[key];
    record(name, without);
    for (const v of variants(prop)) record(name, { ...base, [key]: v });
    // One level down: object properties and array elements.
    const nested = prop.type === "object" && prop.properties ? prop : prop.type === "array" && prop.items && prop.items.properties ? prop.items : null;
    if (nested) {
      const inner = full(nested);
      for (const [k2, p2] of Object.entries(nested.properties)) {
        for (const v of [undefined, ...variants(p2)]) {
          const copy = { ...inner };
          if (v === undefined) delete copy[k2];
          else copy[k2] = v;
          record(name, { ...base, [key]: prop.type === "array" ? [copy] : copy });
        }
      }
      record(name, { ...base, [key]: prop.type === "array" ? [{ ...inner, extra: 1 }] : { ...inner, extra: 1 } });
    }
    if (prop.type === "object" && prop.additionalProperties && typeof prop.additionalProperties === "object") {
      for (const v of variants(prop.additionalProperties)) record(name, { ...base, [key]: { k: v, b: 2 } });
    }
  }
}

process.stdout.write(`${JSON.stringify(cases)}\n`);
