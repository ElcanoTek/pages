// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";

// MCP Streamable HTTP endpoint. Transport/envelope/lifecycle validation is
// delegated to the official MCP TypeScript SDK; Pages only supplies strict
// host/origin/auth boundaries and typed adapters over its state machines.

const express = require("express");
const { McpServer } = require("@modelcontextprotocol/sdk/server/mcp.js");
const { StreamableHTTPServerTransport } = require("@modelcontextprotocol/sdk/server/streamableHttp.js");
const {
  LATEST_PROTOCOL_VERSION,
  SUPPORTED_PROTOCOL_VERSIONS,
  RequestIdSchema,
  JSONRPCMessageSchema,
  InitializeRequestSchema,
  ListToolsRequestSchema,
  CallToolRequestSchema,
  PingRequestSchema,
} = require("@modelcontextprotocol/sdk/types.js");
const tokens = require("./tokens");
const limits = require("./ratelimit");
const { ApiError, fromDbError } = require("./apierror");
const { DASHBOARD_ORIGIN } = require("./csp");
const { TOOLS, pageUrls } = require("./mcp-tools");
const { version: PKG_VERSION } = require("../package.json");

const PROTOCOL_VERSION = LATEST_PROTOCOL_VERSION;
const SUPPORTED_PROTOCOLS = Object.freeze([...SUPPORTED_PROTOCOL_VERSIONS]);
const SERVER_INFO = Object.freeze({ name: "pages", title: "Elcano Pages", version: PKG_VERSION });

const INSTRUCTIONS = [
  "Elcano Pages hosts versioned, Flag-themed client dashboards at stable live URLs.",
  "Use list_workspaces and filtered list_pages to discover existing work before creating anything.",
  "deploy_page creates a missing page and publishes by default on open pages; update_page requires an existing page. Pass expected_version when updating a page you previously read. These tools are for small inline HTML only.",
  "For HTML saved in a workspace file or over 20,000 UTF-8 bytes, never paste the whole file into deploy_page/update_page and never pass a path, $(cat...), or placeholder. Compute the exact file byte count and lowercase SHA-256, call start_page_upload, append the original bytes in ordered base64 chunks no larger than the returned max_chunk_bytes, then call deploy_page_upload. This preserves the complete file without model/tool-argument truncation; do not remove features or minify merely to fit one call. Use cancel_page_upload to discard an abandoned staged upload.",
  "Always follow page_is_live, version_is_live, live_version_id, and next_step. On approval-gated pages a new version waits for a human in urls.admin while an older published version may remain live.",
  "Use create_workspace, rename_workspace, and set_page_workspace for reversible organization. Workspace deletion, approval/rejection, takedown controls, theme changes, password clearing, and restoration remain human-admin-only.",
  "Set a client password before sharing client-only work. delete_page is a reversible soft delete, but confirm with the user first.",
  "When a user says 'update <slug> dashboard with ...' or otherwise asks to change a specific existing dashboard, call prepare_dashboard_update first with that exact slug and the user's complete plain-language request. If an older static client allowlist hides that canonical tool, call its read-only compatibility alias configure_page_refresh with the same arguments. Use update_type=data for values only, layout for design/schema/JavaScript, or auto when classification is genuinely unclear. Never create a replacement slug or companion data page.",
  "For a one-time request, follow the returned prompt now in this conversation. If it returns migration_required, run that same-slug migration prompt, then call prepare_dashboard_update again as instructed. For recurring=true, show the returned prompt to the user verbatim so they can place it in their scheduler of choice; do not execute it now, claim that Pages scheduled it, or require Pages, MOC, Chat, Fleet, or Cutlass configuration changes.",
  "When following a managed-data prompt, call get_page_data first, gather complete and fresh source data, map only the schema-defined data object, then call update_page_data with that live_version_id as expected_version and the latest represented source_as_of. Pages generates refreshed_at, preserves all layout/schema bytes, rejects source regressions, and dedupes exact retries. Never call the update when source data is missing, stale, or incomplete. After an ambiguous write or stale_version, reread once and compare schema/data hashes before at most one safe retry.",
].join(" ");

