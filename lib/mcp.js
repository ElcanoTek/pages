"use strict";
// lib/mcp.js — MCP-over-HTTP at /mcp (PLAN.md §11). The agent-native surface for
// chat & cutlass. It is a THIN wrapper over the exact same state machine
// (lib/versions.js) the REST API uses — there is no privileged path that skips
// the gating, dedupe, optimistic-concurrency, or audit log.
//
// Wire contract (verified against /root/cutlass's MCP client):
//   • JSON-RPC 2.0 over a single `POST /mcp` (we hand-roll it — 3 methods, no SDK)
//   • protocol version "2024-11-05"
//   • methods: initialize | tools/list | tools/call (+ ping, notifications/*)
//   • Authorization: Bearer <api_token>  (same tokens as REST; lib/tokens.js)
//   • responds with application/json (cutlass accepts JSON or SSE; JSON is simpler
//     and sufficient — we never push server-initiated messages)
//
// Agents get the OPEN-page fast path (deploy → publish/rollback). On an
// approval-gated page deploy returns a `pending` version and the tool result
// hands back the /admin URL so the agent can route a human. approve/reject/
// disable are NOT exposed here — they are admin-cookie+CSRF only (PLAN.md §6,§10).

const express = require("express");
const versions = require("./versions");
const tokens = require("./tokens");
const { ApiError } = require("./apierror");
const { version: PKG_VERSION } = require("../package.json");

const PROTOCOL_VERSION = "2024-11-05";
const SERVER_INFO = { name: "pages", version: PKG_VERSION };

const DASHBOARD_HOST = (process.env.DASHBOARD_HOST || "pages.elcanotek.com").toLowerCase();
const CONTENT_HOST = (process.env.CONTENT_HOST || "elcano-pages.com").toLowerCase();

function scheme(host) {
  return host.startsWith("localhost") || host.startsWith("127.") ? "http" : "https";
}

// The URLs an agent routes a human (or client) to. `live` reflects the
// direct-serve decision (HANDOFF): user pages are served on the content host.
function pageUrls(slug) {
  return {
    admin: `${scheme(DASHBOARD_HOST)}://${DASHBOARD_HOST}/admin/${slug}`,
    view: `${scheme(DASHBOARD_HOST)}://${DASHBOARD_HOST}/view/${slug}`,
    live: `${scheme(CONTENT_HOST)}://${CONTENT_HOST}/${slug}`,
  };
}

// ── tools (each wraps versions.js; ctx = { actor, actorType, ip }) ────────────

