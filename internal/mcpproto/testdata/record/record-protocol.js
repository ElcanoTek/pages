// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
//
// Recorders for the Go tests' expected outputs (run once, after `npm ci`
// for the SDK devDependency):
//   node mkprobes.js && node record-protocol.js <repo root>
//   node record-jsjson.js <repo root>
// Records SDK 1.32.1 McpServer answers for the mcp-protocol corpus, plus the
// vendored envelope schema verdicts, into internal/mcpproto/testdata.
"use strict";
const path = require("node:path");
const fs = require("node:fs");
const WT = process.argv[2];
const { createRequire } = require("node:module");
const r = createRequire(path.join(WT, "package.json"));
const z = r("zod/v4");
const { McpServer } = r("@modelcontextprotocol/sdk/server/mcp.js");
const { InMemoryTransport } = r("@modelcontextprotocol/sdk/inMemory.js");
const sdkTypes = r("@modelcontextprotocol/sdk/types.js");
const protocol = r("./lib/mcp-protocol");

const SERVER_INFO = { name: "northwind", title: "Northwind", version: "9.9.9" };
const INSTRUCTIONS = "Use the Northwind tools.";
const SYNTHETIC = [
  ["echo", { title: "Echo", description: "Echo a value.",
    inputSchema: z.object({ value: z.string().min(1), n: z.number().int().default(3) }).strict(),
    outputSchema: z.object({ value: z.string(), n: z.number() }).strict(),
    annotations: { readOnlyHint: true },
    handler: async (args) => ({ content: [{ type: "text", text: JSON.stringify(args) }], structuredContent: args }) }],
  ["transform", { title: "Transform", description: "Arguments reach the handler parsed.",
    inputSchema: z.object({ id: z.union([z.number().int(), z.string()]).transform(String) }).strict(),
    outputSchema: z.object({ id: z.string() }).strict(),
    handler: async (args) => ({ content: [{ type: "text", text: args.id }], structuredContent: { id: args.id } }) }],
  ["bad_output", { title: "Bad output", description: "Returns structured content its schema refuses.",
    inputSchema: z.object({}).strict(),
    outputSchema: z.object({ ok: z.literal(true), items: z.array(z.string()) }).strict(),
    handler: async () => ({ content: [{ type: "text", text: "{}" }], structuredContent: { ok: false, items: [1] } }) }],
  ["no_structured", { title: "No structured content", description: "Omits structuredContent despite an output schema.",
    inputSchema: z.object({}).strict(), outputSchema: z.object({ ok: z.boolean() }).strict(),
    handler: async () => ({ content: [{ type: "text", text: "plain" }] }) }],
  ["domain_error", { title: "Domain error", description: "An isError result skips output validation.",
    inputSchema: z.object({}).strict(), outputSchema: z.object({ ok: z.literal(true) }).strict(),
    handler: async () => ({ content: [{ type: "text", text: '{"ok":false}' }], structuredContent: { ok: false }, isError: true }) }],
  ["throws", { title: "Throws", description: "A handler that throws becomes a tool error.",
    inputSchema: z.object({}).strict(), outputSchema: z.object({}).strict(),
    handler: async () => { throw new Error("boom"); } }],
  ["throws_value", { title: "Throws a value", description: "A non-Error throw is stringified.",
    inputSchema: z.object({}).strict(), outputSchema: z.object({}).strict(),
    handler: async () => { throw "plain string"; } }],
];

async function sdkReference(tools) {
  const server = new McpServer(SERVER_INFO, { instructions: INSTRUCTIONS });
  for (const [name, tool] of tools) {
    server.registerTool(name, { title: tool.title, description: tool.description, inputSchema: tool.inputSchema,
      outputSchema: tool.outputSchema, annotations: tool.annotations }, (args, extra) => tool.handler(args, extra));
  }
  server.server.registerCapabilities({ tools: { listChanged: false } });
  const [serverSide, clientSide] = InMemoryTransport.createLinkedPair();
  await server.connect(serverSide);
  const pending = new Map();
  clientSide.onmessage = (m) => { const res = pending.get(m.id); if (res) { pending.delete(m.id); res(m); } };
  await clientSide.start();
  return {
    send(message) { return new Promise((resolve, reject) => { pending.set(message.id, resolve); clientSide.send(message).catch(reject); }); },
    close: () => server.close(),
  };
}

