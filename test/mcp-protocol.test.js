// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/mcp-protocol.js against the official MCP TypeScript SDK it replaced.
//
// The SDK is a devDependency kept for exactly this: every case below runs the
// same JSON-RPC message through Pages' handler and through the SDK's own
// McpServer (connected over its in-memory transport) and requires identical
// responses. The HTTP framing is covered separately by
// test/mcp-wire.integration.js; this file covers the protocol layer, including
// paths no HTTP fixture can reach (output-schema validation, handler throws,
// a token that can see no tools).

const test = require("node:test");
const assert = require("node:assert/strict");
const z = require("zod/v4");
const { McpServer } = require("@modelcontextprotocol/sdk/server/mcp.js");
const { InMemoryTransport } = require("@modelcontextprotocol/sdk/inMemory.js");
const sdkTypes = require("@modelcontextprotocol/sdk/types.js");

const protocol = require("../lib/mcp-protocol");
const vendored = require("../lib/vendor/mcp-sdk/types");

const SERVER_INFO = { name: "northwind", title: "Northwind", version: "9.9.9" };
const INSTRUCTIONS = "Use the Northwind tools.";

// Synthetic tools that exercise every CallToolResult branch the SDK had.
const SYNTHETIC = [
  [
    "echo",
    {
      title: "Echo",
      description: "Echo a value.",
      inputSchema: z.object({ value: z.string().min(1), n: z.number().int().default(3) }).strict(),
      outputSchema: z.object({ value: z.string(), n: z.number() }).strict(),
      annotations: { readOnlyHint: true },
      handler: async (args) => ({ content: [{ type: "text", text: JSON.stringify(args) }], structuredContent: args }),
    },
  ],
  [
    "transform",
    {
      title: "Transform",
      description: "Arguments reach the handler parsed.",
      inputSchema: z.object({ id: z.union([z.number().int(), z.string()]).transform(String) }).strict(),
      outputSchema: z.object({ id: z.string() }).strict(),
      handler: async (args) => ({ content: [{ type: "text", text: args.id }], structuredContent: { id: args.id } }),
    },
  ],
  [
    "bad_output",
    {
      title: "Bad output",
      description: "Returns structured content its schema refuses.",
      inputSchema: z.object({}).strict(),
      outputSchema: z.object({ ok: z.literal(true), items: z.array(z.string()) }).strict(),
      handler: async () => ({ content: [{ type: "text", text: "{}" }], structuredContent: { ok: false, items: [1] } }),
    },
  ],
  [
    "no_structured",
    {
      title: "No structured content",
      description: "Omits structuredContent despite an output schema.",
      inputSchema: z.object({}).strict(),
      outputSchema: z.object({ ok: z.boolean() }).strict(),
      handler: async () => ({ content: [{ type: "text", text: "plain" }] }),
    },
  ],
  [
    "domain_error",
    {
      title: "Domain error",
      description: "An isError result skips output validation.",
      inputSchema: z.object({}).strict(),
      outputSchema: z.object({ ok: z.literal(true) }).strict(),
      handler: async () => ({
        content: [{ type: "text", text: '{"ok":false}' }],
        structuredContent: { ok: false },
        isError: true,
      }),
    },
  ],
  [
    "throws",
    {
      title: "Throws",
      description: "A handler that throws becomes a tool error.",
      inputSchema: z.object({}).strict(),
      outputSchema: z.object({}).strict(),
      handler: async () => {
        throw new Error("boom");
      },
    },
  ],
  [
    "throws_value",
    {
      title: "Throws a value",
      description: "A non-Error throw is stringified.",
      inputSchema: z.object({}).strict(),
      outputSchema: z.object({}).strict(),
      handler: async () => {
        throw "plain string"; // eslint-disable-line no-throw-literal
      },
    },
  ],
];

// The pre-swap lib/mcp.js server, verbatim in shape: McpServer + registerTool
// + the narrowed tools capability.
async function sdkReference(tools) {
  const server = new McpServer(SERVER_INFO, { instructions: INSTRUCTIONS });
  for (const [name, tool] of tools) {
    server.registerTool(
      name,
      {
        title: tool.title,
        description: tool.description,
        inputSchema: tool.inputSchema,
        outputSchema: tool.outputSchema,
        annotations: tool.annotations,
      },
      (args, extra) => tool.handler(args, extra)
    );
  }
  server.server.registerCapabilities({ tools: { listChanged: false } });
  const [serverSide, clientSide] = InMemoryTransport.createLinkedPair();
  await server.connect(serverSide);
  const pending = new Map();
  clientSide.onmessage = (message) => {
    const resolve = pending.get(message.id);
    if (resolve) {
      pending.delete(message.id);
      resolve(message);
    }
  };
  await clientSide.start();
  return {
    send(message) {
      return new Promise((resolve, reject) => {
        pending.set(message.id, resolve);
        clientSide.send(message).catch(reject);
        setTimeout(() => reject(new Error(`SDK reference did not answer ${JSON.stringify(message)}`)), 5000).unref();
      });
    },
    close: () => server.close(),
  };
}