const TOOLS = {
  list_pages: {
    description: "List all pages (newest first) with each page's currently-published version id.",
    inputSchema: { type: "object", properties: {}, additionalProperties: false },
    handler: async () => ({ pages: await versions.listPages() }),
  },

  get_page: {
    description: "Get a page's metadata and its currently-published version (html + details).",
    inputSchema: {
      type: "object",
      properties: { slug: { type: "string", description: "page slug, e.g. 'omnicom' or 'omnicom/q2'" } },
      required: ["slug"],
      additionalProperties: false,
    },
    handler: async (a) => {
      const r = await versions.getPage(a.slug);
      return { ...r, urls: pageUrls(r.page.slug) };
    },
  },

  deploy_page: {
    description:
      "Create-or-update a page and deploy a new HTML version. Creates the page if it doesn't exist. " +
      "Default render_mode 'themed' (write Flag-tokened HTML; the Flag design system is injected at " +
      "render time — do NOT inline your own tokens or use external CDNs). Set publish:true to make it " +
      "live immediately on OPEN pages; on approval-gated pages it lands as 'pending' for a human to approve.",
    inputSchema: {
      type: "object",
      properties: {
        slug: { type: "string" },
        html: { type: "string", description: "the full page HTML (or a fragment; a <head> is synthesized)" },
        title: { type: "string", description: "page title (used only when creating the page)" },
        render_mode: { type: "string", enum: ["themed", "raw"], description: "default 'themed'" },
        note: { type: "string", description: "optional changelog note for this version" },
        publish: { type: "boolean", description: "publish immediately (open pages only)" },
      },
      required: ["slug", "html"],
      additionalProperties: false,
    },
    handler: async (a, ctx) => {
      // create-if-missing, then deploy — composing public functions only (no backdoor).
      let created = false;
      try {
        await versions.getPage(a.slug);
      } catch (err) {
        if (err instanceof ApiError && err.code === "page_not_found") {
          await versions.createPage({ slug: a.slug, title: a.title || "" }, ctx);
          created = true;
        } else {
          throw err;
        }
      }
      const r = await versions.deploy(
        { slug: a.slug, html: a.html, renderMode: a.render_mode, note: a.note, source: "mcp", publish: !!a.publish },
        ctx
      );
      return {
        created,
        version: r.version,
        deduped: r.deduped,
        published: r.published,
        gated: r.gated,
        // If it was gated (or publish wasn't requested), tell the agent where a human reviews it.
        urls: pageUrls(versions.normalizeSlug(a.slug)),
      };
    },
  },

  update_page: {
    description: "Deploy a new HTML version to an EXISTING page (alias of deploy_page without create). Fails if the page is missing.",
    inputSchema: {
      type: "object",
      properties: {
        slug: { type: "string" },
        html: { type: "string" },
        render_mode: { type: "string", enum: ["themed", "raw"] },
        note: { type: "string" },
        publish: { type: "boolean" },
      },
      required: ["slug", "html"],
      additionalProperties: false,
    },
    handler: async (a, ctx) => {
      const r = await versions.deploy(
        { slug: a.slug, html: a.html, renderMode: a.render_mode, note: a.note, source: "mcp", publish: !!a.publish },
        ctx
      );
      return { version: r.version, deduped: r.deduped, published: r.published, gated: r.gated, urls: pageUrls(versions.normalizeSlug(a.slug)) };
    },
  },

  publish_page: {
    description: "Publish a draft version (make it live). Open pages only — agents cannot publish approval-gated pages.",
    inputSchema: {
      type: "object",
      properties: {
        slug: { type: "string" },
        version_id: { type: "integer", description: "the draft version to publish" },
        expected_version: { type: "integer", description: "optimistic concurrency: the version you believe is live (409 on mismatch)" },
      },
      required: ["slug", "version_id"],
      additionalProperties: false,
    },
    handler: async (a, ctx) => ({
      version: await versions.publish({ slug: a.slug, versionId: a.version_id, expectedVersion: a.expected_version }, ctx),
    }),
  },

  rollback_page: {
    description: "Roll the live pointer back to an already-approved version. Omit version_id to roll back to the previous approved version.",
    inputSchema: {
      type: "object",
      properties: {
        slug: { type: "string" },
        version_id: { type: "integer", description: "target approved version; omit for the previous one" },
        expected_version: { type: "integer" },
      },
      required: ["slug"],
      additionalProperties: false,
    },
    handler: async (a, ctx) => ({
      version: await versions.rollback(
        { slug: a.slug, versionId: a.version_id != null ? a.version_id : null, expectedVersion: a.expected_version },
        ctx
      ),
    }),
  },

  list_versions: {
    description: "List a page's full version history (newest first), including drafts and the pending review queue.",
    inputSchema: {
      type: "object",
      properties: { slug: { type: "string" } },
      required: ["slug"],
      additionalProperties: false,
    },
    handler: async (a) => ({ versions: await versions.listVersions(a.slug) }),
  },

  page_urls: {
    description: "Get the admin / view / live URLs for a page (where to route a human or client).",
    inputSchema: {
      type: "object",
      properties: { slug: { type: "string" } },
      required: ["slug"],
      additionalProperties: false,
    },
    handler: async (a) => pageUrls(versions.normalizeSlug(a.slug)),
  },
};