const CLIENT = { name: "contoso", version: "1" };
let nextId = 1;
const request = (method, params) => ({ jsonrpc: "2.0", id: nextId++, method, ...(params === undefined ? {} : { params }) });
const CORPUS = () => [
  request("initialize", { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: CLIENT }),
  request("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: CLIENT }),
  request("initialize", { protocolVersion: "2099-01-01", capabilities: {}, clientInfo: CLIENT }),
  request("initialize", { protocolVersion: "", capabilities: { roots: {} }, clientInfo: { ...CLIENT, title: "C" } }),
  request("ping"), request("ping", {}), request("ping", { _meta: { progressToken: "p" } }), request("ping", { task: {} }),
  request("tools/list"), request("tools/list", { _meta: {} }),
  request("tools/call", { name: "echo", arguments: { value: "hi" } }),
  request("tools/call", { name: "echo", arguments: { value: "hi", n: 7 } }),
  request("tools/call", { name: "echo", arguments: { value: "" } }),
  request("tools/call", { name: "echo", arguments: { value: "hi", extra: 1 } }),
  request("tools/call", { name: "echo" }),
  request("tools/call", { name: "echo", arguments: { value: 5, n: 1.5 } }),
  request("tools/call", { name: "transform", arguments: { id: 42 } }),
  request("tools/call", { name: "bad_output", arguments: {} }),
  request("tools/call", { name: "no_structured", arguments: {} }),
  request("tools/call", { name: "domain_error", arguments: {} }),
  request("tools/call", { name: "throws", arguments: {} }),
  request("tools/call", { name: "throws_value", arguments: {} }),
  request("tools/call", { name: "not_registered", arguments: {} }),
  request("tools/call", { name: "echo", arguments: { value: "hi" }, task: {} }),
  request("tools/call", { name: "echo", arguments: { value: "hi" }, _meta: { progressToken: 1 } }),
  request("resources/list"), request("prompts/list", {}), request("logging/setLevel", { level: "info" }),
  request("completion/complete", {}),
];

async function record(tools) {
  const ref = await sdkReference(tools);
  const ourServer = protocol.createMcpServer({ serverInfo: SERVER_INFO, instructions: INSTRUCTIONS, tools });
  const out = [];
  try {
    for (const message of CORPUS()) {
      const expected = JSON.stringify(await ref.send(message));
      const ours = JSON.stringify(await ourServer.handleRequest(structuredClone(message)));
      if (ours !== expected) throw new Error(`JS port disagrees with SDK on ${JSON.stringify(message)}`);
      out.push({ request: JSON.stringify(message), response: expected });
    }
  } finally { await ref.close(); }
  return out;
}

(async () => {
  const dir = path.join(WT, "internal/mcpproto/testdata");
  fs.mkdirSync(dir, { recursive: true });
  const corpus = { synthetic: await record(SYNTHETIC), empty: await record([]) };
  fs.writeFileSync(path.join(dir, "protocol-corpus.json"), JSON.stringify(corpus, null, 1) + "\n");

  // Envelope probes: the SDK's verdict for each schema on each value.
  const probes = JSON.parse(fs.readFileSync(path.join(__dirname, "probes.json"), "utf8"));
  const names = ["RequestIdSchema", "JSONRPCRequestSchema", "JSONRPCMessageSchema", "TaskAugmentedRequestParamsSchema",
    "InitializeRequestSchema", "PingRequestSchema", "ListToolsRequestSchema", "CallToolRequestSchema"];
  const verdicts = [];
  for (const probe of probes) {
    const values = probe === null ? [null] : [probe, probe && probe.id, probe && probe.params];
    for (const value of values) {
      if (value === undefined) continue; // the Go side has no undefined; "absent" is covered by the parents
      const row = { value: JSON.stringify(value) };
      for (const n of names) row[n] = sdkTypes[n].safeParse(value).success;
      verdicts.push(row);
    }
  }
  fs.writeFileSync(path.join(dir, "envelope-verdicts.json"), JSON.stringify(verdicts, null, 1) + "\n");
  console.log("recorded", corpus.synthetic.length, corpus.empty.length, verdicts.length);
})();
