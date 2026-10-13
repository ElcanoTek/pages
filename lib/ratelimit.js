// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.
"use strict";
// lib/ratelimit.js — per-surface rate limits (PLAN §7/§9). A single small box that
// serves untrusted content and runs an agent deploy loop is trivially DoS'd, and a
// stuck agent will hammer the deploy/MCP endpoints — so cap the hot paths.
//
// Limits are tuned per surface and overridable by env (RL_*). Keying is by client
// IP, using the same one-hop proxy policy on both inner apps in server.js. The
// default trusts a local Caddy peer; PAGES_TRUST_PROXY configures other explicit
// proxy addresses or disables forwarding for direct access. Earlier forwarded
// hops are never used. Returns JSON 429 except content reads and the password
// form, which get HTML so a human sees something sane.

const crypto = require("node:crypto");
const { rateLimit, ipKeyGenerator } = require("express-rate-limit");
const { gateHeaders } = require("./csp");
const { rateLimitPage, busyPage } = require("./contentview");
const standaloneChrome = require("./standalone-chrome");

const N = (v, d) => (Number.isFinite(Number(v)) && Number(v) > 0 ? Number(v) : d);
const MIN = 60 * 1000;

function make({ windowMs, limit, handler, keyGenerator }) {
  return rateLimit({
    windowMs,
    limit,
    ...(keyGenerator ? { keyGenerator } : {}),
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

// Central sign-in (PAGES_AUTH_MODE=central). Each stage has its own budget,
// so pressure on one cannot block another: a script looping /auth/login from
// a shared office or VPN address must not starve a callback that is already
// in flight, nor stop a signed-in person from signing out. HTML 429, because
// a browser is always the caller.
function signinBusy(_req, res) {
  res
    .status(429)
    .set("Retry-After", "60")
    .set("Cache-Control", "no-store")
    .type("html")
    .send(
      standaloneChrome.render({
        title: "Sign-in · Pages",
        kicker: "Sign-in",
        heading: "Too many sign-in attempts",
        bodyHtml: "<p>Wait a minute, then try again.</p>",
        assetsBase: "/shell-assets/flag",
      })
    );
}

// /auth/login: starting a handoff costs a session lookup and a sealed cookie.
const signin = make({ windowMs: MIN, limit: N(process.env.RL_SIGNIN_PER_MIN, 60), handler: signinBusy });

// /auth/callback: each one costs a server-to-server call to Auth's /token.
const signinCallback = make({ windowMs: MIN, limit: N(process.env.RL_SIGNIN_CALLBACK_PER_MIN, 60), handler: signinBusy });

// /logout: keyed by the session cookie (hashed, never stored raw) when one is
// sent, so another client on the same address cannot spend a signed-in
// person's budget. Requests without one share the address's budget. A forged
// cookie earns its sender a fresh key: one primary-key UPDATE, no more than
// the unlimited session lookup on any /admin request, plus a small counter
// that the store forgets within two windows. Accepted: an address-wide gate
// in front of it would let anyone on that address block a real sign-out.
const signout = make({
  windowMs: MIN,
  limit: N(process.env.RL_SIGNOUT_PER_MIN, 30),
  handler: signinBusy,
  keyGenerator: (req) => {
    const auth = require("./auth");
    const token = auth.central && auth.parseCookies(req.headers.cookie)[auth.central.sessionCookie];
    return token ? `session:${crypto.createHash("sha256").update(token).digest("base64url")}` : ipKeyGenerator(req.ip);
  },
});

// /auth/backchannel-logout: Auth's signed logout and access events. Its own
// budget, so agent API traffic from the same address cannot delay a
// revocation. Generous for a provisioning burst; a 429 is retried by Auth.
const backchannel = make({ windowMs: MIN, limit: N(process.env.RL_BACKCHANNEL_PER_MIN, 600) });

module.exports = { api, mcp, content, password, signin, signinCallback, signout, backchannel };