const DATA_UPDATE_INSTRUCTIONS = [
  "This token is restricted to Pages managed-data automation for exact granted slugs.",
  "Call get_page_data first and use its live_version_id as update_page_data.expected_version.",
  "Gather every required source and enforce freshness/completeness before writing. Never guess, send credentials, or update partial/stale data.",
  "Map a complete data object against the returned self-contained JSON Schema. source_as_of is the latest represented source coverage; Pages separately generates refreshed_at.",
  "update_page_data preserves all layout and schema bytes, rejects regression/future coverage, serializes writers, and dedupes exact template+data+source retries. publish defaults true; publish:false creates a canary.",
  "On stale_version or an ambiguous write, reread and compare schema/data hashes, then retry at most once only if the intended source coverage is not already represented.",
].join(" ");

// Subtrees Pages echoes back to the caller VERBATIM because the caller owns
// them: the managed data payload, the configs and schemas around it, and the
// profile computed from that payload. The id canonicalization below is
// a key-NAME heuristic, and a name heuristic has no business inside somebody
// else's document — a client's `campaign_id: 12345` came back as `"12345"` from
// every read and write, so the documented read-modify-write loop (get_page_data
// → edit → update_page_data) handed a string to a schema that says
// `{"type":"integer"}` and was refused on data Pages had just served. Under a
// permissive schema it stored a string where a number was, which flips the
// profile's `kind` from `number` to `key` and then breaks `expect.totals` on
// that path. The page HTML always held the real number, so the served page and
// the tool response disagreed about the same field.
const CALLER_OWNED_KEYS = new Set([
  "data", // the managed payload (DataEnvelopeSchema.data)
  "config", // a template-built page's deploy-time config
  "reference_config", // the config a template ships as a starting point
  "schema", // an embedded JSON Schema: `{"enum":[{"id":5}]}` must stay an integer
  "config_schema",
  "data_profile", // computed FROM the payload; its scalar keys are payload paths
]);

// Walks plain JSON only — never live objects. Applied AFTER JSON.stringify has
// already run toJSON, rendered Dates, and rejected BigInt/cyclic values, so this
// keeps those semantics exactly instead of reimplementing them. An earlier
// version walked the live value and silently mangled Buffers, skipped toJSON,
// and turned a cyclic TypeError into a RangeError.
function canonicalizeIds(value) {
  if (Array.isArray(value)) return value.map(canonicalizeIds);
  if (!value || typeof value !== "object") return value;
  const out = {};
  for (const key of Object.keys(value)) {
    const item = value[key];
    let next;
    if (CALLER_OWNED_KEYS.has(key)) next = item;
    else if ((key === "id" || key.endsWith("_id")) && Number.isSafeInteger(item) && item > 0) next = String(item);
    else next = canonicalizeIds(item);
    // defineProperty, not assignment: `__proto__` is a real own key after
    // JSON.parse, and `out[key] = …` would invoke Object.prototype's setter,
    // dropping the key from the response entirely.
    Object.defineProperty(out, key, { value: next, enumerable: true, writable: true, configurable: true });
  }
  return out;
}

function jsonValue(value) {
  // Converts Date values to RFC 3339 strings and rejects accidental BigInt,
  // undefined, or cyclic output before it reaches structuredContent. Numeric
  // IDs produced by a small number of domain helpers are canonicalized to the
  // same decimal-string representation node-postgres uses for BIGINT rows —
  // outside the caller-owned subtrees above.
  return canonicalizeIds(JSON.parse(JSON.stringify(value)));
}

function toolResult(data) {
  const structuredContent = jsonValue(data);
  return {
    content: [{ type: "text", text: JSON.stringify(structuredContent) }],
    structuredContent,
  };
}

