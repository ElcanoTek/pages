// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/mcp-protocol.js — the stateless MCP server core behind POST /mcp.
//
// Pages used to run the official MCP TypeScript SDK's McpServer and
// StreamableHTTPServerTransport for this. In stateless JSON-response mode (one
// transport per request, no sessions, no SSE, no batches) almost all of that
// machinery is idle: what remains is JSON-RPC 2.0 dispatch of four methods. This
// module is that remainder, written to answer exactly as SDK 1.32.1 did —
// methods, error codes and messages, result shapes and key order. That claim is
// held by tests rather than by this comment:
//   - test/mcp-wire.integration.js replays 83 requests and compares status,
//     headers and body with a golden recorded from the SDK transport;
//   - test/mcp-protocol.test.js runs a message corpus through this module and
//     through the SDK's own McpServer side by side;
//   - test/mcp.integration.js drives the endpoint with the SDK's real Client.
// The SDK stays a devDependency for those three checks only.
//
// HTTP concerns (Host/Origin, auth, media types, body parsing, status codes)
// live in lib/mcp.js. The envelope schemas are adapted from the SDK
// (lib/vendor/mcp-sdk/types.js, MIT).

const {
  LATEST_PROTOCOL_VERSION,
  SUPPORTED_PROTOCOL_VERSIONS,
  TaskAugmentedRequestParamsSchema,
  InitializeRequestSchema,
  PingRequestSchema,
  ListToolsRequestSchema,
  CallToolRequestSchema,
} = require("./vendor/mcp-sdk/types");
const z = require("zod/v4");

const ErrorCode = Object.freeze({
  ParseError: -32700,
  InvalidRequest: -32600,
  MethodNotFound: -32601,
  InvalidParams: -32602,
  InternalError: -32603,
});

// Same message shape as the SDK's McpError, which is what reaches a model when
// a tools/call fails before a Pages handler runs ("MCP error -32602: …").
class McpError extends Error {
  constructor(code, message, data) {
    super(`MCP error ${code}: ${message}`);
    this.code = code;
    this.data = data;
    this.name = "McpError";
  }
}

// The SDK's tool definitions carry this for every registerTool() tool.
const EXECUTION = Object.freeze({ taskSupport: "forbidden" });

function isObjectSchema(schema) {
  const def = schema && schema._zod && schema._zod.def;
  return Boolean(def && (def.type === "object" || def.shape !== undefined));
}

// The SDK's path formatting for zod issues: `edits[0].find`, `object root`.
function dotPath(path) {
  if (path.length === 0) return "object root";
  return path.reduce((acc, seg, index) => {
    if (index === 0) return String(seg);
    if (typeof seg === "number") return `${acc}[${seg}]`;
    return `${acc}.${seg}`;
  }, "");
}

function parseErrorMessage(error) {
  if (error && typeof error === "object") {
    if (Array.isArray(error.issues) && error.issues.length > 0) {
      return error.issues
        .map((issue) => (issue.path && issue.path.length ? `${issue.message} at ${dotPath(issue.path)}` : issue.message))
        .join("\n");
    }
    if (typeof error.message === "string") return error.message;
    try {
      return JSON.stringify(error);
    } catch {
      return String(error);
    }
  }
  return String(error);
}

function toolErrorResult(message) {
  return { content: [{ type: "text", text: message }], isError: true };
}

// tools/list entry, built once per tool: the registry is immutable for a
// process lifetime, and z.toJSONSchema is deterministic. Key order matches the
// SDK's (name, title, description, inputSchema, annotations, execution, _meta,
// outputSchema); JSON drops the undefined _meta exactly as it did there.
function toolDefinition(name, tool) {
  const definition = {
    name,
    title: tool.title,
    description: tool.description,
    inputSchema: z.toJSONSchema(tool.inputSchema, { target: "draft-7", io: "input" }),
    annotations: tool.annotations,
    execution: EXECUTION,
    _meta: undefined,
  };
  if (tool.outputSchema) {
    definition.outputSchema = z.toJSONSchema(tool.outputSchema, { target: "draft-7", io: "output" });
  }
  return Object.freeze(definition);
}

const definitionCache = new WeakMap();
function cachedDefinition(name, tool) {
  let definition = definitionCache.get(tool);
  if (!definition) {
    definition = toolDefinition(name, tool);
    definitionCache.set(tool, definition);
  }
  return definition;
}