function ours(tools) {
  return protocol.createMcpServer({ serverInfo: SERVER_INFO, instructions: INSTRUCTIONS, tools });
}

const CLIENT = { name: "contoso", version: "1" };
let nextId = 1;
const request = (method, params) => ({ jsonrpc: "2.0", id: nextId++, method, ...(params === undefined ? {} : { params }) });

// Requests that pass lib/mcp.js's envelope checks reach the protocol layer.
const CORPUS = () => [
  request("initialize", { protocolVersion: "2025-11-25", capabilities: {}, clientInfo: CLIENT }),
  request("initialize", { protocolVersion: "2024-11-05", capabilities: {}, clientInfo: CLIENT }),
  request("initialize", { protocolVersion: "2099-01-01", capabilities: {}, clientInfo: CLIENT }),
  request("initialize", { protocolVersion: "", capabilities: { roots: {} }, clientInfo: { ...CLIENT, title: "C" } }),
  request("ping"),
  request("ping", {}),
  request("ping", { _meta: { progressToken: "p" } }),
  request("ping", { task: {} }),
  request("tools/list"),
  request("tools/list", { _meta: {} }),
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
  request("resources/list"),
  request("prompts/list", {}),
  request("logging/setLevel", { level: "info" }),
  request("completion/complete", {}),
];

async function assertSameAnswers(tools, corpus) {
  const reference = await sdkReference(tools);
  try {
    const server = ours(tools);
    for (const message of corpus) {
      const expected = await reference.send(message);
      const actual = await server.handleRequest(structuredClone(message));
      // Byte-for-byte, so key order is part of the contract.
      assert.equal(JSON.stringify(actual), JSON.stringify(expected), `${message.method} ${JSON.stringify(message.params)}`);
    }
  } finally {
    await reference.close();
  }
}

test("mcp-protocol: answers like the SDK McpServer for every result and error branch", async () => {
  await assertSameAnswers(SYNTHETIC, CORPUS());
});

test("mcp-protocol: a server with no visible tools answers like the SDK (tools/* not found)", async () => {
  await assertSameAnswers([], CORPUS());
});

test("mcp-protocol: the real Pages catalog lists identically to the SDK", async () => {
  // Same schemas, same z.toJSONSchema settings, same key order: the advertised
  // catalog clients (and Anthropic's schema validator) see is unchanged.
  const { TOOLS } = require("../lib/mcp-tools");
  const tools = Object.entries(TOOLS).map(([name, tool]) => [name, { ...tool, handler: async () => ({ content: [] }) }]);
  await assertSameAnswers(tools, [request("tools/list"), request("tools/call", { name: "get_page", arguments: { bogus: 1 } })]);
});

test("mcp-protocol: version negotiation returns the client's version when supported, else the latest", async () => {
  const server = ours(SYNTHETIC);
  assert.deepEqual(protocol.SUPPORTED_PROTOCOL_VERSIONS, sdkTypes.SUPPORTED_PROTOCOL_VERSIONS);
  assert.equal(protocol.LATEST_PROTOCOL_VERSION, sdkTypes.LATEST_PROTOCOL_VERSION);
  for (const version of [...protocol.SUPPORTED_PROTOCOL_VERSIONS, "2023-01-01", "latest", "2025-11-25 "]) {
    const response = await server.handleRequest(
      request("initialize", { protocolVersion: version, capabilities: {}, clientInfo: CLIENT })
    );
    const expected = protocol.SUPPORTED_PROTOCOL_VERSIONS.includes(version) ? version : protocol.LATEST_PROTOCOL_VERSION;
    assert.equal(response.result.protocolVersion, expected, version);
    assert.deepEqual(response.result.capabilities, { tools: { listChanged: false } });
    assert.deepEqual(response.result.serverInfo, SERVER_INFO);
    assert.equal(response.result.instructions, INSTRUCTIONS);
  }
});

test("mcp-protocol: tool registration refuses duplicates and non-object schemas", () => {
  const tool = SYNTHETIC[0][1];
  assert.throws(() => ours([["echo", tool], ["echo", tool]]), /already registered/);
  assert.throws(() => ours([["x", { ...tool, inputSchema: z.string() }]]), /z\.object/);
  assert.throws(() => ours([["x", { ...tool, outputSchema: z.array(z.string()) }]]), /z\.object/);
});

test("mcp-protocol: tool arguments reach the handler parsed, with extra and requestId", async () => {
  let seen = null;
  const server = ours([
    [
      "probe",
      {
        ...SYNTHETIC[0][1],
        handler: async (args, extra) => {
          seen = { args, extra };
          return { content: [{ type: "text", text: "{}" }], structuredContent: { value: "x", n: 1 } };
        },
      },
    ],
  ]);
  const message = { jsonrpc: "2.0", id: "req-7", method: "tools/call", params: { name: "probe", arguments: { value: "v" } } };
  await server.handleRequest(message, { authInfo: { token: "opaque" } });
  assert.deepEqual(seen.args, { value: "v", n: 3 }, "zod defaults applied");
  assert.equal(seen.extra.requestId, "req-7");
  assert.deepEqual(seen.extra.authInfo, { token: "opaque" });
});