function actorContext(extra) {
  const auth = extra && extra.authInfo;
  const metadata = auth && auth.extra;
  if (!metadata || !metadata.actor || metadata.actorType !== "agent") {
    throw new Error("authenticated MCP actor context is missing");
  }
  return Object.freeze({
    actor: metadata.actor,
    actorType: "agent",
    ip: metadata.ip || null,
    tokenId: metadata.tokenId,
    allowedSlugs: Array.isArray(metadata.allowedSlugs) ? metadata.allowedSlugs : [],
    scope: Array.isArray(auth.scopes) ? auth.scopes[0] : undefined,
    transport: "mcp",
    requestId: extra.requestId,
  });
}

function toolError(error, toolName, requestId) {
  const mapped = error instanceof ApiError ? error : fromDbError(error);
  if (mapped) {
    const body = {
      ok: false,
      error: mapped.message,
      code: mapped.code || "request_failed",
      ...(mapped.details === undefined ? {} : { details: mapped.details }),
    };
    return {
      content: [{ type: "text", text: JSON.stringify(body) }],
      structuredContent: body,
      isError: true,
    };
  }
  console.error(`mcp tool error [${toolName}, request ${String(requestId)}]:`, error && (error.stack || error.message));
  const body = { ok: false, error: "internal error", code: "internal_error" };
  return {
    content: [{ type: "text", text: JSON.stringify(body) }],
    structuredContent: body,
    isError: true,
  };
}

function createServer(agent = null) {
  const instructions = agent && agent.scope === "data_update" ? DATA_UPDATE_INSTRUCTIONS : INSTRUCTIONS;
  const server = new McpServer(SERVER_INFO, { instructions });
  for (const [name, tool] of Object.entries(TOOLS)) {
    if (agent && !tokens.isMcpToolAllowed(agent, name)) continue;
    server.registerTool(
      name,
      {
        title: tool.title,
        description: tool.description,
        inputSchema: tool.inputSchema,
        outputSchema: tool.outputSchema,
        annotations: tool.annotations,
      },
      async (args, extra) => {
        try {
          const ctx = actorContext(extra);
          tokens.authorizeMcpTool(ctx, name, args);
          return toolResult(await tool.handler(args, ctx));
        } catch (error) {
          return toolError(error, name, extra.requestId);
        }
      }
    );
  }
  // McpServer conservatively advertises listChanged:true when its first tool is
  // registered. Pages' registry is immutable for a process lifetime, so state
  // the narrower capability before connecting the transport.
  server.server.registerCapabilities({ tools: { listChanged: false } });
  return server;
}