// createMcpServer — one per HTTP request, like the SDK server it replaces.
//   serverInfo:   { name, title, version }
//   instructions: string
//   tools:        [[name, { title, description, inputSchema, outputSchema,
//                  annotations, handler(args, extra) }], ...] — handler returns a
//                  CallToolResult.
// handleRequest(message, extra) takes one valid JSON-RPC request and resolves to
// the JSON-RPC response object; it never rejects.
function createMcpServer({ serverInfo, instructions, tools }) {
  const registry = new Map();
  for (const [name, tool] of tools) {
    if (registry.has(name)) throw new Error(`Tool ${name} is already registered`);
    if (!isObjectSchema(tool.inputSchema) || (tool.outputSchema && !isObjectSchema(tool.outputSchema))) {
      throw new Error(`Tool ${name} must declare z.object input and output schemas`);
    }
    registry.set(name, tool);
  }
  // Pages' registry is immutable for a process lifetime, so tools never change
  // under a client: listChanged is false. The capability is declared even when a
  // token can see no tools, as the SDK server did.
  const capabilities = { tools: { listChanged: false } };

  async function callTool(request, extra) {
    const { name } = request.params;
    const tool = registry.get(name);
    try {
      if (!tool) throw new McpError(ErrorCode.InvalidParams, `Tool ${name} not found`);
      const parsed = await tool.inputSchema.safeParseAsync(request.params.arguments ?? {});
      if (!parsed.success) {
        throw new McpError(
          ErrorCode.InvalidParams,
          `Input validation error: Invalid arguments for tool ${name}: ${parseErrorMessage(parsed.error)}`
        );
      }
      const result = await tool.handler(parsed.data, extra);
      if (tool.outputSchema && !result.isError) {
        if (!result.structuredContent) {
          throw new McpError(
            ErrorCode.InvalidParams,
            `Output validation error: Tool ${name} has an output schema but no structured content was provided`
          );
        }
        const output = await tool.outputSchema.safeParseAsync(result.structuredContent);
        if (!output.success) {
          throw new McpError(
            ErrorCode.InvalidParams,
            `Output validation error: Invalid structured content for tool ${name}: ${parseErrorMessage(output.error)}`
          );
        }
      }
      return result;
    } catch (error) {
      // Tool-level failures stay on the result channel so the model sees them.
      return toolErrorResult(error instanceof Error ? error.message : String(error));
    }
  }

  const handlers = {
    initialize(request) {
      const requested = request.params.protocolVersion;
      return {
        protocolVersion: SUPPORTED_PROTOCOL_VERSIONS.includes(requested) ? requested : LATEST_PROTOCOL_VERSION,
        capabilities,
        serverInfo,
        ...(instructions && { instructions }),
      };
    },
    ping: () => ({}),
  };
  const schemas = {
    initialize: InitializeRequestSchema,
    ping: PingRequestSchema,
    "tools/list": ListToolsRequestSchema,
    "tools/call": CallToolRequestSchema,
  };
  if (registry.size > 0) {
    handlers["tools/list"] = () => ({
      tools: [...registry].map(([name, tool]) => cachedDefinition(name, tool)),
    });
    handlers["tools/call"] = callTool;
  }

  async function handleRequest(request, extra = {}) {
    const { id, method } = request;
    const handler = Object.prototype.hasOwnProperty.call(handlers, method) ? handlers[method] : undefined;
    if (!handler) {
      return { jsonrpc: "2.0", id, error: { code: ErrorCode.MethodNotFound, message: "Method not found" } };
    }
    try {
      // Task-augmented requests ("params.task") need a tasks capability that
      // Pages does not offer; refused before the handler, as the SDK did.
      if (TaskAugmentedRequestParamsSchema.safeParse(request.params).success && request.params.task) {
        throw new Error(`Server does not support task creation (required for ${method})`);
      }
      const parsed = schemas[method].parse(request);
      const result = await handler(parsed, { ...extra, requestId: id });
      return { result, jsonrpc: "2.0", id };
    } catch (error) {
      return {
        jsonrpc: "2.0",
        id,
        error: {
          code: Number.isSafeInteger(error && error.code) ? error.code : ErrorCode.InternalError,
          message: (error && error.message) ?? "Internal error",
          ...(error && error.data !== undefined && { data: error.data }),
        },
      };
    }
  }

  return { capabilities, handleRequest, tools: registry };
}

module.exports = {
  LATEST_PROTOCOL_VERSION,
  SUPPORTED_PROTOCOL_VERSIONS,
  ErrorCode,
  McpError,
  createMcpServer,
};
