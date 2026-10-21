// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
//
// Recorders for the Go tests' expected outputs (run once, after `npm ci`
// for the SDK devDependency):
//   node mkprobes.js && node record-protocol.js <repo root>
//   node record-jsjson.js <repo root>
// Records JSON.stringify(JSON.parse(text)) for a corpus of texts (as bytes).
"use strict";
const fs = require("node:fs");
const path = require("node:path");
const texts = [
  "0", "-0", "1", "1.0", "1e2", "1E+2", "1e21", "1e20", "123456789012345678901", "0.1", "0.000001", "0.0000001",
  "1.5e-7", "-1.5e-7", "5e-324", "1.7976931348623157e308", "1e400", "-1e400", "1e-400", "9007199254740993",
  "0.30000000000000004", "123.456", "1e-6", "12e-7", "2.5e+25", "100", "4294967295", "1.2345678901234567e-20",
  '"plain"', '"<>&\'"', '"\\u2028\\u2029"', '"\u2028"', '"\\u0000\\u001f\\u007f\\u0080"', '"\\b\\f\\n\\r\\t\\/\\"\\\\"',
  '"\\ud800"', '"\\udc00"', '"\\ud83d\\ude00"', '"\\ud83d"', '"a\\ud800b\\udfffc"', '"\\uD83D\\uDE00"', '"😀é"',
  '{"b":1,"a":2,"1":3,"0":4,"01":5,"4294967294":6,"4294967295":7,"-1":8,"1.5":9}',
  '{"a":1,"b":2,"a":3}', '{"__proto__":1,"x":{"__proto__":{"y":1}}}', '{"10":1,"9":2,"2":3,"x":4,"3":5}',
  '[1,"a",null,true,false,{},[]]', ' \t\r\n{ "a" : [ 1 , 2 ] } \n', '{"":1}', '[[[[[]]]]]',
];
const bad = ["", "{", "[1,]", "{'a':1}", "01", "1.", ".5", "+1", "NaN", "\"\t\"", "\"\\x\"", "tru", "[1] x", "\"\\u12\"", "-", "1e", "{\"a\" 1}", "\u00a01"];
const rawBytes = [
  Buffer.from([0x22, 0xff, 0x22]), Buffer.from([0x22, 0xe2, 0x82, 0x22]), Buffer.from([0x22, 0xed, 0xa0, 0x80, 0x22]),
  Buffer.from([0x22, 0xf0, 0x9f, 0x98, 0x22]), Buffer.from([0x22, 0xc0, 0xaf, 0x22]), Buffer.from([0x22, 0xf4, 0x90, 0x80, 0x80, 0x22]),
  Buffer.from([0x22, 0xe0, 0x80, 0x80, 0x41, 0x22]), Buffer.from([0x22, 0x80, 0x80, 0x22]),
];
const out = [];
for (const t of texts) out.push({ in: Buffer.from(t).toString("base64"), out: JSON.stringify(JSON.parse(t)) });
for (const t of bad) {
  let ok = true;
  try { JSON.parse(t); } catch { ok = false; }
  if (ok) throw new Error("expected failure " + t);
  out.push({ in: Buffer.from(t).toString("base64"), error: true });
}
for (const b of rawBytes) out.push({ in: b.toString("base64"), out: JSON.stringify(JSON.parse(b.toString("utf8"))) });
// Number formatting on computed values.
const numbers = [1 / 3, 2 / 3, 1e21 + 1, 123e-20, 0.5, 1e-7, 1.0000000000000002, 2 ** 53, 2 ** 64, -(2 ** 31), 1234.5678e10];
const nums = numbers.map((n) => ({ hex: Buffer.from(new Float64Array([n]).buffer).toString("hex"), out: String(n) }));
fs.writeFileSync(path.join(process.argv[2], "internal/mcpproto/jsjson/testdata/corpus.json"), JSON.stringify({ texts: out, numbers: nums }, null, 1) + "\n");
console.log("ok", out.length);