const TOOL_LIST = Object.entries(TOOLS).map(([name, t]) => ({
  name,
  description: t.description,
  inputSchema: t.inputSchema,
}));

// ── JSON-RPC plumbing ────────────────────────────────────────────────────────

const rpcResult = (id, result) => ({ jsonrpc: "2.0", id, result });
const rpcError = (id, code, message, data) => ({ jsonrpc: "2.0", id, error: { code, message, ...(data ? { data } : {}) } });

// dispatch one JSON-RPC message → a response object, or null for a notification.
async function dispatch(msg, ctx) {
  if (!msg || msg.jsonrpc !== "2.0" || typeof msg.method !== "string") {
    return rpcError(msg && msg.id != null ? msg.id : null, -32600, "Invalid Request");
  }
  const isNotification = msg.id === undefined || msg.id === null;

  switch (msg.method) {
    case "initialize":
      // Echo a protocol version we support. We only speak PROTOCOL_VERSION.
      return rpcResult(msg.id, {
        protocolVersion: PROTOCOL_VERSION,
        capabilities: { tools: {} },
        serverInfo: SERVER_INFO,
      });

    case "notifications/initialized":
    case "notifications/cancelled":
      return null; // notifications get no response

    case "ping":
      return rpcResult(msg.id, {});

    case "tools/list":
      return rpcResult(msg.id, { tools: TOOL_LIST });

    case "tools/call": {
      const params = msg.params || {};
      const tool = TOOLS[params.name];
      if (!tool) return rpcError(msg.id, -32602, `Unknown tool: ${params.name}`);
      try {
        const data = await tool.handler(params.arguments || {}, ctx);
        return rpcResult(msg.id, { content: [{ type: "text", text: JSON.stringify(data) }] });
      } catch (err) {
        // Business errors (404/409/403 from the state machine) → a tool result the
        // model can read and react to (isError:true), NOT a protocol error.
        if (err instanceof ApiError) {
          return rpcResult(msg.id, {
            content: [{ type: "text", text: JSON.stringify({ error: err.message, code: err.code }) }],
            isError: true,
          });
        }
        console.error("mcp tool error:", err.stack || err.message);
        return rpcError(msg.id, -32603, "Internal error");
      }
    }

    default:
      if (isNotification) return null;
      return rpcError(msg.id, -32601, `Method not found: ${msg.method}`);
  }
}

// ── router ───────────────────────────────────────────────────────────────────

const router = express.Router();

// Auth failures → 401 JSON (the server is "disabled" without a valid token, which
// is how chat/cutlass gate registration on PAGES_API_TOKEN being set).
router.post("/", tokens.requireBearer, async (req, res, next) => {
  try {
    const ctx = { actor: req.agent.actor, actorType: req.agent.actorType, ip: req.ip };
    const body = req.body;
    if (Array.isArray(body)) {
      const out = (await Promise.all(body.map((m) => dispatch(m, ctx)))).filter((r) => r !== null);
      return out.length ? res.json(out) : res.status(202).end();
    }
    const resp = await dispatch(body, ctx);
    if (resp === null) return res.status(202).end();
    res.json(resp);
  } catch (err) {
    next(err);
  }
});

// MCP Streamable HTTP allows a GET to open a server→client SSE stream; we don't
// push anything, so decline it cleanly.
router.get("/", (_req, res) => res.status(405).json({ error: "method not allowed; POST JSON-RPC to /mcp" }));

// requireBearer rejects via next(ApiError) → render as JSON with the right status.
// eslint-disable-next-line no-unused-vars
router.use((err, req, res, _next) => {
  if (err instanceof ApiError) return res.status(err.status).json({ error: err.message, code: err.code });
  console.error("mcp error:", err.stack || err.message);
  res.status(500).json({ error: "internal error" });
});

module.exports = { router, dispatch, TOOLS, TOOL_LIST, PROTOCOL_VERSION, pageUrls };
