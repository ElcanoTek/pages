// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/ratelimit.js — per-surface rate limits (PLAN §7/§9). A single small box that
// serves untrusted content and runs an agent deploy loop is trivially DoS'd, and a
// stuck agent will hammer the deploy/MCP endpoints — so cap the hot paths.
//
// Limits are tuned per surface and overridable by env (RL_*). Keying is by client
// IP, which only works because server.js sets `trust proxy` to 1: Caddy is the one
// hop in front of us, so req.ip is the real client rather than 127.0.0.1. That 1 is
// also what keeps express-rate-limit quiet — it rejects a blanket `true` with
// ERR_ERL_PERMISSIVE_TRUST_PROXY, since anyone could then spoof X-Forwarded-For and
// walk around every limit here. Returns JSON 429 except the password form, which
// gets a tiny themed-ish HTML 429 so a human sees something sane.

const { rateLimit } = require("express-rate-limit");
const { gateHeaders } = require("./csp");
const { rateLimitPage, busyPage } = require("./contentview");

const N = (v, d) => (Number.isFinite(Number(v)) && Number(v) > 0 ? Number(v) : d);
const MIN = 60 * 1000;

function make({ windowMs, limit, handler }) {
  return rateLimit({
    windowMs,
    limit,
    standardHeaders: "draft-7",
    legacyHeaders: false,
    // Default keyGenerator already keys by client IP. Since v8 an IPv6 client is
    // keyed by its /56 subnet rather than its exact address, which is the useful
    // behaviour for the password limiter below: a v7 attacker with any ordinary
    // IPv6 allocation could reset the brute-force counter for free by moving to
    // the next address in their own /64.
    handler:
      handler ||
      ((req, res) => res.status(429).json({ error: "rate limit exceeded — slow down", code: "rate_limited" })),
  });
}

// Agent surfaces: REST + MCP. Generous enough for normal deploys, low enough that a
// runaway agent can't melt the box.
const api = make({ windowMs: MIN, limit: N(process.env.RL_API_PER_MIN, 120) });
const mcp = make({
  windowMs: MIN,
  limit: N(process.env.RL_MCP_PER_MIN, 120),
  handler: (_req, res) =>
    res.status(429).json({
      jsonrpc: "2.0",
      id: null,
      error: { code: -32000, message: "Rate limit exceeded; retry later" },
    }),
});

// Content host read path (/raw + direct-serve). Higher ceiling — legitimate viewers
// reload — but still a backstop against scraping/DoS of untrusted pages.
//
// HTML, not the default JSON: every request this limiter rejects is a person or
// an iframe loading a client's dashboard, never an API client. It used to answer
// a client navigation with a raw `{"error":…}` body and none of the content
// zone's headers, so a client reloading their own page was shown a JSON blob.
const content = make({
  windowMs: MIN,
  limit: N(process.env.RL_CONTENT_PER_MIN, 240),
  // framable: this limiter also guards /raw, which the /admin preview frames.
  // A frame-ancestors 'none' rejection would answer an operator with a blank
  // preview instead of the explanation.
  handler: (req, res) => res
    .status(429)
    .set(gateHeaders({ framable: true }))
    .set("Retry-After", "60")
    .type("html")
    .send(busyPage({ slug: String(req.path || "").replace(/^\/+/, "") })),
});

// Password form: brute-force guard. Strict, longer window, HTML 429.
const password = make({
  windowMs: N(process.env.RL_PASSWORD_WINDOW_MIN, 15) * MIN,
  limit: N(process.env.RL_PASSWORD_TRIES, 20),
  handler: (req, res) => {
    // req was discarded, so the page could neither link back nor say the real
    // wait, and no Retry-After reached a client that would have honoured it.
    const minutes = N(process.env.RL_PASSWORD_WINDOW_MIN, 15);
    const slug = String(req.path || "").replace(/^\/+/, "");
    res
      .status(429)
      .set(gateHeaders())
      .set("Retry-After", String(minutes * 60))
      .type("html")
      .send(rateLimitPage({ slug, minutes }));
  },
});

module.exports = { api, mcp, content, password };