// The vendored envelope schemas must accept and refuse exactly what the SDK's
// do: lib/mcp.js uses them to decide 400 / -32600 / -32602 before dispatch.
test("mcp-protocol: vendored envelope schemas agree with the SDK on every probe", () => {
  const probes = [
    undefined, null, 0, "x", [], {},
    { jsonrpc: "2.0", id: 1, method: "ping" },
    { jsonrpc: "2.0", id: "a", method: "ping", params: {} },
    { jsonrpc: "2.0", id: -1, method: "ping" },
    { jsonrpc: "2.0", id: 1.5, method: "ping" },
    { jsonrpc: "2.0", id: null, method: "ping" },
    { jsonrpc: "2.0", id: true, method: "ping" },
    { jsonrpc: "1.0", id: 1, method: "ping" },
    { jsonrpc: "2.0", id: 1, method: 5 },
    { jsonrpc: "2.0", id: 1, method: "ping", params: [] },
    { jsonrpc: "2.0", id: 1, method: "ping", params: null },
    { jsonrpc: "2.0", id: 1, method: "ping", params: { _meta: null } },
    { jsonrpc: "2.0", id: 1, method: "ping", params: { _meta: { progressToken: 1.5 } } },
    { jsonrpc: "2.0", id: 1, method: "ping", params: { _meta: { "io.modelcontextprotocol/related-task": {} } } },
    { jsonrpc: "2.0", id: 1, method: "ping", params: { _meta: { "io.modelcontextprotocol/related-task": { taskId: "t" } } } },
    { jsonrpc: "2.0", id: 1, method: "ping", params: { task: 5 } },
    { jsonrpc: "2.0", id: 1, method: "ping", params: { task: {} } },
    { jsonrpc: "2.0", id: 1, method: "ping", extra: 1 },
    { jsonrpc: "2.0", method: "notifications/initialized" },
    { jsonrpc: "2.0", method: "notifications/initialized", params: {} },
    { jsonrpc: "2.0", method: "notifications/initialized", params: 1 },
    { jsonrpc: "2.0", id: 1, result: {} },
    { jsonrpc: "2.0", id: 1, result: [] },
    { jsonrpc: "2.0", id: 1, error: { code: 1, message: "m" } },
    { jsonrpc: "2.0", error: { code: 1, message: "m" } },
    { jsonrpc: "2.0", id: 1, error: { code: 1.5, message: "m" } },
    { jsonrpc: "2.0", id: 1, method: "tools/list", params: { cursor: "c" } },
    { jsonrpc: "2.0", id: 1, method: "tools/list", params: { cursor: 1 } },
    { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "x" } },
    { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "x", arguments: {} } },
    { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "x", arguments: [] } },
    { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "x", arguments: null } },
    { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: 1 } },
    { jsonrpc: "2.0", id: 1, method: "tools/call", params: { name: "x", task: { ttl: "1" } } },
    { jsonrpc: "2.0", id: 1, method: "tools/call" },
    ...[
      {},
      { roots: { listChanged: true } },
      { roots: 1 },
      { sampling: {} },
      { sampling: { context: null } },
      { elicitation: {} },
      { elicitation: { form: { applyDefaults: true } } },
      { elicitation: { form: { applyDefaults: "yes" } } },
      { elicitation: { url: 1 } },
      { elicitation: 5 },
      { experimental: { a: {} } },
      { experimental: { a: 1 } },
      { tasks: { list: {}, requests: { sampling: { createMessage: {} } } } },
      { tasks: { list: 1 } },
      { extensions: { "x/y": {} } },
    ].map((capabilities) => ({
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: { protocolVersion: "2025-11-25", capabilities, clientInfo: CLIENT },
    })),
    { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: 1, capabilities: {}, clientInfo: CLIENT } },
    { jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "v", capabilities: {}, clientInfo: { name: "n" } } },
    {
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: { protocolVersion: "v", capabilities: {}, clientInfo: { ...CLIENT, icons: [{ src: "a", theme: "dim" }] } },
    },
    {
      jsonrpc: "2.0",
      id: 1,
      method: "initialize",
      params: { protocolVersion: "v", capabilities: {}, clientInfo: { ...CLIENT, icons: [{ src: "a", sizes: ["48x48"] }] } },
    },
    { jsonrpc: "2.0", method: "initialize", params: { protocolVersion: "v", capabilities: {}, clientInfo: CLIENT } },
  ];
  const pairs = [
    "RequestIdSchema",
    "JSONRPCRequestSchema",
    "JSONRPCMessageSchema",
    "TaskAugmentedRequestParamsSchema",
    "InitializeRequestSchema",
    "PingRequestSchema",
    "ListToolsRequestSchema",
    "CallToolRequestSchema",
  ];
  for (const name of pairs) {
    for (const probe of probes) {
      for (const value of [probe, probe && probe.id, probe && probe.params]) {
        assert.equal(
          vendored[name].safeParse(value).success,
          sdkTypes[name].safeParse(value).success,
          `${name} disagrees on ${JSON.stringify(value)}`
        );
      }
    }
  }
});