function normalizeOrigin(value) {
  const candidate = String(value || "").trim();
  if (!/^[a-z][a-z0-9+.-]*:\/\/[^/?#]+$/i.test(candidate)) return null;
  try {
    const parsed = new URL(candidate);
    if (parsed.username || parsed.password || !["http:", "https:"].includes(parsed.protocol)) return null;
    return parsed.origin;
  } catch {
    return null;
  }
}

function normalizeHostname(value) {
  const candidate = String(value || "").trim().toLowerCase();
  if (!candidate || /[@/?#\\\s]/.test(candidate)) return null;
  try {
    const parsed = new URL(`http://${candidate}`);
    if (parsed.username || parsed.password) return null;
    return parsed.hostname.toLowerCase();
  } catch {
    return null;
  }
}

const ALLOWED_ORIGINS = new Set(
  [DASHBOARD_ORIGIN, ...(process.env.MCP_ALLOWED_ORIGINS || "").split(",")]
    .map((value) => value.trim())
    .filter(Boolean)
    .map(normalizeOrigin)
    .filter(Boolean)
);

const ALLOWED_HOSTS = new Set(
  [
    process.env.DASHBOARD_HOST || "pages.elcanotek.com",
    (() => {
      try {
        return new URL(DASHBOARD_ORIGIN).hostname;
      } catch {
        return null;
      }
    })(),
    ...(process.env.MCP_ALLOWED_HOSTS || "").split(","),
  ]
    .map(normalizeHostname)
    .filter(Boolean)
);

function requestHostname(req) {
  const host = String(req.headers.host || "");
  if (!host || /[@/?#\\\s]/.test(host)) return "";
  try {
    const parsed = new URL(`http://${host}`);
    if (parsed.username || parsed.password) return "";
    return parsed.hostname.toLowerCase();
  } catch {
    return "";
  }
}

function rpcError(id, code, message, data) {
  return {
    jsonrpc: "2.0",
    id,
    error: { code, message, ...(data === undefined ? {} : { data }) },
  };
}

function sendRpcError(res, status, code, message, id = null, headers = undefined) {
  if (headers) res.set(headers);
  return res.status(status).type("application/json").send(JSON.stringify(rpcError(id, code, message)));
}

function validateBoundary(req, res, next) {
  res.setHeader("Cache-Control", "no-store");
  const hostname = requestHostname(req);
  if (!hostname || !ALLOWED_HOSTS.has(hostname)) {
    return sendRpcError(res, 403, -32000, "Forbidden: invalid Host header");
  }
  const origin = req.headers.origin;
  if (origin !== undefined) {
    const normalized = normalizeOrigin(String(origin));
    if (!normalized || !ALLOWED_ORIGINS.has(normalized)) {
      return sendRpcError(res, 403, -32000, "Forbidden: invalid Origin header");
    }
    res.setHeader("Access-Control-Allow-Origin", normalized);
    res.setHeader("Vary", "Origin");
    res.setHeader("Access-Control-Allow-Methods", "POST, OPTIONS");
    res.setHeader(
      "Access-Control-Allow-Headers",
      "Authorization, Content-Type, Accept, MCP-Protocol-Version, MCP-Session-Id"
    );
    res.setHeader("Access-Control-Expose-Headers", "MCP-Protocol-Version, MCP-Session-Id, WWW-Authenticate");
  }
  next();
}

function attachAuthInfo(req, _res, next) {
  // The official transport passes req.auth into every handler as AuthInfo.
  // Use an opaque verified-token reference, never the bearer secret itself.
  req.auth = {
    token: `pages-token:${String(req.agent.tokenId)}`,
    clientId: req.agent.actor,
    scopes: [req.agent.scope],
    extra: {
      actor: req.agent.actor,
      actorType: req.agent.actorType,
      tokenId: req.agent.tokenId,
      allowedSlugs: req.agent.allowedSlugs,
      ip: req.ip,
    },
  };
  next();
}

function validatePostMediaType(req, res, next) {
  if (req.method !== "POST") return next();
  const mediaType = String(req.headers["content-type"] || "")
    .split(";", 1)[0]
    .trim()
    .toLowerCase();
  if (mediaType !== "application/json") {
    return sendRpcError(res, 415, -32000, "Unsupported Media Type: Content-Type must be application/json");
  }
  const accept = String(req.headers.accept || "").toLowerCase();
  const acceptedTypes = new Set(
    accept.split(",").flatMap((entry) => {
      const [type, ...params] = entry.split(";").map((value) => value.trim());
      const quality = params.find((value) => value.startsWith("q="));
      if (quality) {
        const q = Number(quality.slice(2));
        if (!Number.isFinite(q) || q <= 0 || q > 1) return [];
      }
      return type ? [type] : [];
    })
  );
  if (!acceptedTypes.has("application/json") || !acceptedTypes.has("text/event-stream")) {
    return sendRpcError(
      res,
      406,
      -32000,
      "Not Acceptable: client must accept both application/json and text/event-stream"
    );
  }
  // Media types are case-insensitive; normalize for the official SDK's strict
  // string check after the standards-aware parsing above.
  req.headers.accept = accept;
  next();
}

const router = express.Router();
router.use(validateBoundary);
router.options("/", (_req, res) => res.status(204).end());
router.use(limits.mcp);
router.use(tokens.requireBearer);
router.use(attachAuthInfo);
router.use(validatePostMediaType);
router.use(express.json({ limit: process.env.MAX_HTML_BYTES || "2mb", strict: false }));

router.post("/", async (req, res, next) => {
  const body = req.body;
  if (Array.isArray(body)) {
    return sendRpcError(res, 400, -32600, "Invalid Request: JSON-RPC batches are not supported");
  }
  if (!JSONRPCMessageSchema.safeParse(body).success) {
    return sendRpcError(res, 400, -32600, "Invalid Request");
  }

  const requestId = RequestIdSchema.safeParse(body.id);
  if (requestId.success && String(body.method).startsWith("notifications/")) {
    return res.json(rpcError(body.id, -32600, "Invalid Request: notifications must not include an id"));
  }
  const methodSchemas = {
    initialize: InitializeRequestSchema,
    "tools/list": ListToolsRequestSchema,
    "tools/call": CallToolRequestSchema,
    ping: PingRequestSchema,
  };
  const methodSchema =
    requestId.success && Object.prototype.hasOwnProperty.call(methodSchemas, body.method)
      ? methodSchemas[body.method]
      : null;
  if (methodSchema && !methodSchema.safeParse(body).success) {
    return res.json(rpcError(body.id, -32602, `Invalid params for ${body.method}`));
  }
  if (
    requestId.success &&
    body.method === "tools/list" &&
    body.params &&
    Object.prototype.hasOwnProperty.call(body.params, "cursor")
  ) {
    return res.json(rpcError(body.id, -32602, "Invalid cursor: the static tool list has no next page"));
  }

  // McpServer's tool registry is intentionally guarded with an own-property
  // check here: prototype-named/unknown tools are protocol Invalid Params and
  // can never resolve through the SDK's internal plain-object registry.
  if (
    body &&
    body.jsonrpc === "2.0" &&
    body.method === "tools/call" &&
    body.id !== undefined &&
    requestId.success &&
    body.params &&
    typeof body.params.name === "string" &&
    !Object.prototype.hasOwnProperty.call(TOOLS, body.params.name)
  ) {
    return res.json(rpcError(body.id, -32602, `Unknown tool: ${body.params.name}`));
  }

  const server = createServer(req.agent);
  const transport = new StreamableHTTPServerTransport({
    sessionIdGenerator: undefined,
    enableJsonResponse: true,
  });
  try {
    await server.connect(transport);
    await transport.handleRequest(req, res, body);
  } catch (error) {
    if (!res.headersSent) return sendRpcError(res, 500, -32603, "Internal error");
    console.error("mcp transport error:", error && (error.stack || error.message));
  } finally {
    await server.close().catch(() => {});
  }
});

function methodNotAllowed(_req, res) {
  res.setHeader("Allow", "POST, OPTIONS");
  return sendRpcError(res, 405, -32000, "Method not allowed; POST JSON-RPC to /mcp");
}

router.get("/", methodNotAllowed);
router.delete("/", methodNotAllowed);
router.all("/", methodNotAllowed);

// Keep every endpoint failure in MCP/JSON-RPC form, including body-parser
// failures that would otherwise become an Express HTML response.
// eslint-disable-next-line no-unused-vars
router.use((error, _req, res, _next) => {
  if (error && error.type === "entity.parse.failed") {
    return sendRpcError(res, 400, -32700, "Parse error: invalid JSON");
  }
  if (error && error.type === "entity.too.large") {
    return sendRpcError(res, 413, -32000, "Request body too large");
  }
  const mapped = error instanceof ApiError ? error : fromDbError(error);
  if (mapped) {
    const headers = mapped.status === 401 ? { "WWW-Authenticate": 'Bearer realm="pages"' } : undefined;
    return sendRpcError(res, mapped.status, -32001, mapped.message, null, headers);
  }
  console.error("mcp boundary error:", error && (error.stack || error.message));
  return sendRpcError(res, 500, -32603, "Internal error");
});

const TOOL_LIST = Object.freeze(Object.keys(TOOLS));

module.exports = {
  router,
  createServer,
  // Exported for tests: the id canonicalization is the one place Pages rewrites
  // a value on its way to a model, so what it does and does not touch is worth
  // pinning directly rather than only through a tool round trip.
  jsonValue,
  TOOLS,
  TOOL_LIST,
  PROTOCOL_VERSION,
  SUPPORTED_PROTOCOLS,
  SERVER_INFO,
  INSTRUCTIONS,
  DATA_UPDATE_INSTRUCTIONS,
  pageUrls,
};
