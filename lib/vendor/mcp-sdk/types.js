// SPDX-License-Identifier: MIT
// Copyright (c) 2024 Anthropic, PBC
//
// Adapted from the MCP TypeScript SDK, @modelcontextprotocol/sdk 1.32.1,
// src/types.ts (dist/cjs/types.js in the published package). Trimmed by
// ElcanoTek, Inc. to the protocol constants and the request-envelope schemas
// Pages' stateless server validates (JSON-RPC message shapes, initialize, ping,
// tools/list, tools/call), and converted to CommonJS. Schema definitions are
// otherwise unchanged so a message the SDK accepted is accepted here and one it
// refused is refused. The full licence text is in ./LICENSE.
"use strict";

const z = require("zod/v4");

const LATEST_PROTOCOL_VERSION = "2025-11-25";
const SUPPORTED_PROTOCOL_VERSIONS = Object.freeze([LATEST_PROTOCOL_VERSION, "2025-06-18", "2025-03-26", "2024-11-05", "2024-10-07"]);
const RELATED_TASK_META_KEY = "io.modelcontextprotocol/related-task";
const JSONRPC_VERSION = "2.0";

const AssertObjectSchema = z.custom((v) => v !== null && (typeof v === "object" || typeof v === "function"));
const ProgressTokenSchema = z.union([z.string(), z.number().int()]);
const CursorSchema = z.string();
const TaskMetadataSchema = z.object({ ttl: z.number().optional() });
const RelatedTaskMetadataSchema = z.object({ taskId: z.string() });
const RequestMetaSchema = z.looseObject({
  progressToken: ProgressTokenSchema.optional(),
  [RELATED_TASK_META_KEY]: RelatedTaskMetadataSchema.optional(),
});
const BaseRequestParamsSchema = z.object({ _meta: RequestMetaSchema.optional() });
const TaskAugmentedRequestParamsSchema = BaseRequestParamsSchema.extend({ task: TaskMetadataSchema.optional() });
const RequestSchema = z.object({ method: z.string(), params: BaseRequestParamsSchema.loose().optional() });
const NotificationsParamsSchema = z.object({ _meta: RequestMetaSchema.optional() });
const NotificationSchema = z.object({ method: z.string(), params: NotificationsParamsSchema.loose().optional() });
const ResultSchema = z.looseObject({ _meta: RequestMetaSchema.optional() });
const RequestIdSchema = z.union([z.string(), z.number().int()]);

const JSONRPCRequestSchema = z
  .object({ jsonrpc: z.literal(JSONRPC_VERSION), id: RequestIdSchema, ...RequestSchema.shape })
  .strict();
const JSONRPCNotificationSchema = z.object({ jsonrpc: z.literal(JSONRPC_VERSION), ...NotificationSchema.shape }).strict();
const JSONRPCResultResponseSchema = z
  .object({ jsonrpc: z.literal(JSONRPC_VERSION), id: RequestIdSchema, result: ResultSchema })
  .strict();
const JSONRPCErrorResponseSchema = z
  .object({
    jsonrpc: z.literal(JSONRPC_VERSION),
    id: RequestIdSchema.optional(),
    error: z.object({ code: z.number().int(), message: z.string(), data: z.unknown().optional() }),
  })
  .strict();
const JSONRPCMessageSchema = z.union([
  JSONRPCRequestSchema,
  JSONRPCNotificationSchema,
  JSONRPCResultResponseSchema,
  JSONRPCErrorResponseSchema,
]);

const IconSchema = z.object({
  src: z.string(),
  mimeType: z.string().optional(),
  sizes: z.array(z.string()).optional(),
  theme: z.enum(["light", "dark"]).optional(),
});
const IconsSchema = z.object({ icons: z.array(IconSchema).optional() });
const BaseMetadataSchema = z.object({ name: z.string(), title: z.string().optional() });
const ImplementationSchema = BaseMetadataSchema.extend({
  ...BaseMetadataSchema.shape,
  ...IconsSchema.shape,
  version: z.string(),
  websiteUrl: z.string().optional(),
  description: z.string().optional(),
});

const FormElicitationCapabilitySchema = z.intersection(
  z.object({ applyDefaults: z.boolean().optional() }),
  z.record(z.string(), z.unknown())
);
const ElicitationCapabilitySchema = z.preprocess(
  (value) => {
    if (value && typeof value === "object" && !Array.isArray(value)) {
      if (Object.keys(value).length === 0) return { form: {} };
    }
    return value;
  },
  z.intersection(
    z.object({ form: FormElicitationCapabilitySchema.optional(), url: AssertObjectSchema.optional() }),
    z.record(z.string(), z.unknown()).optional()
  )
);
const ClientTasksCapabilitySchema = z.looseObject({
  list: AssertObjectSchema.optional(),
  cancel: AssertObjectSchema.optional(),
  requests: z
    .looseObject({
      sampling: z.looseObject({ createMessage: AssertObjectSchema.optional() }).optional(),
      elicitation: z.looseObject({ create: AssertObjectSchema.optional() }).optional(),
    })
    .optional(),
});
const ClientCapabilitiesSchema = z.object({
  experimental: z.record(z.string(), AssertObjectSchema).optional(),
  sampling: z.object({ context: AssertObjectSchema.optional(), tools: AssertObjectSchema.optional() }).optional(),
  elicitation: ElicitationCapabilitySchema.optional(),
  roots: z.object({ listChanged: z.boolean().optional() }).optional(),
  tasks: ClientTasksCapabilitySchema.optional(),
  extensions: z.record(z.string(), AssertObjectSchema).optional(),
});

const InitializeRequestParamsSchema = BaseRequestParamsSchema.extend({
  protocolVersion: z.string(),
  capabilities: ClientCapabilitiesSchema,
  clientInfo: ImplementationSchema,
});
const InitializeRequestSchema = RequestSchema.extend({
  method: z.literal("initialize"),
  params: InitializeRequestParamsSchema,
});
const PingRequestSchema = RequestSchema.extend({
  method: z.literal("ping"),
  params: BaseRequestParamsSchema.optional(),
});
const PaginatedRequestParamsSchema = BaseRequestParamsSchema.extend({ cursor: CursorSchema.optional() });
const PaginatedRequestSchema = RequestSchema.extend({ params: PaginatedRequestParamsSchema.optional() });
const ListToolsRequestSchema = PaginatedRequestSchema.extend({ method: z.literal("tools/list") });
const CallToolRequestParamsSchema = TaskAugmentedRequestParamsSchema.extend({
  name: z.string(),
  arguments: z.record(z.string(), z.unknown()).optional(),
});
const CallToolRequestSchema = RequestSchema.extend({
  method: z.literal("tools/call"),
  params: CallToolRequestParamsSchema,
});

module.exports = {
  LATEST_PROTOCOL_VERSION,
  SUPPORTED_PROTOCOL_VERSIONS,
  JSONRPC_VERSION,
  RequestIdSchema,
  JSONRPCRequestSchema,
  JSONRPCMessageSchema,
  TaskAugmentedRequestParamsSchema,
  InitializeRequestSchema,
  PingRequestSchema,
  ListToolsRequestSchema,
  CallToolRequestSchema,
};
